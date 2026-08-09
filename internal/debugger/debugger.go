// Package debugger implements a cross-platform Go process debugger.
// Public surface: the Debugger interface, obtained via New() or
// NewWithBackend(). See AGENTS.md for the engine concurrency model.
package debugger

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/bingosuite/bingo/pkg/protocol"
)

var (
	ErrProcessExited  = errors.New("debugger: process exited")
	ErrNotSuspended   = errors.New("debugger: process is not suspended")
	ErrAlreadyRunning = errors.New("debugger: process already running")
	ErrNoProcess      = errors.New("debugger: no process")
	ErrNotRunning     = errors.New("debugger: process is not running")

	// ErrSessionInvalidated marks a backend failure after which the tracee can
	// no longer be described, let alone debugged — the process image was
	// replaced, or a stop arrived in a shape the wait loop cannot interpret.
	//
	// It exists because those failures leave threads ptrace-stopped and owned by
	// us. Simply reporting the error and tearing down abandons them: closing the
	// tracer thread makes the kernel detach every tracee and RESUME it, with
	// software breakpoints still written into its text. The engine therefore
	// discharges the tracee explicitly — restore the original bytes, then kill a
	// launched process or detach an attached one — before it exits its loop.
	ErrSessionInvalidated = errors.New("debugger: session invalidated")

	// ErrImageReplaced is the execve case of the above, kept distinct because it
	// inverts one step of that cleanup: the saved instruction bytes describe an
	// image that no longer exists, so writing them back would poke old bytes at
	// old addresses into the NEW image and corrupt it. Breakpoint restoration is
	// therefore skipped when the image is gone; nothing of ours is in it to
	// remove.
	ErrImageReplaced = fmt.Errorf("%w: process image replaced", ErrSessionInvalidated)
)

// Debugger is the interface consumed by the hub. All methods are goroutine-safe.
// Inspection and step methods require the process to be suspended.
type Debugger interface {
	// Launch starts binaryPath stopped at its first instruction. DWARF info is
	// loaded automatically. env is appended to the server's environment.
	Launch(binaryPath string, args []string, env []string) error

	// Attach connects to a running PID and stops it. binaryPath is optional but
	// required for breakpoints/locals/frames (DWARF source).
	Attach(pid int, binaryPath string) error

	// Kill terminates the tracee. Idempotent.
	Kill() error

	SetBreakpoint(file string, line int) (protocol.Breakpoint, error)
	ClearBreakpoint(id int) error

	Continue() error
	StepOver() error
	StepInto() error
	StepOut() error

	// Pause asynchronously interrupts a running tracee, forcing it to suspend.
	// It returns ErrNotRunning if the process is not currently running. The
	// suspend itself is reported asynchronously via EventPaused, so a nil
	// return only means the interrupt was requested (fire-and-forget).
	Pause() error

	// Locals: frame 0 is innermost.
	Locals(frameIndex int) ([]protocol.Variable, error)

	// Evaluate resolves a single variable NAME in the given frame (local or
	// parameter, then a frame-package global, then a whole-image fallback) to
	// its bounded typed tree. Name-only: no dotted paths, indexing, or
	// arithmetic. Non-suspending, non-resuming.
	Evaluate(frameIndex int, name string) (protocol.Variable, error)

	StackFrames() ([]protocol.Frame, error)
	Goroutines() (protocol.GoroutinesPayload, error)

	// GoroutineSnapshot returns the full concurrency picture on demand — every
	// goroutine (with parent linkage), every OS thread, and the current
	// goroutine. Requires the process to be suspended. It is a pure
	// observation: it reports no created/exited deltas and does not advance the
	// lifecycle baseline, which the automatic entry/breakpoint/pause snapshots
	// alone own.
	GoroutineSnapshot() (protocol.GoroutineSnapshotPayload, error)

	// Events delivers async notifications. Closed on shutdown; caller must drain.
	Events() <-chan protocol.Event
}

// New returns a Debugger backed by the platform-native OS backend. log is the
// single sink for all debugger logging; pass nil to fall back to
// slog.Default().
func New(log *slog.Logger) Debugger {
	return newEngine(newBackend(), log)
}

// NewWithBackend returns a Debugger using the supplied Backend. Tests only.
func NewWithBackend(b Backend, log *slog.Logger) Debugger {
	return newEngine(b, log)
}
