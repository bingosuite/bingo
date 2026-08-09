package hub

import (
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bingosuite/bingo/pkg/protocol"
)

const (
	writeTimeout   = 10 * time.Second
	pongTimeout    = 60 * time.Second
	pingInterval   = 54 * time.Second
	maxMessageSize = 64 * 1024

	closeProtocolError = 1002
	maxCloseReasonSize = 123
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

	// writeMu serialises the normal write pump with a protocol close emitted by
	// the read pump when an incompatible peer must be rejected immediately.
	writeMu sync.Mutex
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
			if !ok {
				_ = c.writeMessage(CloseMessage, nil)
				return
			}
			if err := c.writeMessage(TextMessage, msg); err != nil {
				c.log.Warn("write error", "err", err)
				return
			}

		case <-ticker.C:
			if err := c.writeMessage(PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) writeMessage(messageType int, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteMessage(messageType, data)
}

func (c *Client) closeWithProtocolError(err error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.closeSend()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_ = c.conn.WriteMessage(CloseMessage, formatCloseMessage(closeProtocolError, err.Error()))
	_ = c.conn.Close()
}

func formatCloseMessage(code int, reason string) []byte {
	if len(reason) > maxCloseReasonSize {
		reason = reason[:maxCloseReasonSize]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	msg := make([]byte, 2+len(reason))
	msg[0] = byte(code >> 8)
	msg[1] = byte(code)
	copy(msg[2:], reason)
	return msg
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
		if err := protocol.ValidateVersion(cmd.Version); err != nil {
			c.log.Warn("incompatible command", "err", err)
			c.closeWithProtocolError(err)
			return
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
