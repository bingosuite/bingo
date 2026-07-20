package dap

import (
	"bufio"
	"log/slog"
	"net"
	"testing"
	"time"

	godap "github.com/google/go-dap"
)

func quietServer(t *testing.T) *Server {
	t.Helper()
	rec := &cmdRecorder{}
	prov := &fakeProvider{sess: &fakeSession{id: "sess-test", cmds: rec}}
	return NewServer(prov, slog.New(slog.NewTextHandler(nopWriter{}, nil)))
}

// TestServerCloseWithIdleClient is the regression guard for the shutdown hang:
// a client that connected but never started a session has no hub to close its
// socket, so Close must force it shut rather than block forever in wg.Wait on
// the handler's ReadProtocolMessage.
func TestServerCloseWithIdleClient(t *testing.T) {
	s := quietServer(t)
	addr, err := s.Serve("127.0.0.1:0")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	conn, err := net.Dial("tcp4", addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Give the accept loop a moment to register the handler so Close must
	// actively tear it down (rather than racing before registration).
	time.Sleep(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- s.Close() }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Close hung with an idle connected client")
	}
}

// TestServerCloseAfterInitialize covers the same shutdown path once the client
// has spoken DAP but still has no session (initialize creates no session).
func TestServerCloseAfterInitialize(t *testing.T) {
	s := quietServer(t)
	addr, err := s.Serve("127.0.0.1:0")
	if err != nil {
		t.Fatalf("serve: %v", err)
	}

	conn, err := net.Dial("tcp4", addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	init := &godap.InitializeRequest{
		Request: godap.Request{
			ProtocolMessage: godap.ProtocolMessage{Seq: 1, Type: "request"},
			Command:         "initialize",
		},
	}
	if err := godap.WriteProtocolMessage(conn, init); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	// Drain the capabilities response so the handler is quiescent.
	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := godap.ReadProtocolMessage(r); err != nil {
		t.Fatalf("read capabilities: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Close() }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Close hung after initialize with no session")
	}
}
