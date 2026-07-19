package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"

	"github.com/gorilla/websocket"
)

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
