//go:build arm64

package debugger

// archTrapInstruction is BRK #0 (0xD4200000, big-endian). arm64 instructions
// are 4 bytes and 4-byte aligned. The CPU stops with PC AT the BRK (unlike
// x86 INT3 which advances past it), so archRewindPC is the identity.
func archTrapInstruction() []byte { return []byte{0x00, 0x00, 0x20, 0xD4} }

func archRewindPC(pc uint64) uint64 { return pc }
