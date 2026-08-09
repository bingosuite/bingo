package hub

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type admissionConn struct {
	command   []byte
	firstRead chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
}

func newAdmissionConn(command protocol.Command) *admissionConn {
	data, _ := json.Marshal(command)
	return &admissionConn{
		command:   data,
		firstRead: make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

func (c *admissionConn) ReadMessage() (int, []byte, error) {
	first := false
	c.readOnce.Do(func() {
		first = true
		close(c.firstRead)
	})
	if first {
		return TextMessage, c.command, nil
	}
	<-c.closed
	return 0, nil, io.EOF
}

func (*admissionConn) WriteMessage(int, []byte) error    { return nil }
func (*admissionConn) SetReadLimit(int64)                {}
func (*admissionConn) SetReadDeadline(time.Time) error   { return nil }
func (*admissionConn) SetWriteDeadline(time.Time) error  { return nil }
func (*admissionConn) SetPongHandler(func(string) error) {}
func (c *admissionConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestBlockedCommandAdmissionUnblocksReadPump(t *testing.T) {
	tests := []struct {
		name string
		stop func(*Hub, *Client)
	}{
		{
			name: "client teardown",
			stop: func(h *Hub, c *Client) {
				h.removeClient(c)
			},
		},
		{
			name: "hub shutdown",
			stop: func(h *Hub, _ *Client) {
				h.shutdown()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHub(nil)
			for i := 0; i < cap(h.cmdCh); i++ {
				h.cmdCh <- clientCommand{cmd: protocol.Command{Kind: protocol.CmdPause}}
			}

			conn := newAdmissionConn(protocol.Command{
				Version: protocol.Version,
				Kind:    protocol.CmdPause,
			})
			client := newClient(conn, h, nil)
			if !h.registry.add(client) {
				t.Fatal("client was not admitted")
			}

			if tt.name == "client teardown" {
				keepalive := newClient(newAdmissionConn(protocol.Command{}), h, nil)
				if !h.registry.add(keepalive) {
					t.Fatal("keepalive client was not admitted")
				}
			}

			readPumpDone := make(chan struct{})
			go func() {
				client.readPump()
				close(readPumpDone)
			}()

			select {
			case <-conn.firstRead:
			case <-time.After(time.Second):
				t.Fatal("read pump did not read the command")
			}
			select {
			case <-readPumpDone:
				t.Fatal("read pump returned before blocked admission was released")
			case <-time.After(20 * time.Millisecond):
			}

			tt.stop(h, client)

			select {
			case <-readPumpDone:
			case <-time.After(time.Second):
				t.Fatal("blocked read pump did not exit")
			}
			h.shutdown()
		})
	}
}

func TestCommandAdmissionTimeoutEvictsClient(t *testing.T) {
	h := newHub(nil)
	h.commandAdmissionTimeout = 20 * time.Millisecond
	for i := 0; i < cap(h.cmdCh); i++ {
		h.cmdCh <- clientCommand{cmd: protocol.Command{Kind: protocol.CmdPause}}
	}

	conn := newAdmissionConn(protocol.Command{})
	client := newClient(conn, h, nil)
	if !h.registry.add(client) {
		t.Fatal("client was not admitted")
	}

	admitted := h.injectCommand(client, protocol.Command{
		Version: protocol.Version,
		Kind:    protocol.CmdPause,
	})
	if admitted {
		t.Fatal("command was admitted despite a persistently full queue")
	}
	if got := h.registry.count(); got != 0 {
		t.Fatalf("client count = %d, want 0 after overload eviction", got)
	}
	select {
	case <-client.disconnected:
	default:
		t.Fatal("overloaded client was not marked disconnected")
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("overloaded client connection was not closed")
	}
	select {
	case <-h.shutdownCh:
	case <-time.After(time.Second):
		t.Fatal("last-client eviction did not shut down the hub")
	}
}

func TestKillAdmissionWaitsForCapacity(t *testing.T) {
	h := newHub(nil)
	for i := 0; i < cap(h.cmdCh); i++ {
		h.cmdCh <- clientCommand{cmd: protocol.Command{Kind: protocol.CmdPause}}
	}

	client := newClient(newAdmissionConn(protocol.Command{}), h, nil)
	admitted := make(chan bool, 1)
	go func() {
		admitted <- h.injectCommand(client, protocol.Command{
			Version: protocol.Version,
			Kind:    protocol.CmdKill,
		})
	}()

	select {
	case <-admitted:
		t.Fatal("Kill admission returned while the command queue was full")
	case <-time.After(20 * time.Millisecond):
	}

	<-h.cmdCh
	select {
	case ok := <-admitted:
		if !ok {
			t.Fatal("Kill admission was canceled after capacity became available")
		}
	case <-time.After(time.Second):
		t.Fatal("Kill admission did not resume when capacity became available")
	}

	foundKill := false
	for len(h.cmdCh) > 0 {
		if cc := <-h.cmdCh; cc.cmd.Kind == protocol.CmdKill {
			foundKill = true
		}
	}
	if !foundKill {
		t.Fatal("admitted Kill was not queued")
	}
}
