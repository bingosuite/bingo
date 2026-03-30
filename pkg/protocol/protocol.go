// Package protocol defines all message types exchanged between the hub and
// connected clients over WebSocket. Both directions use JSON envelopes with a
// typed Kind field; all payload structs live in this package so that the hub,
// the debugger, and the client SDK share exactly one definition of truth.
package protocol

import "encoding/json"

// Version is included in every envelope. Clients should reject messages whose
// version major component differs from their own.
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

// ── Event kinds ───────────────────────────────────────────────────────────────

type EventKind string

const (
	// Suspending events — hub blocks until a client sends a resuming command.
	EventBreakpointHit EventKind = "BreakpointHit"
	EventPanic         EventKind = "Panic"

	// Informational events — broadcast immediately, hub does not block.
	EventOutput        EventKind = "Output"
	EventProcessExited EventKind = "ProcessExited"

	// Confirmation events — sent synchronously after a command succeeds.
	EventBreakpointSet     EventKind = "BreakpointSet"
	EventBreakpointCleared EventKind = "BreakpointCleared"
	EventStepped           EventKind = "Stepped"
	EventContinued         EventKind = "Continued"

	// Inspection results — responses to Locals/Frames/Goroutines commands.
	EventLocals     EventKind = "Locals"
	EventFrames     EventKind = "Frames"
	EventGoroutines EventKind = "Goroutines"

	// Session state — broadcast to all clients on state transitions and to
	// newly joined clients so they are immediately synced.
	EventSessionState EventKind = "SessionState"

	// Error — command failed; never suspends.
	EventError EventKind = "Error"
)

// ── Command kinds ─────────────────────────────────────────────────────────────

type CommandKind string

const (
	// CmdNone is the zero value. Used internally when an error has no
	// associated command (e.g. an OS-level error from the debugger backend).
	CmdNone CommandKind = ""

	// Process lifecycle.
	CmdLaunch CommandKind = "Launch"
	CmdAttach CommandKind = "Attach"
	CmdKill   CommandKind = "Kill"

	// Breakpoints.
	CmdSetBreakpoint   CommandKind = "SetBreakpoint"
	CmdClearBreakpoint CommandKind = "ClearBreakpoint"

	// Execution control — only valid while suspended.
	CmdContinue CommandKind = "Continue"
	CmdStepOver CommandKind = "StepOver"
	CmdStepInto CommandKind = "StepInto"
	CmdStepOut  CommandKind = "StepOut"

	// Inspection — only valid while suspended.
	CmdLocals     CommandKind = "Locals"
	CmdFrames     CommandKind = "Frames"
	CmdGoroutines CommandKind = "Goroutines"
)
