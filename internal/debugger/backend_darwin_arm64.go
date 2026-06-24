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
	if err := ptrace(ptDarwinContinue, uintptr(b.pid), 1, 0); err != nil {
		return fmt.Errorf("PT_CONTINUE: %w", err)
	}
	return nil
}

func (b *darwinBackend) SingleStep(tid int) error {
	b.setStep(tid)
	// Darwin PT_STEP is per-process (by PID), not per-thread. The tid argument
	// is a Mach thread port, not a valid ptrace identifier — use b.pid.
	if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0); err != nil {
		b.clearStep()
		return fmt.Errorf("PT_STEP: %w", err)
	}
	return nil
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

		// Other signals during step: re-deliver via PT_STEP so the step
		// completes. If PT_STEP fails, fall through to StopSignal so the
		// engine can recover any in-flight step-over BP. Don't fall back to
		// PT_CONTINUE+continue: that loops on SIGURG every ~10ms forever.
		if b.isStepping() {
			if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, uintptr(sig)); err == nil {
				continue
			} else if isNoSuchProcess(err) {
				return StopEvent{Reason: StopKilled, TID: tid}, nil
			}
			b.clearStep()
		}
		return StopEvent{Reason: StopSignal, Signal: int(sig)}, nil
	}
}

func (b *darwinBackend) setPID(pid int) { b.pid = pid }

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
	if b.isStepping() {
		request = ptDarwinStep
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
