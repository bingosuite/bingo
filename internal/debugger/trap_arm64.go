//go:build arm64

package debugger

// trap_arm64.go defines the two architecture-specific constants that every
// arm64 backend (Linux, Darwin) needs for breakpoint management.
// Build tag is arm64 only — no OS restriction.

// archTrapInstruction returns the 4-byte BRK #0 instruction.
// Encoding: 0xD4200000 big-endian = {0x00, 0x00, 0x20, 0xD4} little-endian.
// arm64 instructions are always 4 bytes and must be 4-byte aligned.
// The CPU stops with PC pointing AT the BRK (unlike x86 INT3 which advances past it).
func archTrapInstruction() []byte { return []byte{0x00, 0x00, 0x20, 0xD4} }

// archRewindPC is the identity on arm64: the CPU delivers the trap with PC
// already pointing at the BRK instruction, so no adjustment is needed.
func archRewindPC(pc uint64) uint64 { return pc }
