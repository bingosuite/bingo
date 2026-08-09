package dap

import (
	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// translateEvent maps one bingo event (as delivered by the hub write pump) to
// zero or more DAP messages. Runs on the hub write-pump goroutine.
func (h *Handler) translateEvent(evt protocol.Event) {
	switch evt.Kind {
	case protocol.EventStepped, protocol.EventBreakpointHit, protocol.EventPaused, protocol.EventPanic:
		h.onStop(evt)
	case protocol.EventContinued:
		h.onContinued()
	case protocol.EventProcessExited:
		h.onProcessExited(evt)
	case protocol.EventOutput:
		h.onOutput(evt)
	case protocol.EventBreakpointSet:
		h.onBreakpointSet(evt)
	case protocol.EventBreakpointCleared:
		h.onBreakpointCleared()
	case protocol.EventLocals:
		h.onLocals(evt)
	case protocol.EventEvaluate:
		h.onEvaluated(evt)
	case protocol.EventFrames:
		h.onFrames(evt)
	case protocol.EventGoroutines:
		h.onGoroutines(evt)
	case protocol.EventGoroutineSnapshot:
		// Deliberately ignored. The auto-streamed concurrency snapshot has no
		// DAP equivalent, and translating it would corrupt the FIFO that
		// correlates a DAP `threads` request to EventGoroutines (snapshots are
		// broadcast unsolicited, with no matching request). DAP clients that
		// want goroutine data use the threads request; the rich snapshot is a
		// WebSocket-only concurrency-visualization stream.
	case protocol.EventRestarted:
		h.onRestarted(evt)
	case protocol.EventError:
		h.onError(evt)
	case protocol.EventSessionState:
		// For a JOINING connection, the hub's welcome state seeds the joiner's
		// initial DAP state. For the normal launch/attach path it is
		// informational (the entry stop drives the initial state instead).
		h.onSessionState(evt)
	}
}

// onSessionState records the managed session's lifecycle state and, for a
// JOINING connection, consumes the hub's welcome EventSessionState, reflecting
// the shared session's current run state as the joiner's initial DAP state: a
// `stopped` if already suspended, `terminated` if already exited, nothing while
// idle/running. The welcome translation fires at most once (gated on
// awaitingWelcome) and is a no-op for the normal launch/attach path, where
// awaitingWelcome is never set.
//
// Outside that welcome, the state is only recorded — with one exception: idle
// and exited mean the process is gone (a failed relaunch leaves a managed
// session idle), so a stale suspended view is dropped. Nothing is sent; process
// death is already reported by EventProcessExited.
func (h *Handler) onSessionState(evt protocol.Event) {
	var p protocol.SessionStatePayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return
	}

	h.mu.Lock()
	h.sessionState = p.State
	if !h.awaitingWelcome {
		if h.sessionEndedLocked() {
			h.suspended = false
		}
		h.mu.Unlock()
		return
	}
	h.awaitingWelcome = false
	tid := h.curThreadID
	if tid == 0 {
		// No stop event has been seen yet on a freshly-joined suspended session,
		// so we have no goroutine id. DAP requires a threadId; the engine
		// inspects the currently-stopped goroutine regardless, so a synthetic
		// id is safe here.
		tid = 1
	}
	switch p.State {
	case protocol.StateSuspended:
		h.suspended = true
		h.mu.Unlock()
		h.sendStopped("pause", tid)
	case protocol.StateExited:
		h.suspended = false
		h.mu.Unlock()
		h.send(&godap.TerminatedEvent{Event: h.event("terminated")})
	default:
		h.suspended = false
		h.mu.Unlock()
	}
}

// stopGoroutine extracts the stopped goroutine from a suspending event payload.
func stopGoroutine(evt protocol.Event) protocol.Goroutine {
	switch evt.Kind {
	case protocol.EventStepped:
		var p protocol.SteppedPayload
		_ = protocol.DecodeEventPayload(evt, &p)
		return p.Goroutine
	case protocol.EventBreakpointHit:
		var p protocol.BreakpointHitPayload
		_ = protocol.DecodeEventPayload(evt, &p)
		return p.Goroutine
	case protocol.EventPaused:
		var p protocol.PausedPayload
		_ = protocol.DecodeEventPayload(evt, &p)
		return p.Goroutine
	case protocol.EventPanic:
		var p protocol.PanicPayload
		_ = protocol.DecodeEventPayload(evt, &p)
		return p.Goroutine
	}
	return protocol.Goroutine{}
}

func (h *Handler) onStop(evt protocol.Event) {
	tid := threadID(stopGoroutine(evt).ID)

	h.mu.Lock()
	// Every stop is a fresh memory snapshot; drop variable subtrees cached
	// against the previous suspension so a stale child ref can't be expanded.
	h.resetVarsLocked()
	launching := h.launching
	restarting := h.restarting
	stopOnEntry := h.stopOnEntry
	h.curThreadID = tid

	// The first stop after Launch/Attach is the entry stop: fire `initialized`
	// (breakpoints can now resolve against the loaded image) but withhold the
	// launch response and any `stopped` until configurationDone.
	if launching {
		h.launching = false
		h.suspended = true
		h.mu.Unlock()
		h.send(&godap.InitializedEvent{Event: h.event("initialized")})
		return
	}

	// The first Stepped after a Restart is the new process's entry stop.
	if restarting && evt.Kind == protocol.EventStepped {
		if h.restartReqSeq != 0 {
			// EventRestarted precedes its process's entry in the hub/client FIFO.
			// A pending newer response makes this the superseded process's entry;
			// preserve the latch for the replacement process.
			h.mu.Unlock()
			return
		}
		h.restarting = false
		if stopOnEntry {
			h.suspended = true
			h.mu.Unlock()
			h.sendStopped("entry", tid)
			return
		}
		h.pendingContinues++
		h.suspended = false
		h.mu.Unlock()
		if cmd, err := marshalCommand(protocol.CmdContinue, nil); err == nil {
			h.enqueue(cmd)
		}
		return
	}

	h.suspended = true
	h.mu.Unlock()
	h.sendStopped(stoppedReason(evt.Kind), tid)
}

func (h *Handler) onContinued() {
	h.mu.Lock()
	tid := h.curThreadID
	if h.pendingContinues > 0 {
		// Our own resume — suppress; DAP already implied continuation via the
		// continue/step response.
		h.pendingContinues--
		h.suspended = false
		h.mu.Unlock()
		return
	}
	h.suspended = false
	h.mu.Unlock()

	// Out-of-band resume (a WebSocket client drove Continue): surface it so the
	// IDE's UI reflects that the tracee is running again.
	h.send(&godap.ContinuedEvent{
		Event: h.event("continued"),
		Body:  godap.ContinuedEventBody{ThreadId: threadID(tid), AllThreadsContinued: true},
	})
}

func (h *Handler) onProcessExited(evt protocol.Event) {
	var p protocol.ProcessExitedPayload
	_ = protocol.DecodeEventPayload(evt, &p)

	h.mu.Lock()
	h.suspended = false
	h.sessionState = protocol.StateExited
	h.mu.Unlock()

	h.send(&godap.ExitedEvent{Event: h.event("exited"), Body: godap.ExitedEventBody{ExitCode: p.ExitCode}})
	h.send(&godap.TerminatedEvent{Event: h.event("terminated")})
}

func (h *Handler) onOutput(evt protocol.Event) {
	var p protocol.OutputPayload
	_ = protocol.DecodeEventPayload(evt, &p)
	category := "stdout"
	if p.Stream == "stderr" {
		category = "stderr"
	}
	h.send(&godap.OutputEvent{Event: h.event("output"), Body: godap.OutputEventBody{Category: category, Output: p.Content}})
}

func (h *Handler) onBreakpointSet(evt protocol.Event) {
	var p protocol.BreakpointSetPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return
	}

	h.mu.Lock()
	var ready *bpRequest
	if len(h.setQ) > 0 {
		slot := h.setQ[0]
		h.setQ = h.setQ[1:]
		if lines := h.bpByFile[slot.file]; lines != nil {
			lines[slot.line] = breakpointState{
				dapID:      p.Breakpoint.ID,
				debuggerID: p.Breakpoint.ID,
			}
		}
		slot.resolved = true
		slot.bp = godap.Breakpoint{
			Id:       p.Breakpoint.ID,
			Verified: true,
			Line:     p.Breakpoint.Location.Line,
			Source:   dapSource(p.Breakpoint.Location),
		}
		if slot.req.done() {
			ready = slot.req
		}
	}
	h.mu.Unlock()

	if ready != nil {
		h.sendSetBreakpointsResponse(ready)
	}
}

func (h *Handler) onBreakpointCleared() {
	h.mu.Lock()
	if len(h.clearQ) > 0 {
		h.clearQ = h.clearQ[1:]
	}
	h.mu.Unlock()
}

func (h *Handler) onFrames(evt protocol.Event) {
	var p protocol.FramesPayload
	_ = protocol.DecodeEventPayload(evt, &p)

	h.mu.Lock()
	h.cachedFrames = p.Frames
	seq, ok := 0, false
	if len(h.framesQ) > 0 {
		seq, ok = h.framesQ[0], true
		h.framesQ = h.framesQ[1:]
	}
	h.mu.Unlock()

	if !ok {
		return // out-of-band Frames (another driver) — nothing to correlate
	}
	h.send(&godap.StackTraceResponse{
		Response: h.response(seq, "stackTrace"),
		Body: godap.StackTraceResponseBody{
			StackFrames: dapStackFrames(p.Frames),
			TotalFrames: len(p.Frames),
		},
	})
}

func (h *Handler) onGoroutines(evt protocol.Event) {
	var p protocol.GoroutinesPayload
	_ = protocol.DecodeEventPayload(evt, &p)

	h.mu.Lock()
	seq, ok := 0, false
	if len(h.threadsQ) > 0 {
		seq, ok = h.threadsQ[0], true
		h.threadsQ = h.threadsQ[1:]
	}
	h.mu.Unlock()

	if !ok {
		return
	}
	h.send(&godap.ThreadsResponse{
		Response: h.response(seq, "threads"),
		Body:     godap.ThreadsResponseBody{Threads: dapThreads(p.Goroutines)},
	})
}

func (h *Handler) onLocals(evt protocol.Event) {
	var p protocol.LocalsPayload
	_ = protocol.DecodeEventPayload(evt, &p)

	h.mu.Lock()
	var vr *varsReq
	if len(h.localsQ) > 0 {
		vr = h.localsQ[0]
		h.localsQ = h.localsQ[1:]
	}
	// Build the typed tree while holding mu: buildVarTree allocates child refs
	// and populates varCache, both mu-guarded.
	vars := h.buildVarTree(p.Variables)
	h.mu.Unlock()

	if vr == nil {
		return
	}
	h.send(&godap.VariablesResponse{
		Response: h.response(vr.seq, "variables"),
		Body:     godap.VariablesResponseBody{Variables: vars},
	})
}

// onEvaluated answers a DAP evaluate request from an EventEvaluate confirmation,
// correlated via the evalQ FIFO. A result with children gets a fresh child ref
// (cached) so the client can expand it with a follow-up variables request.
func (h *Handler) onEvaluated(evt protocol.Event) {
	var p protocol.EvaluatePayload
	_ = protocol.DecodeEventPayload(evt, &p)

	h.mu.Lock()
	seq, ok := 0, false
	if len(h.evalQ) > 0 {
		seq, ok = h.evalQ[0], true
		h.evalQ = h.evalQ[1:]
	}
	ref := 0
	if ok && len(p.Result.Children) > 0 {
		ref = h.allocVarRef()
		h.varCache[ref] = h.buildVarTree(p.Result.Children)
	}
	h.mu.Unlock()

	if !ok {
		return // out-of-band evaluate (another driver) — nothing to correlate
	}
	h.send(&godap.EvaluateResponse{
		Response: h.response(seq, "evaluate"),
		Body: godap.EvaluateResponseBody{
			Result:             p.Result.Value,
			Type:               p.Result.Type,
			VariablesReference: ref,
		},
	})
}

func (h *Handler) onRestarted(evt protocol.Event) {
	var p protocol.RestartedPayload
	decoded := protocol.DecodeEventPayload(evt, &p) == nil

	h.mu.Lock()
	seq := h.restartReqSeq
	h.restartReqSeq = 0
	// The relaunch succeeded, so the captured pre-request view is obsolete: the
	// new process reports its own state through its entry stop.
	h.restartWasSuspended = false
	var discarded []godap.Breakpoint
	if decoded {
		discarded = h.reconcileRestartBreakpointsLocked(p)
	}
	h.mu.Unlock()

	for _, bp := range discarded {
		h.send(&godap.BreakpointEvent{
			Event: h.event("breakpoint"),
			Body: godap.BreakpointEventBody{
				Reason:     "changed",
				Breakpoint: bp,
			},
		})
	}
	if seq != 0 {
		h.send(&godap.RestartResponse{Response: h.response(seq, "restart")})
	}
}

func (h *Handler) reconcileRestartBreakpointsLocked(p protocol.RestartedPayload) []godap.Breakpoint {
	for _, bp := range p.Breakpoints {
		lines := h.bpByFile[bp.Location.File]
		state, ok := lines[bp.Location.Line]
		if !ok || state.debuggerID == 0 {
			continue
		}
		state.debuggerID = bp.ID
		lines[bp.Location.Line] = state
	}

	discarded := make([]godap.Breakpoint, 0, len(p.Discarded))
	for _, dropped := range p.Discarded {
		lines := h.bpByFile[dropped.Location.File]
		state, ok := lines[dropped.Location.Line]
		if !ok || state.debuggerID == 0 {
			continue
		}
		delete(lines, dropped.Location.Line)
		discarded = append(discarded, godap.Breakpoint{
			Id:       state.dapID,
			Verified: false,
			Message:  dropped.Reason,
			Source:   dapSource(dropped.Location),
			Line:     dropped.Location.Line,
		})
	}
	return discarded
}

// onError routes a bingo EventError to the DAP request it corresponds to,
// resolving any pending correlation FIFO so the client is not left hanging.
func (h *Handler) onError(evt protocol.Event) {
	var p protocol.ErrorPayload
	_ = protocol.DecodeEventPayload(evt, &p)

	switch p.Command {
	case protocol.CmdLaunch, protocol.CmdAttach:
		h.failStart(p.Message)
	case protocol.CmdSetBreakpoint:
		h.failBreakpointSet(p.Message)
	case protocol.CmdClearBreakpoint:
		h.onBreakpointCleared()
	case protocol.CmdGoroutines:
		h.mu.Lock()
		seq, ok := 0, false
		if len(h.threadsQ) > 0 {
			seq, ok = h.threadsQ[0], true
			h.threadsQ = h.threadsQ[1:]
		}
		h.mu.Unlock()
		if ok {
			h.send(&godap.ThreadsResponse{Response: h.response(seq, "threads"), Body: godap.ThreadsResponseBody{Threads: dapThreads(nil)}})
		}
	case protocol.CmdFrames:
		h.mu.Lock()
		seq, ok := 0, false
		if len(h.framesQ) > 0 {
			seq, ok = h.framesQ[0], true
			h.framesQ = h.framesQ[1:]
		}
		h.mu.Unlock()
		if ok {
			h.send(&godap.StackTraceResponse{Response: h.response(seq, "stackTrace"), Body: godap.StackTraceResponseBody{StackFrames: []godap.StackFrame{}}})
		}
	case protocol.CmdLocals:
		h.mu.Lock()
		var vr *varsReq
		if len(h.localsQ) > 0 {
			vr = h.localsQ[0]
			h.localsQ = h.localsQ[1:]
		}
		h.mu.Unlock()
		if vr != nil {
			h.send(&godap.VariablesResponse{Response: h.response(vr.seq, "variables"), Body: godap.VariablesResponseBody{Variables: []godap.Variable{}}})
		}
	case protocol.CmdEvaluate:
		h.mu.Lock()
		seq, ok := 0, false
		if len(h.evalQ) > 0 {
			seq, ok = h.evalQ[0], true
			h.evalQ = h.evalQ[1:]
		}
		h.mu.Unlock()
		if ok {
			h.send(h.errorResponse(seq, "evaluate", p.Message))
		}
	case protocol.CmdRestart:
		h.failRestart(p.Message)
	case protocol.CmdContinue, protocol.CmdStepOver, protocol.CmdStepInto, protocol.CmdStepOut:
		h.failResume(p.Command, p.Message)
	default:
		h.emitConsole("error: " + p.Message + "\n")
	}
}

// resumeCommandName maps a rejected resuming command back to the DAP request
// that issued it, for the console line reporting the failure.
func resumeCommandName(kind protocol.CommandKind) string {
	switch kind {
	case protocol.CmdStepOver:
		return "next"
	case protocol.CmdStepInto:
		return "stepIn"
	case protocol.CmdStepOut:
		return "stepOut"
	default:
		return "continue"
	}
}

// failResume resynchronizes the adapter after the hub REJECTS a resume it has
// already acknowledged as successful. onContinue/onStep answer optimistically
// (DAP has no way to retract a successful continue/next response), but a
// rejected resume leaves the engine suspended — see AGENTS.md → Suspend/resume
// (Rejected resumes). Without this the adapter would keep suspended=false
// forever: every later threads/stackTrace/variables/evaluate takes its
// not-suspended branch and answers synthetically without reaching the hub, and
// the IDE shows a running program that can never stop again. A `stopped` event
// is the only DAP message that can walk the client back, so one is emitted with
// the rejection text.
//
// Recovery deliberately does NOT go through onStop: the process never moved, so
// the current suspension's varCache stays valid and the launching/restarting
// latches must not be disturbed.
func (h *Handler) failResume(kind protocol.CommandKind, msg string) {
	h.mu.Lock()
	if kind == protocol.CmdContinue && h.pendingContinues > 0 {
		// The EventContinued this resume would have produced never arrives, so
		// its suppression debt has to be settled here or the NEXT out-of-band
		// continue would be swallowed instead of surfaced.
		h.pendingContinues--
	}
	// Skip while the handshake still owns the initial state report and would
	// send it after this: `launching` waits for the entry EventStepped (a
	// `stopped` here would precede the launch response), and a joiner's
	// `awaitingWelcome` waits for the hub's welcome EventSessionState, which
	// can be raced by another client's rejection between AddClient registering
	// this client and the welcome being delivered. Skip while already
	// suspended: a real stop won the race, or an earlier rejection already
	// resynced, and the client's current `stopped` already describes this
	// suspension. Skip once the session is idle/exited: there is no process to
	// be stopped at, so the rejection is just the hub reporting that.
	resync := !h.suspended && !h.launching && !h.awaitingWelcome && !h.sessionEndedLocked()
	if resync {
		h.suspended = true
	}
	tid := threadID(h.curThreadID)
	h.mu.Unlock()

	h.emitConsole(resumeCommandName(kind) + " failed: " + msg + "\n")
	if !resync {
		return
	}
	h.send(&godap.StoppedEvent{
		Event: h.event("stopped"),
		Body: godap.StoppedEventBody{
			Reason:            "exception",
			Description:       "runtime error",
			Text:              msg,
			ThreadId:          tid,
			AllThreadsStopped: true,
		},
	})
}

// failRestart resolves a rejected CmdRestart: release the response gate and the
// entry latch, and restore the view onRestart optimistically cleared. Unlike
// failResume it emits no `stopped` — the delayed error response tells the client
// the restart failed, so whatever state the client was in remains the current
// one.
//
// The restore is the CAPTURED pre-request view, not an unconditional suspend:
// DAP allows restart while the tracee is running, and the hub rejects a restart
// on an attach-created session (no prior Launch) without touching that still-
// running process — claiming a suspension there would desynchronize the adapter
// the opposite way. A relaunch that fails after the old process was killed
// leaves the session idle, which likewise must not report a suspension.
func (h *Handler) failRestart(msg string) {
	h.mu.Lock()
	seq := h.restartReqSeq
	h.restartReqSeq = 0
	h.restarting = false
	if seq != 0 {
		if h.restartWasSuspended && !h.sessionEndedLocked() {
			h.suspended = true
		}
		h.restartWasSuspended = false
	}
	h.mu.Unlock()

	if seq != 0 {
		h.send(h.errorResponse(seq, "restart", msg))
	}
}

// failStart reports a Launch/Attach failure during the handshake: error the
// pending start request and terminate the DAP session.
func (h *Handler) failStart(msg string) {
	h.mu.Lock()
	launching := h.launching
	seq := h.startReqSeq
	cmd := h.startCmd
	h.launching = false
	h.mu.Unlock()

	if !launching {
		return
	}
	if cmd == "" {
		cmd = "launch"
	}
	h.send(h.errorResponse(seq, cmd, msg))
	h.send(&godap.TerminatedEvent{Event: h.event("terminated")})
}

// failBreakpointSet resolves the head pending set slot as unverified, then
// completes its request if that was the last outstanding slot.
func (h *Handler) failBreakpointSet(msg string) {
	h.mu.Lock()
	var ready *bpRequest
	if len(h.setQ) > 0 {
		slot := h.setQ[0]
		h.setQ = h.setQ[1:]
		if lines := h.bpByFile[slot.file]; lines != nil {
			delete(lines, slot.line)
		}
		slot.resolved = true
		slot.bp = godap.Breakpoint{Verified: false, Line: slot.line, Message: msg}
		if slot.req.done() {
			ready = slot.req
		}
	}
	h.mu.Unlock()

	if ready != nil {
		h.sendSetBreakpointsResponse(ready)
	}
}

func (h *Handler) sendSetBreakpointsResponse(r *bpRequest) {
	bps := make([]godap.Breakpoint, len(r.slots))
	for i, s := range r.slots {
		bps[i] = s.bp
	}
	h.send(&godap.SetBreakpointsResponse{
		Response: h.response(r.reqSeq, "setBreakpoints"),
		Body:     godap.SetBreakpointsResponseBody{Breakpoints: bps},
	})
}
