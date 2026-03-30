package debugger

// Backend is the narrow OS-level contract every platform must satisfy.
// The engine never calls ptrace/Mach/Win32 directly — it only calls Backend.
//
// All Backend methods are called exclusively from the engine's event-loop
// goroutine, so implementations do not need to be concurrency-safe internally.
type Backend interface {
	// ContinueProcess resumes all threads of the tracee.
	ContinueProcess() error

	// SingleStep executes exactly one machine instruction on thread tid,
	// then stops.
	SingleStep(tid int) error

	// StopProcess pauses the tracee without killing it.
	StopProcess() error

	// ReadMemory copies len(dst) bytes from the tracee address space at addr.
	ReadMemory(addr uint64, dst []byte) error

	// WriteMemory copies src into the tracee address space at addr.
	WriteMemory(addr uint64, src []byte) error

	// GetRegisters returns the register state of thread tid.
	GetRegisters(tid int) (Registers, error)

	// SetRegisters writes reg back to thread tid.
	SetRegisters(tid int, reg Registers) error

	// Threads returns the TIDs of all tracee threads. Main thread is first.
	Threads() ([]int, error)

	// Wait blocks until the tracee produces a debug stop.
	// Must return ErrProcessExited when the tracee has exited.
	Wait() (StopEvent, error)
}

// ── pidSetter ─────────────────────────────────────────────────────────────────

// pidSetter is an optional interface a Backend may implement so the engine can
// notify it of the tracee PID immediately after launch or attach.
//
// All current platform backends implement this. Test fakes that don't need a
// real PID can omit it — setPID becomes a no-op.
type pidSetter interface {
	setPID(pid int)
}

// setPID calls b.setPID(pid) if b implements pidSetter; otherwise a no-op.
func setPID(b Backend, pid int) {
	if ps, ok := b.(pidSetter); ok {
		ps.setPID(pid)
	}
}

// ── StopEvent ─────────────────────────────────────────────────────────────────

// StopReason classifies why the tracee stopped.
type StopReason uint8

const (
	StopBreakpoint StopReason = iota // software breakpoint (int3 / brk)
	StopSingleStep                   // single-step completed
	StopSignal                       // delivery of any other signal
	StopExited                       // process called exit()
	StopKilled                       // process was killed externally
)

// StopEvent is what Backend.Wait returns.
type StopEvent struct {
	Reason   StopReason
	TID      int    // thread that produced the stop
	PC       uint64 // program counter at time of stop (rewound for breakpoints)
	ExitCode int    // only valid for StopExited
	Signal   int    // only valid for StopSignal
}
