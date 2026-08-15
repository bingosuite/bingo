package dap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/internal/debugger"
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

func (r *cmdRecorder) count(kind protocol.CommandKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var count int
	for _, cmd := range r.cmds {
		if cmd.Kind == kind {
			count++
		}
	}
	return count
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

func (r *cmdRecorder) waitForCommands(t *testing.T, kind protocol.CommandKind, count int) []protocol.Command {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		var matches []protocol.Command
		for _, cmd := range r.cmds {
			if cmd.Kind == kind {
				matches = append(matches, cmd)
			}
		}
		r.mu.Unlock()
		if len(matches) >= count {
			return matches
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d commands of kind %s; saw %v", count, kind, r.kinds())
	return nil
}

func (r *cmdRecorder) requireNoAdditionalCommands(t *testing.T, kind protocol.CommandKind, expected int) {
	t.Helper()
	deadline := time.Now().Add(50 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := r.count(kind); got != expected {
			t.Fatalf("%s command count = %d, want %d throughout quiet period; saw %v", kind, got, expected, r.kinds())
		}
		time.Sleep(time.Millisecond)
	}
	if got := r.count(kind); got != expected {
		t.Fatalf("%s command count = %d, want %d after quiet period; saw %v", kind, got, expected, r.kinds())
	}
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

type hubProvider struct {
	session Session
}

func (p *hubProvider) CreateSession() (Session, error) {
	return p.session, nil
}

func (p *hubProvider) GetSession(id string) (Session, bool) {
	return p.session, p.session != nil && p.session.SessionID() == id
}

type gatedBreakpointDebugger struct {
	events chan protocol.Event
	gate   chan struct{}

	started     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once

	mu       sync.Mutex
	setLines []int
}

func newGatedBreakpointDebugger() *gatedBreakpointDebugger {
	return &gatedBreakpointDebugger{
		events:  make(chan protocol.Event, 1),
		gate:    make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (d *gatedBreakpointDebugger) release() {
	d.releaseOnce.Do(func() { close(d.gate) })
}

func (d *gatedBreakpointDebugger) setCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.setLines)
}

func (d *gatedBreakpointDebugger) Launch(string, []string, []string) error {
	d.events <- protocol.MustEvent(protocol.EventStepped, 1,
		protocol.SteppedPayload{Goroutine: protocol.Goroutine{ID: 1}})
	return nil
}

func (*gatedBreakpointDebugger) Attach(int, string) error { return nil }
func (*gatedBreakpointDebugger) Kill() error              { return nil }
func (d *gatedBreakpointDebugger) SetBreakpoint(file string, line int) (protocol.Breakpoint, error) {
	d.startOnce.Do(func() { close(d.started) })
	<-d.gate
	d.mu.Lock()
	d.setLines = append(d.setLines, line)
	d.mu.Unlock()
	return protocol.Breakpoint{
		ID:       line,
		Location: protocol.Location{File: file, Line: line},
	}, nil
}
func (*gatedBreakpointDebugger) ClearBreakpoint(int) error { return nil }
func (*gatedBreakpointDebugger) Continue() error           { return nil }
func (*gatedBreakpointDebugger) StepOver() error           { return nil }
func (*gatedBreakpointDebugger) StepInto() error           { return nil }
func (*gatedBreakpointDebugger) StepOut() error            { return nil }
func (*gatedBreakpointDebugger) Pause() error              { return nil }
func (*gatedBreakpointDebugger) Locals(int) ([]protocol.Variable, error) {
	return nil, nil
}
func (*gatedBreakpointDebugger) Evaluate(int, string) (protocol.Variable, error) {
	return protocol.Variable{}, nil
}
func (*gatedBreakpointDebugger) StackFrames() ([]protocol.Frame, error) {
	return nil, nil
}
func (*gatedBreakpointDebugger) Goroutines() (protocol.GoroutinesPayload, error) {
	return protocol.GoroutinesPayload{}, nil
}
func (*gatedBreakpointDebugger) GoroutineSnapshot() (protocol.GoroutineSnapshotPayload, error) {
	return protocol.GoroutineSnapshotPayload{}, nil
}
func (d *gatedBreakpointDebugger) Events() <-chan protocol.Event { return d.events }

var _ debugger.Debugger = (*gatedBreakpointDebugger)(nil)

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

type nopConn struct{}

func (nopConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (nopConn) Write(p []byte) (int, error)      { return len(p), nil }
func (nopConn) Close() error                     { return nil }
func (nopConn) LocalAddr() net.Addr              { return nil }
func (nopConn) RemoteAddr() net.Addr             { return nil }
func (nopConn) SetDeadline(time.Time) error      { return nil }
func (nopConn) SetReadDeadline(time.Time) error  { return nil }
func (nopConn) SetWriteDeadline(time.Time) error { return nil }

type failWriteConn struct {
	nopConn
	beforeWrite func()
	closed      chan struct{}
}

func (c *failWriteConn) Write([]byte) (int, error) {
	c.beforeWrite()
	return 0, errors.New("forced write failure")
}

func (c *failWriteConn) Close() error {
	close(c.closed)
	return nil
}

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

// expectNoResponse asserts the handler is not answering anything right now — a
// setBreakpoints request must stay open while an operation it owns is in flight.
func (hh *harness) expectNoResponse() {
	hh.t.Helper()
	_ = hh.client.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	defer func() { _ = hh.client.SetReadDeadline(time.Time{}) }()
	if _, err := hh.reader.Peek(1); err == nil {
		hh.t.Fatal("handler responded while an owned breakpoint operation was still pending")
	} else if nerr, ok := err.(net.Error); !ok || !nerr.Timeout() {
		hh.t.Fatalf("peek while pending: %v", err)
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

func TestUnknownBreakpointStopOmitsThreadID(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)

	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{
		Goroutine: protocol.Goroutine{Status: "unknown", Current: true},
	})
	stopped := recvType[*godap.StoppedEvent](hh)
	if stopped.Body.ThreadId != 0 {
		t.Errorf("threadId = %d, want omitted for unknown goroutine", stopped.Body.ThreadId)
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
	// A ClearBreakpoint for the removed line 10 must have been enqueued, and the
	// response must wait for it: the request owns that removal.
	hh.cmds.waitForCommand(t, protocol.CmdClearBreakpoint)
	hh.expectNoResponse()
	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})

	resp2 := recvType[*godap.SetBreakpointsResponse](hh)
	if len(resp2.Body.Breakpoints) != 1 || resp2.Body.Breakpoints[0].Line != 20 {
		t.Fatalf("diff response = %+v", resp2.Body.Breakpoints)
	}
}

func seedRestartBreakpointCache(t *testing.T, hh *harness, source godap.Source) {
	t.Helper()
	hh.sendReq("setBreakpoints", &godap.SetBreakpointsRequest{Arguments: godap.SetBreakpointsArguments{
		Source:      source,
		Breakpoints: []godap.SourceBreakpoint{{Line: 10}, {Line: 20}},
	}})
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 2)
	hh.inject(protocol.EventBreakpointSet, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 41, Location: protocol.Location{File: source.Path, Line: 10}},
	})
	hh.inject(protocol.EventBreakpointSet, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 42, Location: protocol.Location{File: source.Path, Line: 20}},
	})
	initial := recvType[*godap.SetBreakpointsResponse](hh)
	requireBreakpointIDs(t, initial, 41, 42)
}

func requireBreakpointIDs(t *testing.T, response *godap.SetBreakpointsResponse, want ...int) {
	t.Helper()
	if len(response.Body.Breakpoints) != len(want) {
		t.Fatalf("breakpoint count = %d, want %d", len(response.Body.Breakpoints), len(want))
	}
	for i, id := range want {
		if response.Body.Breakpoints[i].Id != id {
			t.Fatalf("breakpoint ids = %+v, want %v", response.Body.Breakpoints, want)
		}
	}
}

func receiveRestartResult(t *testing.T, hh *harness, restartSeq int) *godap.BreakpointEvent {
	t.Helper()
	var changed *godap.BreakpointEvent
	var restarted *godap.RestartResponse
	for range 2 {
		switch msg := hh.recv().(type) {
		case *godap.BreakpointEvent:
			changed = msg
		case *godap.RestartResponse:
			restarted = msg
		}
	}
	if restarted == nil || restarted.RequestSeq != restartSeq || !restarted.Success {
		t.Fatalf("restart response = %+v, want success for request %d", restarted, restartSeq)
	}
	if changed == nil {
		t.Fatal("discarded breakpoint did not emit a breakpoint event")
	}
	return changed
}

func requireDiscardedBreakpointEvent(t *testing.T, changed *godap.BreakpointEvent) {
	t.Helper()
	if changed.Body.Reason != "changed" ||
		changed.Body.Breakpoint.Id != 42 ||
		changed.Body.Breakpoint.Verified ||
		changed.Body.Breakpoint.Line != 20 ||
		changed.Body.Breakpoint.Message != "no such line" {
		t.Fatalf("discarded breakpoint event = %+v", changed.Body)
	}
}

func requireRestartBreakpointCache(t *testing.T, hh *harness, source godap.Source) {
	t.Helper()
	hh.handler.mu.Lock()
	retained := hh.handler.bpByFile[source.Path][10]
	_, droppedStillCached := hh.handler.bpByFile[source.Path][20]
	hh.handler.mu.Unlock()
	if retained == nil || retained.installedID != 101 || retained.dapID != 41 {
		t.Fatalf("retained breakpoint state = %+v, want installedID=101 dapID=41", retained)
	}
	if droppedStillCached {
		t.Fatal("discarded breakpoint remained in cache")
	}
}

func retryDiscardedBreakpoint(t *testing.T, hh *harness, source godap.Source) {
	t.Helper()
	hh.sendReq("setBreakpoints", &godap.SetBreakpointsRequest{Arguments: godap.SetBreakpointsArguments{
		Source:      source,
		Breakpoints: []godap.SourceBreakpoint{{Line: 10}, {Line: 20}},
	}})
	setCommands := hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 3)
	var retry protocol.SetBreakpointPayload
	if err := protocol.DecodeCommandPayload(setCommands[2], &retry); err != nil {
		t.Fatal(err)
	}
	if retry.File != source.Path || retry.Line != 20 {
		t.Fatalf("discarded breakpoint retry = %+v, want %s:20", retry, source.Path)
	}
	hh.inject(protocol.EventBreakpointSet, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 202, Location: protocol.Location{File: source.Path, Line: 20}},
	})
	retried := recvType[*godap.SetBreakpointsResponse](hh)
	requireBreakpointIDs(t, retried, 41, 202)
}

func clearReidentifiedBreakpoint(t *testing.T, hh *harness, source godap.Source) {
	t.Helper()
	hh.sendReq("setBreakpoints", &godap.SetBreakpointsRequest{Arguments: godap.SetBreakpointsArguments{
		Source:      source,
		Breakpoints: []godap.SourceBreakpoint{{Line: 20}},
	}})
	clearCommands := hh.cmds.waitForCommands(t, protocol.CmdClearBreakpoint, 1)
	var clear protocol.ClearBreakpointPayload
	if err := protocol.DecodeCommandPayload(clearCommands[0], &clear); err != nil {
		t.Fatal(err)
	}
	if clear.ID != 101 {
		t.Fatalf("clear breakpoint id = %d, want fresh debugger id 101", clear.ID)
	}
	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})
	_ = recvType[*godap.SetBreakpointsResponse](hh)
}

func TestRestartReconcilesBreakpointCache(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	source := godap.Source{Path: "/x/main.go", Name: "main.go"}
	seedRestartBreakpointCache(t, hh, source)

	restartSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{
		Breakpoints: []protocol.Breakpoint{
			{ID: 101, Location: protocol.Location{File: source.Path, Line: 10}},
		},
		Discarded: []protocol.DiscardedBreakpoint{
			{Location: protocol.Location{File: source.Path, Line: 20}, Reason: "no such line"},
		},
	})

	changed := receiveRestartResult(t, hh, restartSeq)
	requireDiscardedBreakpointEvent(t, changed)
	requireRestartBreakpointCache(t, hh, source)
	retryDiscardedBreakpoint(t, hh, source)
	clearReidentifiedBreakpoint(t, hh, source)
}

func TestRestartedPayloadDecodeFailureStillResponds(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)

	restartSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventRestarted, map[string]any{"breakpoints": "invalid"})

	restarted := recvType[*godap.RestartResponse](hh)
	if restarted.RequestSeq != restartSeq || !restarted.Success {
		t.Fatalf("restart response = %+v, want success for request %d", restarted.Response, restartSeq)
	}
}

func TestRestartRejectsOverlappingRequest(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)

	firstSeq := hh.sendReq("restart", &godap.RestartRequest{})
	secondSeq := hh.sendReq("restart", &godap.RestartRequest{})
	rejected := recvType[*godap.ErrorResponse](hh)
	if rejected.RequestSeq != secondSeq || rejected.Success || rejected.Message != "restart already in progress" {
		t.Fatalf("overlapping restart response = %+v, want immediate error for request %d", rejected.Response, secondSeq)
	}
	hh.cmds.waitForCommands(t, protocol.CmdRestart, 1)
	hh.cmds.requireNoAdditionalCommands(t, protocol.CmdRestart, 1)

	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{})
	restarted := recvType[*godap.RestartResponse](hh)
	if restarted.RequestSeq != firstSeq || !restarted.Success {
		t.Fatalf("restart response = %+v, want success for original request %d", restarted.Response, firstSeq)
	}
}

func TestRestartAfterSuccessSupersedesPendingEntry(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)

	firstSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{})
	first := recvType[*godap.RestartResponse](hh)
	if first.RequestSeq != firstSeq || !first.Success {
		t.Fatalf("first restart response = %+v, want success for request %d", first.Response, firstSeq)
	}

	secondSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommands(t, protocol.CmdRestart, 2)

	continues := hh.cmds.count(protocol.CmdContinue)
	hh.inject(protocol.EventStepped, protocol.SteppedPayload{Goroutine: protocol.Goroutine{ID: 1}})
	hh.cmds.requireNoAdditionalCommands(t, protocol.CmdContinue, continues)

	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{})
	second := recvType[*godap.RestartResponse](hh)
	if second.RequestSeq != secondSeq || !second.Success {
		t.Fatalf("second restart response = %+v, want success for request %d", second.Response, secondSeq)
	}
	hh.inject(protocol.EventStepped, protocol.SteppedPayload{Goroutine: protocol.Goroutine{ID: 2}})
	hh.cmds.waitForCommands(t, protocol.CmdContinue, continues+1)
	hh.inject(protocol.EventContinued, protocol.ContinuedPayload{})
}

func TestRestartErrorAllowsRetry(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)

	firstSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventError, protocol.ErrorPayload{Command: protocol.CmdRestart, Message: "relaunch failed"})
	failed := recvType[*godap.ErrorResponse](hh)
	if failed.RequestSeq != firstSeq || failed.Success {
		t.Fatalf("failed restart response = %+v, want error for request %d", failed.Response, firstSeq)
	}

	secondSeq := hh.sendReq("restart", &godap.RestartRequest{})
	if commands := hh.cmds.waitForCommands(t, protocol.CmdRestart, 2); len(commands) != 2 {
		t.Fatalf("restart commands after retry = %d, want 2", len(commands))
	}
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{})
	restarted := recvType[*godap.RestartResponse](hh)
	if restarted.RequestSeq != secondSeq || !restarted.Success {
		t.Fatalf("retried restart response = %+v, want success for request %d", restarted.Response, secondSeq)
	}
}

func TestRestartPreservesBreakpointCorrelationQueues(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	setOp := &bpOp{file: "/x/main.go", line: 10}
	clearOp := &bpOp{file: "/x/main.go", line: 20}
	h.setQ = []*bpOp{setOp}
	h.clearQ = []*bpOp{clearOp}

	h.onRestarted(protocol.MustEvent(protocol.EventRestarted, 1, protocol.RestartedPayload{}))

	if len(h.setQ) != 1 || h.setQ[0] != setOp {
		t.Fatalf("setQ changed across restart: %+v", h.setQ)
	}
	if len(h.clearQ) != 1 || h.clearQ[0] != clearOp {
		t.Fatalf("clearQ changed across restart: %+v", h.clearQ)
	}
}

func requireSequentialOpaqueBreakpoints(t *testing.T, got []godap.Breakpoint, count int) map[int]struct{} {
	t.Helper()
	if len(got) != count {
		t.Fatalf("got %d breakpoints, want %d", len(got), count)
	}
	ids := make(map[int]struct{}, count+1)
	for i, bp := range got {
		wantLine := i + 1
		if !bp.Verified || bp.Id <= 0 || bp.Line != wantLine {
			t.Fatalf("breakpoint %d = %+v, want verified line %d with a positive id", i, bp, wantLine)
		}
		if _, duplicate := ids[bp.Id]; duplicate {
			t.Fatalf("breakpoint %d reused id %d", i, bp.Id)
		}
		ids[bp.Id] = struct{}{}
	}
	return ids
}

func TestSetBreakpointsBurstThroughHubPreservesFIFO(t *testing.T) {
	dbg := newGatedBreakpointDebugger()
	defer dbg.release()

	session := hub.NewSession("sess-burst", func() debugger.Debugger { return dbg }, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go session.Run(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-session.Done():
		case <-time.After(2 * time.Second):
			t.Error("hub did not stop")
		}
	})

	hh := newHarnessProvider(t, &hubProvider{session: session}, &cmdRecorder{})
	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)

	launchSeq := hh.sendReq("launch", &godap.LaunchRequest{
		Arguments: json.RawMessage(`{"program":"/bin/x","stopOnEntry":true}`),
	})
	_ = recvType[*godap.InitializedEvent](hh)

	const breakpointCount = 98
	requested := make([]godap.SourceBreakpoint, breakpointCount)
	for i := range requested {
		requested[i] = godap.SourceBreakpoint{Line: i + 1}
	}
	setSeq := hh.sendReq("setBreakpoints", &godap.SetBreakpointsRequest{
		Arguments: godap.SetBreakpointsArguments{
			Source:      godap.Source{Path: "/x/main.go", Name: "main.go"},
			Breakpoints: requested,
		},
	})

	select {
	case <-dbg.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first SetBreakpoint did not reach the debugger")
	}

	// The handler processes requests serially. With 98 commands and a 64-slot
	// cmdOut, this response proves the hub read pump consumed enough commands to
	// saturate its 32-slot queue while the first debugger call remained gated.
	hh.sendReq("scopes", &godap.ScopesRequest{
		Arguments: godap.ScopesArguments{FrameId: 1},
	})
	_ = recvType[*godap.ScopesResponse](hh)

	dbg.release()

	resp := recvType[*godap.SetBreakpointsResponse](hh)
	if resp.RequestSeq != setSeq {
		t.Fatalf("setBreakpoints request seq = %d, want %d", resp.RequestSeq, setSeq)
	}
	ids := requireSequentialOpaqueBreakpoints(t, resp.Body.Breakpoints, breakpointCount)
	if got := dbg.setCount(); got != breakpointCount {
		t.Fatalf("SetBreakpoint calls = %d, want %d", got, breakpointCount)
	}

	requested = append(requested, godap.SourceBreakpoint{Line: 1000})
	nextSeq := hh.sendReq("setBreakpoints", &godap.SetBreakpointsRequest{
		Arguments: godap.SetBreakpointsArguments{
			Source:      godap.Source{Path: "/x/main.go", Name: "main.go"},
			Breakpoints: requested,
		},
	})
	next := recvType[*godap.SetBreakpointsResponse](hh)
	if next.RequestSeq != nextSeq {
		t.Fatalf("next setBreakpoints request seq = %d, want %d", next.RequestSeq, nextSeq)
	}
	last := next.Body.Breakpoints[len(next.Body.Breakpoints)-1]
	if !last.Verified || last.Id <= 0 || last.Line != 1000 {
		t.Fatalf("next breakpoint = %+v, want verified line 1000 with a positive id", last)
	}
	if _, duplicate := ids[last.Id]; duplicate {
		t.Fatalf("next breakpoint reused id %d", last.Id)
	}

	hh.handler.mu.Lock()
	pendingSets := len(hh.handler.setQ)
	hh.handler.mu.Unlock()
	if pendingSets != 0 {
		t.Fatalf("pending set FIFO entries = %d, want 0", pendingSets)
	}

	hh.sendReq("configurationDone", &godap.ConfigurationDoneRequest{})
	_ = recvType[*godap.ConfigurationDoneResponse](hh)
	launch := recvType[*godap.LaunchResponse](hh)
	if launch.RequestSeq != launchSeq {
		t.Fatalf("launch request seq = %d, want %d", launch.RequestSeq, launchSeq)
	}
	_ = recvType[*godap.StoppedEvent](hh)
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
	hh.sendReq("stackTrace", &godap.StackTraceRequest{
		Arguments: godap.StackTraceArguments{ThreadId: 1},
	})
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

func cachedChildRef(t *testing.T, hh *harness) int {
	t.Helper()
	suspendAtBreakpoint(t, hh)
	return cacheChildRef(t, hh, "X")
}

func cacheChildRef(t *testing.T, hh *harness, childName string) int {
	t.Helper()
	locals := hh.cmds.count(protocol.CmdLocals)
	hh.sendReq("variables", &godap.VariablesRequest{Arguments: godap.VariablesArguments{VariablesReference: 1}})
	hh.cmds.waitForCommands(t, protocol.CmdLocals, locals+1)
	hh.inject(protocol.EventLocals, protocol.LocalsPayload{Variables: []protocol.Variable{{
		Name: "p", Value: "main.Point", Type: "main.Point", Kind: "struct",
		Children: []protocol.Variable{{Name: childName, Value: "1", Type: "int"}},
	}}})
	resp := recvType[*godap.VariablesResponse](hh)
	if len(resp.Body.Variables) != 1 || resp.Body.Variables[0].VariablesReference == 0 {
		t.Fatalf("variables = %+v, want one expandable child", resp.Body.Variables)
	}
	return resp.Body.Variables[0].VariablesReference
}

func requireStaleChildRefEmpty(t *testing.T, hh *harness, ref int) {
	t.Helper()
	locals := hh.cmds.count(protocol.CmdLocals)
	hh.sendReq("variables", &godap.VariablesRequest{Arguments: godap.VariablesArguments{VariablesReference: ref}})
	resp := recvType[*godap.VariablesResponse](hh)
	if len(resp.Body.Variables) != 0 {
		t.Fatalf("stale child ref expanded to %+v, want empty", resp.Body.Variables)
	}
	hh.cmds.requireNoAdditionalCommands(t, protocol.CmdLocals, locals)
}

func requireChildRefExpands(t *testing.T, hh *harness, ref int, childName string) {
	t.Helper()
	locals := hh.cmds.count(protocol.CmdLocals)
	hh.sendReq("variables", &godap.VariablesRequest{Arguments: godap.VariablesArguments{VariablesReference: ref}})
	resp := recvType[*godap.VariablesResponse](hh)
	if len(resp.Body.Variables) != 1 || resp.Body.Variables[0].Name != childName {
		t.Fatalf("child ref expanded to %+v, want %q", resp.Body.Variables, childName)
	}
	hh.cmds.requireNoAdditionalCommands(t, protocol.CmdLocals, locals)
}

func TestChildVariableRefIsStaleAfterOwnContinue(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	ref := cachedChildRef(t, hh)

	driveContinue(t, hh)

	requireStaleChildRefEmpty(t, hh, ref)
}

func TestChildVariableRefIsStaleAfterOutOfBandContinue(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	ref := cachedChildRef(t, hh)

	hh.inject(protocol.EventContinued, protocol.ContinuedPayload{})
	_ = recvType[*godap.ContinuedEvent](hh)

	requireStaleChildRefEmpty(t, hh, ref)
}

func TestChildVariableRefSurvivesRejectedContinue(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	ref := cachedChildRef(t, hh)

	driveContinue(t, hh)
	const msg = "continue rejected without moving the process"
	rejectResume(t, hh, protocol.CmdContinue, "continue", msg)
	requireResyncStopped(t, hh, msg)

	requireChildRefExpands(t, hh, ref, "X")
}

func TestChildVariableRefSurvivesRejectedStep(t *testing.T) {
	tests := []struct {
		request string
		kind    protocol.CommandKind
	}{
		{request: "next", kind: protocol.CmdStepOver},
		{request: "stepIn", kind: protocol.CmdStepInto},
		{request: "stepOut", kind: protocol.CmdStepOut},
	}

	for _, tt := range tests {
		t.Run(tt.request, func(t *testing.T) {
			hh := newHarness(t)
			hh.doHandshake(t)
			ref := cachedChildRef(t, hh)

			sendStepRequest(t, hh, tt.request)
			hh.cmds.waitForCommand(t, tt.kind)
			const msg = "step rejected without moving the process"
			rejectResume(t, hh, tt.kind, tt.request, msg)
			requireResyncStopped(t, hh, msg)

			requireChildRefExpands(t, hh, ref, "X")
		})
	}
}

func TestChildVariableRefResetsAtNextStop(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	staleRef := cachedChildRef(t, hh)

	hh.inject(protocol.EventContinued, protocol.ContinuedPayload{})
	_ = recvType[*godap.ContinuedEvent](hh)
	suspendAtBreakpoint(t, hh)

	requireStaleChildRefEmpty(t, hh, staleRef)
	freshRef := cacheChildRef(t, hh, "Y")
	requireChildRefExpands(t, hh, freshRef, "Y")
}

func TestChildVariableRefIsStaleDuringOutOfBandStep(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	staleRef := cachedChildRef(t, hh)

	hh.inject(protocol.EventSessionState, protocol.SessionStatePayload{
		SessionID: "sess-test", State: protocol.StateRunning, Clients: 2,
	})
	requireStaleChildRefEmpty(t, hh, staleRef)

	hh.inject(protocol.EventStepped, protocol.SteppedPayload{Goroutine: protocol.Goroutine{ID: 7}})
	_ = recvType[*godap.StoppedEvent](hh)
	requireStaleChildRefEmpty(t, hh, staleRef)

	freshRef := cacheChildRef(t, hh, "Y")
	requireChildRefExpands(t, hh, freshRef, "Y")
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

func TestReadMessageDrainsQueuedCommandAcrossClose(t *testing.T) {
	kill, err := marshalCommand(protocol.CmdKill, nil)
	if err != nil {
		t.Fatal(err)
	}

	type readResult struct {
		messageType int
		data        []byte
		err         error
	}

	// The unfixed method lost roughly one command per 10,000 iterations on this
	// schedule; enough trials make a silent pass vanishingly unlikely.
	const trials = 500_000
	for i := 0; i < trials; i++ {
		h := &Handler{
			conn:   nopConn{},
			cmdOut: make(chan []byte, cmdBufferSize),
			done:   make(chan struct{}),
		}
		result := make(chan readResult, 1)
		go func() {
			messageType, data, err := h.ReadMessage()
			result <- readResult{messageType: messageType, data: data, err: err}
		}()

		runtime.Gosched()
		h.enqueue(kill)
		_ = h.Close()

		got := <-result
		if got.err != nil {
			t.Fatalf("trial %d: ReadMessage returned before delivering the queued command: %v", i, got.err)
		}
		if got.messageType != hub.TextMessage || !bytes.Equal(got.data, kill) {
			t.Fatalf("trial %d: ReadMessage returned (%d, %q), want (%d, %q)", i, got.messageType, got.data, hub.TextMessage, kill)
		}
		if _, _, err := h.ReadMessage(); !errors.Is(err, io.EOF) {
			t.Fatalf("trial %d: ReadMessage after draining command returned %v, want EOF", i, err)
		}
	}
}

func TestDisconnectQueuesKillBeforeFailedResponse(t *testing.T) {
	filler, err := marshalCommand(protocol.CmdContinue, nil)
	if err != nil {
		t.Fatal(err)
	}

	conn := &failWriteConn{closed: make(chan struct{})}
	h := NewHandler(conn, nil, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	h.session = &fakeSession{id: "sess-test"}
	conn.beforeWrite = func() {
		// Filling the queue during the response write makes the ordering
		// deterministic: a later Kill cannot race done after send closes it.
		for len(h.cmdOut) < cap(h.cmdOut) {
			h.cmdOut <- filler
		}
	}

	h.onDisconnect(&godap.DisconnectRequest{})

	select {
	case <-conn.closed:
	default:
		t.Fatal("DisconnectResponse write failure did not close the connection")
	}

	_, data, err := h.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage after failed DisconnectResponse: %v", err)
	}
	rec := &cmdRecorder{}
	rec.add(data)
	if kinds := rec.kinds(); len(kinds) != 1 || kinds[0] != protocol.CmdKill {
		t.Fatalf("first command after failed DisconnectResponse = %v, want [%s]", kinds, protocol.CmdKill)
	}
}

func TestUnknownStepOmitsThreadID(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventStepped, protocol.SteppedPayload{
		Goroutine: protocol.Goroutine{Status: "unknown", Current: true},
	})

	stopped := recvType[*godap.StoppedEvent](hh)
	if stopped.Body.Reason != "step" {
		t.Errorf("reason = %q, want step", stopped.Body.Reason)
	}
	if stopped.Body.ThreadId != 0 {
		t.Errorf("threadId = %d, want omitted for unknown goroutine", stopped.Body.ThreadId)
	}

	hh.sendReq("threads", &godap.ThreadsRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdGoroutines)
	hh.inject(protocol.EventGoroutines, protocol.GoroutinesPayload{
		Goroutines: []protocol.Goroutine{
			{ID: 1, Status: "waiting"},
			{ID: 7, Status: "running", Current: true},
			{ID: 9, Status: "waiting"},
		},
	})
	threads := recvType[*godap.ThreadsResponse](hh)
	if len(threads.Body.Threads) != 1 || threads.Body.Threads[0].Id != 7 {
		t.Fatalf("threads after unknown stop = %+v, want only resolved current g7", threads.Body.Threads)
	}

	framesBefore := hh.cmds.count(protocol.CmdFrames)
	hh.sendReq("stackTrace", &godap.StackTraceRequest{
		Arguments: godap.StackTraceArguments{ThreadId: 1},
	})
	nonCurrent := recvType[*godap.StackTraceResponse](hh)
	if len(nonCurrent.Body.StackFrames) != 0 {
		t.Fatalf("non-current stack = %+v, want empty", nonCurrent.Body.StackFrames)
	}
	if got := hh.cmds.count(protocol.CmdFrames); got != framesBefore {
		t.Fatalf("non-current stack enqueued %d CmdFrames, want %d", got, framesBefore)
	}

	hh.sendReq("stackTrace", &godap.StackTraceRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdFrames)
	hh.inject(protocol.EventFrames, protocol.FramesPayload{Frames: []protocol.Frame{{
		Index:    0,
		Location: protocol.Location{Function: "main.worker", File: "/x/main.go", Line: 20},
	}}})
	current := recvType[*godap.StackTraceResponse](hh)
	if len(current.Body.StackFrames) != 1 || current.Body.StackFrames[0].Name != "main.worker" {
		t.Fatalf("thread-id-free current stack = %+v", current.Body.StackFrames)
	}
}

func TestUnknownStepUsesOneSyntheticStackTarget(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventStepped, protocol.SteppedPayload{
		Goroutine: protocol.Goroutine{Status: "unknown", Current: true},
	})
	_ = recvType[*godap.StoppedEvent](hh)

	hh.sendReq("threads", &godap.ThreadsRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdGoroutines)
	hh.inject(protocol.EventGoroutines, protocol.GoroutinesPayload{
		Goroutines: []protocol.Goroutine{
			{ID: 1, Status: "waiting"},
			{ID: 9, Status: "waiting"},
		},
	})
	threads := recvType[*godap.ThreadsResponse](hh)
	if len(threads.Body.Threads) != 1 ||
		threads.Body.Threads[0].Id != 1 ||
		threads.Body.Threads[0].Name != "stopped goroutine (unknown)" {
		t.Fatalf("threads after unresolved stop = %+v", threads.Body.Threads)
	}

	hh.sendReq("stackTrace", &godap.StackTraceRequest{
		Arguments: godap.StackTraceArguments{ThreadId: 1},
	})
	hh.cmds.waitForCommand(t, protocol.CmdFrames)
	hh.inject(protocol.EventFrames, protocol.FramesPayload{Frames: []protocol.Frame{{
		Index:    0,
		Location: protocol.Location{Function: "main.worker", File: "/x/main.go", Line: 20},
	}}})
	stack := recvType[*godap.StackTraceResponse](hh)
	if len(stack.Body.StackFrames) != 1 || stack.Body.StackFrames[0].Name != "main.worker" {
		t.Fatalf("synthetic stopped stack = %+v", stack.Body.StackFrames)
	}
}

func TestUnknownStepServesStackWithoutThreadQuery(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventStepped, protocol.SteppedPayload{
		Goroutine: protocol.Goroutine{Status: "unknown", Current: true},
	})
	_ = recvType[*godap.StoppedEvent](hh)

	hh.sendReq("stackTrace", &godap.StackTraceRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdFrames)
	hh.inject(protocol.EventFrames, protocol.FramesPayload{Frames: []protocol.Frame{{
		Index:    0,
		Location: protocol.Location{Function: "main.worker", File: "/x/main.go", Line: 20},
	}}})
	stack := recvType[*godap.StackTraceResponse](hh)
	if len(stack.Body.StackFrames) != 1 || stack.Body.StackFrames[0].Name != "main.worker" {
		t.Fatalf("thread-id-free stack = %+v", stack.Body.StackFrames)
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

// --- rejected run-control helpers ----------------------------------------------
//
// Every rejected-resume scenario shares the same shape: park the adapter in a
// known run state, issue a run-control request it answers optimistically, then
// inject the hub's EventError and assert how the adapter resynchronizes. These
// helpers own the shared mechanics; each test still spells out its own state,
// command kind and expected recovery so a failure names the exact scenario.

// suspendAtBreakpoint parks the handler at a breakpoint on goroutine 7 and
// consumes the resulting stopped event.
func suspendAtBreakpoint(t *testing.T, hh *harness) {
	t.Helper()
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 7}})
	_ = recvType[*godap.StoppedEvent](hh)
}

// newSuspendedHarness completes the handshake and leaves the adapter suspended
// at a breakpoint — the starting state for every rejected-resume scenario.
func newSuspendedHarness(t *testing.T) *harness {
	t.Helper()
	hh := newHarness(t)
	hh.doHandshake(t)
	suspendAtBreakpoint(t, hh)
	return hh
}

// driveContinue issues a DAP continue on goroutine 7, consumes its optimistic
// success response, and waits for the bingo command to reach the hub.
func driveContinue(t *testing.T, hh *harness) {
	t.Helper()
	before := hh.cmds.count(protocol.CmdContinue)
	hh.sendReq("continue", &godap.ContinueRequest{Arguments: godap.ContinueArguments{ThreadId: 7}})
	_ = recvType[*godap.ContinueResponse](hh)
	hh.cmds.waitForCommands(t, protocol.CmdContinue, before+1)
}

// sendStepRequest issues one DAP step request on goroutine 7 and consumes its
// bare acknowledgement. The three kinds are spelled out rather than derived:
// they share a wire shape but decode to distinct response types, and an explicit
// case keeps a failure pointing at the exact step that broke.
func sendStepRequest(t *testing.T, hh *harness, request string) {
	t.Helper()
	switch request {
	case "next":
		hh.sendReq(request, &godap.NextRequest{Arguments: godap.NextArguments{ThreadId: 7}})
		_ = recvType[*godap.NextResponse](hh)
	case "stepIn":
		hh.sendReq(request, &godap.StepInRequest{Arguments: godap.StepInArguments{ThreadId: 7}})
		_ = recvType[*godap.StepInResponse](hh)
	case "stepOut":
		hh.sendReq(request, &godap.StepOutRequest{Arguments: godap.StepOutArguments{ThreadId: 7}})
		_ = recvType[*godap.StepOutResponse](hh)
	default:
		t.Fatalf("unknown step request %q", request)
	}
}

// rejectResume injects the hub's rejection of an in-flight resume and asserts
// the console line, which must name the DAP request that failed rather than the
// generic "error:" the pre-fix default branch emitted.
func rejectResume(t *testing.T, hh *harness, kind protocol.CommandKind, request, msg string) {
	t.Helper()
	hh.inject(protocol.EventError, protocol.ErrorPayload{Command: kind, Message: msg})
	out := recvType[*godap.OutputEvent](hh)
	if want := request + " failed: " + msg + "\n"; out.Body.Output != want {
		t.Errorf("console output = %q, want %q", out.Body.Output, want)
	}
}

// requireResyncStopped reads the stopped event a rejected Continue/Step must
// send to walk the client back from the success response it already received.
func requireResyncStopped(t *testing.T, hh *harness, wantMessage string) {
	t.Helper()
	stopped := recvType[*godap.StoppedEvent](hh)
	if stopped.Body.Reason != "exception" {
		t.Errorf("stopped reason = %q, want exception", stopped.Body.Reason)
	}
	if stopped.Body.Text != wantMessage {
		t.Errorf("stopped text = %q, want %q", stopped.Body.Text, wantMessage)
	}
	if !stopped.Body.AllThreadsStopped {
		t.Error("stopped allThreadsStopped = false, want true")
	}
	if stopped.Body.ThreadId != 7 {
		t.Errorf("stopped threadId = %d, want the current thread 7", stopped.Body.ThreadId)
	}
}

// rejectRestart sends a DAP restart, waits for the destructive command, injects
// the hub's rejection, and asserts the delayed error response correlates back to
// that request.
func rejectRestart(t *testing.T, hh *harness, msg string) {
	t.Helper()
	before := hh.cmds.count(protocol.CmdRestart)
	seq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommands(t, protocol.CmdRestart, before+1)

	hh.inject(protocol.EventError, protocol.ErrorPayload{Command: protocol.CmdRestart, Message: msg})
	failed := recvType[*godap.ErrorResponse](hh)
	if failed.RequestSeq != seq || failed.Success {
		t.Fatalf("restart response = %+v, want an error for request %d", failed.Response, seq)
	}
}

// requireSuspendedFlag asserts the handler's internal suspended view, which is
// what gates whether inspection requests are forwarded to the hub.
func requireSuspendedFlag(t *testing.T, hh *harness, want bool) {
	t.Helper()
	hh.handler.mu.Lock()
	got := hh.handler.suspended
	hh.handler.mu.Unlock()
	if got != want {
		t.Fatalf("handler suspended = %v, want %v", got, want)
	}
}

// requireInspectionReachesHub proves the handler considers itself suspended
// again: a stackTrace must enqueue a Frames command instead of being answered
// synthetically, and the correlated response must carry the injected frames. Any
// stopped event arriving while it waits is a duplicate and fails the test.
func requireInspectionReachesHub(t *testing.T, hh *harness) {
	t.Helper()
	before := hh.cmds.count(protocol.CmdFrames)
	hh.sendReq("stackTrace", &godap.StackTraceRequest{Arguments: godap.StackTraceArguments{ThreadId: 7}})
	hh.cmds.waitForCommands(t, protocol.CmdFrames, before+1)
	hh.inject(protocol.EventFrames, protocol.FramesPayload{Frames: []protocol.Frame{
		{Index: 0, Location: protocol.Location{Function: "main.f", File: "/x/main.go", Line: 12}},
	}})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		switch m := hh.recv().(type) {
		case *godap.StoppedEvent:
			t.Fatalf("unexpected stopped event while inspecting: %+v", m.Body)
		case *godap.StackTraceResponse:
			if len(m.Body.StackFrames) != 1 {
				t.Fatalf("stack frames = %+v, want the frame answered by the hub", m.Body.StackFrames)
			}
			return
		}
	}
	t.Fatal("timed out waiting for the hub-answered stackTrace response")
}

// requireInspectionAnsweredSynthetically is the not-suspended counterpart of
// requireInspectionReachesHub: a stackTrace must be answered locally with an
// empty stack and must NOT enqueue a Frames command at the hub.
func requireInspectionAnsweredSynthetically(t *testing.T, hh *harness) {
	t.Helper()
	before := hh.cmds.count(protocol.CmdFrames)
	hh.sendReq("stackTrace", &godap.StackTraceRequest{Arguments: godap.StackTraceArguments{ThreadId: 7}})
	st := recvType[*godap.StackTraceResponse](hh)
	if len(st.Body.StackFrames) != 0 {
		t.Fatalf("stack frames = %+v, want an empty synthetic answer", st.Body.StackFrames)
	}
	hh.cmds.requireNoAdditionalCommands(t, protocol.CmdFrames, before)
}

// requireNoStoppedEvent fails if any stopped event arrives within the window.
// It leaves the read deadline expired, so callers must not read afterwards.
func requireNoStoppedEvent(t *testing.T, hh *harness, within time.Duration) {
	t.Helper()
	_ = hh.client.SetReadDeadline(time.Now().Add(within))
	for {
		data, err := godap.ReadBaseMessage(hh.reader)
		if err != nil {
			return
		}
		m, decodeErr := hh.codec.DecodeMessage(data)
		if decodeErr != nil {
			continue
		}
		if stopped, ok := m.(*godap.StoppedEvent); ok {
			t.Fatalf("unexpected stopped event: %+v", stopped.Body)
		}
	}
}

// quietWindow bounds the "nothing further arrives" assertions. Translation is
// synchronous in inject, so an erroneous event is already on the wire long
// before this elapses.
const quietWindow = 150 * time.Millisecond

// restartNoLaunchMsg is the hub's rejection for a session it cannot relaunch
// (attach-created, or no prior Launch). It leaves the process untouched.
const restartNoLaunchMsg = "no launched process to restart — use Launch first"

// TestRejectedContinueRestoresSuspendedState covers the rejected Continue: the
// adapter already answered the continue request successfully, so it must
// resynchronize with the still-suspended engine and report a stop.
func TestRejectedContinueRestoresSuspendedState(t *testing.T) {
	hh := newSuspendedHarness(t)
	driveContinue(t, hh)

	const msg = "Continue: reinstall breakpoint: write memory: permission denied"
	rejectResume(t, hh, protocol.CmdContinue, "continue", msg)
	requireResyncStopped(t, hh, msg)
	requireInspectionReachesHub(t, hh)
}

// TestRejectedStepRestoresSuspendedState covers every step kind: all three DAP
// step requests share onStep's optimistic resume, so all three must be handled
// explicitly in onError rather than falling into the generic console default.
// Each kind is its own named subtest, so a regression names the failing step.
func TestRejectedStepRestoresSuspendedState(t *testing.T) {
	steps := []struct {
		request string
		kind    protocol.CommandKind
	}{
		{"next", protocol.CmdStepOver},
		{"stepIn", protocol.CmdStepInto},
		{"stepOut", protocol.CmdStepOut},
	}

	for _, step := range steps {
		t.Run(step.request, func(t *testing.T) {
			hh := newSuspendedHarness(t)
			sendStepRequest(t, hh, step.request)
			hh.cmds.waitForCommand(t, step.kind)

			msg := string(step.kind) + " rejected by the engine"
			rejectResume(t, hh, step.kind, step.request, msg)
			requireResyncStopped(t, hh, msg)
			requireInspectionReachesHub(t, hh)
		})
	}
}

// TestRejectedOutermostStepOutRestoresInspection pins the routine deterministic
// trigger: StepOut at the outermost frame fails in the engine before resuming
// and emits no stop of its own.
func TestRejectedOutermostStepOutRestoresInspection(t *testing.T) {
	hh := newSuspendedHarness(t)
	sendStepRequest(t, hh, "stepOut")
	hh.cmds.waitForCommand(t, protocol.CmdStepOut)

	const msg = "StepOut: null frame pointer — at outermost frame?"
	rejectResume(t, hh, protocol.CmdStepOut, "stepOut", msg)
	requireResyncStopped(t, hh, msg)
	requireInspectionReachesHub(t, hh)
}

// TestRejectedResumeSettlesContinueSuppression proves the pendingContinues debt
// of a rejected Continue is settled exactly once: the EventContinued it would
// have produced never arrives, so the NEXT out-of-band continue must surface.
func TestRejectedResumeSettlesContinueSuppression(t *testing.T) {
	hh := newSuspendedHarness(t)
	driveContinue(t, hh)

	const msg = "Continue: not suspended"
	rejectResume(t, hh, protocol.CmdContinue, "continue", msg)
	requireResyncStopped(t, hh, msg)

	// Another client drives the resume: with the debt settled this must be
	// surfaced rather than suppressed as our own.
	hh.inject(protocol.EventContinued, protocol.ContinuedPayload{})
	cont := recvType[*godap.ContinuedEvent](hh)
	if cont.Body.ThreadId != 7 || !cont.Body.AllThreadsContinued {
		t.Fatalf("continued body = %+v, want the current thread and allThreadsContinued", cont.Body)
	}
}

// TestRejectedResumeWhileSuspendedDoesNotDuplicateStopped covers a rejection
// that loses the race with a real stop: the client's existing stopped already
// describes this suspension, so a second one must not be fabricated.
func TestRejectedResumeWhileSuspendedDoesNotDuplicateStopped(t *testing.T) {
	hh := newSuspendedHarness(t)
	driveContinue(t, hh)

	// A genuine stop lands first, putting the client back in a stopped state.
	suspendAtBreakpoint(t, hh)

	rejectResume(t, hh, protocol.CmdContinue, "continue", "stale rejection")
	requireNoStoppedEvent(t, hh, quietWindow)
	requireSuspendedFlag(t, hh, true)
}

// TestRejectedRestartRestoresSuspendedWithoutStopped pins the restart variant
// for a restart issued WHILE SUSPENDED: the delayed error response already tells
// the client the restart failed while it was stopped, so no extra stopped event
// may be sent — but the suspended view onRestart cleared optimistically must
// still be restored.
func TestRejectedRestartRestoresSuspendedWithoutStopped(t *testing.T) {
	hh := newSuspendedHarness(t)
	rejectRestart(t, hh, restartNoLaunchMsg)

	requireSuspendedFlag(t, hh, true)
	requireInspectionReachesHub(t, hh)
	requireNoStoppedEvent(t, hh, quietWindow)
}

// TestRejectedRestartWhileRunningStaysRunning covers the inverse desync: DAP
// permits restart while the tracee is running, and the hub rejects a restart on
// an attach-created session without touching that still-running process. The
// adapter must restore the RUNNING view it cleared, not invent a suspension —
// otherwise it would forward inspection requests for a process that never
// stopped.
func TestRejectedRestartWhileRunningStaysRunning(t *testing.T) {
	hh := newHarness(t)
	// doHandshake leaves the session running with no stop delivered.
	hh.doHandshake(t)
	requireSuspendedFlag(t, hh, false)

	rejectRestart(t, hh, restartNoLaunchMsg)

	requireSuspendedFlag(t, hh, false)
	requireInspectionAnsweredSynthetically(t, hh)
	requireNoStoppedEvent(t, hh, quietWindow)
}

// TestRejectedRestartAfterFailedRelaunchDropsSuspended covers the hub's
// relaunch-failure path: it kills the old process, reports the error, then
// transitions the managed session to idle. There is no process left, so the
// captured pre-request suspension must not be reasserted — and a retry restart
// must still correlate to its own request.
func TestRejectedRestartAfterFailedRelaunchDropsSuspended(t *testing.T) {
	hh := newSuspendedHarness(t)
	rejectRestart(t, hh, "restart: relaunch failed: fork/exec /bin/x: no such file or directory")

	// The hub reports the teardown right after the error.
	hh.inject(protocol.EventSessionState, protocol.SessionStatePayload{
		SessionID: "sess-test", State: protocol.StateIdle, Clients: 1,
	})
	requireSuspendedFlag(t, hh, false)
	requireInspectionAnsweredSynthetically(t, hh)

	// Retry correlation survives: the gate was released, so a new restart is
	// accepted and answered against its own request sequence.
	retrySeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommands(t, protocol.CmdRestart, 2)
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{})
	restarted := recvType[*godap.RestartResponse](hh)
	if restarted.RequestSeq != retrySeq || !restarted.Success {
		t.Fatalf("retried restart response = %+v, want success for request %d", restarted.Response, retrySeq)
	}
	requireNoStoppedEvent(t, hh, quietWindow)
}

// TestRejectedResumeAfterSessionEndedEmitsNoStopped pins the same lifecycle
// guard for Continue/Step: once the session is idle or exited the hub answers
// every command with "no active debugger", and fabricating a stop there would
// leave the client stopped on a process that no longer exists.
func TestRejectedResumeAfterSessionEndedEmitsNoStopped(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)

	hh.inject(protocol.EventProcessExited, protocol.ProcessExitedPayload{ExitCode: 0})
	_ = recvType[*godap.ExitedEvent](hh)
	_ = recvType[*godap.TerminatedEvent](hh)

	driveContinue(t, hh)
	rejectResume(t, hh, protocol.CmdContinue, "continue", "no active debugger — send Launch or Attach first")

	requireSuspendedFlag(t, hh, false)
	requireNoStoppedEvent(t, hh, quietWindow)
}

// TestRejectedResumeDuringJoinDefersToWelcome covers a joiner that receives
// another client's resume rejection in the window between AddClient registering
// it and the hub's welcome arriving: the welcome owns the joiner's initial state,
// so the rejection must not fabricate a `stopped` carrying a foreign error.
func TestRejectedResumeDuringJoinDefersToWelcome(t *testing.T) {
	// An empty welcome keeps the fake session from sending one, holding the
	// handler in the awaitingWelcome window for the whole test.
	hh := newHarnessWelcome(t, "")

	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)
	hh.sendReq("attach", &godap.AttachRequest{Arguments: json.RawMessage(`{"session":"sess-test"}`)})
	_ = recvType[*godap.InitializedEvent](hh)

	rejectResume(t, hh, protocol.CmdContinue, "continue", "another client's continue failed")
	requireNoStoppedEvent(t, hh, quietWindow)

	// The welcome still owns the initial state and reports the suspension.
	hh.inject(protocol.EventSessionState, protocol.SessionStatePayload{
		SessionID: "sess-test", State: protocol.StateSuspended, Clients: 2,
	})
	_ = hh.client.SetReadDeadline(time.Now().Add(2 * time.Second))
	stopped := recvType[*godap.StoppedEvent](hh)
	if stopped.Body.Reason != "pause" {
		t.Fatalf("welcome stopped reason = %q, want pause", stopped.Body.Reason)
	}
}

// TestRejectedAutoContinueRestoresSuspendedState covers the auto-continue the
// handshake itself issues at configurationDone: it takes the same optimistic
// resume path, so its rejection must resynchronize identically.
func TestRejectedAutoContinueRestoresSuspendedState(t *testing.T) {
	hh := newHarness(t)

	hh.sendReq("initialize", initArgs())
	_ = recvType[*godap.InitializeResponse](hh)

	lr := &godap.LaunchRequest{Arguments: json.RawMessage(`{"program":"/bin/x","stopOnEntry":false}`)}
	hh.sendReq("launch", lr)
	hh.cmds.waitForCommand(t, protocol.CmdLaunch)
	hh.inject(protocol.EventStepped, protocol.SteppedPayload{Goroutine: protocol.Goroutine{ID: 7}})
	_ = recvType[*godap.InitializedEvent](hh)

	hh.sendReq("configurationDone", &godap.ConfigurationDoneRequest{})
	_ = recvType[*godap.ConfigurationDoneResponse](hh)
	_ = recvType[*godap.LaunchResponse](hh)
	hh.cmds.waitForCommand(t, protocol.CmdContinue)

	const msg = "Continue: resume from breakpoint: single-step: no such process"
	rejectResume(t, hh, protocol.CmdContinue, "continue", msg)
	requireResyncStopped(t, hh, msg)
	requireInspectionReachesHub(t, hh)
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
		Goroutines: []protocol.Goroutine{{ID: 7, Status: "running", Current: true}},
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

// TestAsyncHaltSurfacesAsStoppedPause pins the adapter side of the engine's
// asynchronous-halt reporting (issue #183). handleStop failures emit a detailed
// EventError followed by a suspending EventPaused; the error alone carries no
// correlation for CmdNone, so it is the Paused that must restore the adapter's
// suspended view and tell the IDE the tracee is halted.
func TestAsyncHaltSurfacesAsStoppedPause(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	hh.sendReq("continue", &godap.ContinueRequest{})
	_ = recvType[*godap.ContinueResponse](hh)
	hh.cmds.waitForCommand(t, protocol.CmdContinue)
	hh.inject(protocol.EventContinued, protocol.ContinuedPayload{})

	// The step-over fails asynchronously: no DAP request is outstanding, so the
	// CmdNone error produces no response of its own.
	hh.inject(protocol.EventError, protocol.ErrorPayload{
		Command: protocol.CmdNone,
		Message: "reinstall breakpoint 0x1000: injected",
	})
	hh.inject(protocol.EventPaused, protocol.PausedPayload{Goroutine: protocol.Goroutine{ID: 1}})

	stopped := recvType[*godap.StoppedEvent](hh)
	if stopped.Body.Reason != "pause" {
		t.Errorf("reason = %q, want pause", stopped.Body.Reason)
	}

	// Being suspended again is what makes the session usable: a data request
	// must now reach the hub instead of being answered with an empty stub.
	hh.sendReq("stackTrace", &godap.StackTraceRequest{Arguments: godap.StackTraceArguments{ThreadId: 1}})
	hh.cmds.waitForCommand(t, protocol.CmdFrames)
}

// TestAsyncHaltDuringRestartIsNotTreatedAsEntry is why the halt is reported as
// EventPaused rather than EventStepped: onStop's restart branch treats the
// first Stepped as the relaunched process's entry stop and, without
// stopOnEntry, auto-continues past it. A halt must never do that — the tracee
// is stopped precisely because it is not safe to resume.
func TestAsyncHaltDuringRestartIsNotTreatedAsEntry(t *testing.T) {
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)

	hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{})
	_ = recvType[*godap.RestartResponse](hh)

	hh.inject(protocol.EventError, protocol.ErrorPayload{
		Command: protocol.CmdNone,
		Message: "get stop PC for tid 1: injected",
	})
	continuesBefore := countKind(hh.cmds.kinds(), protocol.CmdContinue)
	hh.inject(protocol.EventPaused, protocol.PausedPayload{Goroutine: protocol.Goroutine{ID: 1}})

	stopped := recvType[*godap.StoppedEvent](hh)
	if stopped.Body.Reason != "pause" {
		t.Errorf("reason = %q, want pause", stopped.Body.Reason)
	}
	if got := countKind(hh.cmds.kinds(), protocol.CmdContinue); got != continuesBefore {
		t.Fatalf("halt during restart auto-continued the tracee: continues %d -> %d",
			continuesBefore, got)
	}
}

func countKind(kinds []protocol.CommandKind, want protocol.CommandKind) int {
	n := 0
	for _, k := range kinds {
		if k == want {
			n++
		}
	}
	return n
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
