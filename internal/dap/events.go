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

// onSessionState consumes the hub's welcome EventSessionState for a JOINING
// connection, reflecting the shared session's current run state as the joiner's
// initial DAP state: a `stopped` if already suspended, `terminated` if already
// exited, nothing while idle/running. It fires at most once (gated on
// awaitingWelcome) and is a no-op for the normal launch/attach path, where
// awaitingWelcome is never set.
func (h *Handler) onSessionState(evt protocol.Event) {
	var p protocol.SessionStatePayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return
	}

	h.mu.Lock()
	if !h.awaitingWelcome {
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

// onBreakpointSet records a confirmed install against the operation at the head
// of setQ. The id is an installed FACT and is recorded even when a later request
// already dropped the line — the trap IS armed, and forgetting it is how it
// becomes unremovable. Advancing the line then immediately clears it, so a
// superseded Set is never committed as the line's desired state.
func (h *Handler) onBreakpointSet(evt protocol.Event) {
	var p protocol.BreakpointSetPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		return
	}

	h.mu.Lock()
	var ready []*bpRequest
	if op := h.popSetLocked(); op != nil {
		if st := h.bpByFile[op.file][op.line]; st != nil {
			st.pending = nil
			st.installedID = p.Breakpoint.ID
			st.loc = p.Breakpoint.Location
			st.failure = ""
			if st.dapID == 0 {
				st.dapID = p.Breakpoint.ID
			}
			ready = h.advanceLineLocked(op.file, op.line)
		}
	}
	h.mu.Unlock()

	h.flushCommands()
	h.respond(ready)
}

// onBreakpointCleared retires the operation at the head of clearQ. A line's
// debugger id is dropped ONLY here: a confirmed removal is the only proof the
// trap is actually gone.
func (h *Handler) onBreakpointCleared() {
	h.mu.Lock()
	var ready []*bpRequest
	if op := h.popClearLocked(); op != nil {
		if st := h.bpByFile[op.file][op.line]; st != nil {
			st.pending = nil
			st.installedID = 0
			st.dapID = 0
			st.loc = protocol.Location{}
			// The removal is complete the moment the trap is gone — even if a
			// later request has already re-desired the line and the next Set is
			// about to be issued for it.
			ready = st.dischargeOwners("")
			ready = append(ready, h.advanceLineLocked(op.file, op.line)...)
		}
	}
	h.mu.Unlock()

	h.flushCommands()
	h.respond(ready)
}

// popSetLocked takes the operation the next EventBreakpointSet belongs to. A nil
// result means the confirmation was driven by another client — there is nothing
// of ours to correlate it to. Caller MUST hold h.mu.
func (h *Handler) popSetLocked() *bpOp {
	if len(h.setQ) == 0 {
		return nil
	}
	op := h.setQ[0]
	h.setQ = h.setQ[1:]
	return op
}

// popClearLocked is popSetLocked for EventBreakpointCleared. Caller MUST hold h.mu.
func (h *Handler) popClearLocked() *bpOp {
	if len(h.clearQ) == 0 {
		return nil
	}
	op := h.clearQ[0]
	h.clearQ = h.clearQ[1:]
	return op
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
	var discarded []godap.Breakpoint
	var ready []*bpRequest
	if decoded {
		discarded, ready = h.reconcileRestartBreakpointsLocked(p)
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
	h.respond(ready)
	if seq != 0 {
		h.send(&godap.RestartResponse{Response: h.response(seq, "restart")})
	}
}

// reconcileRestartBreakpointsLocked adopts the relaunched process's breakpoint
// identities: retained lines keep their stable DAP id but take the fresh
// debugger id, and discarded lines are dropped and reported unverified. A line
// with an operation in flight is left alone — that operation still owns the
// line's convergence and its confirmation is still queued in setQ/clearQ, so
// rewriting its state here would desynchronise both. Caller MUST hold h.mu.
func (h *Handler) reconcileRestartBreakpointsLocked(p protocol.RestartedPayload) ([]godap.Breakpoint, []*bpRequest) {
	for _, bp := range p.Breakpoints {
		st := h.bpByFile[bp.Location.File][bp.Location.Line]
		if st == nil || st.installedID == 0 || st.pending != nil {
			continue
		}
		st.installedID = bp.ID
		st.loc = bp.Location
	}

	discarded := make([]godap.Breakpoint, 0, len(p.Discarded))
	var ready []*bpRequest
	for _, dropped := range p.Discarded {
		st := h.bpByFile[dropped.Location.File][dropped.Location.Line]
		if st == nil || st.installedID == 0 || st.pending != nil {
			continue
		}
		dapID := st.dapID
		st.installedID = 0
		st.dapID = 0
		st.desired = false
		st.loc = protocol.Location{}
		st.failure = dropped.Reason
		// The breakpoint is gone, so anyone parked on the line — including a
		// request still waiting for it to be removed — can be answered now.
		ready = append(ready, h.settleLineLocked(dropped.Location.File, dropped.Location.Line)...)
		discarded = append(discarded, godap.Breakpoint{
			Id:       dapID,
			Verified: false,
			Message:  dropped.Reason,
			Source:   dapSource(dropped.Location),
			Line:     dropped.Location.Line,
		})
	}
	return discarded, ready
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
		h.failBreakpointClear(p.Message)
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
		h.mu.Lock()
		seq := h.restartReqSeq
		h.restartReqSeq = 0
		h.restarting = false
		h.mu.Unlock()
		if seq != 0 {
			h.send(h.errorResponse(seq, "restart", p.Message))
		}
	case protocol.CmdContinue:
		h.mu.Lock()
		if h.pendingContinues > 0 {
			h.pendingContinues--
		}
		h.mu.Unlock()
		h.emitConsole("continue failed: " + p.Message + "\n")
	default:
		h.emitConsole("error: " + p.Message + "\n")
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

// failBreakpointSet reports a rejected SetBreakpoint against the operation at the
// head of setQ: every slot parked on the line is answered unverified. The line's
// intent is dropped so the convergence loop does not spin re-issuing a request
// the debugger just refused — the client's next setBreakpoints drives any retry.
func (h *Handler) failBreakpointSet(msg string) {
	h.mu.Lock()
	var ready []*bpRequest
	if op := h.popSetLocked(); op != nil {
		if st := h.bpByFile[op.file][op.line]; st != nil {
			st.pending = nil
			st.failure = msg
			st.desired = false
			ready = h.settleLineLocked(op.file, op.line)
		}
	}
	h.mu.Unlock()

	h.respond(ready)
}

// failBreakpointClear reports a rejected ClearBreakpoint. The line's mapping is
// deliberately RETAINED: the trap is still armed in the debugger, so dropping
// its id would leave a breakpoint the client could never remove. The requests
// that asked for the removal are failed rather than answered success.
func (h *Handler) failBreakpointClear(msg string) {
	h.mu.Lock()
	var ready []*bpRequest
	if op := h.popClearLocked(); op != nil {
		if st := h.bpByFile[op.file][op.line]; st != nil {
			st.pending = nil
			ready = h.failClearLocked(op.file, op.line, msg)
		}
	}
	h.mu.Unlock()

	h.respond(ready)
}
