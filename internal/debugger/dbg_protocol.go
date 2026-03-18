package debugger

import "encoding/json"

// Debugger -> Hub

type DebuggerEventType string

const (
	DbgEventBreakpointHit        DebuggerEventType = "breakpointHit"
	DbgEventInitialBreakpointHit DebuggerEventType = "initialBreakpointHit"
	DbgEventDebugEnded           DebuggerEventType = "debugEnded"
)

type DebugEvent struct {
	Type DebuggerEventType `json:"type"`
	Data json.RawMessage   `json:"data,omitempty"`
}

// BreakpointEvent represents a breakpoint hit event
type BreakpointHitEvent struct {
	PID      int    `json:"pid"`
	Filename string `json:"filename"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

// InitialBreakpointHitEvent represents the initial breakpoint hit when debugging starts
type InitialBreakpointHitEvent struct {
	PID int `json:"pid"`
}

// DebugEndedEvent represents that the debug target has exited
type DebugEndedEvent struct {
	Ended bool `json:"ended"`
}

// Hub -> Debugger

type DebuggerCommandType string

const (
	DbgCommandContinue      DebuggerCommandType = "continue"
	DbgCommandStepOver      DebuggerCommandType = "stepOver"
	DbgCommandSingleStep    DebuggerCommandType = "singleStep"
	DbgCommandQuit          DebuggerCommandType = "quit"
	DbgCommandSetBreakpoint DebuggerCommandType = "setBreakpoint"
)

// DebugCommand represents commands that can be sent to the debugger
type DebugCommand struct {
	Type DebuggerCommandType `json:"type"` // "continue", "stepover", "singlestep", "quit", "setBreakpoint"
	Data any                 `json:"data,omitempty"`
}

type SetBreakpointCommand struct {
	Filename string `json:"filename"`
	Line     int    `json:"line"`
}
