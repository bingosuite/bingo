package debuginfo

import (
	"debug/gosym"
)

// Target represents information about the debugging target process
type Target struct {
	Path string
	PID  int
	PGID int
}

type DebugInfo interface {
	// GetTarget returns the Target struct of the debugger instance
	GetTarget() Target

	// PCToLine returns the filename, line number and function from the stack memory address
	PCToLine(pc uint64) (file string, line int, fn *gosym.Func)

	// LineToPC returns the memory address, function or an error from the filename and line number
	LineToPC(file string, line int) (pc uint64, fn *gosym.Func, err error)

	// LookupFunc returns the Func struct who's name corresponds to the value of fn
	LookupFunc(fn string) *gosym.Func
}
