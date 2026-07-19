//go:build e2e && ((linux && amd64) || (darwin && arm64 && bingonative))

// Full-stack acceptance spec: unlike declareBasicStepOverSpec (which drives the
// debugger.Debugger in-process), this exercises the ENTIRE vertical slice —
// pkg/client -> WebSocket -> internal/server -> internal/hub -> internal/debugger
// -> real tracee. It boots a real server on an ephemeral port, connects the
// real reference client over a real gorilla WebSocket, and asserts that
// continue/setBreakpoint round-trips behave correctly when observed as events on
// the wire.
//
// The point is to catch WIRING regressions the backend-only specs can't: hub
// seq re-stamping of real events, the suspend/resume gate firing on a genuine
// BreakpointHit, synchronous SetBreakpoint confirmation routing, the
// launch/welcome handshake, and (for restart) the hub-level kill+relaunch+
// reinstall path. The low-level correctness itself is still covered by the
// direct specs; this just proves the transport faithfully carries it.
//
// It deliberately drives Continue->Pause->Paused round-trips (plus a single
// breakpoint hit) rather than repeated Continue-from-breakpoint or step-over:
// the transport carries every suspending event through the same gate/seq
// machinery regardless of kind, and repeatedly RESUMING FROM a software
// breakpoint (which both Continue-from-a-BP and step-over do — the
// restore->single-step-over-trap->reinstall dance) hits a darwin backend
// lost-wakeup (issue #89, see AGENTS.md). Every resume here is a PLAIN continue
// (from the launch stop or a Paused stop), never a resume-from-breakpoint, which
// is what makes this spec deterministic on darwin; the resume-from-breakpoint
// correctness itself stays in the in-process specs.
//
// Tuning:
//
//	BINGO_E2E_FS_ITERS  (default 15)  full-stack continue+pause round-trips
package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/server"
	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// declareFullStackSpec adds the client -> WebSocket -> hub -> debugger -> tracee
// acceptance spec to the enclosing Ginkgo container. It asserts the transport
// contract via Continue->Pause->Paused round-trips plus one breakpoint hit:
// every command and event crosses the real transport, the hub's suspend gate
// blocks on each genuine Paused/BreakpointHit until the next resume, and every
// broadcast event carries a strictly increasing hub seq. Every resume is a plain
// continue (never a resume-from-breakpoint) so it stays deterministic on darwin
// (issue #89).
func declareFullStackSpec() {
	It("carries continue/pause round-trips and a breakpoint hit over the WebSocket transport", Label("fullstack"), func() {
		line := markerLine(basicTargetSrc, "// BP")
		bin := buildTarget("fullstack_target", basicTargetSrc)

		h := newFullStackHarness(bin)

		// One strictly-increasing hub-seq checker shared across every event we
		// drain, in both phases (see AGENTS.md -> Hub seq stream).
		var lastSeq uint64
		seen := false
		checkSeq := func(evt protocol.Event) {
			if seen {
				Expect(evt.Seq).To(BeNumerically(">", lastSeq),
					"hub seq must strictly increase: got %d after %d", evt.Seq, lastSeq)
			}
			lastSeq, seen = evt.Seq, true
		}

		iters := envInt("BINGO_E2E_FS_ITERS", 15)

		// Phase 1: Continue->Pause->Paused round trips. The first Continue
		// resumes from the launch stop and each subsequent one from a Paused
		// stop -- both PLAIN continues, never a resume-from-breakpoint -- so this
		// sidesteps the darwin #89 lost-wakeup while fully exercising the command
		// transport, the suspend gate firing on every Paused, and seq ordering.
		for i := 0; i < iters; i++ {
			Expect(h.c.Continue()).To(Succeed(), "Continue #%d", i)
			// Let the tracee actually be running before the async interrupt, so
			// Pause exercises the running->suspended path rather than racing the
			// resume.
			time.Sleep(20 * time.Millisecond)
			Expect(h.c.Pause()).To(Succeed(), "Pause #%d", i)
			evt := awaitEventFunc(h.c.Events(), 20*time.Second, checkSeq,
				protocol.EventPaused, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventPaused),
				"Pause #%d expected Paused, got %s: %s", i, evt.Kind, evt.Payload)
		}

		// Phase 2: one breakpoint round trip. SetBreakpoint is synchronous over
		// the wire (confirmation routing); the Continue that reaches it is a
		// plain resume from the Paused stop, so a genuine BreakpointHit crosses
		// the gate/seq machinery WITHOUT ever resuming from a breakpoint. We stop
		// here (no resume from the BP) -- cleanup Kills the suspended tracee.
		bp, err := h.c.SetBreakpoint("fullstack_target.go", line)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint over the wire")
		Expect(bp.Location.Line).To(Equal(line), "breakpoint resolved to the requested line")

		Expect(h.c.Continue()).To(Succeed(), "Continue from Paused to the breakpoint")
		evt := awaitEventFunc(h.c.Events(), 20*time.Second, checkSeq,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
			"expected BreakpointHit over the wire, got %s: %s", evt.Kind, evt.Payload)
		var hit protocol.BreakpointHitPayload
		Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode BreakpointHit")
		Expect(hit.Breakpoint.Location.Line).To(Equal(line), "BreakpointHit at BP line")

		AddReportEntry("fullstack-iterations", iters)
	})
}

// declareRestartSpec adds the hub-level Restart acceptance spec. Restart lives
// entirely in the hub (kill the current debugger, relaunch, reinstall
// breakpoints on the fresh instance), so it can only be driven through the full
// stack. It runs to a breakpoint, Restarts, and asserts the confirmation reports
// the breakpoint reinstalled (nothing discarded) and that a plain Continue then
// reaches that breakpoint again — proving the process was relaunched from the
// top and the breakpoint really was re-armed on the new process.
func declareRestartSpec() {
	It("reinstalls breakpoints and reruns from the top on Restart", Label("restart"), func() {
		line := markerLine(basicTargetSrc, "// BP")
		bin := buildTarget("restart_target", basicTargetSrc)

		h := newFullStackHarness(bin)

		bp, err := h.c.SetBreakpoint("restart_target.go", line)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint over the wire")
		Expect(bp.Location.Line).To(Equal(line), "breakpoint resolved to the requested line")

		Expect(h.c.Continue()).To(Succeed(), "Continue to breakpoint")
		evt := awaitEvent(h.c.Events(), 20*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit), "hit the breakpoint before restart")

		// Restart: kill + relaunch + reinstall breakpoints. Blocks until the hub
		// confirms via EventRestarted.
		restarted, err := h.c.Restart(nil, nil)
		Expect(err).NotTo(HaveOccurred(), "Restart over the wire")
		Expect(restarted.Discarded).To(BeEmpty(), "no breakpoints discarded on restart")
		Expect(restarted.Breakpoints).NotTo(BeEmpty(), "breakpoint reinstalled on restart")
		Expect(restarted.Breakpoints[0].Location.Line).To(Equal(line), "reinstalled at the same line")

		// The relaunched process starts from the top; the intervening launch-stop
		// Stepped is ignored by awaitEvent. Continue must reach the reinstalled
		// breakpoint again, proving both the relaunch and the reinstall.
		Expect(h.c.Continue()).To(Succeed(), "Continue after restart")
		evt = awaitEvent(h.c.Events(), 20*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
			"hit the reinstalled breakpoint after restart, got %s: %s", evt.Kind, evt.Payload)
		var hit protocol.BreakpointHitPayload
		Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode post-restart BreakpointHit")
		Expect(hit.Breakpoint.Location.Line).To(Equal(line), "post-restart hit at the BP line")
	})
}

// awaitEventFunc is awaitEvent with a per-event observer, so callers can assert
// invariants (e.g. seq monotonicity) over every event drained, not just the one
// that matches. onEach runs for each event before the kind match test.
func awaitEventFunc(ch <-chan protocol.Event, timeout time.Duration, onEach func(protocol.Event), kinds ...protocol.EventKind) protocol.Event {
	GinkgoHelper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				Fail(fmt.Sprintf("events channel closed while waiting for %v", kinds))
			}
			if onEach != nil {
				onEach(evt)
			}
			if e2eDebug {
				GinkgoWriter.Printf("event: kind=%v seq=%d payload=%s\n", evt.Kind, evt.Seq, evt.Payload)
			}
			for _, k := range kinds {
				if evt.Kind == k {
					return evt
				}
			}
		case <-deadline:
			Fail(fmt.Sprintf("TIMEOUT after %s waiting for %v (possible hang)", timeout, kinds))
		}
	}
}

// --- full-stack harness ---

type fullStackHarness struct {
	srv *server.Server
	c   client.Client
}

// newFullStackHarness boots a real server, connects the real client, launches
// bin, and waits for the initial launch stop to arrive over the wire. Cleanup
// resumes+kills (bounded) and shuts the server down; shutdown cancels the hub
// context, which unblocks a suspended hub.
func newFullStackHarness(bin string) *fullStackHarness {
	GinkgoHelper()

	srv, addr := startTestServer()

	c, err := client.Create(addr)
	Expect(err).NotTo(HaveOccurred(), "client.Create(%s)", addr)

	DeferCleanup(func() {
		// Kill is a resuming command — it unblocks the hub if it's parked in
		// the suspend gate. Bounded so a wedged backend reports instead of
		// hanging the whole suite.
		done := make(chan struct{})
		go func() { _ = c.Kill(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			AddReportEntry("fullstack-kill-timeout", "client.Kill did not return within 5s")
		}
		_ = c.Close()
		srv.Shutdown(5 * time.Second)
	})

	// Launch is fire-and-forget; the initial stop surfaces as Stepped on Events.
	// A launch-time failure (e.g. task_for_pid denied on a locked-down host)
	// comes back asynchronously as an EventError, so surface it clearly instead
	// of masquerading as a Stepped timeout.
	Expect(c.Launch(bin, nil, nil)).To(Succeed(), "client.Launch")
	evt := awaitEvent(c.Events(), 20*time.Second, protocol.EventStepped, protocol.EventError)
	if evt.Kind == protocol.EventError {
		var ep protocol.ErrorPayload
		_ = json.Unmarshal(evt.Payload, &ep)
		Fail(fmt.Sprintf("launch failed over the wire: %s", ep.Message))
	}

	return &fullStackHarness{srv: srv, c: c}
}

// startTestServer binds a real server to an ephemeral loopback port and waits
// until it answers on /api/sessions. Retries a few times to shrug off the tiny
// window between releasing the probe listener and the server re-binding it.
func startTestServer() (*server.Server, string) {
	GinkgoHelper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if e2eDebug {
		log = slog.Default()
	}

	const attempts = 5
	for i := 0; i < attempts; i++ {
		addr, err := freeLoopbackAddr()
		Expect(err).NotTo(HaveOccurred(), "allocate an ephemeral port")

		srv := server.New(addr, log)
		go func() { _ = srv.Start() }()

		if waitServerReady(addr, 3*time.Second) {
			return srv, addr
		}
		srv.Shutdown(time.Second) // lost the port race or bind failed; retry
	}
	Fail("server did not become ready after multiple attempts")
	return nil, ""
}

// freeLoopbackAddr asks the OS for an unused loopback port, then releases it so
// the server can bind it. tcp4 to match Server.Start (which listens on tcp4).
func freeLoopbackAddr() (string, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	return addr, ln.Close()
}

// waitServerReady polls the REST endpoint until the listener is accepting.
func waitServerReady(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := client.ListSessions(addr); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
