//go:build amd64

package debugger

// archTrapInstruction is INT3 (0xCC). Patching this byte over any instruction
// causes the CPU to deliver a trap when that address executes.
func archTrapInstruction() []byte { return []byte{0xCC} }

// archRewindPC corrects PC after INT3: x86 advances RIP past the trap before
// delivering the exception, so we subtract 1 to recover the patched address.
func archRewindPC(pc uint64) uint64 { return pc - 1 }
