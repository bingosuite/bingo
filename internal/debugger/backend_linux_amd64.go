//go:build linux && amd64

package debugger

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// ── Dedicated ptrace thread ──────────────────────────────────────────────────
//
// Linux ties every ptrace *control* request for a tracee (PTRACE_CONT,
// PTRACE_SINGLESTEP, PTRACE_SETOPTIONS, PTRACE_PEEK/POKE, PTRACE_GETREGS, …) to
// the specific OS thread that created the tracee: the thread that forked a
// PTRACE_TRACEME child, or PTRACE_ATTACHed it. Issuing such a call from any
// other thread does not resume/step the target — on our CI kernels it silently
// returns success while leaving the thread ptrace-stopped, wedging the whole
// group.
//
// The engine already funnels its command-side ptrace onto one locked goroutine
// (engine.loop, which is also the thread that forks the tracee). But Backend.Wait
// runs on a *separate* waitLoop goroutine. The clone / thread-lifecycle handling
// Wait performs (resuming the parent and each freshly cloned LWP) therefore ran
// on the wrong thread and never actually resumed anything.
//
// We fix this the way Delve does (execPtraceFunc, pkg/proc/native/proc_linux.go):
// a single dedicated OS thread owns the tracee for its entire lifetime. The
// fork/exec, every ptrace request, wait4, and the bookkeeping they touch are all
// marshalled onto it. ptraceThread is that thread.
type ptraceThread struct {
	reqs chan func()
}

func newPtraceThread() *ptraceThread {
	pt := &ptraceThread{reqs: make(chan func())}
	ready := make(chan struct{})
	go func() {
		// Pin to one OS thread for the process lifetime and never unlock: every
		// ptrace call and every wait4 for the tracee must originate here.
		runtime.LockOSThread()
		close(ready)
		for fn := range pt.reqs {
			fn()
		}
	}()
	<-ready
	return pt
}

// do runs fn on the ptrace thread and blocks until it completes. Calls serialize:
// while Wait is parked in wait4 inside its own do() closure, later do() calls
// queue behind it until the tracee stops. That matches how the engine drives the
// process — resume, then wait for the next stop — so no legitimate flow contends.
func (pt *ptraceThread) do(fn func()) {
	done := make(chan struct{})
	pt.reqs <- func() {
		defer close(done)
		fn()
	}
	<-done
}

// linuxPtrace is published by newBackend so the package-level process hooks in
// this file (startTracedProcess / attachToProcess / killProcess — invoked via
// the shared process type rather than as backend methods) fork and reap on the
// same thread the backend uses for ptrace. The Linux E2E runs one debugger per
// process, so a single package handle is sufficient; each newBackend replaces it.
var linuxPtrace *ptraceThread

func newBackend() Backend {
	pt := newPtraceThread()
	linuxPtrace = pt
	return &linuxBackend{threads: make(map[int]*threadState), pt: pt}
}

// ── Process lifecycle hooks (executed on the ptrace thread) ───────────────────

// linuxPtraceOptions enables clone tracing so every LWP the Go runtime spawns is
// automatically attached and stopped by the kernel. Without this the cloned
// threads are NOT tracees: wait4 never reports them and control-flow that
// migrates onto them wedges the single traced thread (the step-over hang). This
// mirrors Delve's ptraceOptionsNormal.
const linuxPtraceOptions = syscall.PTRACE_O_TRACECLONE

// startTracedProcess forks under ptrace on the dedicated thread. The child is
// stopped at its first instruction (execve SIGTRAP) ready for breakpoints.
func startTracedProcess(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	var (
		pid int
		cmd *exec.Cmd
		err error
	)
	linuxPtrace.do(func() { pid, cmd, err = startTracedProcessLocked(binaryPath, args, env) })
	return pid, cmd, err
}

func startTracedProcessLocked(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	// codeql-suppress[go/command-injection]: The debugger intentionally launches the local binary selected by the operator.
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("exec %q: %w", binaryPath, err)
	}

	pid := cmd.Process.Pid

	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("wait for execve stop: %w", err)
	}
	if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("unexpected initial stop: %v", ws)
	}

	if err := syscall.PtraceSetOptions(pid, linuxPtraceOptions); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("PTRACE_SETOPTIONS: %w", err)
	}

	return pid, cmd, nil
}

func attachToProcess(pid int) error {
	var err error
	linuxPtrace.do(func() { err = attachToProcessLocked(pid) })
	return err
}

func attachToProcessLocked(pid int) error {
	if err := syscall.PtraceAttach(pid); err != nil {
		return fmt.Errorf("PTRACE_ATTACH pid %d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return fmt.Errorf("wait after PTRACE_ATTACH: %w", err)
	}
	// Enable clone tracing so LWPs created after we attach are traced too.
	if err := syscall.PtraceSetOptions(pid, linuxPtraceOptions); err != nil {
		return fmt.Errorf("PTRACE_SETOPTIONS pid %d: %w", pid, err)
	}
	return nil
}

func killProcess(pid int, cmd *exec.Cmd) error {
	var err error
	linuxPtrace.do(func() { err = killProcessLocked(pid, cmd) })
	return err
}

func killProcessLocked(pid int, cmd *exec.Cmd) error {
	if cmd != nil {
		if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
			return err
		}
		_ = cmd.Wait()
		return nil
	}
	// Attached (not launched): detach, don't kill — we don't own the process.
	_ = syscall.PtraceDetach(pid)
	return nil
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

// ── Backend ───────────────────────────────────────────────────────────────────

// threadState tracks a single tracee LWP. running is true after we resume it and
// false once we have observed a stop for it. Only ever touched on the ptrace
// thread, so no locking is required.
type threadState struct {
	running bool
}

type linuxBackend struct {
	pt *ptraceThread

	pid         int
	stepping    bool // true after SingleStep; classifies the next SIGTRAP
	stepTID     int  // thread we issued the single-step against
	lastStopTID int

	// threads holds every LWP we are tracing (thread-group leader + clones).
	// Populated at launch/attach and kept in sync via PTRACE_EVENT_CLONE and
	// thread-exit events in Wait.
	threads map[int]*threadState
}

// do runs fn on the backend's dedicated ptrace thread and blocks for its result.
func (b *linuxBackend) do(fn func()) { b.pt.do(fn) }

// ContinueProcess resumes every thread we currently believe is stopped. A Go
// program only makes progress when the whole thread group runs (timers, sysmon,
// GC and goroutine migration all depend on sibling threads), so resuming a single
// tid is not enough — that is what left the tracer waiting forever after a
// step-over.
func (b *linuxBackend) ContinueProcess() error {
	var err error
	b.do(func() { err = b.continueProcessLocked() })
	return err
}

func (b *linuxBackend) continueProcessLocked() error {
	b.stepping = false
	b.stepTID = 0
	var firstErr error
	for tid, t := range b.threads {
		if t.running {
			continue
		}
		if err := b.resumeThread(tid, 0); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *linuxBackend) SingleStep(tid int) error {
	var err error
	b.do(func() { err = b.singleStepLocked(tid) })
	return err
}

func (b *linuxBackend) singleStepLocked(tid int) error {
	b.stepping = true
	b.stepTID = tid
	b.ensureThread(tid)
	if err := syscall.PtraceSingleStep(tid); err != nil {
		return fmt.Errorf("PTRACE_SINGLESTEP tid %d: %w", tid, err)
	}
	if t := b.threads[tid]; t != nil {
		t.running = true
	}
	return nil
}

// StopProcess sends SIGSTOP to interrupt a running tracee. It deliberately does
// NOT hop onto the ptrace thread: that thread is blocked in wait4 while the
// process runs, and the whole point is to make that wait4 return. kill(2) is not
// thread-bound, so signalling from any thread is fine.
func (b *linuxBackend) StopProcess() error {
	p, err := os.FindProcess(b.pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGSTOP)
}

func (b *linuxBackend) ReadMemory(addr uint64, dst []byte) error {
	var err error
	b.do(func() { err = b.readMemoryLocked(addr, dst) })
	return err
}

func (b *linuxBackend) readMemoryLocked(addr uint64, dst []byte) error {
	tid := b.traceTID()
	n, err := syscall.PtracePeekData(tid, uintptr(addr), dst)
	if err != nil {
		return fmt.Errorf("PTRACE_PEEKDATA tid %d 0x%x: %w", tid, addr, err)
	}
	if n != len(dst) {
		return fmt.Errorf("PTRACE_PEEKDATA tid %d 0x%x: short read %d/%d", tid, addr, n, len(dst))
	}
	return nil
}

func (b *linuxBackend) WriteMemory(addr uint64, src []byte) error {
	var err error
	b.do(func() { err = b.writeMemoryLocked(addr, src) })
	return err
}

func (b *linuxBackend) writeMemoryLocked(addr uint64, src []byte) error {
	tid := b.traceTID()
	n, err := syscall.PtracePokeData(tid, uintptr(addr), src)
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
	var (
		reg Registers
		err error
	)
	b.do(func() { reg, err = b.getRegistersLocked(tid) })
	return reg, err
}

func (b *linuxBackend) getRegistersLocked(tid int) (Registers, error) {
	var r syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(tid, &r); err != nil {
		return Registers{}, fmt.Errorf("PTRACE_GETREGS tid %d: %w", tid, err)
	}
	return Registers{
		PC:  r.Rip,
		SP:  r.Rsp,
		BP:  r.Rbp,
		TLS: r.Fs_base,
	}, nil
}

// SetRegisters writes back the engine-owned fields, preserving everything else by
// reading the full register set first.
func (b *linuxBackend) SetRegisters(tid int, reg Registers) error {
	var err error
	b.do(func() { err = b.setRegistersLocked(tid, reg) })
	return err
}

func (b *linuxBackend) setRegistersLocked(tid int, reg Registers) error {
	var r syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(tid, &r); err != nil {
		return fmt.Errorf("PTRACE_GETREGS (pre-set) tid %d: %w", tid, err)
	}
	r.Rip = reg.PC
	r.Rsp = reg.SP
	r.Rbp = reg.BP
	r.Fs_base = reg.TLS
	if err := syscall.PtraceSetRegs(tid, &r); err != nil {
		return fmt.Errorf("PTRACE_SETREGS tid %d: %w", tid, err)
	}
	return nil
}

// Threads returns all tracee TIDs with the last-stopped thread first. The engine
// reads registers/memory through threads[0] for frame and goroutine snapshots;
// that thread must be one that is actually stopped, otherwise PTRACE_GETREGS fails
// with ESRCH.
func (b *linuxBackend) Threads() ([]int, error) {
	var (
		tids []int
		err  error
	)
	b.do(func() { tids, err = b.threadsLocked() })
	return tids, err
}

func (b *linuxBackend) threadsLocked() ([]int, error) {
	tids, err := taskTIDs(b.pid)
	if err != nil {
		return nil, fmt.Errorf("read /proc/%d/task: %w", b.pid, err)
	}
	if len(tids) == 0 {
		return nil, fmt.Errorf("no threads for pid %d", b.pid)
	}
	ordered := make([]int, 0, len(tids))
	haveStop := false
	for _, tid := range tids {
		if tid == b.lastStopTID {
			haveStop = true
			break
		}
	}
	if haveStop {
		ordered = append(ordered, b.lastStopTID)
	}
	for _, tid := range tids {
		if haveStop && tid == b.lastStopTID {
			continue
		}
		ordered = append(ordered, tid)
	}
	return ordered, nil
}

// Wait blocks until the tracee produces a meaningful debug stop. New-thread
// (clone) events, thread exits and transparent signals (SIGURG preemption,
// SIGCONT, …) are handled internally and never surface to the engine; only a
// breakpoint/single-step trap, an unexpected signal, or a process exit is
// returned. Single-step vs breakpoint disambiguation uses b.stepping (reliable
// because ptrace is serialized on the dedicated thread). The whole loop runs
// inside one do() closure so every ptrace call it makes stays on the tracer
// thread.
func (b *linuxBackend) Wait() (StopEvent, error) {
	var (
		evt StopEvent
		err error
	)
	b.do(func() { evt, err = b.waitLocked() })
	return evt, err
}

//nolint:gocognit,gocyclo // The wait loop is one serialized ptrace state machine.
func (b *linuxBackend) waitLocked() (StopEvent, error) {
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
		if tid <= 0 {
			continue
		}

		if ws.Exited() || ws.Signaled() {
			delete(b.threads, tid)
			if tid == b.pid {
				b.recordStop(tid)
				if ws.Exited() {
					return StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, nil
				}
				return StopEvent{Reason: StopKilled, TID: tid}, nil
			}
			// A non-leader LWP exited; drop it and keep waiting.
			continue
		}

		if !ws.Stopped() {
			continue
		}

		sig := ws.StopSignal()

		// A stop for a tid we are not yet tracking is a freshly cloned LWP. Its
		// initial SIGSTOP can arrive before OR after the parent's
		// PTRACE_EVENT_CLONE; either way the child is stopped and its wait
		// notification has just been reaped, so it is safe to initialize tracing
		// on it here and resume it. Clone children in the debuggee never execute a
		// breakpoint address, so an unknown tid is always a new LWP, not a real
		// trap.
		if _, known := b.threads[tid]; !known {
			b.initNewThread(tid)
			inject := 0
			if sig != syscall.SIGSTOP && sig != syscall.SIGTRAP {
				inject = int(sig)
			}
			_ = b.resumeThread(tid, inject)
			continue
		}

		// PTRACE_EVENT stops are encoded as SIGTRAP | (event << 8). With only
		// PTRACE_O_TRACECLONE set, the sole event we expect is CLONE; anything
		// else is treated as a spurious event-stop and resumed.
		if sig == syscall.SIGTRAP {
			switch ws.TrapCause() {
			case syscall.PTRACE_EVENT_CLONE:
				// The parent thread stopped to report the clone. The child is
				// picked up via its own stop above. Resume the parent.
				_ = b.resumeThread(tid, 0)
				continue

			case 0:
				b.recordStop(tid)
				if b.stepping {
					b.stepping = false
					b.stepTID = 0
					return StopEvent{Reason: StopSingleStep, TID: tid}, nil
				}
				return StopEvent{Reason: StopBreakpoint, TID: tid}, nil

			default:
				_ = b.resumeThread(tid, 0)
				continue
			}
		}

		if isTransparentSignal(sig) {
			// SIGURG carries Go's async preemption; re-deliver it so goroutine
			// scheduling keeps working. Other benign signals are absorbed.
			inject := 0
			if sig == syscall.SIGURG {
				inject = int(sig)
			}
			if b.stepping && tid == b.stepTID {
				// Keep making single-step progress on the stepping thread.
				b.ensureThread(tid)
				if err := syscall.PtraceSingleStep(tid); err != nil && !isNoSuchProcess(err) {
					return StopEvent{}, fmt.Errorf("PTRACE_SINGLESTEP after signal tid %d: %w", tid, err)
				}
				if t := b.threads[tid]; t != nil {
					t.running = true
				}
			} else {
				_ = b.resumeThread(tid, inject)
			}
			continue
		}

		if sig == syscall.SIGSTOP {
			// A known thread received SIGSTOP. We never send SIGSTOP ourselves
			// during normal flow; absorb it and keep the thread going.
			_ = b.resumeThread(tid, 0)
			continue
		}

		// Any other signal (SIGSEGV, SIGILL, …) is surfaced to the engine.
		b.recordStop(tid)
		return StopEvent{Reason: StopSignal, TID: tid, Signal: int(sig)}, nil
	}
}

var _ Backend = (*linuxBackend)(nil)

func (b *linuxBackend) setPID(pid int) {
	b.do(func() {
		b.pid = pid
		b.lastStopTID = pid
		if b.threads == nil {
			b.threads = make(map[int]*threadState)
		}
		// At launch only the thread-group leader exists and it is stopped. Under
		// attach we have already stopped the leader; future clones are picked up
		// through PTRACE_EVENT_CLONE.
		b.threads[pid] = &threadState{running: false}
	})
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
		b.ensureThread(tid).running = false
	}
}

func (b *linuxBackend) ensureThread(tid int) *threadState {
	if b.threads == nil {
		b.threads = make(map[int]*threadState)
	}
	t := b.threads[tid]
	if t == nil {
		t = &threadState{}
		b.threads[tid] = t
	}
	return t
}

// resumeThread PTRACE_CONTs a single tid, tolerating the thread having already
// gone away (ESRCH) by forgetting it.
func (b *linuxBackend) resumeThread(tid, sig int) error {
	if err := syscall.PtraceCont(tid, sig); err != nil {
		if isNoSuchProcess(err) {
			delete(b.threads, tid)
			return nil
		}
		return err
	}
	b.ensureThread(tid).running = true
	return nil
}

// initNewThread starts tracing a freshly cloned LWP. It is only called from Wait
// after the thread's stop has already been reaped, so PTRACE_SETOPTIONS cannot
// race the thread's creation and never needs a blocking wait. Options are
// best-effort: a thread that already died is dropped later on resume.
func (b *linuxBackend) initNewThread(tid int) {
	if _, ok := b.threads[tid]; ok {
		return
	}
	_ = syscall.PtraceSetOptions(tid, linuxPtraceOptions)
	b.threads[tid] = &threadState{running: false}
}

func isTransparentSignal(sig syscall.Signal) bool {
	switch sig {
	case syscall.SIGURG, syscall.SIGCONT, syscall.SIGCHLD,
		syscall.SIGWINCH, syscall.SIGIO, syscall.SIGPROF:
		return true
	}
	return false
}

func isNoSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrNotExist)
}

func isNoChildProcess(err error) bool {
	return errors.Is(err, syscall.ECHILD)
}

func taskTIDs(pid int) ([]int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(entries))
	for _, entry := range entries {
		var tid int
		if _, err := fmt.Sscanf(entry.Name(), "%d", &tid); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids, nil
}
