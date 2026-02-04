package client

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sync/atomic"

	"github.com/bingosuite/bingo/internal/ws"
	"github.com/gorilla/websocket"
)

type Client struct {
	serverURL string
	sessionID string
	conn      *websocket.Conn
	send      chan ws.Message
	done      chan struct{}
	state     atomic.Value // ws.State
}

func NewClient(serverURL, sessionID string) *Client {
	c := &Client{
		serverURL: serverURL,
		sessionID: sessionID,
		send:      make(chan ws.Message, 256),
		done:      make(chan struct{}),
	}
	c.state.Store(ws.StateExecuting)
	return c
}

func (c *Client) Connect() error {
	// Build WebSocket URL with session ID
	u := url.URL{
		Scheme:   "ws",
		Host:     c.serverURL,
		Path:     "/ws/",
		RawQuery: "session=" + c.sessionID,
	}
	log.Printf("Connecting to %s", u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("dial error: %w", err)
	}

	c.conn = conn
	log.Println("Connected to server")
	return nil
}

func (c *Client) Run() error {
	if c.conn == nil {
		return fmt.Errorf("connection not established")
	}

	// Start read and write pumps
	go c.readPump()
	go c.writePump()

	return nil
}

func (c *Client) readPump() {
	defer func() {
		close(c.done)
		if err := c.conn.Close(); err != nil {
			log.Printf("Close error: %v", err)
		}
	}()

	for {
		var msg ws.Message
		if err := c.conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			return
		}

		c.handleMessage(msg)
	}
}

func (c *Client) writePump() {
	for message := range c.send {
		if err := c.conn.WriteJSON(message); err != nil {
			log.Printf("Write error: %v", err)
			return
		}
	}
	if err := c.conn.WriteMessage(websocket.CloseMessage, []byte{}); err != nil {
		log.Printf("Failed to close websocket: %v", err)
		return
	}
	c.state.Store(ws.StateExecuting)
}

func (c *Client) handleMessage(msg ws.Message) {
	log.Printf("Received message type: %s", msg.Type)

	switch ws.EventType(msg.Type) {
	case ws.EventSessionStarted:
		log.Println("Debug session started")

	case ws.EventStateUpdate:
		var update ws.StateUpdateEvent
		if err := unmarshalData(msg.Data, &update); err != nil {
			log.Printf("Error parsing stateUpdate: %v", err)
			return
		}
		c.setState(update.NewState)
		log.Printf("State updated: %s", update.NewState)

	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

func unmarshalData(data []byte, v interface{}) error {
	// Handle empty data
	if len(data) == 0 {
		return nil
	}
	return unmarshalJSON(data, v)
}

func unmarshalJSON(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

func (c *Client) SendCommand(cmdType string, payload []byte) error {
	msg := ws.Message{
		Type: cmdType,
		Data: payload,
	}

	select {
	case c.send <- msg:
		log.Printf("Queued command: %s", cmdType)
		return nil
	case <-c.done:
		return fmt.Errorf("connection closed")
	}
}

func (c *Client) Continue() error {
	cmd := ws.ContinueCmd{
		Type:      ws.CmdContinue,
		SessionID: c.sessionID,
	}
	payload, err := marshalJSON(cmd)
	if err != nil {
		return err
	}
	return c.SendCommand(string(ws.CmdContinue), payload)
}

func (c *Client) StepOver() error {
	cmd := ws.StepOverCmd{
		Type:      ws.CmdStepOver,
		SessionID: c.sessionID,
	}
	payload, err := marshalJSON(cmd)
	if err != nil {
		return err
	}
	return c.SendCommand(string(ws.CmdStepOver), payload)
}

func marshalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func (c *Client) setState(state ws.State) {
	c.state.Store(state)
}

func (c *Client) State() ws.State {
	return c.state.Load().(ws.State)
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// func main() {
// 	serverAddr := flag.String("server", "localhost:8080", "Server address")
// 	sessionID := flag.String("session", "test-session", "Session ID")
// 	flag.Parse()

// 	client := NewClient(*serverAddr, *sessionID)

// 	// Connect to the server
// 	if err := client.Connect(); err != nil {
// 		log.Fatalf("Failed to connect: %v", err)
// 	}
// 	if err := client.Run(); err != nil {
// 		log.Fatalf("Failed to start client: %v", err)
// 	}
// 	defer func() {
// 		if err := client.Close(); err != nil {
// 			log.Printf("Client close error: %v", err)
// 		}
// 	}()

// 	// Set up interrupt handler
// 	interrupt := make(chan os.Signal, 1)
// 	signal.Notify(interrupt, os.Interrupt)

// 	// Wait for interrupt or server close
// 	select {
// 	case <-interrupt:
// 		log.Println("Interrupt signal received")
// 	case <-client.done:
// 		log.Println("Server closed connection")
// 	}
// }
