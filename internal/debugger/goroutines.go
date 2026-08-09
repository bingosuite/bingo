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
//     degrading: an incomplete walk falls back to a synthetic unknown
//     goroutine rather than being mistaken for a lifecycle change.
//   - Every read SUCCEEDS but the values are mutually inconsistent, because
//     they were taken from different generations of a table that was
//     republished underneath us. Degradation cannot see this — nothing failed —
//     so it is prevented instead, by the read ordering in allgsMetadata.

const (
	// maxGoroutineScan bounds both the rich runtime.allgs prefix and the cheap
	// fallback anchor scan so corrupt metadata can't make a snapshot unbounded.
	maxGoroutineScan = 8192
	// maxThreadScan bounds the runtime.allm linked-list walk likewise.
	maxThreadScan = 2048
	// maxCurrentThreadScan bounds the targeted continuation past the rich allm
	// prefix. It reads only enough fields to find the stopped M or current g.
	maxCurrentThreadScan = 2048
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

	mProcid    int64
	mG0        int64
	mG0OK      bool
	mGsignal   int64
	mGsignalOK bool
	mCurg      int64
	mID        int64
	mAlllink   int64
	mSpinning  int64

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
	l.mG0, l.mG0OK = dw.structMemberOffset("runtime.m", "g0")
	l.mGsignal, l.mGsignalOK = dw.structMemberOffset("runtime.m", "gsignal")
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
	AnchorID int
}

type threadWalkResult struct {
	Items    []protocol.Thread
	Complete bool
	Clipped  bool
}

type currentGoroutineResult struct {
	Item     protocol.Goroutine
	Found    bool
	Complete bool
}

type goroutineHeader struct {
	goid   int64
	status uint32
}

// readGoroutine reads one runtime.g at gptr. Include is false for intentional
// freelist/dead entries; Complete is false only when membership or current-
// identity data is unreadable. liveSP/livePC identify the currently-stopped
// thread so the goroutine running there is marked Current and gets its live PC.
func (e *engine) readGoroutine(l *goLayout, gptr, liveSP, livePC uint64) goroutineReadResult {
	header, ok := e.readGoroutineHeader(l, gptr)
	if !ok {
		return goroutineReadResult{}
	}
	if !header.included() {
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
		ID:     int(header.goid),
		Status: goStatusString(header.status),
	}
	if parent, ok := e.readI64(gptr + uint64(l.gParentGoid)); ok && parent > 0 {
		g.ParentID = int(parent)
	}
	if startpc, ok := e.readU64(gptr + uint64(l.gStartpc)); ok {
		g.StartLoc = e.locForPC(startpc)
	}
	if gopc, ok := e.readU64(gptr + uint64(l.gGopc)); ok {
		// gopc is captured by the runtime with sys.GetCallerPC(), so it is the
		// return address of the `go` statement's call, not the statement itself.
		g.CreatedLoc = e.locForPC(returnLookupPC(gopc))
	}
	if header.status == 4 { // waiting
		if wr, ok := e.readU8(gptr + uint64(l.gWaitreason)); ok {
			g.WaitReason = e.waitReasonString(l, wr)
		}
	}

	current := stackContainsSP(lo, hi, liveSP)
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

func (e *engine) readGoroutineHeader(l *goLayout, gptr uint64) (goroutineHeader, bool) {
	rawStatus, ok := e.readU32(gptr + uint64(l.gAtomicstatus))
	if !ok {
		return goroutineHeader{}, false
	}
	goid, ok := e.readI64(gptr + uint64(l.gGoid))
	if !ok {
		return goroutineHeader{}, false
	}
	return goroutineHeader{
		goid:   goid,
		status: rawStatus &^ gScanBit,
	}, true
}

func (h goroutineHeader) included() bool {
	return h.goid > 0 && h.status != gStatusDead
}

func (e *engine) readGoroutineStackBounds(l *goLayout, gptr uint64) (lo, hi uint64, ok bool) {
	lo, okLo := e.readU64(gptr + uint64(l.gStack) + uint64(l.stackLo))
	hi, okHi := e.readU64(gptr + uint64(l.gStack) + uint64(l.stackHi))
	return lo, hi, okLo && okHi
}

func stackContainsSP(lo, hi, sp uint64) bool {
	return sp != 0 && sp >= lo && sp < hi
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

func allgsEntryAddress(ptr, index uint64) (uint64, bool) {
	const pointerSize = uint64(8)
	if index > (^uint64(0)-ptr)/pointerSize {
		return 0, false
	}
	return ptr + index*pointerSize, true
}

// buildGoroutineList enumerates runtime.allgs. Complete is false when the
// runtime can't be read (no DWARF, bad layout, unreadable membership or
// required rooted current-identity data, or no live goroutines yet). An invalid
// architecture register hint is only a miss. Clipped is independent: the rich
// walk reached its safety ceiling but every visited entry was readable.
func (e *engine) buildGoroutineList(
	liveSP, livePC, currentGptr uint64,
	stopTID int,
) goroutineWalkResult {
	l, ok := e.getGoLayout()
	if !ok {
		return goroutineWalkResult{}
	}
	ptr, length, ok := e.allgsMetadata()
	if !ok || ptr == 0 || length == 0 {
		return goroutineWalkResult{}
	}
	return e.buildGoroutineListFrom(l, ptr, length, liveSP, livePC, currentGptr, stopTID)
}

func (e *engine) buildGoroutineListFrom(
	l *goLayout,
	ptr, length, liveSP, livePC, currentGptr uint64,
	stopTID int,
) goroutineWalkResult {
	richLength := min(length, uint64(maxGoroutineScan))
	rich, currentFound := e.readRichGoroutines(l, ptr, richLength, liveSP, livePC)
	if !rich.Complete || len(rich.Items) == 0 {
		return goroutineWalkResult{}
	}

	current := currentGoroutineResult{Complete: true}
	if !currentFound {
		current = e.resolveCurrentGoroutineAnchor(
			l, ptr, richLength, length, liveSP, livePC, currentGptr, stopTID,
		)
		if !current.Complete {
			return goroutineWalkResult{}
		}
	}

	anchorID := 0
	if current.Found {
		rich.Items, anchorID = installCurrentGoroutine(rich.Items, current.Item)
	}
	return goroutineWalkResult{
		Items:    rich.Items,
		Complete: true,
		Clipped:  length > richLength,
		AnchorID: anchorID,
	}
}

func installCurrentGoroutine(
	gs []protocol.Goroutine,
	current protocol.Goroutine,
) ([]protocol.Goroutine, int) {
	for i := range gs {
		if gs[i].ID == current.ID {
			gs[i] = current
			return gs, 0
		}
	}
	gs = append(gs, protocol.Goroutine{})
	copy(gs[1:], gs)
	gs[0] = current
	return gs, current.ID
}

func (e *engine) readRichGoroutines(
	l *goLayout,
	ptr, length, liveSP, livePC uint64,
) (goroutineWalkResult, bool) {
	out := make([]protocol.Goroutine, 0, int(length)+1)
	currentFound := false
	for i := uint64(0); i < length; i++ {
		entryAddr, ok := allgsEntryAddress(ptr, i)
		if !ok {
			return goroutineWalkResult{}, false
		}
		gptr, ok := e.readU64(entryAddr)
		if !ok {
			return goroutineWalkResult{}, false
		}
		if gptr == 0 {
			continue
		}
		result := e.readGoroutine(l, gptr, liveSP, livePC)
		if !result.Complete {
			return goroutineWalkResult{}, false
		}
		if result.Include {
			out = append(out, result.Item)
			currentFound = currentFound || result.Item.Current
		}
	}
	return goroutineWalkResult{Items: out, Complete: true}, currentFound
}

func (e *engine) resolveCurrentGoroutineAnchor(
	l *goLayout,
	ptr, richLength, length, liveSP, livePC, currentGptr uint64,
	stopTID int,
) currentGoroutineResult {
	current := e.resolveTargetedCurrentGoroutine(
		l, liveSP, livePC, currentGptr, stopTID,
	)
	if !current.Complete || current.Found {
		return current
	}
	if liveSP == 0 {
		return currentGoroutineResult{Complete: true}
	}
	if richLength >= length {
		return currentGoroutineResult{Complete: true}
	}
	fallbackEnd := min(length, richLength+uint64(maxGoroutineScan))
	return e.findCurrentGoroutine(l, ptr, richLength, fallbackEnd, liveSP, livePC)
}

func (e *engine) resolveTargetedCurrentGoroutine(
	l *goLayout,
	liveSP, livePC, currentGptr uint64,
	stopTID int,
) currentGoroutineResult {
	if currentGptr != 0 {
		current := e.currentGoroutineFromRegister(l, currentGptr, liveSP, livePC)
		if current.Found {
			return current
		}
	}
	if procid, ok := e.archRuntimeMProcID(stopTID); ok {
		current := e.findCurrentGoroutineByProcID(l, procid, livePC)
		if !current.Complete || current.Found {
			return current
		}
	}
	return currentGoroutineResult{Complete: true}
}

func (e *engine) currentGoroutineFromRegister(
	l *goLayout,
	gptr, liveSP, livePC uint64,
) currentGoroutineResult {
	// X28 is an ABI hint, not a runtime root: cgo, pre-runtime code, or a
	// non-Go thread can leave a nonzero value that is unreadable or stale.
	// Rejecting that hint must not discard an otherwise complete rich walk.
	header, ok := e.readGoroutineHeader(l, gptr)
	if !ok {
		return currentGoroutineResult{Complete: true}
	}
	if header.goid > 0 {
		result := e.readGoroutine(l, gptr, liveSP, livePC)
		if !result.Complete {
			return currentGoroutineResult{Complete: true}
		}
		if result.Include && result.Item.Current {
			return currentGoroutineResult{
				Item:     result.Item,
				Found:    true,
				Complete: true,
			}
		}
		return currentGoroutineResult{Complete: true}
	}
	if header.goid != 0 {
		return currentGoroutineResult{Complete: true}
	}
	mptr, ok := e.readU64(gptr + uint64(l.gM))
	if !ok {
		return currentGoroutineResult{Complete: true}
	}
	if mptr == 0 {
		return currentGoroutineResult{Complete: true}
	}
	schedulerG, complete := e.isSchedulerG(l, mptr, gptr)
	if !complete {
		return currentGoroutineResult{Complete: true}
	}
	if !schedulerG {
		return currentGoroutineResult{Complete: true}
	}
	current := e.readCurrentGoroutineFromM(l, mptr, livePC)
	if !current.Complete {
		return currentGoroutineResult{Complete: true}
	}
	return current
}

func (e *engine) isSchedulerG(l *goLayout, mptr, gptr uint64) (bool, bool) {
	if l.mG0OK {
		g0, ok := e.readU64(mptr + uint64(l.mG0))
		if !ok {
			return false, false
		}
		if g0 == gptr {
			return true, true
		}
	}
	if l.mGsignalOK {
		gsignal, ok := e.readU64(mptr + uint64(l.mGsignal))
		if !ok {
			return false, false
		}
		if gsignal == gptr {
			return true, true
		}
	}
	return false, true
}

func (e *engine) readCurrentGoroutineFromM(
	l *goLayout,
	mptr, livePC uint64,
) currentGoroutineResult {
	gptr, ok := e.readU64(mptr + uint64(l.mCurg))
	if !ok {
		return currentGoroutineResult{}
	}
	if gptr == 0 {
		return currentGoroutineResult{Complete: true}
	}
	owner, ok := e.readU64(gptr + uint64(l.gM))
	if !ok {
		return currentGoroutineResult{}
	}
	if owner != mptr {
		return currentGoroutineResult{Complete: true}
	}
	result := e.readGoroutine(l, gptr, 0, 0)
	if !result.Complete {
		return currentGoroutineResult{}
	}
	if !result.Include {
		return currentGoroutineResult{Complete: true}
	}
	current := result.Item
	current.Current = true
	current.CurrentLoc = e.locForPC(livePC)
	return currentGoroutineResult{
		Item:     current,
		Found:    true,
		Complete: true,
	}
}

// findCurrentGoroutine is the bounded fallback when the architecture-specific
// current-g pointer is unavailable. It reads only each pointer and stack bounds
// until SP containment identifies the anchor, then pays the full decode cost
// once. An unreadable pointer ends the pass because allgs is contiguous.
func (e *engine) findCurrentGoroutine(
	l *goLayout,
	ptr, start, length, liveSP, livePC uint64,
) currentGoroutineResult {
	for i := start; i < length; i++ {
		entryAddr, ok := allgsEntryAddress(ptr, i)
		if !ok {
			return currentGoroutineResult{}
		}
		gptr, ok := e.readU64(entryAddr)
		if !ok {
			return currentGoroutineResult{}
		}
		if gptr == 0 {
			continue
		}
		lo, hi, ok := e.readGoroutineStackBounds(l, gptr)
		if !ok {
			return currentGoroutineResult{}
		}
		if !stackContainsSP(lo, hi, liveSP) {
			continue
		}
		result := e.readGoroutine(l, gptr, liveSP, livePC)
		if !result.Complete {
			return currentGoroutineResult{}
		}
		if result.Include && result.Item.Current {
			return currentGoroutineResult{
				Item:     result.Item,
				Found:    true,
				Complete: true,
			}
		}
		return currentGoroutineResult{Complete: true}
	}
	return currentGoroutineResult{Complete: true}
}

// findCurrentGoroutineByProcID resolves the stopped Linux M exactly, then uses
// m.curg even when the stop occurred on g0 and SP is outside the user g's stack.
// The second window is targeted-only so the rich thread list remains capped.
func (e *engine) findCurrentGoroutineByProcID(
	l *goLayout,
	procid, livePC uint64,
) currentGoroutineResult {
	if e.dw == nil {
		return currentGoroutineResult{Complete: true}
	}
	base, ok := e.dw.runtimeVarAddr("runtime.allm")
	if !ok {
		return currentGoroutineResult{Complete: true}
	}
	mptr, ok := e.readU64(base)
	if !ok {
		return currentGoroutineResult{}
	}
	if mptr == 0 {
		return currentGoroutineResult{Complete: true}
	}
	return e.findCurrentGoroutineByProcIDFrom(l, mptr, procid, livePC)
}

func (e *engine) findCurrentGoroutineByProcIDFrom(
	l *goLayout,
	mptr, procid, livePC uint64,
) currentGoroutineResult {
	match, next, complete := e.findMByProcID(l, mptr, procid, maxThreadScan)
	if !complete {
		return currentGoroutineResult{}
	}
	if match == 0 && next != 0 {
		match, _, complete = e.findMByProcID(l, next, procid, maxCurrentThreadScan)
		if !complete {
			return currentGoroutineResult{}
		}
	}
	if match == 0 {
		return currentGoroutineResult{Complete: true}
	}
	return e.readCurrentGoroutineFromM(l, match, livePC)
}

func (e *engine) findMByProcID(
	l *goLayout,
	mptr, procid uint64,
	limit int,
) (match, next uint64, complete bool) {
	for i := 0; i < limit && mptr != 0; i++ {
		candidate, ok := e.readU64(mptr + uint64(l.mProcid))
		if !ok {
			return 0, 0, false
		}
		if candidate == procid {
			return mptr, 0, true
		}
		next, ok := e.readU64(mptr + uint64(l.mAlllink))
		if !ok {
			return 0, 0, false
		}
		mptr = next
	}
	return 0, mptr, true
}

func (e *engine) allmHead() (uint64, bool) {
	if e.dw == nil {
		return 0, false
	}
	base, ok := e.dw.runtimeVarAddr("runtime.allm")
	if !ok {
		return 0, false
	}
	mptr, ok := e.readU64(base)
	return mptr, ok && mptr != 0
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
	mptr, ok := e.allmHead()
	if !ok {
		return threadWalkResult{}
	}
	return e.readThreadsFrom(l, mptr, currentGoid, livePC)
}

func (e *engine) readThreadsFrom(
	l *goLayout,
	mptr uint64,
	currentGoid int,
	livePC uint64,
) threadWalkResult {
	out := make([]protocol.Thread, 0, 16)
	currentFound := false
	i := 0
	for ; i < maxThreadScan && mptr != 0; i++ {
		t := e.readThread(l, mptr, currentGoid, livePC)
		currentFound = currentFound || t.Current
		out = append(out, t)

		next, ok := e.readU64(mptr + uint64(l.mAlllink))
		if !ok {
			return threadWalkResult{}
		}
		mptr = next
	}
	clipped := i == maxThreadScan && mptr != 0
	if currentGoid > 0 && !currentFound && mptr != 0 {
		currentM, complete := e.findMByCurrentGoid(l, mptr, currentGoid, maxCurrentThreadScan)
		if !complete {
			return threadWalkResult{}
		}
		if currentM != 0 {
			current := e.readThread(l, currentM, currentGoid, livePC)
			if !current.Current {
				return threadWalkResult{}
			}
			out = append(out, current)
		}
	}
	return threadWalkResult{
		Items:    out,
		Complete: true,
		Clipped:  clipped,
	}
}

func (e *engine) readThread(
	l *goLayout,
	mptr uint64,
	currentGoid int,
	livePC uint64,
) protocol.Thread {
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
	return t
}

func (e *engine) findMByCurrentGoid(
	l *goLayout,
	mptr uint64,
	currentGoid, limit int,
) (uint64, bool) {
	for i := 0; i < limit && mptr != 0; i++ {
		curg, ok := e.readU64(mptr + uint64(l.mCurg))
		if !ok {
			return 0, false
		}
		if curg != 0 {
			goid, ok := e.readI64(curg + uint64(l.gGoid))
			if !ok {
				return 0, false
			}
			if int(goid) == currentGoid {
				return mptr, true
			}
		}
		next, ok := e.readU64(mptr + uint64(l.mAlllink))
		if !ok {
			return 0, false
		}
		mptr = next
	}
	return 0, true
}

// liveRegisters reads the currently-stopped thread's TID, SP, PC, and Go ABI
// current-g pointer. The pointer is only a candidate: readGoroutine validates it
// against SP containment or resolves its M's curg before it becomes the anchor.
// TID survives a register-read failure so Linux can still resolve m.curg.
func (e *engine) liveRegisters() (tid int, sp, pc, currentGptr uint64) {
	tid, err := e.activeTID()
	if err != nil {
		return 0, 0, 0, 0
	}
	regs, err := e.backend.GetRegisters(tid)
	if err != nil {
		return tid, 0, 0, 0
	}
	currentGptr, _ = e.archCurrentGoroutinePointer(regs)
	return tid, regs.SP, regs.PC, currentGptr
}

func unknownGoroutine(loc protocol.Location) protocol.Goroutine {
	return protocol.Goroutine{
		Status:     "unknown",
		CurrentLoc: loc,
		Current:    true,
	}
}

// syntheticGoroutine represents an unresolved stopped goroutine without
// borrowing a real goid. DAP may assign its own transport-only thread handle.
func (e *engine) syntheticGoroutine(livePC uint64) protocol.Goroutine {
	return unknownGoroutine(e.locForPC(livePC))
}

// degradedSnapshot is the single construction point for an incomplete runtime
// walk. Downstream reporting must describe every such snapshot as degraded,
// regardless of whether goroutine or thread traversal failed.
//
// It is packed like any other snapshot even though a single goroutine cannot
// approach the byte budget: the contract also caps per-element strings, and
// this one's location comes from DWARF like any other. Every path that emits a
// goroutine event goes through the packer, so none can produce something a
// conforming consumer must reject.
//
// Both counts are marked lower bounds. Absent totals mean "complete and exact",
// which this emphatically is not: the stand-in is what we show when the runtime
// could NOT be read, so claiming the runtime holds exactly one goroutine and no
// threads would be the same class of overclaim the totals exist to prevent.
// "At least one" is true whether the runtime is merely not up yet or a scan
// failed partway.
func (e *engine) degradedSnapshot(livePC uint64) (protocol.GoroutineSnapshotPayload, protocol.Goroutine) {
	stand := e.syntheticGoroutine(livePC)
	packed, _ := packForWire(protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{stand},
	}, true, true)
	return packed, stand
}

// readGoroutines returns the live goroutine set, falling back to a single
// synthetic goroutine when the runtime can't be read. This backs the on-demand
// Goroutines() query and the DAP threads list.
//
// The list is packed to the wire contract before it leaves: an unbounded list is
// the bug in issue #194, and a consumer that enforces the contract would have to
// reject it. Packing preserves the current goroutine and reports the original
// count, so a truncated list is still honest about its scale.
func (e *engine) readGoroutines() (protocol.GoroutinesPayload, error) {
	tid, sp, pc, currentGptr := e.liveRegisters()
	result := e.buildGoroutineList(sp, pc, currentGptr, tid)
	gs, clipped := result.Items, result.Clipped
	if !result.Complete {
		// Lower bound, not an exact count: the stand-in stands for a runtime we
		// could not read. See goroutineSnapshot's degraded branch.
		gs, clipped = []protocol.Goroutine{e.syntheticGoroutine(pc)}, true
	}
	packed, report := protocol.PackGoroutines(gs, clipped)
	if report.Oversized {
		e.log.Warn("goroutine list exceeds the wire contract",
			"bytes", report.Bytes, "goroutines", report.Totals.Goroutines)
	}
	return packed, nil
}

// packForWire bounds a snapshot for delivery. It is deliberately the ONLY thing
// packing decides: what may be delivered. The stop's identity is chosen from the
// pre-pack scan by the caller and never read back out of the result, because
// packing may legitimately degrade to empty collections when the required
// anchors alone cannot satisfy the byte, count or string contract — at which
// point the packed payload names no goroutine at all.
func packForWire(in protocol.GoroutineSnapshotPayload, goroutinesClipped, threadsClipped bool) (protocol.GoroutineSnapshotPayload, protocol.GoroutinePackReport) {
	return protocol.PackSnapshot(in, goroutinesClipped, threadsClipped)
}

// goroutineSnapshot builds the automatic stop snapshot and adopts its complete
// live set as the next lifecycle baseline.
func (e *engine) goroutineSnapshot() protocol.GoroutineSnapshotPayload {
	packed, _ := e.buildSnapshot(true)
	return packed
}

// goroutineSnapshotQuery is a pure observation: it emits no lifecycle deltas and
// leaves prevGoids untouched, so a refresh cannot consume the next stop's delta.
func (e *engine) goroutineSnapshotQuery() protocol.GoroutineSnapshotPayload {
	packed, _ := e.buildSnapshot(false)
	return packed
}

// snapshotWithCurrent is the automatic-stop path for callers that also need the
// stopped goroutine's identity independently of the delivery bound.
func (e *engine) snapshotWithCurrent() (protocol.GoroutineSnapshotPayload, protocol.Goroutine) {
	return e.buildSnapshot(true)
}

// buildSnapshot reads both runtime collections completely before it can adopt a
// lifecycle baseline, then returns the packed payload alongside the stop's
// identity from the full pre-pack scan.
//
// The payload and the identity are deliberately separate. Packing bounds what
// may be DELIVERED, and can legitimately degrade to empty collections when the
// required anchors alone cannot fit the byte, count or string contract. Reading
// the current goroutine back out of the packed payload would then lose the
// stop's identity entirely — the breakpoint or pause event would name no
// goroutine, and a DAP client would fall back to a synthetic thread while the
// debugger knew exactly where it was stopped. A producer-side delivery cap must
// never erase what the stop is.
func (e *engine) buildSnapshot(trackLifecycle bool) (protocol.GoroutineSnapshotPayload, protocol.Goroutine) {
	tid, sp, pc, currentGptr := e.liveRegisters()

	goroutines := e.buildGoroutineList(sp, pc, currentGptr, tid)
	if !goroutines.Complete {
		// Degraded snapshot: still report where we're stopped, but don't touch
		// the remembered live set — an empty read (e.g. at the entry stop before
		// runtime init) must not look like every goroutine exited.
		return e.degradedSnapshot(pc)
	}

	_, current := snapshotGoroutineIDs(goroutines.Items, goroutines.AnchorID)
	threads := e.readThreads(current, pc)
	if !threads.Complete {
		// An incomplete thread walk degrades the whole snapshot, so the deltas
		// must not run either: diffing against a runtime we only partially read
		// would adopt a truncated live set as the next baseline.
		return e.degradedSnapshot(pc)
	}
	if trackLifecycle {
		return e.finishSnapshot(goroutines, threads, pc)
	}
	return e.finishSnapshotWithLifecycle(goroutines, threads, pc, false)
}

// finishSnapshot completes a SUCCESSFUL scan: it resolves the stop's identity,
// computes the lifecycle deltas, and bounds the result for delivery.
//
// The identity is resolved from the walk — the full scan — and returned
// untouched. It is deliberately never read back out of the packed payload:
// packing decides only what may be DELIVERED and can degrade to empty
// collections when the required anchors cannot satisfy the byte, count or
// string contract, at which point the payload names no goroutine and the stop
// would lose the very thing it exists to report.
func (e *engine) finishSnapshot(
	goroutines goroutineWalkResult,
	threads threadWalkResult,
	pc uint64,
) (protocol.GoroutineSnapshotPayload, protocol.Goroutine) {
	return e.finishSnapshotWithLifecycle(goroutines, threads, pc, true)
}

// finishSnapshotWithLifecycle is the single automatic/query split. Only the
// automatic path may advance prevGoids; both paths pack the same complete scan.
func (e *engine) finishSnapshotWithLifecycle(
	goroutines goroutineWalkResult,
	threads threadWalkResult,
	pc uint64,
	trackLifecycle bool,
) (protocol.GoroutineSnapshotPayload, protocol.Goroutine) {
	live, current := snapshotGoroutineIDs(goroutines.Items, goroutines.AnchorID)
	currentG := currentGoroutineOf(goroutines.Items)
	if !currentG.Current {
		// The scan attributed no current goroutine, but the stop still has to
		// name something.
		currentG = e.syntheticGoroutine(pc)
	}

	snap := protocol.GoroutineSnapshotPayload{
		Goroutines: goroutines.Items,
		Threads:    threads.Items,
		Current:    current,
	}
	if trackLifecycle {
		snap.Created, snap.Exited = e.diffGoids(live)
	}
	packed, report := packForWire(snap, goroutines.Clipped, threads.Clipped)
	if report.Degraded || report.Oversized {
		e.log.Warn("goroutine snapshot could not be packed within the wire contract",
			"bytes", report.Bytes, "degraded", report.Degraded, "oversized", report.Oversized,
			"goroutines", report.Totals.Goroutines, "threads", report.Totals.Threads)
	}
	return packed, currentG
}

// snapshotFrom is the complete-scan seam used by lifecycle ownership tests.
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
	packed, _ := packForWire(snap, false, false)
	return packed
}

// currentGoroutineOf returns the scanned goroutine the stop belongs to. It reads
// from the pre-pack scan for the reason snapshotWithCurrent documents.
func currentGoroutineOf(gs []protocol.Goroutine) protocol.Goroutine {
	for _, g := range gs {
		if g.Current {
			return g
		}
	}
	return protocol.Goroutine{}
}

// snapshotGoroutineIDs keeps lifecycle deltas tied to the stable rich prefix.
// A beyond-cap current anchor is present for stop identity but excluded from the
// baseline; otherwise switching between live tail goroutines would fabricate
// created/exited deltas for goroutines that merely fell outside the next scan.
func snapshotGoroutineIDs(
	gs []protocol.Goroutine,
	anchorID int,
) (live map[int]struct{}, current int) {
	live = make(map[int]struct{}, len(gs))
	for _, g := range gs {
		if g.ID != anchorID {
			live[g.ID] = struct{}{}
		}
		if g.Current {
			current = g.ID
		}
	}
	return live, current
}

// diffGoids compares the current live goid set against the previous automatic
// and returns the created (new) and exited (gone) goids, then adopts the new
// set. Returns nil slices on the first snapshot so a fresh session doesn't
// report every existing goroutine as "created". Only reached from the
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
// as the single Goroutine embedded in a stop event. An unresolved snapshot must
// never substitute another real goroutine merely because it appears first.
func currentGoroutineFrom(
	snap protocol.GoroutineSnapshotPayload,
	fallbackLoc protocol.Location,
) protocol.Goroutine {
	for _, g := range snap.Goroutines {
		if g.Current {
			return currentGoroutineAt(g, fallbackLoc)
		}
	}
	return unknownGoroutine(fallbackLoc)
}

// currentGoroutineAt applies the stop's resolved location to the goroutine the
// stop is on. It takes the goroutine directly rather than a payload because the
// stop's identity must come from the PRE-PACK scan: packing bounds what may be
// delivered and can degrade to empty collections, and reading the identity back
// out of a packed payload would leave the stop naming no goroutine at all.
//
// An unresolved identity (goid 0) stays unresolved — substituting a real
// goroutine would misattribute the stop.
func currentGoroutineAt(g protocol.Goroutine, fallbackLoc protocol.Location) protocol.Goroutine {
	if !g.Current || g.ID == 0 {
		if fallbackLoc == (protocol.Location{}) {
			fallbackLoc = g.CurrentLoc
		}
		return unknownGoroutine(fallbackLoc)
	}
	if fallbackLoc != (protocol.Location{}) {
		g.CurrentLoc = fallbackLoc
	}
	return g
}
