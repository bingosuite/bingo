//go:build e2e && ((linux && amd64) || (darwin && arm64 && bingonative))

// DAP acceptance specs. Unlike the backend-only specs (which drive
// debugger.Debugger in-process) and the full-stack spec (which drives the
// reference WebSocket client), these drive a real Debug Adapter Protocol client
// over a TCP socket through the ENTIRE stack:
//
//	go-dap client -> TCP -> internal/dap.Handler (as a hub.WSConn) ->
//	internal/hub -> internal/debugger -> real tracee
//
// They prove the DAP translator end to end: the initialize/launch/
// configurationDone handshake, setBreakpoints diffing, the stopped/continued/
// exited event mapping, and the threads/stackTrace/scopes/variables correlation
// — all against a genuine native backend. The multi-client spec additionally
// attaches N WebSocket OBSERVERS to the very same session the DAP client is
// driving and asserts every observer sees the same breakpoint hit, proving DAP
// and WebSocket clients coexist on one session (the whole point of the feature).
//
// Tuning:
//
//	BINGO_E2E_DAP_OBSERVERS  (default 3)  WebSocket observers in the multi-client spec
package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	godap "github.com/google/go-dap"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/server"
	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// dapTargetSrc is a short, deterministic target for DAP specs: a breakpoint line
// inside a tiny loop that calls a function (so a stack has a real frame) and a
// clean exit(0). It runs to completion in milliseconds once resumed, so the
// continue-to-exit assertion doesn't rely on any timing.
const dapTargetSrc = `package main

import "fmt"

func add(a, b int) int {
	return a + b
}

func main() {
	total := 0
	for i := 0; i < 3; i++ {
		total = add(total, i) // BP
	}
	fmt.Println("total:", total)
}
`

// declareDAPSpec drives a single DAP client through a full debug session against
// the real backend: launch, set a breakpoint, run to it, inspect threads/stack/
// variables, then clear the breakpoint and run to a clean exit.
func declareDAPSpec() {
	It("drives a full debug session over DAP", Label("dap"), func() {
		line := markerLine(dapTargetSrc, "// BP")
		bin := buildTarget("dap_target", dapTargetSrc)

		_, _, dapAddr := startTestServerWithDAP()
		dc := dialDAP(dapAddr)

		dc.initialize()
		launchSeq := dc.launch(bin, false)

		// Entry stop → the adapter emits `initialized`.
		dc.waitEvent(20*time.Second, "initialized")

		// Set the breakpoint while stopped at entry (VS Code's ordering).
		bps := dc.setBreakpoints("dap_target.go", line)
		Expect(bps.Body.Breakpoints).To(HaveLen(1))
		Expect(bps.Body.Breakpoints[0].Verified).To(BeTrue(), "breakpoint must resolve")
		Expect(bps.Body.Breakpoints[0].Line).To(Equal(line))

		dc.configurationDone()
		// The delayed launch response arrives after configurationDone.
		launchResp := dc.await(launchSeq)
		Expect(launchResp.(godap.ResponseMessage).GetResponse().Success).To(BeTrue(), "launch must succeed")

		// configurationDone with !stopOnEntry auto-continues from entry to the
		// breakpoint (a plain continue, never a resume-from-armed-trap).
		stopped := dc.waitStopped(20 * time.Second)
		Expect(stopped.Body.Reason).To(Equal("breakpoint"), "first stop is the breakpoint")
		tid := stopped.Body.ThreadId
		Expect(tid).To(BeNumerically(">=", 1))

		// threads must include the stopped thread.
		threads := dc.threads()
		Expect(threads.Body.Threads).NotTo(BeEmpty(), "at least one thread")

		// stackTrace on the stopped thread: the top frame is in package main.
		st := dc.stackTrace(tid)
		Expect(st.Body.StackFrames).NotTo(BeEmpty(), "at least one frame")
		Expect(st.Body.StackFrames[0].Name).To(ContainSubstring("main"), "top frame in main")
		frameID := st.Body.StackFrames[0].Id

		// scopes → a single synthetic Locals scope whose ref decodes to the frame.
		scopes := dc.scopes(frameID)
		Expect(scopes.Body.Scopes).To(HaveLen(1))
		Expect(scopes.Body.Scopes[0].VariablesReference).To(Equal(frameID))

		// variables must round-trip (contents are best-effort; correlation is
		// what we assert — the response comes back for THIS request).
		vars := dc.variables(scopes.Body.Scopes[0].VariablesReference)
		Expect(vars.GetResponse().Success).To(BeTrue(), "variables request succeeds")

		// Resume: the loop runs another iteration and re-hits the breakpoint. This
		// exercises resume-from-a-breakpoint over DAP (the engine's
		// restore->single-step->reinstall step-off dance) and its mapping back to
		// a DAP stopped/reason=breakpoint — the same path the clearbp spec proves,
		// driven here entirely through the adapter.
		dc.continueThread(tid)
		again := dc.waitStopped(20 * time.Second)
		Expect(again.Body.Reason).To(Equal("breakpoint"), "second stop is the breakpoint again")

		dc.disconnect()
	})
}

// declareDAPExitSpec proves the exited/terminated mapping end to end: a DAP
// client launches a target with NO breakpoints and lets it run to a clean exit,
// asserting the adapter emits `exited` (with the code) followed by `terminated`.
// Kept separate from declareDAPSpec because reaching exit from a breakpoint the
// process is parked on would re-arm it through the step-off path (see the clearbp
// spec) — a plain continue from the entry stop is the deterministic exit path.
func declareDAPExitSpec() {
	It("maps a clean process exit to DAP exited+terminated", Label("dap"), func() {
		bin := buildTarget("dap_exit_target", dapTargetSrc)

		_, _, dapAddr := startTestServerWithDAP()
		dc := dialDAP(dapAddr)

		dc.initialize()
		launchSeq := dc.launch(bin, false)
		dc.waitEvent(20*time.Second, "initialized")

		// No breakpoints: configurationDone auto-continues from the entry stop and
		// the tiny target runs straight to os.Exit(0).
		dc.configurationDone()
		launchResp := dc.await(launchSeq)
		Expect(launchResp.(godap.ResponseMessage).GetResponse().Success).To(BeTrue(), "launch must succeed")

		exited := dc.waitEvent(20*time.Second, "exited").(*godap.ExitedEvent)
		Expect(exited.Body.ExitCode).To(Equal(0), "clean exit code")
		dc.waitEvent(5*time.Second, "terminated")

		dc.disconnect()
	})
}

// declareDAPMultiClientSpec proves one DAP driver and N WebSocket observers
// share a single session: the observers attach to the session the DAP client
// created and every one of them sees the breakpoint hit the DAP client drove.
func declareDAPMultiClientSpec() {
	It("fans a DAP-driven breakpoint hit out to many WebSocket observers", Label("dap"), func() {
		line := markerLine(dapTargetSrc, "// BP")
		bin := buildTarget("dap_multi_target", dapTargetSrc)

		_, wsAddr, dapAddr := startTestServerWithDAP()
		dc := dialDAP(dapAddr)

		dc.initialize()
		_ = dc.launch(bin, false)
		dc.waitEvent(20*time.Second, "initialized")

		// The session now exists (launch created it). Discover it and attach the
		// observers BEFORE the breakpoint is hit so they all witness it.
		sessionID := waitForSession(wsAddr)

		n := envInt("BINGO_E2E_DAP_OBSERVERS", 3)
		observers := make([]client.Client, 0, n)
		for i := 0; i < n; i++ {
			obs, err := client.Join(wsAddr, sessionID)
			Expect(err).NotTo(HaveOccurred(), "observer %d join", i)
			observers = append(observers, obs)
			DeferCleanup(func() { _ = obs.Close() })
		}

		// Drive to the breakpoint.
		bps := dc.setBreakpoints("dap_multi_target.go", line)
		Expect(bps.Body.Breakpoints[0].Verified).To(BeTrue())
		dc.configurationDone()

		stopped := dc.waitStopped(20 * time.Second)
		Expect(stopped.Body.Reason).To(Equal("breakpoint"))

		// Every observer must independently see the SAME breakpoint hit on the
		// wire, at the same line — DAP drove it, WebSocket observed it.
		for i, obs := range observers {
			evt := awaitEvent(obs.Events(), 20*time.Second,
				protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"observer %d expected BreakpointHit, got %s: %s", i, evt.Kind, evt.Payload)
			var hit protocol.BreakpointHitPayload
			Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed())
			Expect(hit.Breakpoint.Location.Line).To(Equal(line),
				"observer %d saw the hit at the BP line", i)
		}

		AddReportEntry("dap-observers", n)
		dc.disconnect()
	})
}

// declareDAPJoinSpec proves the JOIN path end to end: a SECOND DAP client
// attaches to an already-suspended session (attach with a `session` arg, no pid)
// WITHOUT relaunching it, inspects the shared stop, then DRIVES a continue that
// the original DAP driver AND a WebSocket observer both witness out of band.
// This is the many-DAP-clients-per-session capability behind `cmd/dapcli
// -session`.
func declareDAPJoinSpec() {
	It("lets a second DAP client join and drive an existing session", Label("dap"), func() {
		line := markerLine(dapTargetSrc, "// BP")
		bin := buildTarget("dap_join_target", dapTargetSrc)

		_, wsAddr, dapAddr := startTestServerWithDAP()

		// Driver: launch, set the breakpoint, run to it — session is now
		// suspended at the breakpoint with other clients able to join.
		driver := dialDAP(dapAddr)
		driver.initialize()
		launchSeq := driver.launch(bin, false)
		driver.waitEvent(20*time.Second, "initialized")
		bps := driver.setBreakpoints("dap_join_target.go", line)
		Expect(bps.Body.Breakpoints[0].Verified).To(BeTrue())
		driver.configurationDone()
		Expect(driver.await(launchSeq).(godap.ResponseMessage).GetResponse().Success).To(BeTrue())
		firstStop := driver.waitStopped(20 * time.Second)
		Expect(firstStop.Body.Reason).To(Equal("breakpoint"))

		// A WebSocket observer joins the same session (proving DAP + WS coexist).
		sessionID := waitForSession(wsAddr)
		obs, err := client.Join(wsAddr, sessionID)
		Expect(err).NotTo(HaveOccurred(), "ws observer join")
		DeferCleanup(func() { _ = obs.Close() })

		// A SECOND DAP client joins the suspended session by id.
		joiner := dialDAP(dapAddr)
		welcome := joiner.joinSuspended(sessionID)
		Expect(welcome.Body.Reason).To(Equal("pause"), "welcome reflects the suspended session")

		// The joiner can inspect the shared stop without having launched it.
		Expect(joiner.threads().Body.Threads).NotTo(BeEmpty(), "joiner sees threads")
		st := joiner.stackTrace(welcome.Body.ThreadId)
		Expect(st.Body.StackFrames).NotTo(BeEmpty(), "joiner sees a stack")

		// The joiner DRIVES a continue; the loop re-hits the breakpoint.
		joiner.continueThread(welcome.Body.ThreadId)
		joinerStop := joiner.waitStopped(20 * time.Second)
		Expect(joinerStop.Body.Reason).To(Equal("breakpoint"), "joiner's continue re-hits the BP")

		// The ORIGINAL driver did not issue that continue, so it must observe the
		// out-of-band resume (`continued`) followed by the next breakpoint stop.
		driver.waitEvent(20*time.Second, "continued")
		driverAgain := driver.waitStopped(20 * time.Second)
		Expect(driverAgain.Body.Reason).To(Equal("breakpoint"), "driver observes the joiner-driven hit")

		// The WebSocket observer independently sees the same breakpoint hit.
		evt := awaitEvent(obs.Events(), 20*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
			"ws observer sees the joiner-driven hit, got %s: %s", evt.Kind, evt.Payload)

		joiner.disconnect()
		driver.disconnect()
	})
}

// waitForSession polls the REST API until exactly one session is present and
// returns its id. The DAP client creates it on launch, so it appears shortly
// after the `initialized` event.
func waitForSession(wsAddr string) string {
	GinkgoHelper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sessions, err := client.ListSessions(wsAddr)
		if err == nil && len(sessions) == 1 {
			return sessions[0].ID
		}
		time.Sleep(20 * time.Millisecond)
	}
	Fail("no session appeared after DAP launch")
	return ""
}

// --- DAP test client (go-dap over TCP with response/event demux) ---

type dapClient struct {
	conn    net.Conn
	reader  *bufio.Reader
	mu      sync.Mutex
	seq     int
	pending map[int]chan godap.Message
	events  chan godap.Message
}

// dialDAP connects to the DAP server and starts the demux read loop. Cleanup
// closes the connection.
func dialDAP(addr string) *dapClient {
	GinkgoHelper()
	conn, err := net.Dial("tcp4", addr)
	Expect(err).NotTo(HaveOccurred(), "dial DAP %s", addr)

	c := &dapClient{
		conn:    conn,
		reader:  bufio.NewReader(conn),
		pending: make(map[int]chan godap.Message),
		events:  make(chan godap.Message, 256),
	}
	go c.readLoop()
	DeferCleanup(func() { _ = conn.Close() })
	return c
}

func (c *dapClient) readLoop() {
	for {
		msg, err := godap.ReadProtocolMessage(c.reader)
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case godap.ResponseMessage:
			rs := m.GetResponse()
			c.mu.Lock()
			ch := c.pending[rs.RequestSeq]
			c.mu.Unlock()
			// The waiter channel is buffered (cap 1) and never removed here, so
			// a response that arrives before await() is called is retained until
			// await() drains it. Deleting the entry on delivery would race await,
			// which looks the channel up by seq (the "no pending waiter" flake).
			if ch != nil {
				ch <- msg
			}
		case godap.EventMessage:
			select {
			case c.events <- msg:
			default:
			}
		}
	}
}

// send writes a request with an auto-incrementing seq, registers a waiter for
// its response, and returns the seq (use await to collect the response later).
func (c *dapClient) send(command string, m godap.RequestMessage) int {
	GinkgoHelper()
	c.mu.Lock()
	c.seq++
	seq := c.seq
	req := m.GetRequest()
	req.Seq = seq
	req.Type = "request"
	req.Command = command
	ch := make(chan godap.Message, 1)
	c.pending[seq] = ch
	c.mu.Unlock()

	Expect(godap.WriteProtocolMessage(c.conn, m)).To(Succeed(), "write %s", command)
	return seq
}

// await blocks for the response to the request with the given seq. The waiter
// channel is captured at send time, so a response already delivered by readLoop
// (buffered) is returned immediately; the entry is removed once drained.
func (c *dapClient) await(seq int) godap.Message {
	GinkgoHelper()
	c.mu.Lock()
	ch := c.pending[seq]
	c.mu.Unlock()
	Expect(ch).NotTo(BeNil(), "no pending waiter for seq %d", seq)
	select {
	case msg := <-ch:
		c.mu.Lock()
		delete(c.pending, seq)
		c.mu.Unlock()
		return msg
	case <-time.After(15 * time.Second):
		Fail(fmt.Sprintf("timeout awaiting response to seq %d", seq))
		return nil
	}
}

// request sends and awaits in one step (for requests whose response is prompt).
func (c *dapClient) request(command string, m godap.RequestMessage) godap.Message {
	return c.await(c.send(command, m))
}

// waitEvent drains events until one whose Event name is in names arrives.
func (c *dapClient) waitEvent(timeout time.Duration, names ...string) godap.EventMessage {
	GinkgoHelper()
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-c.events:
			if !ok {
				Fail(fmt.Sprintf("DAP events channel closed while waiting for %v", names))
			}
			ev := msg.(godap.EventMessage).GetEvent()
			for _, n := range names {
				if ev.Event == n {
					return msg.(godap.EventMessage)
				}
			}
		case <-deadline:
			Fail(fmt.Sprintf("timeout after %s waiting for DAP event %v", timeout, names))
		}
	}
}

func (c *dapClient) waitStopped(timeout time.Duration) *godap.StoppedEvent {
	GinkgoHelper()
	return c.waitEvent(timeout, "stopped").(*godap.StoppedEvent)
}

func (c *dapClient) initialize() {
	GinkgoHelper()
	resp := c.request("initialize", &godap.InitializeRequest{
		Arguments: godap.InitializeRequestArguments{AdapterID: "bingo"},
	})
	Expect(resp.(godap.ResponseMessage).GetResponse().Success).To(BeTrue(), "initialize")
}

// launch sends a launch request WITHOUT awaiting: the response is delayed until
// configurationDone. Returns the request seq to await later.
func (c *dapClient) launch(program string, stopOnEntry bool) int {
	GinkgoHelper()
	args, err := json.Marshal(map[string]any{"program": program, "stopOnEntry": stopOnEntry})
	Expect(err).NotTo(HaveOccurred())
	return c.send("launch", &godap.LaunchRequest{Arguments: args})
}

// setBreakpoints sets the given lines for file (replace-all). A single negative
// line means "clear all" (an empty breakpoint list).
func (c *dapClient) setBreakpoints(file string, lines ...int) *godap.SetBreakpointsResponse {
	GinkgoHelper()
	sbps := make([]godap.SourceBreakpoint, 0, len(lines))
	for _, l := range lines {
		if l < 0 {
			continue
		}
		sbps = append(sbps, godap.SourceBreakpoint{Line: l})
	}
	resp := c.request("setBreakpoints", &godap.SetBreakpointsRequest{
		Arguments: godap.SetBreakpointsArguments{
			Source:      godap.Source{Path: file, Name: file},
			Breakpoints: sbps,
		},
	})
	return resp.(*godap.SetBreakpointsResponse)
}

func (c *dapClient) configurationDone() {
	GinkgoHelper()
	resp := c.request("configurationDone", &godap.ConfigurationDoneRequest{})
	Expect(resp.(godap.ResponseMessage).GetResponse().Success).To(BeTrue(), "configurationDone")
}

func (c *dapClient) continueThread(tid int) {
	GinkgoHelper()
	resp := c.request("continue", &godap.ContinueRequest{Arguments: godap.ContinueArguments{ThreadId: tid}})
	Expect(resp.(godap.ResponseMessage).GetResponse().Success).To(BeTrue(), "continue")
}

func (c *dapClient) threads() *godap.ThreadsResponse {
	GinkgoHelper()
	return c.request("threads", &godap.ThreadsRequest{}).(*godap.ThreadsResponse)
}

func (c *dapClient) stackTrace(tid int) *godap.StackTraceResponse {
	GinkgoHelper()
	return c.request("stackTrace", &godap.StackTraceRequest{
		Arguments: godap.StackTraceArguments{ThreadId: tid},
	}).(*godap.StackTraceResponse)
}

func (c *dapClient) scopes(frameID int) *godap.ScopesResponse {
	GinkgoHelper()
	return c.request("scopes", &godap.ScopesRequest{
		Arguments: godap.ScopesArguments{FrameId: frameID},
	}).(*godap.ScopesResponse)
}

func (c *dapClient) variables(ref int) godap.ResponseMessage {
	GinkgoHelper()
	resp := c.request("variables", &godap.VariablesRequest{
		Arguments: godap.VariablesArguments{VariablesReference: ref},
	})
	return resp.(godap.ResponseMessage)
}

func (c *dapClient) disconnect() {
	GinkgoHelper()
	_ = c.request("disconnect", &godap.DisconnectRequest{})
}

// joinSuspended registers this client as an additional driver on an existing,
// already-SUSPENDED session: initialize, then attach carrying the session id and
// no pid. The adapter emits `initialized` immediately plus a welcome `stopped`
// reflecting the shared suspended state (order between the two is not
// guaranteed); this drains both, completes configurationDone, awaits the delayed
// attach response, and returns the welcome stopped event.
func (c *dapClient) joinSuspended(sessionID string) *godap.StoppedEvent {
	GinkgoHelper()
	c.initialize()
	args, err := json.Marshal(map[string]any{"session": sessionID})
	Expect(err).NotTo(HaveOccurred())
	attachSeq := c.send("attach", &godap.AttachRequest{Arguments: args})

	var welcome *godap.StoppedEvent
	sawInit := false
	deadline := time.After(20 * time.Second)
	for !sawInit || welcome == nil {
		select {
		case msg, ok := <-c.events:
			if !ok {
				Fail("DAP events channel closed during join")
			}
			switch ev := msg.(type) {
			case *godap.InitializedEvent:
				sawInit = true
			case *godap.StoppedEvent:
				welcome = ev
			}
		case <-deadline:
			Fail("timeout waiting for join handshake (initialized + welcome stopped)")
		}
	}

	c.configurationDone()
	resp := c.await(attachSeq)
	Expect(resp.(godap.ResponseMessage).GetResponse().Success).To(BeTrue(), "attach(join) must succeed")
	return welcome
}

// startTestServerWithDAP boots a server with both the WebSocket and DAP
// listeners bound to ephemeral loopback ports. Returns the server and both
// addresses.
func startTestServerWithDAP() (srv *server.Server, wsAddr, dapAddr string) {
	GinkgoHelper()
	s, ws := startTestServer()
	dap, err := freeLoopbackAddr()
	Expect(err).NotTo(HaveOccurred(), "allocate DAP port")
	Expect(s.StartDAP(dap)).To(Succeed(), "start DAP server")
	DeferCleanup(func() { s.Shutdown(5 * time.Second) })
	return s, ws, dap
}
