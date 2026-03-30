// Package hub coordinates a single debug session: it bridges connected
// WebSocket clients with a Debugger instance.
//
// Two constructors are provided:
//
//   - New(dbg, log) creates a hub with a pre-attached debugger (backward compat,
//     used by tests).
//   - NewSession(sessionID, factory, log) creates a managed session hub. The
//     debugger is created lazily on Launch/Attach and torn down on process exit,
//     allowing re-launch within the same session.
//
// Lifecycle:
//
//	h := hub.NewSession(id, factory, log)
//	go h.Run(ctx)        // blocks until ctx cancelled or last client leaves
//	h.AddClient(conn)    // called from the HTTP handler on each WS upgrade
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

// suspendingEvents are event kinds that cause the hub to pause and wait for
// a resuming command before the process is allowed to continue running.
var suspendingEvents = map[protocol.EventKind]bool{
	protocol.EventBreakpointHit: true,
	protocol.EventPanic:         true,
	protocol.EventStepped:       true,
}

// resumingCommands are commands that unblock a suspended hub.
var resumingCommands = map[protocol.CommandKind]bool{
	protocol.CmdContinue: true,
	protocol.CmdStepOver: true,
	protocol.CmdStepInto: true,
	protocol.CmdStepOut:  true,
	protocol.CmdKill:     true,
}

// Hub owns one debug session. It bridges the Debugger with all connected
// WebSocket clients, fanning events out and serialising commands in.
type Hub struct {
	// sessionID is the server-assigned UUID for managed sessions.
	// Empty for raw hubs created via New() (backward compat / tests).
	sessionID string

	// newDebugger is the factory for creating debugger instances.
	// Called on Launch/Attach. nil for raw hubs created via New().
	newDebugger func() debugger.Debugger

	// dbg is the current debugger instance. nil when the hub is in idle
	// state (no process launched). For raw hubs, always non-nil.
	dbg      debugger.Debugger
	registry *registry
	log      *slog.Logger

	// state tracks the session lifecycle for broadcasting to clients.
	// Protected by stateMu since it is read from AddClient (HTTP goroutine)
	// and written from the Run loop.
	stateMu sync.RWMutex
	state   protocol.SessionState

	// cmdCh carries non-resuming commands from client read-pumps to the
	// hub's main loop. Buffered so read-pumps don't block.
	cmdCh chan clientCommand

	// resumeCh carries the first resuming command while the hub is suspended.
	// Capacity 1: first-write-wins; extras are dropped in injectCommand.
	resumeCh chan protocol.Command

	// seq is the single sequence counter for ALL events broadcast by the hub.
	// Both debugger events and hub-synthesised events (confirmations, errors)
	// are stamped with this counter before being sent to clients, so every
	// client sees one monotonically increasing stream and can detect gaps.
	seq atomic.Uint64

	// shutdownOnce ensures Kill and registry teardown happen exactly once,
	// even when ctx.Done() and last-client-disconnect race.
	shutdownOnce sync.Once

	// shutdownCh is closed by shutdown() to signal the Run loop to exit.
	shutdownCh chan struct{}

	// done is closed when Run returns.
	done chan struct{}
}

type clientCommand struct {
	cmd    protocol.Command
	client *Client
}

// newHub allocates the common Hub fields shared by both constructors.
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

// New creates a Hub wired to dbg. The hub starts with the debugger already
// attached — no Launch/Attach is needed. State events are not broadcast.
// Intended for tests and single-session setups.
func New(dbg debugger.Debugger, log *slog.Logger) *Hub {
	h := newHub(log)
	h.dbg = dbg
	h.state = protocol.StateRunning
	return h
}

// NewSession creates a Hub for a named server-managed session. The hub starts
// in idle state; when a Launch/Attach command arrives, newDebugger is called to
// create the debugger instance. On process exit the hub transitions back to
// idle and a fresh debugger is created on the next Launch/Attach.
func NewSession(sessionID string, newDebugger func() debugger.Debugger, log *slog.Logger) *Hub {
	h := newHub(log)
	h.sessionID = sessionID
	h.newDebugger = newDebugger
	h.state = protocol.StateIdle
	return h
}

// ── Accessors ─────────────────────────────────────────────────────────────────

// SessionID returns the server-assigned session identifier.
func (h *Hub) SessionID() string { return h.sessionID }

// State returns the current session state. Safe to call from any goroutine.
func (h *Hub) State() protocol.SessionState {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	return h.state
}

// ClientCount returns the number of connected clients.
func (h *Hub) ClientCount() int { return h.registry.count() }

// Done returns a channel closed when Run returns.
func (h *Hub) Done() <-chan struct{} { return h.done }

// ── Run loop ──────────────────────────────────────────────────────────────────

// Run starts the hub's event loop. It blocks until:
//   - ctx is cancelled, or
//   - shutdown() is called (last client disconnects), or
//   - for raw hubs: the debugger's Events channel closes.
//
// Safe to call exactly once.
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
				// Debugger shut down (events channel closed).
				if h.newDebugger != nil {
					// Managed session: clean up and go idle for re-launch.
					h.handleDebuggerClosed()
					continue
				}
				// Raw hub: no factory, so we shut down.
				return
			}
			h.handleEvent(ctx, evt)

		case cc := <-h.cmdCh:
			h.executeCommand(cc.cmd)
		}
	}
}

// eventsCh returns the current debugger's events channel, or nil if no
// debugger is attached. A nil channel blocks forever in select, which is
// the correct behaviour when the hub is idle (waiting for Launch/Attach).
func (h *Hub) eventsCh() <-chan protocol.Event {
	if h.dbg == nil {
		return nil
	}
	return h.dbg.Events()
}

// AddClient registers conn as a new WebSocket client and starts its pumps.
// Safe to call from any goroutine (typically the HTTP upgrade handler).
func (h *Hub) AddClient(conn WSConn, log *slog.Logger) *Client {
	c := newClient(conn, h, log)
	h.registry.add(c)
	go c.writePump()
	go c.readPump()
	h.log.Info("client connected", "total", h.registry.count())

	// For managed sessions, send the current state so the client is
	// immediately synced.
	if h.sessionID != "" {
		h.sendStateTo(c)
	}

	return c
}

// removeClient is called by a client's readPump when the connection closes.
func (h *Hub) removeClient(c *Client) {
	h.registry.remove(c)
	remaining := h.registry.count()
	h.log.Info("client disconnected", "remaining", remaining)
	if remaining == 0 {
		h.log.Info("last client disconnected — shutting down")
		// Run in a separate goroutine: readPump must not block on dbg.Kill().
		go h.shutdown()
	}
}

// ── Event handling ────────────────────────────────────────────────────────────

// handleEvent re-stamps evt with the hub's seq, broadcasts it to all clients,
// and — for suspending events — blocks until a resuming command arrives or the
// session ends.
//
// Re-stamping is necessary because the debugger engine has its own seq counter,
// and the hub synthesises additional events (errors, confirmations). Without
// re-stamping, clients would see two overlapping monotonic sequences.
func (h *Hub) handleEvent(ctx context.Context, evt protocol.Event) {
	evt.Seq = h.seq.Add(1)
	h.broadcast(evt)

	// Track state transitions derived from debugger events.
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
			// A debugger event arrived while we are suspended. The most
			// important case is ProcessExited: if the process exits while
			// paused (e.g. Kill was called externally), we must broadcast it
			// and stop waiting — there is nobody left to send a resume command.
			// For any other event we broadcast it and keep waiting; such events
			// should not normally arrive while the process is stopped, but we
			// handle them defensively.
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
			// Non-resuming command while suspended (SetBreakpoint, Locals, …).
			// Execute immediately — process is paused — then keep waiting.
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

// handleDebuggerClosed cleans up after the debugger's events channel closes.
// The hub transitions through exited (if not already) to idle, ready for a
// new Launch/Attach cycle.
func (h *Hub) handleDebuggerClosed() {
	if h.State() != protocol.StateExited {
		h.transitionState(protocol.StateExited)
	}
	h.dbg = nil
	h.transitionState(protocol.StateIdle)
	h.log.Info("debugger closed — session idle, ready for re-launch")
}

// ── Command execution ─────────────────────────────────────────────────────────

// executeCommand dispatches cmd to the debugger and broadcasts any synchronous
// confirmation event. Errors are broadcast as EventError to all clients.
func (h *Hub) executeCommand(cmd protocol.Command) {
	// For managed sessions, Launch/Attach creates a fresh debugger.
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

	// Guard: no debugger available.
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

	// Track state transitions from successfully executed commands.
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

// injectCommand is called by client read-pumps to deliver a parsed command.
// Resuming commands go to resumeCh to directly unblock a suspended hub;
// all other commands go to cmdCh for the main loop to process.
func (h *Hub) injectCommand(_ *Client, cmd protocol.Command) {
	if resumingCommands[cmd.Kind] {
		select {
		case h.resumeCh <- cmd:
		default:
			// A resuming command is already queued — first writer wins.
		}
		return
	}
	select {
	case h.cmdCh <- clientCommand{cmd: cmd}:
	default:
		h.log.Warn("command queue full — dropping", "kind", cmd.Kind)
	}
}

// ── State machine ─────────────────────────────────────────────────────────────

// transitionState updates the session state and, for managed sessions,
// broadcasts the new state to all connected clients.
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

	// Only broadcast session state for managed sessions (with a session ID).
	if h.sessionID != "" {
		h.broadcastSessionState()
	}
}

// broadcastSessionState sends the current state to all connected clients.
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

// sendStateTo delivers the current session state to a single client (welcome
// message on connect).
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

// ── Broadcast ─────────────────────────────────────────────────────────────────

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

// ── Shutdown ──────────────────────────────────────────────────────────────────

// shutdown closes all client connections and kills the debugger exactly once.
// Safe to call concurrently from the ctx.Done path and last-client-disconnect.
func (h *Hub) shutdown() {
	h.shutdownOnce.Do(func() {
		h.log.Info("hub shutting down")
		// Signal the Run loop to exit.
		close(h.shutdownCh)
		h.registry.closeAll()
		if h.dbg != nil {
			_ = h.dbg.Kill()
		}
	})
}
