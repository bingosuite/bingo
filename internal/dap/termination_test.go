package dap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

type terminationConn struct {
	nopConn
	mu   sync.Mutex
	wire bytes.Buffer
}

func (c *terminationConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wire.Write(p)
}

func (c *terminationConn) messages(t *testing.T) []godap.Message {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	reader := bufio.NewReader(bytes.NewReader(c.wire.Bytes()))
	var messages []godap.Message
	for {
		msg, err := godap.ReadProtocolMessage(reader)
		if errors.Is(err, io.EOF) {
			return messages
		}
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, msg)
	}
}

func terminationHandler(t *testing.T, state protocol.SessionState) (*Handler, *terminationConn) {
	t.Helper()
	conn := &terminationConn{}
	h := NewHandler(conn, nil, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
	h.session = &fakeSession{id: "termination"}
	h.sessionState = state
	h.suspended = state == protocol.StateSuspended
	t.Cleanup(func() { _ = h.Close() })
	return h, conn
}

func terminalEvents(t *testing.T, conn *terminationConn) []string {
	t.Helper()
	var events []string
	for _, msg := range conn.messages(t) {
		if e, ok := msg.(godap.EventMessage); ok {
			switch e.GetEvent().Event {
			case "exited", "terminated", "stopped", "continued":
				events = append(events, e.GetEvent().Event)
			}
		}
	}
	return events
}

func assertTerminalEvents(t *testing.T, conn *terminationConn, want string) {
	t.Helper()
	if got := strings.Join(terminalEvents(t, conn), ","); got != want {
		t.Fatalf("lifecycle events = %q, want %q", got, want)
	}
}

func stateEvent(state protocol.SessionState) protocol.Event {
	return protocol.MustEvent(protocol.EventSessionState, 1, protocol.SessionStatePayload{
		SessionID: "termination", State: state, Clients: 2,
	})
}

func terminateRequest(seq int) *godap.TerminateRequest {
	return &godap.TerminateRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: seq}}}
}

type terminateStateCase struct {
	name       string
	state      protocol.SessionState
	noSession  bool
	launching  bool
	restarting bool
	wantKill   bool
	wantEnd    bool
}

func TestTerminateStateMatrix(t *testing.T) {
	for _, tc := range []terminateStateCase{
		{name: "no session", noSession: true, wantEnd: true},
		{name: "idle", state: protocol.StateIdle, wantEnd: true},
		{name: "exited", state: protocol.StateExited, wantEnd: true},
		{name: "running", state: protocol.StateRunning, wantKill: true},
		{name: "suspended", state: protocol.StateSuspended, wantKill: true},
		{name: "welcome not delivered", wantKill: true},
		{name: "initial idle during launch", state: protocol.StateIdle, launching: true, wantKill: true},
		{name: "idle during restart", state: protocol.StateIdle, restarting: true, wantKill: true},
		{name: "exited during restart", state: protocol.StateExited, restarting: true, wantKill: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkTerminateState(t, tc)
		})
	}
}

func checkTerminateState(t *testing.T, tc terminateStateCase) {
	t.Helper()
	h, conn := terminationHandler(t, tc.state)
	if tc.noSession {
		h.session = nil
	}
	h.launching, h.restarting = tc.launching, tc.restarting
	h.onTerminate(terminateRequest(1))
	h.onTerminate(terminateRequest(2))
	assertTerminateCommands(t, h, tc.wantKill)
	wantEvents := ""
	if tc.wantEnd {
		wantEvents = "terminated"
	}
	assertTerminalEvents(t, conn, wantEvents)
	assertTerminateAcknowledgements(t, conn)
}

func assertTerminateCommands(t *testing.T, h *Handler, wantKill bool) {
	t.Helper()
	wantCommands := 0
	if wantKill {
		wantCommands = 1
	}
	if len(h.cmdOut) != wantCommands {
		t.Fatalf("queued commands = %d, want %d", len(h.cmdOut), wantCommands)
	}
	if wantKill {
		command, err := protocol.UnmarshalCommand(<-h.cmdOut)
		if err != nil || command.Kind != protocol.CmdKill {
			t.Fatalf("command = %+v, %v", command, err)
		}
	}
}

func assertTerminateAcknowledgements(t *testing.T, conn *terminationConn) {
	t.Helper()
	responses := 0
	for _, msg := range conn.messages(t) {
		if r, ok := msg.(*godap.TerminateResponse); ok {
			responses++
			if !r.Success || r.RequestSeq != responses {
				t.Fatalf("terminate acknowledgement = %+v", r)
			}
		}
	}
	if responses != 2 {
		t.Fatalf("acknowledgements = %d, want 2", responses)
	}
}

func TestTerminalLifecycleSources(t *testing.T) {
	for _, state := range []protocol.SessionState{protocol.StateRunning, protocol.StateSuspended, protocol.StateIdle} {
		for _, realExit := range []bool{false, true} {
			name := string(state) + "/channel close"
			if realExit {
				name = string(state) + "/natural exit"
			}
			t.Run(name, func(t *testing.T) {
				checkTerminalLifecycleSource(t, state, realExit)
			})
		}
	}
}

func checkTerminalLifecycleSource(t *testing.T, state protocol.SessionState, realExit bool) {
	t.Helper()
	h, conn := terminationHandler(t, state)
	if realExit {
		h.onProcessExited(protocol.MustEvent(protocol.EventProcessExited, 1,
			protocol.ProcessExitedPayload{ExitCode: 37}))
	}
	h.onSessionState(stateEvent(protocol.StateExited))
	h.onSessionState(stateEvent(protocol.StateIdle))
	h.onSessionState(stateEvent(protocol.StateIdle))
	h.onTerminate(terminateRequest(1))
	h.onProcessExited(protocol.MustEvent(protocol.EventProcessExited, 2,
		protocol.ProcessExitedPayload{ExitCode: 99}))
	h.onStop(protocol.MustEvent(protocol.EventStepped, 3, protocol.SteppedPayload{}))
	h.onContinued()
	want := "terminated"
	if realExit {
		want = "exited,terminated"
		first := conn.messages(t)[0].(*godap.ExitedEvent)
		if first.Body.ExitCode != 37 {
			t.Fatalf("exit code = %d, want 37", first.Body.ExitCode)
		}
	}
	assertTerminalEvents(t, conn, want)
	if len(h.cmdOut) != 0 {
		t.Fatal("already-completed termination sent a Kill")
	}
}

func TestInitialIdleDoesNotTerminate(t *testing.T) {
	for _, joining := range []bool{false, true} {
		h, conn := terminationHandler(t, "")
		h.awaitingWelcome = joining
		h.onSessionState(stateEvent(protocol.StateIdle))
		assertTerminalEvents(t, conn, "")
		h.onTerminate(terminateRequest(1))
		assertTerminalEvents(t, conn, "terminated")
	}
}

func TestTerminateWaitsThroughDelayedWelcomeAndLaunch(t *testing.T) {
	h, conn := terminationHandler(t, "")
	h.launching, h.startReqSeq, h.startCmd = true, 1, "launch"
	h.onTerminate(terminateRequest(2))
	h.onSessionState(stateEvent(protocol.StateIdle))
	assertTerminalEvents(t, conn, "")
	h.onSessionState(stateEvent(protocol.StateRunning))
	h.onStop(protocol.MustEvent(protocol.EventStepped, 1, protocol.SteppedPayload{}))
	h.onConfigurationDone(&godap.ConfigurationDoneRequest{})
	assertTerminalEvents(t, conn, "")
	if len(h.cmdOut) != 1 {
		t.Fatal("configurationDone resumed during termination")
	}
	h.onSessionState(stateEvent(protocol.StateExited))
	h.onConfigurationDone(&godap.ConfigurationDoneRequest{})
	assertTerminalEvents(t, conn, "terminated")
	failures := 0
	for _, m := range conn.messages(t) {
		if r, ok := m.(*godap.ErrorResponse); ok && r.RequestSeq == 1 && r.Command == "launch" {
			failures++
		}
		if _, ok := m.(*godap.LaunchResponse); ok {
			t.Fatal("terminated launch was acknowledged as successful")
		}
	}
	if failures != 1 {
		t.Fatalf("pending launch errors = %d, want 1", failures)
	}
}

func TestTerminateDuringIdleJoinWelcome(t *testing.T) {
	h, conn := terminationHandler(t, "")
	h.awaitingWelcome = true
	h.onTerminate(terminateRequest(1))
	assertTerminalEvents(t, conn, "")
	h.onSessionState(stateEvent(protocol.StateIdle))
	assertTerminalEvents(t, conn, "terminated")
}

func TestFailedTerminateRemainsRetryable(t *testing.T) {
	for _, state := range []protocol.SessionState{protocol.StateRunning, protocol.StateSuspended} {
		for _, cause := range []error{errors.New("backend Kill refused"), debugger.ErrAttachedDetachIncomplete} {
			t.Run(string(state)+"/"+cause.Error(), func(t *testing.T) {
				checkFailedTerminateRetry(t, state, cause)
			})
		}
	}
}

func checkFailedTerminateRetry(t *testing.T, state protocol.SessionState, cause error) {
	t.Helper()
	h, conn := terminationHandler(t, state)
	h.onTerminate(terminateRequest(1))
	h.onError(protocol.MustEvent(protocol.EventError, 1, protocol.ErrorPayload{
		Command: protocol.CmdKill, Message: cause.Error(),
	}))
	assertTerminalEvents(t, conn, "")
	if h.terminating || h.terminated || h.suspended != (state == protocol.StateSuspended) {
		t.Fatal("failed cleanup changed the live process state or blocked retry")
	}
	output := conn.messages(t)[1].(*godap.OutputEvent)
	if !strings.Contains(output.Body.Output, cause.Error()) {
		t.Fatalf("cleanup failure was not surfaced: %+v", output)
	}
	h.onTerminate(terminateRequest(2))
	if len(h.cmdOut) != 2 {
		t.Fatal("second terminate did not retry Kill")
	}
	h.onSessionState(stateEvent(protocol.StateExited))
	assertTerminalEvents(t, conn, "terminated")
}

func TestTerminateDisconnectPreservesCleanup(t *testing.T) {
	for _, completed := range []bool{false, true} {
		h, conn := terminationHandler(t, protocol.StateSuspended)
		h.onTerminate(terminateRequest(1))
		wantKills := 2
		if completed {
			h.onSessionState(stateEvent(protocol.StateExited))
			wantKills = 1
		}
		h.onDisconnect(&godap.DisconnectRequest{})
		if len(h.cmdOut) != wantKills {
			t.Fatalf("terminate/disconnect queued %d Kills, want %d", len(h.cmdOut), wantKills)
		}
		want := ""
		if completed {
			want = "terminated"
		}
		assertTerminalEvents(t, conn, want)
		for range wantKills {
			_, data, err := h.ReadMessage()
			if err != nil {
				t.Fatalf("pending Kill lost on disconnect: %v", err)
			}
			cmd, err := protocol.UnmarshalCommand(data)
			if err != nil || cmd.Kind != protocol.CmdKill {
				t.Fatalf("pending command = %+v, %v", cmd, err)
			}
		}
		if _, _, err := h.ReadMessage(); !errors.Is(err, io.EOF) {
			t.Fatalf("disconnect did not close: %v", err)
		}
	}
}

func TestDisconnectDuringRestartAfterNaturalExit(t *testing.T) {
	h, _ := terminationHandler(t, protocol.StateRunning)
	h.onProcessExited(protocol.MustEvent(protocol.EventProcessExited, 1, protocol.ProcessExitedPayload{ExitCode: 7}))
	h.onRestart(&godap.RestartRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: 1}}})
	h.onDisconnect(&godap.DisconnectRequest{})
	for _, want := range []protocol.CommandKind{protocol.CmdRestart, protocol.CmdKill} {
		_, data, err := h.ReadMessage()
		if err != nil {
			t.Fatalf("replacement cleanup lost: %v", err)
		}
		cmd, err := protocol.UnmarshalCommand(data)
		if err != nil || cmd.Kind != want {
			t.Fatalf("command = %+v, %v; want %s", cmd, err, want)
		}
	}
	if _, _, err := h.ReadMessage(); !errors.Is(err, io.EOF) {
		t.Fatalf("disconnect did not close: %v", err)
	}
}

func TestRestartAndTerminationBoundaries(t *testing.T) {
	h, conn := terminationHandler(t, protocol.StateSuspended)
	h.stopOnEntry = true
	h.onRestart(&godap.RestartRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: 1}}})
	h.onSessionState(stateEvent(protocol.StateRunning))
	h.onRestarted(protocol.MustEvent(protocol.EventRestarted, 1, protocol.RestartedPayload{}))
	h.onStop(protocol.MustEvent(protocol.EventStepped, 2, protocol.SteppedPayload{}))
	assertTerminalEvents(t, conn, "stopped")
	h.onTerminate(terminateRequest(2))
	h.onRestart(&godap.RestartRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: 3}}})
	last := conn.messages(t)[len(conn.messages(t))-1].(*godap.ErrorResponse)
	if last.Command != "restart" || last.Message != "termination already in progress" {
		t.Fatalf("restart during teardown = %+v", last)
	}
	if len(h.cmdOut) != 2 {
		t.Fatal("restart during termination enqueued a replacement")
	}
	h.onSessionState(stateEvent(protocol.StateExited))
	assertTerminalEvents(t, conn, "stopped,terminated")
}

func TestRestartAfterNaturalExitDoesNotReconsumeOldTerminal(t *testing.T) {
	h, conn := terminationHandler(t, protocol.StateRunning)
	h.onProcessExited(protocol.MustEvent(protocol.EventProcessExited, 1, protocol.ProcessExitedPayload{ExitCode: 7}))
	h.onRestart(&godap.RestartRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: 1}}})
	h.onSessionState(stateEvent(protocol.StateExited))
	h.onSessionState(stateEvent(protocol.StateIdle))
	h.onSessionState(stateEvent(protocol.StateRunning))
	h.onRestarted(protocol.MustEvent(protocol.EventRestarted, 1, protocol.RestartedPayload{}))
	h.onStop(protocol.MustEvent(protocol.EventStepped, 2, protocol.SteppedPayload{}))
	assertTerminalEvents(t, conn, "exited,terminated")
	if h.terminated || h.restarting {
		t.Fatal("successful restart did not open its new lifecycle")
	}
	h.onSessionState(stateEvent(protocol.StateExited))
	assertTerminalEvents(t, conn, "exited,terminated,terminated")
}

func TestTerminateDuringRestartAfterNaturalExit(t *testing.T) {
	h, conn := terminationHandler(t, protocol.StateRunning)
	h.onProcessExited(protocol.MustEvent(protocol.EventProcessExited, 1, protocol.ProcessExitedPayload{ExitCode: 7}))
	h.onRestart(&godap.RestartRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: 1}}})
	h.onTerminate(terminateRequest(2))
	h.onTerminate(terminateRequest(3))
	if len(h.cmdOut) != 2 {
		t.Fatal("restart and one replacement Kill must both be queued")
	}
	h.onSessionState(stateEvent(protocol.StateExited))
	h.onSessionState(stateEvent(protocol.StateIdle))
	assertTerminalEvents(t, conn, "exited,terminated")
	h.onSessionState(stateEvent(protocol.StateRunning))
	h.onRestarted(protocol.MustEvent(protocol.EventRestarted, 2, protocol.RestartedPayload{}))
	h.onStop(protocol.MustEvent(protocol.EventStepped, 3, protocol.SteppedPayload{}))
	if len(h.cmdOut) != 2 {
		t.Fatal("replacement entry resumed during termination")
	}
	assertTerminalEvents(t, conn, "exited,terminated,stopped")
	h.onSessionState(stateEvent(protocol.StateExited))
	assertTerminalEvents(t, conn, "exited,terminated,stopped,terminated")
}

func TestMalformedProcessExitDoesNotInventCode(t *testing.T) {
	h, conn := terminationHandler(t, protocol.StateRunning)
	h.onProcessExited(protocol.Event{Kind: protocol.EventProcessExited, Payload: json.RawMessage(`{"exitCode":"invalid"}`)})
	assertTerminalEvents(t, conn, "")
	h.onSessionState(stateEvent(protocol.StateExited))
	assertTerminalEvents(t, conn, "terminated")
}

// The real hub must observe channel closure, not a fake ProcessExited. Keep a
// second client alive so neither handler.Close nor last-client shutdown can
// accidentally supply the completion this regression requires.
type closingDebugger struct {
	*raceDebugger
	killCalls chan struct{}
	mu        sync.Mutex
	killErr   error
	killReply <-chan error
	closeOnce sync.Once
}

func (d *closingDebugger) Attach(int, string) error {
	return d.Launch("", nil, nil)
}

func (d *closingDebugger) Kill() error {
	d.killCalls <- struct{}{}
	if d.killReply != nil {
		return <-d.killReply
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.killErr
}

func TestDisconnectRetriesAnUnconfirmedTerminate(t *testing.T) {
	killReply := make(chan error)
	dbg := &closingDebugger{
		raceDebugger: newRaceDebugger(),
		killCalls:    make(chan struct{}, 8),
		killReply:    killReply,
	}
	log := slog.New(slog.NewTextHandler(nopWriter{}, nil))
	session := hub.NewSession("termination", func() debugger.Debugger { return dbg }, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.Run(ctx)
	t.Cleanup(func() {
		close(killReply)
		cancel()
		select {
		case <-session.Done():
		case <-time.After(3 * time.Second):
			t.Error("hub did not shut down")
		}
	})
	obs := &terminationObserver{observer: newObserver(), delivered: make(chan protocol.Event, 64)}
	if _, err := session.AddClient(obs, log); err != nil {
		t.Fatal(err)
	}
	hh := newHarnessProvider(t, &hubProvider{session: session}, nil)
	hh.sendReq("initialize", initArgs())
	recvType[*godap.InitializeResponse](hh)
	hh.sendReq("attach", &godap.AttachRequest{Arguments: json.RawMessage(`{"pid":123,"stopOnEntry":true}`)})
	recvType[*godap.InitializedEvent](hh)
	hh.sendReq("configurationDone", &godap.ConfigurationDoneRequest{})
	recvType[*godap.ConfigurationDoneResponse](hh)
	recvType[*godap.AttachResponse](hh)
	recvType[*godap.StoppedEvent](hh)
	hh.sendReq("terminate", terminateRequest(0))
	recvType[*godap.TerminateResponse](hh)
	select {
	case <-dbg.killCalls:
	case <-time.After(3 * time.Second):
		t.Fatal("first Stop never reached Kill")
	}
	hh.sendReq("disconnect", &godap.DisconnectRequest{
		Arguments: &godap.DisconnectArguments{TerminateDebuggee: true},
	})
	if _, ok := hh.recv().(*godap.DisconnectResponse); !ok {
		t.Fatal("unconfirmed cleanup emitted a terminal")
	}
	select {
	case killReply <- debugger.ErrAttachedDetachIncomplete:
	case <-time.After(3 * time.Second):
		t.Fatal("first Kill did not await its outcome")
	}
	select {
	case <-dbg.killCalls:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect lost its retry after the unconfirmed Kill failed")
	}
	select {
	case killReply <- nil:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect Kill did not await its outcome")
	}
	dbg.complete()
	obs.waitState(t, protocol.StateExited)
	obs.waitState(t, protocol.StateIdle)
	select {
	case <-session.Done():
		t.Fatal("cleanup depended on shutting down the observer session")
	default:
	}
}

func (d *closingDebugger) complete() {
	d.closeOnce.Do(func() { close(d.events) })
}

type terminationObserver struct {
	*observer
	delivered chan protocol.Event
}

func (o *terminationObserver) WriteMessage(_ int, data []byte) error {
	evt, err := protocol.UnmarshalEvent(data)
	if err != nil {
		return err
	}
	select {
	case o.delivered <- evt:
		return nil
	case <-o.closed:
		return io.EOF
	}
}

func (o *terminationObserver) waitState(t *testing.T, state protocol.SessionState) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-o.delivered:
			if evt.Kind == protocol.EventProcessExited {
				t.Fatal("explicit Kill fabricated a process exit")
			}
			if evt.Kind == protocol.EventSessionState {
				var p protocol.SessionStatePayload
				if err := protocol.DecodeEventPayload(evt, &p); err != nil {
					t.Fatal(err)
				}
				if p.State == state {
					return
				}
			}
		case <-deadline:
			t.Fatalf("observer never saw %s", state)
		}
	}
}

func TestSingleTerminateCompletesOnHubDebuggerClosure(t *testing.T) {
	for _, suspended := range []bool{false, true} {
		for _, retry := range []bool{false, true} {
			name := "running"
			if suspended {
				name = "suspended"
			}
			if retry {
				name += "/retryable detach"
			}
			t.Run(name, func(t *testing.T) {
				checkSingleTerminateClosure(t, suspended, retry)
			})
		}
	}
}

func checkSingleTerminateClosure(t *testing.T, suspended, retry bool) {
	t.Helper()
	dbg := &closingDebugger{raceDebugger: newRaceDebugger(), killCalls: make(chan struct{}, 8)}
	if retry {
		dbg.killErr = debugger.ErrAttachedDetachIncomplete
	}
	log := slog.New(slog.NewTextHandler(nopWriter{}, nil))
	session := hub.NewSession("termination", func() debugger.Debugger { return dbg }, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go session.Run(ctx)
	t.Cleanup(func() {
		dbg.mu.Lock()
		dbg.killErr = nil
		dbg.mu.Unlock()
		cancel()
		select {
		case <-session.Done():
		case <-time.After(3 * time.Second):
			t.Error("hub did not shut down")
		}
	})
	obs := &terminationObserver{observer: newObserver(), delivered: make(chan protocol.Event, 64)}
	if _, err := session.AddClient(obs, log); err != nil {
		t.Fatal(err)
	}
	hh := newHarnessProvider(t, &hubProvider{session: session}, nil)
	launchTerminationSession(t, hh, obs, suspended)
	hh.sendReq("terminate", terminateRequest(0))
	awaitTerminateAcknowledgement(t, hh, retry)
	awaitTerminationKill(t, dbg, "first Stop never reached Kill")
	if retry {
		retryTerminationKill(t, hh, dbg)
	}
	assertHubTerminationClosure(t, hh, session, obs, dbg)
}

func launchTerminationSession(t *testing.T, hh *harness, obs *terminationObserver, suspended bool) {
	t.Helper()
	hh.sendReq("initialize", initArgs())
	recvType[*godap.InitializeResponse](hh)
	args, err := json.Marshal(map[string]any{"program": "/fake", "stopOnEntry": suspended})
	if err != nil {
		t.Fatal(err)
	}
	hh.sendReq("launch", &godap.LaunchRequest{Arguments: args})
	recvType[*godap.InitializedEvent](hh)
	hh.sendReq("configurationDone", &godap.ConfigurationDoneRequest{})
	recvType[*godap.ConfigurationDoneResponse](hh)
	recvType[*godap.LaunchResponse](hh)
	if suspended {
		recvType[*godap.StoppedEvent](hh)
	}
	obs.waitState(t, protocol.StateSuspended)
	if !suspended {
		obs.waitState(t, protocol.StateRunning)
	}
}

func awaitTerminateAcknowledgement(t *testing.T, hh *harness, retry bool) {
	t.Helper()
	acknowledged, rejected := false, false
	for !acknowledged || (retry && !rejected) {
		switch message := hh.recv().(type) {
		case *godap.TerminateResponse:
			acknowledged = true
		case *godap.OutputEvent:
			if !retry || !strings.Contains(message.Body.Output, debugger.ErrAttachedDetachIncomplete.Error()) {
				t.Fatalf("unexpected output: %+v", message)
			}
			rejected = true
		default:
			t.Fatalf("unexpected message before cleanup: %T", message)
		}
	}
}

func awaitTerminationKill(t *testing.T, dbg *closingDebugger, failure string) {
	t.Helper()
	select {
	case <-dbg.killCalls:
	case <-time.After(3 * time.Second):
		t.Fatal(failure)
	}
}

func retryTerminationKill(t *testing.T, hh *harness, dbg *closingDebugger) {
	t.Helper()
	dbg.mu.Lock()
	dbg.killErr = nil
	dbg.mu.Unlock()
	hh.sendReq("terminate", terminateRequest(0))
	recvType[*godap.TerminateResponse](hh)
	awaitTerminationKill(t, dbg, "failed Stop was not retryable")
}

func assertHubTerminationClosure(t *testing.T, hh *harness, session *hub.Hub, obs *terminationObserver, dbg *closingDebugger) {
	t.Helper()
	// A response to a later request is a deterministic DAP read-loop
	// barrier; no lifecycle event is allowed before actual closure.
	hh.sendReq("setExceptionBreakpoints", &godap.SetExceptionBreakpointsRequest{})
	if _, ok := hh.recv().(*godap.SetExceptionBreakpointsResponse); !ok {
		t.Fatal("terminate acknowledgement prematurely ended the session")
	}
	dbg.complete()
	if _, ok := hh.recv().(*godap.TerminatedEvent); !ok {
		t.Fatal("channel closure must emit terminated without an invented exited")
	}
	obs.waitState(t, protocol.StateExited)
	obs.waitState(t, protocol.StateIdle)
	if session.ClientCount() != 2 {
		t.Fatal("terminal depended on closing a client")
	}
	hh.sendReq("disconnect", &godap.DisconnectRequest{})
	if _, ok := hh.recv().(*godap.DisconnectResponse); !ok {
		t.Fatal("duplicate terminal before disconnect")
	}
	select {
	case <-dbg.killCalls:
		t.Fatal("disconnect issued a redundant Kill")
	default:
	}
	select {
	case <-session.Done():
		t.Fatal("Stop shut down the shared observer session")
	default:
	}
}
