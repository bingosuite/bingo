package client

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"

	"github.com/gorilla/websocket"
)

// Compile-time check that wsClient implements Client.
var _ Client = (*wsClient)(nil)

const (
	// syncTimeout is how long synchronous methods (SetBreakpoint, Locals, …)
	// wait for the server's confirmation event before giving up.
	syncTimeout = 10 * time.Second

	// dialTimeout is how long Create/Join wait for the initial SessionState
	// event from the server.
	dialTimeout = 5 * time.Second

	// eventBufferSize is the capacity of the public Events channel.
	// If the caller falls behind, events are dropped with a warning.
	eventBufferSize = 64
)

// ── pendingReq ───────────────────────────────────────────────────────────────

// pendingReq represents a synchronous method blocked waiting for a specific
// confirmation event (or an error event for the same command kind).
type pendingReq struct {
	wantKind protocol.EventKind   // expected success event kind
	cmdKind  protocol.CommandKind // command sent (to match EventError)
	ch       chan protocol.Event  // result (buffered capacity 1)
}

// ── wsClient ─────────────────────────────────────────────────────────────────

// wsClient is the gorilla/websocket-backed Client implementation.
type wsClient struct {
	conn *websocket.Conn
	log  *slog.Logger

	// Session metadata, updated by the read pump from SessionState events.
	metaMu    sync.RWMutex
	sessionID string
	state     protocol.SessionState

	// events is the public channel for async events. Closed by the read
	// pump when the connection drops.
	events chan protocol.Event

	// syncMu serialises synchronous command calls so at most one
	// send-and-wait cycle is active at any time.
	syncMu sync.Mutex

	// pending is the current sync request. Protected by pendingMu.
	// The read pump checks this on every incoming event and routes
	// matching confirmations / errors to pending.ch instead of events.
	pendingMu sync.Mutex
	pending   *pendingReq

	// writeMu protects WebSocket writes. gorilla is safe for one
	// concurrent reader and one concurrent writer — we guarantee the
	// latter with this mutex.
	writeMu sync.Mutex

	// done is closed when the client is shutting down (either Close was
	// called or the connection dropped). Unblocks any pending syncMu wait.
	done      chan struct{}
	closeOnce sync.Once
}

// dial establishes a WebSocket connection and waits for the initial
// SessionState welcome event from the server.
func dial(addr, query string) (Client, error) {
	url := fmt.Sprintf("ws://%s/ws?%s", addr, query)

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}

	c := &wsClient{
		conn:   conn,
		log:    slog.Default(),
		events: make(chan protocol.Event, eventBufferSize),
		done:   make(chan struct{}),
	}

	go c.readPump()

	// Block until the server sends the initial SessionState event so we
	// know the session ID and current state before returning to the caller.
	select {
	case evt, ok := <-c.events:
		if !ok {
			return nil, fmt.Errorf("connection closed before receiving session state")
		}
		if evt.Kind != protocol.EventSessionState {
			return nil, fmt.Errorf("expected SessionState event, got %s", evt.Kind)
		}
		// State and session ID are already populated by the read pump.
	case <-time.After(dialTimeout):
		conn.Close()
		return nil, fmt.Errorf("timeout waiting for session state from server")
	}

	return c, nil
}

// ── Read pump ────────────────────────────────────────────────────────────────

// readPump runs in its own goroutine, reading events from the WebSocket and
// routing them to either the pending sync request or the public Events channel.
func (c *wsClient) readPump() {
	defer func() {
		c.signalDone()
		close(c.events)
	}()

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			// Connection closed (normal or error) — exit silently.
			return
		}

		evt, err := protocol.UnmarshalEvent(data)
		if err != nil {
			c.log.Warn("invalid event from server", "err", err)
			continue
		}

		// Always update internal session metadata from state events.
		if evt.Kind == protocol.EventSessionState {
			var p protocol.SessionStatePayload
			if protocol.DecodeEventPayload(evt, &p) == nil {
				c.metaMu.Lock()
				c.sessionID = p.SessionID
				c.state = p.State
				c.metaMu.Unlock()
			}
		}

		// If a sync method is waiting, check whether this event matches.
		if c.routeToPending(evt) {
			continue
		}

		// Forward to the public events channel.
		select {
		case c.events <- evt:
		default:
			c.log.Warn("events buffer full — dropping", "kind", evt.Kind)
		}
	}
}

// routeToPending checks if evt matches the current pending sync request.
// Returns true if the event was consumed (sent to the pending channel).
func (c *wsClient) routeToPending(evt protocol.Event) bool {
	c.pendingMu.Lock()
	p := c.pending
	c.pendingMu.Unlock()

	if p == nil {
		return false
	}

	// Direct match: this is the confirmation event we're waiting for.
	if evt.Kind == p.wantKind {
		p.ch <- evt
		return true
	}

	// Error match: the server returned an error for the command we sent.
	if evt.Kind == protocol.EventError {
		var ep protocol.ErrorPayload
		if protocol.DecodeEventPayload(evt, &ep) == nil && ep.Command == p.cmdKind {
			p.ch <- evt
			return true
		}
	}

	return false
}

// ── Write helpers ────────────────────────────────────────────────────────────

// send writes a command to the WebSocket. Thread-safe.
func (c *wsClient) send(cmd protocol.Command) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// sendAndWait sends cmd and blocks until the server sends the expected
// confirmation event or an error event for the same command kind.
// Only one sendAndWait may be active at a time (guarded by syncMu).
func (c *wsClient) sendAndWait(cmd protocol.Command, wantKind protocol.EventKind) (protocol.Event, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	ch := make(chan protocol.Event, 1)
	req := &pendingReq{wantKind: wantKind, cmdKind: cmd.Kind, ch: ch}

	c.pendingMu.Lock()
	c.pending = req
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		c.pending = nil
		c.pendingMu.Unlock()
	}()

	if err := c.send(cmd); err != nil {
		return protocol.Event{}, err
	}

	select {
	case evt := <-ch:
		if evt.Kind == protocol.EventError {
			var ep protocol.ErrorPayload
			_ = protocol.DecodeEventPayload(evt, &ep)
			return protocol.Event{}, fmt.Errorf("server: %s", ep.Message)
		}
		return evt, nil
	case <-time.After(syncTimeout):
		return protocol.Event{}, fmt.Errorf("timeout waiting for %s response", wantKind)
	case <-c.done:
		return protocol.Event{}, fmt.Errorf("client closed")
	}
}

// newCommand creates a versioned Command envelope with a marshalled payload.
func newCommand(kind protocol.CommandKind, payload any) (protocol.Command, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("marshal %s payload: %w", kind, err)
	}
	return protocol.Command{
		Version: protocol.Version,
		Kind:    kind,
		Payload: raw,
	}, nil
}

// ── Interface implementation: accessors ──────────────────────────────────────

func (c *wsClient) SessionID() string {
	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	return c.sessionID
}

func (c *wsClient) State() protocol.SessionState {
	c.metaMu.RLock()
	defer c.metaMu.RUnlock()
	return c.state
}

func (c *wsClient) Events() <-chan protocol.Event { return c.events }

// ── Interface implementation: fire-and-forget commands ───────────────────────

func (c *wsClient) Launch(program string, args, env []string) error {
	cmd, err := newCommand(protocol.CmdLaunch, protocol.LaunchPayload{
		Program: program, Args: args, Env: env,
	})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

func (c *wsClient) Attach(pid int, binaryPath string) error {
	cmd, err := newCommand(protocol.CmdAttach, protocol.AttachPayload{
		PID: pid, BinaryPath: binaryPath,
	})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

func (c *wsClient) Kill() error {
	cmd, err := newCommand(protocol.CmdKill, struct{}{})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

func (c *wsClient) Continue() error {
	cmd, err := newCommand(protocol.CmdContinue, struct{}{})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

func (c *wsClient) StepOver() error {
	cmd, err := newCommand(protocol.CmdStepOver, struct{}{})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

func (c *wsClient) StepInto() error {
	cmd, err := newCommand(protocol.CmdStepInto, struct{}{})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

func (c *wsClient) StepOut() error {
	cmd, err := newCommand(protocol.CmdStepOut, struct{}{})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

// ── Interface implementation: synchronous commands ───────────────────────────

func (c *wsClient) SetBreakpoint(file string, line int) (protocol.Breakpoint, error) {
	cmd, err := newCommand(protocol.CmdSetBreakpoint, protocol.SetBreakpointPayload{
		File: file, Line: line,
	})
	if err != nil {
		return protocol.Breakpoint{}, err
	}
	evt, err := c.sendAndWait(cmd, protocol.EventBreakpointSet)
	if err != nil {
		return protocol.Breakpoint{}, err
	}
	var p protocol.BreakpointSetPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return protocol.Breakpoint{}, fmt.Errorf("decode BreakpointSet: %w", err)
	}
	return p.Breakpoint, nil
}

func (c *wsClient) ClearBreakpoint(id int) error {
	cmd, err := newCommand(protocol.CmdClearBreakpoint, protocol.ClearBreakpointPayload{ID: id})
	if err != nil {
		return err
	}
	_, err = c.sendAndWait(cmd, protocol.EventBreakpointCleared)
	return err
}

func (c *wsClient) Locals(frameIndex int) ([]protocol.Variable, error) {
	cmd, err := newCommand(protocol.CmdLocals, protocol.LocalsPayloadCmd{FrameIndex: frameIndex})
	if err != nil {
		return nil, err
	}
	evt, err := c.sendAndWait(cmd, protocol.EventLocals)
	if err != nil {
		return nil, err
	}
	var p protocol.LocalsPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return nil, fmt.Errorf("decode Locals: %w", err)
	}
	return p.Variables, nil
}

func (c *wsClient) StackFrames() ([]protocol.Frame, error) {
	cmd, err := newCommand(protocol.CmdFrames, struct{}{})
	if err != nil {
		return nil, err
	}
	evt, err := c.sendAndWait(cmd, protocol.EventFrames)
	if err != nil {
		return nil, err
	}
	var p protocol.FramesPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return nil, fmt.Errorf("decode Frames: %w", err)
	}
	return p.Frames, nil
}

func (c *wsClient) Goroutines() ([]protocol.Goroutine, error) {
	cmd, err := newCommand(protocol.CmdGoroutines, struct{}{})
	if err != nil {
		return nil, err
	}
	evt, err := c.sendAndWait(cmd, protocol.EventGoroutines)
	if err != nil {
		return nil, err
	}
	var p protocol.GoroutinesPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return nil, fmt.Errorf("decode Goroutines: %w", err)
	}
	return p.Goroutines, nil
}

// ── Interface implementation: lifecycle ──────────────────────────────────────

// Close disconnects from the server. Safe to call multiple times.
func (c *wsClient) Close() error {
	c.signalDone()
	return c.conn.Close()
}

// signalDone closes the done channel exactly once, unblocking any pending
// synchronous wait. Called by Close and by the read pump on connection drop.
func (c *wsClient) signalDone() {
	c.closeOnce.Do(func() { close(c.done) })
}
