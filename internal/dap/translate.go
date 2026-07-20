package dap

import (
	"path/filepath"
	"strconv"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// DAP has a single-threaded stop model keyed by threadId; bingo reports the
// stopped goroutine. A goroutine id of 0 (runtime g0 / not yet assigned) is not
// a valid DAP threadId, so clamp to 1. threadID keeps that mapping in one place.
func threadID(goroutineID int) int {
	if goroutineID < 1 {
		return 1
	}
	return goroutineID
}

// stoppedReason maps a suspending bingo event kind to the DAP `stopped` reason
// string. Anything not a recognised stop maps to the generic "pause".
func stoppedReason(kind protocol.EventKind) string {
	switch kind {
	case protocol.EventBreakpointHit:
		return "breakpoint"
	case protocol.EventStepped:
		return "step"
	case protocol.EventPanic:
		return "exception"
	case protocol.EventPaused:
		return "pause"
	default:
		return "pause"
	}
}

// dapSource builds a DAP Source from a bingo location. Path is the absolute
// file the DWARF reader reported; Name is the basename for display.
func dapSource(loc protocol.Location) *godap.Source {
	if loc.File == "" {
		return nil
	}
	return &godap.Source{Name: filepath.Base(loc.File), Path: loc.File}
}

// frameID is the DAP stackFrame id assigned to a bingo frame. It is
// deliberately frameIndex+1 so it is always non-zero (0 is reserved by DAP for
// "no frame") and is trivially reversible to the bingo frame index — see
// frameIndexFromRef, which the variables request uses to fetch that frame's
// locals.
func frameID(frameIndex int) int { return frameIndex + 1 }

// frameIndexFromRef reverses the frameID / variablesReference encoding back to
// the bingo frame index. scopes returns variablesReference == frameID, so the
// same decode serves both stackTrace frame ids and variable references.
func frameIndexFromRef(ref int) int { return ref - 1 }

// dapStackFrames converts bingo frames to DAP stack frames.
func dapStackFrames(frames []protocol.Frame) []godap.StackFrame {
	out := make([]godap.StackFrame, 0, len(frames))
	for _, f := range frames {
		name := f.Location.Function
		if name == "" {
			name = "?"
		}
		out = append(out, godap.StackFrame{
			Id:     frameID(f.Index),
			Name:   name,
			Source: dapSource(f.Location),
			Line:   f.Location.Line,
			Column: 0,
		})
	}
	return out
}

// dapVariables converts bingo variables to DAP variables. bingo values are
// leaf scalars today (no structured children), so VariablesReference is 0.
func dapVariables(vars []protocol.Variable) []godap.Variable {
	out := make([]godap.Variable, 0, len(vars))
	for _, v := range vars {
		out = append(out, godap.Variable{
			Name:               v.Name,
			Value:              v.Value,
			Type:               v.Type,
			VariablesReference: 0,
		})
	}
	return out
}

// dapThreads converts bingo goroutines to DAP threads. An empty goroutine list
// (the debugger could not enumerate any) yields a single synthetic main thread
// so the client always has a thread to hang a stack trace off of.
func dapThreads(gs []protocol.Goroutine) []godap.Thread {
	if len(gs) == 0 {
		return []godap.Thread{{Id: 1, Name: "main"}}
	}
	out := make([]godap.Thread, 0, len(gs))
	for _, g := range gs {
		name := "goroutine " + strconv.Itoa(g.ID)
		if g.Status != "" {
			name += " (" + g.Status + ")"
		}
		out = append(out, godap.Thread{Id: threadID(g.ID), Name: name})
	}
	return out
}
