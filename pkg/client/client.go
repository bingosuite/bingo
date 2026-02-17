package client

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"sync"

	"github.com/bingosuite/bingo/internal/ws"
	"github.com/gorilla/websocket"
)

type Client struct {
	serverURL string
	sessionID string
	conn      *websocket.Conn
	send      chan ws.Message
	done      chan struct{}
	sessionMu sync.RWMutex
	stateMu   sync.RWMutex
	state     ws.State
}

func NewClient(serverURL, sessionID string) *Client {
	c := &Client{
		serverURL: serverURL,
		sessionID: sessionID,
		send:      make(chan ws.Message, 256),
		done:      make(chan struct{}),
		state:     ws.StateExecuting,
	}
	return c
}

func (c *Client) Connect() error {
	// Build WebSocket URL with session ID
	sessionID := c.SessionID()
	u := url.URL{
		Scheme:   "ws",
		Host:     c.serverURL,
		Path:     "/ws/",
		RawQuery: "session=" + sessionID,
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
	c.setState(ws.StateExecuting)
}

func (c *Client) handleMessage(msg ws.Message) {
	log.Printf("Received message type: %s", msg.Type)

	switch ws.EventType(msg.Type) {
	case ws.EventSessionStarted:
		var started ws.SessionStartedEvent
		if err := unmarshalData(msg.Data, &started); err != nil {
			log.Printf("Error parsing sessionStarted: %v", err)
			return
		}
		if started.SessionID != "" {
			c.setSessionID(started.SessionID)
			log.Printf("Session ID set: %s", started.SessionID)
			return
		}
		log.Println("Debug session started")

	case ws.EventStateUpdate:
		var update ws.StateUpdateEvent
		if err := unmarshalData(msg.Data, &update); err != nil {
			log.Printf("Error parsing stateUpdate: %v", err)
			return
		}
		c.setState(update.NewState)
		log.Printf("State updated: %s", update.NewState)

	case ws.EventBreakpointHit:
		var hit ws.BreakpointHitEvent
		if err := unmarshalData(msg.Data, &hit); err != nil {
			log.Printf("Error parsing breakpointHit: %v", err)
			return
		}
		c.setState(ws.StateBreakpoint)
		log.Printf("Breakpoint hit at %s:%d in %s", hit.Filename, hit.Line, hit.Function)

	case ws.EventInitialBreakpoint:
		var initial ws.InitialBreakpointEvent
		if err := unmarshalData(msg.Data, &initial); err != nil {
			log.Printf("Error parsing initialBreakpoint: %v", err)
			return
		}
		c.setState(ws.StateBreakpoint)
		log.Printf("Initial breakpoint hit (pid=%d)", initial.PID)

	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

func unmarshalData(data []byte, v any) error {
	// Handle empty data
	if len(data) == 0 {
		return nil
	}
	return unmarshalJSON(data, v)
}

func unmarshalJSON(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func (c *Client) SendCommand(cmdType string, payload []byte) error {
	// TODO: decide which states allow which commands
	if c.State() != ws.StateBreakpoint {
		return fmt.Errorf("cannot send command in state: %s", c.State())
	}
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
		SessionID: c.SessionID(),
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
		SessionID: c.SessionID(),
	}
	payload, err := marshalJSON(cmd)
	if err != nil {
		return err
	}
	return c.SendCommand(string(ws.CmdStepOver), payload)
}

func (c *Client) StartDebug(targetPath string) error {
	cmd := ws.StartDebugCmd{
		Type:       ws.CmdStartDebug,
		SessionID:  c.SessionID(),
		TargetPath: targetPath,
	}
	payload, err := marshalJSON(cmd)
	if err != nil {
		return err
	}
	msg := ws.Message{
		Type: string(ws.CmdStartDebug),
		Data: payload,
	}

	select {
	case c.send <- msg:
		log.Printf("Queued command: %s", msg.Type)
		return nil
	case <-c.done:
		return fmt.Errorf("connection closed")
	}
}

func (c *Client) SetBreakpoint(filename string, line int) error {
	cmd := ws.SetBreakpointCmd{
		Type:      ws.CmdSetBreakpoint,
		SessionID: c.SessionID(),
		Filename:  filename,
		Line:      line,
	}
	payload, err := marshalJSON(cmd)
	if err != nil {
		return err
	}
	return c.SendCommand(string(ws.CmdSetBreakpoint), payload)
}

func marshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (c *Client) setState(state ws.State) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.state = state
}

func (c *Client) setSessionID(sessionID string) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.sessionID = sessionID
}

func (c *Client) SessionID() string {
	c.sessionMu.RLock()
	defer c.sessionMu.RUnlock()
	return c.sessionID
}

func (c *Client) State() ws.State {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
