package protocol

// Location is a source position.
type Location struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Function string `json:"function,omitempty"`
}

// Breakpoint is a resolved breakpoint as reported by the debugger.
type Breakpoint struct {
	ID       int      `json:"id"`
	Location Location `json:"location"`
	Enabled  bool     `json:"enabled"`
}

// Variable is a local variable or function argument.
type Variable struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Type    string `json:"type"`
	Address uint64 `json:"address,omitempty"`
}

// Frame is a single entry in the call stack.
type Frame struct {
	Index    int        `json:"index"`
	Location Location   `json:"location"`
	Locals   []Variable `json:"locals,omitempty"`
}

// Goroutine is a snapshot of a running goroutine.
type Goroutine struct {
	ID         int      `json:"id"`
	Status     string   `json:"status"` // "running" | "waiting" | "syscall" | "dead"
	CurrentLoc Location `json:"currentLoc"`
	GoLoc      Location `json:"goLoc"` // where the goroutine was spawned
	WaitReason string   `json:"waitReason,omitempty"`
}

// SessionState represents the lifecycle phase of a debug session.
// See AGENTS.md → session state machine.
type SessionState string

const (
	StateIdle      SessionState = "idle"
	StateRunning   SessionState = "running"
	StateSuspended SessionState = "suspended"
	StateExited    SessionState = "exited"
)

type BreakpointHitPayload struct {
	Breakpoint Breakpoint `json:"breakpoint"`
	Goroutine  Goroutine  `json:"goroutine"`
	Frames     []Frame    `json:"frames"`
}

type PanicPayload struct {
	Message   string    `json:"message"`
	Goroutine Goroutine `json:"goroutine"`
	Frames    []Frame   `json:"frames"`
}

type OutputPayload struct {
	Stream  string `json:"stream"` // "stdout" | "stderr"
	Content string `json:"content"`
}

type ProcessExitedPayload struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason,omitempty"` // "killed" | "exited"
}

type BreakpointSetPayload struct {
	Breakpoint Breakpoint `json:"breakpoint"`
}

type BreakpointClearedPayload struct {
	ID int `json:"id"`
}

type SteppedPayload struct {
	Goroutine Goroutine `json:"goroutine"`
	Location  Location  `json:"location"`
	Frames    []Frame   `json:"frames"`
}

// PausedPayload reports where the tracee was halted by a Pause request. It
// mirrors SteppedPayload: the location is wherever execution happened to be
// interrupted (an async stop), not a source-line boundary.
type PausedPayload struct {
	Goroutine Goroutine `json:"goroutine"`
	Location  Location  `json:"location"`
	Frames    []Frame   `json:"frames"`
}

type ContinuedPayload struct{}

type LocalsPayload struct {
	FrameIndex int        `json:"frameIndex"`
	Variables  []Variable `json:"variables"`
}

type FramesPayload struct {
	Frames []Frame `json:"frames"`
}

type GoroutinesPayload struct {
	Goroutines []Goroutine `json:"goroutines"`
}

type SessionStatePayload struct {
	SessionID string       `json:"sessionID"`
	State     SessionState `json:"state"`
	Clients   int          `json:"clients"`
}

// ErrorPayload reports a failed command. Command uses omitempty so CmdNone
// (the empty-string sentinel) is dropped from the wire.
type ErrorPayload struct {
	Command CommandKind `json:"command,omitempty"`
	Message string      `json:"message"`
}

type LaunchPayload struct {
	Program string   `json:"program"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"` // additional KEY=VALUE entries
}

// AttachPayload asks the debugger to attach to PID. BinaryPath is optional but
// required for breakpoints, locals, and stack frames (DWARF source).
type AttachPayload struct {
	PID        int    `json:"pid"`
	BinaryPath string `json:"binaryPath,omitempty"`
}

type SetBreakpointPayload struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

type ClearBreakpointPayload struct {
	ID int `json:"id"`
}

// LocalsPayloadCmd asks for locals in a stack frame. FrameIndex 0 is innermost.
type LocalsPayloadCmd struct {
	FrameIndex int `json:"frameIndex"`
}

// RestartPayload optionally overrides the args/env used for the relaunch.
// Leave a field nil to reuse the value from the original Launch; pass a
// non-nil slice (including an empty one) to override it — an empty slice
// clears the args/env entirely.
//
// The fields deliberately omit `omitempty`: encoding/json treats a nil slice
// and a non-nil empty slice identically under omitempty (both are dropped),
// which would make an explicit "clear to empty" override indistinguishable
// from "reuse" on the wire. The hub gates the override on nil-ness
// (internal/hub handleRestart: `if override.Args != nil`), so that distinction
// must survive the round trip. See issue #102.
type RestartPayload struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

// DiscardedBreakpoint reports a previously-set breakpoint that could not be
// reinstalled after a Restart (e.g. the file:line no longer resolves).
type DiscardedBreakpoint struct {
	Location Location `json:"location"`
	Reason   string   `json:"reason"`
}

// RestartedPayload confirms a completed Restart.
type RestartedPayload struct {
	Program     string                `json:"program"`
	Breakpoints []Breakpoint          `json:"breakpoints,omitempty"`
	Discarded   []DiscardedBreakpoint `json:"discarded,omitempty"`
}
