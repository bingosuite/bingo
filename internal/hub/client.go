package hub

import (
	"log/slog"
	"sync"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

const (
	writeTimeout   = 10 * time.Second
	pongTimeout    = 60 * time.Second
	pingInterval   = 54 * time.Second
	maxMessageSize = 64 * 1024
)

// WSConn is the subset of a WebSocket connection the Client needs. Abstracted
// so tests can inject a fake without importing a WS library.
type WSConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetReadLimit(limit int64)
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	SetPongHandler(h func(appData string) error)
	Close() error
}

// WebSocket message types matching gorilla/websocket values.
const (
	TextMessage  = 1
	CloseMessage = 8
	PingMessage  = 9
	PongMessage  = 10
)

// Client represents one connected WebSocket peer.
type Client struct {
	conn WSConn
	hub  *Hub
	log  *slog.Logger

	// send is closed exactly once — by the registry on shutdown, or by
	// deliver() on buffer overflow. sendMu guards close-vs-send races.
	send   chan []byte
	sendMu sync.Mutex
	closed bool
}

func newClient(conn WSConn, h *Hub, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		conn: conn,
		hub:  h,
		send: make(chan []byte, 256),
		log:  log,
	}
}

func (c *Client) closeSend() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.send)
}

// writePump serialises outbound messages onto the WebSocket. One goroutine
// per client; exits when c.send is closed or a write fails.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingInterval)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if !ok {
				_ = c.conn.WriteMessage(CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(TextMessage, msg); err != nil {
				c.log.Warn("write error", "err", err)
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.conn.WriteMessage(PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

// readPump reads inbound messages and routes them to the hub. One goroutine
// per client; on return the client is considered disconnected.
func (c *Client) readPump() {
	defer func() {
		c.hub.removeClient(c)
	}()

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongTimeout))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if !isNormalClose(err) {
				c.log.Warn("read error", "err", err)
			}
			return
		}

		cmd, err := protocol.UnmarshalCommand(data)
		if err != nil {
			c.log.Warn("invalid command", "err", err, "raw", string(data))
			continue
		}

		c.hub.injectCommand(c, cmd)
	}
}

// deliver queues msg. Non-blocking: if the buffer is full the caller should
// evict the client so one slow client can't stall the hub.
func (c *Client) deliver(msg []byte) bool {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.send <- msg:
		return true
	default:
		c.log.Warn("send buffer full — evicting slow client")
		c.closed = true
		close(c.send)
		return false
	}
}

func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	s := err.Error()
	return s == "websocket: close 1000 (normal)" ||
		s == "websocket: close 1001 (going away)" ||
		s == "EOF" ||
		s == "use of closed network connection"
}
