package ws

import (
	"log"

	"github.com/gorilla/websocket"
)

type Connection struct {
	id   string
	conn *websocket.Conn
	hub  *Hub
	send chan Message
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
		if err := c.conn.Close(); err != nil {
			log.Printf("Connection %s close error: %v", c.id, err)
		}
	}()

	for {
		var msg Message
		err := c.conn.ReadJSON(&msg)
		if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
			log.Printf("Connection %s unexpected close: %v", c.id, err)
			break
		}
		if err != nil {
			break
		}
		c.hub.SendCommand(msg)
	}
}

func (c *Connection) WritePump() {
	defer func() {
		if err := c.conn.Close(); err != nil {
			log.Printf("Connection %s close error: %v", c.id, err)
		}
	}()

	for message := range c.send {
		if err := c.conn.WriteJSON(message); err != nil {
			log.Printf("Connection %s write error: %v", c.id, err)
			return
		}
	}
	if err := c.conn.WriteMessage(websocket.CloseMessage, []byte{}); err != nil {
		log.Printf("Failed to close websocket: %v", err)
		return
	}
}
