package protocol

import (
	"encoding/json"
	"math"
	"sort"
	"sync/atomic"
)

// GoroutinePackReport describes what a Pack call put on the wire. Producers use
// it to log or surface truncation; it is never serialized.
type GoroutinePackReport struct {
	// Totals are the original untrimmed counts plus the scan-clipped flag,
	// regardless of whether they were attached to the payload.
	Totals SnapshotTotals

	// Goroutines and Threads are the counts that survived onto the wire.
	Goroutines int
	Threads    int

	// Bytes is the exact marshalled Event size measured at the widest possible
	// sequence number (see packBudget).
	Bytes int

	// Degraded is true when not even the anchor elements fit, so the wire
	// carries empty collections. The lifecycle deltas are still intact.
	Degraded bool

	// Oversized is true when even the degraded result exceeds the budget, which
	// can only happen if the lifecycle deltas alone do — and those are never
	// trimmed. The producer's runtime scan bounds them well below that in
	// practice, so this is a corrupt-input signal rather than a normal outcome.
	Oversized bool
}

// Omitted reports whether the wire is missing elements the producer had.
func (r GoroutinePackReport) Omitted() bool {
	return r.Goroutines < r.Totals.Goroutines || r.Threads < r.Totals.Threads
}

// PackSnapshot bounds a GoroutineSnapshotPayload to MaxGoroutineEventBytes and
// the element-count caps, returning a wire-ready payload plus a report.
//
// scanClipped says the caller's runtime walk hit its own ceiling before this
// call, which makes the reported goroutine total a lower bound.
//
// Selection is deterministic so every client on one stop sees the same thing:
// the current goroutine, then its ancestors nearest-first, then the rest by
// ascending goid. The current goroutine, its whole ancestor chain, and the
// current thread are REQUIRED — a result that cannot keep them degrades to empty
// collections instead. Threads take an ordered floor before goroutines compete
// for the budget, then reclaim whatever is left.
func PackSnapshot(snap GoroutineSnapshotPayload, scanClipped bool) (GoroutineSnapshotPayload, GoroutinePackReport) {
	totals := SnapshotTotals{
		Goroutines: len(snap.Goroutines),
		Threads:    len(snap.Threads),
		Clipped:    scanClipped,
	}
	// shape is the single source of the payload: pack measures through it and
	// the caller assembles through it, so the measured bytes cannot drift from
	// the bytes actually returned.
	shape := func(gs []Goroutine, ts []Thread, totals *SnapshotTotals) GoroutineSnapshotPayload {
		return GoroutineSnapshotPayload{
			Goroutines: gs,
			Threads:    ts,
			Current:    snap.Current,
			Created:    snap.Created,
			Exited:     snap.Exited,
			Totals:     totals,
		}
	}
	build := func(gs []Goroutine, ts []Thread, totals *SnapshotTotals) any {
		return shape(gs, ts, totals)
	}

	// The snapshot shape IS a tree, so the whole spawn chain is required.
	gs, ts, report := pack(snap.Goroutines, snap.Threads, snap.Current, totals, EventGoroutineSnapshot, build, true)
	return shape(gs, ts, payloadTotals(totals, report)), report
}

// PackGoroutines bounds the EventGoroutines list. It shares PackSnapshot's exact
// accounting and ordering; only the payload schema differs. There are no threads
// in this shape, so the whole budget goes to goroutines.
func PackGoroutines(gs []Goroutine, scanClipped bool) (GoroutinesPayload, GoroutinePackReport) {
	totals := SnapshotTotals{Goroutines: len(gs), Clipped: scanClipped}
	shape := func(gs []Goroutine, totals *SnapshotTotals) GoroutinesPayload {
		return GoroutinesPayload{Goroutines: gs, Totals: totals}
	}
	build := func(gs []Goroutine, _ []Thread, totals *SnapshotTotals) any {
		return shape(gs, totals)
	}

	// This shape is a FLAT list — DAP renders it as threads, with no hierarchy to
	// break — so ancestors are ordered first but not required. Degrading it to
	// empty would make the DAP translator fabricate a synthetic "main" thread,
	// which is precisely the lie issue #194 exists to stop.
	packed, _, report := pack(gs, nil, 0, totals, EventGoroutines, build, false)
	return shape(packed, payloadTotals(totals, report)), report
}

// payloadTotals decides whether the payload carries Totals: only when the wire
// omits elements or the producer's scan was clipped. A complete, unclipped
// unclipped result stays byte-identical to the pre-1.4 shape.
func payloadTotals(totals SnapshotTotals, report GoroutinePackReport) *SnapshotTotals {
	if !report.Omitted() && !totals.Clipped {
		return nil
	}
	copied := totals
	return &copied
}

// payloadBuilder assembles one of the two goroutine payload shapes. Taking it as
// a parameter is what lets both packers share a single exact algorithm.
type payloadBuilder func(gs []Goroutine, ts []Thread, totals *SnapshotTotals) any

// pack is the shared algorithm. It marshals each candidate at most twice and
// makes at most two ordered passes, so cost is O(n) marshals (plus the ordering
// sort) rather than the O(n²) of re-marshalling the whole payload per candidate.
func pack(
	goroutines []Goroutine,
	threads []Thread,
	current int,
	totals SnapshotTotals,
	kind EventKind,
	build payloadBuilder,
	requireAncestors bool,
) ([]Goroutine, []Thread, GoroutinePackReport) {
	orderedG, anchorG := orderGoroutines(goroutines, current)
	orderedT, anchorT := orderThreads(threads)
	if !requireAncestors && anchorG > 1 {
		anchorG = 1 // ordering is kept; only the current goroutine is required
	}

	degraded := func() ([]Goroutine, []Thread, GoroutinePackReport) {
		gs, ts := []Goroutine{}, []Thread{}
		report := GoroutinePackReport{Totals: totals, Degraded: true}
		report.Bytes, _ = packBudget(kind, build(gs, ts, payloadTotals(totals, report)))
		// Emptying the collections is the last lever available: the lifecycle
		// deltas are never trimmed, so if they alone overflow there is nothing
		// left to give. Say so rather than pretending the result conforms.
		report.Oversized = report.Bytes > MaxGoroutineEventBytes
		return gs, ts, report
	}

	// Whether Totals ends up on the wire changes the reserve, and it is only
	// known after packing — except when the scan was clipped, which forces it.
	// So pack optimistically first; if that pass turns out to omit elements,
	// Totals appears and the reserve was too small, so repack against the true
	// one.
	//
	// Exactly two passes suffice: if pass 2 omitted NOTHING then every element
	// fit under its smaller budget, so every element would also have fit under
	// pass 1's larger one — contradicting pass 1 having omitted something. So
	// pass 2 always omits, Totals stays present, and its reserve was right.
	// (Note the weaker claim "pass 2 omits at least as many" is false: with
	// skip-and-continue a tighter budget can reject one big early element and
	// admit several small later ones.)
	gs, ts, report, ok := packOnce(orderedG, anchorG, orderedT, anchorT, totals, totals.Clipped, kind, build)
	if !ok {
		return degraded()
	}
	if !totals.Clipped && report.Omitted() {
		gs, ts, report, ok = packOnce(orderedG, anchorG, orderedT, anchorT, totals, true, kind, build)
		if !ok {
			return degraded()
		}
	}

	// The incremental accounting is exact (see packState), but the contract is
	// the measured size of the payload the caller actually sends, so that is
	// what is measured. A miss degrades rather than emitting a frame the
	// transport would reject outright.
	size, ok := packBudget(kind, build(gs, ts, payloadTotals(totals, report)))
	if !ok || size > MaxGoroutineEventBytes {
		return degraded()
	}
	report.Bytes = size
	return gs, ts, report
}

// packOnce runs one exact pass against a reserve that either includes Totals or
// does not. ok is false when the required anchors cannot be honoured.
func packOnce(
	orderedG []Goroutine,
	anchorG int,
	orderedT []Thread,
	anchorT int,
	totals SnapshotTotals,
	withTotals bool,
	kind EventKind,
	build payloadBuilder,
) ([]Goroutine, []Thread, GoroutinePackReport, bool) {
	// Deriving the reserve from a real marshal of the real skeleton — envelope,
	// current goid, and the lifecycle deltas that are never trimmed — keeps the
	// accounting tied to the schema instead of a hard-coded constant.
	var reserved *SnapshotTotals
	if withTotals {
		reserved = &totals
	}
	reserve, ok := packBudget(kind, build([]Goroutine{}, []Thread{}, reserved))
	if !ok || reserve > MaxGoroutineEventBytes {
		return nil, nil, GoroutinePackReport{}, false
	}

	// The reserve measured empty (`[]`) collections, so the result must present
	// them the same way throughout: a nil slice marshals as `null`, two bytes
	// the accounting never charged for.
	state := &packState{
		limit:      MaxGoroutineEventBytes,
		used:       reserve,
		goroutines: []Goroutine{},
		threads:    []Thread{},
	}

	// Anchors: the current goroutine, its whole spawn chain, and the thread the
	// debugger is stopped on. They are placed before anything else, and their
	// failure to fit is what defines a degraded result. The count caps bind
	// here too — a result cannot both honour the cap and keep every anchor.
	if anchorG > MaxSnapshotGoroutines || anchorT > MaxSnapshotThreads {
		return nil, nil, GoroutinePackReport{}, false
	}
	for i := 0; i < anchorG; i++ {
		if !state.addGoroutine(orderedG[i]) {
			return nil, nil, GoroutinePackReport{}, false
		}
	}
	for i := 0; i < anchorT; i++ {
		if !state.addThread(orderedT[i]) {
			return nil, nil, GoroutinePackReport{}, false
		}
	}

	// Thread floor, then goroutines, then leftover threads. Skipping an
	// oversized element and continuing (rather than stopping at the first
	// misfit) keeps one pathological element from hiding every element after it.
	next := anchorT
	for ; next < len(orderedT) && len(state.threads) < MinThreadsRetained; next++ {
		if len(state.threads) >= MaxSnapshotThreads {
			break
		}
		state.addThread(orderedT[next])
	}
	for i := anchorG; i < len(orderedG); i++ {
		if len(state.goroutines) >= MaxSnapshotGoroutines {
			break
		}
		state.addGoroutine(orderedG[i])
	}
	for ; next < len(orderedT); next++ {
		if len(state.threads) >= MaxSnapshotThreads {
			break
		}
		state.addThread(orderedT[next])
	}

	return state.goroutines, state.threads, GoroutinePackReport{
		Totals:     totals,
		Goroutines: len(state.goroutines),
		Threads:    len(state.threads),
	}, true
}

// packState threads the one shared byte budget through both element phases.
//
// The accounting is exact because JSON arrays are additive: an element's
// standalone marshal is byte-identical to its appearance inside the array
// (escaping is per-value and context-free, and encoding/json emits a
// json.RawMessage payload verbatim), so an array of k elements costs the sum of
// their marshals plus k-1 separators on top of the empty `[]` already counted in
// the reserve.
type packState struct {
	limit      int
	used       int
	goroutines []Goroutine
	threads    []Thread
}

func (p *packState) addGoroutine(g Goroutine) bool {
	size, ok := elementBytes(g)
	if !ok || !p.charge(size, len(p.goroutines)) {
		return false
	}
	p.goroutines = append(p.goroutines, g)
	return true
}

func (p *packState) addThread(t Thread) bool {
	size, ok := elementBytes(t)
	if !ok || !p.charge(size, len(p.threads)) {
		return false
	}
	p.threads = append(p.threads, t)
	return true
}

func (p *packState) charge(size, placed int) bool {
	cost := size
	if placed > 0 {
		cost++ // the comma separating this element from the previous one
	}
	if p.used+cost > p.limit {
		return false
	}
	p.used += cost
	return true
}

func elementBytes(v any) (int, bool) {
	packElementMarshals.Add(1)
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}
	return len(raw), true
}

// packElementMarshals and packEnvelopeMarshals make the cost model observable so
// a test can prove the algorithm stays O(n): one marshal per candidate element
// and a constant number of whole-payload measurements, never a re-marshal of the
// whole payload per candidate. The atomic adds are noise next to a JSON marshal.
var (
	packElementMarshals  atomic.Int64
	packEnvelopeMarshals atomic.Int64
)

// packBudget measures the exact marshalled Event at the widest possible sequence
// number. The hub re-stamps seq at broadcast time with a value the producer
// cannot know, so accounting against anything narrower would under-count by up
// to the width of a uint64.
func packBudget(kind EventKind, payload any) (int, bool) {
	packEnvelopeMarshals.Add(1)
	evt, err := NewEvent(kind, math.MaxUint64, payload)
	if err != nil {
		return 0, false
	}
	raw, err := MarshalEvent(evt)
	if err != nil {
		return 0, false
	}
	return len(raw), true
}

// orderGoroutines returns the deterministic packing order — the current
// goroutine, its ancestors nearest-first, then the rest by ascending goid — and
// how many leading entries are REQUIRED anchors.
//
// The whole spawn chain above the current goroutine is an anchor, not merely a
// preference: a tree missing an interior ancestor cannot be rendered as the
// hierarchy it actually is, so a result that drops one would be misleading in a
// way a truncated tail is not.
func orderGoroutines(gs []Goroutine, current int) ([]Goroutine, int) {
	if len(gs) == 0 {
		return nil, 0
	}
	first := make(map[int]int, len(gs))
	for i, g := range gs {
		if _, dup := first[g.ID]; !dup {
			first[g.ID] = i
		}
	}

	placed := make([]bool, len(gs))
	ordered := make([]Goroutine, 0, len(gs))
	anchors := 0
	if goid := currentGoid(gs, current); goid != 0 {
		if ci, ok := first[goid]; ok {
			ordered = append(ordered, gs[ci])
			placed[ci] = true
			anchors = 1
			for parent := gs[ci].ParentID; parent != 0; {
				pi, ok := first[parent]
				if !ok || placed[pi] {
					break // unknown or already placed: a cycle would loop forever
				}
				ordered = append(ordered, gs[pi])
				placed[pi] = true
				anchors++
				parent = gs[pi].ParentID
			}
		}
	}

	rest := make([]Goroutine, 0, len(gs)-len(ordered))
	for i, g := range gs {
		if !placed[i] {
			rest = append(rest, g)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].ID < rest[j].ID })
	return append(ordered, rest...), anchors
}

// currentGoid resolves which goroutine the debugger is stopped in, preferring
// the payload's explicit goid and falling back to the per-element flag (the
// EventGoroutines shape carries no separate current field).
func currentGoid(gs []Goroutine, current int) int {
	if current != 0 {
		return current
	}
	for _, g := range gs {
		if g.Current && g.ID != 0 {
			return g.ID
		}
	}
	return 0
}

// orderThreads puts the current thread first and the rest in ascending MID, and
// reports whether the leading entry is an anchor.
func orderThreads(ts []Thread) ([]Thread, int) {
	if len(ts) == 0 {
		return nil, 0
	}
	ordered := make([]Thread, 0, len(ts))
	anchors := 0
	currentIndex := -1
	for i, t := range ts {
		if t.Current {
			currentIndex = i
			break
		}
	}
	if currentIndex >= 0 {
		ordered = append(ordered, ts[currentIndex])
		anchors = 1
	}

	rest := make([]Thread, 0, len(ts))
	for i, t := range ts {
		if i != currentIndex {
			rest = append(rest, t)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].MID < rest[j].MID })
	return append(ordered, rest...), anchors
}
