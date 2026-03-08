package debugger

// BreakpointEvent represents a breakpoint hit event
type BreakpointEvent struct {
	PID      int    `json:"pid"`
	Filename string `json:"filename"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

// InitialBreakpointHitEvent represents the initial breakpoint hit when debugging starts
type InitialBreakpointHitEvent struct {
	PID int `json:"pid"`
}

// DebugCommand represents commands that can be sent to the debugger
type DebugCommand struct {
	Type string `json:"type"` // "continue", "stepover", "singlestep", "quit", "setBreakpoint"
	Data any    `json:"data,omitempty"`
}

type Debugger interface {
	// StartWithDebug launches the target binary at the given path under debugger control
	StartWithDebug(path string) error

	// Continue resumes execution of the process with the given PID after a breakpoint
	Continue(pid int) error

	// SingleStep executes a single instruction in the process with the given PID
	SingleStep(pid int) error

	// StopDebug detaches from the target and ends the debug session
	StopDebug() error

	// SetBreakpoint inserts a breakpoint at the given source line in the target
	SetBreakpoint(pid int, line int) error

	// ClearBreakpoint removes the breakpoint at the given source line
	ClearBreakpoint(pid int, line int) error
}
