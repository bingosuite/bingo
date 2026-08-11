package client_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"

	"github.com/gorilla/websocket"
)

func TestCreateContextCancelsStalledHandshake(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, createErr := client.CreateContext(ctx, listener.Addr().String())
		result <- createErr
	}()

	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("client did not start the WebSocket handshake")
	}
	defer func() { _ = conn.Close() }()

	cancel()
	select {
	case createErr := <-result:
		if !errors.Is(createErr, context.Canceled) {
			t.Fatalf("CreateContext error = %v, want context cancellation", createErr)
		}
	case <-time.After(time.Second):
		t.Fatal("CreateContext did not cancel the stalled WebSocket handshake")
	}
}

func TestCreateContextCancelsWelcomeWait(t *testing.T) {
	welcomeWaitActive := make(chan error, 1)
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			welcomeWaitActive <- err
			return
		}
		defer func() { _ = conn.Close() }()

		pong := make(chan struct{})
		var pongOnce sync.Once
		conn.SetPongHandler(func(data string) error {
			if data == "welcome-ready" {
				pongOnce.Do(func() { close(pong) })
			}
			return nil
		})
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		if err := conn.WriteControl(
			websocket.PingMessage,
			[]byte("welcome-ready"),
			time.Now().Add(time.Second),
		); err != nil {
			welcomeWaitActive <- err
			return
		}
		select {
		case <-pong:
			welcomeWaitActive <- nil
		case <-time.After(time.Second):
			welcomeWaitActive <- errors.New("client read pump did not answer ping")
			return
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, createErr := client.CreateContext(ctx, strings.TrimPrefix(server.URL, "http://"))
		result <- createErr
	}()

	select {
	case err := <-welcomeWaitActive:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not enter the post-upgrade welcome wait")
	}

	start := time.Now()
	cancel()
	select {
	case createErr := <-result:
		if !errors.Is(createErr, context.Canceled) {
			t.Fatalf("CreateContext error = %v, want context cancellation", createErr)
		}
		if !strings.Contains(createErr.Error(), "wait for session state") {
			t.Fatalf("CreateContext error = %v, want welcome-wait context", createErr)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("welcome wait cancellation took %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("CreateContext did not cancel the welcome wait")
	}
}

func TestListSessionsContextCancelsRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, listErr := client.ListSessionsContext(ctx, strings.TrimPrefix(server.URL, "http://"))
		result <- listErr
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("list sessions request did not start")
	}
	cancel()
	select {
	case listErr := <-result:
		if !errors.Is(listErr, context.Canceled) {
			t.Fatalf("ListSessionsContext error = %v, want context cancellation", listErr)
		}
	case <-time.After(time.Second):
		t.Fatal("ListSessionsContext did not cancel the request")
	}
}

func TestPeerCloseNormalizesCommandErrors(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		welcome := protocol.MustEvent(protocol.EventSessionState, 1, protocol.SessionStatePayload{
			SessionID: "closing-session",
			State:     protocol.StateIdle,
			Clients:   1,
		})
		data, err := protocol.MarshalEvent(welcome)
		if err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second),
		)
	}))
	defer server.Close()

	c, err := client.Create(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	select {
	case _, ok := <-c.Events():
		if ok {
			t.Fatal("unexpected event before peer-close completion")
		}
	case <-time.After(time.Second):
		t.Fatal("client did not observe peer close")
	}

	if err := c.Continue(); !errors.Is(err, client.ErrClosed) {
		t.Errorf("Continue error = %v, want client.ErrClosed", err)
	}
	if _, err := c.SetBreakpoint("main.go", 42); !errors.Is(err, client.ErrClosed) {
		t.Errorf("SetBreakpoint error = %v, want client.ErrClosed", err)
	}
}

// fakeServer is a minimal WebSocket bingo server that drives the real client
// through its actual dial → command → confirmation flow. It records every
// command it receives and answers each with an optional reply event.
type fakeServer struct {
	ts    *httptest.Server
	reply func(protocol.Command) (protocol.Event, bool)

	mu       sync.Mutex
	commands []protocol.Command
}

func newFakeServer(reply func(protocol.Command) (protocol.Event, bool)) *fakeServer {
	fs := &fakeServer{reply: reply}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	fs.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		// Welcome message: the client blocks in dial until this arrives.
		welcome := protocol.MustEvent(protocol.EventSessionState, 1, protocol.SessionStatePayload{
			SessionID: "test-session",
			State:     protocol.StateIdle,
			Clients:   1,
		})
		if b, err := protocol.MarshalEvent(welcome); err == nil {
			_ = conn.WriteMessage(websocket.TextMessage, b)
		}

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			cmd, err := protocol.UnmarshalCommand(data)
			if err != nil {
				continue
			}
			fs.mu.Lock()
			fs.commands = append(fs.commands, cmd)
			fs.mu.Unlock()

			if fs.reply != nil {
				if evt, ok := fs.reply(cmd); ok {
					if b, err := protocol.MarshalEvent(evt); err == nil {
						_ = conn.WriteMessage(websocket.TextMessage, b)
					}
				}
			}
		}
	}))
	return fs
}

func (fs *fakeServer) addr() string { return strings.TrimPrefix(fs.ts.URL, "http://") }
func (fs *fakeServer) close()       { fs.ts.Close() }

func (fs *fakeServer) lastCommand() (protocol.Command, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.commands) == 0 {
		return protocol.Command{}, false
	}
	return fs.commands[len(fs.commands)-1], true
}

func newScriptedServer(t *testing.T, script func(*websocket.Conn)) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade WebSocket: %v", err)
			close(done)
			return
		}
		defer close(done)
		defer func() { _ = conn.Close() }()
		script(conn)
	}))
	return ts, done
}

func writeServerEvent(t *testing.T, conn *websocket.Conn, event protocol.Event) {
	t.Helper()
	data, err := protocol.MarshalEvent(event)
	if err != nil {
		t.Errorf("marshal event: %v", err)
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Errorf("write event: %v", err)
	}
}

func replyEvent(kind protocol.EventKind, payload any) protocol.Event {
	return protocol.MustEvent(kind, 2, payload)
}

func dialTestClient(t *testing.T, fs *fakeServer) client.Client {
	t.Helper()
	c, err := client.Create(fs.addr())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return c
}

func TestCreateRejectsIncompatibleWelcomeVersion(t *testing.T) {
	ts, handlerDone := newScriptedServer(t, func(conn *websocket.Conn) {
		welcome := protocol.MustEvent(protocol.EventSessionState, 1, protocol.SessionStatePayload{
			SessionID: "test-session",
			State:     protocol.StateIdle,
			Clients:   1,
		})
		welcome.Version = "999.0"
		writeServerEvent(t, conn, welcome)
		_, _, _ = conn.ReadMessage()
	})
	defer ts.Close()

	c, err := client.Create(strings.TrimPrefix(ts.URL, "http://"))
	if c != nil {
		_ = c.Close()
		t.Fatal("Create returned a client for an incompatible welcome")
	}
	if err == nil ||
		!strings.Contains(err.Error(), `expected "`+protocol.Version+`"`) ||
		!strings.Contains(err.Error(), `received "999.0"`) {
		t.Fatalf("Create error does not identify both versions: %v", err)
	}

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not close the incompatible welcome connection")
	}
}

func requireVersionError(t *testing.T, context string, err error) {
	t.Helper()
	var versionErr *protocol.VersionError
	if !errors.As(err, &versionErr) {
		t.Fatalf("%s = %v, want VersionError", context, err)
	}
	if versionErr.Expected != protocol.Version || versionErr.Received != "999.0" {
		t.Fatalf("%s = %v, want expected %q and received %q", context, err, protocol.Version, "999.0")
	}
}

func TestMidstreamVersionMismatchFailsPendingRequest(t *testing.T) {
	ts, handlerDone := newScriptedServer(t, func(conn *websocket.Conn) {
		writeServerEvent(t, conn, protocol.MustEvent(
			protocol.EventSessionState,
			1,
			protocol.SessionStatePayload{
				SessionID: "test-session",
				State:     protocol.StateIdle,
				Clients:   1,
			},
		))

		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read SetBreakpoint: %v", err)
			return
		}
		cmd, err := protocol.UnmarshalCommand(data)
		if err != nil {
			t.Errorf("unmarshal SetBreakpoint: %v", err)
			return
		}
		if cmd.Kind != protocol.CmdSetBreakpoint {
			t.Errorf("command kind = %s, want %s", cmd.Kind, protocol.CmdSetBreakpoint)
			return
		}

		reply := protocol.MustEvent(
			protocol.EventBreakpointSet,
			2,
			protocol.BreakpointSetPayload{Breakpoint: protocol.Breakpoint{ID: 1}},
		)
		reply.Version = "999.0"
		writeServerEvent(t, conn, reply)
		_, _, _ = conn.ReadMessage()
	})
	defer ts.Close()

	c, err := client.Create(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = c.Close() }()

	errCh := make(chan error, 1)
	go func() {
		_, err := c.SetBreakpoint("main.go", 42)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		requireVersionError(t, "SetBreakpoint error", err)
	case <-time.After(2 * time.Second):
		t.Fatal("SetBreakpoint waited instead of failing on the terminal version error")
	}

	select {
	case _, ok := <-c.Events():
		if ok {
			t.Fatal("Events remained open after a terminal version mismatch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client read pump did not terminate after a version mismatch")
	}

	start := time.Now()
	_, err = c.SetBreakpoint("main.go", 43)
	requireVersionError(t, "later SetBreakpoint error", err)
	if time.Since(start) > time.Second {
		t.Fatal("later request waited instead of reusing the terminal version error")
	}
	closeErr := c.Close()
	requireVersionError(t, "Close error", closeErr)

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not close the mid-stream incompatible connection")
	}
}

// TestRestartEmptyArgsOverrideReachesWire is the client-side regression for
// issue #102: an explicit empty-slice override must serialise as [] so the hub
// can distinguish "clear the args" from "reuse the original Launch args".
func TestRestartEmptyArgsOverrideReachesWire(t *testing.T) {
	fs := newFakeServer(func(cmd protocol.Command) (protocol.Event, bool) {
		if cmd.Kind == protocol.CmdRestart {
			return replyEvent(protocol.EventRestarted, protocol.RestartedPayload{Program: "/app"}), true
		}
		return protocol.Event{}, false
	})
	defer fs.close()

	c := dialTestClient(t, fs)
	defer func() { _ = c.Close() }()

	if _, err := c.Restart([]string{}, []string{}); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	cmd, ok := fs.lastCommand()
	if !ok || cmd.Kind != protocol.CmdRestart {
		t.Fatalf("server did not receive a Restart command; got %+v ok=%v", cmd, ok)
	}
	payload := string(cmd.Payload)
	if !strings.Contains(payload, `"args":[]`) {
		t.Errorf("empty Args override dropped from wire; payload=%s", payload)
	}
	if !strings.Contains(payload, `"env":[]`) {
		t.Errorf("empty Env override dropped from wire; payload=%s", payload)
	}
}

// TestRestartNilArgsDoesNotClearOnWire is the companion: nil means "reuse", so
// it must NOT serialise as an empty [] that the hub would read as "clear".
func TestRestartNilArgsDoesNotClearOnWire(t *testing.T) {
	fs := newFakeServer(func(cmd protocol.Command) (protocol.Event, bool) {
		if cmd.Kind == protocol.CmdRestart {
			return replyEvent(protocol.EventRestarted, protocol.RestartedPayload{Program: "/app"}), true
		}
		return protocol.Event{}, false
	})
	defer fs.close()

	c := dialTestClient(t, fs)
	defer func() { _ = c.Close() }()

	if _, err := c.Restart(nil, nil); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	cmd, ok := fs.lastCommand()
	if !ok {
		t.Fatal("server did not receive a Restart command")
	}
	if strings.Contains(string(cmd.Payload), `"args":[]`) {
		t.Errorf("nil Args must not encode as []; payload=%s", cmd.Payload)
	}
}

// TestSetBreakpointReturnsConfirmation exercises the synchronous demux happy
// path: the confirmation event's payload is decoded and returned to the caller.
func TestSetBreakpointReturnsConfirmation(t *testing.T) {
	fs := newFakeServer(func(cmd protocol.Command) (protocol.Event, bool) {
		if cmd.Kind == protocol.CmdSetBreakpoint {
			return replyEvent(protocol.EventBreakpointSet, protocol.BreakpointSetPayload{
				Breakpoint: protocol.Breakpoint{
					ID:       7,
					Enabled:  true,
					Location: protocol.Location{File: "main.go", Line: 42},
				},
			}), true
		}
		return protocol.Event{}, false
	})
	defer fs.close()

	c := dialTestClient(t, fs)
	defer func() { _ = c.Close() }()

	bp, err := c.SetBreakpoint("main.go", 42)
	if err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}
	if bp.ID != 7 || bp.Location.Line != 42 {
		t.Errorf("unexpected breakpoint: %+v", bp)
	}
}

// TestSyncCommandRoutesServerError verifies an EventError for the same command
// kind satisfies (and fails) the pending synchronous request.
func TestSyncCommandRoutesServerError(t *testing.T) {
	fs := newFakeServer(func(cmd protocol.Command) (protocol.Event, bool) {
		if cmd.Kind == protocol.CmdSetBreakpoint {
			return replyEvent(protocol.EventError, protocol.ErrorPayload{
				Command: protocol.CmdSetBreakpoint,
				Message: "no address for main.go:42",
			}), true
		}
		return protocol.Event{}, false
	})
	defer fs.close()

	c := dialTestClient(t, fs)
	defer func() { _ = c.Close() }()

	_, err := c.SetBreakpoint("main.go", 42)
	if err == nil || !strings.Contains(err.Error(), "no address for main.go:42") {
		t.Fatalf("expected routed server error, got %v", err)
	}
}

// TestCloseUnblocksPendingSyncCall ensures a synchronous call returns promptly
// (rather than blocking until its timeout) when the client is closed while the
// server never answers.
func TestCloseUnblocksPendingSyncCall(t *testing.T) {
	fs := newFakeServer(func(protocol.Command) (protocol.Event, bool) {
		return protocol.Event{}, false // never reply
	})
	defer fs.close()

	c := dialTestClient(t, fs)

	errCh := make(chan error, 1)
	go func() {
		_, err := c.SetBreakpoint("main.go", 42)
		errCh <- err
	}()

	// Give the command time to reach the server and register the pending req.
	_ = c.Close()

	err := <-errCh
	if err == nil {
		t.Fatal("expected an error after Close, got nil")
	}
}

// TestGoroutinesUnwrapsPackedPayload verifies the client unwraps the 1.3
// GoroutinesPayload — including the optional Totals a bounded list carries —
// and returns the real truncated list rather than erroring or emptying it.
// See issue #194.
func TestGoroutinesUnwrapsPackedPayload(t *testing.T) {
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
	packed, report := protocol.PackGoroutines(raw, false)
	if report.Degraded || !report.Omitted() {
		t.Fatalf("report = %+v; want a real truncated list", report)
	}

	fs := newFakeServer(func(cmd protocol.Command) (protocol.Event, bool) {
		if cmd.Kind == protocol.CmdGoroutines {
			return replyEvent(protocol.EventGoroutines, packed), true
		}
		return protocol.Event{}, false
	})
	defer fs.close()

	c := dialTestClient(t, fs)
	defer func() { _ = c.Close() }()

	gs, err := c.Goroutines()
	if err != nil {
		t.Fatalf("Goroutines: %v", err)
	}
	if len(gs) != len(packed.Goroutines) {
		t.Fatalf("Goroutines = %d; want the %d packed entries", len(gs), len(packed.Goroutines))
	}
	if gs[0].ID != packed.Goroutines[0].ID {
		t.Errorf("Goroutines[0].ID = %d; want %d", gs[0].ID, packed.Goroutines[0].ID)
	}

	// GoroutineList carries the honesty channel Goroutines cannot: without it a
	// caller has no way to learn the list was bounded, and would report a
	// truncated set as the whole runtime.
	list, err := c.GoroutineList()
	if err != nil {
		t.Fatalf("GoroutineList: %v", err)
	}
	if len(list.Goroutines) != len(gs) {
		t.Fatalf("GoroutineList returned %d entries; Goroutines returned %d",
			len(list.Goroutines), len(gs))
	}
	if list.Totals == nil {
		t.Fatal("GoroutineList dropped Totals for a truncated list")
	}
	if list.Totals.Goroutines != len(raw) {
		t.Errorf("Totals.Goroutines = %d; want the original %d", list.Totals.Goroutines, len(raw))
	}
}

// TestGoroutineListOmitsTotalsWhenComplete pins the other half of the contract:
// Totals is absent exactly when nothing was left out, so its presence alone
// means "this is not everything".
func TestGoroutineListOmitsTotalsWhenComplete(t *testing.T) {
	packed, report := protocol.PackGoroutines([]protocol.Goroutine{
		{ID: 1, Status: "running", Current: true},
		{ID: 2, Status: "waiting"},
	}, false)
	if report.Omitted() {
		t.Fatalf("fixture must be complete, got %+v", report)
	}

	fs := newFakeServer(func(cmd protocol.Command) (protocol.Event, bool) {
		if cmd.Kind == protocol.CmdGoroutines {
			return replyEvent(protocol.EventGoroutines, packed), true
		}
		return protocol.Event{}, false
	})
	defer fs.close()

	c := dialTestClient(t, fs)
	defer func() { _ = c.Close() }()

	list, err := c.GoroutineList()
	if err != nil {
		t.Fatalf("GoroutineList: %v", err)
	}
	if list.Totals != nil {
		t.Errorf("Totals = %+v; want nil for a complete list", list.Totals)
	}
	if len(list.Goroutines) != 2 {
		t.Errorf("Goroutines = %d; want 2", len(list.Goroutines))
	}
}
