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

// Bounds on the eager tree. The per-node depth (maxValueDepth) and per-aggregate
// width (maxChildren) caps plus the active-pointer cycle guard stop any single
// path from running away, but a collection-of-collections is still bounded only
// by their product (e.g. [][][][]int ≈ maxChildren⁴ nodes, each doing its own
// ReadMemory). maxTotalNodes/maxTotalBytes add a SHARED ceiling across the whole
// REQUEST — threaded through one formatCtx and debited once per node and by each
// read — so one Locals/Evaluate/variables request can never wedge the
// single-threaded engine loop no matter how deep or wide the target's data is,
// nor how many root variables the frame has. Exhausting either budget degrades
// the subtree to a single truncation node; it never errors the stop.
const (
	maxValueDepth  = 4    // aggregate nesting levels expanded
	maxChildren    = 100  // fields/elements per aggregate before a "… N more" node
	maxPtrDeref    = 1    // pointer-chase depth
	maxStringBytes = 256  // bytes read from a string/[]byte before truncation
	maxScalarBytes = 4096 // defensive cap on any single ReadMemory

	maxTotalNodes = 10000      // nodes across ONE request before truncation
	maxTotalBytes = 256 * 1024 // bytes read across ONE request before truncation
)

// truncatedValue marks a node the eager walk refused to expand because the
// shared per-request node/byte budget (formatCtx) was exhausted.
const truncatedValue = "<truncated: inspection budget exhausted>"

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

// formatCtx carries the per-REQUEST state shared across every root variable's
// recursive walk. One context serves a whole Locals request; Evaluate has a
// single root and gets its own.
//
// activePointers contains only the current recursion path; entries are removed
// while unwinding (the enterPointer/leave pairing) so sibling aliases — within a
// root AND across roots — expand independently. Because every claim is balanced
// by its defer, the map is empty again between roots: sharing the context must
// never make a later root's alias look cyclic.
//
// depth and ptrDepth are passed by value so a sibling's recursion budget is
// independent. nodesLeft and bytesLeft are the shared GLOBAL ceiling (see the
// bounds block): decremented once per formatNode and by each read, and NOT reset
// per root, so neither a collection-of-collections nor a frame full of large
// locals can fan out unboundedly.
type formatCtx struct {
	b              Backend
	activePointers map[uint64]bool
	nodesLeft      int // remaining nodes for this request (shared across roots)
	bytesLeft      int // remaining bytes to read for this request (shared across roots)
}

// newFormatCtx starts a fresh inspection budget. Callers that render several
// root variables for one request (LocalsForFrame) must create exactly one and
// reuse it; a per-root context would multiply the ceiling by the root count.
func newFormatCtx(b Backend) *formatCtx {
	return &formatCtx{
		b:              b,
		activePointers: map[uint64]bool{},
		nodesLeft:      maxTotalNodes,
		bytesLeft:      maxTotalBytes,
	}
}

// read fetches n bytes at addr like readMem, additionally debiting the shared
// byte budget so the whole eager walk can't over-read across many small nodes.
func (ctx *formatCtx) read(addr uint64, n int) ([]byte, error) {
	buf, err := readMem(ctx.b, addr, n)
	ctx.bytesLeft -= len(buf)
	return buf, err
}

// budgetExhausted reports whether the shared node or byte ceiling for one
// request has been reached. Once true the walk emits a single truncation node
// instead of recursing or reading further — degrading the subtree, never
// erroring the stop.
func (ctx *formatCtx) budgetExhausted() bool {
	return ctx.nodesLeft <= 0 || ctx.bytesLeft <= 0
}

// enterPointer claims addr for the current recursion path. A rejected claim
// returns no cleanup function, so a cycle cannot remove its ancestor's entry.
func (ctx *formatCtx) enterPointer(addr uint64) (leave func(), ok bool) {
	if ctx.activePointers[addr] {
		return nil, false
	}
	ctx.activePointers[addr] = true
	return func() { delete(ctx.activePointers, addr) }, true
}

// formatTyped renders the value of type typ stored at addr into a bounded
// protocol.Variable tree rooted at name, using a budget of its own. It is the
// single-root entry point (Evaluate, tests); multi-root callers must share one
// context via formatRoot instead.
func (r *dwarfReader) formatTyped(b Backend, name string, typ dwarf.Type, addr uint64) protocol.Variable {
	return r.formatRoot(newFormatCtx(b), name, typ, addr)
}

// formatRoot renders one root variable against an existing request budget.
func (r *dwarfReader) formatRoot(ctx *formatCtx, name string, typ dwarf.Type, addr uint64) protocol.Variable {
	return r.formatNode(name, typ, addr, 0, 0, ctx)
}

// formatRequestRoots renders the roots of ONE inspection request against the
// single shared budget in ctx, calling render for root i.
//
// The budget is checked between roots, not just inside a tree, so a frame full
// of large aggregates stops at the global ceiling instead of spending a fresh
// one per root. Elided roots collapse into exactly ONE trailing truncation node
// — a marker per remaining root would itself be unbounded on a frame with
// thousands of locals.
func formatRequestRoots(ctx *formatCtx, roots int, render func(i int) protocol.Variable) []protocol.Variable {
	var out []protocol.Variable
	for i := 0; i < roots; i++ {
		if ctx.budgetExhausted() {
			out = append(out, truncatedNode())
			break
		}
		out = append(out, render(i))
	}
	return out
}

func (r *dwarfReader) formatNode(name string, typ dwarf.Type, addr uint64, depth, ptrDepth int, ctx *formatCtx) protocol.Variable {
	// Global budget guard: every node in the tree passes through here, so this
	// single check bounds the total node count regardless of nesting shape.
	if ctx.budgetExhausted() {
		return protocol.Variable{Name: name, Address: addr, Value: truncatedValue}
	}
	ctx.nodesLeft--
	out := protocol.Variable{Name: name, Address: addr}
	if typ == nil {
		out.Value = r.hexFallback(ctx, addr, 8)
		return out
	}

	concrete, display, special := unwrapType(typ)
	out.Type = display

	switch special {
	case kindMap, kindChan:
		out.Kind = special
		out.Value = r.summaryPointer(ctx, addr, display)
		return out
	case kindInterface:
		out.Kind = kindInterface
		out.Value = r.summaryInterface(ctx, addr, display)
		return out
	}

	switch t := concrete.(type) {
	case *dwarf.IntType:
		out.Kind = kindInt
		r.setScalar(&out, ctx, addr, int(t.Size()), formatInt)
	case *dwarf.CharType:
		out.Kind = kindInt
		r.setScalar(&out, ctx, addr, int(t.Size()), formatInt)
	case *dwarf.UintType:
		out.Kind = kindUint
		r.setScalar(&out, ctx, addr, int(t.Size()), formatUint)
	case *dwarf.UcharType:
		out.Kind = kindUint
		r.setScalar(&out, ctx, addr, int(t.Size()), formatUint)
	case *dwarf.BoolType:
		out.Kind = kindBool
		r.setScalar(&out, ctx, addr, int(t.Size()), formatBool)
	case *dwarf.FloatType:
		out.Kind = kindFloat
		r.setScalar(&out, ctx, addr, int(t.Size()), formatFloat)
	case *dwarf.ComplexType:
		out.Kind = kindComplex
		r.setScalar(&out, ctx, addr, int(t.Size()), formatComplex)
	case *dwarf.PtrType:
		r.formatPointer(&out, t, addr, depth, ptrDepth, ctx)
	case *dwarf.FuncType:
		out.Kind = kindFunc
		out.Value = r.summaryPointer(ctx, addr, display)
	case *dwarf.StructType:
		r.formatStruct(&out, t, addr, depth, ptrDepth, ctx)
	case *dwarf.ArrayType:
		r.formatArray(&out, t, addr, depth, ptrDepth, ctx)
	default:
		out.Value = r.hexFallback(ctx, addr, sizeOf(concrete))
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
	buf, err := ctx.read(addr, 8)
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
	if ptrDepth >= maxPtrDeref || depth >= maxValueDepth {
		return
	}
	if _, ok := t.Type.(*dwarf.VoidType); ok {
		return // unsafe.Pointer — nothing typed to deref
	}
	leave, ok := ctx.enterPointer(p)
	if !ok {
		return
	}
	defer leave()
	child := r.formatNode("*"+out.Name, t.Type, p, depth+1, ptrDepth+1, ctx)
	out.Children = []protocol.Variable{child}
}

func (r *dwarfReader) formatStruct(out *protocol.Variable, t *dwarf.StructType, addr uint64, depth, ptrDepth int, ctx *formatCtx) {
	// Go strings and slices are DWARF structs; recognise them by name/shape.
	if t.StructName == "string" {
		out.Kind = kindString
		out.Value = r.readGoString(ctx, addr)
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
		if ctx.budgetExhausted() {
			out.Children = append(out.Children, truncatedNode())
			break
		}
		child := r.formatNode(f.Name, f.Type, addr+uint64(f.ByteOffset), depth+1, ptrDepth, ctx)
		out.Children = append(out.Children, child)
	}
}

func (r *dwarfReader) formatSlice(out *protocol.Variable, t *dwarf.StructType, addr uint64, depth, ptrDepth int, ctx *formatCtx) {
	out.Kind = kindSlice
	hdr, err := ctx.read(addr, 24) // {array *elem, len int, cap int}
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
		if ctx.budgetExhausted() {
			out.Children = append(out.Children, truncatedNode())
			return
		}
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
func (r *dwarfReader) readGoString(ctx *formatCtx, addr uint64) string {
	hdr, err := ctx.read(addr, 16)
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
	data, err := ctx.read(ptr, int(length))
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
func (r *dwarfReader) summaryPointer(ctx *formatCtx, addr uint64, display string) string {
	buf, err := ctx.read(addr, 8)
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
func (r *dwarfReader) summaryInterface(ctx *formatCtx, addr uint64, display string) string {
	buf, err := ctx.read(addr, 16)
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

func (r *dwarfReader) hexFallback(ctx *formatCtx, addr uint64, n int) string {
	if n <= 0 {
		n = 8
	}
	if n > 8 {
		n = 8
	}
	buf, err := ctx.read(addr, n)
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
func (r *dwarfReader) setScalar(out *protocol.Variable, ctx *formatCtx, addr uint64, size int, fn func([]byte) string) {
	if size <= 0 {
		out.Value = r.hexFallback(ctx, addr, 8)
		return
	}
	buf, err := ctx.read(addr, size)
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

// truncatedNode is the single leaf a loop appends when the shared per-request
// budget (formatCtx) is exhausted, standing in for every elided remaining child
// — or, at the root level, for every remaining root variable.
func truncatedNode() protocol.Variable {
	return protocol.Variable{Name: "…", Value: truncatedValue}
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
