// Package hub coordinates a single debug session: it bridges WebSocket clients
// with a Debugger instance. See AGENTS.md for the suspend/resume protocol and
// session lifecycle.
package hub

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// suspendingEvents pause the hub and require a resuming command before the
// process is allowed to continue.
var suspendingEvents = map[protocol.EventKind]bool{
	protocol.EventBreakpointHit: true,
	protocol.EventPanic:         true,
	protocol.EventStepped:       true,
}

// resumingCommands unblock a suspended hub.
var resumingCommands = map[protocol.CommandKind]bool{
	protocol.CmdContinue: true,
	protocol.CmdStepOver: true,
	protocol.CmdStepInto: true,
	protocol.CmdStepOut:  true,
	protocol.CmdKill:     true,
}

// Hub owns one debug session. It bridges the Debugger with all connected
// clients, fanning events out and serialising commands in.
type Hub struct {
	// sessionID is empty for raw hubs created via New() (tests / single-session).
	sessionID string

	// newDebugger creates a debugger on Launch/Attach. nil for raw hubs.
	newDebugger func() debugger.Debugger

	// dbg is the active debugger. nil while idle (no process launched).
	dbg      debugger.Debugger
	registry *registry
	log      *slog.Logger

	// state guarded by stateMu — read from AddClient (HTTP goroutine), written
	// from the Run loop.
	stateMu sync.RWMutex
	state   protocol.SessionState

	// cmdCh: non-resuming commands from client read-pumps to the main loop.
	cmdCh chan clientCommand

	// resumeCh: capacity 1, first-write-wins. Extras dropped in injectCommand.
	resumeCh chan protocol.Command

	// seq is the single counter for ALL outbound events. The hub re-stamps
	// debugger events with this counter, so clients see one monotonic stream
	// and can detect gaps. The engine has its own seq.
	seq atomic.Uint64

	// shutdownOnce: Kill and registry teardown must happen exactly once,
	// even when ctx.Done() and last-client-disconnect race.
	shutdownOnce sync.Once

	shutdownCh chan struct{}
	done       chan struct{}
}

type clientCommand struct {
	cmd    protocol.Command
	client *Client
}

func newHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		registry:   newRegistry(),
		cmdCh:      make(chan clientCommand, 32),
		resumeCh:   make(chan protocol.Command, 1),
		shutdownCh: make(chan struct{}),
		done:       make(chan struct{}),
		log:        log,
	}
}

// New creates a Hub wired to dbg. The debugger is already attached — no
// Launch/Attach needed. State events are not broadcast. Tests / single-session.
func New(dbg debugger.Debugger, log *slog.Logger) *Hub {
	h := newHub(log)
	h.dbg = dbg
	h.state = protocol.StateRunning
	return h
}

// NewSession creates a Hub for a server-managed session. Starts idle; the
// debugger is created on Launch/Attach via newDebugger.
func NewSession(sessionID string, newDebugger func() debugger.Debugger, log *slog.Logger) *Hub {
	h := newHub(log)
	h.sessionID = sessionID
	h.newDebugger = newDebugger
	h.state = protocol.StateIdle
	return h
}

func (h *Hub) SessionID() string { return h.sessionID }

func (h *Hub) State() protocol.SessionState {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	return h.state
}

func (h *Hub) ClientCount() int { return h.registry.count() }

// Done is closed when Run returns.
func (h *Hub) Done() <-chan struct{} { return h.done }

// Run blocks until ctx is cancelled, shutdown() is called (last client left),
// or — for raw hubs — the debugger's Events channel closes. Call exactly once.
func (h *Hub) Run(ctx context.Context) {
	defer func() {
		h.shutdown()
		close(h.done)
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case <-h.shutdownCh:
			return

		case evt, ok := <-h.eventsCh():
			if !ok {
				if h.newDebugger != nil {
					// Managed session: clean up and go idle for re-launch.
					h.handleDebuggerClosed()
					continue
				}
				return
			}
			h.handleEvent(ctx, evt)

		case cc := <-h.cmdCh:
			h.executeCommand(cc.cmd)
		}
	}
}

// eventsCh returns the current debugger's events channel, or nil when idle.
// A nil channel blocks forever in select — correct behaviour while waiting
// for Launch/Attach.
func (h *Hub) eventsCh() <-chan protocol.Event {
	if h.dbg == nil {
		return nil
	}
	return h.dbg.Events()
}

// AddClient registers conn as a new client. Safe from any goroutine.
func (h *Hub) AddClient(conn WSConn, log *slog.Logger) *Client {
	c := newClient(conn, h, log)
	h.registry.add(c)
	go c.writePump()
	go c.readPump()
	h.log.Info("client connected", "total", h.registry.count())

	if h.sessionID != "" {
		h.sendStateTo(c)
	}

	return c
}

func (h *Hub) removeClient(c *Client) {
	h.registry.remove(c)
	remaining := h.registry.count()
	h.log.Info("client disconnected", "remaining", remaining)
	if remaining == 0 {
		h.log.Info("last client disconnected — shutting down")
		// Separate goroutine: readPump must not block on dbg.Kill().
		go h.shutdown()
	}
}

// handleEvent re-stamps evt with the hub's seq, broadcasts it, and — for
// suspending events — blocks until a resuming command arrives or the session
// ends. Re-stamping is needed because the engine has its own seq and the hub
// also synthesises errors/confirmations.
func (h *Hub) handleEvent(ctx context.Context, evt protocol.Event) {
	evt.Seq = h.seq.Add(1)
	h.broadcast(evt)

	switch evt.Kind {
	case protocol.EventBreakpointHit, protocol.EventPanic, protocol.EventStepped:
		h.transitionState(protocol.StateSuspended)
	case protocol.EventProcessExited:
		h.transitionState(protocol.StateExited)
	}

	if !suspendingEvents[evt.Kind] {
		return
	}

	h.log.Info("suspended — waiting for resuming command", "event", evt.Kind)

	timeout := time.NewTimer(30 * time.Minute)
	defer timeout.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-h.shutdownCh:
			return

		case nextEvt, ok := <-h.eventsCh():
			// Debugger event while suspended. The important case is
			// ProcessExited: if the process exits while paused (Kill called
			// externally), broadcast it and stop — nobody will send resume.
			// Other events shouldn't normally arrive here but we forward them
			// defensively.
			if !ok {
				if h.newDebugger != nil {
					h.handleDebuggerClosed()
				}
				return
			}
			nextEvt.Seq = h.seq.Add(1)
			h.broadcast(nextEvt)
			if nextEvt.Kind == protocol.EventProcessExited {
				h.transitionState(protocol.StateExited)
				return
			}

		case cmd := <-h.resumeCh:
			h.log.Info("resuming", "command", cmd.Kind)
			h.executeCommand(cmd)
			return

		case cc := <-h.cmdCh:
			// Non-resuming command (SetBreakpoint, Locals, …) while suspended.
			// Execute immediately — process is paused — and keep waiting.
			h.executeCommand(cc.cmd)

		case <-timeout.C:
			h.log.Warn("30-minute suspend timeout — auto-continuing")
			if h.dbg != nil {
				if err := h.dbg.Continue(); err != nil {
					h.log.Warn("auto-continue failed", "err", err)
				}
			}
			return
		}
	}
}

// handleDebuggerClosed: transition through exited (if not already) to idle,
// ready for a new Launch/Attach cycle.
func (h *Hub) handleDebuggerClosed() {
	if h.State() != protocol.StateExited {
		h.transitionState(protocol.StateExited)
	}
	h.dbg = nil
	h.transitionState(protocol.StateIdle)
	h.log.Info("debugger closed — session idle, ready for re-launch")
}

func (h *Hub) executeCommand(cmd protocol.Command) {
	if h.sessionID != "" && (cmd.Kind == protocol.CmdLaunch || cmd.Kind == protocol.CmdAttach) {
		if h.dbg != nil {
			h.broadcastError(cmd.Kind, fmt.Errorf("debugger already active (state: %s)", h.State()))
			return
		}
		if h.newDebugger == nil {
			h.broadcastError(cmd.Kind, fmt.Errorf("no debugger factory configured"))
			return
		}
		h.dbg = h.newDebugger()
	}

	if h.dbg == nil {
		h.broadcastError(cmd.Kind, fmt.Errorf("no active debugger — send Launch or Attach first"))
		return
	}

	result, err := dispatch(h.dbg, cmd)
	if err != nil {
		h.log.Warn("command failed", "kind", cmd.Kind, "err", err)
		h.broadcastError(cmd.Kind, err)
		return
	}

	switch cmd.Kind {
	case protocol.CmdLaunch, protocol.CmdAttach:
		h.transitionState(protocol.StateRunning)
	case protocol.CmdContinue, protocol.CmdStepOver, protocol.CmdStepInto, protocol.CmdStepOut:
		h.transitionState(protocol.StateRunning)
	}

	if result.event != nil {
		result.event.Seq = h.seq.Add(1)
		h.broadcast(*result.event)
	}
}

// injectCommand is called by client read-pumps. Resuming commands go to
// resumeCh to directly unblock a suspended hub; everything else to cmdCh.
func (h *Hub) injectCommand(_ *Client, cmd protocol.Command) {
	if resumingCommands[cmd.Kind] {
		select {
		case h.resumeCh <- cmd:
		default:
			// First writer wins; later resumers are dropped.
		}
		return
	}
	select {
	case h.cmdCh <- clientCommand{cmd: cmd}:
	default:
		h.log.Warn("command queue full — dropping", "kind", cmd.Kind)
	}
}

// transitionState updates state and, for managed sessions, broadcasts.
func (h *Hub) transitionState(newState protocol.SessionState) {
	h.stateMu.Lock()
	old := h.state
	if old == newState {
		h.stateMu.Unlock()
		return
	}
	h.state = newState
	h.stateMu.Unlock()

	h.log.Info("state transition", "from", old, "to", newState)

	if h.sessionID != "" {
		h.broadcastSessionState()
	}
}

func (h *Hub) broadcastSessionState() {
	h.stateMu.RLock()
	state := h.state
	h.stateMu.RUnlock()

	evt, err := protocol.NewEvent(protocol.EventSessionState, h.seq.Add(1), protocol.SessionStatePayload{
		SessionID: h.sessionID,
		State:     state,
		Clients:   h.registry.count(),
	})
	if err != nil {
		h.log.Error("failed to create session state event", "err", err)
		return
	}
	h.broadcast(evt)
}

// sendStateTo delivers the current state to a single client (welcome message).
func (h *Hub) sendStateTo(c *Client) {
	h.stateMu.RLock()
	state := h.state
	h.stateMu.RUnlock()

	evt, err := protocol.NewEvent(protocol.EventSessionState, h.seq.Add(1), protocol.SessionStatePayload{
		SessionID: h.sessionID,
		State:     state,
		Clients:   h.registry.count(),
	})
	if err != nil {
		h.log.Error("failed to create welcome state event", "err", err)
		return
	}
	wire, err := protocol.MarshalEvent(evt)
	if err != nil {
		h.log.Error("failed to marshal welcome state event", "err", err)
		return
	}
	c.deliver(wire)
}

func (h *Hub) broadcast(evt protocol.Event) {
	wire, err := protocol.MarshalEvent(evt)
	if err != nil {
		h.log.Error("marshal event failed", "err", err)
		return
	}
	h.registry.broadcast(wire)
}

func (h *Hub) broadcastError(kind protocol.CommandKind, err error) {
	evt, e := protocol.NewEvent(protocol.EventError, h.seq.Add(1), protocol.ErrorPayload{
		Command: kind,
		Message: err.Error(),
	})
	if e != nil {
		return
	}
	h.broadcast(evt)
}

// shutdown closes all clients and kills the debugger exactly once. Safe to
// call concurrently from ctx.Done and last-client-disconnect.
func (h *Hub) shutdown() {
	h.shutdownOnce.Do(func() {
		h.log.Info("hub shutting down")
		close(h.shutdownCh)
		h.registry.closeAll()
		if h.dbg != nil {
			_ = h.dbg.Kill()
		}
	})
}
