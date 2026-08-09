package dap

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"
)

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "temporary accept failure" }
func (temporaryAcceptError) Timeout() bool   { return false }
func (temporaryAcceptError) Temporary() bool { return true }

type retryListener struct {
	mu       sync.Mutex
	attempts int
	retried  chan struct{}
	closed   chan struct{}
	once     sync.Once
}

func newRetryListener() *retryListener {
	return &retryListener{
		retried: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (l *retryListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.attempts++
	attempt := l.attempts
	l.mu.Unlock()
	if attempt == 1 {
		return nil, temporaryAcceptError{}
	}
	l.once.Do(func() { close(l.retried) })
	<-l.closed
	return nil, net.ErrClosed
}

func (l *retryListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *retryListener) Addr() net.Addr { return retryAddr("retry") }

type retryAddr string

func (a retryAddr) Network() string { return "test" }
func (a retryAddr) String() string  { return string(a) }

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

func TestServeContextRejectsCanceledStartup(t *testing.T) {
	s := quietServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.ServeContext(ctx, "127.0.0.1:0"); err == nil {
		t.Fatal("ServeContext succeeded after cancellation")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestAcceptLoopRetriesTemporaryErrors(t *testing.T) {
	s := quietServer(t)
	listener := newRetryListener()
	s.mu.Lock()
	s.listener = listener
	s.wg.Add(1)
	s.mu.Unlock()
	go s.acceptLoop(listener)

	select {
	case <-listener.retried:
	case <-time.After(time.Second):
		t.Fatal("accept loop did not retry a temporary error")
	}

	if err := s.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close: %v", err)
	}
}
