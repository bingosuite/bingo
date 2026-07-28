package debugger

import (
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// This file reads the Go runtime's goroutine (runtime.allgs) and OS-thread
// (runtime.allm) tables straight from the tracee's memory, using struct offsets
// resolved from DWARF at runtime (never hardcoded — they shift between Go
// versions). It powers the concurrency snapshot: the goroutine set with parent
// linkage for a spawn tree, the thread set, the current goroutine, and the
// created/exited lifecycle deltas. See AGENTS.md → goroutine snapshot reading.
//
// Everything here runs on the serialized engine loop while the tracee is
// suspended, so memory reads are race-free. Every read is best-effort: any
// failure (no DWARF, an unexpected layout, an unreadable address, or a runtime
// not yet initialized at the entry stop) degrades gracefully to the legacy
// single-synthetic goroutine rather than erroring the stop.

const (
	// maxGoroutineScan bounds the runtime.allgs walk so a corrupt slice header
	// or a pathological target can't make one snapshot read unboundedly.
	maxGoroutineScan = 8192
	// maxThreadScan bounds the runtime.allm linked-list walk likewise.
	maxThreadScan = 2048
	// maxGoStringLen caps a wait-reason string read from tracee memory.
	maxGoStringLen = 256

	gScanBit    = 0x1000 // _Gscan — OR'd onto a status while the GC scans a stack
	gStatusDead = 6      // _Gdead
)

// goLayout holds the DWARF-resolved byte offsets of the runtime structs the
// reader touches. Resolved once per loaded image. valid is false when any
// required field is missing, which sends callers down the fallback path.
type goLayout struct {
	valid bool

	gStack        int64 // g: embedded runtime.stack
	gM            int64 // g: *m
	gSched        int64 // g: embedded runtime.gobuf
	gAtomicstatus int64
	gGoid         int64
	gWaitreason   int64
	gParentGoid   int64
	gGopc         int64
	gStartpc      int64

	stackLo int64 // stack.lo, relative to gStack
	stackHi int64 // stack.hi, relative to gStack

	bufPC int64 // gobuf.pc, relative to gSched

	mProcid   int64
	mCurg     int64
	mID       int64
	mAlllink  int64
	mSpinning int64

	// waitReasonStrings array, for turning g.waitreason into a human string.
	wrBase   uint64
	wrCount  int
	wrStride int
}

// resolveGoLayout builds the layout from DWARF. Missing required offsets yield
// an invalid layout (valid=false); wait-reason resolution is optional.
func resolveGoLayout(dw *dwarfReader) goLayout {
	var l goLayout
	ok := true
	req := func(structName, field string) int64 {
		off, found := dw.structMemberOffset(structName, field)
		if !found {
			ok = false
		}
		return off
	}

	l.gStack = req("runtime.g", "stack")
	l.gM = req("runtime.g", "m")
	l.gSched = req("runtime.g", "sched")
	l.gAtomicstatus = req("runtime.g", "atomicstatus")
	l.gGoid = req("runtime.g", "goid")
	l.gWaitreason = req("runtime.g", "waitreason")
	l.gParentGoid = req("runtime.g", "parentGoid")
	l.gGopc = req("runtime.g", "gopc")
	l.gStartpc = req("runtime.g", "startpc")

	l.stackLo = req("runtime.stack", "lo")
	l.stackHi = req("runtime.stack", "hi")

	l.bufPC = req("runtime.gobuf", "pc")

	l.mProcid = req("runtime.m", "procid")
	l.mCurg = req("runtime.m", "curg")
	l.mID = req("runtime.m", "id")
	l.mAlllink = req("runtime.m", "alllink")
	l.mSpinning = req("runtime.m", "spinning")

	if base, count, stride, wrOK := dw.runtimeArrayInfo("runtime.waitReasonStrings"); wrOK {
		l.wrBase, l.wrCount = base, count
		if stride > 0 {
			l.wrStride = stride
		} else {
			l.wrStride = 16 // a Go string header is 2 words on 64-bit
		}
	}

	l.valid = ok
	return l
}

// getGoLayout returns the cached layout, resolving it on first use for the
// currently loaded image. Returns false when DWARF is absent or the layout
// can't be resolved.
func (e *engine) getGoLayout() (*goLayout, bool) {
	if e.dw == nil {
		return nil, false
	}
	if e.goLayout == nil {
		l := resolveGoLayout(e.dw)
		e.goLayout = &l
	}
	if !e.goLayout.valid {
		return nil, false
	}
	return e.goLayout, true
}

func (e *engine) readU64(addr uint64) (uint64, bool) {
	var b [8]byte
	if err := e.backend.ReadMemory(addr, b[:]); err != nil {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b[:]), true
}

func (e *engine) readU32(addr uint64) (uint32, bool) {
	var b [4]byte
	if err := e.backend.ReadMemory(addr, b[:]); err != nil {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[:]), true
}

func (e *engine) readU8(addr uint64) (uint8, bool) {
	var b [1]byte
	if err := e.backend.ReadMemory(addr, b[:]); err != nil {
		return 0, false
	}
	return b[0], true
}

// readGoStringHeader reads a Go string header (ptr,len) at hdrAddr and returns
// its bytes, capped at maxGoStringLen.
func (e *engine) readGoStringHeader(hdrAddr uint64) string {
	ptr, ok := e.readU64(hdrAddr)
	if !ok || ptr == 0 {
		return ""
	}
	n, ok := e.readU64(hdrAddr + 8)
	if !ok || n == 0 {
		return ""
	}
	if n > maxGoStringLen {
		n = maxGoStringLen
	}
	buf := make([]byte, n)
	if err := e.backend.ReadMemory(ptr, buf); err != nil {
		return ""
	}
	return string(buf)
}

// waitReasonString maps a g.waitreason enum value to its runtime string by
// indexing runtime.waitReasonStrings in tracee memory. "" when unresolved or
// out of range (index 0 is the zero reason, deliberately blank).
func (e *engine) waitReasonString(l *goLayout, reason uint8) string {
	if l.wrBase == 0 || l.wrCount == 0 || reason == 0 || int(reason) >= l.wrCount {
		return ""
	}
	return e.readGoStringHeader(l.wrBase + uint64(int(reason)*l.wrStride))
}

// goStatusString maps a goroutine's atomicstatus (scan bit already stripped) to
// a human-readable string. Values are stable across Go versions; unknowns fall
// back to a numeric form.
func goStatusString(s uint32) string {
	switch s {
	case 0:
		return "idle"
	case 1:
		return "runnable"
	case 2:
		return "running"
	case 3:
		return "syscall"
	case 4:
		return "waiting"
	case gStatusDead:
		return "dead"
	case 8:
		return "copystack"
	case 9:
		return "preempted"
	case 10:
		return "leaked"
	case 11:
		return "waiting for cgo callback"
	default:
		return fmt.Sprintf("status(%d)", s)
	}
}

// locForPC resolves a PC to a Location, tolerating a zero PC (returns an empty
// Location) so a parked goroutine with no scheduled PC doesn't error.
func (e *engine) locForPC(pc uint64) protocol.Location {
	if pc == 0 || e.dw == nil {
		return protocol.Location{}
	}
	return e.dw.locationForPC(pc)
}

// readGoroutine reads one runtime.g at gptr into a protocol.Goroutine. It
// returns ok=false when the goroutine should be skipped (freelist slot, dead,
// or unreadable). liveSP/livePC identify the currently-stopped thread so the
// goroutine running there is marked Current and gets its live PC as CurrentLoc.
func (e *engine) readGoroutine(l *goLayout, gptr, liveSP, livePC uint64) (protocol.Goroutine, bool) {
	rawStatus, ok := e.readU32(gptr + uint64(l.gAtomicstatus))
	if !ok {
		return protocol.Goroutine{}, false
	}
	status := rawStatus &^ gScanBit

	goid, ok := e.readI64(gptr + uint64(l.gGoid))
	if !ok || goid <= 0 || status == gStatusDead {
		// Freelist / dead slots carry goid 0 or a stale id; a UI wants only the
		// live set. Their departure is reported via the exited delta.
		return protocol.Goroutine{}, false
	}

	g := protocol.Goroutine{
		ID:     int(goid),
		Status: goStatusString(status),
	}
	if parent, ok := e.readI64(gptr + uint64(l.gParentGoid)); ok && parent > 0 {
		g.ParentID = int(parent)
	}
	if startpc, ok := e.readU64(gptr + uint64(l.gStartpc)); ok {
		g.StartLoc = e.locForPC(startpc)
	}
	if gopc, ok := e.readU64(gptr + uint64(l.gGopc)); ok {
		g.CreatedLoc = e.locForPC(gopc)
	}
	if status == 4 { // waiting
		if wr, ok := e.readU8(gptr + uint64(l.gWaitreason)); ok {
			g.WaitReason = e.waitReasonString(l, wr)
		}
	}

	lo, okLo := e.readU64(gptr + uint64(l.gStack) + uint64(l.stackLo))
	hi, okHi := e.readU64(gptr + uint64(l.gStack) + uint64(l.stackHi))
	current := okLo && okHi && liveSP != 0 && liveSP >= lo && liveSP < hi
	g.Current = current

	if mptr, ok := e.readU64(gptr + uint64(l.gM)); ok && mptr != 0 {
		if procid, ok := e.readU64(mptr + uint64(l.mProcid)); ok {
			g.ThreadID = int(procid)
		}
	}

	// The current goroutine's live PC is authoritative; a parked goroutine's
	// scheduled PC (gobuf.pc) is where it will resume.
	if current {
		g.CurrentLoc = e.locForPC(livePC)
	} else if schedPC, ok := e.readU64(gptr + uint64(l.gSched) + uint64(l.bufPC)); ok {
		g.CurrentLoc = e.locForPC(schedPC)
	}

	return g, true
}

func (e *engine) readI64(addr uint64) (int64, bool) {
	v, ok := e.readU64(addr)
	return int64(v), ok
}

// buildGoroutineList enumerates runtime.allgs. ok is false when the runtime
// can't be read (no DWARF, bad layout, unreadable slice, or no live goroutines
// yet), so callers fall back to the synthetic goroutine.
func (e *engine) buildGoroutineList(liveSP, livePC uint64) ([]protocol.Goroutine, bool) {
	l, ok := e.getGoLayout()
	if !ok {
		return nil, false
	}
	base, ok := e.dw.runtimeVarAddr("runtime.allgs")
	if !ok {
		return nil, false
	}
	ptr, ok := e.readU64(base) // slice header: array pointer @ +0
	if !ok || ptr == 0 {
		return nil, false
	}
	length, ok := e.readU64(base + 8) // slice header: len @ +8
	if !ok || length == 0 {
		return nil, false
	}
	if length > maxGoroutineScan {
		length = maxGoroutineScan
	}

	out := make([]protocol.Goroutine, 0, length)
	for i := uint64(0); i < length; i++ {
		gptr, ok := e.readU64(ptr + i*8)
		if !ok || gptr == 0 {
			continue
		}
		if g, ok := e.readGoroutine(l, gptr, liveSP, livePC); ok {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// readThreads walks runtime.allm and reports every OS thread (M). currentGoid
// (from the goroutine list) marks the thread whose goroutine is under
// inspection as Current and gives it the live PC. Best-effort: nil on failure.
func (e *engine) readThreads(currentGoid int, livePC uint64) []protocol.Thread {
	l, ok := e.getGoLayout()
	if !ok {
		return nil
	}
	base, ok := e.dw.runtimeVarAddr("runtime.allm")
	if !ok {
		return nil
	}
	mptr, ok := e.readU64(base) // runtime.allm is a *m; read the head pointer
	if !ok {
		return nil
	}

	var out []protocol.Thread
	for i := 0; i < maxThreadScan && mptr != 0; i++ {
		t := protocol.Thread{}
		if procid, ok := e.readU64(mptr + uint64(l.mProcid)); ok {
			t.ID = int(procid)
		}
		if id, ok := e.readI64(mptr + uint64(l.mID)); ok {
			t.MID = int(id)
		}
		if sp, ok := e.readU8(mptr + uint64(l.mSpinning)); ok {
			t.Spinning = sp != 0
		}
		if curg, ok := e.readU64(mptr + uint64(l.mCurg)); ok && curg != 0 {
			if goid, ok := e.readI64(curg + uint64(l.gGoid)); ok && goid > 0 {
				t.GoID = int(goid)
				if int(goid) == currentGoid {
					t.Current = true
					t.CurrentLoc = e.locForPC(livePC)
				} else if schedPC, ok := e.readU64(curg + uint64(l.gSched) + uint64(l.bufPC)); ok {
					t.CurrentLoc = e.locForPC(schedPC)
				}
			}
		}
		out = append(out, t)

		next, ok := e.readU64(mptr + uint64(l.mAlllink))
		if !ok {
			break
		}
		mptr = next
	}
	return out
}

// liveRegisters reads the currently-stopped thread's SP and PC (used to locate
// the current goroutine and to give it a live location). Zeroes on failure.
func (e *engine) liveRegisters() (sp, pc uint64) {
	tid, err := e.activeTID()
	if err != nil {
		return 0, 0
	}
	regs, err := e.backend.GetRegisters(tid)
	if err != nil {
		return 0, 0
	}
	return regs.SP, regs.PC
}

// syntheticGoroutine is the legacy fallback: a single goroutine standing in for
// the stopped thread's location when the runtime can't be introspected. It
// preserves behavior for stripped binaries, attach-without-DWARF, and the
// pre-runtime-init entry stop (and keeps the fakeBackend unit tests green).
func (e *engine) syntheticGoroutine(livePC uint64) protocol.Goroutine {
	return protocol.Goroutine{
		ID:         1,
		Status:     "waiting",
		CurrentLoc: e.locForPC(livePC),
		Current:    true,
	}
}

// readGoroutines returns the live goroutine set, falling back to a single
// synthetic goroutine when the runtime can't be read. This backs the on-demand
// Goroutines() query and the DAP threads list.
func (e *engine) readGoroutines() ([]protocol.Goroutine, error) {
	sp, pc := e.liveRegisters()
	if gs, ok := e.buildGoroutineList(sp, pc); ok {
		return gs, nil
	}
	return []protocol.Goroutine{e.syntheticGoroutine(pc)}, nil
}

// goroutineSnapshot builds the full concurrency picture and computes the
// created/exited deltas against the previous snapshot. It updates the engine's
// remembered live set, so successive snapshots report only what changed. Runs
// on the engine loop thread; prevGoids needs no synchronization.
func (e *engine) goroutineSnapshot() protocol.GoroutineSnapshotPayload {
	sp, pc := e.liveRegisters()

	gs, ok := e.buildGoroutineList(sp, pc)
	if !ok {
		// Degraded snapshot: still report where we're stopped, but don't touch
		// the remembered live set — an empty read (e.g. at the entry stop before
		// runtime init) must not look like every goroutine exited.
		return protocol.GoroutineSnapshotPayload{
			Goroutines: []protocol.Goroutine{e.syntheticGoroutine(pc)},
		}
	}

	var current int
	live := make(map[int]struct{}, len(gs))
	for _, g := range gs {
		live[g.ID] = struct{}{}
		if g.Current {
			current = g.ID
		}
	}

	created, exited := e.diffGoids(live)

	return protocol.GoroutineSnapshotPayload{
		Goroutines: gs,
		Threads:    e.readThreads(current, pc),
		Current:    current,
		Created:    created,
		Exited:     exited,
	}
}

// diffGoids compares the current live goid set against the previous snapshot's
// and returns the created (new) and exited (gone) goids, then adopts the new
// set. Returns nil slices on the first snapshot so a fresh session doesn't
// report every existing goroutine as "created".
func (e *engine) diffGoids(live map[int]struct{}) (created, exited []int) {
	if e.prevGoids != nil {
		for id := range live {
			if _, seen := e.prevGoids[id]; !seen {
				created = append(created, id)
			}
		}
		for id := range e.prevGoids {
			if _, still := live[id]; !still {
				exited = append(exited, id)
			}
		}
	}
	e.prevGoids = live
	sort.Ints(created)
	sort.Ints(exited)
	return created, exited
}

// currentGoroutineFrom returns the goroutine marked Current in a snapshot, used
// as the single Goroutine embedded in a stop event. Falls back to the first
// entry (snapshots always carry at least the synthetic goroutine).
func currentGoroutineFrom(snap protocol.GoroutineSnapshotPayload) protocol.Goroutine {
	for _, g := range snap.Goroutines {
		if g.Current {
			return g
		}
	}
	if len(snap.Goroutines) > 0 {
		return snap.Goroutines[0]
	}
	return protocol.Goroutine{}
}
