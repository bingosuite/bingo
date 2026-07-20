// Package hub coordinates a single debug session: it bridges WebSocket clients
// with a Debugger instance. See AGENTS.md for the suspend/resume protocol and
// session lifecycle.
package hub

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// suspendingEvents pause the hub and require a resuming command before the
// process is allowed to continue. EventPaused is included: a Pause request
// halts the tracee and suspends it exactly like a breakpoint hit, just
// asynchronously on demand rather than as a self-stop.
var suspendingEvents = map[protocol.EventKind]bool{
	protocol.EventBreakpointHit: true,
	protocol.EventPanic:         true,
	protocol.EventStepped:       true,
	protocol.EventPaused:        true,
}

// resumingCommands unblock a suspended hub via resumeCh (first-writer-wins).
// They are only meaningful while the process is suspended.
//
// CmdKill and CmdPause are deliberately NOT here: both must act while the
// process is RUNNING, not only while suspended, so they ride the ordinary
// cmdCh that Run's main loop drains. Kill routed through resumeCh could not
// terminate a runaway target (tight loop, no breakpoints) because resumeCh is
// only drained inside the suspended wait — see AGENTS.md → Suspend/resume.
var resumingCommands = map[protocol.CommandKind]bool{
	protocol.CmdContinue: true,
	protocol.CmdStepOver: true,
	protocol.CmdStepInto: true,
	protocol.CmdStepOut:  true,
}

// Hub owns one debug session. It bridges the Debugger with all connected
// clients, fanning events out and serialising commands in.
type Hub struct {
	// sessionID is empty for raw hubs created via New() (tests / single-session).
	sessionID string

	// newDebugger creates a debugger on Launch/Attach. nil for raw hubs.
	newDebugger func() debugger.Debugger

	// dbg is the active debugger. nil while idle (no process launched).
	//
	// All writes happen on the Run goroutine (executeCommand, handleRestart,
	// handleDebuggerClosed, teardownFailedStart). shutdown() may read it from a
	// DIFFERENT goroutine (removeClient spawns `go h.shutdown()` when the last
	// client leaves), so writes go through setDbg and shutdown's read takes
	// dbgMu — otherwise a mid-flight Launch/Restart racing the last disconnect
	// is an unsynchronized interface read. Run-goroutine reads need no lock:
	// they only ever contend with same-goroutine writes (sequential) or with
	// shutdown's read (read-read).
	dbgMu    sync.Mutex
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

	// lastLaunch remembers the most recently successful Launch payload so
	// Restart can relaunch the same binary with the same args/env. nil for
	// Attach-based sessions and before any successful Launch — Restart
	// refuses those, mirroring Delve's canRestart check (Restart only makes
	// sense for a process bingo itself started).
	lastLaunch *protocol.LaunchPayload

	// restartBreakpoints mirrors the breakpoints installed on the current
	// debugger (id -> location), purely so Restart can reinstall them on the
	// relaunched process. The engine's breakpointTable remains the sole
	// source of truth for the live process; this is bookkeeping the hub
	// needs across a Kill+relaunch, when the old breakpointTable is gone.
	restartBreakpoints map[int]protocol.Location
}

type clientCommand struct {
	cmd protocol.Command
}

func newHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		registry:           newRegistry(),
		cmdCh:              make(chan clientCommand, 32),
		resumeCh:           make(chan protocol.Command, 1),
		shutdownCh:         make(chan struct{}),
		done:               make(chan struct{}),
		log:                log,
		restartBreakpoints: make(map[int]protocol.Location),
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

// setDbg replaces the active debugger. Called only on the Run goroutine, but
// takes dbgMu so a concurrent shutdown() on another goroutine reads a
// consistent value (see the dbg field comment).
func (h *Hub) setDbg(d debugger.Debugger) {
	h.dbgMu.Lock()
	h.dbg = d
	h.dbgMu.Unlock()
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
	removed := h.registry.remove(c)
	c.closeSend()
	if !removed {
		return
	}
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
	suspending := suspendingEvents[evt.Kind]

	// Discard any resuming command buffered while the process was still running
	// BEFORE broadcasting the suspending event. Such a command is necessarily
	// stale — a legitimate resume can only be sent in response to this event,
	// which the client hasn't observed yet — so dropping it stops it
	// auto-continuing past the suspend and robbing the client of its chance to
	// inspect. Draining *before* the broadcast is what makes that safe: the
	// broadcast is the starting gun, so any resume the client sends back
	// necessarily lands in resumeCh after the drain and is caught by the wait
	// loop below. Draining after the broadcast left a race — an in-process
	// client with no network latency could put its legitimate resume in
	// resumeCh before the drain ran, and the drain would silently eat it,
	// wedging the session (and flaking the hub tests under load).
	if suspending {
		h.drainResumeCh()
	}

	evt.Seq = h.seq.Add(1)
	h.broadcast(evt)

	switch evt.Kind {
	case protocol.EventBreakpointHit, protocol.EventPanic, protocol.EventStepped, protocol.EventPaused:
		h.transitionState(protocol.StateSuspended)
	case protocol.EventProcessExited:
		h.transitionState(protocol.StateExited)
	}

	if !suspending {
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
			// A resume ends the suspend only if it actually took effect. When
			// the debugger rejects it — e.g. a transient backend error while
			// reinstalling a software breakpoint (AGENTS.md → step-over flow),
			// leaving the engine stateSuspended — executeCommand broadcasts an
			// EventError but performs no → running transition. Returning here
			// would strand the client: a retry resume lands in resumeCh, which
			// only this wait loop drains (Run's outer loop never selects on it),
			// so the process could never be resumed again. Keep waiting unless
			// the resume advanced the session out of suspended (running on
			// success, or exited if the process died mid-resume).
			h.executeCommand(cmd)
			if h.State() != protocol.StateSuspended {
				return
			}

		case cc := <-h.cmdCh:
			// Non-resuming command (SetBreakpoint, Locals, …) while suspended.
			// Execute immediately — process is paused — and keep waiting.
			// Restart and Kill are the exceptions: Restart tears down and
			// replaces the suspended process, and Kill terminates it, so in
			// both cases the process we were waiting to resume no longer
			// exists — return and let Run's outer loop pick up the new or
			// closed debugger's events channel.
			h.executeCommand(cc.cmd)
			if cc.cmd.Kind == protocol.CmdRestart || cc.cmd.Kind == protocol.CmdKill {
				return
			}

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
	h.setDbg(nil)
	h.transitionState(protocol.StateIdle)
	h.log.Info("debugger closed — session idle, ready for re-launch")
}

func (h *Hub) executeCommand(cmd protocol.Command) {
	// Restart doesn't fit the generic dispatch(dbg, cmd) shape below: it
	// tears down h.dbg and replaces it with a brand new instance, which only
	// the hub (holder of newDebugger) can do. See handleRestart.
	if cmd.Kind == protocol.CmdRestart {
		h.handleRestart(cmd)
		return
	}

	if h.sessionID != "" && (cmd.Kind == protocol.CmdLaunch || cmd.Kind == protocol.CmdAttach) {
		if h.dbg != nil {
			h.broadcastError(cmd.Kind, fmt.Errorf("debugger already active (state: %s)", h.State()))
			return
		}
		if h.newDebugger == nil {
			h.broadcastError(cmd.Kind, fmt.Errorf("no debugger factory configured"))
			return
		}
		h.setDbg(h.newDebugger())
	}

	if h.dbg == nil {
		// Kill with no active debugger is a benign no-op: there is nothing to
		// terminate, so report success rather than an error. This keeps Kill
		// idempotent across the running/idle/exited states now that it is
		// routed through cmdCh and can reach an already-torn-down session.
		if cmd.Kind == protocol.CmdKill {
			return
		}
		h.broadcastError(cmd.Kind, fmt.Errorf("no active debugger — send Launch or Attach first"))
		return
	}

	result, err := dispatch(h.dbg, cmd)
	if err != nil {
		h.log.Warn("command failed", "kind", cmd.Kind, "err", err)
		if h.sessionID != "" && (cmd.Kind == protocol.CmdLaunch || cmd.Kind == protocol.CmdAttach) {
			h.teardownFailedStart()
		}
		h.broadcastError(cmd.Kind, err)
		return
	}

	switch cmd.Kind {
	case protocol.CmdLaunch:
		h.transitionState(protocol.StateRunning)
		h.rememberLaunch(cmd)
		h.restartBreakpoints = make(map[int]protocol.Location)
	case protocol.CmdAttach:
		h.transitionState(protocol.StateRunning)
		// Restart only makes sense for a process bingo itself launched —
		// mirrors Delve's canRestart check.
		h.lastLaunch = nil
		h.restartBreakpoints = make(map[int]protocol.Location)
	case protocol.CmdContinue, protocol.CmdStepOver, protocol.CmdStepInto, protocol.CmdStepOut:
		h.transitionState(protocol.StateRunning)
	case protocol.CmdSetBreakpoint:
		h.rememberBreakpoint(result)
	case protocol.CmdClearBreakpoint:
		h.forgetBreakpoint(cmd)
	}

	if result.event != nil {
		result.event.Seq = h.seq.Add(1)
		h.broadcast(*result.event)
	}
}

func (h *Hub) teardownFailedStart() {
	dbg := h.dbg
	h.setDbg(nil)
	if dbg != nil {
		_ = dbg.Kill()
	}
	if h.State() != protocol.StateIdle {
		h.transitionState(protocol.StateIdle)
	}
}

// rememberLaunch decodes cmd's LaunchPayload and stores a copy for a future
// Restart. Decode failures are ignored — Launch has already succeeded by the
// time this is called, so at worst Restart later reports "nothing to
// restart" rather than corrupting an active session.
func (h *Hub) rememberLaunch(cmd protocol.Command) {
	var p protocol.LaunchPayload
	if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
		return
	}
	h.lastLaunch = &p
}

// rememberBreakpoint records a successfully-set breakpoint's id -> location
// so Restart can reinstall it later.
func (h *Hub) rememberBreakpoint(result dispatchResult) {
	if result.event == nil {
		return
	}
	var p protocol.BreakpointSetPayload
	if err := protocol.DecodeEventPayload(*result.event, &p); err != nil {
		return
	}
	h.restartBreakpoints[p.Breakpoint.ID] = p.Breakpoint.Location
}

// forgetBreakpoint removes a cleared breakpoint from the Restart bookkeeping.
func (h *Hub) forgetBreakpoint(cmd protocol.Command) {
	var p protocol.ClearBreakpointPayload
	if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
		return
	}
	delete(h.restartBreakpoints, p.ID)
}

// sortedRestartLocations returns the tracked breakpoint locations in
// ascending ID order, so Restart reinstalls them in a deterministic sequence
// (and thus assigns deterministic new IDs) across runs.
func (h *Hub) sortedRestartLocations() []protocol.Location {
	ids := make([]int, 0, len(h.restartBreakpoints))
	for id := range h.restartBreakpoints {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	locs := make([]protocol.Location, 0, len(ids))
	for _, id := range ids {
		locs = append(locs, h.restartBreakpoints[id])
	}
	return locs
}

// handleRestart kills the current process (if any), relaunches the last
// Launch'd binary, and reinstalls previously-set breakpoints at their
// original file:line locations — addresses are re-resolved via DWARF since a
// relaunch can change the load address. Breakpoints that fail to resolve are
// reported as discarded, mirroring Delve's Restart (pkg/proc/target_group.go).
// Only supported for managed, Launch-based sessions: Attach-based sessions
// have no "same binary" to relaunch, matching Delve's canRestart check.
//
// The old debugger's remaining events (e.g. a final ProcessExited from the
// Kill below) are deliberately not forwarded to clients: from the client's
// perspective Restart is one atomic operation, not a Kill followed by a
// fresh Launch. Once Kill returns, the old debugger is abandoned — its
// internal goroutines still tear down on their own (see AGENTS.md → shutdown
// sequence), they're just no longer observed by the hub.
func (h *Hub) handleRestart(cmd protocol.Command) {
	if h.sessionID == "" || h.newDebugger == nil {
		h.broadcastError(cmd.Kind, fmt.Errorf("restart requires a managed session"))
		return
	}
	if h.lastLaunch == nil {
		h.broadcastError(cmd.Kind, fmt.Errorf("no launched process to restart — use Launch first"))
		return
	}

	var override protocol.RestartPayload
	if len(cmd.Payload) > 0 {
		if err := protocol.DecodeCommandPayload(cmd, &override); err != nil {
			h.broadcastError(cmd.Kind, err)
			return
		}
	}

	program := h.lastLaunch.Program
	args := h.lastLaunch.Args
	if override.Args != nil {
		args = override.Args
	}
	env := h.lastLaunch.Env
	if override.Env != nil {
		env = override.Env
	}

	saved := h.sortedRestartLocations()

	if h.dbg != nil {
		_ = h.dbg.Kill()
		h.setDbg(nil)
	}

	newDbg := h.newDebugger()
	if err := newDbg.Launch(program, args, env); err != nil {
		h.broadcastError(cmd.Kind, fmt.Errorf("restart: relaunch failed: %w", err))
		h.transitionState(protocol.StateIdle)
		return
	}
	h.setDbg(newDbg)
	h.lastLaunch = &protocol.LaunchPayload{Program: program, Args: args, Env: env}
	h.transitionState(protocol.StateRunning)

	installed := make([]protocol.Breakpoint, 0, len(saved))
	discarded := make([]protocol.DiscardedBreakpoint, 0)
	newBreakpoints := make(map[int]protocol.Location, len(saved))
	for _, loc := range saved {
		bp, err := newDbg.SetBreakpoint(loc.File, loc.Line)
		if err != nil {
			discarded = append(discarded, protocol.DiscardedBreakpoint{Location: loc, Reason: err.Error()})
			continue
		}
		installed = append(installed, bp)
		newBreakpoints[bp.ID] = bp.Location
	}
	h.restartBreakpoints = newBreakpoints

	evt, err := protocol.NewEvent(protocol.EventRestarted, h.seq.Add(1), protocol.RestartedPayload{
		Program:     program,
		Breakpoints: installed,
		Discarded:   discarded,
	})
	if err != nil {
		h.log.Error("failed to create Restarted event", "err", err)
		return
	}
	h.broadcast(evt)
}

// injectCommand is called by client read-pumps. Resuming commands (Continue,
// Step*) go to resumeCh to directly unblock a suspended hub; everything else —
// including Kill and Pause, which must act while the process is running — goes
// to cmdCh, drained by Run's main loop and the suspended wait loop alike.
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

// drainResumeCh removes any single buffered resuming command without blocking.
// resumeCh has capacity 1, so one non-blocking receive empties it.
func (h *Hub) drainResumeCh() {
	select {
	case <-h.resumeCh:
	default:
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
	if !c.deliver(wire) {
		h.removeClient(c)
	}
}

func (h *Hub) broadcast(evt protocol.Event) {
	wire, err := protocol.MarshalEvent(evt)
	if err != nil {
		h.log.Error("marshal event failed", "err", err)
		return
	}
	for _, c := range h.registry.snapshot() {
		if !c.deliver(wire) {
			h.removeClient(c)
		}
	}
}

func (h *Hub) broadcastError(kind protocol.CommandKind, err error) {
	evt, e := protocol.NewEvent(protocol.EventError, h.seq.Add(1), protocol.ErrorPayload{
		Command: kind,
		Message: err.Error(),
	})
	if e != nil {
		h.log.Error("failed to marshal error event", "err", e, "cause", err)
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
		// shutdown may run on a non-Run goroutine (go h.shutdown() from
		// removeClient), so snapshot dbg under dbgMu to avoid racing a
		// mid-flight Launch/Restart on the Run goroutine.
		h.dbgMu.Lock()
		dbg := h.dbg
		h.dbgMu.Unlock()
		if dbg != nil {
			_ = dbg.Kill()
		}
	})
}
