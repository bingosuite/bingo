package dap

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// cmdRecorder collects the bingo commands the handler enqueues (the DAP read
// loop → hub direction), standing in for the hub's read pump.
type cmdRecorder struct {
	mu   sync.Mutex
	cmds []protocol.Command
}

func (r *cmdRecorder) add(data []byte) {
	cmd, err := protocol.UnmarshalCommand(data)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.cmds = append(r.cmds, cmd)
	r.mu.Unlock()
}

func (r *cmdRecorder) kinds() []protocol.CommandKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.CommandKind, len(r.cmds))
	for i, c := range r.cmds {
		out[i] = c.Kind
	}
	return out
}

// waitForCommand polls until a command of kind appears or the deadline passes.
func (r *cmdRecorder) waitForCommand(t *testing.T, kind protocol.CommandKind) protocol.Command {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, c := range r.cmds {
			if c.Kind == kind {
				r.mu.Unlock()
				return c
			}
		}
		r.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for command %s; saw %v", kind, r.kinds())
	return protocol.Command{}
}

type fakeSession struct {
	id      string
	cmds    *cmdRecorder
	welcome protocol.SessionState
}

func (s *fakeSession) SessionID() string { return s.id }

// AddClient mirrors the hub: it optionally delivers a welcome EventSessionState
// (as the real hub's sendStateTo does) and starts a read pump draining the
// handler's enqueued commands. Returns nil — the handler never calls methods on
// the *hub.Client it stores.
func (s *fakeSession) AddClient(conn hub.WSConn, _ *slog.Logger) *hub.Client {
	if s.welcome != "" {
		// Deliver the welcome asynchronously, mirroring the hub's write pump so
		// it lands after AddClient returns (the join path sets its flags before
		// calling AddClient, so either ordering is handled).
		go func() {
			evt := protocol.MustEvent(protocol.EventSessionState, 0, protocol.SessionStatePayload{
				SessionID: s.id, State: s.welcome, Clients: 1,
			})
			if data, err := protocol.MarshalEvent(evt); err == nil {
				_ = conn.WriteMessage(hub.TextMessage, data)
			}
		}()
	}
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			s.cmds.add(data)
		}
	}()
	return nil
}

type fakeProvider struct{ sess *fakeSession }

func (p *fakeProvider) CreateSession() (Session, error)   { return p.sess, nil }
func (p *fakeProvider) GetSession(string) (Session, bool) { return p.sess, true }

// harness wires a Handler to a loopback TCP socket so the test can speak real
// DAP wire messages to it and inject bingo events via WriteMessage.
type harness struct {
	t       *testing.T
	handler *Handler
	client  net.Conn
	reader  *bufio.Reader
	cmds    *cmdRecorder
	seq     int
}

func newHarness(t *testing.T) *harness {
	return newHarnessWelcome(t, "")
}

// newHarnessWelcome builds a harness whose fake session delivers the given
// welcome state to a newly-added client (the empty string sends none).
func newHarnessWelcome(t *testing.T, welcome protocol.SessionState) *harness {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	rec := &cmdRecorder{}
	prov := &fakeProvider{sess: &fakeSession{id: "sess-test", cmds: rec, welcome: welcome}}

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		accepted <- c
	}()

	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverConn := <-accepted

	h := NewHandler(serverConn, prov, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	go h.Serve()

	t.Cleanup(func() {
		_ = client.Close()
		_ = h.Close()
	})

	return &harness{t: t, handler: h, client: client, reader: bufio.NewReader(client), cmds: rec}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// sendReq writes a DAP request with an auto-incrementing seq, returning that seq.
func (hh *harness) sendReq(command string, m godap.RequestMessage) int {
	hh.t.Helper()
	hh.seq++
	req := m.GetRequest()
	req.Seq = hh.seq
	req.Type = "request"
	req.Command = command
	if err := godap.WriteProtocolMessage(hh.client, m); err != nil {
		hh.t.Fatalf("write %s: %v", command, err)
	}
	return hh.seq
}

// recv reads the next DAP message with a read deadline.
func (hh *harness) recv() godap.Message {
	hh.t.Helper()
	_ = hh.client.SetReadDeadline(time.Now().Add(2 * time.Second))
	m, err := godap.ReadProtocolMessage(hh.reader)
	if err != nil {
		hh.t.Fatalf("recv: %v", err)
	}
	return m
}

// recvType reads until a message of type T arrives (skipping others), or fails.
func recvType[T godap.Message](hh *harness) T {
	hh.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m := hh.recv()
		if typed, ok := m.(T); ok {
			return typed
		}
	}
	var zero T
	hh.t.Fatalf("timed out waiting for message type %T", zero)
	return zero
}

// inject delivers a bingo event to the handler as the hub write pump would.
func (hh *harness) inject(kind protocol.EventKind, payload any) {
	hh.t.Helper()
	evt := protocol.MustEvent(kind, 1, payload)
	data, err := protocol.MarshalEvent(evt)
	if err != nil {
		hh.t.Fatal(err)
	}
	if err := hh.handler.WriteMessage(hub.TextMessage, data); err != nil {
		hh.t.Fatal(err)
	}
}

func initArgs() *godap.InitializeRequest {
	return &godap.InitializeRequest{Arguments: godap.InitializeRequestArguments{AdapterID: "bingo"}}
}

// doHandshake runs initialize→launch→entry→configurationDone (no stopOnEntry),
// leaving the session running with a pending Continue suppressed.
func (hh *harness) doHandshake(t *testing.T) {
	t.Helper()
	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)

	lr := &godap.LaunchRequest{Arguments: json.RawMessage(`{"program":"/bin/x","stopOnEntry":false}`)}
	hh.sendReq("launch", lr)

	// Entry stop → handler emits `initialized`.
	hh.cmds.waitForCommand(t, protocol.CmdLaunch)
	hh.inject(protocol.EventStepped, protocol.SteppedPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.InitializedEvent](hh)

	hh.sendReq("configurationDone", &godap.ConfigurationDoneRequest{})
	_ = recvType[*godap.ConfigurationDoneResponse](hh)
	_ = recvType[*godap.LaunchResponse](hh)
	// configurationDone with !stopOnEntry enqueues a Continue.
	hh.cmds.waitForCommand(t, protocol.CmdContinue)
	// Suppress our own continue.
	hh.inject(protocol.EventContinued, protocol.ContinuedPayload{})
}

func TestHandshakeLaunchToBreakpoint(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)

	// Program runs to a breakpoint on goroutine 5.
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{
		Goroutine:  protocol.Goroutine{ID: 5},
		Breakpoint: protocol.Breakpoint{Location: protocol.Location{File: "/x/main.go", Line: 12}},
	})
	stopped := recvType[*godap.StoppedEvent](hh)
	if stopped.Body.Reason != "breakpoint" {
		t.Errorf("reason = %q, want breakpoint", stopped.Body.Reason)
	}
	if stopped.Body.ThreadId != 5 {
		t.Errorf("threadId = %d, want 5", stopped.Body.ThreadId)
	}
}

func TestOwnContinueIsSuppressed(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	// Client-driven continue: response, then EventContinued must be suppressed.
	hh.sendReq("continue", &godap.ContinueRequest{})
	_ = recvType[*godap.ContinueResponse](hh)
	hh.cmds.waitForCommand(t, protocol.CmdContinue)
	hh.inject(protocol.EventContinued, protocol.ContinuedPayload{})

	// The next real event must be the exit, NOT a `continued` (which would mean
	// our own resume leaked through).
	hh.inject(protocol.EventProcessExited, protocol.ProcessExitedPayload{ExitCode: 0})
	_ = recvType[*godap.ExitedEvent](hh)
}

func TestOutOfBandContinueSurfaces(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 3}})
	_ = recvType[*godap.StoppedEvent](hh)

	// A WebSocket client drove Continue — the DAP adapter did NOT, so
	// pendingContinues is 0 and the EventContinued must surface as `continued`.
	hh.inject(protocol.EventContinued, protocol.ContinuedPayload{})
	cont := recvType[*godap.ContinuedEvent](hh)
	if cont.Body.ThreadId != 3 {
		t.Errorf("continued threadId = %d, want 3", cont.Body.ThreadId)
	}
}

func TestSetBreakpointsDiffAndFIFO(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	// Request two breakpoints in one setBreakpoints call.
	sb := &godap.SetBreakpointsRequest{Arguments: godap.SetBreakpointsArguments{
		Source:      godap.Source{Path: "/x/main.go", Name: "main.go"},
		Breakpoints: []godap.SourceBreakpoint{{Line: 10}, {Line: 20}},
	}}
	hh.sendReq("setBreakpoints", sb)

	// Two SetBreakpoint commands enqueued; confirm in order.
	hh.cmds.waitForCommand(t, protocol.CmdSetBreakpoint)
	hh.inject(protocol.EventBreakpointSet, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 101, Location: protocol.Location{File: "/x/main.go", Line: 10}},
	})
	hh.inject(protocol.EventBreakpointSet, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 102, Location: protocol.Location{File: "/x/main.go", Line: 20}},
	})

	resp := recvType[*godap.SetBreakpointsResponse](hh)
	if len(resp.Body.Breakpoints) != 2 {
		t.Fatalf("got %d breakpoints, want 2", len(resp.Body.Breakpoints))
	}
	if !resp.Body.Breakpoints[0].Verified || resp.Body.Breakpoints[0].Line != 10 {
		t.Errorf("bp0 = %+v", resp.Body.Breakpoints[0])
	}
	if resp.Body.Breakpoints[1].Id != 102 || resp.Body.Breakpoints[1].Line != 20 {
		t.Errorf("bp1 = %+v", resp.Body.Breakpoints[1])
	}

	// Now clear line 10, keep line 20: a diffing setBreakpoints with only {20}.
	sb2 := &godap.SetBreakpointsRequest{Arguments: godap.SetBreakpointsArguments{
		Source:      godap.Source{Path: "/x/main.go", Name: "main.go"},
		Breakpoints: []godap.SourceBreakpoint{{Line: 20}},
	}}
	hh.sendReq("setBreakpoints", sb2)
	// Line 20 unchanged → resolves immediately without a new SetBreakpoint.
	resp2 := recvType[*godap.SetBreakpointsResponse](hh)
	if len(resp2.Body.Breakpoints) != 1 || resp2.Body.Breakpoints[0].Line != 20 {
		t.Fatalf("diff response = %+v", resp2.Body.Breakpoints)
	}
	// A ClearBreakpoint for the removed line 10 must have been enqueued.
	hh.cmds.waitForCommand(t, protocol.CmdClearBreakpoint)
}

func TestStackTraceAndVariablesCorrelation(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	// threads → Goroutines command → EventGoroutines → ThreadsResponse.
	hh.sendReq("threads", &godap.ThreadsRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdGoroutines)
	hh.inject(protocol.EventGoroutines, protocol.GoroutinesPayload{
		Goroutines: []protocol.Goroutine{{ID: 1, Status: "running"}},
	})
	thr := recvType[*godap.ThreadsResponse](hh)
	if len(thr.Body.Threads) != 1 || thr.Body.Threads[0].Id != 1 {
		t.Fatalf("threads = %+v", thr.Body.Threads)
	}

	// stackTrace → Frames command → EventFrames → StackTraceResponse.
	hh.sendReq("stackTrace", &godap.StackTraceRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdFrames)
	hh.inject(protocol.EventFrames, protocol.FramesPayload{Frames: []protocol.Frame{
		{Index: 0, Location: protocol.Location{Function: "main.f", File: "/x/main.go", Line: 12}},
	}})
	st := recvType[*godap.StackTraceResponse](hh)
	if len(st.Body.StackFrames) != 1 || st.Body.StackFrames[0].Id != 1 {
		t.Fatalf("frames = %+v", st.Body.StackFrames)
	}

	// scopes → synthetic Locals scope with variablesReference == frameId.
	hh.sendReq("scopes", &godap.ScopesRequest{Arguments: godap.ScopesArguments{FrameId: 1}})
	sc := recvType[*godap.ScopesResponse](hh)
	if len(sc.Body.Scopes) != 1 || sc.Body.Scopes[0].VariablesReference != 1 {
		t.Fatalf("scopes = %+v", sc.Body.Scopes)
	}

	// variables → Locals command (frameIndex 0) → EventLocals → VariablesResponse.
	hh.sendReq("variables", &godap.VariablesRequest{Arguments: godap.VariablesArguments{VariablesReference: 1}})
	locCmd := hh.cmds.waitForCommand(t, protocol.CmdLocals)
	var lp protocol.LocalsPayloadCmd
	if err := protocol.DecodeCommandPayload(locCmd, &lp); err != nil {
		t.Fatal(err)
	}
	if lp.FrameIndex != 0 {
		t.Errorf("frameIndex = %d, want 0", lp.FrameIndex)
	}
	hh.inject(protocol.EventLocals, protocol.LocalsPayload{Variables: []protocol.Variable{{Name: "x", Value: "0x2a", Type: "int"}}})
	vr := recvType[*godap.VariablesResponse](hh)
	if len(vr.Body.Variables) != 1 || vr.Body.Variables[0].Name != "x" {
		t.Fatalf("variables = %+v", vr.Body.Variables)
	}
}

func TestStepEmitsStoppedStep(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	hh.sendReq("next", &godap.NextRequest{Arguments: godap.NextArguments{ThreadId: 1}})
	_ = recvType[*godap.NextResponse](hh)
	hh.cmds.waitForCommand(t, protocol.CmdStepOver)

	hh.inject(protocol.EventStepped, protocol.SteppedPayload{Goroutine: protocol.Goroutine{ID: 1}})
	stopped := recvType[*godap.StoppedEvent](hh)
	if stopped.Body.Reason != "step" {
		t.Errorf("reason = %q, want step", stopped.Body.Reason)
	}
}

func TestDisconnectTerminatesLaunchedDebuggee(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)

	hh.sendReq("disconnect", &godap.DisconnectRequest{})
	_ = recvType[*godap.DisconnectResponse](hh)
	// A launch session terminates the debuggee: a Kill must be enqueued.
	hh.cmds.waitForCommand(t, protocol.CmdKill)
}

// TestJoinExistingSuspendedSession drives the JOIN path: attach with a session
// id and no pid registers as an additional client on an already-suspended
// session WITHOUT relaunching it, reflects the welcome as an initial
// stopped(pause), lets the joiner inspect and drive, and never enqueues a
// Launch/Attach.
func TestJoinExistingSuspendedSession(t *testing.T) {
	hh := newHarnessWelcome(t, protocol.StateSuspended)

	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)

	ar := &godap.AttachRequest{Arguments: json.RawMessage(`{"session":"sess-test"}`)}
	joinSeq := hh.sendReq("attach", ar)

	// The join emits `initialized` immediately (no entry stop to wait for) and
	// translates the suspended welcome into an initial stopped(pause). Order
	// between the two is not guaranteed, so collect both.
	var sawInit, sawStopped bool
	deadline := time.Now().Add(2 * time.Second)
	for (!sawInit || !sawStopped) && time.Now().Before(deadline) {
		switch ev := hh.recv().(type) {
		case *godap.InitializedEvent:
			sawInit = true
		case *godap.StoppedEvent:
			sawStopped = true
			if ev.Body.Reason != "pause" {
				t.Errorf("stopped reason = %q, want pause", ev.Body.Reason)
			}
		}
	}
	if !sawInit || !sawStopped {
		t.Fatalf("handshake incomplete: initialized=%v stopped=%v", sawInit, sawStopped)
	}

	// configurationDone completes the join WITHOUT resuming the shared session.
	hh.sendReq("configurationDone", &godap.ConfigurationDoneRequest{})
	_ = recvType[*godap.ConfigurationDoneResponse](hh)
	resp := recvType[*godap.AttachResponse](hh)
	if resp.RequestSeq != joinSeq {
		t.Errorf("attach response seq = %d, want %d", resp.RequestSeq, joinSeq)
	}

	// A joiner must not launch or attach: no such command may have been enqueued.
	for _, k := range hh.cmds.kinds() {
		if k == protocol.CmdLaunch || k == protocol.CmdAttach || k == protocol.CmdContinue {
			t.Fatalf("join enqueued %s; joiners must not launch/attach/resume", k)
		}
	}

	// The joiner can inspect the shared suspended session.
	hh.sendReq("threads", &godap.ThreadsRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdGoroutines)
	hh.inject(protocol.EventGoroutines, protocol.GoroutinesPayload{
		Goroutines: []protocol.Goroutine{{ID: 7, Status: "running"}},
	})
	thr := recvType[*godap.ThreadsResponse](hh)
	if len(thr.Body.Threads) != 1 || thr.Body.Threads[0].Id != 7 {
		t.Fatalf("threads = %+v", thr.Body.Threads)
	}

	// Driving continue from the joiner resumes the shared session.
	hh.sendReq("continue", &godap.ContinueRequest{})
	_ = recvType[*godap.ContinueResponse](hh)
	hh.cmds.waitForCommand(t, protocol.CmdContinue)
}
