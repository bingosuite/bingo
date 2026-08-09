package dap

import (
	"sort"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// onSetBreakpoints applies DAP replace-all semantics for one source as a
// transaction over the lines of that source.
//
// The request records its intent (latest wins) and then parks on the lines it
// touches: a slot per requested line, and a removal obligation per line it drops
// that is still armed or still has an operation in flight. Nothing is answered
// until every one of those has settled, so a request can never report success
// while a clear it caused is unconfirmed — which is what makes a rejected clear
// reportable at all. Convergence itself is driven by advanceLineLocked, one
// in-flight operation per line, so a line whose Set is already pending attaches
// the new slot instead of issuing a duplicate.
//
// The FIFO correlation assumes the DAP client is the sole breakpoint driver for
// this session; a WebSocket client concurrently setting breakpoints on the same
// session could misalign confirmations. That is an inherent limit of bingo's
// id-less confirmation events, documented in AGENTS.md.
func (h *Handler) onSetBreakpoints(req *godap.SetBreakpointsRequest) {
	src := req.Arguments.Source
	file := src.Path
	if file == "" {
		file = src.Name
	}

	r := &bpRequest{reqSeq: req.Seq}

	h.mu.Lock()
	lines := h.bpByFile[file]
	if lines == nil {
		lines = make(map[int]*bpLine)
		h.bpByFile[file] = lines
	}

	wanted := make(map[int]bool, len(req.Arguments.Breakpoints))
	for _, b := range req.Arguments.Breakpoints {
		wanted[b.Line] = true
		if lines[b.Line] == nil {
			lines[b.Line] = &bpLine{}
		}
	}
	for line, st := range lines {
		st.desired = wanted[line]
	}

	// One slot per requested line, in request order. A repeated line, or a line
	// whose Set is already in flight, simply parks another waiter on it.
	for _, b := range req.Arguments.Breakpoints {
		slot := &bpSlot{req: r, line: b.Line, source: src}
		r.slots = append(r.slots, slot)
		st := lines[b.Line]
		st.failure = ""
		st.setWaiters = append(st.setWaiters, slot)
	}

	// Every dropped line that is still armed — or whose in-flight Set is about
	// to arm it — is this request's obligation to remove.
	touched := sortedLines(lines)
	for _, line := range touched {
		st := lines[line]
		if st.desired || (st.installedID == 0 && st.pending == nil) {
			continue
		}
		st.clearOwners = append(st.clearOwners, r)
		r.openClears++
	}

	var ready []*bpRequest
	for _, line := range touched {
		ready = append(ready, h.advanceLineLocked(file, line)...)
	}
	// A request that neither installs nor removes anything (an empty request
	// for a source with nothing armed) settles on nobody's behalf.
	if r.ready() {
		ready = append(ready, r)
	}
	h.mu.Unlock()

	h.flushCommands()
	h.respond(ready)
}

// sortedLines orders a source's lines so command issue and response assembly are
// deterministic regardless of map iteration order.
func sortedLines(lines map[int]*bpLine) []int {
	out := make([]int, 0, len(lines))
	for line := range lines {
		out = append(out, line)
	}
	sort.Ints(out)
	return out
}

// advanceLineLocked drives one line toward its desired state, issuing the single
// command the gap calls for; when installed and desired already agree it settles
// everyone parked on the line instead. A line with an operation in flight is
// left alone — that operation owns it, and its completion re-enters here, which
// is how a Set superseded before it confirmed gets cleared the moment its id
// arrives. Caller MUST hold h.mu.
func (h *Handler) advanceLineLocked(file string, line int) []*bpRequest {
	st := h.bpByFile[file][line]
	if st == nil || st.pending != nil {
		return nil
	}

	switch {
	case st.desired && st.installedID == 0:
		cmd, err := marshalCommand(protocol.CmdSetBreakpoint, protocol.SetBreakpointPayload{File: file, Line: line})
		if err != nil {
			st.desired = false
			st.failure = err.Error()
			return h.settleLineLocked(file, line)
		}
		st.pending = &bpOp{file: file, line: line}
		h.setQ = append(h.setQ, st.pending)
		h.queueCommandLocked(cmd)

	case !st.desired && st.installedID != 0:
		cmd, err := marshalCommand(protocol.CmdClearBreakpoint, protocol.ClearBreakpointPayload{ID: st.installedID})
		if err != nil {
			return h.failClearLocked(file, line, err.Error())
		}
		st.pending = &bpOp{file: file, line: line}
		h.clearQ = append(h.clearQ, st.pending)
		h.queueCommandLocked(cmd)

	default:
		return h.settleLineLocked(file, line)
	}
	return nil
}

// settleLineLocked answers everyone parked on a converged line. Removal
// obligations are discharged once the line is actually gone — or once a later
// request re-desired it, which supersedes the removal under latest-wins.
// Caller MUST hold h.mu.
func (h *Handler) settleLineLocked(file string, line int) []*bpRequest {
	st := h.bpByFile[file][line]
	if st == nil {
		return nil
	}
	ready := st.resolveSlots()
	if st.installedID == 0 || st.desired {
		ready = append(ready, st.dischargeOwners("")...)
	}
	h.gcLineLocked(file, line)
	return ready
}

// failClearLocked completes the removal obligations on a line whose clear could
// not be issued or was rejected, failing their requests. It deliberately does
// NOT re-enter the convergence loop: the line is still armed and still unwanted,
// so advancing it again would retry a failing clear forever. The retained
// mapping means the next setBreakpoints for the source can retry it.
//
// Slots parked on the line are answered here too, unconditionally. A failed
// clear ends the line's only in-flight operation, so this is the last thing that
// will happen to it until a new request arrives — anything still waiting would
// otherwise wait forever. That includes an earlier pipelined request whose Set
// this clear was cancelling: its breakpoint really is armed, so the line's
// current identity is a truthful answer for it. Caller MUST hold h.mu.
func (h *Handler) failClearLocked(file string, line int, msg string) []*bpRequest {
	st := h.bpByFile[file][line]
	if st == nil {
		return nil
	}
	ready := st.dischargeOwners(msg)
	ready = append(ready, st.resolveSlots()...)
	// Only reachable when the line is no longer armed (a restart discarded it
	// under the in-flight clear); a still-armed line is retained by gc.
	h.gcLineLocked(file, line)
	return ready
}

// gcLineLocked forgets a line the adapter has no reason to remember: unwanted,
// nothing armed, nothing in flight, nobody waiting. A line that is still armed
// is ALWAYS retained even when unwanted — dropping its debugger id would leave a
// breakpoint no later request could ever remove. Caller MUST hold h.mu.
func (h *Handler) gcLineLocked(file string, line int) {
	lines := h.bpByFile[file]
	st := lines[line]
	if st == nil {
		return
	}
	if st.desired || st.installedID != 0 || st.pending != nil {
		return
	}
	if len(st.setWaiters) > 0 || len(st.clearOwners) > 0 {
		return
	}
	delete(lines, line)
}

// resolveSlots answers every requested slot parked on the line from its current
// installed identity. Caller MUST hold h.mu.
func (st *bpLine) resolveSlots() []*bpRequest {
	if len(st.setWaiters) == 0 {
		return nil
	}
	var ready []*bpRequest
	for _, slot := range st.setWaiters {
		slot.resolved = true
		slot.bp = st.breakpoint(slot)
		if slot.req.ready() {
			ready = append(ready, slot.req)
		}
	}
	st.setWaiters = nil
	return ready
}

// dischargeOwners completes the removal obligations parked on the line. A
// non-empty failure marks each owning request failed. Caller MUST hold h.mu.
func (st *bpLine) dischargeOwners(failure string) []*bpRequest {
	if len(st.clearOwners) == 0 {
		return nil
	}
	var ready []*bpRequest
	for _, r := range st.clearOwners {
		if failure != "" && r.clearFailure == "" {
			r.clearFailure = failure
		}
		r.openClears--
		if r.ready() {
			ready = append(ready, r)
		}
	}
	st.clearOwners = nil
	return ready
}

// breakpoint renders the line's installed identity as the DAP breakpoint
// reported for one requested slot. The debugger may resolve a request to a
// different line than asked, so a confirmed location wins over the requested one.
func (st *bpLine) breakpoint(slot *bpSlot) godap.Breakpoint {
	source := slot.source
	if st.installedID == 0 {
		return godap.Breakpoint{Verified: false, Line: slot.line, Source: &source, Message: st.failure}
	}
	line := slot.line
	resolved := &source
	if st.loc.Line != 0 {
		line = st.loc.Line
	}
	if s := dapSource(st.loc); s != nil {
		resolved = s
	}
	return godap.Breakpoint{Id: st.dapID, Verified: true, Line: line, Source: resolved}
}

// respond answers completed requests in request order. Two pipelined requests
// can complete on the same confirmation, and a DAP client applies responses as
// they arrive, so the later request must be answered last or an older view of
// the source would land on top of the authoritative one.
func (h *Handler) respond(ready []*bpRequest) {
	if len(ready) == 0 {
		return
	}
	sort.SliceStable(ready, func(i, j int) bool { return ready[i].reqSeq < ready[j].reqSeq })
	for _, r := range ready {
		h.sendSetBreakpointsResponse(r)
	}
}

// sendSetBreakpointsResponse answers one setBreakpoints request. A request whose
// removal was rejected by the debugger is failed rather than answered success:
// its breakpoints are still armed, so reporting the requested state would be a
// lie the client has no other way to detect.
func (h *Handler) sendSetBreakpointsResponse(r *bpRequest) {
	if r.clearFailure != "" {
		h.send(h.errorResponse(r.reqSeq, "setBreakpoints", "clear breakpoint failed: "+r.clearFailure))
		return
	}
	bps := make([]godap.Breakpoint, len(r.slots))
	for i, s := range r.slots {
		bps[i] = s.bp
	}
	h.send(&godap.SetBreakpointsResponse{
		Response: h.response(r.reqSeq, "setBreakpoints"),
		Body:     godap.SetBreakpointsResponseBody{Breakpoints: bps},
	})
}
