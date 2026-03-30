//go:build amd64

package debugger

// trap_amd64.go defines the two architecture-specific constants that every
// amd64 backend (Linux, Darwin, Windows) needs for breakpoint management.
// Build tag is amd64 only — no OS restriction.

// archTrapInstruction returns the single-byte INT3 instruction (0xCC).
// Writing this byte over any instruction in the tracee's text segment causes
// the CPU to deliver a trap when that address is executed.
func archTrapInstruction() []byte { return []byte{0xCC} }

// archRewindPC corrects the program counter after an INT3 trap.
// The x86 CPU advances RIP past the INT3 before delivering the exception,
// so the reported PC is one byte past the address we patched.
// We subtract 1 to recover the breakpoint address for table lookup.
func archRewindPC(pc uint64) uint64 { return pc - 1 }
