package debugger

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type goroutineMemoryBackend struct {
	mem    map[uint64]byte
	regs   Registers
	regErr error
	reads  map[uint64]int
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
		mProcid:       0,
		mID:           8,
		mSpinning:     16,
		mG0:           24,
		mG0OK:         true,
		mGsignal:      32,
		mGsignalOK:    true,
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

func seedTestM(
	backend *goroutineMemoryBackend,
	layout *goLayout,
	mptr, procid, curg, next uint64,
) {
	backend.seedU64(mptr+uint64(layout.mProcid), procid)
	backend.seedU64(mptr+uint64(layout.mID), procid)
	backend.seedU64(mptr+uint64(layout.mCurg), curg)
	backend.seedU64(mptr+uint64(layout.mAlllink), next)
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
	return b.regs, b.regErr
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
	result := e.buildGoroutineListFrom(layout, allgsPtr, length, liveSP, 0x1234, 0, 0)
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
	for i := 0; i < maxGoroutineScan; i++ {
		if got[i+1].ID != i+1 {
			t.Fatalf("rich goroutine %d has id %d, want preserved prefix id %d", i, got[i+1].ID, i+1)
		}
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
		layout, allgsPtr, length, stackLo+8, 0x1234, currentG, 0,
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
		layout, allgsPtr, length, 0xdeadbeef, 0x1234, 0, 0,
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

func TestBuildGoroutineListUsesMCurgForSchedulerStop(t *testing.T) {
	const (
		allgsPtr = uint64(0x1000)
		mptr     = uint64(0x400000)
		systemG  = uint64(0x500000)
		currentG = uint64(0x500100)
		systemSP = uint64(0xa0000008)
	)
	layout := goroutineTestLayout()
	for _, tc := range []struct {
		name   string
		offset int64
	}{
		{name: "g0", offset: layout.mG0},
		{name: "gsignal", offset: layout.mGsignal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := newGoroutineMemoryBackend()
			backend.seedU64(allgsPtr, currentG)
			seedTestGoroutine(backend, layout, systemG, 0, systemSP-8, systemSP+0xff8)
			seedTestGoroutine(backend, layout, currentG, 99, 0xb0000000, 0xb0001000)
			backend.seedU64(systemG+uint64(layout.gM), mptr)
			backend.seedU64(currentG+uint64(layout.gM), mptr)
			seedTestM(backend, layout, mptr, 77, currentG, 0)
			backend.seedU64(mptr+uint64(tc.offset), systemG)

			e := &engine{backend: backend}
			result := e.buildGoroutineListFrom(
				layout, allgsPtr, 1, systemSP, 0x1234, systemG, 0,
			)
			got, anchorID := result.Items, result.AnchorID
			if !result.Complete || len(got) != 1 || !got[0].Current || got[0].ID != 99 {
				t.Fatalf("goroutines = %+v, complete = %t; want current m.curg id 99", got, result.Complete)
			}
			if anchorID != 0 {
				t.Fatalf("anchor id = %d, want 0 because m.curg was already in the rich prefix", anchorID)
			}
			if got[0].ThreadID != 77 {
				t.Fatalf("current thread id = %d, want 77", got[0].ThreadID)
			}
		})
	}
}

func TestUnresolvedCurrentIdentityNeverFallsBackToGoroutineOne(t *testing.T) {
	const (
		allgsPtr = uint64(0x1000)
		realG    = uint64(0x200000)
		systemG  = uint64(0x300000)
		mptr     = uint64(0x400000)
		systemSP = uint64(0xa0000008)
	)
	fallback := protocol.Location{File: "runtime.go", Line: 42, Function: "runtime.schedule"}

	for _, tc := range []struct {
		name        string
		liveSP      uint64
		currentGptr uint64
		seed        func(*goroutineMemoryBackend, *goLayout)
	}{
		{
			name: "no live registers",
		},
		{
			name:        "unresolved g0",
			liveSP:      systemSP,
			currentGptr: systemG,
			seed: func(backend *goroutineMemoryBackend, layout *goLayout) {
				seedTestGoroutine(backend, layout, systemG, 0, systemSP-8, systemSP+0xff8)
				backend.seedU64(systemG+uint64(layout.gM), mptr)
				seedTestM(backend, layout, mptr, 77, 0, 0)
				backend.seedU64(mptr+uint64(layout.mG0), systemG)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := goroutineTestLayout()
			backend := newGoroutineMemoryBackend()
			backend.seedU64(allgsPtr, realG)
			seedTestGoroutine(backend, layout, realG, 1, 0x1000, 0x2000)
			if tc.seed != nil {
				tc.seed(backend, layout)
			}

			e := &engine{backend: backend}
			result := e.buildGoroutineListFrom(
				layout, allgsPtr, 1, tc.liveSP, 0x1234, tc.currentGptr, 0,
			)
			if !result.Complete || len(result.Items) != 1 ||
				result.Items[0].ID != 1 || result.Items[0].Current {
				t.Fatalf("goroutine walk = %+v, want complete real list with unknown current", result)
			}

			current := currentGoroutineFrom(protocol.GoroutineSnapshotPayload{
				Goroutines: result.Items,
			}, fallback)
			if current.ID != 0 || current.Status != "unknown" ||
				!current.Current || current.CurrentLoc != fallback {
				t.Fatalf("current goroutine = %+v, want honest unknown identity", current)
			}
		})
	}
}

func TestCurrentGoroutineFromRegisterRejectsUnrelatedPositiveGoID(t *testing.T) {
	const (
		mptr      = uint64(0x400000)
		candidate = uint64(0x500000)
		currentG  = uint64(0x500100)
	)
	layout := goroutineTestLayout()
	backend := newGoroutineMemoryBackend()
	seedTestGoroutine(backend, layout, candidate, 88, 0xa0000000, 0xa0001000)
	seedTestGoroutine(backend, layout, currentG, 99, 0xb0000000, 0xb0001000)
	backend.seedU64(candidate+uint64(layout.gM), mptr)
	backend.seedU64(currentG+uint64(layout.gM), mptr)
	seedTestM(backend, layout, mptr, 77, currentG, 0)

	e := &engine{backend: backend}
	result := e.currentGoroutineFromRegister(
		layout, candidate, 0xdeadbeef, 0x1234, currentGoroutineRich,
	)
	if !result.Complete || result.Found {
		t.Fatalf("current goroutine = %+v; want complete rejection of stale positive-goid candidate", result)
	}
}

func TestCurrentGoroutineIdentityReadsOnlyRequiredFields(t *testing.T) {
	const (
		gptr   = uint64(0x200000)
		liveSP = uint64(0x8008)
	)
	layout := goroutineTestLayout()
	backend := newGoroutineMemoryBackend()
	seedTestGoroutine(backend, layout, gptr, 42, 0x8000, 0x9000)

	e := &engine{backend: backend}
	result := e.currentGoroutineFromRegister(
		layout, gptr, liveSP, 0x1234, currentGoroutineIdentity,
	)
	if !result.Complete || !result.Found ||
		result.Item.ID != 42 || !result.Item.Current {
		t.Fatalf("current identity = %+v, want current g42", result)
	}

	required := []uint64{
		gptr + uint64(layout.gAtomicstatus),
		gptr + uint64(layout.gGoid),
		gptr + uint64(layout.gStack) + uint64(layout.stackLo),
		gptr + uint64(layout.gStack) + uint64(layout.stackHi),
	}
	if len(backend.reads) != len(required) {
		t.Fatalf("read addresses = %#v, want only %d required identity fields", backend.reads, len(required))
	}
	for _, addr := range required {
		if backend.reads[addr] != 1 {
			t.Fatalf("reads at %#x = %d, want 1", addr, backend.reads[addr])
		}
	}
}

func TestFindCurrentGoroutineByProcIDContinuesPastRichThreadCap(t *testing.T) {
	const (
		mBase    = uint64(0x400000)
		mStride  = uint64(0x100)
		currentG = uint64(0x900000)
		targetID = uint64(777777)
	)
	layout := goroutineTestLayout()
	backend := newGoroutineMemoryBackend()
	count := maxThreadScan + 2
	for i := 0; i < count; i++ {
		mptr := mBase + uint64(i)*mStride
		next := uint64(0)
		if i+1 < count {
			next = mptr + mStride
		}
		procid := uint64(1000 + i)
		curg := uint64(0)
		if i == count-1 {
			procid = targetID
			curg = currentG
			seedTestGoroutine(backend, layout, currentG, 99, 0xb0000000, 0xb0001000)
			backend.seedU64(currentG+uint64(layout.gM), mptr)
		}
		seedTestM(backend, layout, mptr, procid, curg, next)
	}

	e := &engine{backend: backend}
	result := e.findCurrentGoroutineByProcIDFrom(
		layout, mBase, targetID, 0x1234, currentGoroutineRich,
	)
	if !result.Complete || !result.Found ||
		!result.Item.Current || result.Item.ID != 99 || result.Item.ThreadID != int(targetID) {
		t.Fatalf("current goroutine = %+v; want id 99 on stopped thread %d", result, targetID)
	}
}

func TestFindCurrentGoroutineByProcIDBoundsTargetedContinuation(t *testing.T) {
	const (
		mBase   = uint64(0x400000)
		mStride = uint64(0x100)
	)
	layout := goroutineTestLayout()
	backend := newGoroutineMemoryBackend()
	count := maxThreadScan + maxCurrentThreadScan + 1
	for i := 0; i < count; i++ {
		mptr := mBase + uint64(i)*mStride
		next := uint64(0)
		if i+1 < count {
			next = mptr + mStride
		}
		seedTestM(backend, layout, mptr, uint64(1000+i), 0, next)
	}

	e := &engine{backend: backend}
	result := e.findCurrentGoroutineByProcIDFrom(
		layout, mBase, ^uint64(0), 0x1234, currentGoroutineRich,
	)
	if !result.Complete || result.Found {
		t.Fatalf("current goroutine = %+v; want complete bounded miss", result)
	}
	firstUnscanned := mBase + uint64(maxThreadScan+maxCurrentThreadScan)*mStride
	if reads := backend.reads[firstUnscanned+uint64(layout.mProcid)]; reads != 0 {
		t.Fatalf("first procid beyond targeted budget reads = %d, want 0", reads)
	}

	backend.reads = make(map[uint64]int)
	threads := e.readThreadsFrom(layout, mBase, 99, 0x1234)
	if !threads.Complete || len(threads.Items) != maxThreadScan {
		t.Fatalf("threads = %+v, want only the bounded rich prefix", threads)
	}
	if reads := backend.reads[firstUnscanned+uint64(layout.mCurg)]; reads != 0 {
		t.Fatalf("first curg beyond targeted budget reads = %d, want 0", reads)
	}
}

func TestReadThreadsAnchorsCurrentBeyondRichThreadCap(t *testing.T) {
	const (
		mBase    = uint64(0x400000)
		mStride  = uint64(0x100)
		currentG = uint64(0x900000)
	)
	layout := goroutineTestLayout()
	backend := newGoroutineMemoryBackend()
	count := maxThreadScan + 2
	for i := 0; i < count; i++ {
		mptr := mBase + uint64(i)*mStride
		next := uint64(0)
		if i+1 < count {
			next = mptr + mStride
		}
		curg := uint64(0)
		if i == count-1 {
			curg = currentG
			seedTestGoroutine(backend, layout, currentG, 99, 0xb0000000, 0xb0001000)
		}
		seedTestM(backend, layout, mptr, uint64(1000+i), curg, next)
	}

	e := &engine{backend: backend}
	result := e.readThreadsFrom(layout, mBase, 99, 0x1234)
	got := result.Items
	if !result.Complete {
		t.Fatal("readThreadsFrom degraded instead of returning the rich prefix plus current anchor")
	}
	if len(got) != maxThreadScan+1 {
		t.Fatalf("threads = %d, want rich prefix plus one current anchor", len(got))
	}
	current := 0
	for _, thread := range got {
		if thread.Current {
			current++
			if thread.GoID != 99 {
				t.Fatalf("current thread = %+v, want goid 99", thread)
			}
		}
	}
	if current != 1 {
		t.Fatalf("current threads = %d, want 1", current)
	}
}

func TestCurrentGoroutineFromReturnsUnknownInsteadOfFirstRealGoroutine(t *testing.T) {
	fallback := protocol.Location{File: "runtime.go", Line: 42, Function: "runtime.schedule"}
	for _, snap := range []protocol.GoroutineSnapshotPayload{
		{Goroutines: []protocol.Goroutine{{ID: 1}, {ID: 2}}},
		{Goroutines: []protocol.Goroutine{{Status: "unknown", Current: true}}},
	} {
		got := currentGoroutineFrom(snap, fallback)
		if got.ID != 0 || got.Status != "unknown" || !got.Current || got.CurrentLoc != fallback {
			t.Fatalf("fallback goroutine = %+v, want current synthetic unknown at %+v", got, fallback)
		}
	}

	got := currentGoroutineFrom(protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{{ID: 99, Status: "running", Current: true}},
	}, fallback)
	if got.ID != 99 || got.CurrentLoc != fallback {
		t.Fatalf("identified current goroutine = %+v, want frame location %+v", got, fallback)
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
	if len(snapshot.Goroutines) != 1 ||
		snapshot.Goroutines[0].ID != 0 ||
		snapshot.Goroutines[0].Status != "unknown" ||
		!snapshot.Goroutines[0].Current {
		t.Fatalf("degraded snapshot goroutines = %+v, want one current unknown goroutine", snapshot.Goroutines)
	}
	if _, ok := e.prevGoids[42]; !ok || len(e.prevGoids) != 1 {
		t.Fatalf("prevGoids = %v, want unchanged {42}", e.prevGoids)
	}
}

func TestLiveRegistersPreservesStoppedTIDOnReadFailure(t *testing.T) {
	backend := newGoroutineMemoryBackend()
	backend.regErr = errors.New("registers unavailable")
	e := &engine{backend: backend, curTID: 77}

	tid, sp, pc, gptr := e.liveRegisters()
	if tid != 77 || sp != 0 || pc != 0 || gptr != 0 {
		t.Fatalf("live registers = tid:%d sp:%x pc:%x g:%x, want tid 77 with unknown registers", tid, sp, pc, gptr)
	}
}
