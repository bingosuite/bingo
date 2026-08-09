// Package hub coordinates a single debug session: it bridges WebSocket clients
// with a Debugger instance. See AGENTS.md for the suspend/resume protocol and
// session lifecycle.
package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

var ErrHubClosed = errors.New("hub is shutting down")

const (
	commandQueueSize               = 32
	defaultCommandAdmissionTimeout = 5 * time.Second
	defaultSuspendTimeout          = 30 * time.Minute
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

	// dbg and closing are guarded by dbgMu. Factory results and shutdown race
	// through that lock so every debugger is owned either by the running hub or
	// by the path responsible for discarding it.
	dbgMu   sync.Mutex
	dbg     debugger.Debugger
	closing bool

	registry *registry
	log      *slog.Logger

	// state guarded by stateMu — read from AddClient (HTTP goroutine), written
	// from the Run loop.
	stateMu sync.RWMutex
	state   protocol.SessionState

	// cmdCh carries non-resuming commands from client read-pumps to the main
	// loop. Producers apply bounded backpressure rather than dropping commands;
	// commandAdmissionTimeout bounds how long one client may wait for capacity.
	cmdCh                   chan clientCommand
	commandAdmissionTimeout time.Duration

	// resumeCh: capacity 1, first-write-wins. Extras dropped in injectCommand.
	resumeCh chan protocol.Command

	// Immutable after Run starts; tests shorten it per hub to exercise the
	// safety-resume path without a mutable process-wide setting.
	suspendTimeout time.Duration

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

	// bps owns the session-stable logical breakpoint ids clients see and their
	// mapping onto the active engine's physical ids, plus each breakpoint's
	// location so Restart can reinstall it on the relaunched process. The
	// engine's breakpointTable remains the sole source of truth for the live
	// process; this is the identity layer above it (see breakpoints.go).
	// Touched only on the Run goroutine.
	bps *breakpointIDs
}

type clientCommand struct {
	cmd protocol.Command
}

func newHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		registry:                newRegistry(),
		cmdCh:                   make(chan clientCommand, commandQueueSize),
		commandAdmissionTimeout: defaultCommandAdmissionTimeout,
		resumeCh:                make(chan protocol.Command, 1),
		suspendTimeout:          defaultSuspendTimeout,
		shutdownCh:              make(chan struct{}),
		done:                    make(chan struct{}),
		log:                     log,
		bps:                     newBreakpointIDs(),
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
	dbg := h.currentDebugger()
	if dbg == nil {
		return nil
	}
	return dbg.Events()
}

func (h *Hub) currentDebugger() debugger.Debugger {
	h.dbgMu.Lock()
	defer h.dbgMu.Unlock()
	return h.dbg
}

func (h *Hub) installDebugger(dbg debugger.Debugger) bool {
	h.dbgMu.Lock()
	defer h.dbgMu.Unlock()
	if h.closing {
		return false
	}
	h.dbg = dbg
	return true
}

func (h *Hub) detachDebugger() (debugger.Debugger, bool) {
	h.dbgMu.Lock()
	defer h.dbgMu.Unlock()
	if h.closing {
		return nil, false
	}
	dbg := h.dbg
	h.dbg = nil
	return dbg, true
}

func (h *Hub) isClosing() bool {
	h.dbgMu.Lock()
	defer h.dbgMu.Unlock()
	return h.closing
}

func (h *Hub) beginShutdown() debugger.Debugger {
	h.dbgMu.Lock()
	defer h.dbgMu.Unlock()
	h.closing = true
	dbg := h.dbg
	h.dbg = nil
	return dbg
}

func discardDebugger(dbg debugger.Debugger) {
	if dbg != nil {
		_ = dbg.Kill()
	}
}

// AddClient registers conn as a new client. Safe from any goroutine. Admission
// closes atomically with registry teardown so a late join cannot enter a dead
// hub after its one-shot shutdown has completed.
func (h *Hub) AddClient(conn WSConn, log *slog.Logger) (*Client, error) {
	c := newClient(conn, h, log)
	if !h.registry.add(c) {
		_ = conn.Close()
		return nil, ErrHubClosed
	}
	go c.writePump()
	go c.readPump()
	h.log.Info("client connected", "total", h.registry.count())

	if h.sessionID != "" {
		h.sendStateTo(c)
	}

	return c, nil
}

func (h *Hub) removeClient(c *Client) {
	removed, remaining := h.registry.remove(c)
	c.closeSend()
	if !removed {
		return
	}
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
	evt = h.localizeBreakpointIDs(evt)

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

	timeout := time.NewTimer(h.suspendTimeout)
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
			nextEvt = h.localizeBreakpointIDs(nextEvt)
			nextEvt.Seq = h.seq.Add(1)
			h.broadcast(nextEvt)
			if nextEvt.Kind == protocol.EventProcessExited {
				h.transitionState(protocol.StateExited)
				return
			}

		case cmd := <-h.resumeCh:
			h.log.Info("resuming", "command", cmd.Kind)
			// A resume ends the suspend only if it actually took effect. When
			// the debugger rejects it SYNCHRONOUSLY — the dispatch returns an
			// error while the engine stays stateSuspended — executeCommand
			// broadcasts an EventError but performs no → running transition.
			// Returning here would strand the client: a retry resume lands in
			// resumeCh, which only this wait loop drains (Run's outer loop
			// never selects on it), so the process could never be resumed
			// again. Keep waiting unless the resume advanced the session out of
			// suspended (running on success, or exited if the process died
			// mid-resume).
			//
			// A resume that is accepted and only fails LATER — e.g. the
			// software-breakpoint reinstall after the step-over single-step —
			// cannot be caught here: the dispatch already returned nil and this
			// loop has already exited. The engine reports those asynchronous
			// halts with a suspending EventPaused so Run's outer loop re-enters
			// this wait; see AGENTS.md → step-over flow.
			h.executeCommand(cmd)
			if h.State() != protocol.StateSuspended {
				return
			}

		case cc := <-h.cmdCh:
			// Non-resuming command (SetBreakpoint, Locals, …) while suspended.
			// Execute immediately — the process is paused — and decide whether
			// to keep waiting from the OBSERVED state, never from the command
			// kind. Restart and Kill only dissolve the suspend when they
			// actually take effect: a Restart rejected before it touches the
			// debugger (raw hub, no prior Launch, malformed payload) or a
			// failed Kill broadcasts an EventError and leaves the original
			// process suspended. Returning on kind alone stranded those cases —
			// a later resume lands in resumeCh, which only this wait loop
			// drains (Run's outer loop never selects on it), wedging the
			// session forever. Same guard as the resumeCh branch above.
			//
			// A SUCCESSFUL Kill also leaves the state suspended (Kill has no
			// state transition of its own), so the loop stays parked until the
			// debugger reports its own teardown — ProcessExited or a closed
			// Events channel — which the eventsCh branch above already handles.
			// That is strictly more accurate than inferring death from a kind.
			h.executeCommand(cc.cmd)
			if h.State() != protocol.StateSuspended {
				return
			}

		case <-timeout.C:
			h.log.Warn("suspend timeout — auto-continuing", "timeout", h.suspendTimeout)
			h.executeCommand(protocol.Command{
				Version: protocol.Version,
				Kind:    protocol.CmdContinue,
			})
			if h.State() != protocol.StateSuspended {
				return
			}
			// A rejected safety resume must leave the same retry path available
			// as a rejected client resume. Re-arm from now so failures do not
			// collapse into a hot retry loop.
			timeout.Reset(h.suspendTimeout)
		}
	}
}

// handleDebuggerClosed: transition through exited (if not already) to idle,
// ready for a new Launch/Attach cycle.
func (h *Hub) handleDebuggerClosed() {
	if _, open := h.detachDebugger(); !open {
		return
	}
	if h.State() != protocol.StateExited {
		if !h.transitionState(protocol.StateExited) {
			return
		}
	}
	if !h.transitionState(protocol.StateIdle) {
		return
	}
	h.log.Info("debugger closed — session idle, ready for re-launch")
}

func (h *Hub) prepareCommandDebugger(cmd protocol.Command) (dbg debugger.Debugger, callerOwned, proceed bool) {
	dbg = h.currentDebugger()
	if h.sessionID == "" || (cmd.Kind != protocol.CmdLaunch && cmd.Kind != protocol.CmdAttach) {
		return dbg, false, true
	}
	if dbg != nil {
		h.broadcastError(cmd.Kind, fmt.Errorf("debugger already active (state: %s)", h.State()))
		return nil, false, false
	}
	if h.newDebugger == nil {
		h.broadcastError(cmd.Kind, fmt.Errorf("no debugger factory configured"))
		return nil, false, false
	}

	dbg = h.newDebugger()
	if h.isClosing() {
		discardDebugger(dbg)
		return nil, false, false
	}
	// Keep the candidate caller-owned through startup. Installing it first
	// would let shutdown Kill an idle debugger before Launch/Attach creates
	// a process, leaving that later process outside either owner's teardown.
	return dbg, true, true
}

func (h *Hub) transferStartedDebugger(dbg debugger.Debugger, cmd protocol.Command, startErr error) bool {
	if startErr != nil {
		discardDebugger(dbg)
		if h.isClosing() {
			return false
		}
		h.log.Warn("command failed", "kind", cmd.Kind, "err", startErr)
		h.broadcastError(cmd.Kind, startErr)
		return false
	}
	if !h.installDebugger(dbg) {
		discardDebugger(dbg)
		return false
	}
	return true
}

func (h *Hub) executeCommand(cmd protocol.Command) {
	// Restart doesn't fit the generic dispatch(dbg, cmd) shape below: it
	// tears down h.dbg and replaces it with a brand new instance, which only
	// the hub (holder of newDebugger) can do. See handleRestart.
	if cmd.Kind == protocol.CmdRestart {
		h.handleRestart(cmd)
		return
	}

	dbg, callerOwned, proceed := h.prepareCommandDebugger(cmd)
	if !proceed {
		return
	}

	if dbg == nil {
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

	result, err := h.dispatchCommand(dbg, cmd)
	if callerOwned && !h.transferStartedDebugger(dbg, cmd, err) {
		return
	}
	if h.isClosing() {
		return
	}
	if err != nil {
		h.log.Warn("command failed", "kind", cmd.Kind, "err", err)
		h.broadcastError(cmd.Kind, err)
		return
	}

	switch cmd.Kind {
	case protocol.CmdLaunch:
		if !h.transitionState(protocol.StateRunning) {
			return
		}
		h.rememberLaunch(cmd)
		h.bps.reset()
	case protocol.CmdAttach:
		if !h.transitionState(protocol.StateRunning) {
			return
		}
		// Restart only makes sense for a process bingo itself launched —
		// mirrors Delve's canRestart check.
		h.lastLaunch = nil
		h.bps.reset()
	case protocol.CmdContinue, protocol.CmdStepOver, protocol.CmdStepInto, protocol.CmdStepOut:
		if !h.transitionState(protocol.StateRunning) {
			return
		}
	}

	if result.event != nil {
		result.event.Seq = h.seq.Add(1)
		h.broadcast(*result.event)
	}
}

// dispatchCommand routes cmd to the debugger. Breakpoint commands are handled
// by the hub itself because they cross the logical/physical id boundary
// (breakpoints.go) — everything else goes through the hub-agnostic dispatch
// table. Both shapes return the same (dispatchResult, error) so error wrapping
// and the single confirmation broadcast stay centralized in executeCommand.
func (h *Hub) dispatchCommand(dbg debugger.Debugger, cmd protocol.Command) (dispatchResult, error) {
	switch cmd.Kind {
	case protocol.CmdSetBreakpoint:
		return h.setBreakpoint(dbg, cmd)
	case protocol.CmdClearBreakpoint:
		return h.clearBreakpoint(dbg, cmd)
	default:
		return dispatch(dbg, cmd)
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

// sortedRestartTargets returns the live logical breakpoints in ascending
// logical-ID order, so Restart reinstalls them in a deterministic sequence
// across runs.
func (h *Hub) sortedRestartTargets() []restartTarget {
	ids := h.bps.installedLogical()
	targets := make([]restartTarget, 0, len(ids))
	for _, id := range ids {
		m, ok := h.bps.lookup(id)
		if !ok {
			continue
		}
		targets = append(targets, restartTarget{logicalID: id, loc: m.loc})
	}
	return targets
}

// restartTarget is one breakpoint Restart must re-establish on the relaunched
// process, carrying the logical identity clients already hold.
type restartTarget struct {
	logicalID int
	loc       protocol.Location
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

	saved := h.sortedRestartTargets()

	oldDbg, open := h.detachDebugger()
	if !open {
		return
	}
	discardDebugger(oldDbg)

	if h.isClosing() {
		return
	}
	newDbg := h.newDebugger()
	if h.isClosing() {
		discardDebugger(newDbg)
		return
	}
	if err := newDbg.Launch(program, args, env); err != nil {
		if h.isClosing() {
			return
		}
		h.broadcastError(cmd.Kind, fmt.Errorf("restart: relaunch failed: %w", err))
		h.transitionState(protocol.StateIdle)
		return
	}
	if !h.installDebugger(newDbg) {
		discardDebugger(newDbg)
		return
	}
	h.lastLaunch = &protocol.LaunchPayload{Program: program, Args: args, Env: env}
	if !h.transitionState(protocol.StateRunning) {
		return
	}

	// Reinstalling re-homes each surviving logical id onto the replacement
	// engine's own (compacted, reused) physical id. The identities clients hold
	// are unchanged, so a clear generated before the restart still names the
	// same breakpoint afterwards; one that cannot be reinstalled loses its
	// mapping and any later clear for it is rejected rather than aliasing a
	// different breakpoint — see breakpoints.go and AGENTS.md.
	installed := make([]protocol.Breakpoint, 0, len(saved))
	discarded := make([]protocol.DiscardedBreakpoint, 0)
	h.bps.reset()
	for _, target := range saved {
		bp, err := newDbg.SetBreakpoint(target.loc.File, target.loc.Line)
		if err != nil {
			discarded = append(discarded, protocol.DiscardedBreakpoint{Location: target.loc, Reason: err.Error()})
			continue
		}
		h.bps.bind(target.logicalID, bp.ID, bp.Location)
		bp.ID = target.logicalID
		installed = append(installed, bp)
	}

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
// Step*) retain their first-writer-wins semantics. Ordinary commands apply
// bounded backpressure so a live client is never left with silently missing
// commands; persistent overload evicts that client instead.
func (h *Hub) injectCommand(c *Client, cmd protocol.Command) bool {
	if resumingCommands[cmd.Kind] {
		select {
		case h.resumeCh <- cmd:
		case <-h.shutdownCh:
			return false
		case <-c.disconnected:
			return false
		default:
			// First writer wins; later resumers are dropped.
		}
		return true
	}

	cc := clientCommand{cmd: cmd}
	select {
	case h.cmdCh <- cc:
		return true
	case <-h.shutdownCh:
		return false
	case <-c.disconnected:
		return false
	default:
	}

	timeout := time.NewTimer(h.commandAdmissionTimeout)
	defer timeout.Stop()

	select {
	case h.cmdCh <- cc:
		return true
	case <-h.shutdownCh:
		return false
	case <-c.disconnected:
		return false
	case <-timeout.C:
		h.log.Warn("command admission timed out — evicting client",
			"kind", cmd.Kind, "timeout", h.commandAdmissionTimeout)
		h.removeClient(c)
		_ = c.conn.Close()
		return false
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

// transitionState updates state and, for managed sessions, broadcasts. State
// changes linearize before shutdown so a closed hub cannot be resurrected.
func (h *Hub) transitionState(newState protocol.SessionState) bool {
	h.dbgMu.Lock()
	if h.closing {
		h.dbgMu.Unlock()
		return false
	}
	h.stateMu.Lock()
	old := h.state
	if old == newState {
		h.stateMu.Unlock()
		h.dbgMu.Unlock()
		return true
	}
	h.state = newState
	h.stateMu.Unlock()
	h.dbgMu.Unlock()

	h.log.Info("state transition", "from", old, "to", newState)

	if h.sessionID != "" {
		h.broadcastSessionState()
	}
	return true
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
		dbg := h.beginShutdown()
		close(h.shutdownCh)
		h.registry.closeAll()
		discardDebugger(dbg)
	})
}
