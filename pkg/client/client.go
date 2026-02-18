package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
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
	closeOnce sync.Once
}

var ErrDisconnected = errors.New("server disconnected")

func NewClient(serverURL, sessionID string) *Client {
	c := &Client{
		serverURL: serverURL,
		sessionID: sessionID,
		send:      make(chan ws.Message, 256),
		done:      make(chan struct{}),
		state:     ws.StateReady,
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
		c.closeConn()
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
	defer c.closeConn()

	for message := range c.send {
		if err := c.conn.WriteJSON(message); err != nil {
			log.Printf("Write error: %v", err)
			return
		}
	}
	if err := c.conn.WriteMessage(websocket.CloseMessage, []byte{}); err != nil {
		if !isConnectionClosedError(err) {
			log.Printf("Failed to send close message: %v", err)
		}
	}
	log.Println("Write pump closing gracefully")
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
		oldState := c.State()
		if oldState != update.NewState {
			c.setState(update.NewState)
			log.Printf("[State Change] %s -> %s", oldState, update.NewState)
		}

	case ws.EventBreakpointHit:
		var hit ws.BreakpointHitEvent
		if err := unmarshalData(msg.Data, &hit); err != nil {
			log.Printf("Error parsing breakpointHit: %v", err)
			return
		}
		oldState := c.State()
		c.setState(ws.StateBreakpoint)
		log.Printf("[State Change] %s -> breakpoint", oldState)
		log.Printf("Breakpoint hit at %s:%d in %s", hit.Filename, hit.Line, hit.Function)

	case ws.EventInitialBreakpoint:
		var initial ws.InitialBreakpointEvent
		if err := unmarshalData(msg.Data, &initial); err != nil {
			log.Printf("Error parsing initialBreakpoint: %v", err)
			return
		}
		oldState := c.State()
		c.setState(ws.StateBreakpoint)
		log.Printf("[State Change] %s -> breakpoint", oldState)
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
	currentState := c.State()
	if currentState != ws.StateBreakpoint {
		return fmt.Errorf("cannot send command '%s' in state '%s' (must be in 'breakpoint' state)", cmdType, currentState)
	}
	msg := ws.Message{
		Type: cmdType,
		Data: payload,
	}

	select {
	case c.send <- msg:
		log.Printf("[Command] Sent %s command (state: %s)", cmdType, c.State())
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
		log.Printf("[Command] Sent %s command (state: %s)", msg.Type, c.State())
		return nil
	case <-c.done:
		return fmt.Errorf("connection closed")
	}
}

func (c *Client) Stop() error {
	cmd := ws.ExitCmd{
		Type:      ws.CmdExit,
		SessionID: c.SessionID(),
	}
	payload, err := marshalJSON(cmd)
	if err != nil {
		return err
	}
	msg := ws.Message{
		Type: string(ws.CmdExit),
		Data: payload,
	}

	select {
	case c.send <- msg:
		log.Printf("[Command] Sent %s command (state: %s)", msg.Type, c.State())
		// State will be updated to 'ready' when server confirms debug session has stopped
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

func (c *Client) Done() <-chan struct{} {
	return c.done
}

func (c *Client) Wait() error {
	<-c.done
	return ErrDisconnected
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		if c.conn != nil {
			err = c.conn.Close()
		}
	})
	return err
}

func (c *Client) closeConn() {
	c.closeOnce.Do(func() {
		if c.conn != nil {
			if err := c.conn.Close(); err != nil {
				if !isConnectionClosedError(err) {
					log.Printf("Close error: %v", err)
				}
			}
		}
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
