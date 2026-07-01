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
	"syscall"
	"unsafe"
)

func newBackend() Backend {
	return &darwinBackend{}
}

type darwinBackend struct {
	pid      int
	stepMu   sync.Mutex
	stepping bool
	stepTID  int // Mach thread port saved by SingleStep, reused on the step trap

	// hwStepTID is the Mach thread that currently has the ARM64 hardware
	// single-step bit (MDSCR_EL1.SS) armed. It is cleared on the next resume so
	// the thread doesn't keep trapping. Guarded by stepMu.
	hwStepTID int

	// stepFrozen holds the Mach threads suspended (thread_suspend) for the
	// duration of a single step so that ONLY the target thread advances. The
	// next resume (ContinueProcess or another SingleStep) resumes them.
	// Guarded by stepMu.
	stepFrozen []int

	// stepFirst records whether the in-flight step targets the task's first
	// thread, in which case the step is driven with PT_STEP (kernel-side SS on
	// the first thread) rather than Mach-armed SS. Guarded by stepMu.
	stepFirst bool
}

// Darwin ptrace request codes from <sys/ptrace.h>. PT_ATTACH (10) makes the
// stop wait4-able by PID. Do NOT use PT_ATTACHEXC (14) — Mach exceptions are
// incompatible with our wait4-based Wait loop.
//
// PT_STEP (9) drives the hardware single-step bit (MDSCR_EL1.SS) on the task's
// FIRST thread only (XNU bsd/kern/mach_process.c: get_firstthread +
// thread_setsinglestep). We use it ONLY when the thread we want to step already
// IS the first thread; otherwise we drive the per-thread SS bit via Mach (see
// SingleStep).
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
		// The tracee is almost always ptrace-stopped here (at a breakpoint, or
		// mid single-step). On Darwin a bare SIGKILL to a ptrace-stopped process
		// is NOT delivered until it runs, so cmd.Process.Kill()+cmd.Wait() would
		// block until the harness timeout. Resume it under ptrace delivering
		// SIGKILL so it runs straight into termination. (Even mid-step the step
		// target thread is left runnable, so it processes the signal and the
		// task tears down any threads still frozen for the step.) Fall back to a
		// plain SIGKILL if it is no longer in a ptrace stop.
		if err := ptrace(ptDarwinContinue, uintptr(pid), 1, uintptr(syscall.SIGKILL)); err != nil {
			if killErr := cmd.Process.Kill(); killErr != nil && !isAlreadyExited(killErr) {
				return killErr
			}
		}
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
	b.clearStep()
	// Clear any lingering hardware single-step bit and unfreeze the threads that
	// were suspended for a step so the whole task runs. Best-effort: a thread
	// may have exited, which must not block the continue.
	_ = b.disarmSingleStep()
	b.endStepIsolation()
	if err := b.resumeTraced(ptDarwinContinue, 1, 0); err != nil {
		return fmt.Errorf("PT_CONTINUE: %w", err)
	}
	return nil
}

// SingleStep advances exactly one instruction on the Mach thread tid and leaves
// the process stopped at a SIGTRAP.
//
// Darwin's ptrace cannot target a thread directly: PT_STEP and PT_CONTINUE both
// act on the task's FIRST thread only (XNU bsd/kern/mach_process.c drives
// thread_setsinglestep(get_firstthread, …)). PT_STEP sets MDSCR_EL1.SS on the
// first thread; PT_CONTINUE clears it. So we split by which thread trapped:
//
//   - tid IS the first thread: use PT_STEP. The kernel arms SS on exactly this
//     thread.
//
//   - tid is NOT the first thread: PT_STEP would arm SS on the wrong (first)
//     thread and let the real one run free past the removed trap — the original
//     hang. Instead drive the per-thread hardware SS bit via Mach (mirrors LLDB
//     debugserver EnableHardwareSingleStep), keeping the sequence as close as
//     possible to the working isFirst path:
//      1. Freeze every SIBLING thread (never the target) with thread_suspend.
//      2. Arm MDSCR_EL1.SS on the target thread via thread_set_state
//         (ARM_DEBUG_STATE64) while it is still in its ptrace-stop. We set only
//         MDSCR_EL1.SS, exactly like debugserver and XNU thread_setsinglestep;
//         the kernel sets PSTATE.SS itself when it applies the debug state on
//         the target's return-to-user (arm_debug_set64 → mask_user_saved_state_cpsr).
//      3. PT_CONTINUE the task. XNU clears MDSCR_EL1.SS on the FIRST thread (not
//         our target, and it is frozen anyway), then task_resume's. Only the
//         target is unfrozen, so only it advances — one instruction under the
//         armed single-step — and traps.
//     The target is never thread_suspend'd/thread_resume'd: it is resumed by
//     PT_CONTINUE, so it returns to user through the normal path where the
//     kernel applies its debug state (MDSCR_EL1.SS + PSTATE.SS).
//
// In BOTH cases every OTHER thread is frozen with thread_suspend for the
// duration of the step. This is what LLDB debugserver does to single-step one
// thread, and it is essential here for a second reason: the churn workload
// cycles runtime.LockOSThread across eight busy goroutines, so Go's sysmon
// floods the task with SIGURG (async preemption). If those threads run during
// the step, every PT_CONTINUE we issue is interrupted by a sibling's SIGURG
// before the target gets scheduled to take its single instruction — a livelock
// that never delivers the step trap. Freezing the siblings removes the storm
// (a suspended thread can neither be preempted nor run sysmon) so the target
// steps deterministically. Only the target is left runnable.
func (b *darwinBackend) SingleStep(tid int) error {
	if tid == 0 {
		return fmt.Errorf("SingleStep: invalid tid 0")
	}
	// Undo any leftover isolation/arming from a previous step before starting.
	b.endStepIsolation()
	_ = b.disarmSingleStep()

	threads, err := b.Threads()
	if err != nil {
		return fmt.Errorf("SingleStep: list threads: %w", err)
	}
	isFirst := len(threads) > 0 && threads[0] == tid
	b.setStep(tid)
	b.setStepFirst(isFirst)

	if isFirst {
		// Freeze every OTHER thread, then PT_STEP: the kernel arms SS on the
		// first thread (== tid) and task_resume's the process; only the target
		// is runnable, so it steps one instruction and traps.
		for _, t := range threads {
			if t != tid {
				_ = b.suspendThread(t)
			}
		}
		b.setStepFrozen(threads, tid)
		if err := b.resumeTraced(ptDarwinStep, 1, 0); err != nil {
			b.endStepIsolation()
			b.clearStep()
			return b.stepErr(fmt.Errorf("PT_STEP first thread %d: %w", tid, err))
		}
		return nil
	}

	// Freeze every SIBLING (never the target). This mirrors the isFirst branch
	// above: the target keeps its existing ptrace-stop and is resumed by the
	// PT_CONTINUE below, so the single-step bit we arm is applied by the kernel
	// on the SAME context-switch-in path that PT_STEP relies on. We deliberately
	// do NOT thread_suspend + thread_resume the target — that extra suspend/
	// resume was the bug: XNU only calls arm_debug_set64 (which loads
	// MDSCR_EL1.SS onto the CPU) for thread == current_thread(); for any other
	// thread the debug state is applied on the next context-switch-in. A target
	// that had not fully switched out before we resumed it could come back
	// WITHOUT arm_debug_set64 reapplying SS, so it ran free past the removed
	// trap and Wait() hung. Leaving the target in its ptrace-stop and resuming
	// it via PT_CONTINUE guarantees a fresh context-switch-in that applies SS.
	for _, t := range threads {
		if t != tid {
			_ = b.suspendThread(t)
		}
	}
	b.setStepFrozen(threads, tid)
	// Arm the per-thread hardware single-step bit (MDSCR_EL1.SS) on the target
	// while it is still ptrace-stopped (stored in the thread's DebugData; the
	// kernel loads it and sets PSTATE.SS on the target's return-to-user). MUST
	// happen before PT_CONTINUE so the target never executes an un-stepped
	// instruction.
	if err := b.armSingleStep(tid); err != nil {
		b.endStepIsolation()
		b.clearStep()
		return b.stepErr(err)
	}
	// PT_CONTINUE resumes the task at the ptrace layer. XNU clears the
	// single-step bit on the task's FIRST thread here (harmless: it is frozen,
	// and it is not our target). task_resume then runs, but only the target is
	// unfrozen, so only the target advances — one instruction under the armed
	// single-step, then it traps.
	if err := b.resumeTraced(ptDarwinContinue, 1, 0); err != nil {
		b.endStepIsolation()
		_ = b.disarmSingleStep()
		b.clearStep()
		return b.stepErr(fmt.Errorf("PT_CONTINUE (single-step target %d): %w", tid, err))
	}
	return nil
}

// stepErr converts a step-setup failure into ErrProcessExited when the tracee is
// no longer alive — e.g. it was killed (or exited) in the small window between
// freezing the threads and resuming the target. Reporting a clean exit lets the
// engine emit ProcessExited instead of surfacing an opaque Mach error
// ("thread_resume N: object terminated"). If the process is still alive the
// original error is returned unchanged, so a genuine failure is never masked.
func (b *darwinBackend) stepErr(err error) error {
	if b.processGone() {
		return ErrProcessExited
	}
	return err
}

// processGone reports whether the tracee's Mach task is gone, which means the
// process has terminated: the task port is destroyed on exit even before the
// zombie is reaped, so task_for_pid/task_threads fail. A live process always
// returns at least one thread, so a healthy tracee is never misclassified.
func (b *darwinBackend) processGone() bool {
	threads, err := b.Threads()
	return err != nil || len(threads) == 0
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
	defer C.vm_deallocate(
		C.mach_task_self_,
		C.vm_address_t(uintptr(unsafe.Pointer(threads))),
		C.vm_size_t(uintptr(count)*unsafe.Sizeof(C.mach_port_t(0))),
	)
	ports := unsafe.Slice((*C.mach_port_t)(unsafe.Pointer(threads)), int(count))
	tids := make([]int, len(ports))
	for i, p := range ports {
		tids[i] = int(p)
	}
	return tids, nil
}

//nolint:gocognit // The wait loop is one serialized ptrace state machine.
func (b *darwinBackend) Wait() (StopEvent, error) {
	for {
		var ws syscall.WaitStatus
		// Reactive task-suspend drain — the race backstop for resumeTraced's
		// resume-time drain. Poll for a pending stop without blocking. If none
		// is reapable yet the task is still Mach-suspended, a surplus
		// task_suspend leaked past the resume-time drain (resumeTraced reads the
		// count once, before the resume, so a suspend from the same stop cluster
		// that lands just after that read is missed). A Mach-suspended task
		// cannot run to create a new stop, and WNOHANG proves none is queued, so
		// any positive count here is pure leaked surplus: drain one and re-poll
		// until the task is runnable again or a real stop surfaces. Bounded so a
		// misbehaving task_resume can never spin forever.
		tid, err := syscall.Wait4(b.pid, &ws, syscall.WNOHANG, nil)
		for drains := 0; err == nil && tid == 0 && drains < 64 && b.taskSuspendCount() > 0; drains++ {
			if rerr := b.taskResume(); rerr != nil {
				break
			}
			tid, err = syscall.Wait4(b.pid, &ws, syscall.WNOHANG, nil)
		}
		if err == nil && tid == 0 {
			tid, err = syscall.Wait4(b.pid, &ws, 0, nil)
		}
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

		// While a single step is in flight, DROP any signal and drive the target
		// forward one more time. Every other thread is frozen (see SingleStep),
		// so the only signals we can see here are ones already pending on the
		// target (typically SIGURG from Go preemption). Re-injecting them, or
		// resuming the whole task, would either restart the sysmon preemption
		// storm or let the step escape — so we re-isolate and re-arm instead.
		if b.isStepping() {
			if err := b.continueStep(); err != nil {
				if isNoSuchProcess(err) || errors.Is(err, ErrProcessExited) {
					b.clearStep()
					b.endStepIsolation()
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				b.clearStep()
				b.endStepIsolation()
				return StopEvent{}, err
			}
			continue
		}

		// SIGURG (Go preemption) and SIGWINCH are transparent runtime signals;
		// re-deliver them so the runtime's scheduling isn't disturbed.
		if sig == syscall.SIGURG || sig == syscall.SIGWINCH {
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

func (b *darwinBackend) setPID(pid int) {
	b.pid = pid
}

func (b *darwinBackend) setStep(tid int) {
	b.stepMu.Lock()
	b.stepping = true
	b.stepTID = tid
	b.stepMu.Unlock()
}

func (b *darwinBackend) setStepFirst(first bool) {
	b.stepMu.Lock()
	b.stepFirst = first
	b.stepMu.Unlock()
}

func (b *darwinBackend) clearStep() {
	b.stepMu.Lock()
	b.stepping = false
	b.stepTID = 0
	b.stepFirst = false
	b.stepMu.Unlock()
}

func (b *darwinBackend) isStepping() bool {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	return b.stepping
}

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

// armSingleStep enables the ARM64 hardware single-step bit (MDSCR_EL1.SS) on
// tid and records it so the next resume clears it. The caller is responsible
// for having already cleared any previous arming (SingleStep does this via
// disarmSingleStep). Must be called with the thread quiesced.
func (b *darwinBackend) armSingleStep(tid int) error {
	if err := b.setThreadSingleStep(tid, true); err != nil {
		return err
	}
	b.stepMu.Lock()
	b.hwStepTID = tid
	b.stepMu.Unlock()
	return nil
}

// disarmSingleStep clears the hardware single-step bit from the armed thread, if
// any. A clear failure (e.g. the thread has exited) is returned but callers
// treat clearing as best-effort.
func (b *darwinBackend) disarmSingleStep() error {
	b.stepMu.Lock()
	tid := b.hwStepTID
	b.hwStepTID = 0
	b.stepMu.Unlock()
	if tid == 0 {
		return nil
	}
	return b.setThreadSingleStep(tid, false)
}

// setThreadSingleStep toggles MDSCR_EL1.SS on one Mach thread via cgo.
func (b *darwinBackend) setThreadSingleStep(tid int, on bool) error {
	thread := C.mach_port_t(tid)
	if thread == 0 {
		return fmt.Errorf("setThreadSingleStep: invalid tid 0")
	}
	enable := C.int(0)
	if on {
		enable = 1
	}
	if kr := C.bingo_set_single_step(thread, enable); kr != C.KERN_SUCCESS {
		return fmt.Errorf("set single-step tid %d (on=%v): %s", tid, on, machErrString(kr))
	}
	return nil
}

// suspendThread / resumeThread wrap Mach thread_suspend / thread_resume. They
// are best-effort: a thread may exit between enumeration and the call.
func (b *darwinBackend) suspendThread(tid int) error {
	if kr := C.bingo_thread_suspend(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
		return fmt.Errorf("thread_suspend %d: %s", tid, machErrString(kr))
	}
	return nil
}

func (b *darwinBackend) resumeThread(tid int) error {
	if kr := C.bingo_thread_resume(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
		return fmt.Errorf("thread_resume %d: %s", tid, machErrString(kr))
	}
	return nil
}

func (b *darwinBackend) resumeThreads(tids []int) {
	for _, t := range tids {
		_ = b.resumeThread(t)
	}
}

// taskSuspendCount returns the Mach suspend count of the whole task, or -1 on
// error (e.g. the task port is gone because the process exited).
func (b *darwinBackend) taskSuspendCount() int {
	task, err := b.task()
	if err != nil {
		return -1
	}
	return int(C.bingo_task_suspend_count(task))
}

// taskResume decrements the tracee task's Mach suspend count by one.
func (b *darwinBackend) taskResume() error {
	task, err := b.task()
	if err != nil {
		return err
	}
	if kr := C.bingo_task_resume(task); kr != C.KERN_SUCCESS {
		return fmt.Errorf("task_resume: %s", machErrString(kr))
	}
	return nil
}

// resumeTraced issues a resuming ptrace request (PT_CONTINUE / PT_STEP) and then
// drains any surplus Mach task-level suspends so the tracee actually runs.
//
// On Darwin the BSD ptrace layer performs at most ONE task_resume per
// PT_CONTINUE/PT_STEP — see XNU bsd/kern/mach_process.c, where the `resume:`
// path calls task_resume(task) once, gated on the single per-process sigwait
// flag. But a multithreaded tracee can be task_suspend'd more than once when
// several threads enter the stop path concurrently. Go's async preemption
// (sysmon delivering SIGURG across the churn workload's busy threads) makes this
// routine: two threads occasionally stop the task at nearly the same instant, so
// its Mach suspend count climbs to 2 while wait4 reports a single stop. A lone
// PT_CONTINUE then leaves the count at 1 — every thread frozen, PC static — and
// the next wait4 blocks forever. That was the residual step wedge.
//
// We read the suspend count while the task is still stopped, issue the request,
// then perform exactly (pre-1) extra task_resume calls to cancel the leaked
// suspends. This handles the common case cheaply at resume time, but it is NOT
// sufficient on its own: `pre` is sampled once, before the request, so a surplus
// suspend from the same stop cluster that lands just after the read is missed and
// the count sticks at 1. The reactive drain-before-block in Wait() is the
// authoritative backstop that closes that race; this drain merely keeps the
// common case off the reactive path. (It cannot over-drain: XNU serializes ptrace
// stops per process, so a positive count never covers a second, independently
// wait4-reportable stop — only leaked surplus.)
func (b *darwinBackend) resumeTraced(request, addr, data uintptr) error {
	pre := b.taskSuspendCount()
	if err := ptrace(request, uintptr(b.pid), addr, data); err != nil {
		return err
	}
	for i := 1; i < pre; i++ {
		if err := b.taskResume(); err != nil {
			// The process may have exited mid-drain; stop and let the wait loop
			// observe the exit rather than masking it.
			break
		}
	}
	return nil
}

// setStepFrozen records every thread except the running target as frozen for
// the current step, so the next resume can unfreeze them.
func (b *darwinBackend) setStepFrozen(all []int, target int) {
	frozen := make([]int, 0, len(all))
	for _, t := range all {
		if t != target {
			frozen = append(frozen, t)
		}
	}
	b.stepMu.Lock()
	b.stepFrozen = frozen
	b.stepMu.Unlock()
}

// endStepIsolation resumes any threads frozen for a step. Idempotent.
func (b *darwinBackend) endStepIsolation() {
	b.stepMu.Lock()
	frozen := b.stepFrozen
	b.stepFrozen = nil
	b.stepMu.Unlock()
	b.resumeThreads(frozen)
}

// continueStep drives an in-flight step forward after the target took a signal
// (typically SIGURG). The signal is dropped. It mirrors SingleStep's split:
//
//   - first thread: PT_STEP again. The kernel re-arms SS on the first thread
//     (== target) and task_resume's; the other threads are still frozen from
//     SingleStep, so only the target advances.
//   - non-first thread: the siblings are still frozen from SingleStep and the
//     target was never suspended, so we just re-arm its per-thread MDSCR_EL1.SS
//     (defensive — the SW_STEP handler clears it after each step, and a signal
//     stop before the step leaves it set; re-arming is idempotent and cheap) and
//     PT_CONTINUE, dropping the signal. Only the target is unfrozen, so only it
//     advances one instruction and traps.
func (b *darwinBackend) continueStep() error {
	b.stepMu.Lock()
	target := b.stepTID
	first := b.stepFirst
	b.stepMu.Unlock()
	if target == 0 {
		return fmt.Errorf("continueStep: no step target")
	}
	if first {
		// Other threads remain frozen from SingleStep; PT_STEP re-arms SS on the
		// first thread and resumes it for one more instruction, dropping the
		// signal (data=0).
		if err := b.resumeTraced(ptDarwinStep, 1, 0); err != nil {
			return b.stepErr(fmt.Errorf("PT_STEP (step resume, first thread): %w", err))
		}
		return nil
	}
	if err := b.armSingleStep(target); err != nil {
		return b.stepErr(err)
	}
	if err := b.resumeTraced(ptDarwinContinue, 1, 0); err != nil {
		return b.stepErr(fmt.Errorf("PT_CONTINUE (step resume, target %d): %w", target, err))
	}
	return nil
}

func (b *darwinBackend) resumeAfterSignal(sig syscall.Signal) error {
	if err := b.resumeTraced(ptDarwinContinue, 1, uintptr(sig)); err != nil {
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

// task returns the Mach task port for the tracee. Requires the
// com.apple.security.cs.debugger entitlement or disabled SIP.
func (b *darwinBackend) task() (C.mach_port_t, error) {
	var task C.mach_port_t
	kr := C.bingo_task_for_pid(C.int(b.pid), &task)
	if kr != C.KERN_SUCCESS {
		return 0, fmt.Errorf("task_for_pid(%d): %s — debugger entitlement required",
			b.pid, machErrString(kr))
	}
	return task, nil
}

func machErrString(kr C.kern_return_t) string {
	return C.GoString(C.mach_error_string(kr))
}
