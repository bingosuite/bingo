//go:build darwin && arm64 && bingonative

package debugger

// backend_darwin_arm64.go is the complete Darwin/arm64 (Apple Silicon) backend.
// It is entirely self-contained: struct, constants, process lifecycle, register
// access via Mach thread_get_state, memory access via mach_vm_read/write, and
// all Backend interface methods live here.
//
// Requires: com.apple.security.cs.debugger entitlement, or SIP disabled.
// Cannot be cross-compiled from Linux/Windows — cgo requires the macOS SDK.

/*
#cgo LDFLAGS: -framework CoreFoundation

#include "mach_darwin_arm64.h"
*/
import "C"

import (
	"debug/macho"
	"fmt"
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
}

// Darwin ptrace request codes from <sys/ptrace.h>.
const (
	ptDarwinContinue = uintptr(7)
	ptDarwinStep     = uintptr(9)
	ptDarwinAttach   = uintptr(14) // PT_ATTACH — not 13 (that is PT_SIGEXC)
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

// ── Process lifecycle ─────────────────────────────────────────────────────────

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
	// Wait4 with a specific PID only works for natural children on Darwin.
	// PT_ATTACH'd processes are not natural children, so use -1 (any child)
	// to collect the post-attach SIGSTOP notification.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(-1, &ws, 0, nil); err != nil {
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
	// Attached (not launched) process: detach only, do not kill.
	// The debugger does not own this process; leaving it running is correct.
	_ = ptrace(ptDarwinDetach, uintptr(pid), 1, 0)
	return nil
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

// ── Backend implementation ────────────────────────────────────────────────────

func (b *darwinBackend) ContinueProcess() error {
	b.stepping = false
	if err := ptrace(ptDarwinContinue, uintptr(b.pid), 1, 0); err != nil {
		return fmt.Errorf("PT_CONTINUE: %w", err)
	}
	return nil
}

func (b *darwinBackend) SingleStep(_ int) error {
	b.stepping = true
	// Darwin ptrace PT_STEP operates on the whole process (by PID), not
	// per-thread. The tid argument (a Mach thread port on ARM64) is not
	// a valid ptrace process identifier, so we always use b.pid here.
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

// ── Register access via Mach thread_get_state ─────────────────────────────────

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

// ── Memory access via Mach mach_vm_read/write ─────────────────────────────────

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

// ── Thread enumeration via task_threads ───────────────────────────────────────

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

// ── Wait ──────────────────────────────────────────────────────────────────────

func (b *darwinBackend) Wait() (StopEvent, error) {
	for {
		var ws syscall.WaitStatus
		// Use -1 (any child) instead of a specific PID: Wait4(specific_pid)
		// only works for natural children on Darwin. PT_ATTACH'd processes
		// aren't natural children, so specific-PID wait blocks forever.
		tid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}
		if tid != b.pid {
			// Stop from an unexpected process — ignore and loop.
			continue
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
		// tid from Wait4 is the process PID, not a Mach thread port.
		// Use Threads() to get the actual thread port before calling GetRegisters.
		threads, terr := b.Threads()
		if terr != nil || len(threads) == 0 {
			return StopEvent{}, fmt.Errorf("Wait: get threads: %w", terr)
		}
		thread := threads[0]

		if ws.StopSignal() == syscall.SIGTRAP {
			regs, err := b.GetRegisters(thread)
			if err != nil {
				return StopEvent{}, err
			}
			if b.stepping {
				b.stepping = false
				return StopEvent{Reason: StopSingleStep, TID: thread, PC: regs.PC}, nil
			}
			return StopEvent{Reason: StopBreakpoint, TID: thread, PC: archRewindPC(regs.PC)}, nil
		}
		// Non-TRAP signal delivered during a single-step (e.g. Go's SIGURG
		// goroutine-preemption signal). Re-deliver it with PT_STEP so the
		// step completes without the engine seeing a spurious StopSignal and
		// calling ContinueProcess(), which would abort the step.
		if b.stepping {
			sig := uintptr(ws.StopSignal())
			if perr := ptrace(ptDarwinStep, uintptr(b.pid), 1, sig); perr == nil {
				continue
			}
		}
		regs, err := b.GetRegisters(thread)
		if err != nil {
			return StopEvent{}, err
		}
		return StopEvent{Reason: StopSignal, TID: thread, PC: regs.PC, Signal: int(ws.StopSignal())}, nil
	}
}

func (b *darwinBackend) setPID(pid int) { b.pid = pid }

// TextSlide returns the ASLR slide for the main executable: the difference
// between where the binary was actually loaded and its preferred __TEXT vmaddr.
// Returns 0 on any error (slide is treated as absent).
//
// We scan the task's VM map for the first executable region whose header has
// the 64-bit Mach-O magic. This works even at the very first ptrace stop
// (before dyld has run), because the kernel maps the binary before handing
// control to dyld — unlike TASK_DYLD_INFO whose image array is unpopulated
// at that point.
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

// machoTextVmaddr returns the preferred load address of the __TEXT segment
// from the Mach-O binary file (the non-ASLR base address).
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

// ── Mach helpers ──────────────────────────────────────────────────────────────

// task returns the Mach task port for the tracee.
// Requires com.apple.security.cs.debugger entitlement or SIP disabled.
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
