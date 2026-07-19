//go:build e2e && ((linux && amd64) || (darwin && arm64 && bingonative))

// Full-stack acceptance spec: unlike declareBasicStepOverSpec (which drives the
// debugger.Debugger in-process), this exercises the ENTIRE vertical slice —
// pkg/client -> WebSocket -> internal/server -> internal/hub -> internal/debugger
// -> real tracee. It boots a real server on an ephemeral port, connects the
// real reference client over a real gorilla WebSocket, and asserts that
// continue/setBreakpoint/step-over behave correctly when observed as events on
// the wire.
//
// The point is to catch WIRING regressions the backend-only specs can't: hub
// seq re-stamping of real events, the suspend/resume gate firing on a genuine
// BreakpointHit, synchronous SetBreakpoint confirmation routing, and the
// launch/welcome handshake. The low-level correctness itself is still covered
// by the direct specs; this just proves the transport faithfully carries it.
//
// Tuning:
//
//	BINGO_E2E_FS_ITERS  (default 15)  full-stack continue+step-over iterations
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
// acceptance spec to the enclosing Ginkgo container. Same correctness contract
// as the basic spec, but every command and event crosses the real transport.
func declareFullStackSpec() {
	It("drives continue and step-over over the WebSocket transport", Label("fullstack"), func() {
		line := markerLine(basicTargetSrc, "// BP")
		bin := buildTarget("fullstack_target", basicTargetSrc)

		h := newFullStackHarness(bin)

		// Synchronous over the wire: blocks until the hub broadcasts the
		// BreakpointSet confirmation, which the client routes back to this call.
		bp, err := h.c.SetBreakpoint("fullstack_target.go", line)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint over the wire")
		Expect(bp.Location.Line).To(Equal(line), "breakpoint resolved to the requested line")

		iters := envInt("BINGO_E2E_FS_ITERS", 15)
		assertContinueStepOver(h.c, line, iters)
		AddReportEntry("fullstack-iterations", iters)
	})
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
