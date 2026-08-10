package debugger

// Regression gates for the coherent read of the goroutine table's metadata.
//
// A Linux ptrace stop halts only the trapping thread, so runtime.allgadd can
// republish runtime.allgs while this walk is reading it. allgadd publishes the
// array pointer before the length, so a reader that takes the pointer first can
// pair a stale array with its successor's length and walk past the end of the
// old allocation. Crucially that overrun READS SUCCESSFULLY — the word after a
// heap object is mapped — so the walk reports Complete and the incomplete-read
// degradation path never fires. The defect is prevented by ordering, in
// allgsMetadata, and these tests exist to keep that ordering.
//
// The fixture models tracee memory with real mapping (a read that leaves every
// mapped region fails, as process_vm_readv would) and drives the production
// goroutineSnapshot/buildGoroutineList against DWARF-resolved runtime offsets
// taken from a real Go binary, so nothing here depends on hardcoded layout.

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// Fabricated tracee addresses; arbitrary but disjoint and aligned.
const (
	probeAllgsHdr = uint64(0x004c0000) // runtime.allgs slice header (ptr,len,cap)
	probeAllgLen  = uint64(0x004c0040) // runtime.allglen
	probeAllgPtr  = uint64(0x004c0080) // runtime.allgptr
	probeAllmHdr  = uint64(0x004c0100) // runtime.allm

	// probeOldArray is the pre-growth backing array: capacity 4 (32 bytes).
	// probeOldRegion also covers the next object in the same size-class span,
	// which is what a read one word past the array's end actually lands on.
	// Mapping it is the point: that read succeeds.
	probeOldArray  = uint64(0x00800000)
	probeOldArrCap = uint64(4)
	probeOldRegion = uint64(64)

	probeNewArray  = uint64(0x00801000)
	probeNewArrCap = uint64(8)

	probeGBase   = uint64(0x00900000)
	probeGStride = uint64(0x400)
	probeGCount  = 5 // goids 101..105

	// probeDecoy models an unrelated live heap object that the span-neighbour
	// word points at. Read as a runtime.g it yields a plausible positive goid
	// and a non-dead status, i.e. a phantom goroutine.
	probeDecoy     = uint64(0x00a00000)
	probeDecoySize = uint64(0x400)

	probeM     = uint64(0x00b00000)
	probeMSize = uint64(0x400)

	probeStackLo = uint64(0x00c00000)
	probeStackHi = uint64(0x00c10000)
	probeSP      = uint64(0x00c08000)

	// probePhantomGoid is heap-pointer shaped, which is what the goid word of an
	// unrelated object tends to look like. As an int64 it is positive, so the
	// goroutine read would accept it.
	probePhantomGoid = int64(0x00007f9ac4010018)
)

type probeRegion struct {
	base uint64
	data []byte
}

// probeBackend is a memory-model Backend. Unlike engine_test.go's fakeBackend
// (whose map-backed memory can never fail) it models real mapping, which is
// what lets these tests assert that the out-of-bounds read SUCCEEDS.
//
// sibling models a concurrently running tracee thread, fired after an exact
// read index so a test can place it precisely inside the reader's sequence.
type probeBackend struct {
	regions []probeRegion

	sibling       func(*probeBackend)
	fireAfterRead int
	readIdx       int
	siblingFired  bool

	reads      int
	readErrors int
	readAddrs  map[uint64]int
}

func (b *probeBackend) mapRegion(base, size uint64) {
	b.regions = append(b.regions, probeRegion{base: base, data: make([]byte, size)})
}

func (b *probeBackend) find(addr uint64, n int) []byte {
	for i := range b.regions {
		r := &b.regions[i]
		if addr >= r.base && addr+uint64(n) <= r.base+uint64(len(r.data)) {
			return r.data[addr-r.base:]
		}
	}
	return nil
}

func (b *probeBackend) poke64(addr, v uint64) {
	s := b.find(addr, 8)
	if s == nil {
		panic(fmt.Sprintf("probe fixture: unmapped write at %#x", addr))
	}
	binary.LittleEndian.PutUint64(s[:8], v)
}

func (b *probeBackend) poke32(addr uint64, v uint32) {
	s := b.find(addr, 4)
	if s == nil {
		panic(fmt.Sprintf("probe fixture: unmapped write at %#x", addr))
	}
	binary.LittleEndian.PutUint32(s[:4], v)
}

func (b *probeBackend) ReadMemory(addr uint64, dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	b.reads++
	if b.readAddrs == nil {
		b.readAddrs = map[uint64]int{}
	}
	b.readAddrs[addr]++

	src := b.find(addr, len(dst))
	if src == nil {
		b.readErrors++
		return fmt.Errorf("probe: unmapped read %#x len %d", addr, len(dst))
	}
	copy(dst, src[:len(dst)])

	b.readIdx++
	if b.fireAfterRead > 0 && !b.siblingFired && b.readIdx == b.fireAfterRead {
		b.siblingFired = true
		if b.sibling != nil {
			b.sibling(b)
		}
	}
	return nil
}

func (b *probeBackend) WriteMemory(addr uint64, src []byte) error {
	dst := b.find(addr, len(src))
	if dst == nil {
		return fmt.Errorf("probe: unmapped write %#x", addr)
	}
	copy(dst, src)
	return nil
}

func (b *probeBackend) GetRegisters(int) (Registers, error) {
	return Registers{SP: probeSP}, nil
}
func (b *probeBackend) SetRegisters(int, Registers) error { return nil }
func (b *probeBackend) Threads() ([]int, error)           { return []int{1}, nil }
func (b *probeBackend) ContinueProcess() error            { return nil }
func (b *probeBackend) SingleStep(int) error              { return nil }
func (b *probeBackend) StopProcess() error                { return nil }
func (b *probeBackend) PauseSignal() int                  { return 0 }
func (b *probeBackend) Wait() (StopEvent, error)          { return StopEvent{}, nil }

// arm schedules the sibling after read index n and clears the read counters.
func (b *probeBackend) arm(n int) {
	b.readIdx = 0
	b.fireAfterRead = n
	b.siblingFired = false
	b.reads, b.readErrors = 0, 0
	b.readAddrs = map[uint64]int{}
}

func gAddr(i int) uint64 { return probeGBase + uint64(i)*probeGStride }

// setMetadata publishes one coherent generation to both the atomic mirror and
// the raw slice header, so either read path sees the same table.
func (b *probeBackend) setMetadata(ptr, length, capacity uint64) {
	b.poke64(probeAllgsHdr+0, ptr)
	b.poke64(probeAllgsHdr+8, length)
	b.poke64(probeAllgsHdr+16, capacity)
	b.poke64(probeAllgPtr, ptr)
	b.poke64(probeAllgLen, length)
}

// siblingAllgadd replays runtime.allgadd's publication order for both the raw
// header (cap, pointer, length — the compiler's store order) and the atomic
// mirror (allgptr then allglen — the runtime's documented order). Pointer
// before length in both is exactly what makes a pointer-first reader unsafe.
func siblingAllgadd(b *probeBackend) {
	b.poke64(probeAllgsHdr+16, probeNewArrCap)
	b.poke64(probeAllgsHdr+0, probeNewArray)
	b.poke64(probeAllgsHdr+8, 5)

	b.poke64(probeAllgPtr, probeNewArray)
	b.poke64(probeAllgLen, 5)
}

// newProbeBackend lays out a miniature but structurally faithful tracee heap:
// five runtime.g structs, a full pre-growth array of capacity 4, a post-growth
// array of capacity 8, one runtime.m, and a mapped span neighbour immediately
// after the old array whose first word points at an unrelated live object.
func newProbeBackend(l *goLayout) *probeBackend {
	b := &probeBackend{}
	b.mapRegion(probeAllgsHdr, 24)
	b.mapRegion(probeAllgLen, 8)
	b.mapRegion(probeAllgPtr, 8)
	b.mapRegion(probeAllmHdr, 8)
	b.mapRegion(probeOldArray, probeOldRegion)
	b.mapRegion(probeNewArray, probeNewArrCap*8)
	for i := 0; i < probeGCount; i++ {
		b.mapRegion(gAddr(i), probeGStride)
	}
	b.mapRegion(probeDecoy, probeDecoySize)
	b.mapRegion(probeM, probeMSize)

	for i := 0; i < probeGCount; i++ {
		g := gAddr(i)
		b.poke32(g+uint64(l.gAtomicstatus), 1) // _Grunnable
		b.poke64(g+uint64(l.gGoid), uint64(101+i))
		b.poke64(g+uint64(l.gStartpc), 0) // zero PCs keep locForPC cheap
		b.poke64(g+uint64(l.gGopc), 0)
		b.poke64(g+uint64(l.gSched)+uint64(l.bufPC), 0)
		// Only g101 owns the stack the stopped SP falls in, so exactly one
		// goroutine is Current.
		lo, hi := probeStackLo+uint64(i+1)*0x100000, probeStackHi+uint64(i+1)*0x100000
		if i == 0 {
			lo, hi = probeStackLo, probeStackHi
		}
		b.poke64(g+uint64(l.gStack)+uint64(l.stackLo), lo)
		b.poke64(g+uint64(l.gStack)+uint64(l.stackHi), hi)
	}

	for i := uint64(0); i < probeOldArrCap; i++ {
		b.poke64(probeOldArray+i*8, gAddr(int(i)))
	}
	// The span neighbour one word past the end of the old array.
	b.poke64(probeOldArray+probeOldArrCap*8, probeDecoy)

	for i := 0; i < probeGCount; i++ {
		b.poke64(probeNewArray+uint64(i)*8, gAddr(i))
	}

	// The decoy read as a runtime.g: non-dead status, plausible positive goid.
	b.poke32(probeDecoy+uint64(l.gAtomicstatus), 1)
	b.poke64(probeDecoy+uint64(l.gGoid), uint64(probePhantomGoid))

	b.poke64(probeM+uint64(l.mProcid), 7)
	b.poke64(probeM+uint64(l.mID), 1)
	b.poke64(probeM+uint64(l.mCurg), gAddr(0))
	b.poke64(probeM+uint64(l.mAlllink), 0)
	b.poke64(probeAllmHdr, probeM)

	b.setMetadata(probeOldArray, 4, 4)
	return b
}

// buildProbeTarget compiles a throwaway Go binary so the fixture resolves
// runtime struct offsets from REAL DWARF instead of hardcoding them.
func buildProbeTarget(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := "package main\n\nfunc main() { done := make(chan struct{}); go func() { close(done) }(); <-done }\n"
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probetarget\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "probetarget")
	cmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", bin, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build probe target: %v\n%s", err, out)
	}
	return bin
}

// newProbeEngine wires the fixture to a real DWARF reader. withMirror=false
// marks runtime.allglen/runtime.allgptr as absent (a cached zero address is how
// runtimeVarAddr reports "resolved, not present"), which forces the raw-header
// fallback path.
func newProbeEngine(t *testing.T, withMirror bool) (*engine, *probeBackend) {
	t.Helper()
	dr, err := openDWARF(buildProbeTarget(t))
	if err != nil {
		t.Fatalf("openDWARF: %v", err)
	}
	layout := resolveGoLayout(dr)
	if !layout.valid {
		t.Fatal("probe: could not resolve runtime layout from DWARF")
	}
	b := newProbeBackend(&layout)
	e := &engine{backend: b, dw: dr, curTID: 1}

	// The fixture's addresses are absolute, so override whatever the binary's
	// DWARF reported.
	dr.cacheMu.Lock()
	if dr.varAddrs == nil {
		dr.varAddrs = map[string]uint64{}
	}
	dr.varAddrs["runtime.allgs"] = probeAllgsHdr
	dr.varAddrs["runtime.allm"] = probeAllmHdr
	if withMirror {
		dr.varAddrs["runtime.allglen"] = probeAllgLen
		dr.varAddrs["runtime.allgptr"] = probeAllgPtr
	} else {
		dr.varAddrs["runtime.allglen"] = 0
		dr.varAddrs["runtime.allgptr"] = 0
	}
	dr.cacheMu.Unlock()
	return e, b
}

func goids(gs []protocol.Goroutine) []int {
	out := make([]int, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.ID)
	}
	return out
}

func containsID(ids []int, want int) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// assertNoFabrication fails when a snapshot reports a goroutine outside the
// fixture's real set, or any lifecycle delta mentioning one.
func assertNoFabrication(t *testing.T, label string, snap protocol.GoroutineSnapshotPayload) {
	t.Helper()
	real := map[int]bool{101: true, 102: true, 103: true, 104: true, 105: true}
	for _, g := range snap.Goroutines {
		if !real[g.ID] {
			t.Fatalf("%s: fabricated goroutine %d in %v", label, g.ID, goids(snap.Goroutines))
		}
	}
	for _, id := range snap.Created {
		if !real[id] {
			t.Fatalf("%s: fabricated Created delta %d (%v)", label, id, snap.Created)
		}
	}
	for _, id := range snap.Exited {
		if !real[id] {
			t.Fatalf("%s: fabricated Exited delta %d (%v)", label, id, snap.Exited)
		}
	}
}

// runTornSchedules drives the production snapshot once per injection point,
// firing the sibling allgadd after every read index in the reader's sequence,
// and fails if any schedule fabricates a goroutine or lifecycle delta.
func runTornSchedules(t *testing.T, withMirror bool, label string) {
	t.Helper()
	e, b := newProbeEngine(t, withMirror)

	b.arm(0)
	base := e.goroutineSnapshot()
	if got := goids(base.Goroutines); len(got) != 4 {
		t.Fatalf("%s baseline: want the four live goroutines, got %v", label, got)
	}
	total := b.reads

	for k := 1; k <= total+2; k++ {
		e.prevGoids = map[int]struct{}{101: {}, 102: {}, 103: {}, 104: {}}
		b.setMetadata(probeOldArray, 4, 4)
		b.sibling = siblingAllgadd
		b.arm(k)

		snap := e.goroutineSnapshot()
		assertNoFabrication(t, fmt.Sprintf("%s schedule %d", label, k), snap)
	}
	t.Logf("%s: %d schedules across a %d-read sequence, no fabricated goroutine or delta",
		label, total+2, total)
}

// TestAllgsMetadataNeverFabricatesViaMirror is the production gate. It drives
// the real goroutineSnapshot with the runtime atomic mirror available and
// requires that no interleaving of runtime.allgadd can fabricate a goroutine or
// a lifecycle delta.
func TestAllgsMetadataNeverFabricatesViaMirror(t *testing.T) {
	runTornSchedules(t, true, "mirror")
}

// TestAllgsMetadataUsesMirrorNotFallback proves the gate above actually
// exercises the runtime.allglen/runtime.allgptr path rather than passing
// because it silently fell back to the raw header.
func TestAllgsMetadataUsesMirrorNotFallback(t *testing.T) {
	e, b := newProbeEngine(t, true)
	b.arm(0)

	_ = e.goroutineSnapshot()

	if b.readAddrs[probeAllgLen] == 0 {
		t.Fatal("runtime.allglen was never read; the mirror path was not taken")
	}
	if b.readAddrs[probeAllgPtr] == 0 {
		t.Fatal("runtime.allgptr was never read; the mirror path was not taken")
	}
	for _, off := range []uint64{0, 8, 16} {
		if n := b.readAddrs[probeAllgsHdr+off]; n != 0 {
			t.Fatalf("raw runtime.allgs header +%d was read %d time(s) despite the mirror "+
				"being available", off, n)
		}
	}
	t.Logf("mirror path confirmed: allglen read %d time(s), allgptr %d time(s), raw header 0",
		b.readAddrs[probeAllgLen], b.readAddrs[probeAllgPtr])
}

// TestAllgsMetadataNeverFabricatesViaFallback is the fallback gate: with the
// mirror symbols absent, the raw slice header must still be read length-first
// so no schedule pairs a stale pointer with a fresh length.
func TestAllgsMetadataNeverFabricatesViaFallback(t *testing.T) {
	runTornSchedules(t, false, "fallback")
}

// TestAllgsMetadataFallbackUsesRawHeader proves the fallback gate exercises the
// raw header rather than the mirror.
func TestAllgsMetadataFallbackUsesRawHeader(t *testing.T) {
	e, b := newProbeEngine(t, false)
	b.arm(0)

	_ = e.goroutineSnapshot()

	if b.readAddrs[probeAllgsHdr+8] == 0 {
		t.Fatal("raw runtime.allgs length word was never read; fallback not taken")
	}
	if b.readAddrs[probeAllgsHdr+0] == 0 {
		t.Fatal("raw runtime.allgs pointer word was never read; fallback not taken")
	}
	if n := b.readAddrs[probeAllgLen] + b.readAddrs[probeAllgPtr]; n != 0 {
		t.Fatalf("mirror was read %d time(s) despite being unavailable", n)
	}
}

// TestAllgsNewGoroutineSurfacesNextSnapshot pins the accepted degradation: a
// goroutine appended during the read may be missed for exactly one snapshot
// (the Go runtime documents this for lock-free readers), and must then appear
// as a genuine creation rather than being lost.
func TestAllgsNewGoroutineSurfacesNextSnapshot(t *testing.T) {
	e, b := newProbeEngine(t, true)

	b.arm(0)
	if got := goids(e.goroutineSnapshot().Goroutines); len(got) != 4 {
		t.Fatalf("baseline: want four goroutines, got %v", got)
	}

	b.sibling = siblingAllgadd
	b.arm(1)
	torn := e.goroutineSnapshot()
	assertNoFabrication(t, "torn", torn)

	b.sibling = nil
	b.arm(0)
	next := e.goroutineSnapshot()
	assertNoFabrication(t, "recovery", next)
	if !containsID(goids(next.Goroutines), 105) {
		t.Fatalf("recovery: the appended goroutine must surface, got %v",
			goids(next.Goroutines))
	}
	if !containsID(next.Created, 105) {
		t.Fatalf("recovery: want Created=[105], got %v", next.Created)
	}
	t.Logf("torn snapshot=%v created=%v exited=%v; recovery snapshot=%v created=%v",
		goids(torn.Goroutines), torn.Created, torn.Exited,
		goids(next.Goroutines), next.Created)
}

// TestAllgsCoherentMetadataIsUnaffected is the negative control: with no
// concurrent republication the walk reports the whole table and the ordinary
// lifecycle delta, so the fix does not change steady-state behaviour.
func TestAllgsCoherentMetadataIsUnaffected(t *testing.T) {
	e, b := newProbeEngine(t, true)

	b.arm(0)
	first := e.goroutineSnapshot()
	if got := goids(first.Goroutines); len(got) != 4 || !containsID(got, 104) {
		t.Fatalf("baseline: got %v", got)
	}

	// The append completes fully before the reader starts.
	siblingAllgadd(b)
	b.sibling = nil
	b.arm(0)

	snap := e.goroutineSnapshot()
	assertNoFabrication(t, "coherent", snap)
	if !containsID(snap.Created, 105) {
		t.Fatalf("coherent: want Created=[105], got %v", snap.Created)
	}
	if len(snap.Exited) != 0 {
		t.Fatalf("coherent: want no exits, got %v", snap.Exited)
	}
	if b.readErrors != 0 {
		t.Fatalf("coherent: want no read failures, got %d", b.readErrors)
	}
}

// TestAllgsOutOfBoundsSlotWouldReadSuccessfully documents WHY completeness
// checking cannot substitute for ordering: the word one past the end of the old
// array is mapped, so reading it succeeds and produces a plausible goroutine.
// If this ever starts failing, the fixture has stopped modelling the hazard and
// the gates above would pass vacuously.
func TestAllgsOutOfBoundsSlotWouldReadSuccessfully(t *testing.T) {
	e, b := newProbeEngine(t, true)
	layout, ok := e.getGoLayout()
	if !ok {
		t.Fatal("probe: layout unavailable")
	}

	b.arm(0)
	oob, ok := e.readU64(probeOldArray + probeOldArrCap*8)
	if !ok {
		t.Fatal("the out-of-bounds slot read failed; the fixture no longer models a mapped " +
			"span neighbour, so the ordering gates would pass for the wrong reason")
	}
	if b.readErrors != 0 {
		t.Fatalf("want the out-of-bounds read to succeed, got %d failures", b.readErrors)
	}

	res := e.readGoroutine(layout, oob, probeSP, 0)
	if !res.Complete || !res.Include {
		t.Fatalf("the span neighbour must still look like a live goroutine "+
			"(Complete=%v Include=%v)", res.Complete, res.Include)
	}
	if res.Item.ID != int(probePhantomGoid) {
		t.Fatalf("want the phantom goid %d, got %d", probePhantomGoid, res.Item.ID)
	}
	t.Logf("hazard intact: the slot past the old array reads successfully (%d failures) and "+
		"yields goid %d with Complete=true — invisible to any completeness check",
		b.readErrors, res.Item.ID)
}

// TestAllgsUnpublishedMirrorFallsBackOrdered covers the third path: the mirror
// symbols exist but the runtime has not written them yet (they are populated by
// the first allgadd, so a pre-init stop reads zeroes). The walk must consult the
// raw header rather than reporting an empty table, and must still read it
// length-first so the fallback cannot fabricate either.
func TestAllgsUnpublishedMirrorFallsBackOrdered(t *testing.T) {
	e, b := newProbeEngine(t, true)

	// Mirror resolvable but unpublished; only the raw header carries the table.
	b.poke64(probeAllgLen, 0)
	b.poke64(probeAllgPtr, 0)
	b.arm(0)

	base := e.goroutineSnapshot()
	if got := goids(base.Goroutines); len(got) != 4 {
		t.Fatalf("unpublished mirror must fall back to the header, got %v", got)
	}
	if b.readAddrs[probeAllgsHdr+8] == 0 || b.readAddrs[probeAllgsHdr+0] == 0 {
		t.Fatal("raw header was not consulted after an unpublished mirror")
	}

	total := b.reads
	for k := 1; k <= total+2; k++ {
		e.prevGoids = map[int]struct{}{101: {}, 102: {}, 103: {}, 104: {}}
		b.setMetadata(probeOldArray, 4, 4)
		b.poke64(probeAllgLen, 0)
		b.poke64(probeAllgPtr, 0)
		// The sibling republishes the header only, matching a runtime whose
		// atomics this image cannot see.
		b.sibling = func(tb *probeBackend) {
			tb.poke64(probeAllgsHdr+16, probeNewArrCap)
			tb.poke64(probeAllgsHdr+0, probeNewArray)
			tb.poke64(probeAllgsHdr+8, 5)
		}
		b.arm(k)

		assertNoFabrication(t, fmt.Sprintf("unpublished-mirror schedule %d", k),
			e.goroutineSnapshot())
	}
	t.Logf("unpublished mirror: %d schedules, fell back to the header with no fabrication",
		total+2)
}

// TestRuntimeVarAddrsMatchesSingleLookups covers the one-pass symbol resolver
// the coherent read relies on. The fixtures above pre-seed the address cache, so
// without this the batched scan itself would go unexercised.
//
// It pins the two properties the batching must not break: every name resolves to
// exactly what the single-name resolver returns, and a name that is genuinely
// absent is cached as absent rather than being mistaken for one that the scan
// simply stopped before reaching.
func TestRuntimeVarAddrsMatchesSingleLookups(t *testing.T) {
	bin := buildProbeTarget(t)
	names := []string{"runtime.allglen", "runtime.allgptr", "runtime.allgs", "runtime.bingoNoSuchVar"}

	batched, err := openDWARF(bin)
	if err != nil {
		t.Fatalf("openDWARF: %v", err)
	}
	got := batched.runtimeVarAddrs(names...)

	for _, name := range names {
		single, err := openDWARF(bin) // fresh reader: cold cache per name
		if err != nil {
			t.Fatalf("openDWARF: %v", err)
		}
		wantAddr, wantOK := single.runtimeVarAddr(name)

		gotAddr, gotOK := got[name]
		if gotOK != wantOK || gotAddr != wantAddr {
			t.Fatalf("%s: batched (%#x, %v) != single (%#x, %v)",
				name, gotAddr, gotOK, wantAddr, wantOK)
		}
	}
	if _, present := got["runtime.bingoNoSuchVar"]; present {
		t.Fatal("an absent name must not appear in the batched result")
	}
	if len(got) != 3 {
		t.Fatalf("want the three real names resolved, got %d entries: %v", len(got), got)
	}

	// The absent name must be cached as decided, so a repeat call cannot rescan
	// and cannot start reporting it as present.
	batched.cacheMu.Lock()
	cached, decided := batched.varAddrs["runtime.bingoNoSuchVar"]
	batched.cacheMu.Unlock()
	if !decided || cached != 0 {
		t.Fatalf("absent name must be cached as 0, got (%#x, decided=%v)", cached, decided)
	}
	if second := batched.runtimeVarAddrs(names...); len(second) != len(got) {
		t.Fatalf("repeat call changed the result: %v vs %v", second, got)
	}
	t.Logf("batched resolution matches single lookups for %d names; absent name cached as absent",
		len(names))
}
