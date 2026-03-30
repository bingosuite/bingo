// Package debugger implements a cross-platform Go process debugger.
//
// The public surface is the Debugger interface. Obtain one via New() or
// NewWithBackend() (for testing). Call Launch or Attach to start a session,
// then read Events() to receive asynchronous notifications. All other methods
// are synchronous and block until the operation completes.
//
// Concurrency model: the engine serialises all state mutations through its
// event loop goroutine. Public methods are safe to call from any goroutine.
package debugger

import (
	"errors"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// ── Sentinel errors ───────────────────────────────────────────────────────────

var (
	// ErrProcessExited is returned or emitted when the tracee exits.
	ErrProcessExited = errors.New("debugger: process exited")

	// ErrNotSuspended is returned when an inspection or step command is
	// issued while the tracee is running.
	ErrNotSuspended = errors.New("debugger: process is not suspended")

	// ErrAlreadyRunning is returned when Launch or Attach is called while
	// a process is already live.
	ErrAlreadyRunning = errors.New("debugger: process already running")

	// ErrNoProcess is returned when a command requires a live process but
	// none has been launched or attached yet.
	ErrNoProcess = errors.New("debugger: no process")
)

// ── Debugger ──────────────────────────────────────────────────────────────────

// Debugger is the interface consumed by the hub. All methods are goroutine-safe.
type Debugger interface {
	// Launch starts binaryPath under the debugger. The process is stopped at
	// its first instruction; no code runs until Continue or a step is called.
	// DWARF debug info is loaded from binaryPath automatically.
	// env contains additional environment variables in KEY=VALUE form;
	// pass nil to inherit the server's environment unchanged.
	Launch(binaryPath string, args []string, env []string) error

	// Attach connects to an already-running process by PID.
	// The process is stopped immediately on attach.
	// binaryPath may be empty; if provided, DWARF debug info is loaded from it
	// so that SetBreakpoint, Locals, and StackFrames work correctly.
	Attach(pid int, binaryPath string) error

	// Kill terminates the tracee and releases all OS resources.
	// Safe to call multiple times; subsequent calls are no-ops.
	Kill() error

	// SetBreakpoint resolves file:line to an address via DWARF and patches
	// the tracee's text segment with a trap instruction. Returns the
	// assigned Breakpoint with its ID and resolved location.
	SetBreakpoint(file string, line int) (protocol.Breakpoint, error)

	// ClearBreakpoint removes the breakpoint with the given ID, restoring
	// the original instruction bytes.
	ClearBreakpoint(id int) error

	// Continue resumes execution. Only valid while suspended.
	Continue() error

	// StepOver executes the current source line, stepping over function calls.
	// Only valid while suspended.
	StepOver() error

	// StepInto executes the current source line, stepping into function calls.
	// Only valid while suspended.
	StepInto() error

	// StepOut runs until the current function returns to its caller.
	// Only valid while suspended.
	StepOut() error

	// Locals returns local variables for the given stack frame index.
	// Frame 0 is the innermost (currently executing) frame.
	// Only valid while suspended.
	Locals(frameIndex int) ([]protocol.Variable, error)

	// StackFrames returns the current call stack.
	// Only valid while suspended.
	StackFrames() ([]protocol.Frame, error)

	// Goroutines returns a snapshot of all live goroutines.
	// Only valid while suspended.
	Goroutines() ([]protocol.Goroutine, error)

	// Events returns the channel on which the debugger pushes events.
	// The channel is closed when the debugger shuts down.
	// The caller must drain this channel continuously.
	Events() <-chan protocol.Event
}

// New returns a Debugger backed by the platform-native OS backend.
func New() Debugger {
	return newEngine(newBackend())
}

// NewWithBackend returns a Debugger using the supplied Backend.
// Intended for unit tests — no OS calls are made.
func NewWithBackend(b Backend) Debugger {
	return newEngine(b)
}
