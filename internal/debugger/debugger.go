package debugger

import (
	"fmt"
	"syscall"

	"github.com/bingosuite/bingo/internal/debuginfo"
)

var (
	bpCode = []byte{0xCC}
)

func SetBreakpoint(d *debuginfo.DebugInfo, pc uint64) error {
	original := make([]byte, len(bpCode))
	if _, err := syscall.PtracePeekData(d.Target.PID, uintptr(pc), original); err != nil {
		return fmt.Errorf("failed to read original machine code into memory: %v", err)
	}
	if _, err := syscall.PtracePokeData(d.Target.PID, uintptr(pc), bpCode); err != nil {
		return fmt.Errorf("failed to write breakpoint into memory: %v", err)
	}
	d.Breakpoints[pc] = original
	return nil
}

func ClearBreakpoint(d *debuginfo.DebugInfo, pc uint64) error {
	if _, err := syscall.PtracePokeData(d.Target.PID, uintptr(pc), d.Breakpoints[pc]); err != nil {
		return fmt.Errorf("failed to write breakpoint into memory: %v", err)
	}
	return nil
}
