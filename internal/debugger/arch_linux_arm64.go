//go:build linux && arm64

package debugger

// arch_arm64.go provides Linux/arm64 register access via PTRACE_GETREGSET /
// PTRACE_SETREGSET with NT_PRSTATUS.
//
// PTRACE_GETREGS does not exist on arm64; PTRACE_GETREGSET is the required
// alternative. Go's syscall package doesn't expose PTRACE_GETREGSET on arm64
// so we call it via raw Syscall6 with the kernel constant directly.
//
// archTrapInstruction and archRewindPC are defined in trap_arm64.go (no OS
// restriction) so that breakpoint.go and all arm64 backends can use them.

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ── Register layout ───────────────────────────────────────────────────────────
//
// struct user_pt_regs from <asm/ptrace.h>:
//
//   struct user_pt_regs {
//       __u64 regs[31];   // X0–X30
//       __u64 sp;
//       __u64 pc;
//       __u64 pstate;
//   };                    // 34 × 8 = 272 bytes
//
// Go arm64 calling convention:
//   X28 = g  (goroutine pointer — our TLS)
//   X29 = FP (frame pointer    — our BP)
//   X30 = LR (link register, not needed by engine)

const ntPRSTATUS = 1

type userPtRegs struct {
	Regs   [31]uint64
	SP     uint64
	PC     uint64
	PState uint64
}

func archGetRegisters(tid int) (Registers, error) {
	var r userPtRegs
	iov := syscall.Iovec{
		Base: (*byte)(unsafe.Pointer(&r)),
		Len:  uint64(unsafe.Sizeof(r)),
	}
	if err := ptraceGetRegset(tid, ntPRSTATUS, &iov); err != nil {
		return Registers{}, fmt.Errorf("PTRACE_GETREGSET tid %d: %w", tid, err)
	}
	return Registers{
		PC:  r.PC,
		SP:  r.SP,
		BP:  r.Regs[29],
		TLS: r.Regs[28],
	}, nil
}

func archSetRegisters(tid int, reg Registers) error {
	var r userPtRegs
	iov := syscall.Iovec{
		Base: (*byte)(unsafe.Pointer(&r)),
		Len:  uint64(unsafe.Sizeof(r)),
	}
	if err := ptraceGetRegset(tid, ntPRSTATUS, &iov); err != nil {
		return fmt.Errorf("PTRACE_GETREGSET (pre-set) tid %d: %w", tid, err)
	}
	r.PC = reg.PC
	r.SP = reg.SP
	r.Regs[29] = reg.BP
	r.Regs[28] = reg.TLS
	iov.Len = uint64(unsafe.Sizeof(r))
	if err := ptraceSetRegset(tid, ntPRSTATUS, &iov); err != nil {
		return fmt.Errorf("PTRACE_SETREGSET tid %d: %w", tid, err)
	}
	return nil
}

// ── PTRACE_GETREGSET / PTRACE_SETREGSET ───────────────────────────────────────

const (
	ptrace_GETREGSET = uintptr(0x4204)
	ptrace_SETREGSET = uintptr(0x4205)
)

func ptraceGetRegset(tid, regset int, iov *syscall.Iovec) error {
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PTRACE,
		ptrace_GETREGSET,
		uintptr(tid),
		uintptr(regset),
		uintptr(unsafe.Pointer(iov)),
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("PTRACE_GETREGSET tid=%d: %w", tid, errno)
	}
	return nil
}

func ptraceSetRegset(tid, regset int, iov *syscall.Iovec) error {
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PTRACE,
		ptrace_SETREGSET,
		uintptr(tid),
		uintptr(regset),
		uintptr(unsafe.Pointer(iov)),
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("PTRACE_SETREGSET tid=%d: %w", tid, errno)
	}
	return nil
}
