//go:build linux && amd64

package debugger

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

func newBackend() Backend {
	return &linuxBackend{tracer: newTracerThread()}
}

// tracerThread pins a single OS thread and runs every ptrace(2) control op on
// it. On Linux ptrace is thread-bound: after a tracee is attached (via
// PTRACE_TRACEME during fork, or PTRACE_ATTACH), *all* subsequent ptrace
// requests for it must come from the exact thread that became the tracer, or
// they fail with ESRCH. bingo previously issued control ops from two different
// goroutines/threads (the engine loop and each waitLoop), so ops made from the
// wait thread hit ESRCH and were silently swallowed, wedging step-over. This
// mirrors Delve's execPtraceFunc / ptraceThread (pkg/proc/native/proc.go):
// funnel fork/exec, attach, detach, cont, single-step, get/set-regs, peek/poke
// and set-options through one thread. wait4 is NOT routed here — it is safe
// from any thread of the tracer process (Delve calls sys.Wait4 directly), and
// keeping it off-thread lets the engine issue control ops while a wait is
// outstanding.
type tracerThread struct {
	funcCh chan func()
	doneCh chan struct{}
	quit   chan struct{}
	once   sync.Once
}

func newTracerThread() *tracerThread {
	t := &tracerThread{
		funcCh: make(chan func()),
		doneCh: make(chan struct{}),
		quit:   make(chan struct{}),
	}
	go t.run()
	return t
}

func (t *tracerThread) run() {
	runtime.LockOSThread()
	// Stays welded to its OS thread until quit, so the kernel keeps seeing the
	// same tracer. Returning ends the goroutine and releases the locked thread.
	for {
		select {
		case fn := <-t.funcCh:
			fn()
			t.doneCh <- struct{}{}
		case <-t.quit:
			return
		}
	}
}

// execPtrace runs fn on the dedicated tracer thread and blocks until it
// completes. Concurrent callers (engine loop vs waitLoop) are serialised by the
// unbuffered channels, so ptrace ops never interleave. After close() the op
// becomes a no-op rather than blocking forever (the tracee is gone anyway).
func (t *tracerThread) execPtrace(fn func()) {
	select {
	case t.funcCh <- fn:
		<-t.doneCh
	case <-t.quit:
	}
}

// close stops the tracer goroutine so its locked OS thread is released. Only
// funcCh's sole channel of control (quit) is closed — never funcCh itself — so
// a racing execPtrace can never send on a closed channel. Callers must ensure
// no execPtrace is in flight (the engine calls this only after its loop exits).
func (t *tracerThread) close() {
	t.once.Do(func() { close(t.quit) })
}

// tracerExecer is implemented by backends whose ptrace control ops must all run
// on one dedicated thread. Only the linux backend implements it; the platform
// free functions (startTracedProcess/attachToProcess/killProcess) use it to run
// the fork/attach/detach on that thread. Darwin does not implement it.
type tracerExecer interface {
	execPtrace(fn func())
}

type linuxBackend struct {
	pid         int
	stepping    bool // true after SingleStep; classifies the next SIGTRAP
	stepTID     int  // the exact thread SingleStep was issued against
	lastStopTID int
	tracer      *tracerThread
}

func (b *linuxBackend) execPtrace(fn func()) { b.tracer.execPtrace(fn) }

// closeTracer releases the dedicated tracer thread. The engine calls this after
// its loop exits (process gone), when no further ptrace ops can be issued.
func (b *linuxBackend) closeTracer() { b.tracer.close() }

const linuxPtraceOptions = syscall.PTRACE_O_TRACEEXIT |
	syscall.PTRACE_O_TRACEEXEC |
	syscall.PTRACE_O_TRACECLONE

// startTracedProcess forks under ptrace. The child is stopped at its first
// instruction (execve SIGTRAP) ready for the engine to set breakpoints. The
// fork+exec, the reap of the initial execve stop and PTRACE_SETOPTIONS all run
// on the backend's dedicated tracer thread: the forking thread becomes the
// tracee's tracer, so every later ptrace op must originate from that same
// thread.
func startTracedProcess(b Backend, binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	tracer, ok := b.(tracerExecer)
	if !ok {
		return 0, nil, fmt.Errorf("startTracedProcess: backend does not support a tracer thread")
	}

	// codeql-suppress[go/command-injection]: The debugger intentionally launches the local binary selected by the operator.
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	var startErr error
	tracer.execPtrace(func() {
		if err := cmd.Start(); err != nil {
			startErr = fmt.Errorf("exec %q: %w", binaryPath, err)
			return
		}
		pid := cmd.Process.Pid
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
			_ = cmd.Process.Kill()
			startErr = fmt.Errorf("wait for execve stop: %w", err)
			return
		}
		if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
			_ = cmd.Process.Kill()
			startErr = fmt.Errorf("unexpected initial stop: %v", ws)
			return
		}
		if err := syscall.PtraceSetOptions(pid, linuxPtraceOptions); err != nil {
			_ = cmd.Process.Kill()
			startErr = fmt.Errorf("PTRACE_SETOPTIONS: %w", err)
			return
		}
	})
	if startErr != nil {
		return 0, nil, startErr
	}

	return cmd.Process.Pid, cmd, nil
}

func attachToProcess(b Backend, pid int) error {
	tracer, ok := b.(tracerExecer)
	if !ok {
		return fmt.Errorf("attachToProcess: backend does not support a tracer thread")
	}
	var attachErr error
	tracer.execPtrace(func() {
		if err := syscall.PtraceAttach(pid); err != nil {
			attachErr = fmt.Errorf("PTRACE_ATTACH pid %d: %w", pid, err)
			return
		}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
			attachErr = fmt.Errorf("wait after PTRACE_ATTACH: %w", err)
			return
		}
	})
	return attachErr
}

func killProcess(b Backend, pid int, cmd *exec.Cmd) error {
	if cmd != nil {
		// SIGKILL via the OS handle is not a ptrace op, so it is safe from any
		// thread and keeps Kill responsive even if the tracer thread is busy.
		if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
			return err
		}
		_ = cmd.Wait()
		return nil
	}
	// Attached (not launched): detach, don't kill — we don't own the process.
	// PTRACE_DETACH must run on the tracer thread.
	if tracer, ok := b.(tracerExecer); ok {
		tracer.execPtrace(func() { _ = syscall.PtraceDetach(pid) })
	} else {
		_ = syscall.PtraceDetach(pid)
	}
	return nil
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

func (b *linuxBackend) ContinueProcess() error {
	b.stepping = false
	b.stepTID = 0
	tid := b.traceTID()
	var err error
	b.execPtrace(func() { err = syscall.PtraceCont(tid, 0) })
	if err != nil {
		return fmt.Errorf("PTRACE_CONT tid %d: %w", tid, err)
	}
	return nil
}

func (b *linuxBackend) SingleStep(tid int) error {
	b.stepping = true
	b.stepTID = tid
	var err error
	b.execPtrace(func() { err = syscall.PtraceSingleStep(tid) })
	if err != nil {
		return fmt.Errorf("PTRACE_SINGLESTEP tid %d: %w", tid, err)
	}
	return nil
}

// StopProcess sends a whole-thread-group SIGSTOP, mirroring Delve's
// requestManualStop-adjacent halt primitive (pkg/proc/native/proc_linux.go
// sends a process-wide signal rather than per-thread, since a ptrace-stopped
// tracee still delivers signals to the group). This is Pause groundwork
// only: it does not implement Delve's trapWaitInternal halt-flag state
// machine that distinguishes a manual stop from a spontaneous trap, so no
// Pause command is wired to it yet — see AGENTS.md. The signal mechanics
// (syscall.Kill, ESRCH-as-idempotent) are shared with the darwin backend via
// stopProcessSignal in process.go.
func (b *linuxBackend) StopProcess() error {
	return stopProcessSignal(b.pid)
}

func (b *linuxBackend) ReadMemory(addr uint64, dst []byte) error {
	tid := b.traceTID()
	var n int
	var err error
	b.execPtrace(func() { n, err = syscall.PtracePeekData(tid, uintptr(addr), dst) })
	if err != nil {
		return fmt.Errorf("PTRACE_PEEKDATA tid %d 0x%x: %w", tid, addr, err)
	}
	if n != len(dst) {
		return fmt.Errorf("PTRACE_PEEKDATA tid %d 0x%x: short read %d/%d", tid, addr, n, len(dst))
	}
	return nil
}

func (b *linuxBackend) WriteMemory(addr uint64, src []byte) error {
	tid := b.traceTID()
	var n int
	var err error
	b.execPtrace(func() { n, err = syscall.PtracePokeData(tid, uintptr(addr), src) })
	if err != nil {
		return fmt.Errorf("PTRACE_POKEDATA tid %d 0x%x: %w", tid, addr, err)
	}
	if n != len(src) {
		return fmt.Errorf("PTRACE_POKEDATA tid %d 0x%x: short write %d/%d", tid, addr, n, len(src))
	}
	return nil
}

// GetRegisters reads PTRACE_GETREGS. The Go runtime stores g at FS_BASE on amd64.
func (b *linuxBackend) GetRegisters(tid int) (Registers, error) {
	var r syscall.PtraceRegs
	var err error
	b.execPtrace(func() { err = syscall.PtraceGetRegs(tid, &r) })
	if err != nil {
		return Registers{}, fmt.Errorf("PTRACE_GETREGS tid %d: %w", tid, err)
	}
	return Registers{
		PC:  r.Rip,
		SP:  r.Rsp,
		BP:  r.Rbp,
		TLS: r.Fs_base,
	}, nil
}

// SetRegisters writes back the engine-owned fields, preserving everything else
// by reading the full register set first.
func (b *linuxBackend) SetRegisters(tid int, reg Registers) error {
	var r syscall.PtraceRegs
	var getErr, setErr error
	b.execPtrace(func() {
		if getErr = syscall.PtraceGetRegs(tid, &r); getErr != nil {
			return
		}
		r.Rip = reg.PC
		r.Rsp = reg.SP
		r.Rbp = reg.BP
		r.Fs_base = reg.TLS
		setErr = syscall.PtraceSetRegs(tid, &r)
	})
	if getErr != nil {
		return fmt.Errorf("PTRACE_GETREGS (pre-set) tid %d: %w", tid, getErr)
	}
	if setErr != nil {
		return fmt.Errorf("PTRACE_SETREGS tid %d: %w", tid, setErr)
	}
	return nil
}

func (b *linuxBackend) Threads() ([]int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", b.pid))
	if err != nil {
		return nil, fmt.Errorf("read /proc/%d/task: %w", b.pid, err)
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		var tid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &tid); err == nil {
			tids = append(tids, tid)
		}
	}
	if len(tids) == 0 {
		return nil, fmt.Errorf("no threads for pid %d", b.pid)
	}
	return tids, nil
}

// Wait blocks until the tracee produces a meaningful debug stop. Single-step
// vs breakpoint disambiguation uses b.stepping AND b.stepTID: only a cause==0
// SIGTRAP on the exact thread we stepped is the step's completion; the same
// stop on any other thread is that thread hitting a software breakpoint.
// PTRACE_EVENT stops (clone/exec/exit) are handled internally and don't
// surface to the engine.
//
// wait4 runs on the calling (waitLoop) thread, NOT the tracer thread: waiting
// for a tracee is legal from any thread of the tracer process, and keeping it
// off the tracer thread lets the engine issue control ops concurrently. Every
// ptrace CONTROL op below, however, is funnelled through b.execPtrace so it
// executes on the one thread the kernel accepts ptrace requests from.
//
//nolint:gocognit,gocyclo // The wait loop is one serialized ptrace state machine.
func (b *linuxBackend) Wait() (StopEvent, error) {
	for {
		var ws syscall.WaitStatus
		// WALL includes clone()d threads.
		tid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		if err != nil {
			if isNoChildProcess(err) {
				return StopEvent{Reason: StopExited, TID: b.pid}, nil
			}
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}

		if ws.Exited() {
			if tid == b.pid {
				b.recordStop(tid)
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, nil
			}
			continue
		}

		if ws.Signaled() {
			if tid != b.pid {
				continue
			}
			b.recordStop(tid)
			return StopEvent{Reason: StopKilled, TID: tid}, nil
		}

		if !ws.Stopped() {
			continue
		}

		sig := ws.StopSignal()

		// PTRACE_EVENT stops are encoded as SIGTRAP | (event << 8).
		if sig == syscall.SIGTRAP {
			cause := ws.TrapCause()

			switch cause {
			case syscall.PTRACE_EVENT_CLONE:
				if err := b.continueIfTraceeExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT clone parent tid %d: %w", tid, err)
				}
				continue

			case syscall.PTRACE_EVENT_EXIT:
				if tid != b.pid {
					if err := b.continueIfTraceeExists(tid, 0); err != nil {
						return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting thread tid %d: %w", tid, err)
					}
					continue
				}
				// Main thread is about to call exit_group — let it actually exit.
				if err := b.continueIfTraceeExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting process tid %d: %w", tid, err)
				}
				b.recordStop(tid)
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: 0}, nil

			case syscall.PTRACE_EVENT_EXEC:
				if err := b.continueIfTraceeExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exec tid %d: %w", tid, err)
				}
				continue

			case 0:
				b.recordStop(tid)

				// Only the exact thread we single-stepped produces a
				// single-step SIGTRAP. A cause==0 SIGTRAP on any OTHER thread
				// while a step is in flight is that thread hitting a software
				// breakpoint (INT3), not the step completing — classify it as a
				// breakpoint so the engine's step-over state machine isn't fed a
				// bogus StopSingleStep for the wrong thread.
				if b.stepping && tid == b.stepTID {
					b.stepping = false
					b.stepTID = 0
					return StopEvent{Reason: StopSingleStep, TID: tid}, nil
				}

				return StopEvent{
					Reason: StopBreakpoint,
					TID:    tid,
				}, nil

			default:
				if err := b.continueIfTraceeExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT trap cause %d tid %d: %w", cause, tid, err)
				}
				continue
			}
		}

		if sig == syscall.SIGSTOP && tid != b.pid {
			// A newly cloned thread's initial group-stop. With
			// PTRACE_O_TRACECLONE the kernel auto-attaches it and it inherits
			// our ptrace options, so we just resume THIS thread. Crucially we
			// must NOT touch the rest of the group: another thread may be
			// stopped at a breakpoint waiting for the engine, and a
			// group-continue here would let it run away (the exact "parking the
			// thread group" hazard that kept clone tracing disabled before).
			if err := b.continueIfTraceeExists(tid, 0); err != nil {
				return StopEvent{}, fmt.Errorf("PTRACE_CONT new thread tid %d: %w", tid, err)
			}
			continue
		}

		// SIGURG is Go's goroutine-preemption signal; it must be re-delivered
		// transparently during both step and continue or scheduling breaks.
		if sig == syscall.SIGURG {
			// Re-issue the single-step only for the thread actually being
			// stepped; a SIGURG on any other thread must be re-delivered and
			// the thread continued, never single-stepped.
			if b.stepping && tid == b.stepTID {
				if err := b.singleStepIfTraceeExists(tid); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_SINGLESTEP after SIGURG tid %d: %w", tid, err)
				}
			} else {
				if err := b.continueIfTraceeExists(tid, int(sig)); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT SIGURG tid %d: %w", tid, err)
				}
			}
			continue
		}

		if sig == syscall.SIGCONT {
			if err := b.continueIfTraceeExists(tid, 0); err != nil {
				return StopEvent{}, fmt.Errorf("PTRACE_CONT SIGCONT tid %d: %w", tid, err)
			}
			continue
		}

		b.recordStop(tid)
		return StopEvent{
			Reason: StopSignal,
			TID:    tid,
			Signal: int(sig),
		}, nil
	}
}

var _ Backend = (*linuxBackend)(nil)

func (b *linuxBackend) setPID(pid int) {
	b.pid = pid
	b.lastStopTID = pid
}

func (b *linuxBackend) traceTID() int {
	if b.lastStopTID != 0 {
		return b.lastStopTID
	}
	return b.pid
}

func (b *linuxBackend) recordStop(tid int) {
	if tid != 0 {
		b.lastStopTID = tid
	}
}

func isNoSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrNotExist)
}

func isNoChildProcess(err error) bool {
	return errors.Is(err, syscall.ECHILD)
}

func (b *linuxBackend) continueIfTraceeExists(tid int, signal int) error {
	if tid == 0 {
		return nil
	}
	var err error
	b.execPtrace(func() { err = syscall.PtraceCont(tid, signal) })
	if err != nil && !isNoSuchProcess(err) {
		return err
	}
	return nil
}

func (b *linuxBackend) singleStepIfTraceeExists(tid int) error {
	if tid == 0 {
		return nil
	}
	var err error
	b.execPtrace(func() { err = syscall.PtraceSingleStep(tid) })
	if err != nil && !isNoSuchProcess(err) {
		return err
	}
	return nil
}
