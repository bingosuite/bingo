//go:build darwin && arm64 && bingonative

package debugger

// Darwin/arm64 (Apple Silicon) Backend. Self-contained: process lifecycle via
// ptrace, registers via Mach thread_get_state, memory via mach_vm_read/write.
//
// Requires com.apple.security.cs.debugger entitlement (or SIP disabled).
// Cannot be cross-compiled from non-macOS — cgo needs the macOS SDK.

/*
#cgo LDFLAGS: -framework CoreFoundation

#include "mach_darwin_arm64.h"
*/
import "C"

import (
	"debug/macho"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

func newBackend() Backend {
	return &darwinBackend{}
}


type darwinBackend struct {
	pid int

	// task_for_pid is expensive and leaks a task send-right on every call, so
	// acquire the tracee task port once and reuse it. Stable for the process
	// lifetime (we never follow exec).
	taskMu   sync.Mutex
	taskPort C.mach_port_t

	// One retained send-right per live tracee thread, keyed by Mach port name.
	// task_threads hands out a fresh send-right per thread on every call; we
	// keep exactly one and deallocate the rest (and the rights of threads that
	// have exited) so the debugger doesn't leak ports under thread churn and so
	// a given thread keeps a stable port name across enumerations.
	threadMu    sync.Mutex
	threadPorts map[C.mach_port_t]struct{}

	// Single-step state. armedTID is the thread whose MDSCR_EL1.SS bit we set
	// (0 = none); it outlives `stepping` because the hardware bit persists until
	// we explicitly clear it (disarmStep). armedIsFirst records whether armedTID
	// is the task's first thread, which selects the resume verb (see SingleStep).
	// suspended lists the Mach thread ports we thread_suspend'd for the duration
	// of a single-step so only the target thread runs; disarmStep resumes them.
	stepMu       sync.Mutex
	stepping     bool
	stepTID      int
	armedTID     int
	armedIsFirst bool
	suspended    []int

	// pendingSig holds a process-level asynchronous signal (SIGWINCH) that
	// arrived while a single-step was armed. Delivering a signal to a thread
	// mid-single-step redirects it into Go's signal trampoline instead of
	// retiring the stepped instruction, which corrupts a breakpoint step-off. We
	// suppress the signal for the step and stash it here so the next full
	// (non-stepping) resume re-delivers it. SIGURG (Go async preemption) is NOT
	// stashed — re-injecting it via ptrace targets the wrong thread and wedges
	// the Go scheduler (see deferSignal). 0 means none.
	pendingSig atomic.Int32
}

// Darwin ptrace request codes from <sys/ptrace.h>. PT_ATTACH (10) makes the
// stop wait4-able by PID. Do NOT use PT_ATTACHEXC (14) — Mach exceptions are
// incompatible with our wait4-based Wait loop.
const (
	ptDarwinContinue = uintptr(7)
	ptDarwinStep     = uintptr(9)
	ptDarwinAttach   = uintptr(10)
	ptDarwinDetach   = uintptr(11)
)

func ptrace(request, pid, addr, data uintptr) error {
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PTRACE,
		request, pid, addr, data,
		0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func startTracedProcess(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
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
		return 0, nil, fmt.Errorf("wait for initial stop: %w", err)
	}
	if !ws.Stopped() {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("expected initial SIGTRAP, got: %v", ws)
	}
	return pid, cmd, nil
}

func attachToProcess(pid int) error {
	if err := ptrace(ptDarwinAttach, uintptr(pid), 0, 0); err != nil {
		return fmt.Errorf("PT_ATTACH pid %d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return fmt.Errorf("wait after PT_ATTACH: %w", err)
	}
	return nil
}

func killProcess(pid int, cmd *exec.Cmd) error {
	if cmd != nil {
		if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
			return err
		}
		// A ptrace-stopped tracee stays frozen in its trace-stop and never
		// processes the pending SIGKILL, so cmd.Wait() would block indefinitely
		// (the "Kill doesn't reap" wedge). Resume it while injecting SIGKILL so
		// the uncatchable signal is delivered and the process exits, then reap.
		// If the tracee isn't currently stopped this ptrace fails harmlessly.
		_ = ptrace(ptDarwinContinue, uintptr(pid), 1, uintptr(syscall.SIGKILL))
		_ = cmd.Wait()
		return nil
	}
	// Attached (not launched): detach, don't kill — we don't own the process.
	_ = ptrace(ptDarwinDetach, uintptr(pid), 1, 0)
	return nil
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

func (b *darwinBackend) ContinueProcess() error {
	// Clear any lingering single-step arming so the task runs free.
	b.disarmStep()
	// Re-deliver any process-level signal (SIGWINCH) that was deferred while a
	// single-step was armed. SIGURG is never stashed here (see deferSignal): it
	// is Go's per-thread async-preemption signal and must not be re-injected via
	// ptrace, which would target the wrong thread.
	sig := b.takePendingSig()
	if err := ptrace(ptDarwinContinue, uintptr(b.pid), 1, uintptr(sig)); err != nil {
		return fmt.Errorf("PT_CONTINUE: %w", err)
	}
	return nil
}

// deferSignal remembers a process-level asynchronous signal that arrived during
// a single-step so ContinueProcess can re-deliver it once the process resumes
// freely.
//
// SIGURG is deliberately NOT stashed. It is Go's async-preemption signal, which
// the runtime delivers to a SPECIFIC M with pthread_kill. Darwin ptrace, however,
// re-injects a caught signal to get_firstthread(task) (XNU bsd/kern/mach_process.c)
// — not the M the runtime targeted. Re-injecting SIGURG therefore preempts the
// wrong thread and corrupts the scheduler's stopTheWorld bookkeeping, which can
// wedge the whole tracee (all Ms park with a runnable G that never gets scheduled).
// Dropping SIGURG is safe: it is a best-effort hint (the runtime re-issues it) and
// Go's cooperative preemption (g.stackguard0 = stackPreempt, checked at call
// prologues) still preempts the goroutine at the next function call. Only SIGWINCH,
// which is process-level and thread-agnostic, is deferred for re-delivery.
func (b *darwinBackend) deferSignal(sig syscall.Signal) {
	if sig == syscall.SIGWINCH {
		b.pendingSig.Store(int32(sig))
	}
}

// takePendingSig atomically returns and clears any deferred signal (0 if none).
func (b *darwinBackend) takePendingSig() syscall.Signal {
	return syscall.Signal(b.pendingSig.Swap(0))
}

// SingleStep advances exactly one instruction on the thread identified by tid
// (a Mach thread port), which must be the thread that is actually stopped where
// we want to step (e.g. the thread that hit a breakpoint).
//
// It does NOT use ptrace(PT_STEP): on Darwin PT_STEP arms single-step on
// get_firstthread(task) only (XNU bsd/kern/mach_process.c), which in a
// multithreaded Go process is almost always an idle runtime M parked in a
// syscall — it never retires an instruction, so no SIGTRAP ever arrives and
// wait4 hangs. Instead we arm hardware single-step (MDSCR_EL1.SS) on tid's own
// debug state and resume the whole task. PT_STEP/PT_CONTINUE only touch
// get_firstthread's single-step bit, so we pick the resume verb to leave
// exactly tid armed:
//   - tid is NOT the first thread -> PT_CONTINUE (clears first thread's bit,
//     leaves tid's bit set: only tid steps)
//   - tid IS  the first thread    -> PT_STEP    (arms the first thread == tid)
func (b *darwinBackend) SingleStep(tid int) error {
	// Drop any previous arming before arming a new thread.
	b.disarmStep()

	thread := C.mach_port_t(tid)
	if thread == 0 {
		return fmt.Errorf("SingleStep: invalid tid 0")
	}
	if kr := C.bingo_set_thread_single_step(thread, 1); kr != C.KERN_SUCCESS {
		return fmt.Errorf("arm single-step tid %d: %s", tid, machErrString(kr))
	}

	isFirst := b.isFirstThread(tid)
	b.armStep(tid, isFirst)

	// Suspend every other thread so the task resume runs only tid. This gives
	// single-thread-step semantics (as LLDB's debugserver does): no sibling can
	// hit a breakpoint or take a signal during the one-instruction step window
	// and perturb the ARM software-step state machine.
	b.suspendOthers(tid)

	req := ptDarwinContinue
	if isFirst {
		req = ptDarwinStep
	}
	if err := ptrace(req, uintptr(b.pid), 1, 0); err != nil {
		b.disarmStep()
		return fmt.Errorf("resume for single-step tid %d: %w", tid, err)
	}
	return nil
}

// StepOffBreakpoint advances tid exactly one instruction and consumes the
// resulting trap internally, without surfacing a StopEvent to the engine's wait
// loop.
//
// On Darwin/arm64 a thread that is parked on the PC where a software breakpoint
// (BRK) just fired cannot be reliably resumed by a plain task PT_CONTINUE: the
// thread stays wedged at that PC and the next wait4 blocks forever. Arming a
// hardware single-step (MDSCR_EL1.SS) first forces the thread through the
// exception-return path so it retires one instruction and moves to a clean PC,
// after which an ordinary continue resumes it normally.
//
// The engine calls this (via an optional interface) after clearing a one-shot
// internal sentinel breakpoint (<stepover-next> / <stepout-return>), so the
// following Continue resumes cleanly. It mirrors the user-breakpoint path, which
// already single-steps off the trap inside resumeFromBreakpoint before resuming.
//
// It must be called ONLY while the process is stopped and no wait loop is active
// (i.e. from the engine's serialized stop handler): it runs its own wait4 to
// catch the step's SIGTRAP.
func (b *darwinBackend) StepOffBreakpoint(tid int) error {
	if tid == 0 {
		return nil
	}
	if err := b.SingleStep(tid); err != nil {
		return err
	}
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(b.pid, &ws, 0, nil)
		if err != nil {
			b.disarmStep()
			if isNoChildProcess(err) {
				return nil
			}
			return fmt.Errorf("wait4 (step off bp): %w", err)
		}
		if ws.Exited() || ws.Signaled() {
			b.disarmStep()
			return nil
		}
		if !ws.Stopped() {
			continue
		}
		sig := ws.StopSignal()
		if sig == syscall.SIGTRAP {
			// Step retired: clear MDSCR_EL1.SS so the next resume runs free.
			b.disarmStep()
			return nil
		}
		// A non-TRAP signal (SIGURG preemption, SIGWINCH, …) arrived before the
		// single-step retired. Delivering it now would redirect this thread into
		// Go's signal trampoline (_sigtramp) instead of executing the instruction
		// at the breakpoint address — the step would "complete" in the handler,
		// leaving the thread never actually stepped past the trap and wedging the
		// following resume. Suppress it for the step (resume with signal 0 so the
		// real instruction retires); deferSignal stashes SIGWINCH for re-delivery
		// and drops SIGURG (re-injecting async preemption via ptrace wedges the
		// scheduler — see deferSignal).
		b.deferSignal(sig)
		if err := ptrace(b.stepResumeVerb(), uintptr(b.pid), 1, 0); err != nil {
			b.disarmStep()
			if isNoSuchProcess(err) {
				return nil
			}
			return fmt.Errorf("resume after signal (step off bp): %w", err)
		}
	}
}

func (b *darwinBackend) StopProcess() error {
	p, err := os.FindProcess(b.pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGSTOP)
}

func (b *darwinBackend) GetRegisters(tid int) (Registers, error) {
	thread := C.mach_port_t(tid)
	if thread == 0 {
		return Registers{}, fmt.Errorf("GetRegisters: invalid tid 0")
	}
	var pc, sp, fp, g C.uint64_t
	kr := C.bingo_get_registers(thread, &pc, &sp, &fp, &g)
	if kr != C.KERN_SUCCESS {
		return Registers{}, fmt.Errorf("thread_get_state tid %d: %s", tid, machErrString(kr))
	}
	return Registers{
		PC:  uint64(pc),
		SP:  uint64(sp),
		BP:  uint64(fp),
		TLS: uint64(g),
	}, nil
}

func (b *darwinBackend) SetRegisters(tid int, reg Registers) error {
	thread := C.mach_port_t(tid)
	if thread == 0 {
		return fmt.Errorf("SetRegisters: invalid tid 0")
	}
	kr := C.bingo_set_registers(thread,
		C.uint64_t(reg.PC),
		C.uint64_t(reg.SP),
		C.uint64_t(reg.BP),
		C.uint64_t(reg.TLS),
	)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("thread_set_state tid %d: %s", tid, machErrString(kr))
	}
	return nil
}

func (b *darwinBackend) ReadMemory(addr uint64, dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	task, err := b.task()
	if err != nil {
		return err
	}
	kr := C.bingo_read_memory(task,
		C.mach_vm_address_t(addr),
		unsafe.Pointer(&dst[0]),
		C.mach_vm_size_t(len(dst)),
	)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("mach_vm_read_overwrite 0x%x: %s", addr, machErrString(kr))
	}
	return nil
}

func (b *darwinBackend) WriteMemory(addr uint64, src []byte) error {
	if len(src) == 0 {
		return nil
	}
	task, err := b.task()
	if err != nil {
		return err
	}
	kr := C.bingo_write_memory(task,
		C.mach_vm_address_t(addr),
		unsafe.Pointer(&src[0]),
		C.mach_vm_size_t(len(src)),
	)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("mach_vm_write 0x%x: %s", addr, machErrString(kr))
	}
	return nil
}

func (b *darwinBackend) Threads() ([]int, error) {
	task, err := b.task()
	if err != nil {
		return nil, err
	}
	var threads C.thread_act_port_array_t
	var count C.mach_msg_type_number_t
	kr := C.bingo_thread_list(task, &threads, &count)
	if kr != C.KERN_SUCCESS {
		return nil, fmt.Errorf("task_threads pid %d: %s", b.pid, machErrString(kr))
	}
	ports := unsafe.Slice((*C.mach_port_t)(unsafe.Pointer(threads)), int(count))

	// Retain exactly one send-right per live thread; drop the duplicate rights
	// this call produced and the rights of threads that have since exited.
	b.threadMu.Lock()
	if b.threadPorts == nil {
		b.threadPorts = make(map[C.mach_port_t]struct{})
	}
	seen := make(map[C.mach_port_t]struct{}, len(ports))
	tids := make([]int, len(ports))
	for i, p := range ports {
		seen[p] = struct{}{}
		if _, ok := b.threadPorts[p]; ok {
			C.bingo_port_deallocate(p)
		} else {
			b.threadPorts[p] = struct{}{}
		}
		tids[i] = int(p)
	}
	for p := range b.threadPorts {
		if _, ok := seen[p]; !ok {
			C.bingo_port_deallocate(p)
			delete(b.threadPorts, p)
		}
	}
	b.threadMu.Unlock()

	C.vm_deallocate(
		C.mach_task_self_,
		C.vm_address_t(uintptr(unsafe.Pointer(threads))),
		C.vm_size_t(uintptr(count)*unsafe.Sizeof(C.mach_port_t(0))),
	)
	return tids, nil
}

// isFirstThread reports whether tid is the task's first thread. task_threads
// returns threads in creation order, so tids[0] is get_firstthread(task) — the
// thread ptrace(PT_STEP/PT_CONTINUE) arms/clears single-step on.
func (b *darwinBackend) isFirstThread(tid int) bool {
	tids, err := b.Threads()
	if err != nil || len(tids) == 0 {
		return false
	}
	return tids[0] == tid
}

//nolint:gocognit // The wait loop is one serialized ptrace state machine.
func (b *darwinBackend) Wait() (StopEvent, error) {
	for {
		var ws syscall.WaitStatus
		tid, err := syscall.Wait4(b.pid, &ws, 0, nil)
		if err != nil {
			if isNoChildProcess(err) {
				return StopEvent{Reason: StopExited, TID: b.pid}, nil
			}
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}
		if ws.Exited() {
			return StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, nil
		}
		if ws.Signaled() {
			return StopEvent{Reason: StopKilled, TID: tid}, nil
		}
		if !ws.Stopped() {
			continue
		}

		sig := ws.StopSignal()

		if sig == syscall.SIGTRAP {
			if stepping, thread := b.consumeStep(); stepping {
				return StopEvent{Reason: StopSingleStep, TID: thread}, nil
			}
			return StopEvent{Reason: StopBreakpoint}, nil
		}

		// A non-TRAP signal arrived. If a single-step is armed on a thread,
		// delivering the signal now would redirect that thread into Go's signal
		// trampoline (_sigtramp) instead of retiring the stepped instruction,
		// corrupting a breakpoint step-off and wedging the next resume. Suppress
		// it for the step (resume with signal 0); deferSignal stashes SIGWINCH
		// for re-delivery on the next full resume and drops SIGURG (see
		// deferSignal for why re-injecting async preemption is unsafe here).
		if b.isStepping() {
			b.deferSignal(sig)
			if err := ptrace(b.stepResumeVerb(), uintptr(b.pid), 1, 0); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				b.disarmStep()
				return StopEvent{Reason: StopSignal, Signal: int(sig)}, nil
			}
			continue
		}

		// Not stepping: the process is running freely. SIGURG is Go's async-
		// preemption signal, delivered per-M with pthread_kill. Darwin ptrace can
		// only re-inject a signal to get_firstthread(task), NOT the M the runtime
		// targeted, so re-injecting SIGURG preempts the wrong thread and corrupts
		// the scheduler's stopTheWorld bookkeeping — occasionally deadlocking the
		// whole tracee (all Ms park with a runnable goroutine). Drop it (resume
		// with signal 0); Go's cooperative preemption keeps the tracee scheduling.
		if sig == syscall.SIGURG {
			if err := ptrace(ptDarwinContinue, uintptr(b.pid), 1, 0); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}
		// SIGWINCH is process-level, so re-injecting it to any thread is correct.
		if sig == syscall.SIGWINCH {
			if err := b.resumeAfterSignal(sig); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}

		return StopEvent{Reason: StopSignal, Signal: int(sig)}, nil
	}
}

func (b *darwinBackend) setPID(pid int) { b.pid = pid }

// armStep records an in-flight single-step on tid whose MDSCR_EL1.SS bit is set.
func (b *darwinBackend) armStep(tid int, isFirst bool) {
	b.stepMu.Lock()
	b.stepping = true
	b.stepTID = tid
	b.armedTID = tid
	b.armedIsFirst = isFirst
	b.stepMu.Unlock()
}

// disarmStep clears the single-step arming: it drops the in-flight state,
// clears MDSCR_EL1.SS on the armed thread, and resumes any sibling threads that
// suspendOthers parked for the step, so the task can run freely again.
func (b *darwinBackend) disarmStep() {
	b.stepMu.Lock()
	armed := b.armedTID
	suspended := b.suspended
	b.suspended = nil
	b.stepping = false
	b.stepTID = 0
	b.armedTID = 0
	b.armedIsFirst = false
	b.stepMu.Unlock()
	if armed != 0 {
		_ = C.bingo_set_thread_single_step(C.mach_port_t(armed), 0)
	}
	for _, t := range suspended {
		_ = C.bingo_thread_resume(C.mach_port_t(t))
	}
}

// suspendOthers thread_suspends every current tracee thread except tid and
// records them so disarmStep can resume them after the step. Ports for threads
// that have already exited simply fail to suspend and are skipped.
func (b *darwinBackend) suspendOthers(tid int) {
	tids, err := b.Threads()
	if err != nil {
		return
	}
	suspended := make([]int, 0, len(tids))
	for _, t := range tids {
		if t == tid {
			continue
		}
		if kr := C.bingo_thread_suspend(C.mach_port_t(t)); kr == C.KERN_SUCCESS {
			suspended = append(suspended, t)
		}
	}
	b.stepMu.Lock()
	b.suspended = suspended
	b.stepMu.Unlock()
}

func (b *darwinBackend) isStepping() bool {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	return b.stepping
}

// stepResumeVerb returns the ptrace request that resumes the task while keeping
// the armed thread's single-step effective. PT_STEP/PT_CONTINUE only toggle
// get_firstthread's single-step bit, so when the armed thread IS the first
// thread we must resume with PT_STEP (which re-arms it); otherwise PT_CONTINUE
// leaves the armed thread's bit intact.
func (b *darwinBackend) stepResumeVerb() uintptr {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	if b.armedTID != 0 && b.armedIsFirst {
		return ptDarwinStep
	}
	return ptDarwinContinue
}

// consumeStep reports whether a single-step was in flight and returns the
// stepped thread's port. The MDSCR_EL1.SS bit stays set (disarmStep clears it
// on the next resume) so we can still identify the armed thread.
func (b *darwinBackend) consumeStep() (bool, int) {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	if !b.stepping {
		return false, 0
	}
	tid := b.stepTID
	b.stepping = false
	b.stepTID = 0
	return true, tid
}

func (b *darwinBackend) resumeAfterSignal(sig syscall.Signal) error {
	request := ptDarwinContinue
	if b.isStepping() {
		request = b.stepResumeVerb()
	}
	if err := ptrace(request, uintptr(b.pid), 1, uintptr(sig)); err != nil {
		return fmt.Errorf("ptrace resume after %s: %w", sig, err)
	}
	return nil
}

func isNoSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

func isNoChildProcess(err error) bool {
	return errors.Is(err, syscall.ECHILD)
}

// TextSlide returns the ASLR slide for the main executable: actual load
// address minus preferred __TEXT vmaddr. Returns 0 on any error.
//
// Scans the task's VM map for the first executable region with the 64-bit
// Mach-O magic. Works even at the very first ptrace stop (before dyld runs),
// unlike TASK_DYLD_INFO whose image array is unpopulated at that point.
func (b *darwinBackend) TextSlide(binaryPath string) int64 {
	task, err := b.task()
	if err != nil {
		return 0
	}

	var loadAddr C.mach_vm_address_t
	if kr := C.bingo_find_macho_load_addr(task, &loadAddr); kr != C.KERN_SUCCESS {
		return 0
	}

	preferredVmaddr, err := machoTextVmaddr(binaryPath)
	if err != nil {
		return 0
	}

	return int64(loadAddr) - int64(preferredVmaddr)
}

func machoTextVmaddr(binaryPath string) (uint64, error) {
	f, err := macho.Open(binaryPath)
	if err != nil {
		return 0, fmt.Errorf("macho.Open %s: %w", binaryPath, err)
	}
	defer func() { _ = f.Close() }()
	seg := f.Segment("__TEXT")
	if seg == nil {
		return 0, fmt.Errorf("no __TEXT segment in %s", binaryPath)
	}
	return seg.Addr, nil
}

var _ Backend = (*darwinBackend)(nil)

// task returns the Mach task port for the tracee, acquiring it once and caching
// it. Requires the com.apple.security.cs.debugger entitlement or disabled SIP.
func (b *darwinBackend) task() (C.mach_port_t, error) {
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	if b.taskPort != 0 {
		return b.taskPort, nil
	}
	var task C.mach_port_t
	kr := C.bingo_task_for_pid(C.int(b.pid), &task)
	if kr != C.KERN_SUCCESS {
		return 0, fmt.Errorf("task_for_pid(%d): %s — debugger entitlement required",
			b.pid, machErrString(kr))
	}
	b.taskPort = task
	return task, nil
}

func machErrString(kr C.kern_return_t) string {
	return C.GoString(C.mach_error_string(kr))
}
