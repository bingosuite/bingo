package debugger

import (
	"bytes"
	"compress/zlib"
	"debug/dwarf"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"

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

	// frame is the parsed .debug_frame CFA evaluator, or nil when the binary
	// carries no frame section. It resolves Go's DW_AT_frame_base
	// (DW_OP_call_frame_cfa) so fbreg locals resolve against the true CFA
	// rather than a fixed frame-pointer offset. See frame.go.
	frame *frameTable

	// funcIndex is a lazily-built, lowpc-sorted table of every subprogram's
	// [low,high) DWARF PC range and name. functionAt binary-searches it instead
	// of linearly scanning every DIE in the binary on each call. Without this,
	// resolving a stack (up to maxStackDepth PCs, each a full scan of the tens of
	// thousands of DIEs in a Go binary) can take many seconds under -race and
	// wedge the single-threaded engine loop past the client's timeout.
	funcIndexOnce sync.Once
	funcIndex     []funcRange

	// cacheMu guards the lazily-populated runtime-introspection caches below.
	// Struct layouts and variable addresses never change for a loaded image, so
	// each is resolved from DWARF at most once. Inspection already runs on the
	// serialized engine loop, but these are guarded anyway so the reader is safe
	// to share.
	cacheMu    sync.Mutex
	varAddrs   map[string]uint64       // package var name → runtime DW_OP_addr (slid); 0 means "resolved, absent"
	structOffs map[string]structLayout // struct name → member offsets
}

// structLayout maps a struct's member names to their byte offsets. found is
// false when the struct type was absent from DWARF, so callers can distinguish
// "no such struct" from "struct with a zero-offset first member".
type structLayout struct {
	found   bool
	offsets map[string]int64
}

// funcRange is one subprogram's DWARF PC range (unslid) and name.
type funcRange struct {
	low, high uint64
	name      string
}

func openDWARF(binaryPath string) (*dwarfReader, error) {
	data, frameBytes, err := loadDWARFData(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("openDWARF %q: %w", binaryPath, err)
	}
	dr := &dwarfReader{data: data}
	if len(frameBytes) > 0 {
		dr.frame = parseFrameTable(frameBytes)
	}
	return dr, nil
}

// loadDWARFData returns the parsed DWARF plus the raw (decompressed)
// .debug_frame bytes. The frame section backs the CFA evaluator (frame.go);
// debug/dwarf does not parse Call Frame Information, so we read the section
// ourselves. A missing frame section is not an error — CFA resolution then
// degrades to the frame-pointer fallback in engine.frameLocation.
func loadDWARFData(binaryPath string) (*dwarf.Data, []byte, error) {
	switch runtime.GOOS {
	case "linux":
		f, err := elf.Open(binaryPath)
		if err != nil {
			return nil, nil, fmt.Errorf("elf.Open: %w", err)
		}
		defer func() { _ = f.Close() }()
		data, err := f.DWARF()
		if err != nil {
			return nil, nil, err
		}
		return data, elfFrameSection(f), nil

	case "darwin":
		f, err := macho.Open(binaryPath)
		if err != nil {
			return nil, nil, fmt.Errorf("macho.Open: %w", err)
		}
		defer func() { _ = f.Close() }()
		data, err := f.DWARF()
		if err != nil {
			return nil, nil, err
		}
		return data, machoFrameSection(f), nil

	default:
		return nil, nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

// elfFrameSection returns the decompressed .debug_frame bytes, or nil. ELF
// Section.Data transparently inflates SHF_COMPRESSED sections; the legacy
// .zdebug_frame form carries its own "ZLIB" header handled by inflateZlib.
func elfFrameSection(f *elf.File) []byte {
	if s := f.Section(".debug_frame"); s != nil {
		if b, err := s.Data(); err == nil {
			return inflateZlib(b)
		}
	}
	if s := f.Section(".zdebug_frame"); s != nil {
		if b, err := s.Data(); err == nil {
			return inflateZlib(b)
		}
	}
	return nil
}

// machoFrameSection returns the decompressed __debug_frame bytes, or nil. Go
// darwin binaries store DWARF compressed as __zdebug_frame with a "ZLIB"
// header; debug/macho does not inflate it, so inflateZlib does.
func machoFrameSection(f *macho.File) []byte {
	if s := f.Section("__debug_frame"); s != nil {
		if b, err := s.Data(); err == nil {
			return inflateZlib(b)
		}
	}
	if s := f.Section("__zdebug_frame"); s != nil {
		if b, err := s.Data(); err == nil {
			return inflateZlib(b)
		}
	}
	return nil
}

// inflateZlib inflates a legacy "ZLIB"-prefixed DWARF section (4-byte magic +
// 8-byte big-endian uncompressed size + zlib stream). Non-prefixed input is
// returned unchanged, so already-inflated bytes pass through.
func inflateZlib(b []byte) []byte {
	if len(b) < 12 || string(b[:4]) != "ZLIB" {
		return b
	}
	zr, err := zlib.NewReader(bytes.NewReader(b[12:]))
	if err != nil {
		return nil
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil
	}
	return out
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

		// Keep the entry that brackets dwarfPC: prev.Address <= dwarfPC <
		// next.Address, with prev a real (non-end-sequence) row. Merely being
		// the greatest address <= dwarfPC is not enough — a CU whose whole line
		// program lies below dwarfPC would otherwise match on its trailing
		// end-sequence row and return a bogus file:line. Go emits CUs with
		// DW_AT_ranges (no contiguous low/high pc), so cuContainsPC can't
		// pre-filter them; the bracket check is what makes the lookup correct.
		var prev dwarf.LineEntry
		var havePrev bool
		var le dwarf.LineEntry
		for {
			if err := lr.Next(&le); err != nil {
				break
			}
			if havePrev && !prev.EndSequence && prev.File != nil &&
				prev.Address <= dwarfPC && dwarfPC < le.Address {
				loc.File = prev.File.Name
				loc.Line = prev.Line
				return loc
			}
			prev = le
			havePrev = true
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

// buildFuncIndex scans the DWARF once, recording every subprogram's PC range
// and name, sorted by low PC for binary search. Ranges are stored unslid
// (as they appear in DWARF); functionAt unslides the query PC to match.
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

// functionAt returns the function name containing pc (runtime address), or "".
func (r *dwarfReader) functionAt(pc uint64) string {
	r.funcIndexOnce.Do(r.buildFuncIndex)
	dwarfPC := uint64(int64(pc) - r.slide)
	// Rightmost subprogram whose low PC is <= dwarfPC.
	i := sort.Search(len(r.funcIndex), func(i int) bool {
		return r.funcIndex[i].low > dwarfPC
	})
	if i == 0 {
		return ""
	}
	fn := r.funcIndex[i-1]
	if dwarfPC >= fn.low && dwarfPC < fn.high {
		return fn.name
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

// LocalsForFrame returns type-aware, bounded variable trees for every local and
// parameter in the subprogram containing pc. Each variable's DWARF type is
// classified and its value formatted with the correct byte width, with nested
// aggregates (structs, slices, arrays, one pointer deref) rendered inline as
// Children; see values.go. Only DW_OP_addr (0x03) and DW_OP_fbreg (0x91)
// locations are evaluated — register-allocated variables come back as
// "<optimized out>".
func (r *dwarfReader) LocalsForFrame(b Backend, pc, frameBase uint64) ([]protocol.Variable, error) {
	entries, err := r.subprogramVars(pc)
	if err != nil {
		return nil, err
	}
	var vars []protocol.Variable
	for _, child := range entries {
		name, _ := child.Val(dwarf.AttrName).(string)
		vars = append(vars, r.formatEntry(b, child, name, frameBase))
	}
	return vars, nil
}

// EvaluateName resolves a single variable name — no dotted paths, indexing, or
// arithmetic (those belong to the later expression-evaluator PR). It first looks
// for a local or parameter in the subprogram containing pc, then falls back to a
// package-level global. The result is the same bounded typed tree LocalsForFrame
// produces.
func (r *dwarfReader) EvaluateName(b Backend, pc, frameBase uint64, name string) (protocol.Variable, error) {
	entries, err := r.subprogramVars(pc)
	if err != nil {
		return protocol.Variable{}, err
	}
	for _, child := range entries {
		if n, _ := child.Val(dwarf.AttrName).(string); n == name {
			return r.formatEntry(b, child, name, frameBase), nil
		}
	}
	if addr, typ, ok := r.globalVar(name); ok {
		return r.formatTyped(b, name, typ, addr), nil
	}
	return protocol.Variable{}, fmt.Errorf("no variable named %q in scope", name)
}

// formatEntry renders one variable/parameter DIE. When its location can't be
// evaluated (register-allocated, or an unsupported expr) it degrades to
// "<optimized out>" while still reporting the type name.
func (r *dwarfReader) formatEntry(b Backend, entry *dwarf.Entry, name string, frameBase uint64) protocol.Variable {
	typ := r.varType(entry)
	addr, ok := r.varAddress(entry, frameBase)
	if !ok {
		return protocol.Variable{Name: name, Type: typeDisplayName(typ), Value: optimizedOut}
	}
	return r.formatTyped(b, name, typ, addr)
}

// subprogramVars collects the variable and formal-parameter DIEs of the
// subprogram whose PC range contains pc. The returned entries are stable copies
// safe to retain past the reader's lifetime.
func (r *dwarfReader) subprogramVars(pc uint64) ([]*dwarf.Entry, error) {
	dwarfPC := uint64(int64(pc) - r.slide)
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil {
			return nil, fmt.Errorf("DWARF subprogramVars: %w", err)
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

		var out []*dwarf.Entry
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
			out = append(out, child)
		}
		return out, nil
	}
	return nil, nil
}

// varType resolves a DIE's DW_AT_type to a concrete dwarf.Type (nil on absence
// or error — the formatter degrades to a hex fallback).
func (r *dwarfReader) varType(entry *dwarf.Entry) dwarf.Type {
	off, ok := entry.Val(dwarf.AttrType).(dwarf.Offset)
	if !ok {
		return nil
	}
	typ, err := r.data.Type(off)
	if err != nil {
		return nil
	}
	return typ
}

// varAddress evaluates a DIE's location expression to a runtime address. ok is
// false for register-allocated variables and unsupported expressions.
func (r *dwarfReader) varAddress(entry *dwarf.Entry, frameBase uint64) (uint64, bool) {
	loc := entry.Val(dwarf.AttrLocation)
	if loc == nil {
		return 0, false
	}
	expr, ok := loc.([]byte)
	if !ok || len(expr) == 0 {
		return 0, false
	}
	switch expr[0] {
	case 0x03: // DW_OP_addr — 8-byte LE DWARF-relative address
		if len(expr) < 9 {
			return 0, false
		}
		return uint64(int64(binary.LittleEndian.Uint64(expr[1:9])) + r.slide), true
	case 0x91: // DW_OP_fbreg — signed LEB128 offset from frame base
		if len(expr) < 2 {
			return 0, false
		}
		offset, _ := decodeSLEB128(expr[1:])
		return uint64(int64(frameBase) + offset), true
	default:
		return 0, false
	}
}

// globalVar resolves a package-level variable to its address and type. It
// matches either the exact DWARF name or a package-qualified "pkg.name" so a
// caller can pass a bare "name". Mirrors resolveVarAddr but also returns type.
func (r *dwarfReader) globalVar(name string) (uint64, dwarf.Type, bool) {
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagVariable {
			if entry.Tag == dwarf.TagSubprogram {
				rd.SkipChildren()
			}
			continue
		}
		n, _ := entry.Val(dwarf.AttrName).(string)
		if n != name && !strings.HasSuffix(n, "."+name) {
			continue
		}
		loc, ok := entry.Val(dwarf.AttrLocation).([]byte)
		if !ok || len(loc) < 9 || loc[0] != 0x03 { // DW_OP_addr
			continue
		}
		addr := uint64(int64(binary.LittleEndian.Uint64(loc[1:9])) + r.slide)
		return addr, r.varType(entry), true
	}
	return 0, nil, false
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

// --- Runtime introspection helpers -----------------------------------------
//
// These resolve Go-runtime symbols and struct layouts straight from DWARF so
// the goroutine/thread reader never hardcodes offsets, which shift between Go
// versions. See AGENTS.md → goroutine snapshot reading.

// runtimeVarAddr returns the runtime (slid) address of a package-level variable
// declared with a DW_OP_addr location (e.g. runtime.allgs, runtime.allm). The
// second result is false when the variable is absent or not a static address.
func (r *dwarfReader) runtimeVarAddr(name string) (uint64, bool) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.varAddrs == nil {
		r.varAddrs = make(map[string]uint64)
	}
	if addr, ok := r.varAddrs[name]; ok {
		return addr, addr != 0
	}

	addr := r.resolveVarAddr(name)
	r.varAddrs[name] = addr
	return addr, addr != 0
}

func (r *dwarfReader) resolveVarAddr(name string) uint64 {
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagVariable {
			// Variables live at CU top level in Go DWARF; don't descend into
			// subprograms (their locals share the tag and would shadow globals).
			if entry.Tag == dwarf.TagSubprogram {
				rd.SkipChildren()
			}
			continue
		}
		n, _ := entry.Val(dwarf.AttrName).(string)
		if n != name {
			continue
		}
		loc, ok := entry.Val(dwarf.AttrLocation).([]byte)
		if !ok || len(loc) < 9 || loc[0] != 0x03 { // DW_OP_addr
			return 0
		}
		return uint64(int64(binary.LittleEndian.Uint64(loc[1:9])) + r.slide)
	}
	return 0
}

// structMemberOffset returns the byte offset of member field within the struct
// type named structName (both as they appear in DWARF, e.g. "runtime.g" /
// "goid"). ok is false when the struct or the member is absent.
func (r *dwarfReader) structMemberOffset(structName, field string) (int64, bool) {
	layout := r.structLayout(structName)
	if !layout.found {
		return 0, false
	}
	off, ok := layout.offsets[field]
	return off, ok
}

func (r *dwarfReader) structLayout(structName string) structLayout {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.structOffs == nil {
		r.structOffs = make(map[string]structLayout)
	}
	if l, ok := r.structOffs[structName]; ok {
		return l
	}
	l := r.resolveStructLayout(structName)
	r.structOffs[structName] = l
	return l
}

func (r *dwarfReader) resolveStructLayout(structName string) structLayout {
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagStructType {
			continue
		}
		n, _ := entry.Val(dwarf.AttrName).(string)
		if n != structName {
			rd.SkipChildren()
			continue
		}
		offsets := make(map[string]int64)
		for {
			child, err := rd.Next()
			if err != nil || child == nil || child.Tag == 0 {
				break
			}
			if child.Tag != dwarf.TagMember {
				continue
			}
			mn, _ := child.Val(dwarf.AttrName).(string)
			if mn == "" {
				continue
			}
			if off, ok := memberOffset(child); ok {
				offsets[mn] = off
			}
		}
		return structLayout{found: true, offsets: offsets}
	}
	return structLayout{}
}

// memberOffset extracts DW_AT_data_member_location, which Go emits as a plain
// integer constant but the DWARF spec also permits as a location expression
// (DW_OP_plus_uconst). Both are handled.
func memberOffset(entry *dwarf.Entry) (int64, bool) {
	v := entry.Val(dwarf.AttrDataMemberLoc)
	switch val := v.(type) {
	case int64:
		return val, true
	case []byte:
		if len(val) >= 2 && val[0] == 0x23 { // DW_OP_plus_uconst
			u, _ := decodeULEB128(val[1:])
			return int64(u), true
		}
	}
	return 0, false
}

// runtimeArrayInfo returns the base address, element count, and element stride
// (bytes) of a package-level array variable declared with a static address
// (e.g. runtime.waitReasonStrings). ok is false when it can't be resolved.
func (r *dwarfReader) runtimeArrayInfo(name string) (base uint64, count int, stride int, ok bool) {
	if base, ok = r.runtimeVarAddr(name); !ok {
		return 0, 0, 0, false
	}
	toff, ok := r.varTypeOffset(name)
	if !ok {
		return 0, 0, 0, false
	}
	if count, stride, ok = r.arrayTypeInfo(toff); !ok {
		return 0, 0, 0, false
	}
	return base, count, stride, true
}

// varTypeOffset finds the DWARF type offset of a package-level variable by name.
func (r *dwarfReader) varTypeOffset(name string) (dwarf.Offset, bool) {
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			return 0, false
		}
		if entry.Tag != dwarf.TagVariable {
			if entry.Tag == dwarf.TagSubprogram {
				rd.SkipChildren()
			}
			continue
		}
		if n, _ := entry.Val(dwarf.AttrName).(string); n != name {
			continue
		}
		off, ok := entry.Val(dwarf.AttrType).(dwarf.Offset)
		return off, ok
	}
}

// arrayTypeInfo resolves an array type's element count and per-element stride
// (bytes) from the DWARF type entry at toff.
func (r *dwarfReader) arrayTypeInfo(toff dwarf.Offset) (count int, stride int, ok bool) {
	tr := r.data.Reader()
	tr.Seek(toff)
	te, err := tr.Next()
	if err != nil || te == nil || te.Tag != dwarf.TagArrayType {
		return 0, 0, false
	}
	total, _ := te.Val(dwarf.AttrByteSize).(int64)
	for {
		ce, err := tr.Next()
		if err != nil || ce == nil || ce.Tag == 0 {
			break
		}
		if ce.Tag != dwarf.TagSubrangeType {
			continue
		}
		if c, ok := ce.Val(dwarf.AttrCount).(int64); ok {
			count = int(c)
		} else if ub, ok := ce.Val(dwarf.AttrUpperBound).(int64); ok {
			count = int(ub) + 1
		}
		break
	}
	if count <= 0 {
		return 0, 0, false
	}
	if total > 0 {
		stride = int(total) / count
	}
	return count, stride, true
}

// decodeULEB128 decodes an unsigned LEB128 integer. Returns (value, bytesRead).
func decodeULEB128(b []byte) (uint64, int) {
	var result uint64
	var shift uint
	for i, byt := range b {
		result |= uint64(byt&0x7f) << shift
		if byt&0x80 == 0 {
			return result, i + 1
		}
		shift += 7
	}
	return result, len(b)
}
