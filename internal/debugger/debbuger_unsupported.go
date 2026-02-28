//go:build !linux

package debugger

import "fmt"

type unsupportedDebugger struct{}

func NewDebugger(
	breakpointHit chan BreakpointEvent,
	initialBreakpointHit chan InitialBreakpointHitEvent,
	debugCommand chan DebugCommand,
	endDebugSession chan bool,
) Debugger {
	return &unsupportedDebugger{}
}

func (d *unsupportedDebugger) StartWithDebug(path string) {
	panic("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) Continue(pid int) {
	panic("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) SingleStep(pid int) {
	panic("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) StopDebug() {}

func (d *unsupportedDebugger) SetBreakpoint(pid, line int) error {
	return fmt.Errorf("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) ClearBreakpoint(pid, line int) error {
	return fmt.Errorf("bingo: debugging is not supported on this platform")
}
