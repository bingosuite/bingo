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

	// taskPort caches the Mach task port for the tracee. task_for_pid must be
	// called only ONCE per process: on modern macOS it reuses the same port
	// NAME across calls but inserts a fresh send right every time, so calling it
	// on every memory/threads op (as the old code did) leaks send-right urefs
	// without ever releasing them. Once that count balloons the kernel
	// SIGKILLs the tracee mid-run. Delve caches the task port the same way
	// (dbp.os.task). Guarded by taskMu; reset by setPID for a new tracee.
	taskMu   sync.Mutex
	taskPort C.mach_port_t

	// Single-step isolation state, only touched on the engine loop goroutine
	// (SingleStep / ContinueProcess). ssThread is the thread whose hardware
	// single-step (MDSCR_EL1.SS) bit we set; suspended holds the Mach thread
	// ports we Mach-suspended for the step so only ssThread runs.
	ssThread  int
	suspended []int
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

// startTracedProcess launches binaryPath under the debugger on Darwin/arm64.
//
// It does NOT use PTRACE_TRACEME (Go's SysProcAttr.Ptrace). That path runs the
// child's cs_allow_invalid() on the PRE-exec image, which execve then discards,
// leaving the target with CS_KILL set and CS_DEBUGGED clear. On Apple Silicon
// that is fatal for software breakpoints: the first time a patched (BRK) code
// page is made coherent to physical memory, a page fault re-validates the page
// hash, it mismatches, and — because CS_KILL is set and CS_DEBUGGED is not — the
// kernel SIGKILLs the tracee (silently; the cs_invalid_page log is gated behind
// the cs_debug boot-arg).
//
// Instead it mirrors lldb/debugserver: posix_spawn the target SUSPENDED, then
// PT_ATTACH on the already-exec'd image (so cs_allow_invalid() runs on the FINAL
// image — clearing CS_KILL|CS_HARD, setting CS_DEBUGGED, and enabling the
// task's vm_map W^X / cs-debugged bypass), then task_resume so it runs into the
// pending attach-SIGSTOP and stops cleanly for wait4.
func startTracedProcess(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	argv := make([]*C.char, 0, len(args)+2)
	cPath := C.CString(binaryPath)
	defer C.free(unsafe.Pointer(cPath))
	argv = append(argv, cPath)
	for _, a := range args {
		ca := C.CString(a)
		defer C.free(unsafe.Pointer(ca))
		argv = append(argv, ca)
	}
	argv = append(argv, nil)

	fullEnv := os.Environ()
	if len(env) > 0 {
		fullEnv = append(fullEnv, env...)
	}
	envp := make([]*C.char, 0, len(fullEnv)+1)
	for _, e := range fullEnv {
		ce := C.CString(e)
		defer C.free(unsafe.Pointer(ce))
		envp = append(envp, ce)
	}
	envp = append(envp, nil)

	rc := C.bingo_posix_spawn_suspended(cPath, &argv[0], &envp[0])
	if int(rc) <= 0 {
		return 0, nil, fmt.Errorf("posix_spawn %q: errno %d", binaryPath, -int(rc))
	}
	pid := int(rc)

	// Attach on the post-exec (suspended) image. PT_ATTACH detects the
	// POSIX_SPAWN_START_SUSPENDED Mach hold, queues SIGSTOP, and lifts the hold
	// itself (task_resume), so the tracee runs into the SIGSTOP and stops.
	if err := ptrace(ptDarwinAttach, uintptr(pid), 0, 0); err != nil {
		_ = terminateSpawned(pid)
		return 0, nil, fmt.Errorf("PT_ATTACH after spawn pid %d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		_ = terminateSpawned(pid)
		return 0, nil, fmt.Errorf("wait after spawn attach: %w", err)
	}
	if !ws.Stopped() {
		_ = terminateSpawned(pid)
		return 0, nil, fmt.Errorf("expected stop after spawn attach, got ws=%#x", ws)
	}
	// Lift any residual POSIX_SPAWN_START_SUSPENDED task-level Mach hold. The
	// attach-SIGSTOP has already BSD-stopped the tracee (ws.Stopped above), so
	// draining the task suspend to 0 here does not let it run away — it just
	// ensures the first Continue after launch actually resumes it instead of
	// leaving it frozen at _dyld_start (the launch-race hang).
	C.bingo_task_drain_suspend(C.int(pid))
	// Wrap the spawned pid so killProcess can terminate it. The Cmd is never
	// Start()ed — only its Process handle (pid) is used for signalling.
	cmd := exec.Command(binaryPath, args...)
	if proc, perr := os.FindProcess(pid); perr == nil {
		cmd.Process = proc
	}
	return pid, cmd, nil
}

// terminateSpawned SIGKILLs a spawned tracee and reaps it. Used on launch-error
// paths where no exec.Cmd exists yet.
func terminateSpawned(pid int) error {
	_ = syscall.Kill(pid, syscall.SIGKILL)
	var ws syscall.WaitStatus
	_, err := syscall.Wait4(pid, &ws, 0, nil)
	return err
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
		// Launched (we own it): SIGKILL and reap. The Cmd was never Start()ed
		// (the tracee is posix_spawn'd), so reap directly with wait4 rather than
		// cmd.Wait(), which would panic.
		if cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
				return err
			}
		} else {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		var ws syscall.WaitStatus
		for {
			if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
				break // ECHILD once reaped, or already gone
			}
			if ws.Exited() || ws.Signaled() {
				break
			}
		}
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
	b.endStepIsolation()
	b.clearStep()
	if err := ptrace(ptDarwinContinue, uintptr(b.pid), 1, 0); err != nil {
		return fmt.Errorf("PT_CONTINUE: %w", err)
	}
	return nil
}

func (b *darwinBackend) SingleStep(tid int) error {
	// Darwin's PT_STEP sets the AArch64 single-step flag on get_firstthread(task)
	// — the task's oldest thread, which under the Go runtime is usually a parked
	// M sitting in a syscall — and then resumes the WHOLE task (see XNU
	// bsd/kern/mach_process.c). So a bare PT_STEP neither steps the thread that
	// hit the breakpoint (its PC never advances past the restored instruction)
	// nor stops the other threads from racing through the breakpoint window; and
	// when that first thread is parked, its step never retires and Wait hangs.
	//
	// Isolate the step to tid instead: set MDSCR_EL1.SS on tid via Mach and
	// suspend every other thread, so resuming the task advances exactly tid by
	// one instruction. This mirrors lldb-debugserver / Delve. The isolation is
	// torn down by endStepIsolation on the next ContinueProcess/SingleStep.
	b.endStepIsolation()
	if err := b.isolateForStep(tid); err != nil {
		return err
	}
	b.setStep(tid)
	err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0)
	if err != nil {
		b.clearStep()
		b.endStepIsolation()
		return fmt.Errorf("PT_STEP: %w", err)
	}
	return nil
}

// isolateForStep suspends every task thread except target and enables hardware
// single-step on target, so a subsequent task resume steps only target by
// exactly one instruction. The suspended ports and target are recorded for
// endStepIsolation. On failure it rolls back so the task is never left with a
// stray suspended thread.
func (b *darwinBackend) isolateForStep(target int) error {
	threads, err := b.Threads()
	if err != nil {
		return fmt.Errorf("single-step isolate: list threads: %w", err)
	}
	var suspended []int
	for _, tid := range threads {
		if tid == target {
			continue
		}
		// A thread may exit between enumeration and suspend; skip failures. A
		// successfully suspended thread cannot exit until we resume it.
		if kr := C.bingo_thread_suspend(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
			continue
		}
		suspended = append(suspended, tid)
	}
	kr := C.bingo_set_single_step(C.mach_port_t(target), 1)
	if kr != C.KERN_SUCCESS {
		for _, tid := range suspended {
			C.bingo_thread_resume(C.mach_port_t(tid))
		}
		return fmt.Errorf("single-step isolate: enable single-step on tid %d: %s",
			target, machErrString(kr))
	}
	b.suspended = suspended
	b.ssThread = target
	return nil
}

// endStepIsolation clears the hardware single-step bit on the stepped thread and
// resumes every thread suspended by isolateForStep. It is idempotent and safe to
// call when no step is in flight.
func (b *darwinBackend) endStepIsolation() {
	if b.ssThread != 0 {
		C.bingo_set_single_step(C.mach_port_t(b.ssThread), 0)
		b.ssThread = 0
	}
	for _, tid := range b.suspended {
		// ptrace(PT_STEP) also arms MDSCR_EL1.SS on get_firstthread(task) —
		// task_threads()[0], the task's OLDEST thread — regardless of which
		// thread we asked to single-step (XNU bsd/kern/mach_process.c: PT_STEP
		// calls thread_setsinglestep(get_firstthread(task), 1)). Whenever the
		// breakpoint thread is not the oldest thread, that stray bit lands on a
		// thread we suspended for the step. Because isolateForStep suspends every
		// thread except the target, get_firstthread is always among b.suspended,
		// so clearing the step bit on every released thread here guarantees zero
		// threads carry a stray single-step out of the isolation — no thread can
		// spuriously trap (or, in a ptrace state XNU considers inconsistent, get
		// the tracee SIGKILLed) the next time it is scheduled.
		C.bingo_set_single_step(C.mach_port_t(tid), 0)
		C.bingo_thread_resume(C.mach_port_t(tid))
	}
	b.suspended = nil
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
		// Go's syscall.WaitStatus.Stopped() returns false for a WIFSTOPPED status
		// whose stop signal is SIGSTOP — it excludes SIGSTOP by the Unix
		// job-control convention (Stopped() == w&0x7f==0x7f && sig != SIGSTOP).
		// Under ptrace a SIGSTOP-delivery-stop is nonetheless a REAL stop that has
		// suspended the tracee (Darwin holds ptrace stops via task_suspend), so we
		// must handle it, not skip it. If we fell through to a bare `continue`
		// here the tracee would stay suspended and the next wait4 would block
		// forever (the stop is consumed and never re-reported) — the ~2% E2E hang.
		// Recover the real stop signal straight from the raw status; only a
		// genuinely non-stopped status (e.g. WIFCONTINUED) skips with no resume.
		sig := ws.StopSignal()
		if !ws.Stopped() {
			if raw := uint32(ws); raw&0x7f == 0x7f {
				sig = syscall.Signal((raw >> 8) & 0xff)
			} else {
				continue
			}
		}

		if sig == syscall.SIGTRAP {
			if stepping, thread := b.consumeStep(); stepping {
				// The single-step has retired. Tear down the isolation NOW —
				// clear the target thread's MDSCR_EL1.SS bit and resume the
				// threads we suspended for the step — instead of deferring to
				// the next ContinueProcess. This mirrors lldb-debugserver /
				// Delve, which drop the CPU trap flag immediately after each
				// step, and guarantees no thread is left carrying a stray
				// single-step bit (see endStepIsolation) while the engine's
				// reinstall path patches the trap back in.
				b.endStepIsolation()
				return StopEvent{Reason: StopSingleStep, TID: thread}, nil
			}
			return StopEvent{Reason: StopBreakpoint}, nil
		}

		// SIGURG is Go's goroutine-preemption signal. Re-deliver transparently.
		if sig == syscall.SIGURG {
			if err := b.resumeAfterSignal(sig); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}

		// SIGWINCH (terminal resize): handled by the Go runtime, re-deliver.
		if sig == syscall.SIGWINCH {
			if err := b.resumeAfterSignal(sig); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}

		// SIGSTOP: a stray job-control stop of the tracee that we did not ask
		// for — e.g. a lingering PT_ATTACH stop surfacing on a later resume, or
		// the OS attempting to suspend an otherwise-idle process. The debugger
		// owns the tracee's run state, so swallow it (resume with no signal, like
		// SIGURG/SIGWINCH) and keep going. Re-delivering it (data=SIGSTOP) would
		// immediately re-stop the tracee in a loop. This is the counterpart to
		// the raw-status decode above: recognising the SIGSTOP-stop and resuming
		// is what prevents the wait4-blocks-forever wedge.
		if sig == syscall.SIGSTOP {
			if err := b.resumeAfterSignal(0); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}

		// Other signals while single-stepping: defer them (data=0) for the same
		// reason as resumeAfterSignal — delivering a signal mid-step diverts the
		// thread into its handler and breaks the step-over-breakpoint sequence.
		// Completing the step first lets the engine reinstall the trap; the
		// signal is observed on a later stop. If PT_STEP fails, fall through to
		// StopSignal so the engine can recover any in-flight step-over BP.
		if b.isStepping() {
			if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0); err == nil {
				continue
			} else if isNoSuchProcess(err) {
				return StopEvent{Reason: StopKilled, TID: tid}, nil
			}
			b.clearStep()
		}
		return StopEvent{Reason: StopSignal, Signal: int(sig)}, nil
	}
}

func (b *darwinBackend) setPID(pid int) {
	b.taskMu.Lock()
	if b.taskPort != 0 {
		C.bingo_port_deallocate(b.taskPort)
		b.taskPort = 0
	}
	b.taskMu.Unlock()
	b.pid = pid
}

func (b *darwinBackend) setStep(tid int) {
	b.stepMu.Lock()
	b.stepping = true
	b.stepTID = tid
	b.stepMu.Unlock()
}

func (b *darwinBackend) clearStep() {
	b.stepMu.Lock()
	b.stepping = false
	b.stepTID = 0
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

func (b *darwinBackend) resumeAfterSignal(sig syscall.Signal) error {
	request := ptDarwinContinue
	data := uintptr(sig)
	if b.isStepping() {
		// Single-stepping over a software breakpoint: do NOT deliver the
		// signal now. Delivering it diverts the thread into the Go signal
		// handler, so the single-step traps at the handler's first instruction
		// and the original instruction at the breakpoint never executes; after
		// sigreturn the thread lands back on the reinstalled trap and re-hits
		// the breakpoint (surfacing a spurious BreakpointHit instead of the
		// Stepped we owe the caller, wedging the step-over). Step with data=0
		// so the real instruction retires. SIGURG (async preemption) and
		// SIGWINCH are best-effort and are re-sent by the runtime, so dropping
		// this one delivery during the ~1-instruction window is harmless.
		request = ptDarwinStep
		data = 0
	} else if sig == syscall.SIGURG {
		// SIGURG is Go's async-preemption signal. Re-delivering it through
		// PT_CONTINUE races the wait4 stop-conversion (a breakpoint SIGTRAP that
		// lands in the same window is lost), which wedges far more often than the
		// preemption it would forward is worth (measured ~5% vs ~2% hang). The Go
		// runtime re-sends preemption via sysmon, so dropping this delivery
		// (data=0) is safe and strictly better here.
		data = 0
	}
	if err := ptrace(request, uintptr(b.pid), 1, data); err != nil {
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

// task returns the Mach task port for the tracee, caching it after the first
// successful task_for_pid. Requires the com.apple.security.cs.debugger
// entitlement or disabled SIP. Caching is mandatory, not just an optimization:
// task_for_pid inserts a new send right on every call, so fetching it per
// memory/threads op leaks urefs until the kernel kills the tracee.
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
