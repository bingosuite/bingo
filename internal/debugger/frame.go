package debugger

import (
	"encoding/binary"
	"runtime"
	"sort"
)

// frameTable is a minimal DWARF .debug_frame Call Frame Information reader that
// resolves only the CFA (Canonical Frame Address) column at a given PC. Go's
// DW_AT_frame_base for every function is DW_OP_call_frame_cfa, and locals are
// addressed as DW_OP_fbreg offsets from that CFA — so reading a local's value
// requires the CFA at the stopped PC.
//
// The CFA is NOT simply frame-pointer + 16 on arm64: Go points the frame
// pointer (x29) at the saved FP/LR pair at the BOTTOM of the frame and lays
// locals out ABOVE it, so the CFA (the caller's SP at the call) is
// frame-pointer + framesize, and the frame size is per-function. Only CFI
// encodes it. On amd64 the body rule is frame-pointer + 16, which CFI also
// encodes, so the same evaluator serves both arches.
//
// Register-rule opcodes (offset/restore/register/expression) are parsed for
// correct operand skipping but otherwise ignored — the CFA column is all the
// locals reader needs. Mirrors the CFA half of Delve's frame reader.
type frameTable struct {
	fdes []fde // sorted by loc for binary search
}

type cie struct {
	codeAlign uint64
	dataAlign int64
	initInstr []byte
}

type fde struct {
	loc, end uint64
	c        *cie
	instr    []byte
}

// parseFrameTable parses a raw (decompressed) .debug_frame section. A parse
// error yields nil so callers fall back to the frame-pointer heuristic rather
// than erroring a stop.
func parseFrameTable(data []byte) *frameTable {
	defer func() { _ = recover() }() // malformed CFI must never crash the engine
	ft := &frameTable{}
	cies := map[uint64]*cie{}
	p := 0
	for p+4 <= len(data) {
		start := uint64(p)
		length := int(binary.LittleEndian.Uint32(data[p:]))
		p += 4
		if length == 0 || p+length > len(data) {
			break
		}
		end := p + length
		id := binary.LittleEndian.Uint32(data[p:])
		p += 4
		if id == 0xffffffff {
			cies[start] = parseCIE(data[p:end])
		} else if c := cies[uint64(id)]; c != nil {
			loc := binary.LittleEndian.Uint64(data[p:])
			rng := binary.LittleEndian.Uint64(data[p+8:])
			ft.fdes = append(ft.fdes, fde{loc: loc, end: loc + rng, c: c, instr: data[p+16 : end]})
		}
		p = end
	}
	sort.Slice(ft.fdes, func(i, j int) bool { return ft.fdes[i].loc < ft.fdes[j].loc })
	if len(ft.fdes) == 0 {
		return nil
	}
	return ft
}

func parseCIE(b []byte) *cie {
	p := 0
	version := b[p]
	p++
	for b[p] != 0 { // augmentation string
		p++
	}
	p++
	if version >= 4 {
		p += 2 // address_size, segment_selector_size
	}
	ca := readULEB(b, &p)
	da := readSLEB(b, &p)
	if version == 1 { // return_address_register
		p++
	} else {
		readULEB(b, &p)
	}
	return &cie{codeAlign: ca, dataAlign: da, initInstr: b[p:]}
}

// cfa returns the Canonical Frame Address at pc. regVal supplies the runtime
// value of a DWARF register (SP/FP); pc is the unslid DWARF address. ok is
// false when no FDE covers pc, the referenced register is unavailable, or CFI
// is malformed — callers then fall back to a frame-pointer offset.
func (ft *frameTable) cfa(pc uint64, regVal func(dwarfReg uint64) (uint64, bool)) (addr uint64, ok bool) {
	if ft == nil {
		return 0, false
	}
	defer func() {
		if recover() != nil {
			addr, ok = 0, false
		}
	}()
	i := sort.Search(len(ft.fdes), func(i int) bool { return ft.fdes[i].end > pc })
	if i >= len(ft.fdes) || pc < ft.fdes[i].loc {
		return 0, false
	}
	f := ft.fdes[i]
	reg, off, found := runCFAProgram(f.c, f.loc, f.instr, pc)
	if !found {
		return 0, false
	}
	base, ok := regVal(reg)
	if !ok {
		return 0, false
	}
	return uint64(int64(base) + off), true
}

// runCFAProgram executes the CIE initial instructions then the FDE
// instructions, advancing the location counter until it would pass pc, and
// returns the CFA rule (register + offset) in effect at pc. Only the
// CFA-defining opcodes are acted on here; every other opcode is delegated to
// skipCFAOperand, which advances the location counter or consumes operands so
// the instruction stream stays in sync (its effect on non-CFA columns is
// irrelevant to the locals reader).
func runCFAProgram(c *cie, startLoc uint64, fdeInstr []byte, pc uint64) (reg uint64, off int64, ok bool) {
	loc := startLoc
	type snap struct {
		r uint64
		o int64
	}
	var stack []snap
	exec := func(b []byte) {
		p := 0
		for p < len(b) {
			if loc > pc {
				return
			}
			op := b[p]
			p++
			switch op >> 6 {
			case 1: // DW_CFA_advance_loc
				loc += uint64(op&0x3f) * c.codeAlign
				continue
			case 2: // DW_CFA_offset
				readULEB(b, &p)
				continue
			case 3: // DW_CFA_restore
				continue
			}
			switch op & 0x3f {
			case 0x0a: // remember_state
				stack = append(stack, snap{reg, off})
			case 0x0b: // restore_state
				if n := len(stack); n > 0 {
					reg, off = stack[n-1].r, stack[n-1].o
					stack = stack[:n-1]
				}
			case 0x0c: // def_cfa
				reg = readULEB(b, &p)
				off = int64(readULEB(b, &p))
			case 0x0d: // def_cfa_register
				reg = readULEB(b, &p)
			case 0x0e: // def_cfa_offset
				off = int64(readULEB(b, &p))
			case 0x12: // def_cfa_sf
				reg = readULEB(b, &p)
				off = readSLEB(b, &p) * c.dataAlign
			case 0x13: // def_cfa_offset_sf
				off = readSLEB(b, &p) * c.dataAlign
			default:
				if !skipCFAOperand(op, b, &p, c, &loc) {
					return // unknown opcode: stop rather than desync
				}
			}
		}
	}
	exec(c.initInstr)
	exec(fdeInstr)
	return reg, off, true
}

// skipCFAOperand handles every CFI opcode that does not redefine the tracked
// CFA register/offset: the location-advancing opcodes (mutating *loc) and the
// register-rule opcodes (whose operands are consumed so the stream stays in
// sync). It returns false for an unrecognized opcode so the caller stops rather
// than desyncing.
func skipCFAOperand(op byte, b []byte, p *int, c *cie, loc *uint64) bool {
	switch op & 0x3f {
	case 0x00: // nop
	case 0x01: // set_loc (absolute)
		*loc = binary.LittleEndian.Uint64(b[*p:])
		*p += 8
	case 0x02: // advance_loc1
		*loc += uint64(b[*p]) * c.codeAlign
		*p++
	case 0x03: // advance_loc2
		*loc += uint64(binary.LittleEndian.Uint16(b[*p:])) * c.codeAlign
		*p += 2
	case 0x04: // advance_loc4
		*loc += uint64(binary.LittleEndian.Uint32(b[*p:])) * c.codeAlign
		*p += 4
	case 0x06, 0x07, 0x08: // restore_extended, undefined, same_value
		readULEB(b, p)
	case 0x05, 0x09, 0x14: // offset_extended, register, val_offset
		readULEB(b, p)
		readULEB(b, p)
	case 0x11, 0x15: // offset_extended_sf, val_offset_sf
		readULEB(b, p)
		readSLEB(b, p)
	case 0x0f: // def_cfa_expression: block only
		*p += int(readULEB(b, p))
	case 0x10, 0x16: // expression, val_expression: register + block
		readULEB(b, p)
		*p += int(readULEB(b, p))
	default:
		return false
	}
	return true
}

func readULEB(b []byte, p *int) uint64 {
	var r uint64
	var s uint
	for {
		c := b[*p]
		*p++
		r |= uint64(c&0x7f) << s
		if c&0x80 == 0 {
			break
		}
		s += 7
	}
	return r
}

func readSLEB(b []byte, p *int) int64 {
	var r int64
	var s uint
	var c byte
	for {
		c = b[*p]
		*p++
		r |= int64(c&0x7f) << s
		s += 7
		if c&0x80 == 0 {
			break
		}
	}
	if s < 64 && c&0x40 != 0 {
		r |= -1 << s
	}
	return r
}

// dwarfSPReg and dwarfFPReg are the DWARF register numbers for the stack and
// frame pointers on each supported arch (arm64: SP=31, x29=29; amd64: RSP=7,
// RBP=6). Both are needed because Go's CFA rule is SP-relative on arm64 and
// FP-relative on amd64.
func dwarfSPReg() uint64 {
	if runtime.GOARCH == "arm64" {
		return 31
	}
	return 7
}

func dwarfFPReg() uint64 {
	if runtime.GOARCH == "arm64" {
		return 29
	}
	return 6
}

// cfa resolves the Canonical Frame Address at runtime PC using the given
// runtime stack- and frame-pointer values. It unslides the PC to match the
// DWARF-encoded FDE ranges. ok is false when CFI is absent or does not cover
// the PC, so the caller can fall back to a frame-pointer offset.
func (r *dwarfReader) cfa(pc, sp, fp uint64) (uint64, bool) {
	if r.frame == nil {
		return 0, false
	}
	dwarfPC := uint64(int64(pc) - r.slide)
	return r.frame.cfa(dwarfPC, func(reg uint64) (uint64, bool) {
		switch reg {
		case dwarfSPReg():
			return sp, true
		case dwarfFPReg():
			return fp, true
		}
		return 0, false
	})
}
