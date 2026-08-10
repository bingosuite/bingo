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
// The engine serializes the walk while the reporting thread is suspended.
// Darwin also stops sibling threads, but Linux ptrace stops are per-thread, so
// runtime mutations can race the walk there. That race has two distinct shapes
// and they need two distinct defences:
//
//   - A required read FAILS or an entry is retired mid-walk. Handled by
//     degrading: an incomplete walk falls back to the legacy single-synthetic
//     goroutine rather than being mistaken for a lifecycle change.
//   - Every read SUCCEEDS but the values are mutually inconsistent, because
//     they were taken from different generations of a table that was
//     republished underneath us. Degradation cannot see this — nothing failed —
//     so it is prevented instead, by the read ordering in allgsMetadata.

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

type goroutineReadResult struct {
	Item     protocol.Goroutine
	Include  bool
	Complete bool
}

type goroutineWalkResult struct {
	Items    []protocol.Goroutine
	Complete bool
	Clipped  bool
}

type threadWalkResult struct {
	Items    []protocol.Thread
	Complete bool
	Clipped  bool
}

// readGoroutine reads one runtime.g at gptr. Include is false for intentional
// freelist/dead entries; Complete is false only when membership or current-
// identity data is unreadable. liveSP/livePC identify the currently-stopped
// thread so the goroutine running there is marked Current and gets its live PC.
func (e *engine) readGoroutine(l *goLayout, gptr, liveSP, livePC uint64) goroutineReadResult {
	rawStatus, ok := e.readU32(gptr + uint64(l.gAtomicstatus))
	if !ok {
		return goroutineReadResult{}
	}
	status := rawStatus &^ gScanBit

	goid, ok := e.readI64(gptr + uint64(l.gGoid))
	if !ok {
		return goroutineReadResult{}
	}
	if goid <= 0 || status == gStatusDead {
		// Freelist / dead slots carry goid 0 or a stale id; a UI wants only the
		// live set. Their departure is reported via the exited delta.
		return goroutineReadResult{Complete: true}
	}

	lo, ok := e.readU64(gptr + uint64(l.gStack) + uint64(l.stackLo))
	if !ok {
		return goroutineReadResult{}
	}
	hi, ok := e.readU64(gptr + uint64(l.gStack) + uint64(l.stackHi))
	if !ok {
		return goroutineReadResult{}
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

	current := liveSP != 0 && liveSP >= lo && liveSP < hi
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

	return goroutineReadResult{
		Item:     g,
		Include:  true,
		Complete: true,
	}
}

func (e *engine) readI64(addr uint64) (int64, bool) {
	v, ok := e.readU64(addr)
	return int64(v), ok
}

// allgsMetadata reads the goroutine table's array pointer and length as a
// coherent pair.
//
// The read ORDER is load-bearing, and it is the opposite of the obvious one.
// Only the trapping thread halts at a Linux ptrace stop, so sibling threads
// keep running while this walk reads tracee memory and runtime.allgadd can
// republish the table between our two reads. allgadd publishes the array
// pointer BEFORE the length, so reading the pointer first can pair a stale
// array with a length belonging to its larger successor and walk past the end
// of the old allocation. That overrun is not detectable downstream: the word
// after a heap object is mapped, so the read SUCCEEDS and the walk reports
// itself Complete while carrying a goroutine that never existed.
//
// Reading the length FIRST is what makes the pair safe, and it is safe only
// because runtime.allgs is append-only and never shrinks: a length observed
// before a pointer therefore always fits inside whichever array that pointer
// names. The worst case degrades to missing a goroutine created during the
// read, which the runtime documents as acceptable for lock-free readers.
//
// runtime.allglen/runtime.allgptr are the runtime's own atomics, published for
// exactly this purpose and carrying the documented contract ("allgptr is
// updated before allglen. Readers should read allglen before allgptr"), so they
// are preferred. The raw runtime.allgs header is a fallback for images without
// those symbols; it applies the same length-before-pointer ordering, but the
// slice header's three words are ordinary compiler-scheduled stores rather than
// a documented contract, so the fallback is best-effort where the mirror is
// guaranteed.
//
// ok is false whenever a required read fails, so callers degrade exactly as
// they do for any other unreadable runtime data.
func (e *engine) allgsMetadata() (ptr, length uint64, ok bool) {
	// One DWARF pass for all three names: resolving them separately costs a
	// full DIE traversal each, on the engine loop, at the first stop.
	addrs := e.dw.runtimeVarAddrs("runtime.allglen", "runtime.allgptr", "runtime.allgs")

	lenAddr, haveLen := addrs["runtime.allglen"]
	ptrAddr, havePtr := addrs["runtime.allgptr"]
	if haveLen && havePtr {
		mlen, lenOK := e.readU64(lenAddr)
		if !lenOK {
			return 0, 0, false
		}
		mptr, ptrOK := e.readU64(ptrAddr)
		if !ptrOK {
			return 0, 0, false
		}
		// A zero pair means the runtime has not published the table through
		// these atomics yet — they are written by the first allgadd, so a
		// pre-init stop sees nothing here. Fall through rather than reporting
		// an empty table, since the header is the older and more widely
		// populated source; when it is empty too the caller degrades exactly
		// as it always has.
		if mptr != 0 && mlen != 0 {
			return mptr, mlen, true
		}
	}

	base, haveBase := addrs["runtime.allgs"]
	if !haveBase {
		return 0, 0, false
	}
	if length, ok = e.readU64(base + 8); !ok { // slice header: len @ +8, FIRST
		return 0, 0, false
	}
	if ptr, ok = e.readU64(base); !ok { // slice header: array pointer @ +0
		return 0, 0, false
	}
	return ptr, length, true
}

// buildGoroutineList enumerates runtime.allgs. Complete is false when the
// runtime can't be read (no DWARF, bad layout, unreadable membership data, or
// no live goroutines yet). Clipped is independent: the walk reached its safety
// ceiling but every visited entry was readable.
func (e *engine) buildGoroutineList(liveSP, livePC uint64) goroutineWalkResult {
	l, ok := e.getGoLayout()
	if !ok {
		return goroutineWalkResult{}
	}
	ptr, length, ok := e.allgsMetadata()
	if !ok || ptr == 0 || length == 0 {
		return goroutineWalkResult{}
	}
	clipped := length > maxGoroutineScan
	if length > maxGoroutineScan {
		length = maxGoroutineScan
	}

	out := make([]protocol.Goroutine, 0, length)
	for i := uint64(0); i < length; i++ {
		gptr, ok := e.readU64(ptr + i*8)
		if !ok {
			return goroutineWalkResult{}
		}
		if gptr == 0 {
			continue
		}
		result := e.readGoroutine(l, gptr, liveSP, livePC)
		if !result.Complete {
			return goroutineWalkResult{}
		}
		if result.Include {
			out = append(out, result.Item)
		}
	}
	if len(out) == 0 {
		return goroutineWalkResult{}
	}
	return goroutineWalkResult{
		Items:    out,
		Complete: true,
		Clipped:  clipped,
	}
}

// readThreads walks runtime.allm and reports every OS thread (M). currentGoid
// (from the goroutine list) marks the thread whose goroutine is under
// inspection as Current and gives it the live PC. Complete is false only when
// the list itself can't be traversed; individual thread metadata is optional.
// Clipped records the independent allm safety ceiling without conflating it
// with an incomplete memory read.
func (e *engine) readThreads(currentGoid int, livePC uint64) threadWalkResult {
	l, ok := e.getGoLayout()
	if !ok {
		return threadWalkResult{}
	}
	base, ok := e.dw.runtimeVarAddr("runtime.allm")
	if !ok {
		return threadWalkResult{}
	}
	mptr, ok := e.readU64(base) // runtime.allm is a *m; read the head pointer
	if !ok {
		return threadWalkResult{}
	}

	var out []protocol.Thread
	i := 0
	for ; i < maxThreadScan && mptr != 0; i++ {
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
			return threadWalkResult{}
		}
		mptr = next
	}
	return threadWalkResult{
		Items:    out,
		Complete: true,
		Clipped:  i == maxThreadScan && mptr != 0,
	}
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

// degradedSnapshot is the single construction point for an incomplete runtime
// walk. Downstream reporting must describe every such snapshot as degraded,
// regardless of whether goroutine or thread traversal failed.
func (e *engine) degradedSnapshot(livePC uint64) protocol.GoroutineSnapshotPayload {
	return protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{e.syntheticGoroutine(livePC)},
	}
}

// readGoroutines returns the live goroutine set, falling back to a single
// synthetic goroutine when the runtime can't be read. This backs the on-demand
// Goroutines() query and the DAP threads list.
func (e *engine) readGoroutines() ([]protocol.Goroutine, error) {
	sp, pc := e.liveRegisters()
	result := e.buildGoroutineList(sp, pc)
	if result.Complete {
		return result.Items, nil
	}
	return []protocol.Goroutine{e.syntheticGoroutine(pc)}, nil
}

// goroutineSnapshot builds the automatic stop snapshot: the full concurrency
// picture plus the created/exited deltas against the previous automatic
// snapshot, adopting the new live set as the next baseline. Only the automatic
// stops (entry, breakpoint hit, pause) own that baseline. Runs on the engine
// loop thread; prevGoids needs no synchronization.
func (e *engine) goroutineSnapshot() protocol.GoroutineSnapshotPayload {
	return e.buildSnapshot(true)
}

// goroutineSnapshotQuery answers an on-demand CmdGoroutineSnapshot. It reports
// the same live picture but is a pure observation: Created/Exited stay empty and
// prevGoids is untouched, so a refresh between two stops cannot consume the
// deltas the next automatic snapshot must report (they are only remembered
// once, in prevGoids, and would otherwise be lost).
func (e *engine) goroutineSnapshotQuery() protocol.GoroutineSnapshotPayload {
	return e.buildSnapshot(false)
}

func (e *engine) buildSnapshot(trackLifecycle bool) protocol.GoroutineSnapshotPayload {
	sp, pc := e.liveRegisters()

	goroutines := e.buildGoroutineList(sp, pc)
	if !goroutines.Complete {
		// Degraded snapshot: still report where we're stopped, but don't touch
		// the remembered live set — an empty read (e.g. at the entry stop before
		// runtime init) must not look like every goroutine exited.
		return e.degradedSnapshot(pc)
	}

	current, live := snapshotGoroutineState(goroutines.Items)
	threads := e.readThreads(current, pc)
	if !threads.Complete {
		return e.degradedSnapshot(pc)
	}
	return e.snapshotFrom(goroutines.Items, threads.Items, current, live, trackLifecycle)
}

// snapshotFrom assembles a payload after both runtime walks have completed.
// trackLifecycle is the single point where an automatic snapshot's ownership of
// the lifecycle baseline differs from a query's read-only view.
func (e *engine) snapshotFrom(
	gs []protocol.Goroutine,
	threads []protocol.Thread,
	current int,
	live map[int]struct{},
	trackLifecycle bool,
) protocol.GoroutineSnapshotPayload {
	snap := protocol.GoroutineSnapshotPayload{
		Goroutines: gs,
		Threads:    threads,
		Current:    current,
	}
	if trackLifecycle {
		snap.Created, snap.Exited = e.diffGoids(live)
	}
	return snap
}

func snapshotGoroutineState(gs []protocol.Goroutine) (int, map[int]struct{}) {
	var current int
	live := make(map[int]struct{}, len(gs))
	for _, g := range gs {
		live[g.ID] = struct{}{}
		if g.Current {
			current = g.ID
		}
	}
	return current, live
}

// diffGoids compares the current live goid set against the previous automatic
// snapshot's and returns the created (new) and exited (gone) goids, then adopts
// the new set. Returns nil slices on the first snapshot so a fresh session
// doesn't report every existing goroutine as "created". Only reached from the
// automatic stop path — see goroutineSnapshotQuery.
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
// as the single Goroutine embedded in a stop event. Unknown is safer than
// attributing the stop to the first unrelated goroutine when a capped scan has
// not yet found the stopped goroutine.
func currentGoroutineFrom(snap protocol.GoroutineSnapshotPayload) protocol.Goroutine {
	for _, g := range snap.Goroutines {
		if g.Current {
			return g
		}
	}
	return protocol.Goroutine{}
}
