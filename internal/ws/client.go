package ws

import (
	"log"

	"github.com/gorilla/websocket"
)

type Client struct {
	conn *websocket.Conn
	hub  *Hub
	send chan Message
}

func NewClient(conn *websocket.Conn, hub *Hub) *Client {
	return &Client{
		conn: conn,
		hub:  hub,
		send: make(chan Message, 256),
	}
}

func (c *Client) ReadPump() {
	defer func() {
		c.hub.Unregister(c)
		if err := c.conn.Close(); err != nil {
			log.Printf("Client close error: %v", err)
		}
	}()

	for {
		var msg Message
		err := c.conn.ReadJSON(&msg)
		if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
			log.Printf("Client unexpected close: %v", err)
			break
		}
		if err != nil {
			break
		}
		c.hub.SendCommand(msg)
	}
}

func (c *Client) WritePump() {
	defer func() {
		if err := c.conn.Close(); err != nil {
			log.Printf("Client close error: %v", err)
		}
	}()

	for message := range c.send {
		if err := c.conn.WriteJSON(message); err != nil {
			log.Printf("Client write error: %v", err)
			return
		}
	}
	if err := c.conn.WriteMessage(websocket.CloseMessage, []byte{}); err != nil {
		panic(err)
	}
}
