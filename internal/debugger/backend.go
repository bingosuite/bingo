package debugger

// Backend is the OS-level contract every platform must satisfy. The engine
// never calls ptrace/Mach directly — only Backend. Except for Wait, methods are
// called from the engine's event-loop goroutine. Wait runs in a separate locked
// goroutine while the process is running; any backend state shared with Wait
// must be synchronized, and Wait should avoid memory/register work that can race
// with command handlers.
type Backend interface {
	ContinueProcess() error
	SingleStep(tid int) error
	StopProcess() error

	// PauseSignal reports the signal number StopProcess delivers to interrupt a
	// running tracee. The engine matches a StopSignal stop against it to tell an
	// operator Pause apart from a genuine program signal (linux: SIGSTOP via
	// tgkill; darwin: a catchable signal that surfaces through its wait4 loop —
	// SIGSTOP cannot, see backend_darwin_arm64.go).
	PauseSignal() int

	ReadMemory(addr uint64, dst []byte) error
	WriteMemory(addr uint64, src []byte) error

	GetRegisters(tid int) (Registers, error)
	SetRegisters(tid int, reg Registers) error

	// Threads returns all tracee TIDs; main thread first.
	Threads() ([]int, error)

	// Wait blocks until the tracee produces a debug stop. Must return
	// ErrProcessExited when the tracee has exited.
	Wait() (StopEvent, error)
}

// pidSetter lets the engine notify a backend of the tracee PID after launch/attach.
type pidSetter interface {
	setPID(pid int)
}

func setPID(b Backend, pid int) {
	if ps, ok := b.(pidSetter); ok {
		ps.setPID(pid)
	}
}

type StopReason uint8

const (
	StopBreakpoint StopReason = iota // software breakpoint (int3 / brk)
	StopSingleStep                   // single-step completed
	StopSignal                       // any other signal
	StopExited                       // process exit()
	StopKilled                       // killed externally
)

// StopEvent is what Backend.Wait returns. PC may be zero; the engine resolves
// missing stop PCs on its serialized event loop before emitting user events.
type StopEvent struct {
	Reason   StopReason
	TID      int
	PC       uint64
	ExitCode int // StopExited only
	Signal   int // StopSignal only
}
