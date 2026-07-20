package dap

import (
	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// onSetBreakpoints reconciles the debugger's breakpoints for one source with
// the complete set the client requested (DAP replace-all semantics). It diffs
// against bpByFile: lines no longer wanted are cleared, new lines are set, and
// unchanged lines keep their existing id. The response lists one breakpoint per
// requested line, in request order — pending ones are filled in as their
// EventBreakpointSet confirmations arrive (see events.go onBreakpointSet).
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

	desired := make(map[int]bool, len(req.Arguments.Breakpoints))
	for _, b := range req.Arguments.Breakpoints {
		desired[b.Line] = true
	}

	reqObj := &bpRequest{reqSeq: req.Seq}
	var clears, sets [][]byte

	h.mu.Lock()
	current := h.bpByFile[file]
	if current == nil {
		current = make(map[int]int)
		h.bpByFile[file] = current
	}

	// Clear breakpoints no longer requested.
	for line, id := range current {
		if desired[line] {
			continue
		}
		delete(current, line)
		if id == 0 {
			continue // never confirmed; nothing to clear on the debugger
		}
		h.clearQ = append(h.clearQ, id)
		if cmd, err := marshalCommand(protocol.CmdClearBreakpoint, protocol.ClearBreakpointPayload{ID: id}); err == nil {
			clears = append(clears, cmd)
		}
	}

	// Set new lines; keep unchanged ones as already-verified.
	for _, b := range req.Arguments.Breakpoints {
		slot := &bpSlot{req: reqObj, file: file, line: b.Line}
		reqObj.slots = append(reqObj.slots, slot)

		if id, ok := current[b.Line]; ok && id != 0 {
			slot.resolved = true
			slot.bp = godap.Breakpoint{Id: id, Verified: true, Line: b.Line, Source: &src}
			continue
		}

		current[b.Line] = 0 // pending sentinel, replaced on confirmation
		h.setQ = append(h.setQ, slot)
		if cmd, err := marshalCommand(protocol.CmdSetBreakpoint, protocol.SetBreakpointPayload{File: file, Line: b.Line}); err == nil {
			sets = append(sets, cmd)
		}
	}
	allResolved := reqObj.done()
	h.mu.Unlock()

	for _, c := range clears {
		h.enqueue(c)
	}
	for _, c := range sets {
		h.enqueue(c)
	}

	// A request with only unchanged (or empty) breakpoints has no pending
	// confirmations, so respond immediately.
	if allResolved {
		h.sendSetBreakpointsResponse(reqObj)
	}
}
