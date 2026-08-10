//go:build linux && amd64

package debugger

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
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
	// stepQueue carries the single-step bookkeeping (stepping/stepTID) and the
	// queue of stops held back while a step is in flight. Embedded so those
	// fields read as b.stepping / b.stepTID at every existing use site.
	stepQueue

	pid    int
	tracer *tracerThread

	// Wait writes this from a waitLoop while running engine commands may read it
	// for TID-less memory operations. Atomicity defines that snapshot; it does
	// not make the selected thread stopped or suppress normal ptrace errors.
	lastStopTID atomic.Int64

	// waitFn/contFn/stepFn are the three kernel calls the wait loop makes. Nil
	// means the real syscall; only tests set them, and they exist because
	// nothing else can reach the loop's branch bodies.
	//
	// Which primitive a branch resumes with is the whole subject of rule 7: a
	// branch that plain-continues the stepped thread cancels its hardware step
	// while leaving stepping/stepTID latched, so every later foreign stop is
	// held forever and Wait never returns again. That decision is pure and
	// table-tested in planAbsorb, but the seven call sites choosing to consult
	// it are not — with a live tracee as the only way in, turning any of them
	// back into a bare PTRACE_CONT passed the entire unit suite. Scripting the
	// wait statuses is what makes those branches executable, so the mutation
	// fails a test instead of shipping a freeze.
	waitFn func(ws *syscall.WaitStatus) (int, error)
	contFn func(tid int, signal int) error
	stepFn func(tid int) error
}

// waitAny blocks for the next stop from any thread of the tracee. WALL includes
// clone()d threads.
func (b *linuxBackend) waitAny(ws *syscall.WaitStatus) (int, error) {
	if b.waitFn != nil {
		return b.waitFn(ws)
	}
	return syscall.Wait4(-1, ws, syscall.WALL, nil)
}

// drainParked returns the next held stop if one may be surfaced now, recording
// it as the current stop.
//
// lastStopTID is updated here, on delivery, and never at park time — from this
// point the delivered thread is the one the engine acts on, and the one the
// next TID-less ContinueProcess and memory write will target.
func (b *linuxBackend) drainParked() (StopEvent, bool) {
	ev, ok := b.releasable()
	if !ok {
		return StopEvent{}, false
	}
	b.recordStop(ev.TID)
	return ev, true
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

// killProcess terminates a launched tracee (SIGKILL) or detaches from an
// attached one. running reports whether the engine's waitLoop is in flight —
// true for a running tracee, false for one suspended at a stop.
func killProcess(b Backend, pid int, cmd *exec.Cmd, running bool) error {
	if cmd != nil {
		// SIGKILL via the OS handle is not a ptrace op, so it is safe from any
		// thread and keeps Kill responsive even if the tracer thread is busy.
		if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
			return err
		}
		// Reaping the zombie belongs to the waitLoop whenever one is in flight
		// (a running tracee). It is blocked in Wait4(-1, WALL) and will absorb
		// every thread's SIGKILL death and surface StopKilled. A second reaper
		// here would both (a) race that waitLoop for the same stops and (b) —
		// since a Go tracee is always multi-threaded — never make progress:
		// Wait4(pid) targets only the thread-group leader, whose zombie is not
		// reapable until all siblings are, and killProcess cannot reach the
		// siblings. Either way Kill wedges (the deadlock reported in #111). So
		// only reap here when there is NO waitLoop — the tracee was suspended at
		// a stop, making killProcess the sole reaper.
		if !running {
			if lb, ok := b.(*linuxBackend); ok {
				lb.reapAfterKill()
			}
		}
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

// reapAfterKill drains a SIGKILL'd tracee that has no waitLoop to reap it (it
// was suspended at a ptrace stop when killed). It waits on Wait4(-1) — any
// thread — never Wait4(pid): a Go tracee is always multi-threaded and the
// thread-group leader's zombie stays unreapable until every sibling thread is
// reaped, so waiting on the leader's pid alone blocks forever. A thread frozen
// at a ptrace stop (e.g. the breakpoint we were suspended at) will not proceed
// to death until resumed, so continue any stopped thread before waiting again;
// the process-wide SIGKILL then kills it. Returns once ECHILD reports the whole
// thread group gone.
//
// It must never run concurrently with the engine's waitLoop: two Wait4(-1)
// callers would steal each other's stops. killProcess guarantees that by only
// invoking it when the tracee was not running (no waitLoop in flight).
func (b *linuxBackend) reapAfterKill() {
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		switch {
		case err == nil:
			if ws.Stopped() {
				_ = b.continueIfTraceeExists(wpid, 0)
			}
			// Exited/Signaled: that thread is reaped; loop for the rest.
		case isNoChildProcess(err):
			return // whole thread group reaped
		case errors.Is(err, syscall.EINTR):
			// interrupted by a signal; retry
		default:
			return // unexpected wait4 error: nothing left to reap
		}
	}
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

func (b *linuxBackend) ContinueProcess() error {
	b.endStep()
	tid := b.traceTID()
	var err error
	b.execPtrace(func() { err = syscall.PtraceCont(tid, 0) })
	if err != nil {
		return fmt.Errorf("PTRACE_CONT tid %d: %w", tid, err)
	}
	return nil
}

func (b *linuxBackend) SingleStep(tid int) error {
	b.beginStep(tid)
	var err error
	b.execPtrace(func() { err = syscall.PtraceSingleStep(tid) })
	if err != nil {
		return fmt.Errorf("PTRACE_SINGLESTEP tid %d: %w", tid, err)
	}
	return nil
}

// StopProcess asynchronously interrupts the running tracee for Pause. It
// directs SIGSTOP at the MAIN thread specifically (tgkill(pid, pid, SIGSTOP))
// rather than the whole thread group (kill(pid, ...)). A process-directed
// SIGSTOP may be dequeued by any thread, and Wait() deliberately swallows a
// non-main thread's SIGSTOP as a clone group-stop (the sig==SIGSTOP &&
// tid!=b.pid branch), so on a multithreaded target a group-directed Pause
// could be lost. Targeting the main thread (whose TID equals the tgid) makes
// the signal surface from Wait() as StopEvent{StopSignal, SIGSTOP} with
// TID==b.pid, where the engine's manual-stop detection turns it into
// EventPaused. The engine never injects this SIGSTOP back (Continue resumes
// with signal 0), so it triggers no group-stop and resume is a plain
// ContinueProcess. ESRCH (thread already gone) is an idempotent no-op,
// matching process.kill. tgkill is a plain signal syscall,
// not a ptrace op, so it need not run on the tracer thread.
func (b *linuxBackend) StopProcess() error {
	if b.pid == 0 {
		return fmt.Errorf("StopProcess: no process")
	}
	if err := syscall.Tgkill(b.pid, b.pid, syscall.SIGSTOP); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("StopProcess: %w", err)
	}
	return nil
}

// PauseSignal is SIGSTOP: the signal StopProcess directs at the main thread and
// that the engine turns into EventPaused. See Backend.PauseSignal.
func (b *linuxBackend) PauseSignal() int { return int(syscall.SIGSTOP) }

// ReadMemory bulk-copies the tracee's address space. process_vm_readv(2) is the
// fast path: a single syscall for the whole buffer that — unlike ptrace(2) — is
// NOT thread-bound, so it runs directly off the calling goroutine without the
// tracer-thread handoff and never word-at-a-times. This is what keeps the
// goroutine snapshot (dozens of small reads per stop, across every live
// goroutine) cheap; the old PTRACE_PEEKDATA-through-execPtrace path was orders
// of magnitude slower and pushed the churn e2e past its target's watchdog.
// PTRACE_PEEKDATA remains the fallback for the rare case process_vm_readv is
// unavailable (old kernel) or short-reads.
func (b *linuxBackend) ReadMemory(addr uint64, dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	if b.pid > 0 {
		local := []unix.Iovec{{Base: &dst[0], Len: uint64(len(dst))}}
		remote := []unix.RemoteIovec{{Base: uintptr(addr), Len: len(dst)}}
		if n, err := unix.ProcessVMReadv(b.pid, local, remote, 0); err == nil && n == len(dst) {
			return nil
		}
	}
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

// Wait blocks until the tracee produces a meaningful debug stop.
//
// Single-step vs breakpoint disambiguation uses b.stepping AND b.stepTID: only
// a cause==0 SIGTRAP on the exact thread we stepped is the step's completion.
// PTRACE_EVENT stops (clone/exec/exit) are handled internally and don't surface
// to the engine.
//
// While a single-step is in flight, a user-visible stop on ANY OTHER thread is
// PARKED rather than returned (classifyUserStop), and delivered from a later
// Wait once no step is outstanding. This preserves the invariant the engine has
// always been written against but could not previously rely on: the TID of the
// stop it is handed is b.lastStopTID, which is the thread the next
// (TID-less) ContinueProcess and the next memory write will target. Returning a
// sibling's stop mid-step broke that — the engine acted on the sibling while the
// backend was still pointed at the stepped thread, losing the breakpoint being
// stepped over (issue #199).
//
// This is NOT an atomic stop-the-world step-over. Threads that are not stopped
// keep running for the duration of the step and may stream past the temporarily
// disarmed trap without trapping at all; parking only governs stops the kernel
// actually reported. Parked threads stay ptrace-stopped until their stop is
// delivered, so a thread can hold at most one entry and the queue is bounded by
// the live thread count.
//
// The queue needs no lock. It is touched only here, and successive Wait calls
// run on different one-shot waitLoop goroutines that the engine starts with a
// `go` statement after consuming the previous Wait's result — so each Wait
// happens-after the previous one, along with the step-state writes the engine
// makes in between.
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
		// A parked stop may only be surfaced once no step is outstanding.
		// Draining here — before blocking in wait4 — is what makes the parked
		// thread the current stop as soon as the step that displaced it has
		// completed and the engine has reinstalled the trap it stepped off.
		if ev, ok := b.drainParked(); ok {
			return ev, nil
		}

		var ws syscall.WaitStatus
		tid, err := b.waitAny(&ws)
		if err != nil {
			if isNoChildProcess(err) {
				b.purge()
				return StopEvent{Reason: StopExited, TID: b.pid}, nil
			}
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}

		if ws.Exited() {
			if tid == b.pid {
				b.purge()
				b.recordStop(tid)
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, nil
			}
			b.clearStepIfStepped(tid)
			continue
		}

		if ws.Signaled() {
			if tid != b.pid {
				b.clearStepIfStepped(tid)
				continue
			}
			b.purge()
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
				if err := b.absorbStop(absorbClone, tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("resume clone parent tid %d: %w", tid, err)
				}
				continue

			case syscall.PTRACE_EVENT_EXIT:
				if tid != b.pid {
					if err := b.absorbStop(absorbThreadExit, tid, 0); err != nil {
						return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting thread tid %d: %w", tid, err)
					}
					continue
				}
				// Main thread is about to exit: nothing parked can ever be
				// delivered, and acting on it would target a dead thread.
				b.purge()
				// Main thread is about to exit. PTRACE_O_TRACEEXIT stops it here
				// BEFORE it dies, and the engine tears down on this StopExited, so
				// the real status never resurfaces as a later wait4 Exited()/
				// Signaled(). Read it now via PTRACE_GETEVENTMSG (a wait(2)-encoded
				// status) so a non-zero exit or a fatal signal isn't misreported as
				// a clean exit 0. GETEVENTMSG must run before we resume the thread —
				// once continued it is gone and the message is unreadable.
				var msg uint
				var msgErr error
				b.execPtrace(func() { msg, msgErr = syscall.PtraceGetEventMsg(tid) })
				if err := b.continueIfTraceeExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting process tid %d: %w", tid, err)
				}
				b.recordStop(tid)
				if msgErr == nil {
					status := syscall.WaitStatus(msg)
					switch {
					case status.Signaled():
						return StopEvent{Reason: StopKilled, TID: tid}, nil
					case status.Exited():
						return StopEvent{Reason: StopExited, TID: tid, ExitCode: status.ExitStatus()}, nil
					}
				}
				// Status unreadable (e.g. ESRCH racing a Kill) or unexpected shape:
				// fall back to a clean exit rather than inventing a code.
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: 0}, nil

			case syscall.PTRACE_EVENT_EXEC:
				// execve replaces the image the stepped-over breakpoint and its
				// saved instruction bytes belong to, so the step can be neither
				// re-armed nor completed. Nothing can restore the old trap
				// because the memory it lived in is gone. Fail the wait instead:
				// the engine reports it and tears the session down cleanly.
				if plan := b.planAbsorb(absorbExec, tid, 0); plan.fail {
					b.abortStep()
					return StopEvent{}, fmt.Errorf("tracee exec'd on tid %d while it was being single-stepped: the stepped-over breakpoint cannot be restored in the new image", tid)
				} else if err := b.applyAbsorb(plan, tid); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exec tid %d: %w", tid, err)
				}
				continue

			case 0:
				reason, disp := classifyUserStop(true, b.stepping, b.stepTID, tid)
				if disp == parkStop {
					// A sibling hit a software breakpoint while another thread
					// is mid-step. It stays ptrace-stopped where it is; the
					// engine sees it only once the step has completed and the
					// trap being stepped over is back in place.
					b.park(StopEvent{Reason: reason, TID: tid})
					continue
				}
				b.recordStop(tid)
				if reason == StopSingleStep {
					b.endStep()
				}
				return StopEvent{Reason: reason, TID: tid}, nil

			default:
				// An unrecognised PTRACE_EVENT. We enable only TRACEEXIT,
				// TRACEEXEC and TRACECLONE, so this is unreachable in practice
				// and there is nothing to reason about if it is reached. On the
				// stepped thread that leaves no safe move: continuing cancels
				// the step with the trap still out of memory, and re-arming
				// assumes a stop shape we do not understand. Fail loudly rather
				// than guess.
				if plan := b.planAbsorb(absorbUnknownEvent, tid, 0); plan.fail {
					b.abortStep()
					return StopEvent{}, fmt.Errorf("unhandled ptrace event %d on tid %d while it was being single-stepped", cause, tid)
				} else if err := b.applyAbsorb(plan, tid); err != nil {
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
			// A brand-new thread is never the one being stepped, but route the
			// resume through the same helper so no absorb site can drift back
			// into an unguarded continue.
			if err := b.absorbStop(absorbNewThread, tid, 0); err != nil {
				return StopEvent{}, fmt.Errorf("resume new thread tid %d: %w", tid, err)
			}
			continue
		}

		// SIGURG is Go's goroutine-preemption signal; it is re-delivered on the
		// continue path so scheduling keeps working. The one thread that does
		// not get it back is the one being single-stepped, where delivering it
		// would step the handler rather than the instruction under the step —
		// see stepQueue.planAbsorb for why that is safe.
		if sig == syscall.SIGURG {
			if err := b.absorbStop(absorbPreempt, tid, int(sig)); err != nil {
				return StopEvent{}, fmt.Errorf("resume after SIGURG tid %d: %w", tid, err)
			}
			continue
		}

		if sig == syscall.SIGCONT {
			if err := b.absorbStop(absorbContinued, tid, 0); err != nil {
				return StopEvent{}, fmt.Errorf("resume after SIGCONT tid %d: %w", tid, err)
			}
			continue
		}

		reason, disp := classifyUserStop(false, b.stepping, b.stepTID, tid)
		if disp == parkStop {
			b.park(StopEvent{Reason: reason, TID: tid, Signal: int(sig)})
			continue
		}
		b.recordStop(tid)
		return StopEvent{
			Reason: reason,
			TID:    tid,
			Signal: int(sig),
		}, nil
	}
}

var _ Backend = (*linuxBackend)(nil)

func (b *linuxBackend) setPID(pid int) {
	b.pid = pid
	b.lastStopTID.Store(int64(pid))
}

func (b *linuxBackend) traceTID() int {
	if tid := b.lastStopTID.Load(); tid != 0 {
		return int(tid)
	}
	return b.pid
}

func (b *linuxBackend) recordStop(tid int) {
	if tid != 0 {
		b.lastStopTID.Store(int64(tid))
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
	if b.contFn != nil {
		err = b.contFn(tid, signal)
	} else {
		b.execPtrace(func() { err = syscall.PtraceCont(tid, signal) })
	}
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
	if b.stepFn != nil {
		err = b.stepFn(tid)
	} else {
		b.execPtrace(func() { err = syscall.PtraceSingleStep(tid) })
	}
	if err != nil && !isNoSuchProcess(err) {
		return err
	}
	return nil
}

// absorbStop resumes a thread whose stop Wait handled internally instead of
// reporting it. Every absorb site must go through here: the kernel delivers one
// stop per resume, so absorbing an event on the thread the engine is stepping
// consumes that step, and a plain continue there would silently cancel it while
// the step gate stays latched. See stepQueue.planAbsorb.
//
// Callers handle plan.fail themselves, because each failing branch has its own
// diagnosis to report.
func (b *linuxBackend) absorbStop(kind absorbKind, tid int, signal int) error {
	return b.applyAbsorb(b.planAbsorb(kind, tid, signal), tid)
}

// applyAbsorb carries out an absorbPlan. The gate is released before the resume
// so an erroring resume cannot leave the step latched behind it.
func (b *linuxBackend) applyAbsorb(plan absorbPlan, tid int) error {
	if plan.clearStep {
		b.clearStepIfStepped(tid)
	}
	if plan.mode == resumeSingleStep {
		b.countStepRearm()
		return b.singleStepIfTraceeExists(tid)
	}
	return b.continueIfTraceeExists(tid, plan.signal)
}
