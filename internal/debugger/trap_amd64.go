//go:build amd64

package debugger

// archTrapInstruction is INT3 (0xCC). Patching this byte over any instruction
// causes the CPU to deliver a trap when that address executes.
func archTrapInstruction() []byte { return []byte{0xCC} }

// archRewindPC corrects PC after INT3: x86 advances RIP past the trap before
// delivering the exception, so we subtract 1 to recover the patched address.
func archRewindPC(pc uint64) uint64 { return pc - 1 }

// archBreakpointTrapMovesPC reports whether the CPU leaves PC past the trap
// instruction after a software breakpoint. On x86 INT3 advances RIP to addr+1,
// so the engine must rewind the trapping thread's real PC to the breakpoint
// address before restoring the original instruction and single-stepping.
func archBreakpointTrapMovesPC() bool { return true }
