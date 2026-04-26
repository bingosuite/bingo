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
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

func newBackend() Backend {
	return &darwinBackend{}
}

type darwinBackend struct {
	pid      int
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
	b.stepping = false
	if err := ptrace(ptDarwinContinue, uintptr(b.pid), 1, 0); err != nil {
		return fmt.Errorf("PT_CONTINUE: %w", err)
	}
	return nil
}

func (b *darwinBackend) SingleStep(tid int) error {
	b.stepping = true
	b.stepTID = tid
	// Darwin PT_STEP is per-process (by PID), not per-thread. The tid argument
	// is a Mach thread port, not a valid ptrace identifier — use b.pid.
	if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0); err != nil {
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

// findBreakpointThread returns the Mach thread port and registers of the
// thread whose PC points at a BRK #0 — i.e. the one that hit our software BP.
// task_threads returns threads in creation order, so threads[0] is often an
// idle Go runtime M parked in pthread_cond_wait, not the goroutine running
// user code. Falls back to threads[0] if nothing is at a BRK.
func (b *darwinBackend) findBreakpointThread(threads []int) (int, Registers) {
	trap := archTrapInstruction()
	for _, t := range threads {
		regs, err := b.GetRegisters(t)
		if err != nil {
			continue
		}
		var buf [4]byte
		if err := b.ReadMemory(regs.PC, buf[:]); err != nil {
			continue
		}
		if buf[0] == trap[0] && buf[1] == trap[1] && buf[2] == trap[2] && buf[3] == trap[3] {
			return t, regs
		}
	}
	if len(threads) > 0 {
		regs, _ := b.GetRegisters(threads[0])
		return threads[0], regs
	}
	return 0, Registers{}
}

func (b *darwinBackend) Wait() (StopEvent, error) {
	for {
		var ws syscall.WaitStatus
		tid, err := syscall.Wait4(b.pid, &ws, 0, nil)
		if err != nil {
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
		// tid from Wait4 is the PID, not a Mach thread port. Use Threads()
		// to get the actual thread ports before reading registers.
		threads, terr := b.Threads()
		if terr != nil || len(threads) == 0 {
			return StopEvent{}, fmt.Errorf("Wait: get threads: %w", terr)
		}

		sig := ws.StopSignal()

		if sig == syscall.SIGTRAP {
			if b.stepping {
				// Use the thread we saved in SingleStep — that's the one
				// whose next instruction we asked to trap.
				thread := b.stepTID
				if thread == 0 {
					thread = threads[0]
				}
				regs, err := b.GetRegisters(thread)
				if err != nil {
					return StopEvent{}, err
				}
				b.stepping = false
				slog.Debug("Wait: SIGTRAP → SingleStep", "pc", fmt.Sprintf("0x%x", regs.PC))
				return StopEvent{Reason: StopSingleStep, TID: thread, PC: regs.PC}, nil
			}
			thread, regs := b.findBreakpointThread(threads)
			slog.Debug("Wait: SIGTRAP → Breakpoint", "pc", fmt.Sprintf("0x%x", regs.PC), "tid", thread)
			return StopEvent{Reason: StopBreakpoint, TID: thread, PC: regs.PC}, nil
		}

		// SIGURG is Go's goroutine-preemption signal. Re-deliver transparently.
		if sig == syscall.SIGURG {
			if b.stepping {
				_ = ptrace(ptDarwinStep, uintptr(b.pid), 1, 0)
			} else {
				_ = ptrace(ptDarwinContinue, uintptr(b.pid), 1, uintptr(sig))
			}
			continue
		}

		// SIGWINCH (terminal resize): handled by the Go runtime, re-deliver.
		if sig == syscall.SIGWINCH {
			if b.stepping {
				_ = ptrace(ptDarwinStep, uintptr(b.pid), 1, uintptr(sig))
			} else {
				_ = ptrace(ptDarwinContinue, uintptr(b.pid), 1, uintptr(sig))
			}
			continue
		}

		// Other signals during step: re-deliver via PT_STEP so the step
		// completes. If PT_STEP fails, fall through to StopSignal so the
		// engine can recover any in-flight step-over BP. Don't fall back to
		// PT_CONTINUE+continue: that loops on SIGURG every ~10ms forever.
		if b.stepping {
			if ptrace(ptDarwinStep, uintptr(b.pid), 1, uintptr(sig)) == nil {
				continue
			}
			b.stepping = false
		}
		// For non-SIGTRAP signals use threads[0] — without Mach exceptions we
		// have no way to know which thread received it.
		sigThread := threads[0]
		regs, err := b.GetRegisters(sigThread)
		if err != nil {
			return StopEvent{}, err
		}
		return StopEvent{Reason: StopSignal, TID: sigThread, PC: regs.PC, Signal: int(sig)}, nil
	}
}

func (b *darwinBackend) setPID(pid int) { b.pid = pid }

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
	defer f.Close()
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
