package dap

import (
	"fmt"
	"path/filepath"
	"strconv"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// An unresolved stop must not claim the unrelated real g1.
func stoppedThreadID(goroutineID int) int {
	if goroutineID < 1 {
		return 0
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

// dapStackFrames converts bingo frames to DAP stack frames.
func dapStackFrames(frames []protocol.Frame) []godap.StackFrame {
	out := make([]godap.StackFrame, 0, len(frames))
	for _, f := range frames {
		name := f.Location.Function
		if name == "" {
			name = "?"
		}
		out = append(out, godap.StackFrame{
			Name:   name,
			Source: dapSource(f.Location),
			Line:   f.Location.Line,
			Column: 0,
		})
	}
	return out
}

// buildVarTree converts a bingo typed variable subtree to DAP variables,
// allocating a fresh variablesReference for every node that has children and
// caching those children under it (varCache) so a follow-up variables request
// expands the node synchronously. This is the eager-tree analogue of DAP's lazy
// child fetch: bingo already computed the bounded subtree, so we just index it.
// Caller MUST hold h.mu (it mutates varCache/nextVarRef).
func (h *Handler) buildVarTree(vars []protocol.Variable) ([]godap.Variable, error) {
	out := make([]godap.Variable, 0, len(vars))
	for _, v := range vars {
		if h.inspectionObjects >= maxInspectionObjects {
			return nil, fmt.Errorf("inspection value budget exhausted; resume to inspect a new stop")
		}
		h.inspectionObjects++
		dv := godap.Variable{Name: v.Name, Value: v.Value, Type: v.Type}
		if len(v.Children) > 0 {
			ref, err := h.allocVarRef()
			if err != nil {
				return nil, err
			}
			children, err := h.buildVarTree(v.Children)
			if err != nil {
				return nil, err
			}
			h.varCache[ref] = children
			dv.VariablesReference = ref
		}
		out = append(out, dv)
	}
	return out, nil
}

func dapThreadName(g protocol.Goroutine) string {
	name := "goroutine " + strconv.Itoa(g.ID)
	if g.Status != "" {
		name += " (" + g.Status + ")"
	}
	return name
}
