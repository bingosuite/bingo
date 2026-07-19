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

	// EventPaused reports that a Pause request forcibly halted the running
	// tracee. Like BreakpointHit it is a suspending event — the process is
	// stopped and the hub waits for a resuming command — but it is delivered
	// asynchronously in response to CmdPause rather than a self-stop.
	EventPaused EventKind = "Paused"
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

	// EventRestarted confirms a completed Restart: the process was
	// relaunched and previously-set breakpoints were reinstalled where
	// possible. It is a confirmation, not a suspending event — the process's
	// own suspend state is reported separately via the Stepped event emitted
	// at the new process's entry point (same as after Launch).
	EventRestarted EventKind = "Restarted"
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

	// CmdPause asynchronously interrupts a running tracee, forcing it to
	// suspend (reported via EventPaused). Unlike the resuming commands it is
	// issued while the process is RUNNING, so it is not a member of the hub's
	// resuming-commands set — see AGENTS.md → Pause.
	CmdPause CommandKind = "Pause"

	CmdLocals     CommandKind = "Locals"
	CmdFrames     CommandKind = "Frames"
	CmdGoroutines CommandKind = "Goroutines"

	// CmdRestart kills the current process (if any) and relaunches the last
	// Launch'd binary, reinstalling previously-set breakpoints. Only
	// supported for managed sessions started via Launch — see AGENTS.md →
	// Restart.
	CmdRestart CommandKind = "Restart"
)
