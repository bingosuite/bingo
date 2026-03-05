package debugger

import (
	"github.com/bingosuite/bingo/internal/debuginfo"
)

type darwinARM64Debugger struct {
	DebugInfo       debuginfo.DebugInfo
	Breakpoints     map[uint64][]byte
	EndDebugSession chan bool

	// Communication with hub
	BreakpointHit        chan BreakpointEvent
	InitialBreakpointHit chan InitialBreakpointHitEvent
	DebugCommand         chan DebugCommand
}

func NewDebugger(breakpointHit chan BreakpointEvent, initialBreakpointHit chan InitialBreakpointHitEvent, debugCommand chan DebugCommand, endDebugSession chan bool) Debugger {
	return &darwinARM64Debugger{
		Breakpoints:          make(map[uint64][]byte),
		EndDebugSession:      endDebugSession,
		BreakpointHit:        breakpointHit,
		InitialBreakpointHit: initialBreakpointHit,
		DebugCommand:         debugCommand,
	}
}

// StartWithDebug launches the target binary at the given path under debugger control
func (d *darwinARM64Debugger) StartWithDebug(path string) {}

// Continue resumes execution of the process with the given PID after a breakpoint
func (d *darwinARM64Debugger) Continue(pid int) {}

// SingleStep executes a single instruction in the process with the given PID
func (d *darwinARM64Debugger) SingleStep(pid int) {}

// StopDebug detaches from the target and ends the debug session
func (d *darwinARM64Debugger) StopDebug() {}

// SetBreakpoint inserts a breakpoint at the given source line in the target
func (d *darwinARM64Debugger) SetBreakpoint(pid int, line int) error {
	return nil
}

// ClearBreakpoint removes the breakpoint at the given source line
func (d *darwinARM64Debugger) ClearBreakpoint(pid int, line int) error {
	return nil
}
