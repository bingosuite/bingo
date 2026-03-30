package protocol

// This file contains the typed payload struct for every EventKind and
// CommandKind. The hub and debugger always work with these concrete types;
// json.RawMessage only appears at the WebSocket boundary.

// ─── Shared sub-types ────────────────────────────────────────────────────────

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
	Status     string   `json:"status"` // "running", "waiting", "syscall", "dead"
	CurrentLoc Location `json:"currentLoc"`
	GoLoc      Location `json:"goLoc"` // where the goroutine was spawned
	WaitReason string   `json:"waitReason,omitempty"`
}

// ─── Session state ───────────────────────────────────────────────────────────

// SessionState represents the current phase of a debug session's lifecycle.
// The server drives all state transitions and broadcasts them to every client
// so that UIs always reflect the authoritative state.
type SessionState string

const (
	// StateIdle means the session exists but no process has been launched yet.
	// Valid commands: Launch, Attach.
	StateIdle SessionState = "idle"

	// StateRunning means the debuggee is executing.
	// Valid commands: SetBreakpoint, ClearBreakpoint, Kill.
	StateRunning SessionState = "running"

	// StateSuspended means the debuggee is stopped (breakpoint, panic, etc.).
	// Valid commands: Continue, Step*, Locals, Frames, Goroutines,
	// SetBreakpoint, ClearBreakpoint, Kill.
	StateSuspended SessionState = "suspended"

	// StateExited means the debuggee has terminated. The session remains open
	// so clients can re-launch. Transitions back to StateIdle automatically.
	StateExited SessionState = "exited"
)

// ─── Event payloads ──────────────────────────────────────────────────────────

// BreakpointHitPayload is sent when a breakpoint is hit.
// The process is suspended until the hub receives a resumption command.
type BreakpointHitPayload struct {
	Breakpoint Breakpoint `json:"breakpoint"`
	Goroutine  Goroutine  `json:"goroutine"`
	Frames     []Frame    `json:"frames"`
}

// PanicPayload is sent when the runtime panic handler is triggered.
type PanicPayload struct {
	Message   string    `json:"message"`
	Goroutine Goroutine `json:"goroutine"`
	Frames    []Frame   `json:"frames"`
}

// OutputPayload carries a line of stdout/stderr from the target process.
type OutputPayload struct {
	Stream  string `json:"stream"` // "stdout" | "stderr"
	Content string `json:"content"`
}

// ProcessExitedPayload is sent when the target process exits.
type ProcessExitedPayload struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason,omitempty"` // "killed" | "exited"
}

// BreakpointSetPayload confirms a breakpoint was set.
type BreakpointSetPayload struct {
	Breakpoint Breakpoint `json:"breakpoint"`
}

// BreakpointClearedPayload confirms a breakpoint was removed.
type BreakpointClearedPayload struct {
	ID int `json:"id"`
}

// SteppedPayload is sent after any step command completes.
type SteppedPayload struct {
	Goroutine Goroutine `json:"goroutine"`
	Location  Location  `json:"location"`
	Frames    []Frame   `json:"frames"`
}

// ContinuedPayload confirms the process has been resumed.
type ContinuedPayload struct{}

// LocalsPayload carries the local variables for the requested frame.
type LocalsPayload struct {
	FrameIndex int        `json:"frameIndex"`
	Variables  []Variable `json:"variables"`
}

// FramesPayload carries the current call stack.
type FramesPayload struct {
	Frames []Frame `json:"frames"`
}

// GoroutinesPayload carries a snapshot of all goroutines.
type GoroutinesPayload struct {
	Goroutines []Goroutine `json:"goroutines"`
}

// SessionStatePayload carries the authoritative session state.
// Broadcast on every state transition and sent to clients on connect.
type SessionStatePayload struct {
	SessionID string       `json:"sessionID"`
	State     SessionState `json:"state"`
	Clients   int          `json:"clients"` // number of connected clients
}

// ErrorPayload is sent when a command fails.
// Command is omitempty so that CmdNone (empty string) is omitted from the wire.
type ErrorPayload struct {
	Command CommandKind `json:"command,omitempty"`
	Message string      `json:"message"`
}

// ─── Command payloads ────────────────────────────────────────────────────────

// LaunchPayload asks the debugger to start a new process.
type LaunchPayload struct {
	// Program is the path to the compiled Go binary on the server's filesystem.
	Program string   `json:"program"`
	Args    []string `json:"args,omitempty"`
	// Env contains additional environment variables in KEY=VALUE form.
	// They are appended to the server process's environment.
	Env []string `json:"env,omitempty"`
}

// AttachPayload asks the debugger to attach to an already-running process.
type AttachPayload struct {
	PID int `json:"pid"`
	// BinaryPath is the path to the binary the process was started from.
	// Optional but strongly recommended: without it, SetBreakpoint, Locals,
	// and StackFrames will not work (no DWARF info available).
	BinaryPath string `json:"binaryPath,omitempty"`
}

// SetBreakpointPayload asks the debugger to install a breakpoint.
type SetBreakpointPayload struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

// ClearBreakpointPayload asks the debugger to remove a breakpoint by ID.
type ClearBreakpointPayload struct {
	ID int `json:"id"`
}

// LocalsPayloadCmd asks for locals in a specific stack frame.
// FrameIndex 0 is the innermost (currently executing) frame.
type LocalsPayloadCmd struct {
	FrameIndex int `json:"frameIndex"`
}
