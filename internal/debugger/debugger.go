package debugger

// DebuggerEvent is implemented by all events the debugger sends to the hub.
// The unexported marker method seals the interface to this package.
type DebuggerEvent interface {
	debuggerEvent()
}

// BreakpointEvent represents a breakpoint hit event
type BreakpointEvent struct {
	PID      int    `json:"pid"`
	Filename string `json:"filename"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

func (BreakpointEvent) debuggerEvent() {}

// InitialBreakpointHitEvent represents the initial breakpoint hit when debugging starts
type InitialBreakpointHitEvent struct {
	PID int `json:"pid"`
}

func (InitialBreakpointHitEvent) debuggerEvent() {}

// SessionEndedEvent is sent when the debug session ends. Err is nil for a clean exit.
type SessionEndedEvent struct {
	Err error
}

func (SessionEndedEvent) debuggerEvent() {}

// DebugCommand represents commands that can be sent to the debugger
type DebugCommand struct {
	Type string `json:"type"` // "continue", "stepover", "singlestep", "quit", "setBreakpoint"
	Data any    `json:"data,omitempty"`
}

type Debugger interface {
	// StartWithDebug launches the target binary at the given path under debugger control.
	// All outcomes (including errors) are delivered via the debuggerEvents channel as a
	// SessionEndedEvent, so this method has no return value.
	StartWithDebug(path string)
}
