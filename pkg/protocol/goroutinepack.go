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
	// Totals are the original untrimmed counts plus each collection's
	// scan-clipped flag, regardless of whether they reached the payload.
	Totals SnapshotTotals

	// Goroutines and Threads are the counts that survived onto the wire.
	Goroutines int
	Threads    int

	// Bytes is the exact marshalled Event size measured at the widest possible
	// sequence number (see packBudget), or ZERO when the result was proven to
	// fit by the cheap upper bound and never needed measuring. It is a
	// diagnostic, not a contract: a zero means "provably fine", not "empty".
	Bytes int

	// Degraded is true when not even the anchor elements fit, so the wire
	// carries empty collections. The lifecycle deltas are still intact.
	Degraded bool

	// Oversized is true when the result cannot conform to the wire contract:
	// either the lifecycle deltas alone exceed the byte budget, or there are
	// more of them than MaxLifecycleDeltaIDs. Deltas are never trimmed, so
	// there is no lever left — the producer must decide what to do rather than
	// have a corrupted lifecycle stream emitted on its behalf. The debugger's
	// own scan bounds both cases well below the limits, so this signals corrupt
	// input rather than a normal outcome.
	Oversized bool
}

// Omitted reports whether the wire is missing elements the producer had.
func (r GoroutinePackReport) Omitted() bool {
	return r.Goroutines < r.Totals.Goroutines || r.Threads < r.Totals.Threads
}

// PackSnapshot bounds a GoroutineSnapshotPayload to MaxGoroutineEventBytes and
// the element-count caps, returning a wire-ready payload plus a report.
//
// goroutinesClipped and threadsClipped say whether each of the caller's runtime
// walks hit its own ceiling before this call, which makes that count a lower
// bound. They are separate because the ceilings are (maxGoroutineScan and
// maxThreadScan), and a consumer must be able to tell which count to trust.
//
// Selection is deterministic so every client on one stop sees the same thing:
// the current goroutine, then its ancestors nearest-first, then the rest by
// ascending goid. The current goroutine, its whole ancestor chain, and the
// current thread are REQUIRED — a result that cannot keep them degrades to empty
// collections instead. Threads take an ordered floor before goroutines compete
// for the budget, then reclaim whatever is left.
func PackSnapshot(snap GoroutineSnapshotPayload, goroutinesClipped, threadsClipped bool) (GoroutineSnapshotPayload, GoroutinePackReport) {
	totals := SnapshotTotals{
		Goroutines:        len(snap.Goroutines),
		Threads:           len(snap.Threads),
		GoroutinesClipped: goroutinesClipped,
		ThreadsClipped:    threadsClipped,
	}
	// shape is the single source of the payload: pack measures through it and
	// the caller assembles through it, so the measured bytes cannot drift from
	// the bytes actually returned.
	shape := func(gs []Goroutine, ts []Thread, current int, totals *SnapshotTotals) GoroutineSnapshotPayload {
		return GoroutineSnapshotPayload{
			Goroutines: gs,
			Threads:    ts,
			Current:    current,
			Created:    snap.Created,
			Exited:     snap.Exited,
			Totals:     totals,
		}
	}
	build := func(gs []Goroutine, ts []Thread, current int, totals *SnapshotTotals) any {
		return shape(gs, ts, current, totals)
	}

	// The snapshot shape IS a tree, so the whole spawn chain is required.
	gs, ts, report := pack(snap.Goroutines, snap.Threads, snap.Current, totals, EventGoroutineSnapshot, build, true,
		len(snap.Created)+len(snap.Exited))
	// Deltas are passed through untouched, so an over-long one is reported
	// rather than trimmed: silently dropping lifecycle events would leave every
	// consumer's created/exited tracking permanently wrong.
	if len(snap.Created) > MaxLifecycleDeltaIDs || len(snap.Exited) > MaxLifecycleDeltaIDs {
		report.Oversized = true
	}
	return shape(gs, ts, deliveredCurrent(gs, snap.Current), payloadTotals(totals, report)), report
}

// PackGoroutines bounds the EventGoroutines list. It shares PackSnapshot's exact
// accounting and ordering; only the payload schema differs. There are no threads
// in this shape, so the whole budget goes to goroutines and only the goroutine
// scan flag is meaningful.
func PackGoroutines(gs []Goroutine, goroutinesClipped bool) (GoroutinesPayload, GoroutinePackReport) {
	totals := SnapshotTotals{Goroutines: len(gs), GoroutinesClipped: goroutinesClipped}
	shape := func(gs []Goroutine, totals *SnapshotTotals) GoroutinesPayload {
		return GoroutinesPayload{Goroutines: gs, Totals: totals}
	}
	build := func(gs []Goroutine, _ []Thread, _ int, totals *SnapshotTotals) any {
		return shape(gs, totals)
	}

	// This shape is a FLAT list — DAP renders it as threads, with no hierarchy to
	// break — so ancestors are ordered first but not required. Degrading it to
	// empty would make the DAP translator fabricate a synthetic "main" thread,
	// which is precisely the lie issue #194 exists to stop.
	packed, _, report := pack(gs, nil, 0, totals, EventGoroutines, build, false, 0)
	return shape(packed, payloadTotals(totals, report)), report
}

// payloadTotals decides whether the payload carries Totals: only when the wire
// omits elements or one of the producer's scans was clipped. A complete,
// unclipped result stays byte-identical to the pre-1.4 shape.
func payloadTotals(totals SnapshotTotals, report GoroutinePackReport) *SnapshotTotals {
	if !report.Omitted() && !totals.AnyClipped() {
		return nil
	}
	copied := totals
	return &copied
}

// payloadBuilder assembles one of the two goroutine payload shapes. Taking it as
// a parameter is what lets both packers share a single exact algorithm. current
// is threaded through because a degraded result must not keep pointing at a
// goroutine it did not deliver.
type payloadBuilder func(gs []Goroutine, ts []Thread, current int, totals *SnapshotTotals) any

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
	deltaCount int,
) ([]Goroutine, []Thread, GoroutinePackReport) {
	orderedG, anchorG := orderGoroutines(goroutines, current)
	orderedT, anchorT := orderThreads(threads)
	if !requireAncestors && anchorG > 1 {
		anchorG = 1 // ordering is kept; only the current goroutine is required
	}

	degraded := func() ([]Goroutine, []Thread, GoroutinePackReport) {
		// current is dropped along with the collections: naming a goroutine the
		// payload does not carry is a dangling reference, and a consumer would
		// either render a selection that does not exist or have to guess.
		gs, ts := []Goroutine{}, []Thread{}
		report := GoroutinePackReport{Totals: totals, Degraded: true}
		report.Bytes, _ = packBudget(kind, build(gs, ts, 0, payloadTotals(totals, report)))
		// Emptying the collections is the last lever available: the lifecycle
		// deltas are never trimmed, so if they alone overflow there is nothing
		// left to give. Say so rather than pretending the result conforms.
		report.Oversized = report.Bytes > MaxGoroutineEventBytes
		return gs, ts, report
	}

	// Fits-as-is fast path. A snapshot is packed on EVERY breakpoint stop, so the
	// overwhelmingly common case — a payload already inside every limit — must
	// not pay for the trimming machinery. Ordering is still applied (it is a
	// sort, not a marshal, and consumers rely on it), but the size is settled by
	// a bound, or failing that a single measurement, rather than a marshal per
	// element.
	//
	// A clipped scan does NOT disqualify this path. Clipping means the totals
	// must be attached, not that anything has to be trimmed — and treating it as
	// "must trim" made a thread-churning target, whose runtime.allm passes the
	// scan ceiling, pay full per-element packing on every stop.
	if len(orderedG) <= MaxSnapshotGoroutines &&
		len(orderedT) <= MaxSnapshotThreads &&
		elementsFitLimits(orderedG, orderedT) {
		gs, ts := nonNilGoroutines(orderedG), nonNilThreads(orderedT)
		fits := GoroutinePackReport{
			Totals:     totals,
			Goroutines: len(gs),
			Threads:    len(ts),
		}
		// Nothing is omitted here, so payloadTotals attaches Totals exactly when
		// a scan was clipped; the bound allows for it either way.
		attached := payloadTotals(totals, fits)
		if boundedSize(gs, ts, deltaCount) <= MaxGoroutineEventBytes {
			return gs, ts, fits
		}
		size, ok := packBudget(kind, build(gs, ts, deliveredCurrent(gs, current), attached))
		if ok && size <= MaxGoroutineEventBytes {
			fits.Bytes = size
			return gs, ts, fits
		}
	}

	// Whether Totals ends up on the wire changes the reserve, and it is only
	// known after packing — except when a scan was clipped, which forces it.
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
	gs, ts, report, ok := packOnce(orderedG, anchorG, orderedT, anchorT, current, totals, totals.AnyClipped(), kind, build)
	if !ok {
		return degraded()
	}
	if !totals.AnyClipped() && report.Omitted() {
		gs, ts, report, ok = packOnce(orderedG, anchorG, orderedT, anchorT, current, totals, true, kind, build)
		if !ok {
			return degraded()
		}
	}

	// The incremental accounting is exact (see packState), but the contract is
	// the measured size of the payload the caller actually sends, so that is
	// what is measured. A miss degrades rather than emitting a frame the
	// transport would reject outright.
	size, ok := packBudget(kind, build(gs, ts, deliveredCurrent(gs, current), payloadTotals(totals, report)))
	if !ok || size > MaxGoroutineEventBytes {
		return degraded()
	}
	report.Bytes = size
	return gs, ts, report
}

// boundedSize is a conservative upper bound on the marshalled Event, computed
// without marshalling anything: a fixed per-element allowance plus the worst
// case for each string. JSON escaping expands a byte by at most six (a control
// byte or an invalid one becomes \uXXXX), so six times the raw length can never
// under-estimate. When the bound already fits, the payload provably fits and no
// measurement is needed; when it does not, the caller measures for real.
// Fixed allowances for boundedSize, each at or above the real worst case: every
// key present, every numeric field at its widest, strings excluded (they are
// charged separately). They are derived from a real marshal in the tests, so
// adding a field to Goroutine, Thread, Location or SnapshotTotals fails there
// rather than silently making the bound under-estimate — which would let the
// fast path emit an event above the cap.
const (
	envelopeAllowance  = 384 // version, kind, a 20-digit seq, payload keys, totals
	goroutineAllowance = 352 // its own keys plus every numeric field
	threadAllowance    = 224
	deltaAllowance     = 24 // a 20-digit goid plus its separator
	escapeFactor       = 6  // the most JSON escaping can expand one byte
)

func boundedSize(gs []Goroutine, ts []Thread, deltas int) int {
	total := envelopeAllowance + deltas*deltaAllowance
	for i := range gs {
		total += goroutineAllowance + escapeFactor*goroutineStringBytes(&gs[i])
		if total > MaxGoroutineEventBytes {
			return total // already inconclusive; stop counting
		}
	}
	for i := range ts {
		total += threadAllowance + escapeFactor*locationStringBytes(&ts[i].CurrentLoc)
		if total > MaxGoroutineEventBytes {
			return total
		}
	}
	return total
}

func goroutineStringBytes(g *Goroutine) int {
	return len(g.Status) + len(g.WaitReason) +
		locationStringBytes(&g.CurrentLoc) +
		locationStringBytes(&g.StartLoc) +
		locationStringBytes(&g.CreatedLoc)
}

func locationStringBytes(l *Location) int {
	return len(l.File) + len(l.Function)
}

// elementsFitLimits reports whether every element already satisfies the
// per-element string limit, so the fast path can skip the trimming pass without
// risking an element the consumer would reject.
func elementsFitLimits(gs []Goroutine, ts []Thread) bool {
	for i := range gs {
		if !goroutineStringsFit(gs[i]) {
			return false
		}
	}
	for i := range ts {
		if !locationStringsFit(ts[i].CurrentLoc) {
			return false
		}
	}
	return true
}

func nonNilGoroutines(gs []Goroutine) []Goroutine {
	if gs == nil {
		return []Goroutine{}
	}
	return gs
}

func nonNilThreads(ts []Thread) []Thread {
	if ts == nil {
		return []Thread{}
	}
	return ts
}

// deliveredCurrent keeps the current goid only when that goroutine actually
// reached the wire. The current goroutine is a required anchor, so in any
// non-degraded result it did; this also covers a caller whose current goid names
// a goroutine absent from the input. The resulting invariant — current is either
// zero or present in Goroutines — is what lets a consumer validate it.
func deliveredCurrent(gs []Goroutine, current int) int {
	if current == 0 {
		return 0
	}
	for _, g := range gs {
		if g.ID == current {
			return current
		}
	}
	return 0
}

// packOnce runs one exact pass against a reserve that either includes Totals or
// does not. ok is false when the required anchors cannot be honoured.
func packOnce(
	orderedG []Goroutine,
	anchorG int,
	orderedT []Thread,
	anchorT int,
	current int,
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
	reserve, ok := packBudget(kind, build([]Goroutine{}, []Thread{}, current, reserved))
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
	if !goroutineStringsFit(g) {
		return false
	}
	size, ok := elementBytes(g)
	if !ok || !p.charge(size, len(p.goroutines)) {
		return false
	}
	p.goroutines = append(p.goroutines, g)
	return true
}

func (p *packState) addThread(t Thread) bool {
	if !locationStringsFit(t.CurrentLoc) {
		return false
	}
	size, ok := elementBytes(t)
	if !ok || !p.charge(size, len(p.threads)) {
		return false
	}
	p.threads = append(p.threads, t)
	return true
}

// goroutineStringsFit and locationStringsFit reject an element the consumer
// would refuse. Rejection is whole-element on purpose: truncating a file path or
// symbol would silently corrupt the very data a concurrency view exists to show.
// A non-anchor is skipped and packing continues; an anchor cannot be skipped, so
// its rejection degrades the result rather than emitting an undecodable event.
func goroutineStringsFit(g Goroutine) bool {
	return stringFits(g.Status) &&
		stringFits(g.WaitReason) &&
		locationStringsFit(g.CurrentLoc) &&
		locationStringsFit(g.StartLoc) &&
		locationStringsFit(g.CreatedLoc)
}

func locationStringsFit(l Location) bool {
	return stringFits(l.File) && stringFits(l.Function)
}

func stringFits(s string) bool {
	// Fast path: a string can never exceed the limit in UTF-16 code units with
	// fewer bytes than that, since every code unit costs at least one byte.
	if len(s) <= MaxGoroutineStringLength {
		return true
	}
	return utf16Len(s) <= MaxGoroutineStringLength
}

// utf16Len counts a string the way the JavaScript consumer does: in UTF-16 code
// units, so an astral character (U+10000 and above) counts as two, not one.
// Counting bytes or runes instead would disagree with the consumer at the
// boundary, and a disagreement there is exactly the bug this guards — the
// producer emits something it believes legal and the consumer must reject it.
//
// Bytes that are not valid UTF-8 are counted as one unit each, matching both
// Go's range loop and encoding/json, which substitutes one U+FFFD per bad byte.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
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
