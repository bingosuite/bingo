//go:build amd64

package debugger

// archTrapInstruction is INT3 (0xCC). Patching this byte over any instruction
// causes the CPU to deliver a trap when that address executes.
func archTrapInstruction() []byte { return []byte{0xCC} }

// archRewindPC corrects PC after INT3: x86 advances RIP past the trap before
// delivering the exception, so we subtract 1 to recover the patched address.
func archRewindPC(pc uint64) uint64 { return pc - 1 }

// archLiveTrapResumePC recognises x86 trap instructions at, or crossing into,
// the canonical one-byte rewind address. The crossing case matters for CD 03:
// RIP advances by two, so subtracting one points at its second byte.
func archLiveTrapResumePC(b Backend, pc uint64) (uint64, bool, []byte, error) {
	current := []byte{0}
	if err := b.ReadMemory(pc, current); err != nil {
		return 0, false, nil, err
	}
	switch current[0] {
	case 0xCC, 0xF1:
		return pc + 1, true, current, nil
	case 0xCD:
		int3 := make([]byte, 2)
		if err := b.ReadMemory(pc, int3); err != nil {
			return 0, false, nil, err
		}
		if int3[1] == 0x03 {
			return pc + 2, true, current, nil
		}
	case 0x03:
		if pc == 0 {
			return 0, false, current, nil
		}
		int3 := make([]byte, 2)
		if err := b.ReadMemory(pc-1, int3); err != nil {
			return 0, false, nil, err
		}
		if int3[0] == 0xCD && int3[1] == 0x03 {
			return pc + 1, true, current, nil
		}
	}
	return 0, false, current, nil
}
