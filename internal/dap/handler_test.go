package dap

import (
	"bufio"
	"encoding/json"
	"errors"
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
	addErr  error

	addStarted chan struct{}
	allowAdd   <-chan struct{}
	addOnce    sync.Once
}

func (s *fakeSession) SessionID() string { return s.id }

// AddClient mirrors the hub: it optionally delivers a welcome EventSessionState
// (as the real hub's sendStateTo does) and starts a read pump draining the
// handler's enqueued commands. The handler never calls methods on the
// *hub.Client it stores.
func (s *fakeSession) AddClient(conn hub.WSConn, _ *slog.Logger) (*hub.Client, error) {
	if s.addStarted != nil {
		s.addOnce.Do(func() { close(s.addStarted) })
	}
	if s.allowAdd != nil {
		<-s.allowAdd
	}
	if s.addErr != nil {
		_ = conn.Close()
		return nil, s.addErr
	}
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
			if s.cmds != nil {
				s.cmds.add(data)
			}
		}
	}()
	return nil, nil
}

type fakeProvider struct {
	sess      *fakeSession
	createErr error
}

func (p *fakeProvider) CreateSession() (Session, error) {
	if p.createErr != nil {
		return nil, p.createErr
	}
	return p.sess, nil
}

func (p *fakeProvider) GetSession(string) (Session, bool) {
	return p.sess, p.sess != nil
}

func TestStartSessionRejectsClosedHub(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()

	session := &fakeSession{
		id:     "closed-session",
		cmds:   &cmdRecorder{},
		addErr: hub.ErrHubClosed,
	}
	handler := NewHandler(serverConn, &fakeProvider{sess: session}, nil)
	defer func() { _ = handler.Close() }()

	err := handler.startSession("")
	if !errors.Is(err, hub.ErrHubClosed) {
		t.Fatalf("expected ErrHubClosed, got %v", err)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.session != nil || handler.client != nil || handler.sessionStarting || handler.sessionAnnounced {
		t.Fatal("rejected session was retained by DAP handler")
	}
}

// harness wires a Handler to a loopback TCP socket so the test can speak real
// DAP wire messages to it and inject bingo events via WriteMessage.
type harness struct {
	t       *testing.T
	handler *Handler
	client  net.Conn
	reader  *bufio.Reader
	codec   *godap.Codec
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
	rec := &cmdRecorder{}
	prov := &fakeProvider{sess: &fakeSession{id: "sess-test", cmds: rec, welcome: welcome}}
	return newHarnessProvider(t, prov, rec)
}

func newHarnessProvider(t *testing.T, prov Provider, rec *cmdRecorder) *harness {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

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

	codec := godap.NewCodec()
	if err := codec.RegisterEvent(sessionEventName, func() godap.Message { return new(sessionEvent) }); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = client.Close()
		_ = h.Close()
	})

	return &harness{t: t, handler: h, client: client, reader: bufio.NewReader(client), codec: codec, cmds: rec}
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
	data, err := godap.ReadBaseMessage(hh.reader)
	if err != nil {
		hh.t.Fatalf("recv: %v", err)
	}
	m, err := hh.codec.DecodeMessage(data)
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

func TestLaunchAnnouncesManagedSessionAfterClientAttach(t *testing.T) {
	rec := &cmdRecorder{}
	addStarted := make(chan struct{})
	allowAdd := make(chan struct{})
	prov := &fakeProvider{sess: &fakeSession{
		id:         "sess-launch",
		cmds:       rec,
		addStarted: addStarted,
		allowAdd:   allowAdd,
	}}
	hh := newHarnessProvider(t, prov, rec)

	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)
	hh.sendReq("launch", &godap.LaunchRequest{Arguments: json.RawMessage(`{"program":"/bin/x"}`)})

	select {
	case <-addStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("AddClient was not called")
	}
	_ = hh.client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := hh.reader.Peek(1); err == nil {
		t.Fatal("session event arrived before AddClient returned")
	} else if nerr, ok := err.(net.Error); !ok || !nerr.Timeout() {
		t.Fatalf("peek before AddClient returned: %v", err)
	}
	_ = hh.client.SetReadDeadline(time.Time{})

	close(allowAdd)
	event := recvType[*sessionEvent](hh)
	if event.Body != (sessionEventBody{Version: 1, SessionID: "sess-launch"}) {
		t.Fatalf("session event body = %+v", event.Body)
	}
	wire, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"seq":2,"type":"event","event":"bingo/session/v1","body":{"version":1,"sessionId":"sess-launch"}}`
	if string(wire) != want {
		t.Fatalf("session event JSON = %s, want %s", wire, want)
	}

	console := recvType[*godap.OutputEvent](hh)
	if console.Body.Category != "console" ||
		console.Body.Output != "bingo session sess-launch ready — observers can join with ?session=sess-launch\n" {
		t.Fatalf("console announcement = %+v", console.Body)
	}
	hh.cmds.waitForCommand(t, protocol.CmdLaunch)
}

func TestJoinAnnouncesManagedSession(t *testing.T) {
	hh := newHarness(t)
	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)

	hh.sendReq("attach", &godap.AttachRequest{Arguments: json.RawMessage(`{"session":"sess-test"}`)})
	event := recvType[*sessionEvent](hh)
	if event.Event.Event != sessionEventName {
		t.Fatalf("event name = %q, want %q", event.Event.Event, sessionEventName)
	}
	if event.Body != (sessionEventBody{Version: 1, SessionID: "sess-test"}) {
		t.Fatalf("session event body = %+v", event.Body)
	}
	_ = recvType[*godap.OutputEvent](hh)
	_ = recvType[*godap.InitializedEvent](hh)
}

func TestSessionAnnouncementIsSingleWinnerUnderRace(t *testing.T) {
	hh := newHarness(t)

	const attempts = 32
	start := make(chan struct{})
	errs := make(chan error, attempts)
	var starts sync.WaitGroup
	for range attempts {
		starts.Add(1)
		go func() {
			defer starts.Done()
			<-start
			errs <- hh.handler.startSession("")
		}()
	}
	close(start)
	starts.Wait()
	close(errs)

	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent starts = %d, want 1", successes)
	}

	var announcements sync.WaitGroup
	for range attempts {
		announcements.Add(1)
		go func() {
			defer announcements.Done()
			hh.handler.announceSession()
		}()
	}
	event := recvType[*sessionEvent](hh)
	if event.Body.SessionID != "sess-test" {
		t.Fatalf("session id = %q, want sess-test", event.Body.SessionID)
	}
	_ = recvType[*godap.OutputEvent](hh)
	announcements.Wait()

	_ = hh.client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := hh.reader.Peek(1); err == nil {
		t.Fatal("duplicate session announcement received")
	} else if nerr, ok := err.(net.Error); !ok || !nerr.Timeout() {
		t.Fatalf("peek after announcements: %v", err)
	}
}

func TestSessionEventNotEmittedWhenCreationFails(t *testing.T) {
	hh := newHarnessProvider(t, &fakeProvider{createErr: errors.New("create failed")}, &cmdRecorder{})
	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)

	hh.sendReq("launch", &godap.LaunchRequest{Arguments: json.RawMessage(`{"program":"/bin/x"}`)})
	resp := recvType[*godap.ErrorResponse](hh)
	if resp.Success || resp.Command != "launch" {
		t.Fatalf("launch response = %+v", resp.Response)
	}
	_ = hh.client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := hh.reader.Peek(1); err == nil {
		t.Fatal("session event emitted after failed creation")
	} else if nerr, ok := err.(net.Error); !ok || !nerr.Timeout() {
		t.Fatalf("peek after failed creation: %v", err)
	}
}

func TestSessionEventNotEmittedWhenJoinFails(t *testing.T) {
	hh := newHarnessProvider(t, &fakeProvider{}, &cmdRecorder{})
	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)

	hh.sendReq("attach", &godap.AttachRequest{Arguments: json.RawMessage(`{"session":"missing"}`)})
	resp := recvType[*godap.ErrorResponse](hh)
	if resp.Success || resp.Command != "attach" {
		t.Fatalf("attach response = %+v", resp.Response)
	}
	_ = hh.client.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := hh.reader.Peek(1); err == nil {
		t.Fatal("session event emitted after failed join")
	} else if nerr, ok := err.(net.Error); !ok || !nerr.Timeout() {
		t.Fatalf("peek after failed join: %v", err)
	}
}

func TestLaunchIgnoresVSCodeEndpointFields(t *testing.T) {
	hh := newHarness(t)
	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)

	// Endpoint and lifecycle fields select or start the server before DAP begins,
	// but VS Code also leaves them in launch arguments. They must not alter the
	// bingo command.
	hh.sendReq("launch", &godap.LaunchRequest{Arguments: json.RawMessage(
		`{"program":"/bin/x","args":["one"],"env":["BINGO_TEST=1"],"serverMode":"auto","managementHost":"127.0.0.1","managementPort":6060,"dapHost":"127.0.0.1","dapPort":4711,"serverReadyTimeoutMs":5000,"managedIdleTimeoutMs":30000}`,
	)})

	cmd := hh.cmds.waitForCommand(t, protocol.CmdLaunch)
	var payload protocol.LaunchPayload
	if err := protocol.DecodeCommandPayload(cmd, &payload); err != nil {
		t.Fatalf("decode launch payload: %v", err)
	}
	if payload.Program != "/bin/x" {
		t.Errorf("program = %q, want /bin/x", payload.Program)
	}
	if len(payload.Args) != 1 || payload.Args[0] != "one" {
		t.Errorf("args = %v, want [one]", payload.Args)
	}
	if len(payload.Env) != 1 || payload.Env[0] != "BINGO_TEST=1" {
		t.Errorf("env = %v, want [BINGO_TEST=1]", payload.Env)
	}
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

// TestInitializeAdvertisesEvaluateForHovers pins the new capability.
func TestInitializeAdvertisesEvaluateForHovers(t *testing.T) {
	hh := newHarness(t)
	hh.sendReq("initialize", initArgs())
	resp := recvType[*godap.InitializeResponse](hh)
	if !resp.Body.SupportsEvaluateForHovers {
		t.Errorf("SupportsEvaluateForHovers = false, want true")
	}
}

// TestVariablesExpandsNestedStruct proves a struct local returned in EventLocals
// with Children is served with a fresh child variablesReference, and that a
// follow-up variables request on that ref returns the cached children WITHOUT a
// second CmdLocals round-trip.
func TestVariablesExpandsNestedStruct(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	// variables on the frame-root ref → CmdLocals → EventLocals (nested struct).
	hh.sendReq("variables", &godap.VariablesRequest{Arguments: godap.VariablesArguments{VariablesReference: 1}})
	hh.cmds.waitForCommand(t, protocol.CmdLocals)
	hh.inject(protocol.EventLocals, protocol.LocalsPayload{Variables: []protocol.Variable{
		{Name: "n", Value: "42", Type: "int"},
		{Name: "p", Value: "main.Point{X:1, Y:2}", Type: "main.Point", Kind: "struct", Children: []protocol.Variable{
			{Name: "X", Value: "1", Type: "int"},
			{Name: "Y", Value: "2", Type: "int"},
		}},
	}})
	vr := recvType[*godap.VariablesResponse](hh)
	if len(vr.Body.Variables) != 2 {
		t.Fatalf("got %d vars, want 2", len(vr.Body.Variables))
	}
	if vr.Body.Variables[0].VariablesReference != 0 {
		t.Errorf("scalar var has ref %d, want 0", vr.Body.Variables[0].VariablesReference)
	}
	childRef := vr.Body.Variables[1].VariablesReference
	if childRef == 0 {
		t.Fatalf("struct var has no child ref")
	}

	// Expanding the child ref is a synchronous cache hit — NO new CmdLocals.
	before := len(hh.cmds.kinds())
	hh.sendReq("variables", &godap.VariablesRequest{Arguments: godap.VariablesArguments{VariablesReference: childRef}})
	child := recvType[*godap.VariablesResponse](hh)
	if len(child.Body.Variables) != 2 || child.Body.Variables[0].Name != "X" || child.Body.Variables[1].Name != "Y" {
		t.Fatalf("expanded struct = %+v", child.Body.Variables)
	}
	if after := len(hh.cmds.kinds()); after != before {
		t.Errorf("child expansion enqueued %d commands, want 0 (cache hit)", after-before)
	}
}

// TestEvaluateName drives evaluate(name)→value and evalQ correlation, including
// a nested result that yields an expandable child ref.
func TestEvaluateName(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	hh.sendReq("evaluate", &godap.EvaluateRequest{Arguments: godap.EvaluateArguments{
		Expression: "p", FrameId: 1, Context: "hover",
	}})
	evalCmd := hh.cmds.waitForCommand(t, protocol.CmdEvaluate)
	var ep protocol.EvaluatePayloadCmd
	if err := protocol.DecodeCommandPayload(evalCmd, &ep); err != nil {
		t.Fatal(err)
	}
	if ep.Name != "p" || ep.FrameIndex != 0 {
		t.Errorf("evaluate cmd = %+v, want {0 p}", ep)
	}

	hh.inject(protocol.EventEvaluate, protocol.EvaluatePayload{Result: protocol.Variable{
		Name: "p", Value: "main.Point{X:1, Y:2}", Type: "main.Point", Kind: "struct",
		Children: []protocol.Variable{{Name: "X", Value: "1", Type: "int"}},
	}})
	resp := recvType[*godap.EvaluateResponse](hh)
	if resp.Body.Result != "main.Point{X:1, Y:2}" || resp.Body.Type != "main.Point" {
		t.Errorf("evaluate response = %+v", resp.Body)
	}
	if resp.Body.VariablesReference == 0 {
		t.Errorf("nested evaluate result has no child ref")
	}
}

// TestEvaluateNotSuspended returns a best-effort empty result rather than
// enqueuing a command when the tracee is running.
func TestEvaluateNotSuspended(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t) // leaves the session running

	hh.sendReq("evaluate", &godap.EvaluateRequest{Arguments: godap.EvaluateArguments{Expression: "x", FrameId: 1}})
	resp := recvType[*godap.EvaluateResponse](hh)
	if resp.Body.Result != "" || resp.Body.VariablesReference != 0 {
		t.Errorf("not-suspended evaluate = %+v, want empty", resp.Body)
	}
	for _, k := range hh.cmds.kinds() {
		if k == protocol.CmdEvaluate {
			t.Fatalf("evaluate while running enqueued CmdEvaluate")
		}
	}
}

// TestEvaluateErrorPath pops evalQ and errors the request when the engine
// reports the name is not in scope.
func TestEvaluateErrorPath(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	seq := hh.sendReq("evaluate", &godap.EvaluateRequest{Arguments: godap.EvaluateArguments{Expression: "nope", FrameId: 1}})
	hh.cmds.waitForCommand(t, protocol.CmdEvaluate)
	hh.inject(protocol.EventError, protocol.ErrorPayload{Command: protocol.CmdEvaluate, Message: `no variable named "nope" in scope`})
	er := recvType[*godap.ErrorResponse](hh)
	if er.RequestSeq != seq || er.Success {
		t.Errorf("error response = %+v, want failure for seq %d", er.Response, seq)
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

// TestThreadsFromPackedGoroutines proves the DAP threads response carries the
// real (truncated) goroutine list rather than falling back to the synthetic
// single "main" thread. The synthetic fallback exists for a genuinely empty
// list; a bounded list is real data and must survive translation intact.
// See issue #194 — a producer that dropped an oversized list instead of packing
// it would make the IDE claim a 5,000-goroutine process has one thread.
func TestThreadsFromPackedGoroutines(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	raw := make([]protocol.Goroutine, 0, 8192)
	for i := 1; i <= 8192; i++ {
		raw = append(raw, protocol.Goroutine{
			ID:         i,
			Status:     "waiting",
			WaitReason: "chan receive",
			CurrentLoc: protocol.Location{
				File:     "/home/runner/go/src/github.com/bingosuite/bingo/internal/service/handler.go",
				Line:     1234,
				Function: "github.com/bingosuite/bingo/internal/service.(*Handler).Serve.func1",
			},
			StartLoc:   protocol.Location{File: "/home/runner/go/src/github.com/bingosuite/bingo/internal/service/handler.go", Line: 88},
			CreatedLoc: protocol.Location{File: "/home/runner/go/src/github.com/bingosuite/bingo/internal/service/handler.go", Line: 91},
		})
	}
	packed, report := protocol.PackGoroutines(raw, true)
	if report.Degraded || len(packed.Goroutines) < 2 {
		t.Fatalf("packed = %d goroutines, degraded=%v; want a real truncated list", len(packed.Goroutines), report.Degraded)
	}
	if len(packed.Goroutines) >= len(raw) {
		t.Fatalf("packed %d of %d; the input must actually be too big", len(packed.Goroutines), len(raw))
	}
	if packed.Totals == nil || packed.Totals.Goroutines != len(raw) || !packed.Totals.GoroutinesClipped {
		t.Fatalf("totals = %+v; want the original count and the clipped flag", packed.Totals)
	}

	hh.sendReq("threads", &godap.ThreadsRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdGoroutines)
	hh.inject(protocol.EventGoroutines, packed)

	thr := recvType[*godap.ThreadsResponse](hh)
	if len(thr.Body.Threads) != len(packed.Goroutines) {
		t.Fatalf("threads = %d; want the %d packed goroutines", len(thr.Body.Threads), len(packed.Goroutines))
	}
	if len(thr.Body.Threads) == 1 && thr.Body.Threads[0].Name == "main" {
		t.Fatal("threads fell back to the synthetic main thread")
	}
	if thr.Body.Threads[0].Id != packed.Goroutines[0].ID {
		t.Fatalf("threads[0].Id = %d; want goid %d", thr.Body.Threads[0].Id, packed.Goroutines[0].ID)
	}

	// The DAP shape is cheap: the lean {id,status} form of the entire 8192-entry
	// set is nowhere near the byte budget, so what bounds it is the element
	// count cap alone — an IDE never loses threads to sheer payload size.
	full, fullReport := protocol.PackGoroutines(dapShaped(raw), false)
	if len(full.Goroutines) != protocol.MaxSnapshotGoroutines {
		t.Fatalf("DAP-shaped pack kept %d; want the count cap %d", len(full.Goroutines), protocol.MaxSnapshotGoroutines)
	}
	if fullReport.Bytes >= protocol.MaxGoroutineEventBytes/2 {
		t.Fatalf("DAP-shaped pack used %d bytes; the lean shape must stay far below the budget", fullReport.Bytes)
	}
}

// dapShaped strips a goroutine down to what a DAP thread actually uses, which
// is what makes the 8192-thread list fit comfortably inside the budget.
func dapShaped(gs []protocol.Goroutine) []protocol.Goroutine {
	out := make([]protocol.Goroutine, 0, len(gs))
	for _, g := range gs {
		out = append(out, protocol.Goroutine{ID: g.ID, Status: g.Status})
	}
	return out
}
