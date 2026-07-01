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

// ptraceResumer is an optional Backend capability for platforms (Linux) where
// ptrace resume commands are restricted to the tracer thread — the engine's
// event-loop goroutine — and therefore cannot be issued from the wait
// goroutine. When Wait observes a stop that needs a ptrace resume (a real
// signal, or a clone/thread lifecycle event under PTRACE_O_TRACECLONE), it
// hands the stop back to the engine, which calls back here from its own
// goroutine. Backends whose ptrace is process-wide (Darwin/Mach) don't
// implement this; the engine falls back to a plain continue for them.
type ptraceResumer interface {
	// ResumeSignal resumes tracee thread tid after a non-trap signal that
	// interrupted the single-step we issued. Fault signals are delivered on the
	// re-step, async signals (SIGURG) are deferred to the next continue.
	ResumeSignal(tid, signal int, stepping bool) error

	// ResumeThread resumes a thread stopped for a clone/thread lifecycle event
	// (a freshly cloned thread's initial SIGSTOP, a PTRACE_EVENT stop, or a
	// signal delivered to a non-debug thread). signal is forwarded to the
	// thread (0 for lifecycle stops); a bare SIGSTOP is never re-delivered.
	// Keeping every tracee thread traced means a goroutine that migrated onto a
	// fresh OS thread and hits a breakpoint there is caught, not lost to a
	// runtime crash.
	ResumeThread(tid, signal int) error
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
	// StopThreadEvent is internal to the Linux backend: a clone/thread
	// lifecycle stop (new-thread SIGSTOP, PTRACE_EVENT_*, or a signal delivered
	// to a non-debug thread) that the engine must resume from its tracer
	// goroutine via ptraceResumer.ResumeThread. It never surfaces to clients.
	StopThreadEvent
)

// StopEvent is what Backend.Wait returns. PC may be zero; the engine resolves
// missing stop PCs on its serialized event loop before emitting user events.
type StopEvent struct {
	Reason   StopReason
	TID      int
	PC       uint64
	ExitCode int  // StopExited only
	Signal   int  // StopSignal / StopThreadEvent: signal to forward on resume
	Stepping bool // StopSignal only: the signal interrupted an in-flight single-step
}
