//go:build !((linux && amd64) || (darwin && arm64))

package debugger

import "fmt"

type unsupportedDebugger struct{}

func NewDebugger(
	breakpointHit chan BreakpointEvent,
	initialBreakpointHit chan InitialBreakpointHitEvent,
	debugCommand chan DebugCommand,
	endDebugSession chan error,
) Debugger {
	return &unsupportedDebugger{}
}

func (d *unsupportedDebugger) StartWithDebug(path string) error {
	return fmt.Errorf("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) Continue(pid int) error {
	return fmt.Errorf("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) SingleStep(pid int) error {
	return fmt.Errorf("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) StopDebug() error {
	return fmt.Errorf("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) SetBreakpoint(pid, line int) error {
	return fmt.Errorf("bingo: debugging is not supported on this platform")
}

func (d *unsupportedDebugger) ClearBreakpoint(pid, line int) error {
	return fmt.Errorf("bingo: debugging is not supported on this platform")
}
