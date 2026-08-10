//go:build arm64

package debugger

import "encoding/binary"

// archTrapInstruction is BRK #0 (0xD4200000, big-endian). arm64 instructions
// are 4 bytes and 4-byte aligned. The CPU stops with PC AT the BRK (unlike
// x86 INT3 which advances past it), so archRewindPC is the identity.
func archTrapInstruction() []byte { return []byte{0x00, 0x00, 0x20, 0xD4} }

func archRewindPC(pc uint64) uint64 { return pc }

func archLiveTrapResumePC(b Backend, pc uint64) (uint64, bool, []byte, error) {
	instruction := make([]byte, 4)
	if err := b.ReadMemory(pc, instruction); err != nil {
		return 0, false, nil, err
	}
	const (
		brkMask = uint32(0xFFE0001F)
		brkBase = uint32(0xD4200000)
	)
	if binary.LittleEndian.Uint32(instruction)&brkMask == brkBase {
		return pc + 4, true, instruction, nil
	}
	return 0, false, instruction, nil
}
