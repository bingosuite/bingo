package ws

import (
	"log"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

type Connection struct {
	id        string
	conn      *websocket.Conn
	hub       *Hub
	send      chan Message
	closeOnce sync.Once
	sendOnce  sync.Once
}

func NewConnection(conn *websocket.Conn, hub *Hub, id string) *Connection {
	return &Connection{
		id:   id,
		conn: conn,
		hub:  hub,
		send: make(chan Message, clientSendBufferSize),
	}
}

func (c *Connection) ReadPump() {
	defer func() {
		c.hub.Unregister(c)
		c.closeConn()
	}()

	for {
		var msg Message
		err := c.conn.ReadJSON(&msg)
		if err != nil {
			// Only log truly unexpected errors, not normal client disconnects
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseAbnormalClosure) {
				log.Printf("[Connection] Connection %s unexpected close: %v", c.id, err)
			}
			break
		}
		c.hub.SendCommand(msg)
	}
}

func (c *Connection) WritePump() {
	defer c.closeConn()

	for message := range c.send {
		if err := c.conn.WriteJSON(message); err != nil {
			log.Printf("[Connection] Connection %s write error: %v", c.id, err)
			return
		}
	}
	// Send channel closed by server - try to send graceful close message
	// If the connection was already closed by client, this will fail silently
	if err := c.conn.WriteMessage(websocket.CloseMessage, []byte{}); err != nil {
		// Don't log if connection was already closed
		if !isConnectionClosedError(err) {
			log.Printf("[Connection] Connection %s: failed to send close message: %v", c.id, err)
		}
	}
}

func (c *Connection) closeConn() {
	c.closeOnce.Do(func() {
		if err := c.conn.Close(); err != nil {
			// Don't log if connection was already closed
			if !isConnectionClosedError(err) {
				log.Printf("[Connection] Connection %s close error: %v", c.id, err)
			}
		}
	})
}

func (c *Connection) Close() {
	c.closeConn()
}

func (c *Connection) CloseSend() {
	c.sendOnce.Do(func() {
		close(c.send)
	})
}

func isConnectionClosedError(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	return websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
		strings.Contains(errMsg, "use of closed network connection") ||
		strings.Contains(errMsg, "broken pipe")
}
