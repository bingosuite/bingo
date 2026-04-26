// Package protocol defines the JSON envelopes and payload types exchanged
// between the bingo server and its clients over WebSocket.
package protocol

import "encoding/json"

const Version = "1.0"

// Event is the envelope for all server-to-client messages.
type Event struct {
	Version string          `json:"v"`
	Kind    EventKind       `json:"kind"`
	Seq     uint64          `json:"seq"`
	Payload json.RawMessage `json:"payload"`
}

// Command is the envelope for all client-to-server messages.
type Command struct {
	Version string          `json:"v"`
	Kind    CommandKind     `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type EventKind string

// Suspending events: the hub blocks after broadcasting these until a resuming
// command (Continue / Step*) arrives. See AGENTS.md → suspend/resume protocol.
const (
	EventBreakpointHit EventKind = "BreakpointHit"
	EventPanic         EventKind = "Panic"
	EventStepped       EventKind = "Stepped"
)

const (
	EventOutput        EventKind = "Output"
	EventProcessExited EventKind = "ProcessExited"

	EventBreakpointSet     EventKind = "BreakpointSet"
	EventBreakpointCleared EventKind = "BreakpointCleared"
	EventContinued         EventKind = "Continued"

	EventLocals     EventKind = "Locals"
	EventFrames     EventKind = "Frames"
	EventGoroutines EventKind = "Goroutines"

	EventSessionState EventKind = "SessionState"

	EventError EventKind = "Error"
)

type CommandKind string

const (
	// CmdNone is the zero value, used for errors with no originating command.
	CmdNone CommandKind = ""

	CmdLaunch CommandKind = "Launch"
	CmdAttach CommandKind = "Attach"
	CmdKill   CommandKind = "Kill"

	CmdSetBreakpoint   CommandKind = "SetBreakpoint"
	CmdClearBreakpoint CommandKind = "ClearBreakpoint"

	CmdContinue CommandKind = "Continue"
	CmdStepOver CommandKind = "StepOver"
	CmdStepInto CommandKind = "StepInto"
	CmdStepOut  CommandKind = "StepOut"

	CmdLocals     CommandKind = "Locals"
	CmdFrames     CommandKind = "Frames"
	CmdGoroutines CommandKind = "Goroutines"
)
