// Package protocol defines the JSON envelopes and payload types exchanged
// between the bingo server and its clients over WebSocket.
package protocol

import (
	"encoding/json"
	"fmt"
)

const Version = "1.3"

// Size limits for the goroutine event family — EventGoroutineSnapshot and
// EventGoroutines. They are deliberately NOT generic envelope limits: every
// other event keeps its existing unbounded behaviour, and neither the hub, the
// Go client, nor Location strings are capped. Only these two events carry an
// unbounded runtime collection, so only they are bounded here (see issue #194).
const (
	// MaxGoroutineEventBytes is the exact marshalled Event ceiling for the
	// goroutine event family, matched to the binding consumer's decoder budget
	// (the VS Code observer). A frame above it is rejected below the decoder by
	// the WebSocket transport, so it can never be delivered at all.
	MaxGoroutineEventBytes = 2 * 1024 * 1024

	// MaxSnapshotGoroutines and MaxSnapshotThreads cap element counts
	// independently of the byte budget: a consumer that walks the collection
	// pays per element regardless of how compact each element is.
	MaxSnapshotGoroutines = 5000
	MaxSnapshotThreads    = 2048

	// MinThreadsRetained is the ordered thread floor packed before goroutines
	// compete for the budget. Threads are far cheaper than goroutines and a
	// thread view is useless once it is arbitrarily truncated, so a realistic
	// machine's worth of threads survives even a saturating goroutine set.
	MinThreadsRetained = 32

	// MaxGoroutineStringLength bounds every string inside a packed Goroutine or
	// Thread — status, wait reason, and each Location's file and function.
	//
	// This is NOT a size optimisation: it is the per-element constraint the
	// consumer already enforces, and the producer must agree with it exactly.
	// A single over-long string is comfortably inside the byte budget, so
	// budgeting alone would happily emit an element the consumer is obliged to
	// reject — deterministically killing that connection on every retry. The
	// element is dropped whole instead; strings and Locations are never
	// truncated. Measured in UTF-16 code units to match the consumer (see
	// utf16Len).
	MaxGoroutineStringLength = 4096
)

// VersionError reports that a peer used a wire version other than Version.
type VersionError struct {
	Expected string
	Received string
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("protocol version mismatch: expected %q, received %q", e.Expected, e.Received)
}

// ValidateVersion requires exact wire-version equality.
func ValidateVersion(received string) error {
	if received != Version {
		return &VersionError{Expected: Version, Received: received}
	}
	return nil
}

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

	// EventEvaluate answers a CmdEvaluate with the resolved variable subtree.
	// Like EventLocals it is a non-suspending confirmation (it follows a data
	// request and never gates the hub), consumed by WebSocket clients directly
	// and by DAP as an `evaluate` response.
	EventEvaluate EventKind = "Evaluate"

	// EventGoroutineSnapshot streams the full concurrency picture (goroutines
	// with parent linkage, OS threads, current goroutine, and created/exited
	// lifecycle deltas). It is emitted automatically on every suspend that can
	// change that picture — breakpoint hit, pause, and launch/attach entry —
	// and on demand in response to CmdGoroutineSnapshot. It is NOT a suspending
	// event: it follows a suspending event (or answers a query) and never gates
	// the hub. See AGENTS.md → goroutine snapshot streaming.
	EventGoroutineSnapshot EventKind = "GoroutineSnapshot"

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

	// CmdEvaluate resolves a single variable NAME in a stack frame (answered
	// with EventEvaluate). Name-only — no expressions. Like CmdLocals it is
	// executed immediately and is neither a suspending request nor a resuming
	// command; it requires the process to be suspended.
	CmdEvaluate CommandKind = "Evaluate"

	// CmdGoroutineSnapshot requests a full concurrency snapshot on demand
	// (answered with EventGoroutineSnapshot). The same snapshot is also pushed
	// automatically on each suspend, so a UI only needs this to refresh out of
	// band (e.g. right after connecting). Requires the process to be suspended.
	CmdGoroutineSnapshot CommandKind = "GoroutineSnapshot"

	// CmdRestart kills the current process (if any) and relaunches the last
	// Launch'd binary, reinstalling previously-set breakpoints. Only
	// supported for managed sessions started via Launch — see AGENTS.md →
	// Restart.
	CmdRestart CommandKind = "Restart"
)
