//go:build !((linux && amd64) || (darwin && arm64))

package debugger

import "fmt"

type unsupportedDebugger struct {
	debuggerEvents chan DebuggerEvent
}

func NewDebugger(
	debuggerEvents chan DebuggerEvent,
	debugCommand chan DebugCommand,
) Debugger {
	return &unsupportedDebugger{debuggerEvents: debuggerEvents}
}

func (d *unsupportedDebugger) StartWithDebug(path string) {
	d.debuggerEvents <- SessionEndedEvent{Err: fmt.Errorf("bingo: debugging is not supported on this platform")}
}
