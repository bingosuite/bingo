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

// Variable is a local variable, function argument, or a nested field/element
// within one. Value is a type-aware, human-readable rendering; Kind is the
// classifier the formatter resolved from DWARF (int/uint/float/bool/string/
// complex/ptr/struct/slice/array/map/chan/func/interface, or "" when it fell
// back to raw hex). Children carries the bounded eager subtree for aggregates
// (struct fields, slice/array elements, one pointer deref) so a client — and
// DAP's variablesReference tree — can expand a value without a follow-up query.
// See AGENTS.md → DWARF reader notes (type-aware LocalsForFrame).
type Variable struct {
	Name     string     `json:"name"`
	Value    string     `json:"value"`
	Type     string     `json:"type"`
	Address  uint64     `json:"address,omitempty"`
	Kind     string     `json:"kind,omitempty"`
	Children []Variable `json:"children,omitempty"`
}

// Frame is a single entry in the call stack.
type Frame struct {
	Index    int        `json:"index"`
	Location Location   `json:"location"`
	Locals   []Variable `json:"locals,omitempty"`
}

// Goroutine is a snapshot of one goroutine in the tracee's runtime. It carries
// enough to reconstruct a spawn hierarchy (ParentID links a goroutine to the
// one that ran its `go` statement) and lifecycle state. IDs are the runtime's
// goid (fits Go int on both supported 64-bit platforms).
type Goroutine struct {
	ID         int      `json:"id"`                   // goid
	ParentID   int      `json:"parentId,omitempty"`   // parent goroutine's goid; 0 for the root
	Status     string   `json:"status"`               // scheduler status: "running" | "runnable" | "waiting" | "syscall" | "dead" | ...
	WaitReason string   `json:"waitReason,omitempty"` // why a waiting goroutine is blocked (e.g. "chan receive")
	CurrentLoc Location `json:"currentLoc"`           // where the goroutine is now (live PC if running, scheduled PC if parked)
	StartLoc   Location `json:"startLoc,omitempty"`   // the goroutine's entry function (startpc)
	CreatedLoc Location `json:"createdLoc,omitempty"` // the `go` statement that spawned it (gopc)
	ThreadID   int      `json:"threadId,omitempty"`   // OS thread (m.procid) currently running it; 0 if not running
	Current    bool     `json:"current,omitempty"`    // true for the goroutine the debugger is stopped in
}

// Thread is one OS thread (a runtime M) in the tracee. It complements the
// goroutine view: a spawn/lifecycle UI needs both the logical goroutines and
// the physical threads they are (or aren't) scheduled onto.
type Thread struct {
	ID         int      `json:"id"`                   // OS thread id (m.procid); 0 if not yet assigned
	MID        int      `json:"mid,omitempty"`        // runtime m.id
	GoID       int      `json:"goid,omitempty"`       // goid currently running on this thread (m.curg); 0 if idle
	Spinning   bool     `json:"spinning,omitempty"`   // scheduler is spinning looking for work
	CurrentLoc Location `json:"currentLoc,omitempty"` // where the running goroutine's code is
	Current    bool     `json:"current,omitempty"`    // true for the thread the debugger is stopped on
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

// EvaluatePayload carries the result of a CmdEvaluate: the resolved variable
// subtree (with Children when it is an aggregate). See EvaluatePayloadCmd.
type EvaluatePayload struct {
	Result Variable `json:"result"`
}

type FramesPayload struct {
	Frames []Frame `json:"frames"`
}

type GoroutinesPayload struct {
	Goroutines []Goroutine `json:"goroutines"`
}

// GoroutineSnapshotPayload is the full concurrency picture at a suspend point:
// every goroutine (with ParentID linkage for a spawn tree), every OS thread,
// which goroutine is current, and the created/exited goid deltas since the
// previous snapshot. It powers lifecycle and spawn-hierarchy visualizations.
//
// It is emitted automatically on each suspend that changes the concurrency
// picture (breakpoint hit, pause, launch/attach entry) and on demand via
// CmdGoroutineSnapshot. Unlike EventGoroutines — which answers a single DAP
// `threads` request and carries goroutines only — this event is not tied to a
// request, so it also carries the thread list and the lifecycle deltas. Created
// and Exited are populated on the automatic snapshots only; an on-demand query
// answers with the live picture and empty deltas so it cannot consume what the
// next automatic snapshot must report.
type GoroutineSnapshotPayload struct {
	Goroutines []Goroutine `json:"goroutines"`
	Threads    []Thread    `json:"threads"`
	Current    int         `json:"current,omitempty"` // goid of the current goroutine, 0 if unknown
	Created    []int       `json:"created,omitempty"` // goids new since the previous automatic snapshot
	Exited     []int       `json:"exited,omitempty"`  // goids gone since the previous automatic snapshot
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

// EvaluatePayloadCmd asks the debugger to resolve a single variable NAME in a
// stack frame (FrameIndex 0 is innermost). This is name-only: no dotted paths,
// indexing, or arithmetic — those belong to a later expression-evaluator PR.
// A local or parameter of the frame is resolved first; failing that, a
// package-level global of the same name. Answered with EventEvaluate.
type EvaluatePayloadCmd struct {
	FrameIndex int    `json:"frameIndex"`
	Name       string `json:"name"`
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
