// Package debugger implements a cross-platform Go process debugger.
// Public surface: the Debugger interface, obtained via New() or
// NewWithBackend(). See AGENTS.md for the engine concurrency model.
package debugger

import (
	"errors"

	"github.com/bingosuite/bingo/pkg/protocol"
)

var (
	ErrProcessExited  = errors.New("debugger: process exited")
	ErrNotSuspended   = errors.New("debugger: process is not suspended")
	ErrAlreadyRunning = errors.New("debugger: process already running")
	ErrNoProcess      = errors.New("debugger: no process")
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

	// Locals: frame 0 is innermost.
	Locals(frameIndex int) ([]protocol.Variable, error)
	StackFrames() ([]protocol.Frame, error)
	Goroutines() ([]protocol.Goroutine, error)

	// Events delivers async notifications. Closed on shutdown; caller must drain.
	Events() <-chan protocol.Event
}

// New returns a Debugger backed by the platform-native OS backend.
func New() Debugger {
	return newEngine(newBackend())
}

// NewWithBackend returns a Debugger using the supplied Backend. Tests only.
func NewWithBackend(b Backend) Debugger {
	return newEngine(b)
}
