package debugger

import (
	"encoding/binary"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type goroutineMemoryBackend struct {
	mem   map[uint64]byte
	regs  Registers
	reads map[uint64]int
}

func newGoroutineMemoryBackend() *goroutineMemoryBackend {
	return &goroutineMemoryBackend{
		mem:   make(map[uint64]byte),
		reads: make(map[uint64]int),
	}
}

func goroutineTestLayout() *goLayout {
	return &goLayout{
		valid:         true,
		gAtomicstatus: 0,
		gGoid:         8,
		gStack:        16,
		stackLo:       0,
		stackHi:       8,
		gM:            32,
		gSched:        40,
		bufPC:         0,
		gWaitreason:   48,
		gParentGoid:   56,
		gGopc:         64,
		gStartpc:      72,
		mCurg:         80,
		mAlllink:      88,
	}
}

func seedTestGoroutine(
	backend *goroutineMemoryBackend,
	layout *goLayout,
	gptr uint64,
	goid uint64,
	stackLo uint64,
	stackHi uint64,
) {
	backend.seedU32(gptr+uint64(layout.gAtomicstatus), 2)
	backend.seedU64(gptr+uint64(layout.gGoid), goid)
	backend.seedU64(gptr+uint64(layout.gStack)+uint64(layout.stackLo), stackLo)
	backend.seedU64(gptr+uint64(layout.gStack)+uint64(layout.stackHi), stackHi)
}

func (b *goroutineMemoryBackend) seedU64(addr, value uint64) {
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], value)
	for i, v := range raw {
		b.mem[addr+uint64(i)] = v
	}
}

func (b *goroutineMemoryBackend) seedU32(addr uint64, value uint32) {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], value)
	for i, v := range raw {
		b.mem[addr+uint64(i)] = v
	}
}

func (b *goroutineMemoryBackend) ContinueProcess() error { return nil }
func (b *goroutineMemoryBackend) SingleStep(int) error   { return nil }
func (b *goroutineMemoryBackend) StopProcess() error     { return nil }
func (b *goroutineMemoryBackend) PauseSignal() int       { return 0 }

func (b *goroutineMemoryBackend) ReadMemory(addr uint64, dst []byte) error {
	b.reads[addr]++
	for i := range dst {
		dst[i] = b.mem[addr+uint64(i)]
	}
	return nil
}

func (b *goroutineMemoryBackend) WriteMemory(addr uint64, src []byte) error {
	for i, v := range src {
		b.mem[addr+uint64(i)] = v
	}
	return nil
}

func (b *goroutineMemoryBackend) GetRegisters(int) (Registers, error) {
	return b.regs, nil
}

func (b *goroutineMemoryBackend) SetRegisters(_ int, regs Registers) error {
	b.regs = regs
	return nil
}

func (b *goroutineMemoryBackend) Threads() ([]int, error) {
	return []int{1}, nil
}

func (b *goroutineMemoryBackend) Wait() (StopEvent, error) {
	return StopEvent{}, ErrProcessExited
}

func TestBuildGoroutineListAnchorsCurrentBeyondRichScan(t *testing.T) {
	const (
		allgsPtr = uint64(0x1000)
		gBase    = uint64(0x200000)
		gStride  = uint64(0x100)
		stack    = uint64(0x80000000)
	)
	layout := goroutineTestLayout()

	backend := newGoroutineMemoryBackend()
	length := uint64(maxGoroutineScan + 4)
	currentIndex := length - 1
	for i := uint64(0); i < length; i++ {
		gptr := gBase + i*gStride
		backend.seedU64(allgsPtr+i*8, gptr)
		seedTestGoroutine(backend, layout, gptr, i+1, stack+i*gStride, stack+(i+1)*gStride)
	}

	liveSP := stack + currentIndex*gStride + 8
	e := &engine{backend: backend}
	result := e.buildGoroutineListFrom(layout, allgsPtr, length, liveSP, 0x1234, 0)
	got, anchorID := result.Items, result.AnchorID
	if !result.Complete {
		t.Fatal("buildGoroutineListFrom degraded instead of returning the rich prefix plus current anchor")
	}
	if want := maxGoroutineScan + 1; len(got) != want {
		t.Fatalf("len(goroutines) = %d, want %d", len(got), want)
	}
	if !got[0].Current || got[0].ID != int(currentIndex+1) {
		t.Fatalf("first goroutine = %+v, want current anchor id %d", got[0], currentIndex+1)
	}
	if anchorID != got[0].ID {
		t.Fatalf("anchor id = %d, want %d", anchorID, got[0].ID)
	}

	currentCount := 0
	for _, g := range got {
		if g.Current {
			currentCount++
		}
	}
	if currentCount != 1 {
		t.Fatalf("current goroutines = %d, want 1", currentCount)
	}

	unmatchedTail := gBase + uint64(maxGoroutineScan+1)*gStride
	if reads := backend.reads[unmatchedTail+uint64(layout.gGoid)]; reads != 0 {
		t.Fatalf("unmatched tail goid reads = %d, want 0; tail scan must read stack bounds only", reads)
	}
}

func TestBuildGoroutineListUsesDirectCurrentPointerBeyondFallback(t *testing.T) {
	const (
		allgsPtr = uint64(0x1000)
		prefixG  = uint64(0x200000)
		currentG = uint64(0x300000)
		stackLo  = uint64(0x90000000)
	)
	layout := goroutineTestLayout()
	backend := newGoroutineMemoryBackend()
	backend.seedU64(allgsPtr, prefixG)
	seedTestGoroutine(backend, layout, prefixG, 1, 0x1000, 0x2000)
	seedTestGoroutine(backend, layout, currentG, 99, stackLo, stackLo+0x1000)

	length := uint64(maxGoroutineScan*3 + 1)
	e := &engine{backend: backend}
	result := e.buildGoroutineListFrom(
		layout, allgsPtr, length, stackLo+8, 0x1234, currentG,
	)
	got, anchorID := result.Items, result.AnchorID
	if !result.Complete || len(got) != 2 {
		t.Fatalf("goroutines = %+v, complete = %t; want rich entry plus direct anchor", got, result.Complete)
	}
	if !got[0].Current || got[0].ID != 99 || anchorID != 99 {
		t.Fatalf("anchor = %+v, anchor id = %d; want current id 99", got[0], anchorID)
	}
	firstTailEntry := allgsPtr + uint64(maxGoroutineScan)*8
	if reads := backend.reads[firstTailEntry]; reads != 0 {
		t.Fatalf("tail pointer reads = %d, want 0 when direct current pointer resolves", reads)
	}
}

func TestBuildGoroutineListBoundsFallbackTail(t *testing.T) {
	const (
		allgsPtr = uint64(0x1000)
		prefixG  = uint64(0x200000)
	)
	layout := goroutineTestLayout()
	backend := newGoroutineMemoryBackend()
	backend.seedU64(allgsPtr, prefixG)
	seedTestGoroutine(backend, layout, prefixG, 1, 0x1000, 0x2000)

	length := uint64(maxGoroutineScan*3 + 1)
	e := &engine{backend: backend}
	result := e.buildGoroutineListFrom(
		layout, allgsPtr, length, 0xdeadbeef, 0x1234, 0,
	)
	got, anchorID := result.Items, result.AnchorID
	if !result.Complete || len(got) != 1 || anchorID != 0 {
		t.Fatalf(
			"goroutines = %+v, anchor id = %d, complete = %t; want bounded rich result",
			got, anchorID, result.Complete,
		)
	}
	lastFallbackEntry := allgsPtr + uint64(maxGoroutineScan*2-1)*8
	if reads := backend.reads[lastFallbackEntry]; reads != 1 {
		t.Fatalf("last fallback pointer reads = %d, want 1", reads)
	}
	firstUnscannedEntry := allgsPtr + uint64(maxGoroutineScan*2)*8
	if reads := backend.reads[firstUnscannedEntry]; reads != 0 {
		t.Fatalf("first pointer beyond fallback budget reads = %d, want 0", reads)
	}
}

func TestFindCurrentGoroutineFromThreadList(t *testing.T) {
	const (
		firstM   = uint64(0x400000)
		secondM  = uint64(0x400100)
		firstG   = uint64(0x500000)
		currentG = uint64(0x500100)
		stackLo  = uint64(0xa0000000)
	)
	layout := goroutineTestLayout()
	backend := newGoroutineMemoryBackend()
	backend.seedU64(firstM+uint64(layout.mCurg), firstG)
	backend.seedU64(firstM+uint64(layout.mAlllink), secondM)
	backend.seedU64(secondM+uint64(layout.mCurg), currentG)
	backend.seedU64(secondM+uint64(layout.mAlllink), 0)
	seedTestGoroutine(backend, layout, firstG, 1, 0x1000, 0x2000)
	seedTestGoroutine(backend, layout, currentG, 99, stackLo, stackLo+0x1000)

	e := &engine{backend: backend}
	result := e.findCurrentGoroutineInThreadList(layout, firstM, stackLo+8, 0x1234)
	if !result.Complete || !result.Found || !result.Item.Current || result.Item.ID != 99 {
		t.Fatalf("current goroutine = %+v; want current id 99", result)
	}
}

func TestSnapshotGoroutineIDsExcludesTailAnchorFromLifecycleSet(t *testing.T) {
	gs := []protocol.Goroutine{
		{ID: 99, Current: true},
		{ID: 1},
		{ID: 2},
	}
	live, current := snapshotGoroutineIDs(gs, 99)
	if current != 99 {
		t.Fatalf("current id = %d, want 99", current)
	}
	if len(live) != 2 {
		t.Fatalf("lifecycle set = %v, want only rich ids 1 and 2", live)
	}
	if _, ok := live[99]; ok {
		t.Fatalf("lifecycle set = %v, must exclude tail anchor", live)
	}
}

func TestDegradedGoroutineSnapshotPreservesPreviousLiveSet(t *testing.T) {
	backend := newGoroutineMemoryBackend()
	backend.regs = Registers{SP: 0x1000, PC: 0x2000}
	e := &engine{
		backend:   backend,
		curTID:    1,
		prevGoids: map[int]struct{}{42: {}},
	}

	snapshot := e.goroutineSnapshot()
	if len(snapshot.Goroutines) != 1 || !snapshot.Goroutines[0].Current {
		t.Fatalf("degraded snapshot goroutines = %+v, want one current synthetic goroutine", snapshot.Goroutines)
	}
	if _, ok := e.prevGoids[42]; !ok || len(e.prevGoids) != 1 {
		t.Fatalf("prevGoids = %v, want unchanged {42}", e.prevGoids)
	}
}
