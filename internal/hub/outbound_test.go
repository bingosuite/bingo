package hub

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type outboundConn struct {
	writes    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

func newOutboundConn() *outboundConn {
	return &outboundConn{
		writes: make(chan []byte, 4),
		closed: make(chan struct{}),
	}
}

func (c *outboundConn) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, io.EOF
}

func (c *outboundConn) WriteMessage(messageType int, data []byte) error {
	if messageType == TextMessage {
		c.writes <- append([]byte(nil), data...)
	}
	return nil
}

func (*outboundConn) SetReadLimit(int64)                {}
func (*outboundConn) SetReadDeadline(time.Time) error   { return nil }
func (*outboundConn) SetWriteDeadline(time.Time) error  { return nil }
func (*outboundConn) SetPongHandler(func(string) error) {}

func (c *outboundConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestJoinCannotSplitSuspendingEventFromStateTransition(t *testing.T) {
	h := newHub(nil)
	h.sessionID = "session"
	h.state = protocol.StateRunning

	existing := newClient(newOutboundConn(), h, nil)
	if !h.registry.add(existing) {
		t.Fatal("existing client was not admitted")
	}
	existing.sendMu.Lock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handled := make(chan struct{})
	go func() {
		h.handleEvent(ctx, protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
		close(handled)
	}()

	deadline := time.Now().Add(time.Second)
	for h.outboundMu.TryLock() {
		h.outboundMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("suspending event did not enter outbound delivery")
		}
		time.Sleep(time.Millisecond)
	}

	joiningConn := newOutboundConn()
	added := make(chan error, 1)
	go func() {
		_, err := h.AddClient(joiningConn, nil)
		added <- err
	}()

	select {
	case err := <-added:
		t.Fatalf("join completed inside the stop/state transaction: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	existing.sendMu.Unlock()

	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("suspending event did not complete")
	}
	select {
	case err := <-added:
		if err != nil {
			t.Fatalf("join failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("join remained blocked after the stop/state transaction")
	}

	select {
	case wire := <-joiningConn.writes:
		evt, err := protocol.UnmarshalEvent(wire)
		if err != nil {
			t.Fatalf("decode join state: %v", err)
		}
		if evt.Kind != protocol.EventSessionState {
			t.Fatalf("first join event = %s, want %s", evt.Kind, protocol.EventSessionState)
		}
		var state protocol.SessionStatePayload
		if err := protocol.DecodeEventPayload(evt, &state); err != nil {
			t.Fatalf("decode join state payload: %v", err)
		}
		if state.State != protocol.StateSuspended {
			t.Fatalf("join state = %s, want %s", state.State, protocol.StateSuspended)
		}
	case <-time.After(time.Second):
		t.Fatal("join state was not delivered")
	}

	h.shutdown()
}
