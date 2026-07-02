package debugger

import (
	"debug/dwarf"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"

	"github.com/bingosuite/bingo/pkg/protocol"
)

const optimizedOut = "<optimized out>"

// dwarfReader wraps *dwarf.Data with the operations the engine needs:
// PC-for-file:line lookup, PC-to-location mapping, and local variable reading.
//
// slide is the ASLR offset: actual_runtime_addr = dwarf_addr + slide.
type dwarfReader struct {
	data  *dwarf.Data
	slide int64

	// funcIndex holds every subprogram's [low,high) DWARF address range sorted
	// by low address, so functionAt can binary-search instead of linearly
	// decoding the whole .debug_info on every frame of every stop. A Go binary
	// has tens of thousands of subprograms and they are NOT emitted in address
	// order, so the old full scan was O(N) even for a hit; collecting frames for
	// each BreakpointHit/Stepped ran it once per frame and, under load, could
	// stall the (single-threaded) engine loop past the client's step timeout.
	funcIndex []funcRange
}

// funcRange is one subprogram's DWARF address range and name.
type funcRange struct {
	low, high uint64
	name      string
}

func openDWARF(binaryPath string) (*dwarfReader, error) {
	data, err := loadDWARFData(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("openDWARF %q: %w", binaryPath, err)
	}
	r := &dwarfReader{data: data}
	r.buildFuncIndex()
	return r, nil
}

// buildFuncIndex scans .debug_info once and records every subprogram's address
// range, sorted by low PC, for O(log N) functionAt lookups. Called once at load
// time (off the stepping hot path). A failure to decode simply leaves the index
// empty and functionAt falls back to a linear scan.
func (r *dwarfReader) buildFuncIndex() {
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagSubprogram {
			continue
		}
		lowpc, hasLow := entry.Val(dwarf.AttrLowpc).(uint64)
		if !hasLow {
			continue
		}
		highpc, ok := highPCValue(entry, lowpc)
		if !ok {
			continue
		}
		name, _ := entry.Val(dwarf.AttrName).(string)
		if name == "" {
			continue
		}
		r.funcIndex = append(r.funcIndex, funcRange{low: lowpc, high: highpc, name: name})
	}
	sort.Slice(r.funcIndex, func(i, j int) bool {
		return r.funcIndex[i].low < r.funcIndex[j].low
	})
}

func loadDWARFData(binaryPath string) (*dwarf.Data, error) {
	switch runtime.GOOS {
	case "linux":
		f, err := elf.Open(binaryPath)
		if err != nil {
			return nil, fmt.Errorf("elf.Open: %w", err)
		}
		defer func() { _ = f.Close() }()
		return f.DWARF()

	case "darwin":
		f, err := macho.Open(binaryPath)
		if err != nil {
			return nil, fmt.Errorf("macho.Open: %w", err)
		}
		defer func() { _ = f.Close() }()
		return f.DWARF()

	default:
		return nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// PCForFileLine returns the lowest is-stmt address for file:line. The file
// comparison is suffix-based so short names match absolute paths in DWARF.
func (r *dwarfReader) PCForFileLine(file string, line int) (uint64, error) {
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil {
			return 0, fmt.Errorf("DWARF reader: %w", err)
		}
		if entry == nil {
			break
		}
		if entry.Tag != dwarf.TagCompileUnit {
			continue
		}

		lr, err := r.data.LineReader(entry)
		if err != nil || lr == nil {
			continue
		}

		var le dwarf.LineEntry
		for {
			if err := lr.Next(&le); err != nil {
				break
			}
			if le.Line != line || !le.IsStmt {
				continue
			}
			if le.File != nil && fileMatches(le.File.Name, file) {
				return uint64(int64(le.Address) + r.slide), nil
			}
		}
	}
	return 0, fmt.Errorf("no address for %s:%d", file, line)
}

// NextLinePC returns the runtime address and line number of the first is-stmt
// entry in file with line > afterLine. (0, 0, false) if none exists.
func (r *dwarfReader) NextLinePC(file string, afterLine int) (uint64, int, bool) {
	bestLine := -1
	bestAddr := uint64(^uint64(0))

	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagCompileUnit {
			continue
		}
		lr, err := r.data.LineReader(entry)
		if err != nil || lr == nil {
			continue
		}
		var le dwarf.LineEntry
		for {
			if err := lr.Next(&le); err != nil {
				break
			}
			if !le.IsStmt || le.File == nil || !fileMatches(le.File.Name, file) {
				continue
			}
			if le.Line <= afterLine {
				continue
			}
			if bestLine < 0 || le.Line < bestLine ||
				(le.Line == bestLine && le.Address < bestAddr) {
				bestLine = le.Line
				bestAddr = le.Address
			}
		}
	}
	if bestLine < 0 {
		return 0, 0, false
	}
	return uint64(int64(bestAddr) + r.slide), bestLine, true
}

func fileMatches(candidate, target string) bool {
	return candidate == target || strings.HasSuffix(candidate, "/"+target)
}

// locationForPC resolves pc (runtime address) to a source location.
func (r *dwarfReader) locationForPC(pc uint64) protocol.Location {
	loc := protocol.Location{Function: r.functionAt(pc)}
	// When the subprogram index is populated, an empty function name is
	// authoritative: pc is not in any of our functions (e.g. a deep frame that
	// walked into libsystem/dyld). There is no source line to find, so skip the
	// compile-unit/line-table scan entirely — that scan is the other O(N) DWARF
	// walk that could stall the engine loop on a bad PC.
	if loc.Function == "" && len(r.funcIndex) > 0 {
		return loc
	}
	dwarfPC := uint64(int64(pc) - r.slide)

	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagCompileUnit {
			continue
		}

		// Skip CUs whose range can't contain dwarfPC, before reading line tables.
		if !cuContainsPC(entry, dwarfPC) {
			rd.SkipChildren()
			continue
		}

		lr, err := r.data.LineReader(entry)
		if err != nil || lr == nil {
			continue
		}

		// Keep the entry whose address is the greatest <= dwarfPC.
		var best dwarf.LineEntry
		var found bool
		var le dwarf.LineEntry
		for {
			if err := lr.Next(&le); err != nil {
				break
			}
			if le.Address <= dwarfPC {
				best = le
				found = true
			} else {
				break
			}
		}
		if found && best.File != nil {
			loc.File = best.File.Name
			loc.Line = best.Line
			return loc
		}
	}
	return loc
}

// cuContainsPC checks whether a CU's address range includes pc. Returns true
// when the CU has no range info, so the caller falls through to a full scan.
func cuContainsPC(entry *dwarf.Entry, pc uint64) bool {
	lowpc, hasLow := entry.Val(dwarf.AttrLowpc).(uint64)
	if !hasLow {
		return true
	}
	highpc, high := highPCValue(entry, lowpc)
	if !high {
		return true
	}
	return pc >= lowpc && pc < highpc
}

// functionAt returns the function name containing pc (runtime address), or "".
func (r *dwarfReader) functionAt(pc uint64) string {
	dwarfPC := uint64(int64(pc) - r.slide)

	// Fast path: binary-search the sorted subprogram index. Find the last range
	// whose low <= dwarfPC and check whether dwarfPC falls inside it.
	if len(r.funcIndex) > 0 {
		i := sort.Search(len(r.funcIndex), func(i int) bool {
			return r.funcIndex[i].low > dwarfPC
		})
		if i > 0 {
			fr := r.funcIndex[i-1]
			if dwarfPC >= fr.low && dwarfPC < fr.high {
				return fr.name
			}
		}
		return ""
	}

	// Fallback (index unavailable): linear scan of .debug_info.
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagSubprogram {
			continue
		}

		lowpc, hasLow := entry.Val(dwarf.AttrLowpc).(uint64)
		if !hasLow {
			continue
		}
		highpc, ok := highPCValue(entry, lowpc)
		if !ok {
			continue
		}
		name, _ := entry.Val(dwarf.AttrName).(string)
		if dwarfPC >= lowpc && dwarfPC < highpc && name != "" {
			return name
		}
	}
	return ""
}

// highPCValue extracts DW_AT_high_pc as an absolute address. The attribute may
// be uint64 (DWARF v2 absolute) or int64 (v4+ offset from low_pc).
func highPCValue(entry *dwarf.Entry, lowpc uint64) (uint64, bool) {
	v := entry.Val(dwarf.AttrHighpc)
	if v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case uint64:
		return val, true
	case int64:
		return lowpc + uint64(val), true
	}
	return 0, false
}

// FramesForStack resolves PCs (from the frame-pointer walk) to source frames.
func (r *dwarfReader) FramesForStack(pcs []uint64) []protocol.Frame {
	frames := make([]protocol.Frame, len(pcs))
	for i, pc := range pcs {
		frames[i] = protocol.Frame{
			Index:    i,
			Location: r.locationForPC(pc),
		}
	}
	return frames
}

// LocalsForFrame returns variables in the subprogram containing pc. Only
// DW_OP_addr (0x03) and DW_OP_fbreg (0x91) are evaluated; register-allocated
// variables come back as "<optimized out>".
func (r *dwarfReader) LocalsForFrame(b Backend, pc, frameBase uint64) ([]protocol.Variable, error) {
	dwarfPC := uint64(int64(pc) - r.slide)
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil {
			return nil, fmt.Errorf("DWARF LocalsForFrame: %w", err)
		}
		if entry == nil {
			break
		}
		if entry.Tag != dwarf.TagSubprogram {
			continue
		}

		lowpc, hasLow := entry.Val(dwarf.AttrLowpc).(uint64)
		if !hasLow {
			rd.SkipChildren()
			continue
		}
		highpc, ok := highPCValue(entry, lowpc)
		if !ok || dwarfPC < lowpc || dwarfPC >= highpc {
			rd.SkipChildren()
			continue
		}

		var vars []protocol.Variable
		for {
			child, err := rd.Next()
			if err == io.EOF || child == nil {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("DWARF child read: %w", err)
			}
			if child.Tag == 0 {
				break
			}
			if child.Tag != dwarf.TagVariable && child.Tag != dwarf.TagFormalParameter {
				continue
			}

			name, _ := child.Val(dwarf.AttrName).(string)
			typ := r.typeName(child)
			value := r.evalLocation(b, child, frameBase)

			vars = append(vars, protocol.Variable{
				Name:  name,
				Type:  typ,
				Value: value,
			})
		}
		return vars, nil
	}
	return nil, nil
}

func (r *dwarfReader) typeName(entry *dwarf.Entry) string {
	off, ok := entry.Val(dwarf.AttrType).(dwarf.Offset)
	if !ok {
		return "unknown"
	}
	tr := r.data.Reader()
	tr.Seek(off)
	te, err := tr.Next()
	if err != nil || te == nil {
		return "unknown"
	}
	name, _ := te.Val(dwarf.AttrName).(string)
	if name == "" {
		return te.Tag.String()
	}
	return name
}

func (r *dwarfReader) evalLocation(b Backend, entry *dwarf.Entry, frameBase uint64) string {
	loc := entry.Val(dwarf.AttrLocation)
	if loc == nil {
		return optimizedOut
	}
	expr, ok := loc.([]byte)
	if !ok || len(expr) == 0 {
		return optimizedOut
	}

	switch expr[0] {
	case 0x03: // DW_OP_addr — followed by an 8-byte LE DWARF-relative address
		if len(expr) < 9 {
			return optimizedOut
		}
		addr := binary.LittleEndian.Uint64(expr[1:9])
		addr = uint64(int64(addr) + r.slide)
		return r.readValueAt(b, addr)

	case 0x91: // DW_OP_fbreg — signed LEB128 offset from frame base
		if len(expr) < 2 {
			return optimizedOut
		}
		offset, _ := decodeSLEB128(expr[1:])
		addr := uint64(int64(frameBase) + offset)
		return r.readValueAt(b, addr)

	default:
		return optimizedOut
	}
}

// readValueAt reads 8 bytes and returns a hex string. A complete impl would
// use the DWARF type to format as int/string/slice header/etc.
func (r *dwarfReader) readValueAt(b Backend, addr uint64) string {
	var buf [8]byte
	if err := b.ReadMemory(addr, buf[:]); err != nil {
		return fmt.Sprintf("<unreadable: %v>", err)
	}
	return fmt.Sprintf("0x%x", binary.LittleEndian.Uint64(buf[:]))
}

// decodeSLEB128 decodes a signed LEB128 integer. Returns (value, bytesConsumed).
func decodeSLEB128(b []byte) (int64, int) {
	var result int64
	var shift uint
	for i, byt := range b {
		result |= int64(byt&0x7f) << shift
		shift += 7
		if byt&0x80 == 0 {
			if shift < 64 && (byt&0x40) != 0 {
				result |= -(1 << shift)
			}
			return result, i + 1
		}
	}
	return result, len(b)
}
