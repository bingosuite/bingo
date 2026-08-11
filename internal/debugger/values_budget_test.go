package debugger

import (
	"debug/dwarf"
	"encoding/binary"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// Request-scoped inspection budget: one Locals request shares a single
// node/byte ceiling across ALL of its root variables (issue #186). These tests
// drive formatRequestRoots — the exact loop LocalsForFrame uses — because
// building synthetic *dwarf.Data for a whole subprogram is not feasible in a
// unit test; real-DWARF coverage lives in the `inspect` E2E label.

// countingBackend records read volume so a test can assert the walk did not
// fan out into a per-root budget's worth of ReadMemory calls.
type countingBackend struct {
	readableBackend
	reads int
	bytes int
}

func (b *countingBackend) ReadMemory(_ uint64, dst []byte) error {
	b.reads++
	b.bytes += len(dst)
	return nil
}

// deepArrayType is the pathological fixture from TestFormatTypedBudget:
// [100][100][100][100]int, whose per-path width/depth caps alone still leave
// ~10^8 potential nodes, so one root alone can exhaust the global ceiling.
func deepArrayType() dwarf.Type {
	arr := func(elem dwarf.Type, count int64) *dwarf.ArrayType {
		return &dwarf.ArrayType{Type: elem, Count: count}
	}
	return arr(arr(arr(arr(testIntType(), 100), 100), 100), 100)
}

func countAllNodes(vars []protocol.Variable) int {
	total := 0
	for i := range vars {
		total += countNodes(vars[i])
	}
	return total
}

func localEntry(name string, addr *uint64) *dwarf.Entry {
	entry := &dwarf.Entry{
		Tag: dwarf.TagVariable,
		Field: []dwarf.Field{
			{Attr: dwarf.AttrName, Val: name},
		},
	}
	if addr != nil {
		expr := make([]byte, 9)
		expr[0] = 0x03
		binary.LittleEndian.PutUint64(expr[1:], *addr)
		entry.Field = append(entry.Field, dwarf.Field{Attr: dwarf.AttrLocation, Val: expr})
	}
	return entry
}

// TestLocalsProductionPathChargesOptimizedOutRoots drives the exact
// LocalsForFrame entries-to-formatEntry path. Optimized-out variables never
// reach formatNode, but each emitted root must still spend one request node.
func TestLocalsProductionPathChargesOptimizedOutRoots(t *testing.T) {
	const roots = maxTotalNodes + 100
	entries := make([]*dwarf.Entry, roots)
	for i := range entries {
		entries[i] = localEntry("optimized", nil)
	}
	backend := &countingBackend{}

	vars := (&dwarfReader{}).formatLocalEntries(backend, entries, 0)

	if got, want := len(vars), maxTotalNodes+1; got != want {
		t.Fatalf("roots = %d, want %d budgeted roots plus one marker", got, want)
	}
	if last := vars[len(vars)-1]; last.Name != "…" || last.Value != truncatedValue {
		t.Fatalf("last root = %#v, want one truncation marker", last)
	}
	if backend.reads != 0 {
		t.Fatalf("optimized-out roots performed %d backend reads, want 0", backend.reads)
	}
}

func TestFormatEntryChargesBestEffortRoots(t *testing.T) {
	const (
		missingAddr  = uint64(0x2000)
		readableAddr = uint64(0x3000)
	)
	backend := newValueMemoryBackend()
	backend.seedUint64(readableAddr, 7)
	ctx := newFormatCtx(backend)
	r := &dwarfReader{}

	optimized := r.formatEntry(ctx, localEntry("optimized", nil), "optimized", 0)
	unreadable := r.formatEntry(ctx, localEntry("unreadable", ptrTo(missingAddr)), "unreadable", 0)
	unknown := r.formatEntry(ctx, localEntry("unknown", ptrTo(readableAddr)), "unknown", 0)

	if optimized.Value != optimizedOut {
		t.Fatalf("optimized root = %q, want %q", optimized.Value, optimizedOut)
	}
	if got, want := unreadable.Value, "<unreadable: unreadable memory>"; got != want {
		t.Fatalf("unreadable root = %q, want %q", got, want)
	}
	if unknown.Value != "0x7" {
		t.Fatalf("unknown root = %q, want 0x7", unknown.Value)
	}
	if got, want := ctx.nodesLeft, maxTotalNodes-3; got != want {
		t.Fatalf("nodesLeft = %d, want %d after three best-effort roots", got, want)
	}
}

func ptrTo(v uint64) *uint64 { return &v }

// TestFormatEntryReservesBytesBeforeBackendRead mutation-locks the production
// formatEntry path: a read that cannot fit must truncate without any backend I/O.
func TestFormatEntryReservesBytesBeforeBackendRead(t *testing.T) {
	const addr = uint64(0x3000)
	backend := &countingBackend{}
	ctx := newFormatCtx(backend)
	ctx.bytesLeft = 1

	got := (&dwarfReader{}).formatEntry(ctx, localEntry("value", ptrTo(addr)), "value", 0)

	if got.Value != truncatedValue {
		t.Fatalf("value = %q, want %q", got.Value, truncatedValue)
	}
	if backend.reads != 0 || backend.bytes != 0 {
		t.Fatalf("backend reads=%d bytes=%d, want no I/O beyond the remaining byte budget",
			backend.reads, backend.bytes)
	}
	if ctx.bytesLeft != 0 {
		t.Fatalf("bytesLeft = %d, want exhausted after a refused read", ctx.bytesLeft)
	}
}

// TestLocalsRequestSharesBudgetAcrossRoots is the #186 regression gate: before
// the fix each root got a fresh maxTotalNodes/maxTotalBytes, so N huge locals
// cost N× the ceiling (40 roots measured at ~400k nodes / ~396k reads). The
// whole request must now stay at ONE ceiling.
func TestLocalsRequestSharesBudgetAcrossRoots(t *testing.T) {
	const roots = 40
	nested := deepArrayType()
	backend := &countingBackend{}
	r := &dwarfReader{}
	ctx := newFormatCtx(backend)

	vars := formatRequestRoots(ctx, roots, func(i int) protocol.Variable {
		return r.formatRoot(ctx, "root", nested, 0x1000)
	})

	total := countAllNodes(vars)
	if total > maxTotalNodes+1000 {
		t.Fatalf("request node count %d exceeds shared ceiling %d(+slack); budget reset per root",
			total, maxTotalNodes)
	}
	// Every node reads at most a handful of times; a per-root budget would put
	// this in the hundreds of thousands.
	if backend.reads > maxTotalNodes+1000 {
		t.Fatalf("ReadMemory calls %d exceed the shared node ceiling %d(+slack)",
			backend.reads, maxTotalNodes)
	}
	if backend.bytes > maxTotalBytes+maxScalarBytes {
		t.Fatalf("bytes read %d exceed the shared byte ceiling %d(+slack)",
			backend.bytes, maxTotalBytes)
	}
	if len(vars) >= roots {
		t.Fatalf("returned %d roots, want the loop to stop early once the budget was spent", len(vars))
	}
}

// TestLocalsTruncatesRemainingRootsOnce pins the exact exhaustion behavior: the
// elided roots collapse into ONE trailing truncation node, not one marker per
// remaining root (which would itself be unbounded on a frame with thousands of
// locals).
func TestLocalsTruncatesRemainingRootsOnce(t *testing.T) {
	const roots = 5000
	nested := deepArrayType()
	r := &dwarfReader{}
	ctx := newFormatCtx(readableBackend{})

	vars := formatRequestRoots(ctx, roots, func(i int) protocol.Variable {
		return r.formatRoot(ctx, "root", nested, 0x1000)
	})

	if len(vars) == 0 {
		t.Fatal("no roots returned")
	}
	rootMarkers := 0
	for _, v := range vars {
		if v.Value == truncatedValue && len(v.Children) == 0 && v.Name == "…" {
			rootMarkers++
		}
	}
	if rootMarkers != 1 {
		t.Fatalf("root-level truncation markers = %d, want exactly 1 for %d elided roots",
			rootMarkers, roots-len(vars)+1)
	}
	last := vars[len(vars)-1]
	if last.Value != truncatedValue {
		t.Fatalf("last root = %q, want the truncation marker %q", last.Value, truncatedValue)
	}
	if len(vars) > 10 {
		t.Fatalf("returned %d roots for a budget one root already exhausts; expansion must stop promptly",
			len(vars))
	}
}

// TestLocalsLaterRootsSeeRemainingBudget proves the budget is carried forward
// rather than reset: each successive root starts from what its predecessors
// left behind, and the first root that finds nothing left is not formatted.
func TestLocalsLaterRootsSeeRemainingBudget(t *testing.T) {
	intType := testIntType()
	arrType := &dwarf.ArrayType{Type: intType, Count: 100}
	r := &dwarfReader{}
	ctx := newFormatCtx(readableBackend{})

	prev := ctx.nodesLeft
	for i := 0; i < 5; i++ {
		r.formatRoot(ctx, "root", arrType, 0x1000)
		if ctx.nodesLeft >= prev {
			t.Fatalf("root %d left nodesLeft=%d, want strictly less than the previous %d",
				i, ctx.nodesLeft, prev)
		}
		if ctx.nodesLeft == maxTotalNodes {
			t.Fatalf("root %d reset the budget to the full ceiling", i)
		}
		prev = ctx.nodesLeft
	}
	// 5 roots × 101 nodes consumed from the shared ceiling.
	if want := maxTotalNodes - 5*101; ctx.nodesLeft != want {
		t.Fatalf("nodesLeft = %d after 5 roots, want %d", ctx.nodesLeft, want)
	}
}

// TestLocalsSiblingPointerAliasesAcrossRoots guards the #167 invariant under a
// shared context: cycle state stays strictly path-local, so a later root
// pointing at an address an earlier root already visited still expands.
func TestLocalsSiblingPointerAliasesAcrossRoots(t *testing.T) {
	ptrType := testPointerType("*int", testIntType())
	backend := newValueMemoryBackend()
	backend.seedUint64(0x1000, 0x2000)
	backend.seedUint64(0x1008, 0x2000)
	backend.seedUint64(0x2000, 42)

	r := &dwarfReader{}
	ctx := newFormatCtx(backend)
	addrs := []uint64{0x1000, 0x1008}

	vars := formatRequestRoots(ctx, len(addrs), func(i int) protocol.Variable {
		v := r.formatRoot(ctx, "alias", ptrType, addrs[i])
		if len(ctx.activePointers) != 0 {
			t.Errorf("root %d leaked %d active pointer(s) into the next root",
				i, len(ctx.activePointers))
		}
		return v
	})

	if len(vars) != 2 {
		t.Fatalf("roots = %d, want 2", len(vars))
	}
	for _, v := range vars {
		if len(v.Children) != 1 {
			t.Fatalf("%s children = %d, want the aliased pointee to expand", v.Name, len(v.Children))
		}
		if got := v.Children[0].Value; got != "42" {
			t.Errorf("%s pointee = %q, want 42", v.Name, got)
		}
	}
}

// TestLocalsSelfCycleStaysBoundedAcrossRoots checks a cyclic root neither runs
// away nor poisons the roots after it.
func TestLocalsSelfCycleStaysBoundedAcrossRoots(t *testing.T) {
	nodeType := &dwarf.StructType{
		CommonType: dwarf.CommonType{ByteSize: 8, Name: "node"},
		StructName: "node",
		Kind:       "struct",
	}
	nodePtr := testPointerType("*node", nodeType)
	nodeType.Field = []*dwarf.StructField{{Name: "Next", Type: nodePtr, ByteOffset: 0}}

	backend := newValueMemoryBackend()
	backend.seedUint64(0x1000, 0x2000)
	backend.seedUint64(0x2000, 0x2000)

	r := &dwarfReader{}
	ctx := newFormatCtx(backend)

	vars := formatRequestRoots(ctx, 3, func(i int) protocol.Variable {
		return r.formatRoot(ctx, "head", nodePtr, 0x1000)
	})

	if len(vars) != 3 {
		t.Fatalf("roots = %d, want 3 (a bounded cycle must not exhaust the budget)", len(vars))
	}
	for i, v := range vars {
		if total := countNodes(v); total > 4 {
			t.Fatalf("root %d self-cycle node count = %d, want <= 4", i, total)
		}
		if len(v.Children) != 1 {
			t.Fatalf("root %d expanded %d children, want the first hop", i, len(v.Children))
		}
	}
}

// filledBackend returns non-zero memory so Go string headers decode to a
// non-nil pointer and an over-long length, making every string node pay the
// maxStringBytes read. Used to hit the BYTE ceiling while node counts stay low.
type filledBackend struct {
	readableBackend
	bytes int
}

func (b *filledBackend) ReadMemory(_ uint64, dst []byte) error {
	for i := range dst {
		dst[i] = 0x01
	}
	b.bytes += len(dst)
	return nil
}

// TestLocalsByteCeilingStopsRequest exercises the byte half of the shared
// ceiling independently: [100]string roots are cheap in nodes (101 each) but
// expensive in bytes (~272 each), so the request must stop on bytesLeft.
func TestLocalsByteCeilingStopsRequest(t *testing.T) {
	stringType := &dwarf.StructType{
		CommonType: dwarf.CommonType{ByteSize: 16, Name: "string"},
		StructName: "string",
		Kind:       "struct",
	}
	arrType := &dwarf.ArrayType{Type: stringType, Count: 100}

	backend := &filledBackend{}
	r := &dwarfReader{}
	ctx := newFormatCtx(backend)

	const roots = 200
	vars := formatRequestRoots(ctx, roots, func(i int) protocol.Variable {
		return r.formatRoot(ctx, "strings", arrType, 0x1000)
	})

	if nodes := countAllNodes(vars); nodes >= maxTotalNodes {
		t.Fatalf("node count %d reached the node ceiling; this fixture must stop on bytes", nodes)
	}
	if ctx.bytesLeft > 0 {
		t.Fatalf("bytesLeft = %d, want the byte ceiling to be the limiter", ctx.bytesLeft)
	}
	if backend.bytes > maxTotalBytes {
		t.Fatalf("bytes read %d exceed the hard shared byte ceiling %d",
			backend.bytes, maxTotalBytes)
	}
	if len(vars) >= roots {
		t.Fatalf("returned %d roots, want the loop to stop once bytes ran out", len(vars))
	}
	if last := vars[len(vars)-1]; last.Value != truncatedValue {
		t.Fatalf("last root = %q, want the truncation marker", last.Value)
	}
}

// TestLocalsDegradesUnreadableRootsWithoutAbort keeps the per-node degradation
// contract under a shared budget: an unreadable or untyped root renders as a
// degraded node and the roots after it still format.
func TestLocalsDegradesUnreadableRootsWithoutAbort(t *testing.T) {
	backend := newValueMemoryBackend()
	backend.seedUint64(0x3000, 7)

	r := &dwarfReader{}
	ctx := newFormatCtx(backend)
	intType := testIntType()

	vars := formatRequestRoots(ctx, 3, func(i int) protocol.Variable {
		switch i {
		case 0:
			return r.formatRoot(ctx, "missing", intType, 0x9000) // unmapped
		case 1:
			return r.formatRoot(ctx, "untyped", nil, 0x9000) // unknown type
		default:
			return r.formatRoot(ctx, "ok", intType, 0x3000)
		}
	})

	if len(vars) != 3 {
		t.Fatalf("roots = %d, want 3", len(vars))
	}
	if got, want := vars[0].Value, "<unreadable: unreadable memory>"; got != want {
		t.Errorf("unreadable root = %q, want %q", got, want)
	}
	if got, want := vars[1].Value, "<unreadable: unreadable memory>"; got != want {
		t.Errorf("untyped root = %q, want %q", got, want)
	}
	if got := vars[2].Value; got != "7" {
		t.Errorf("root after degraded ones = %q, want 7", got)
	}
}

// TestEvaluateBudgetIsPerRequest pins that Evaluate — one root per request —
// keeps a fresh ceiling, so it is unaffected by the Locals sharing and by any
// earlier inspection.
func TestEvaluateBudgetIsPerRequest(t *testing.T) {
	nested := deepArrayType()
	r := &dwarfReader{}

	first := countNodes(r.formatTyped(readableBackend{}, "grid", nested, 0x1000))
	second := countNodes(r.formatTyped(readableBackend{}, "grid", nested, 0x1000))

	if first != second {
		t.Fatalf("evaluate node counts differ across requests (%d then %d); the budget must reset per request",
			first, second)
	}
	if first < maxTotalNodes {
		t.Fatalf("evaluate expanded only %d nodes, want a full ceiling of %d", first, maxTotalNodes)
	}
}
