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

// tempBPStepper is an optional Backend capability. A backend implements it and
// returns true when it CANNOT reliably hardware single-step an arbitrary thread,
// so the engine must step over a software breakpoint by planting a temporary
// breakpoint on the next source line and continuing, instead of the
// restore→single-step→reinstall dance.
//
// Darwin/arm64 needs this: its ptrace PT_STEP always arms the single-step on the
// task's FIRST thread (get_firstthread), not the tid we pass. When the thread
// that hit the breakpoint is not thread[0] and we Mach-suspend thread[0] to keep
// it from running free, the kernel's per-process step never retires and wait4
// blocks forever. Linux ptrace single-steps a specific thread, so it does not
// implement this interface (single-stepping stays the default).
type tempBPStepper interface {
	needsTempBPStepOver() bool
}

func needsTempBPStepOver(b Backend) bool {
	s, ok := b.(tempBPStepper)
	return ok && s.needsTempBPStepOver()
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
