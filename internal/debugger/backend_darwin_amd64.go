//go:build darwin && amd64 && bingonative

package debugger

// backend_darwin_amd64.go is the complete Darwin/amd64 backend.
// It is entirely self-contained: struct, constants, process lifecycle,
// register access, memory access, and all Backend interface methods live here.
// Nothing is shared with the arm64 backend at the Go level.

import (
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
// PT_ATTACH is 14 — not 13, which is PT_SIGEXC.
const (
	ptDarwinContinue = uintptr(7)
	ptDarwinStep     = uintptr(9)
	ptDarwinAttach   = uintptr(14)
	ptDarwinDetach   = uintptr(11)
)

// ptrace calls the Darwin ptrace syscall via Syscall6.
// ptrace(2) takes four args beyond the syscall number; Syscall (not Syscall6)
// only handles three, so the fourth argument would be silently dropped.
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
	_ = ptrace(ptDarwinDetach, uintptr(pid), 1, 0)
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return p.Kill()
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

func (b *darwinBackend) SingleStep(tid int) error {
	b.stepping = true
	if err := ptrace(ptDarwinStep, uintptr(tid), 1, 0); err != nil {
		return fmt.Errorf("PT_STEP tid %d: %w", tid, err)
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

// ── Register access (Darwin/amd64 struct reg layout) ─────────────────────────
//
// PT_GETREGS (12) fills struct reg from <machine/reg.h>.
// Verified field offsets (8 bytes each):
//   80  r_rbp  ← BP    152  r_rip  ← PC    176  r_rsp  ← SP

const (
	ptGetRegs  = uintptr(12)
	ptSetRegs  = uintptr(13)
	amd64RegSz = 192
	offBP      = 80
	offPC      = 152
	offSP      = 176
)

func (b *darwinBackend) GetRegisters(tid int) (Registers, error) {
	var buf [amd64RegSz]byte
	if _, _, errno := syscall.Syscall6(syscall.SYS_PTRACE,
		ptGetRegs, uintptr(tid), uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0,
	); errno != 0 {
		return Registers{}, fmt.Errorf("PT_GETREGS tid %d: %w", tid, errno)
	}
	return Registers{
		PC: leU64(buf[:], offPC),
		SP: leU64(buf[:], offSP),
		BP: leU64(buf[:], offBP),
	}, nil
}

func (b *darwinBackend) SetRegisters(tid int, reg Registers) error {
	var buf [amd64RegSz]byte
	if _, _, errno := syscall.Syscall6(syscall.SYS_PTRACE,
		ptGetRegs, uintptr(tid), uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0,
	); errno != 0 {
		return fmt.Errorf("PT_GETREGS (pre-set) tid %d: %w", tid, errno)
	}
	putLeU64(buf[:], offPC, reg.PC)
	putLeU64(buf[:], offSP, reg.SP)
	putLeU64(buf[:], offBP, reg.BP)
	if _, _, errno := syscall.Syscall6(syscall.SYS_PTRACE,
		ptSetRegs, uintptr(tid), uintptr(unsafe.Pointer(&buf[0])), 0, 0, 0,
	); errno != 0 {
		return fmt.Errorf("PT_SETREGS tid %d: %w", tid, errno)
	}
	return nil
}

// ── Memory access (PT_READ_D / PT_WRITE_D) ────────────────────────────────────

func (b *darwinBackend) ReadMemory(addr uint64, dst []byte) error {
	const ptReadD = uintptr(2)
	wordSize := int(unsafe.Sizeof(uintptr(0)))
	for i := 0; i < len(dst); i += wordSize {
		word, _, errno := syscall.Syscall6(syscall.SYS_PTRACE,
			ptReadD, uintptr(b.pid), uintptr(addr)+uintptr(i), 0, 0, 0,
		)
		if errno != 0 {
			return fmt.Errorf("PT_READ_D 0x%x: %w", addr+uint64(i), errno)
		}
		for j := 0; j < wordSize && i+j < len(dst); j++ {
			dst[i+j] = byte(word >> (j * 8))
		}
	}
	return nil
}

func (b *darwinBackend) WriteMemory(addr uint64, src []byte) error {
	const ptReadD = uintptr(2)
	const ptWriteD = uintptr(4)
	wordSize := int(unsafe.Sizeof(uintptr(0)))
	for i := 0; i < len(src); i += wordSize {
		existing, _, errno := syscall.Syscall6(syscall.SYS_PTRACE,
			ptReadD, uintptr(b.pid), uintptr(addr)+uintptr(i), 0, 0, 0,
		)
		if errno != 0 {
			return fmt.Errorf("PT_READ_D (rmw) 0x%x: %w", addr+uint64(i), errno)
		}
		word := uint64(existing)
		for j := 0; j < wordSize && i+j < len(src); j++ {
			shift := uint(j * 8)
			word = (word &^ (0xff << shift)) | (uint64(src[i+j]) << shift)
		}
		if _, _, errno := syscall.Syscall6(syscall.SYS_PTRACE,
			ptWriteD, uintptr(b.pid), uintptr(addr)+uintptr(i), uintptr(word), 0, 0,
		); errno != 0 {
			return fmt.Errorf("PT_WRITE_D 0x%x: %w", addr+uint64(i), errno)
		}
	}
	return nil
}

// ── Thread enumeration ────────────────────────────────────────────────────────

// Threads returns the main thread only. Full enumeration via task_threads
// requires cgo; the ptrace-only amd64 backend omits it.
func (b *darwinBackend) Threads() ([]int, error) {
	return []int{b.pid}, nil
}

// ── Wait ──────────────────────────────────────────────────────────────────────

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
		if ws.StopSignal() == syscall.SIGTRAP {
			regs, err := b.GetRegisters(tid)
			if err != nil {
				return StopEvent{}, err
			}
			if b.stepping {
				b.stepping = false
				return StopEvent{Reason: StopSingleStep, TID: tid, PC: regs.PC}, nil
			}
			return StopEvent{Reason: StopBreakpoint, TID: tid, PC: archRewindPC(regs.PC)}, nil
		}
		regs, err := b.GetRegisters(tid)
		if err != nil {
			return StopEvent{}, err
		}
		return StopEvent{Reason: StopSignal, TID: tid, PC: regs.PC, Signal: int(ws.StopSignal())}, nil
	}
}

func (b *darwinBackend) setPID(pid int) { b.pid = pid }

var _ Backend = (*darwinBackend)(nil)

// ── Little-endian helpers ─────────────────────────────────────────────────────

func leU64(b []byte, off int) uint64 {
	s := b[off:]
	return uint64(s[0]) | uint64(s[1])<<8 | uint64(s[2])<<16 | uint64(s[3])<<24 |
		uint64(s[4])<<32 | uint64(s[5])<<40 | uint64(s[6])<<48 | uint64(s[7])<<56
}

func putLeU64(b []byte, off int, v uint64) {
	b[off+0] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
	b[off+4] = byte(v >> 32)
	b[off+5] = byte(v >> 40)
	b[off+6] = byte(v >> 48)
	b[off+7] = byte(v >> 56)
}
