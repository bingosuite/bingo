//go:build linux && amd64

package debugger

// arch_amd64.go provides Linux/amd64 register access via PTRACE_GETREGS /
// PTRACE_SETREGS. These syscalls are Linux-specific; Darwin/amd64 register
// access lives in darwin_amd64.go.
//
// archTrapInstruction and archRewindPC are defined in trap_amd64.go (no OS
// restriction) so that breakpoint.go and all three backends can use them.

import (
	"fmt"
	"syscall"
)

// archGetRegisters reads the full integer register set for thread tid via
// PTRACE_GETREGS and extracts the four fields the engine needs.
func archGetRegisters(tid int) (Registers, error) {
	var r syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(tid, &r); err != nil {
		return Registers{}, fmt.Errorf("PTRACE_GETREGS tid %d: %w", tid, err)
	}
	return Registers{
		PC:  r.Rip,
		SP:  r.Rsp,
		BP:  r.Rbp,
		TLS: r.Fs_base, // Go runtime stores the g pointer at FS_BASE on amd64
	}, nil
}

// archSetRegisters writes back the four engine-owned fields.
// Reads the full register set first to preserve everything else.
func archSetRegisters(tid int, reg Registers) error {
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
