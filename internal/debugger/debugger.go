package debugger

type Debugger interface {
	// StartWithDebug launches the target binary at the given path under debugger control
	StartWithDebug(path string)

	// Continue resumes execution of the process with the given PID after a breakpoint
	Continue(pid int)

	// SingleStep executes a single instruction in the process with the given PID
	SingleStep(pid int)

	// StopDebug detaches from the target and ends the debug session
	StopDebug()

	// SetBreakpoint inserts a breakpoint at the given source line in the target
	SetBreakpoint(pid int, line int) error

	// ClearBreakpoint removes the breakpoint at the given source line
	ClearBreakpoint(pid int, line int) error
}
