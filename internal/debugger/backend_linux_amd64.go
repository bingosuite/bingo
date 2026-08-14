//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func newBackend() Backend {
	return &linuxBackend{
		tracer: newTracerThread(),
		waits:  processLinuxWaitBroker.newOwner(),
	}
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
	tid    atomic.Int64
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
	t.tid.Store(int64(unix.Gettid()))
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

func (t *tracerThread) threadID() int {
	return int(t.tid.Load())
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

func (t *tracerThread) closed() bool {
	select {
	case <-t.quit:
		return true
	default:
		return false
	}
}

type linuxBackend struct {
	// stepQueue carries the single-step bookkeeping (stepping/stepTID) and the
	// queue of stops held back while a step is in flight. Embedded so those
	// fields read as b.stepping / b.stepTID at every existing use site.
	stepQueue

	pid    int
	tracer *tracerThread
	waits  linuxWaitSource

	// attachedTracees is non-nil only for a process bingo attached to rather
	// than launched. Access is serialized by the engine waiter handoff: normal
	// Wait owns it while a waiter is active, and attached teardown cancels and
	// joins that waiter before the engine loop takes ownership.
	attachedTracees map[int]*linuxTracee
	attachCleanup   bool
	attachGone      bool
	attachImageGone bool

	// Wait writes this from a waitLoop while running engine commands may read it
	// for TID-less memory operations. Atomicity defines that snapshot; it does
	// not make the selected thread stopped or suppress normal ptrace errors.
	lastStopTID atomic.Int64

	// leaderExitStatus/leaderExitStashed hold the wait(2)-encoded status read at
	// the thread-group leader's PTRACE_EVENT_EXIT, which is EVIDENCE of a
	// terminal rather than proof of one.
	//
	// de_thread() retires the old leader when a NON-leader calls execve(): the
	// execing thread takes over the leader's pid and the retired task's exit is
	// reported as EVENT_EXIT under tid == pid while the process lives on. A
	// leader that merely pthread_exit()s produces the same shape. In both cases
	// the decoded status is an ordinary "exited, code 0", indistinguishable from
	// a genuine clean exit — verified against Linux 6.10 — so the terminal can
	// only be committed once the whole group is actually gone, and must be
	// retracted when a later exec proves retirement.
	//
	// Touched only inside Wait, like the park queue, so it needs no lock.
	leaderExitStatus  uint
	leaderExitStashed bool

	// waitFn/contFn/stepFn/eventMsgFn are the four kernel calls the wait loop
	// makes. Nil
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
	waitFn         func(ws *syscall.WaitStatus) (int, error)
	contFn         func(tid int, signal int) error
	stepFn         func(tid int) error
	eventMsgFn     func(tid int) (uint, error)
	pendingSignals pendingSignals

	// Nil in production; tests inject this at the raw syscall seam to pin the
	// exact ptrace request, TID, and signal without launching a tracee.
	ptraceSyscall6Fn func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno)
	tgkillFn         func(tgid, tid int, signal syscall.Signal) error
	threadsFn        func() ([]int, error)
	processExistsFn  func() bool
	tidExistsFn      func(int) bool
	tracerPIDFn      func(int) (int, error)
}

// eventMsg reads the message the kernel attached to a PTRACE_EVENT stop: the
// wait(2)-encoded status at EVENT_EXIT, the execing thread's former tid at
// EVENT_EXEC. Like the other kernel calls it is seam-able so the wait loop's
// branches can be driven without a tracee.
func (b *linuxBackend) eventMsg(tid int) (uint, error) {
	if b.eventMsgFn != nil {
		return b.eventMsgFn(tid)
	}
	var msg uint
	var err error
	b.execPtrace(func() { msg, err = syscall.PtraceGetEventMsg(tid) })
	return msg, err
}

// waitAny receives the next status owned by this debugger. Production statuses
// come from the process-global exact-TID broker; tests can still script the
// serialized Wait state machine directly.
func (b *linuxBackend) waitAny(ctx context.Context) (linuxWaitResult, error) {
	if b.waitFn != nil {
		var ws syscall.WaitStatus
		tid, err := b.waitFn(&ws)
		return linuxWaitResult{tid: tid, status: ws}, err
	}
	if b.waits == nil {
		return linuxWaitResult{}, syscall.ECHILD
	}
	return b.waits.next(ctx)
}

// drainParked returns the next held stop if one may be surfaced now, installing
// its current-TID and pending-signal state together.
//
// lastStopTID is updated here, on delivery, and never at park time — from this
// point the delivered thread is the one the engine acts on, and the one the
// next TID-less ContinueProcess and memory write will target.
func (b *linuxBackend) drainParked() (StopEvent, bool) {
	ev, ok := b.releasable()
	if !ok {
		return StopEvent{}, false
	}
	b.recordDeliveredStop(ev)
	return ev, true
}

// completeStepThreadExit is called by the engine only after Wait returned the
// internal death boundary AND the engine has reinstalled the breakpoint that
// boundary exists to protect, so no Wait call overlaps this gate transition.
//
// Releasing the anchor is the last step, and only ever the step owner we held
// ourselves: a parked sibling lent its TID without being dequeued, so resuming
// it would run a thread whose stop the engine has not been told about. The
// release itself runs through the tracer thread like every other ptrace op.
// ESRCH/ENOENT is benign — the thread was mid-exit and simply finished dying —
// and continueWithoutPendingSignals already treats it as success.
//
// A non-benign failure leaves the gate CLOSED and the hold in place. Nothing may
// drain past a thread we could not release, and reporting the error lets the
// engine halt suspended rather than resume into a state it cannot describe.
func (b *linuxBackend) completeStepThreadExit() error {
	if tid, ok := b.heldStepOwner(); ok {
		if err := b.continueWithoutPendingSignals(tid); err != nil {
			return fmt.Errorf("release step owner tid %d held to reconcile its exit: %w", tid, err)
		}
		b.clearHeldStepOwner()
	}
	b.stepQueue.completeStepThreadExit()
	return nil
}

func (b *linuxBackend) execPtrace(fn func()) { b.tracer.execPtrace(fn) }

// noteLeaderExit records the status read at the leader's PTRACE_EVENT_EXIT
// without committing to a terminal. See the field doc for why that stop is not
// proof the process is dying.
func (b *linuxBackend) noteLeaderExit(status uint, ok bool) {
	b.leaderExitStatus = status
	b.leaderExitStashed = ok
}

// retractLeaderExit discards a stashed leader exit because an exec proved the
// leader was retired by de_thread() rather than the process dying. It reports
// whether anything was actually retracted, purely so the exec diagnostic can say
// so.
func (b *linuxBackend) retractLeaderExit() bool {
	had := b.leaderExitStashed
	b.leaderExitStashed = false
	b.leaderExitStatus = 0
	return had
}

// commitLeaderExit turns a confirmed group death into the terminal stop.
//
// The status wait4 actually reported wins whenever there is one: the
// thread-group leader's wait status carries the GROUP exit code, whereas the
// PTRACE_EVENT_EXIT payload is only that TASK's do_exit value. Those differ
// exactly when the leader dies separately from the group — a leader that
// pthread_exit()s with 0 while a surviving worker later calls exit_group(7)
// must report 7, not 0.
//
// The stash is the fallback for the one case with no observed status at all:
// ECHILD, where the group is gone and nothing is left to ask. Reading it at the
// EVENT_EXIT stop remains mandatory (#94) because that is the only moment it can
// be read, and it is still what keeps a non-zero exit or a fatal signal from
// being reported as a clean 0 on that path.
func (b *linuxBackend) commitLeaderExit(observed StopEvent, haveObserved bool) StopEvent {
	stashed := b.leaderExitStashed
	status := syscall.WaitStatus(b.leaderExitStatus)
	b.leaderExitStashed = false
	b.leaderExitStatus = 0

	if haveObserved {
		return observed
	}
	if stashed {
		switch {
		case status.Signaled():
			return StopEvent{Reason: StopKilled, TID: b.pid}
		case status.Exited():
			return StopEvent{Reason: StopExited, TID: b.pid, ExitCode: status.ExitStatus()}
		}
	}
	return observed
}

// leaderExitPendingForTest reports whether a terminal is being withheld.
func (b *linuxBackend) leaderExitPendingForTest() bool { return b.leaderExitStashed }

// closeTracer releases the dedicated tracer thread after engine shutdown.
// A terminal waitLoop may still be unwinding, so its lock-free step queue
// remains Wait-owned; only synchronized pending-signal state is cleared here.
func (b *linuxBackend) closeTracer() {
	b.pendingSignals.purge()
	b.tracer.close()
	if b.waits != nil {
		b.waits.close()
	}
}

const linuxPtraceOptions = syscall.PTRACE_O_TRACEEXIT |
	syscall.PTRACE_O_TRACEEXEC |
	syscall.PTRACE_O_TRACECLONE

// startTracedProcess forks under ptrace. The child is stopped at its first
// instruction (execve SIGTRAP) ready for the engine to set breakpoints. The
// fork+exec and PTRACE_SETOPTIONS run on the backend's dedicated tracer thread:
// the forking thread becomes the tracee's tracer, so every later ptrace op must
// originate from that same thread. The initial status is consumed outside the
// tracer closure through the process-global wait broker; blocking the tracer
// thread on a wait would prevent every later control operation.
func startTracedProcess(b Backend, binaryPath string, args []string, env []string) (retPID int, retCmd *exec.Cmd, retErr error) {
	lb, ok := b.(*linuxBackend)
	if !ok || lb.tracer == nil || lb.waits == nil {
		return 0, nil, fmt.Errorf("startTracedProcess: backend does not support Linux wait ownership")
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
	lb.execPtrace(func() {
		if err := cmd.Start(); err != nil {
			startErr = fmt.Errorf("exec %q: %w", binaryPath, err)
		}
	})
	if startErr != nil {
		return 0, nil, startErr
	}

	pid := cmd.Process.Pid
	lb.setPID(pid)
	if _, err := lb.waits.register(pid); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("register initial tid %d: %w", pid, err)
	}
	defer func() {
		if retErr == nil {
			return
		}
		killErr := cmd.Process.Kill()
		if errors.Is(killErr, os.ErrProcessDone) || isNoSuchProcess(killErr) {
			killErr = nil
		}
		reapErr := lb.reapAfterKill()
		lb.setPID(0)
		if cleanupErr := errors.Join(killErr, reapErr); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("discard failed launch: %w", cleanupErr))
		}
	}()

	result, err := lb.waitAny(context.Background())
	if err != nil {
		return 0, nil, fmt.Errorf("wait for execve stop: %w", err)
	}
	ws := result.status
	if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
		return 0, nil, fmt.Errorf("unexpected initial stop: %v", ws)
	}

	lb.execPtrace(func() {
		if err := syscall.PtraceSetOptions(pid, linuxPtraceOptions); err != nil {
			startErr = fmt.Errorf("PTRACE_SETOPTIONS: %w", err)
		}
	})
	if startErr != nil {
		return 0, nil, startErr
	}
	return pid, cmd, nil
}

// killProcess terminates a launched tracee (SIGKILL) or detaches from an
// attached one. running reports whether the engine's waitLoop is in flight —
// true for a running tracee, false for one suspended at a stop.
func killProcess(b Backend, pid int, cmd *exec.Cmd, running bool) error {
	if lb, ok := b.(*linuxBackend); ok && (cmd != nil || !lb.attached()) {
		// stepQueue stays wait-loop-owned while a running tracee still has a
		// waitLoop in flight; only the synchronized signal state is safe to
		// clear from the engine goroutine.
		lb.pendingSignals.purge()
	}
	if cmd != nil {
		var cleanupErr error
		if lb, ok := b.(*linuxBackend); ok {
			if err := lb.registerThreadGroup(); err != nil && !isNoSuchProcess(err) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("register thread group before kill: %w", err))
			}
		}
		// SIGKILL via the OS handle is not a ptrace op, so it is safe from any
		// thread and keeps Kill responsive even if the tracer thread is busy.
		if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
			return errors.Join(cleanupErr, err)
		}
		if lb, ok := b.(*linuxBackend); ok {
			if err := lb.registerThreadGroup(); err != nil && !isNoSuchProcess(err) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("register thread group after kill: %w", err))
			}
		}
		// A running tracee already has an engine waitLoop consuming this
		// owner's routed queue. A suspended tracee does not, so Kill drains the
		// same queue itself until the broker retires every exact TID.
		if !running {
			if lb, ok := b.(*linuxBackend); ok {
				if err := lb.reapAfterKill(); err != nil {
					cleanupErr = errors.Join(cleanupErr, err)
				}
			}
		}
		return cleanupErr
	}
	if lb, ok := b.(*linuxBackend); ok {
		return lb.detachAttached()
	}
	return fmt.Errorf("attached detach is unsupported by this backend")
}

// reapAfterKill drains a SIGKILL'd tracee that has no engine waitLoop. Raw wait
// ownership remains with the broker; this consumer only resumes owned stopped
// TIDs until the broker has retired the exact registered set.
func (b *linuxBackend) reapAfterKill() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for {
		if b.waits == nil {
			return nil
		}
		result, err := b.waits.next(ctx)
		switch {
		case err == nil:
			if result.status.Stopped() {
				if result.status.StopSignal() == syscall.SIGTRAP &&
					result.status.TrapCause() == syscall.PTRACE_EVENT_CLONE {
					if err := b.registerClone(result.tid); err != nil {
						return err
					}
				}
				if err := b.continueWithoutPendingSignals(result.tid); err != nil {
					return fmt.Errorf("resume killed tid %d: %w", result.tid, err)
				}
			}
		case isNoChildProcess(err):
			return nil
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("timed out reaping killed process %d", b.pid)
		default:
			return fmt.Errorf("reap killed process %d: %w", b.pid, err)
		}
	}
}

func (b *linuxBackend) registerClone(parentTID int) error {
	if b.waits == nil {
		return nil
	}
	childTID, err := b.eventMsg(parentTID)
	if err != nil {
		return fmt.Errorf("%w: read cloned tid from parent %d: %v",
			ErrSessionInvalidated, parentTID, err)
	}
	generation, err := b.waits.register(int(childTID))
	if err != nil {
		return fmt.Errorf("%w: register cloned tid %d from parent %d: %v",
			ErrSessionInvalidated, childTID, parentTID, err)
	}
	b.registerAttachedClone(int(childTID), generation)
	return nil
}

func (b *linuxBackend) registerThreadGroup() error {
	if b.waits == nil || b.pid == 0 {
		return nil
	}
	tids, err := b.Threads()
	if err != nil {
		return err
	}
	for _, tid := range tids {
		generation, err := b.waits.register(tid)
		if err != nil {
			return err
		}
		if b.attached() && b.attachedTracees[tid] == nil {
			b.registerAttachedClone(tid, generation)
		}
	}
	return nil
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

func (b *linuxBackend) ContinueProcess() error {
	if b.stepExitPending {
		return fmt.Errorf("PTRACE_CONT: stepped-thread exit is awaiting breakpoint reconciliation; kill or restart to recover")
	}
	b.endStep()
	if b.attached() {
		return b.continueAttached()
	}
	tid := b.traceTID()
	return b.continueTID(tid)
}

func (b *linuxBackend) continueTID(tid int) error {
	pending := b.pendingSignals.takeForContinue(tid)
	requeued, err := b.requeueSignals(tid, pending.deferred)
	if err != nil {
		b.pendingSignals.restore(tid, pending.withoutDeferredPrefix(requeued))
		return err
	}
	b.execPtrace(func() { err = b.ptraceCont(tid, pending.current) })
	if err != nil {
		b.pendingSignals.restore(tid, pendingSignalBatch{current: pending.current})
		return fmt.Errorf("PTRACE_CONT tid %d signal %d: %w", tid, pending.current, err)
	}
	b.markAttachedRunning(tid)
	return nil
}

func (b *linuxBackend) SingleStep(tid int) error {
	if b.stepExitPending {
		return fmt.Errorf("PTRACE_SINGLESTEP: stepped-thread exit is awaiting breakpoint reconciliation; kill or restart to recover")
	}
	if b.attachCleanup {
		return fmt.Errorf("PTRACE_SINGLESTEP: attached detach cleanup is pending; retry Kill")
	}
	b.beginStep(tid)
	var err error
	b.execPtrace(func() { err = b.ptraceSingleStep(tid) })
	if err != nil {
		return fmt.Errorf("PTRACE_SINGLESTEP tid %d: %w", tid, err)
	}
	b.markAttachedRunning(tid)
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
// EventPaused. recordDeliveredStop excludes this SIGSTOP from pending delivery,
// so Continue resumes with signal 0; it triggers no group-stop and resume is a
// plain ContinueProcess. ESRCH (thread already gone) is an idempotent no-op,
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
	if b.threadsFn != nil {
		return b.threadsFn()
	}
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
// through startWait after consuming the previous result — so each Wait
// happens-after the previous one, along with the step-state writes the engine
// makes in between.
//
// A delivered StopSignal also installs pendingSignals[tid] before lastStopTID is
// published. The map is mutex-protected because Wait writes it and the engine
// consumes it on resume; keeping it per TID prevents another stop from stealing
// or clearing the signal.
//
// The process-global broker collects wait statuses without occupying the tracer
// thread, so the engine can issue control ops concurrently. Every ptrace CONTROL
// op below is funnelled through b.execPtrace and executes on the one thread the
// kernel accepts ptrace requests from.
func (b *linuxBackend) Wait() (StopEvent, error) {
	return b.wait(context.Background())
}

//nolint:gocognit,gocyclo // The wait loop is one serialized ptrace state machine.
func (b *linuxBackend) wait(ctx context.Context) (StopEvent, error) {
	for {
		// A stepped thread can die before reporting completion. Reconciling
		// that needs a genuinely ptrace-stopped TID for the engine to reinstall
		// the disarmed trap through: the dying owner itself when nothing is
		// parked (held at its exit stop for exactly this), otherwise the oldest
		// held stop, which lends its TID but stays queued.
		if ev, ok := b.stepExitBoundary(); ok {
			b.recordStop(ev.TID)
			return ev, nil
		}

		// A parked stop may only be surfaced once no step is outstanding.
		// Draining here — before blocking in wait4 — is what makes the parked
		// thread the current stop as soon as the step that displaced it has
		// completed and the engine has reinstalled the trap it stepped off.
		if ev, ok := b.drainParked(); ok {
			return ev, nil
		}

		result, err := b.waitAny(ctx)
		if err != nil {
			if isNoChildProcess(err) {
				b.purge()
				return b.commitLeaderExit(StopEvent{Reason: StopExited, TID: b.pid}, false), nil
			}
			if errors.Is(err, context.Canceled) {
				return StopEvent{}, err
			}
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}
		if result.retired {
			if err := b.recordAttachedRetirement(result.tid, result.generation); err != nil {
				return StopEvent{}, err
			}
			continue
		}
		tid := result.tid
		ws := result.status

		if ws.Exited() {
			if tid == b.pid {
				b.removeAttachedTracee(tid)
				b.purge()
				b.recordStop(tid)
				return b.commitLeaderExit(
					StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, true), nil
			}
			b.pendingSignals.clear(tid)
			b.removeAttachedTracee(tid)
			b.interruptStepIfStepped(tid)
			continue
		}

		if ws.Signaled() {
			if tid != b.pid {
				b.pendingSignals.clear(tid)
				b.removeAttachedTracee(tid)
				b.interruptStepIfStepped(tid)
				continue
			}
			b.removeAttachedTracee(tid)
			b.purge()
			b.recordStop(tid)
			return b.commitLeaderExit(StopEvent{Reason: StopKilled, TID: tid}, true), nil
		}

		if !ws.Stopped() {
			continue
		}

		sig := ws.StopSignal()
		if int(uint32(ws)>>16) == unix.PTRACE_EVENT_STOP && sig != syscall.SIGTRAP {
			event := StopEvent{Reason: StopSignal, TID: tid, Signal: int(sig)}
			b.markAttachedStopped(tid, event, false, int(sig), true)
			return event, nil
		}

		// PTRACE_EVENT stops are encoded as SIGTRAP | (event << 8).
		if sig == syscall.SIGTRAP {
			cause := ws.TrapCause()

			switch cause {
			case syscall.PTRACE_EVENT_CLONE:
				if err := b.registerClone(tid); err != nil {
					b.abortStepForWaitFailure()
					return StopEvent{}, err
				}
				if err := b.absorbStop(absorbClone, tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("resume clone parent tid %d: %w", tid, err)
				}
				continue

			case syscall.PTRACE_EVENT_EXIT:
				if tid != b.pid {
					b.pendingSignals.clear(tid)
					if err := b.absorbStop(absorbThreadExit, tid, 0); err != nil {
						return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting thread tid %d: %w", tid, err)
					}
					continue
				}
				// A stop under the LEADER's id is evidence of a terminal, never
				// proof of one, so it is stashed rather than acted on.
				//
				// de_thread() retires the old leader when a NON-leader calls
				// execve(): the execing thread takes over the leader's pid and
				// the retired task's exit surfaces here while the process runs
				// on under that same pid. A leader that merely pthread_exit()s
				// is the same shape. Returning a terminal here declares a live
				// process dead — verified against Linux 6.10, where the retired
				// leader's status decodes to an ordinary "exited, code 0" that
				// no heuristic can tell from a real one, and the genuine death
				// arrives much later.
				//
				// PTRACE_O_TRACEEXIT stops the leader BEFORE it dies, so read
				// the wait(2)-encoded status now via PTRACE_GETEVENTMSG: it is
				// the authoritative record of a non-zero exit or a fatal signal
				// (#94), and once the thread is resumed it is unreadable. Then
				// resume and keep waiting. The terminal is committed when the
				// group is genuinely gone (the leader's real Exited()/Signaled(),
				// or ECHILD) and retracted if an exec proves retirement.
				msg, msgErr := b.eventMsg(tid)
				b.noteLeaderExit(msg, msgErr == nil)
				if err := b.registerThreadGroup(); err != nil && !isNoSuchProcess(err) {
					b.abortStepForWaitFailure()
					return StopEvent{}, fmt.Errorf(
						"%w: register threads while retiring leader tid %d: %v",
						ErrSessionInvalidated, tid, err)
				}
				// Route the resume through the ordinary dying-thread absorb.
				// The leader can be the thread under a single step, and a bare
				// continue there consumes that step while stepping/stepTID stay
				// latched: no completion can ever arrive, so every later foreign
				// stop parks forever and Wait never returns (park-queue rule 7).
				// Going through absorbThreadExit also lets the leader become the
				// reconciliation anchor when it owns a lifted trap, in which case
				// it is deliberately not resumed until the engine has reinstalled.
				if err := b.absorbStop(absorbThreadExit, tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting process tid %d: %w", tid, err)
				}
				continue

			case syscall.PTRACE_EVENT_EXEC:
				// execve replaces the ENTIRE process image, for every thread —
				// not just the one that called it. Breakpoint addresses, the
				// original instruction bytes saved behind each trap and the
				// thread ids the engine is tracking all describe an image that
				// no longer exists, so there is nothing left to reconcile
				// against and no safe way to carry the session forward. Fail
				// the wait unconditionally: it is fatal whether or not a step
				// was in flight and whichever thread it is reported on. The
				// error is marked ErrSessionInvalidated so the engine discards
				// the tracee explicitly instead of abandoning stopped threads.
				//
				// It is also the retraction point for a stashed leader exit: an
				// exec here proves de_thread() retired the old leader rather
				// than the process dying, so that pending terminal must not be
				// committed.
				//
				// The exec stop is deliberately NOT resumed here — the engine's
				// discard reaps or detaches it, along with anything parked. Nor
				// is startup affected: the launch path consumes its own execve
				// stop through the broker BEFORE enabling PTRACE_O_TRACEEXEC,
				// and Restart runs on a brand-new debugger.
				formerTID, _ := b.eventMsg(tid)
				retracted := b.retractLeaderExit()
				b.attachImageGone = true
				b.markAttachedStopped(tid, StopEvent{Reason: stopAttachedInternal, TID: tid}, false, 0, true)
				b.pendingSignals.purge()
				b.abortStepForWaitFailure()
				return StopEvent{}, fmt.Errorf(
					"%w (PTRACE_EVENT_EXEC reported under pid %d, "+
						"execing thread's former tid %d%s): debugging across an exec is unsupported — "+
						"every breakpoint address, saved instruction byte and thread id belongs to the "+
						"image that is now gone. After execve the kernel reports this stop under the "+
						"thread-group leader's pid, which does not identify the thread that called it",
					ErrImageReplaced, tid, formerTID,
					map[bool]string{true: ", retiring the previous leader", false: ""}[retracted])

			case 0:
				reason, disp := classifyUserStop(true, b.stepping, b.stepTID, tid)
				if disp == parkStop || b.stepExitPending {
					// A sibling hit a software breakpoint while another thread
					// is mid-step. It stays ptrace-stopped where it is; the
					// engine sees it only once the step has completed and the
					// trap being stepped over is back in place.
					event := StopEvent{Reason: reason, TID: tid, Signal: int(sig)}
					b.park(event)
					b.markAttachedStopped(tid, event, false, 0, false)
					continue
				}
				ev := StopEvent{Reason: reason, TID: tid}
				b.recordDeliveredStop(ev)
				if reason == StopSingleStep {
					b.endStep()
				}
				return ev, nil

			case unix.PTRACE_EVENT_STOP:
				state := b.attachedTracees[tid]
				if state != nil && state.initialStopPending {
					state.initialStopPending = false
					if err := b.absorbStop(absorbNewThread, tid, 0); err != nil {
						return StopEvent{}, fmt.Errorf("resume seized thread tid %d: %w", tid, err)
					}
					continue
				}
				if err := b.absorbStop(absorbInterrupt, tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("resume event-stop tid %d: %w", tid, err)
				}
				continue

			default:
				// An unrecognised PTRACE_EVENT. With exactly TRACEEXIT,
				// TRACEEXEC and TRACECLONE enabled — and PTRACE_TRACEME/ATTACH
				// rather than SEIZE — the only causes the kernel can report are
				// CLONE, EXEC and EXIT: FORK/VFORK/VFORK_DONE/SECCOMP each need
				// an option we never set, and EVENT_STOP needs SEIZE. So this is
				// unreachable in practice and there is nothing to reason about
				// if it is reached. On the stepped thread that leaves no safe
				// move: continuing cancels the step with the trap still out of
				// memory, and re-arming assumes a stop shape we do not
				// understand. Fail loudly rather than guess — and mark it
				// session-invalidating so the engine discharges every stop it
				// owns instead of leaving them frozen behind a closing tracer.
				if plan := b.planAbsorb(absorbUnknownEvent, tid, 0); plan.fail {
					b.abortStepForWaitFailure()
					return StopEvent{}, fmt.Errorf(
						"%w: unhandled ptrace event %d on tid %d while it was being single-stepped",
						ErrSessionInvalidated, cause, tid)
				} else if err := b.applyAbsorb(plan, tid); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT trap cause %d tid %d: %w", cause, tid, err)
				}
				continue
			}
		}

		if sig == syscall.SIGSTOP && tid != b.pid {
			// This is normally a new clone's initial group-stop, but the raw
			// predicate is not proof of newness: an existing worker, including
			// the active step owner, can report the same stop. Clearing that
			// owner's retained signal would lose it when the step is re-armed
			// with signal zero. Other TIDs retain the established reuse cleanup.
			if b.resumeFor(tid) != resumeSingleStep {
				b.pendingSignals.clear(tid)
			}
			// Resume only this thread. A group-continue could release a sibling
			// parked at a breakpoint before the engine has handled its stop.
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
		if disp == parkStop || b.stepExitPending {
			event := StopEvent{Reason: reason, TID: tid, Signal: int(sig)}
			b.park(event)
			b.markAttachedStopped(tid, event, false, 0, false)
			continue
		}
		ev := StopEvent{
			Reason: reason,
			TID:    tid,
			Signal: int(sig),
		}
		b.recordDeliveredStop(ev)
		return ev, nil
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

// recordDeliveredStop installs a signal before publishing its TID as the
// current resume target. Outside teardown, a concurrent reader may observe the
// previous TID, but it cannot observe the new TID without the signal that
// belongs to it.
func (b *linuxBackend) recordDeliveredStop(ev StopEvent) {
	if ev.Reason == StopSignal && ev.Signal != b.PauseSignal() {
		b.pendingSignals.set(ev.TID, ev.Signal)
	}
	b.recordStop(ev.TID)
	b.markAttachedStopped(ev.TID, ev, ev.Reason == StopSignal, 0, true)
}

func (b *linuxBackend) purge() {
	b.stepQueue.purge()
	b.pendingSignals.purge()
}

func (b *linuxBackend) abortStepForWaitFailure() {
	if b.attached() {
		b.collectStepQueueForDetach()
	}
	b.abortStep()
}

func isNoSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrNotExist)
}

func isNoChildProcess(err error) bool {
	return errors.Is(err, syscall.ECHILD)
}

// continueIfTraceeExists is the only internal continue path. A SIGURG delayed
// through a single-step is first requeued to the same TID and resumed with
// signal zero; only the resulting signal-delivery stop may inject SIGURG.
func (b *linuxBackend) continueIfTraceeExists(tid int, signal int) error {
	if tid == 0 {
		return nil
	}
	pending, resumeSignal := b.pendingSignals.takeForExplicitResume(tid, signal)
	requeued, err := b.requeueSignals(tid, pending.deferred)
	if err != nil {
		b.pendingSignals.restore(tid, pending.withoutDeferredPrefix(requeued))
		return err
	}
	if b.contFn != nil {
		err = b.contFn(tid, resumeSignal)
	} else {
		b.execPtrace(func() { err = b.ptraceCont(tid, resumeSignal) })
	}
	if err != nil {
		// A failed internal resume does not prove retirement. The TID remains
		// ptrace-stopped, so successfully requeued standard signals cannot be
		// delivered before a retry and matching tgkill calls coalesce; if ESRCH
		// instead meant it escaped the stop, set coalesces the fresh delivery
		// against the restored batch.
		b.pendingSignals.restore(tid, pending)
		if !isNoSuchProcess(err) {
			return err
		}
	}
	b.markAttachedRunning(tid)
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
		b.execPtrace(func() { err = b.ptraceSingleStep(tid) })
	}
	if err != nil && !isNoSuchProcess(err) {
		return err
	}
	b.markAttachedRunning(tid)
	return nil
}

// absorbStop resumes a thread whose stop Wait handled internally instead of
// reporting it. Every absorb site must go through here: the kernel delivers one
// stop per resume, so absorbing an event on the thread the engine is stepping
// consumes that step, and a plain continue there would silently cancel it while
// the step gate stays latched. See stepQueue.planAbsorb.
//
// A signal absorbed on the stepped thread is delayed through the signal-zero
// re-step. Injecting it with PTRACE_SINGLESTEP would stop in the handler's signal
// frame before the requested instruction executes. The next continue requeues
// it to the same TID and forwards it from a fresh signal-delivery stop.
//
// Callers handle plan.fail themselves, because each failing branch has its own
// diagnosis to report.
func (b *linuxBackend) absorbStop(kind absorbKind, tid int, signal int) error {
	return b.applyAbsorb(b.planAbsorb(kind, tid, signal), tid)
}

// applyAbsorb carries out an absorbPlan. A dying step owner transitions to the
// reconciliation gate before it is resumed, so no later stop can escape between
// the thread's exit and the engine restoring its breakpoint transaction. When
// the plan holds that owner it is deliberately NOT resumed here: it is the
// engine's only memory-write anchor, and completeStepThreadExit releases it once
// the reinstall is done.
func (b *linuxBackend) applyAbsorb(plan absorbPlan, tid int) error {
	if plan.stepThreadExits {
		b.interruptStepIfStepped(tid)
	}
	if plan.holdStepOwner {
		b.holdStepOwner(tid)
		b.markAttachedStopped(tid, StopEvent{Reason: StopStepThreadExited, TID: tid}, false, 0, false)
		return nil
	}
	if plan.mode == resumeSingleStep {
		b.pendingSignals.delay(tid, plan.signal)
		b.countStepRearm()
		return b.singleStepIfTraceeExists(tid)
	}
	return b.continueIfTraceeExists(tid, plan.signal)
}

func (b *linuxBackend) requeueSignals(tid int, signals []int) (int, error) {
	for i, signal := range signals {
		if err := b.requeueSignal(tid, signal); err != nil {
			return i, err
		}
	}
	return len(signals), nil
}

func (b *linuxBackend) requeueSignal(tid, signal int) error {
	if signal == 0 {
		return nil
	}
	tgkill := syscall.Tgkill
	if b.tgkillFn != nil {
		tgkill = b.tgkillFn
	}
	if err := tgkill(b.pid, tid, syscall.Signal(signal)); err != nil {
		return fmt.Errorf("requeue signal %d to tid %d: %w", signal, tid, err)
	}
	return nil
}

func (b *linuxBackend) continueWithoutPendingSignals(tid int) error {
	b.pendingSignals.clear(tid)
	return b.continueIfTraceeExists(tid, 0)
}

func (b *linuxBackend) ptraceCont(tid, signal int) error {
	return b.ptraceResume(syscall.PTRACE_CONT, tid, signal)
}

func (b *linuxBackend) ptraceSingleStep(tid int) error {
	return b.ptraceResume(syscall.PTRACE_SINGLESTEP, tid, 0)
}

func (b *linuxBackend) ptraceResume(request, tid, signal int) error {
	call := syscall.Syscall6
	if b.ptraceSyscall6Fn != nil {
		call = b.ptraceSyscall6Fn
	}
	_, _, errno := call(
		syscall.SYS_PTRACE,
		uintptr(request),
		uintptr(tid),
		0,
		uintptr(signal),
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
