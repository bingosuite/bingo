package debugger

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// Type-aware value formatting: classify a variable's DWARF type and render it
// into a bounded, eager protocol.Variable tree (children inline). "Eager" is a
// deliberate choice — it lets both WebSocket clients and DAP expand nested
// values without a string path/expression parser (that parser is a later PR).
//
// Everything here is best-effort: an unreadable address or an unrecognised type
// degrades a single node to a hex or <optimized out> rendering rather than
// erroring the stop. See AGENTS.md → DWARF reader notes.

// Bounds on the eager tree. Together with the visited-address cycle guard these
// keep one stop's inspection cheap and finite regardless of how deep or wide the
// target's data is, and make self-referential structures (linked lists, back
// pointers) safe to format.
const (
	maxValueDepth  = 4    // aggregate nesting levels expanded
	maxChildren    = 100  // fields/elements per aggregate before a "… N more" node
	maxPtrDeref    = 1    // pointer-chase depth
	maxStringBytes = 256  // bytes read from a string/[]byte before truncation
	maxScalarBytes = 4096 // defensive cap on any single ReadMemory
)

// Variable.Kind classifier strings.
const (
	kindInt       = "int"
	kindUint      = "uint"
	kindFloat     = "float"
	kindBool      = "bool"
	kindComplex   = "complex"
	kindString    = "string"
	kindPtr       = "ptr"
	kindStruct    = "struct"
	kindSlice     = "slice"
	kindArray     = "array"
	kindMap       = "map"
	kindChan      = "chan"
	kindFunc      = "func"
	kindInterface = "interface"
)

// formatCtx carries the per-inspection state shared across the whole recursive
// walk. b and visited are shared; depth/ptrDepth are passed by value so a
// sibling's recursion budget is independent.
type formatCtx struct {
	b       Backend
	visited map[uint64]bool // addresses whose formatting has begun (cycle guard)
}

// formatTyped renders the value of type typ stored at addr into a bounded
// protocol.Variable tree rooted at name.
func (r *dwarfReader) formatTyped(b Backend, name string, typ dwarf.Type, addr uint64) protocol.Variable {
	ctx := &formatCtx{b: b, visited: map[uint64]bool{}}
	return r.formatNode(name, typ, addr, 0, 0, ctx)
}

func (r *dwarfReader) formatNode(name string, typ dwarf.Type, addr uint64, depth, ptrDepth int, ctx *formatCtx) protocol.Variable {
	out := protocol.Variable{Name: name, Address: addr}
	if typ == nil {
		out.Value = r.hexFallback(ctx.b, addr, 8)
		return out
	}

	concrete, display, special := unwrapType(typ)
	out.Type = display

	switch special {
	case kindMap, kindChan:
		out.Kind = special
		out.Value = r.summaryPointer(ctx.b, addr, display)
		return out
	case kindInterface:
		out.Kind = kindInterface
		out.Value = r.summaryInterface(ctx.b, addr, display)
		return out
	}

	switch t := concrete.(type) {
	case *dwarf.IntType:
		out.Kind = kindInt
		r.setScalar(&out, ctx.b, addr, int(t.Size()), formatInt)
	case *dwarf.CharType:
		out.Kind = kindInt
		r.setScalar(&out, ctx.b, addr, int(t.Size()), formatInt)
	case *dwarf.UintType:
		out.Kind = kindUint
		r.setScalar(&out, ctx.b, addr, int(t.Size()), formatUint)
	case *dwarf.UcharType:
		out.Kind = kindUint
		r.setScalar(&out, ctx.b, addr, int(t.Size()), formatUint)
	case *dwarf.BoolType:
		out.Kind = kindBool
		r.setScalar(&out, ctx.b, addr, int(t.Size()), formatBool)
	case *dwarf.FloatType:
		out.Kind = kindFloat
		r.setScalar(&out, ctx.b, addr, int(t.Size()), formatFloat)
	case *dwarf.ComplexType:
		out.Kind = kindComplex
		r.setScalar(&out, ctx.b, addr, int(t.Size()), formatComplex)
	case *dwarf.PtrType:
		r.formatPointer(&out, t, addr, depth, ptrDepth, ctx)
	case *dwarf.FuncType:
		out.Kind = kindFunc
		out.Value = r.summaryPointer(ctx.b, addr, display)
	case *dwarf.StructType:
		r.formatStruct(&out, t, addr, depth, ptrDepth, ctx)
	case *dwarf.ArrayType:
		r.formatArray(&out, t, addr, depth, ptrDepth, ctx)
	default:
		out.Value = r.hexFallback(ctx.b, addr, sizeOf(concrete))
	}
	return out
}

// unwrapType strips TypedefType/QualType layers to the concrete type. display is
// the outermost named layer (what a user sees). special is non-empty when the
// concrete representation would misclassify Go's higher-level kinds — map/chan
// are pointers, interfaces are two-word structs / self-referential typedefs — so
// the caller summarises instead of descending into runtime internals. A
// visited-typedef guard breaks the `interface {}` (and named-interface)
// self-reference.
func unwrapType(typ dwarf.Type) (concrete dwarf.Type, display, special string) {
	display = typeDisplayName(typ)
	seen := map[dwarf.Type]bool{}
	for {
		switch t := typ.(type) {
		case *dwarf.TypedefType:
			switch {
			case strings.HasPrefix(t.Name, "map["):
				return typ, display, kindMap
			case strings.HasPrefix(t.Name, "chan ") ||
				strings.HasPrefix(t.Name, "<-chan ") ||
				strings.HasPrefix(t.Name, "chan<-"):
				return typ, display, kindChan
			case strings.HasPrefix(t.Name, "interface {") ||
				strings.HasPrefix(t.Name, "interface{"):
				return typ, display, kindInterface
			}
			if t.Type == nil || seen[t.Type] {
				// A typedef that resolves to itself (Go emits `error` and
				// `interface {}` this way) — treat as an interface value.
				return typ, display, kindInterface
			}
			seen[typ] = true
			typ = t.Type
		case *dwarf.QualType:
			if t.Type == nil {
				return typ, display, ""
			}
			typ = t.Type
		default:
			return typ, display, ""
		}
	}
}

func (r *dwarfReader) formatPointer(out *protocol.Variable, t *dwarf.PtrType, addr uint64, depth, ptrDepth int, ctx *formatCtx) {
	out.Kind = kindPtr
	buf, err := readMem(ctx.b, addr, 8)
	if err != nil {
		out.Value = unreadable(err)
		return
	}
	p := binary.LittleEndian.Uint64(buf)
	if p == 0 {
		out.Value = "nil"
		return
	}
	out.Value = fmt.Sprintf("0x%x", p)
	// One bounded dereference as a child, guarded against cycles and budget.
	if ptrDepth >= maxPtrDeref || depth >= maxValueDepth || ctx.visited[p] {
		return
	}
	if _, ok := t.Type.(*dwarf.VoidType); ok {
		return // unsafe.Pointer — nothing typed to deref
	}
	ctx.visited[p] = true
	child := r.formatNode("*"+out.Name, t.Type, p, depth+1, ptrDepth+1, ctx)
	out.Children = []protocol.Variable{child}
}

func (r *dwarfReader) formatStruct(out *protocol.Variable, t *dwarf.StructType, addr uint64, depth, ptrDepth int, ctx *formatCtx) {
	// Go strings and slices are DWARF structs; recognise them by name/shape.
	if t.StructName == "string" {
		out.Kind = kindString
		out.Value = r.readGoString(ctx.b, addr)
		return
	}
	if strings.HasPrefix(t.StructName, "[]") {
		r.formatSlice(out, t, addr, depth, ptrDepth, ctx)
		return
	}

	out.Kind = kindStruct
	if t.StructName != "" {
		out.Value = t.StructName
	} else {
		out.Value = "{...}"
	}
	if depth >= maxValueDepth {
		return
	}
	for i, f := range t.Field {
		if f == nil {
			continue
		}
		if i >= maxChildren {
			out.Children = append(out.Children, moreNode(len(t.Field)-maxChildren))
			break
		}
		child := r.formatNode(f.Name, f.Type, addr+uint64(f.ByteOffset), depth+1, ptrDepth, ctx)
		out.Children = append(out.Children, child)
	}
}

func (r *dwarfReader) formatSlice(out *protocol.Variable, t *dwarf.StructType, addr uint64, depth, ptrDepth int, ctx *formatCtx) {
	out.Kind = kindSlice
	hdr, err := readMem(ctx.b, addr, 24) // {array *elem, len int, cap int}
	if err != nil {
		out.Value = unreadable(err)
		return
	}
	ptr := binary.LittleEndian.Uint64(hdr[0:8])
	length := int64(binary.LittleEndian.Uint64(hdr[8:16]))
	capacity := int64(binary.LittleEndian.Uint64(hdr[16:24]))
	out.Value = fmt.Sprintf("%s len:%d cap:%d", t.StructName, length, capacity)

	elem := sliceElemType(t)
	if elem == nil || ptr == 0 || length <= 0 || depth >= maxValueDepth {
		return
	}
	r.formatElements(out, elem, ptr, length, depth, ptrDepth, ctx)
}

func (r *dwarfReader) formatArray(out *protocol.Variable, t *dwarf.ArrayType, addr uint64, depth, ptrDepth int, ctx *formatCtx) {
	out.Kind = kindArray
	out.Value = fmt.Sprintf("[%d]%s", t.Count, typeDisplayName(t.Type))
	if t.Type == nil || t.Count <= 0 || depth >= maxValueDepth {
		return
	}
	r.formatElements(out, t.Type, addr, t.Count, depth, ptrDepth, ctx)
}

// formatElements renders up to maxChildren elements of elemType starting at base,
// appending a "… N more" node when the collection is larger.
func (r *dwarfReader) formatElements(out *protocol.Variable, elemType dwarf.Type, base uint64, count int64, depth, ptrDepth int, ctx *formatCtx) {
	stride := sizeOf(elemType)
	if stride <= 0 {
		return
	}
	shown := count
	if shown > maxChildren {
		shown = maxChildren
	}
	for i := int64(0); i < shown; i++ {
		elemAddr := base + uint64(i)*uint64(stride)
		child := r.formatNode(fmt.Sprintf("[%d]", i), elemType, elemAddr, depth+1, ptrDepth, ctx)
		out.Children = append(out.Children, child)
	}
	if count > shown {
		out.Children = append(out.Children, moreNode(int(count-shown)))
	}
}

// readGoString reads a Go string header {ptr, len} at addr and returns a quoted,
// length-bounded rendering.
func (r *dwarfReader) readGoString(b Backend, addr uint64) string {
	hdr, err := readMem(b, addr, 16)
	if err != nil {
		return unreadable(err)
	}
	ptr := binary.LittleEndian.Uint64(hdr[0:8])
	length := int64(binary.LittleEndian.Uint64(hdr[8:16]))
	if length <= 0 || ptr == 0 {
		return `""`
	}
	truncated := false
	if length > maxStringBytes {
		length = maxStringBytes
		truncated = true
	}
	data, err := readMem(b, ptr, int(length))
	if err != nil {
		return unreadable(err)
	}
	s := strconv.Quote(string(data))
	if truncated {
		s = s + "..."
	}
	return s
}

// summaryPointer renders a value whose storage at addr is a single pointer word
// (map/chan/func) as "<display> (0x...)" — or "nil".
func (r *dwarfReader) summaryPointer(b Backend, addr uint64, display string) string {
	buf, err := readMem(b, addr, 8)
	if err != nil {
		return unreadable(err)
	}
	p := binary.LittleEndian.Uint64(buf)
	if p == 0 {
		return display + " (nil)"
	}
	return fmt.Sprintf("%s (0x%x)", display, p)
}

// summaryInterface renders a two-word interface value {type, data} as a nil/
// non-nil summary. A full dynamic-type resolution is deferred (later PR).
func (r *dwarfReader) summaryInterface(b Backend, addr uint64, display string) string {
	buf, err := readMem(b, addr, 16)
	if err != nil {
		return unreadable(err)
	}
	typ := binary.LittleEndian.Uint64(buf[0:8])
	data := binary.LittleEndian.Uint64(buf[8:16])
	if typ == 0 && data == 0 {
		return display + " (nil)"
	}
	return fmt.Sprintf("%s (0x%x)", display, data)
}

func (r *dwarfReader) hexFallback(b Backend, addr uint64, n int) string {
	if n <= 0 {
		n = 8
	}
	if n > 8 {
		n = 8
	}
	buf, err := readMem(b, addr, n)
	if err != nil {
		return unreadable(err)
	}
	var u uint64
	for i := len(buf) - 1; i >= 0; i-- {
		u = u<<8 | uint64(buf[i])
	}
	return fmt.Sprintf("0x%x", u)
}

// setScalar reads size bytes at addr and formats them with fn, degrading to a
// hex/unreadable rendering on a short or failed read.
func (r *dwarfReader) setScalar(out *protocol.Variable, b Backend, addr uint64, size int, fn func([]byte) string) {
	if size <= 0 {
		out.Value = r.hexFallback(b, addr, 8)
		return
	}
	buf, err := readMem(b, addr, size)
	if err != nil {
		out.Value = unreadable(err)
		return
	}
	out.Value = fn(buf)
}

// --- pure leaf formatters (unit-testable without DWARF/backend) --------------

func formatInt(buf []byte) string {
	if len(buf) == 0 {
		return "0"
	}
	var u uint64
	for i := len(buf) - 1; i >= 0; i-- {
		u = u<<8 | uint64(buf[i])
	}
	bits := len(buf) * 8
	if bits < 64 && u&(1<<(uint(bits)-1)) != 0 {
		u |= ^uint64(0) << uint(bits) // sign-extend
	}
	return strconv.FormatInt(int64(u), 10)
}

func formatUint(buf []byte) string {
	var u uint64
	for i := len(buf) - 1; i >= 0; i-- {
		u = u<<8 | uint64(buf[i])
	}
	return strconv.FormatUint(u, 10)
}

func formatBool(buf []byte) string {
	for _, x := range buf {
		if x != 0 {
			return "true"
		}
	}
	return "false"
}

func formatFloat(buf []byte) string {
	switch len(buf) {
	case 4:
		f := math.Float32frombits(binary.LittleEndian.Uint32(buf))
		return strconv.FormatFloat(float64(f), 'g', -1, 32)
	case 8:
		f := math.Float64frombits(binary.LittleEndian.Uint64(buf))
		return strconv.FormatFloat(f, 'g', -1, 64)
	default:
		return "<bad-float>"
	}
}

func formatComplex(buf []byte) string {
	switch len(buf) {
	case 8:
		re := math.Float32frombits(binary.LittleEndian.Uint32(buf[0:4]))
		im := math.Float32frombits(binary.LittleEndian.Uint32(buf[4:8]))
		return formatComplexParts(float64(re), float64(im), 32)
	case 16:
		re := math.Float64frombits(binary.LittleEndian.Uint64(buf[0:8]))
		im := math.Float64frombits(binary.LittleEndian.Uint64(buf[8:16]))
		return formatComplexParts(re, im, 64)
	default:
		return "<bad-complex>"
	}
}

func formatComplexParts(re, im float64, bits int) string {
	sign := "+"
	if math.Signbit(im) {
		sign = "-"
		im = -im
	}
	return "(" + strconv.FormatFloat(re, 'g', -1, bits) + sign +
		strconv.FormatFloat(im, 'g', -1, bits) + "i)"
}

// --- small helpers -----------------------------------------------------------

func moreNode(n int) protocol.Variable {
	return protocol.Variable{Name: "…", Value: fmt.Sprintf("%d more", n)}
}

func unreadable(err error) string { return fmt.Sprintf("<unreadable: %v>", err) }

// readMem reads exactly n bytes at addr, capping n defensively so a bogus DWARF
// size can't trigger a huge allocation.
func readMem(b Backend, addr uint64, n int) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	if n > maxScalarBytes {
		n = maxScalarBytes
	}
	buf := make([]byte, n)
	if err := b.ReadMemory(addr, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// sizeOf returns a type's byte size, or 8 as a safe default when unknown.
func sizeOf(typ dwarf.Type) int {
	if typ == nil {
		return 8
	}
	if s := typ.Size(); s > 0 {
		return int(s)
	}
	return 8
}

// sliceElemType returns the element type of a Go slice DWARF struct, read from
// its "array" field's pointee.
func sliceElemType(t *dwarf.StructType) dwarf.Type {
	for _, f := range t.Field {
		if f != nil && f.Name == "array" {
			if pt, ok := f.Type.(*dwarf.PtrType); ok {
				return pt.Type
			}
		}
	}
	return nil
}

// typeDisplayName is the human-facing type string: the struct/typedef name when
// present, else the dwarf.Type's own rendering.
func typeDisplayName(typ dwarf.Type) string {
	switch t := typ.(type) {
	case nil:
		return "unknown"
	case *dwarf.StructType:
		if t.StructName != "" {
			return t.StructName
		}
		return "struct"
	case *dwarf.TypedefType:
		return t.Name
	}
	return typ.String()
}
