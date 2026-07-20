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
	case protocol.EventFrames:
		h.onFrames(evt)
	case protocol.EventGoroutines:
		h.onGoroutines(evt)
	case protocol.EventRestarted:
		h.onRestarted()
	case protocol.EventError:
		h.onError(evt)
	case protocol.EventSessionState:
		// Informational for DAP; the IDE has no concept of the bingo session
		// lifecycle beyond stopped/continued/exited.
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
			lines[slot.line] = p.Breakpoint.ID
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
	h.mu.Unlock()

	if vr == nil {
		return
	}
	h.send(&godap.VariablesResponse{
		Response: h.response(vr.seq, "variables"),
		Body:     godap.VariablesResponseBody{Variables: dapVariables(p.Variables)},
	})
}

func (h *Handler) onRestarted() {
	h.mu.Lock()
	seq := h.restartReqSeq
	h.restartReqSeq = 0
	h.mu.Unlock()

	if seq != 0 {
		h.send(&godap.RestartResponse{Response: h.response(seq, "restart")})
	}
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
