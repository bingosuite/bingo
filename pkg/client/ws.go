package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"

	"github.com/gorilla/websocket"
)

var _ Client = (*wsClient)(nil)

const (
	dialTimeout     = 5 * time.Second
	eventBufferSize = 64
)

var syncTimeout = 10 * time.Second

// pendingReq keeps its place after a timeout because the server command is not
// cancelled. A nil ch marks reply debt whose eventual response must be consumed.
type pendingReq struct {
	wantKind protocol.EventKind
	cmdKind  protocol.CommandKind
	ch       chan protocol.Event
}

type wsClient struct {
	conn *websocket.Conn
	log  *slog.Logger

	metaMu    sync.RWMutex
	sessionID string
	state     protocol.SessionState

	events chan protocol.Event

	// syncMu serialises sendAndWait so one in-flight pending request at a time.
	syncMu sync.Mutex

	// pending preserves request order across timeouts so a late reply cannot be
	// mistaken for a newer same-kind request.
	pendingMu sync.Mutex
	pending   []*pendingReq

	// writeMu: gorilla allows one concurrent reader and one concurrent writer.
	writeMu sync.Mutex

	readErrMu sync.RWMutex
	readErr   error

	done      chan struct{}
	closeOnce sync.Once
}

// dial opens the WebSocket and waits for the server's welcome SessionState.
func dial(ctx context.Context, addr, query string) (Client, error) {
	url := fmt.Sprintf("ws://%s/ws?%s", addr, query)

	dialer := *websocket.DefaultDialer
	var stopDialCancel func() bool
	dialer.NetDialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		rawConn, err := (&net.Dialer{}).DialContext(dialCtx, network, address)
		if err != nil {
			return nil, err
		}
		// gorilla applies context deadlines to the upgrade but does not close an
		// established socket when a deadline-free context is canceled.
		stopDialCancel = context.AfterFunc(ctx, func() { _ = rawConn.Close() })
		return rawConn, nil
	}
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if stopDialCancel != nil {
		stopDialCancel()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, fmt.Errorf("dial %s: %w", url, ctxErr)
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", url, err)
	}

	c := &wsClient{
		conn:   conn,
		log:    slog.Default(),
		events: make(chan protocol.Event, eventBufferSize),
		done:   make(chan struct{}),
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = c.Close()
		}
	}()

	go c.readPump()

	timer := time.NewTimer(dialTimeout)
	defer timer.Stop()
	select {
	case evt, ok := <-c.events:
		if !ok {
			if err := c.terminalReadError(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("connection closed before receiving session state")
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("wait for session state: %w", err)
		}
		if evt.Kind != protocol.EventSessionState {
			return nil, fmt.Errorf("expected SessionState event, got %s", evt.Kind)
		}
	case <-timer.C:
		return nil, fmt.Errorf("timeout waiting for session state from server")
	case <-ctx.Done():
		return nil, fmt.Errorf("wait for session state: %w", ctx.Err())
	}

	cleanup = false
	return c, nil
}

func (c *wsClient) readPump() {
	defer func() {
		c.signalDone()
		close(c.events)
	}()

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}

		evt, err := protocol.UnmarshalEvent(data)
		if err != nil {
			c.log.Warn("invalid event from server", "err", err)
			continue
		}
		if err := protocol.ValidateVersion(evt.Version); err != nil {
			c.terminateRead(fmt.Errorf("server event: %w", err))
			return
		}

		if evt.Kind == protocol.EventSessionState {
			var p protocol.SessionStatePayload
			if protocol.DecodeEventPayload(evt, &p) == nil {
				c.metaMu.Lock()
				c.sessionID = p.SessionID
				c.state = p.State
				c.metaMu.Unlock()
			}
		}

		if c.routeToPending(evt) {
			continue
		}

		select {
		case c.events <- evt:
		default:
			c.log.Warn("events buffer full — dropping", "kind", evt.Kind)
		}
	}
}

func (c *wsClient) routeToPending(evt protocol.Event) bool {
	var errorCommand protocol.CommandKind
	if evt.Kind == protocol.EventError {
		var ep protocol.ErrorPayload
		if protocol.DecodeEventPayload(evt, &ep) != nil {
			return false
		}
		errorCommand = ep.Command
	}

	c.pendingMu.Lock()
	for i, p := range c.pending {
		matches := evt.Kind == p.wantKind
		if evt.Kind == protocol.EventError {
			matches = errorCommand == p.cmdKind
		}
		if !matches {
			continue
		}

		ch := p.ch
		c.removePendingAt(i)
		c.pendingMu.Unlock()

		if ch != nil {
			ch <- evt
		}
		return true
	}
	c.pendingMu.Unlock()

	return false
}

func (c *wsClient) removePendingAt(i int) {
	copy(c.pending[i:], c.pending[i+1:])
	c.pending[len(c.pending)-1] = nil
	c.pending = c.pending[:len(c.pending)-1]
}

func (c *wsClient) discardPending(req *pendingReq) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	for i, p := range c.pending {
		if p == req {
			c.removePendingAt(i)
			return
		}
	}
}

func (c *wsClient) retirePending(req *pendingReq) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	for _, p := range c.pending {
		if p == req {
			p.ch = nil
			return
		}
	}
}

func (c *wsClient) send(cmd protocol.Command) error {
	if err := c.operationError(nil); err != nil {
		return err
	}
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	c.writeMu.Lock()
	if err := c.operationError(nil); err != nil {
		c.writeMu.Unlock()
		return err
	}
	writeErr := c.conn.WriteMessage(websocket.TextMessage, data)
	c.writeMu.Unlock()
	return c.normalizeSendError(writeErr)
}

func (c *wsClient) normalizeSendError(err error) error {
	return c.operationError(err)
}

func (c *wsClient) operationError(fallback error) error {
	c.readErrMu.RLock()
	defer c.readErrMu.RUnlock()
	if c.readErr != nil {
		return c.readErr
	}
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	if errors.Is(fallback, websocket.ErrCloseSent) {
		return ErrClosed
	}
	return fallback
}

// sendAndWait sends cmd and blocks for the matching confirmation event or an
// EventError for the same command kind.
func (c *wsClient) sendAndWait(cmd protocol.Command, wantKind protocol.EventKind) (protocol.Event, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	ch := make(chan protocol.Event, 1)
	req := &pendingReq{wantKind: wantKind, cmdKind: cmd.Kind, ch: ch}

	c.pendingMu.Lock()
	c.pending = append(c.pending, req)
	c.pendingMu.Unlock()

	if err := c.send(cmd); err != nil {
		c.discardPending(req)
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
		c.retirePending(req)
		return protocol.Event{}, fmt.Errorf("timeout waiting for %s response", wantKind)
	case <-c.done:
		c.discardPending(req)
		return protocol.Event{}, c.operationError(nil)
	}
}

func (c *wsClient) terminateRead(err error) {
	c.readErrMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.readErrMu.Unlock()
	_ = c.conn.Close()
}

func (c *wsClient) terminalReadError() error {
	c.readErrMu.RLock()
	defer c.readErrMu.RUnlock()
	return c.readErr
}

func newCommand(kind protocol.CommandKind, payload any) (protocol.Command, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return protocol.Command{}, fmt.Errorf("marshal %s payload: %w", kind, err)
	}
	return protocol.Command{Version: protocol.Version, Kind: kind, Payload: raw}, nil
}

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

func (c *wsClient) Restart(args, env []string) (protocol.RestartedPayload, error) {
	cmd, err := newCommand(protocol.CmdRestart, protocol.RestartPayload{Args: args, Env: env})
	if err != nil {
		return protocol.RestartedPayload{}, err
	}
	evt, err := c.sendAndWait(cmd, protocol.EventRestarted)
	if err != nil {
		return protocol.RestartedPayload{}, err
	}
	var p protocol.RestartedPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return protocol.RestartedPayload{}, fmt.Errorf("decode Restarted: %w", err)
	}
	return p, nil
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

// Pause is fire-and-forget like Continue: it sends CmdPause and returns. The
// resulting halt arrives asynchronously as EventPaused on Events().
func (c *wsClient) Pause() error {
	cmd, err := newCommand(protocol.CmdPause, struct{}{})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

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

func (c *wsClient) Evaluate(frameIndex int, name string) (protocol.Variable, error) {
	cmd, err := newCommand(protocol.CmdEvaluate, protocol.EvaluatePayloadCmd{
		FrameIndex: frameIndex,
		Name:       name,
	})
	if err != nil {
		return protocol.Variable{}, err
	}
	evt, err := c.sendAndWait(cmd, protocol.EventEvaluate)
	if err != nil {
		return protocol.Variable{}, err
	}
	var p protocol.EvaluatePayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return protocol.Variable{}, fmt.Errorf("decode Evaluate: %w", err)
	}
	return p.Result, nil
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
	p, err := c.GoroutineList()
	if err != nil {
		return nil, err
	}
	return p.Goroutines, nil
}

func (c *wsClient) GoroutineList() (protocol.GoroutinesPayload, error) {
	cmd, err := newCommand(protocol.CmdGoroutines, struct{}{})
	if err != nil {
		return protocol.GoroutinesPayload{}, err
	}
	evt, err := c.sendAndWait(cmd, protocol.EventGoroutines)
	if err != nil {
		return protocol.GoroutinesPayload{}, err
	}
	var p protocol.GoroutinesPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return protocol.GoroutinesPayload{}, fmt.Errorf("decode Goroutines: %w", err)
	}
	return p, nil
}

// RequestGoroutineSnapshot sends CmdGoroutineSnapshot and returns once it is on
// the wire. It deliberately does not use sendAndWait: EventGoroutineSnapshot is
// also pushed unsolicited on every entry/breakpoint/pause stop, so a kind-keyed
// pending entry would let an automatic push satisfy this call (and, after a
// timeout, let this call's reply debt swallow an automatic push). The snapshot
// arrives on Events() like every other one.
func (c *wsClient) RequestGoroutineSnapshot() error {
	cmd, err := newCommand(protocol.CmdGoroutineSnapshot, struct{}{})
	if err != nil {
		return err
	}
	return c.send(cmd)
}

// Close disconnects from the server. Safe to call multiple times.
func (c *wsClient) Close() error {
	c.signalDone()
	return c.conn.Close()
}

func (c *wsClient) signalDone() {
	c.closeOnce.Do(func() { close(c.done) })
}
