//go:build amd64

package debugger

// archTrapInstruction is INT3 (0xCC). Patching this byte over any instruction
// causes the CPU to deliver a trap when that address executes.
func archTrapInstruction() []byte { return []byte{0xCC} }

// archRewindPC corrects PC after INT3: x86 advances RIP past the trap before
// delivering the exception, so we subtract 1 to recover the patched address.
func archRewindPC(pc uint64) uint64 { return pc - 1 }

func archTrapStartingAt(b Backend, pc uint64) (uint64, bool, error) {
	current := []byte{0}
	if err := b.ReadMemory(pc, current); err != nil {
		return 0, false, err
	}
	if current[0] == 0xCC || current[0] == 0xF1 {
		return pc + 1, true, nil
	}
	if current[0] != 0xCD {
		return 0, false, nil
	}
	int3 := make([]byte, 2)
	if err := b.ReadMemory(pc, int3); err != nil {
		return 0, false, err
	}
	if int3[1] == 0x03 {
		return pc + 2, true, nil
	}
	return 0, false, nil
}

func archLiveTrapResumePC(b Backend, rewoundPC uint64) (uint64, bool, error) {
	if resumePC, live, err := archTrapStartingAt(b, rewoundPC); err != nil || live {
		return resumePC, live, err
	}
	if rewoundPC == 0 {
		return 0, false, nil
	}
	int3 := make([]byte, 2)
	if err := b.ReadMemory(rewoundPC-1, int3); err != nil {
		return 0, false, err
	}
	if int3[0] == 0xCD && int3[1] == 0x03 {
		return rewoundPC + 1, true, nil
	}
	return 0, false, nil
}
