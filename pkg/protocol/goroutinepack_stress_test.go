package protocol_test

import (
	"encoding/json"
	"math"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// TestPackExactSizeUnderAdversarialStrings is the regression guard for the
// packer's additivity assumption: it charges each element its standalone
// marshal length, which is only exact if encoding/json's escaping is
// context-free and a json.RawMessage payload survives the envelope verbatim.
// Randomized hostile input — HTML-escaped runes, U+2028/U+2029, control
// characters, lone surrogates, invalid UTF-8, and long values — must never make
// the measured size disagree with the real one, or a frame could slip past the
// cap and be rejected below the consumer's decoder. See issue #194.
func TestPackExactSizeUnderAdversarialStrings(t *testing.T) {
	rng := rand.New(rand.NewSource(20260809))
	nasty := []string{
		"<", ">", "&", "\"", "\\", "\u2028", "\u2029", "\x00", "\x1f", "\x7f",
		"\U0001F600", "服务", "café", string([]byte{0xff, 0xfe}), string([]byte{0xc3, 0x28}),
		string(utf16.Decode([]uint16{0xD800})), "\ufffd", "\n\r\t",
	}
	pick := func() string {
		s := ""
		for i := 0; i < rng.Intn(6); i++ {
			s += nasty[rng.Intn(len(nasty))]
		}
		if rng.Intn(10) == 0 {
			s += string(make([]byte, rng.Intn(3000)))
		}
		return s
	}

	for round := 0; round < 120; round++ {
		n := 1 + rng.Intn(200)
		gs := make([]protocol.Goroutine, 0, n)
		for i := 1; i <= n; i++ {
			gs = append(gs, protocol.Goroutine{
				ID: i, ParentID: rng.Intn(n + 1), Status: pick(), WaitReason: pick(),
				CurrentLoc: protocol.Location{File: pick(), Line: rng.Intn(9999), Function: pick()},
				StartLoc:   protocol.Location{File: pick(), Function: pick()},
				CreatedLoc: protocol.Location{File: pick(), Function: pick()},
				ThreadID:   rng.Intn(64), Current: i == 1,
			})
		}
		ts := make([]protocol.Thread, 0, 40)
		for i := 0; i < rng.Intn(40); i++ {
			ts = append(ts, protocol.Thread{
				ID: i, MID: rng.Intn(100), GoID: rng.Intn(n + 1),
				CurrentLoc: protocol.Location{File: pick(), Function: pick()},
				Current:    i == 0,
			})
		}
		created := make([]int, rng.Intn(50))
		for i := range created {
			created[i] = math.MaxInt64 - i
		}

		snap, rep := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
			Goroutines: gs, Threads: ts, Current: 1, Created: created,
		}, rng.Intn(2) == 0, rng.Intn(2) == 0)
		actual := eventBytesPlain(t, protocol.EventGoroutineSnapshot, snap)
		if actual > protocol.MaxGoroutineEventBytes {
			t.Fatalf("round %d: snapshot %d bytes exceeds cap", round, actual)
		}
		// Bytes is zero when the cheap bound proved the fit without measuring;
		// a non-zero value must still be the real one.
		if rep.Bytes != 0 && rep.Bytes != actual {
			t.Fatalf("round %d: reported %d, actual %d", round, rep.Bytes, actual)
		}

		list, rep2 := protocol.PackGoroutines(gs, false)
		actual2 := eventBytesPlain(t, protocol.EventGoroutines, list)
		if actual2 > protocol.MaxGoroutineEventBytes {
			t.Fatalf("round %d: list %d bytes exceeds cap", round, actual2)
		}
		if rep2.Bytes != 0 && rep2.Bytes != actual2 {
			t.Fatalf("round %d: list reported %d, actual %d", round, rep2.Bytes, actual2)
		}
	}
}

func eventBytesPlain(t *testing.T, kind protocol.EventKind, payload any) int {
	t.Helper()
	evt, err := protocol.NewEvent(kind, math.MaxUint64, payload)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	raw, err := protocol.MarshalEvent(evt)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	return len(raw)
}

// TestPackNeverExceedsCapAcrossTheBoundary sweeps every byte size in a window
// around the cap. Both directions matter: the packer must never return a payload
// above the cap, and must never falsely degrade one that fits. The second is the
// regression the two-pass exact reserve exists for — reserving space for Totals
// that the result does not carry silently shrank the usable budget.
func TestPackNeverExceedsCapAcrossTheBoundary(t *testing.T) {
	// Every element string is separately capped, so a payload can only approach
	// the byte budget through many elements. buildSized assembles one of exactly
	// the requested size out of legal elements: an anchor, fixed-size fillers,
	// and a tail tuned to land on the byte.
	fillerText := strings.Repeat("w", 4000)
	build := func(fillers int, tail string) protocol.GoroutineSnapshotPayload {
		gs := []protocol.Goroutine{{ID: 1, Status: "running", Current: true}}
		for i := 2; i <= fillers+1; i++ {
			gs = append(gs, protocol.Goroutine{ID: i, Status: "waiting", WaitReason: fillerText})
		}
		if tail != "" {
			gs = append(gs, protocol.Goroutine{ID: 900000, Status: "waiting", WaitReason: tail})
		}
		return protocol.GoroutineSnapshotPayload{
			Goroutines: gs, Threads: []protocol.Thread{}, Current: 1,
		}
	}
	sized := func(t *testing.T, size int) protocol.GoroutineSnapshotPayload {
		t.Helper()
		anchorOnly := eventBytesPlain(t, protocol.EventGoroutineSnapshot, build(0, ""))
		perFiller := eventBytesPlain(t, protocol.EventGoroutineSnapshot, build(1, "")) - anchorOnly
		for fillers := (size - anchorOnly) / perFiller; fillers >= 0; fillers-- {
			body := eventBytesPlain(t, protocol.EventGoroutineSnapshot, build(fillers, ""))
			oneChar := eventBytesPlain(t, protocol.EventGoroutineSnapshot, build(fillers, "x")) - body
			length := size - body - oneChar + 1
			if length < 1 || length > protocol.MaxGoroutineStringLength {
				continue
			}
			snap := build(fillers, strings.Repeat("t", length))
			if got := eventBytesPlain(t, protocol.EventGoroutineSnapshot, snap); got != size {
				t.Fatalf("built %d bytes, want %d", got, size)
			}
			return snap
		}
		t.Fatalf("could not build a payload of exactly %d bytes", size)
		return protocol.GoroutineSnapshotPayload{}
	}

	for offset := -60; offset <= 60; offset++ {
		target := protocol.MaxGoroutineEventBytes + offset
		snap := sized(t, target)

		out, rep := protocol.PackSnapshot(snap, false, false)
		actual := eventBytesPlain(t, protocol.EventGoroutineSnapshot, out)
		if actual > protocol.MaxGoroutineEventBytes {
			t.Fatalf("offset %d: returned %d bytes, over cap", offset, actual)
		}
		if rep.Bytes != 0 && rep.Bytes != actual {
			t.Fatalf("offset %d: report %d actual %d", offset, rep.Bytes, actual)
		}
		if rep.Degraded {
			t.Fatalf("offset %d: degraded a payload whose anchor fits", offset)
		}
		if offset <= 0 {
			if rep.Omitted() {
				t.Fatalf("offset %d: dropped an element from a payload that fits", offset)
			}
			if len(out.Goroutines) != len(snap.Goroutines) {
				t.Fatalf("offset %d: kept %d of %d", offset, len(out.Goroutines), len(snap.Goroutines))
			}
		} else if !rep.Omitted() {
			t.Fatalf("offset %d: kept an over-budget payload whole", offset)
		}
	}
}

// packStressCase is one randomized input plus the facts an assertion needs to
// judge the result. Keeping generation, packing and checking in separate units
// is what keeps each readable — the invariants are subtle enough that burying
// them inside a generator loop hides which one actually failed.
type packStressCase struct {
	round   int
	input   protocol.GoroutineSnapshotPayload
	current int
	clipped bool
}

// randomPackStressCase mixes the shapes that interact badly: deep ancestor
// chains, threads, large lifecycle deltas, scan clipping, and element sizes that
// straddle the budget.
func randomPackStressCase(rng *rand.Rand, round int) packStressCase {
	n := 1 + rng.Intn(60)
	// Half the rounds stay comfortably inside every limit. Without them the
	// generator would omit something almost every time — a 70 KB wait reason
	// exceeds the per-string cap on its own — and "complete but clipped", the
	// case where the clipped flag ALONE must force Totals onto the wire, would
	// never be exercised.
	roomy := rng.Intn(2) == 0
	gs := make([]protocol.Goroutine, 0, n)
	for i := 1; i <= n; i++ {
		reason := rng.Intn(70000)
		if roomy {
			reason = rng.Intn(80)
		}
		g := protocol.Goroutine{
			ID: i, Status: "waiting",
			WaitReason: strings.Repeat("w", reason),
		}
		if i > 1 && rng.Intn(3) > 0 {
			g.ParentID = i - 1 // deep chain
		}
		gs = append(gs, g)
	}

	ts := make([]protocol.Thread, 0, 8)
	for i := 0; i < rng.Intn(8); i++ {
		ts = append(ts, protocol.Thread{ID: i, MID: rng.Intn(50), Current: i == 0})
	}

	deltas := rng.Intn(200)
	if roomy {
		deltas = rng.Intn(5)
	}
	created := make([]int, deltas)
	for i := range created {
		created[i] = math.MaxInt64 - i
	}

	current := 1 + rng.Intn(n)
	return packStressCase{
		round: round,
		input: protocol.GoroutineSnapshotPayload{
			Goroutines: gs, Threads: ts, Current: current, Created: created,
		},
		current: current,
		clipped: rng.Intn(2) == 0,
	}
}

// assertSizeIsReportedAndBounded pins the contract itself: the payload fits the
// budget unless the packer explicitly says it could not, and the reported size is
// the real one rather than an estimate.
func assertSizeIsReportedAndBounded(
	t *testing.T,
	c packStressCase,
	out protocol.GoroutineSnapshotPayload,
	rep protocol.GoroutinePackReport,
) {
	t.Helper()
	actual := eventBytesPlain(t, protocol.EventGoroutineSnapshot, out)
	if !rep.Oversized && actual > protocol.MaxGoroutineEventBytes {
		t.Fatalf("round %d: %d bytes over cap (degraded=%v)", c.round, actual, rep.Degraded)
	}
	if rep.Bytes != 0 && rep.Bytes != actual {
		t.Fatalf("round %d: report %d actual %d", c.round, rep.Bytes, actual)
	}
}

// assertTotalsPresence pins the honesty rule: Totals appears exactly when the
// result is incomplete, so its presence alone means "this is not everything".
func assertTotalsPresence(
	t *testing.T,
	c packStressCase,
	out protocol.GoroutineSnapshotPayload,
	rep protocol.GoroutinePackReport,
) {
	t.Helper()
	want := rep.Omitted() || c.clipped
	if want != (out.Totals != nil) {
		t.Fatalf("round %d: totals presence %v want %v (omitted=%v clipped=%v)",
			c.round, out.Totals != nil, want, rep.Omitted(), c.clipped)
	}
}

// assertAnchorsRetained walks the spawn chain the packer promised to keep. A
// non-degraded result must carry the current goroutine, every ancestor of it,
// and the current thread; and Current must name a goroutine actually delivered.
func assertAnchorsRetained(
	t *testing.T,
	c packStressCase,
	out protocol.GoroutineSnapshotPayload,
	rep protocol.GoroutinePackReport,
) {
	t.Helper()
	if rep.Degraded {
		return
	}

	delivered := make(map[int]bool, len(out.Goroutines))
	for _, g := range out.Goroutines {
		delivered[g.ID] = true
	}
	if out.Current != 0 && !delivered[out.Current] {
		t.Fatalf("round %d: current g%d is not among the delivered goroutines", c.round, out.Current)
	}

	for id := range ancestorChain(c.input.Goroutines, c.current) {
		if !delivered[id] {
			t.Fatalf("round %d: anchor g%d missing from non-degraded result", c.round, id)
		}
	}
	if len(c.input.Threads) > 0 && len(out.Threads) == 0 {
		t.Fatalf("round %d: current thread dropped", c.round)
	}
}

// ancestorChain yields the current goroutine and each ancestor above it, stopping
// at an unknown parent or a cycle so a malformed input cannot hang the test.
func ancestorChain(gs []protocol.Goroutine, current int) map[int]struct{} {
	byID := make(map[int]protocol.Goroutine, len(gs))
	for _, g := range gs {
		if _, dup := byID[g.ID]; !dup {
			byID[g.ID] = g
		}
	}

	chain := make(map[int]struct{})
	for id := current; id != 0; {
		if _, seen := chain[id]; seen {
			break
		}
		g, ok := byID[id]
		if !ok {
			break
		}
		chain[id] = struct{}{}
		id = g.ParentID
	}
	return chain
}

// TestPackTwoPassInvariantsUnderRandomInput pins the invariants that cannot be
// verified by inspection: the result never exceeds the cap, the reported size is
// the real one, Totals is present exactly when the result is incomplete, and
// every anchor survives a non-degraded pack.
func TestPackTwoPassInvariantsUnderRandomInput(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for round := 0; round < 600; round++ {
		c := randomPackStressCase(rng, round)
		out, rep := protocol.PackSnapshot(c.input, c.clipped, c.clipped)

		assertSizeIsReportedAndBounded(t, c, out, rep)
		assertTotalsPresence(t, c, out, rep)
		assertAnchorsRetained(t, c, out, rep)
	}
}

// stringBytesOf sums the raw string bytes an element carries, so a test can
// separate what boundedSize charges per-string from its fixed allowance.
func stringBytesOf(g protocol.Goroutine) int {
	loc := func(l protocol.Location) int { return len(l.File) + len(l.Function) }
	return len(g.Status) + len(g.WaitReason) +
		loc(g.CurrentLoc) + loc(g.StartLoc) + loc(g.CreatedLoc)
}

// TestPackAllowancesCoverTheRealWorstCase derives each fixed allowance from an
// actual marshal instead of trusting the constant. Every key is present and
// every numeric field is at its widest, so adding a field to Goroutine, Thread,
// Location or SnapshotTotals fails here — rather than silently making
// boundedSize under-estimate and letting the fast path emit an over-cap event.
func TestPackAllowancesCoverTheRealWorstCase(t *testing.T) {
	envelope, goroutine, thread, delta := protocol.PackAllowances()
	widest := protocol.Location{File: "x", Line: math.MaxInt64, Function: "y"}

	g := protocol.Goroutine{
		ID: math.MaxInt64, ParentID: math.MaxInt64, Status: "s", WaitReason: "w",
		CurrentLoc: widest, StartLoc: widest, CreatedLoc: widest,
		ThreadID: math.MaxInt64, Current: true,
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	if fixed := len(raw) - stringBytesOf(g); fixed > goroutine {
		t.Fatalf("goroutine allowance %d is below the real worst case %d", goroutine, fixed)
	}

	th := protocol.Thread{
		ID: math.MaxInt64, MID: math.MaxInt64, GoID: math.MaxInt64,
		Spinning: true, CurrentLoc: widest, Current: true,
	}
	raw, err = json.Marshal(th)
	if err != nil {
		t.Fatal(err)
	}
	strBytes := len(widest.File) + len(widest.Function)
	if fixed := len(raw) - strBytes; fixed > thread {
		t.Fatalf("thread allowance %d is below the real worst case %d", thread, fixed)
	}

	totals := protocol.SnapshotTotals{
		Goroutines: math.MaxInt64, Threads: math.MaxInt64,
		GoroutinesClipped: true, ThreadsClipped: true,
	}
	skeleton := protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{}, Threads: []protocol.Thread{},
		Current: math.MaxInt64, Created: []int{}, Exited: []int{}, Totals: &totals,
	}
	if size := eventBytesPlain(t, protocol.EventGoroutineSnapshot, skeleton); size > envelope {
		t.Fatalf("envelope allowance %d is below the real worst case %d", envelope, size)
	}

	// A goid is at most 19 digits plus its separator.
	if delta < 20 {
		t.Fatalf("delta allowance %d cannot hold a max-width goid", delta)
	}
}

// TestBoundedSizeNeverUnderEstimates is the soundness property the fast path
// rests on: if the bound says a payload fits, it must actually fit. An
// under-estimate would emit an event above the cap that the consumer is obliged
// to reject — the exact failure this contract exists to prevent.
func TestBoundedSizeNeverUnderEstimates(t *testing.T) {
	// Control characters are the adversarial case: each expands sixfold, which
	// is precisely the escape factor, so the fixed allowance carries no slack.
	strings := []string{
		"", "a", "\x00\x01\x02", "\x1f", "<>&\"\\", "\u2028\u2029",
		"\U0001F600", "服务", string([]byte{0xff, 0xfe}),
	}
	widestLoc := func(s string) protocol.Location {
		return protocol.Location{File: s, Line: math.MaxInt64, Function: s}
	}

	for _, s := range strings {
		for _, n := range []int{1, 50, 400} {
			gs := make([]protocol.Goroutine, 0, n)
			for i := 1; i <= n; i++ {
				gs = append(gs, protocol.Goroutine{
					ID: math.MaxInt64 - i, ParentID: math.MaxInt64 - i, ThreadID: math.MaxInt64,
					Status: s, WaitReason: s, Current: i == 1,
					CurrentLoc: widestLoc(s), StartLoc: widestLoc(s), CreatedLoc: widestLoc(s),
				})
			}
			ts := make([]protocol.Thread, 0, 16)
			for i := 0; i < 16; i++ {
				ts = append(ts, protocol.Thread{
					ID: math.MaxInt64, MID: math.MaxInt64, GoID: math.MaxInt64,
					Spinning: true, CurrentLoc: widestLoc(s), Current: i == 0,
				})
			}
			deltas := []int{math.MaxInt64, math.MaxInt64 - 1}

			bound := protocol.BoundedSizeForTest(gs, ts, len(deltas)*2)
			actual := eventBytesPlain(t, protocol.EventGoroutineSnapshot,
				protocol.GoroutineSnapshotPayload{
					Goroutines: gs, Threads: ts, Current: gs[0].ID,
					Created: deltas, Exited: deltas,
					Totals: &protocol.SnapshotTotals{
						Goroutines: math.MaxInt64, Threads: math.MaxInt64,
						GoroutinesClipped: true, ThreadsClipped: true,
					},
				})
			if bound < actual {
				t.Fatalf("bound %d under-estimates actual %d (n=%d, string=%q)",
					bound, actual, n, s)
			}
		}
	}
}

// TestPackMaxShapedPayload drives the packer at both element caps with the
// widest numeric fields and minimal strings — the shape whose cost is dominated
// by the fixed allowances rather than by content.
func TestPackMaxShapedPayload(t *testing.T) {
	gs := make([]protocol.Goroutine, 0, protocol.MaxSnapshotGoroutines)
	for i := 1; i <= protocol.MaxSnapshotGoroutines; i++ {
		gs = append(gs, protocol.Goroutine{
			ID: math.MaxInt64 - i, ParentID: math.MaxInt64 - i, ThreadID: math.MaxInt64,
			Status: "r", Current: i == 1,
			CurrentLoc: protocol.Location{File: "f", Line: math.MaxInt64, Function: "n"},
		})
	}
	ts := make([]protocol.Thread, 0, protocol.MaxSnapshotThreads)
	for i := 0; i < protocol.MaxSnapshotThreads; i++ {
		ts = append(ts, protocol.Thread{
			ID: math.MaxInt64, MID: math.MaxInt64, GoID: math.MaxInt64,
			Spinning: true, Current: i == 0,
			CurrentLoc: protocol.Location{File: "f", Line: math.MaxInt64, Function: "n"},
		})
	}

	out, report := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
		Goroutines: gs, Threads: ts, Current: gs[0].ID,
	}, false, false)

	if report.Degraded || report.Oversized {
		t.Fatalf("max-shaped payload degraded=%v oversized=%v", report.Degraded, report.Oversized)
	}
	actual := eventBytesPlain(t, protocol.EventGoroutineSnapshot, out)
	if actual > protocol.MaxGoroutineEventBytes {
		t.Fatalf("max-shaped payload is %d bytes, over the cap", actual)
	}
	// Either the bound proved it (fast path) or the measurement did; both are
	// correct, but the bound must never have claimed a fit it could not back.
	if bound := protocol.BoundedSizeForTest(out.Goroutines, out.Threads, 0); bound < actual {
		t.Fatalf("bound %d under-estimates actual %d at max shape", bound, actual)
	}
	if len(out.Goroutines) != protocol.MaxSnapshotGoroutines || len(out.Threads) != protocol.MaxSnapshotThreads {
		t.Fatalf("max-shaped payload lost elements: %d goroutines, %d threads",
			len(out.Goroutines), len(out.Threads))
	}
}

// TestFastPathEmissionsAreUnderTheCap targets the only region where an unsound
// bound can actually do damage: when boundedSize proves a fit, the packer
// returns WITHOUT marshalling, so nothing downstream would notice an over-cap
// event. A max-shaped payload does not exercise this — its bound correctly
// exceeds the cap and falls through to measurement. So this sweeps shapes up to
// and across the point where the fast path stops being taken, and for every
// pack that skipped measurement it marshals the real event and holds it to the
// cap. Most cases here do take the fast path; the guard below keeps the test
// honest if a future change moves that boundary.
// snapshotOfWidth builds a snapshot of n goroutines whose eight string fields
// each carry width control characters — the adversarial fill, since they escape
// to six bytes, exactly what the bound charges, leaving it no slack.
func snapshotOfWidth(n, width int) protocol.GoroutineSnapshotPayload {
	fill := strings.Repeat("\x1f", width)
	loc := protocol.Location{File: fill, Line: math.MaxInt64, Function: fill}
	gs := make([]protocol.Goroutine, 0, n)
	for i := 1; i <= n; i++ {
		gs = append(gs, protocol.Goroutine{
			ID: math.MaxInt64 - i, ParentID: math.MaxInt64 - i, ThreadID: math.MaxInt64,
			Status: fill, WaitReason: fill, Current: i == 1,
			CurrentLoc: loc, StartLoc: loc, CreatedLoc: loc,
		})
	}
	ts := make([]protocol.Thread, 0, 64)
	for i := 0; i < 64; i++ {
		ts = append(ts, protocol.Thread{
			ID: math.MaxInt64, MID: math.MaxInt64, GoID: math.MaxInt64,
			Current: i == 0, CurrentLoc: loc,
		})
	}
	return protocol.GoroutineSnapshotPayload{Goroutines: gs, Threads: ts, Current: gs[0].ID}
}

// packedOnFastPath reports whether the pack skipped measurement entirely.
func packedOnFastPath(in protocol.GoroutineSnapshotPayload) (protocol.GoroutineSnapshotPayload, protocol.GoroutinePackReport, bool) {
	protocol.ResetPackMarshalCounts()
	out, report := protocol.PackSnapshot(in, false, false)
	elements, envelopes := protocol.PackMarshalCounts()
	return out, report, elements == 0 && envelopes == 0
}

// TestFastPathEmissionsAreUnderTheCap targets the only region where an unsound
// bound can do damage: when boundedSize proves a fit, the packer returns WITHOUT
// marshalling, so nothing downstream would notice an over-cap event.
//
// It searches for the boundary rather than sampling a grid. The dangerous band
// is narrow in both element count and string width, and the two trade off
// against each other, so any fixed sample steps over it: with one under-estimate
// a width sweep put adjacent points 52 KB below the cap and far above it, and a
// two-dimensional grid still straddled the violation at n between its 400 and
// 1000 rungs. The largest element count that still takes the fast path is
// exactly where bound approaches the cap, which is where any under-estimate
// shows up, so each width is bisected for that count and checked there.
func TestFastPathEmissionsAreUnderTheCap(t *testing.T) {
	checked := 0
	for _, width := range []int{0, 1, 2, 3, 4, 6, 8, 12, 16, 24, 32, 48, 64, 96, 128, 256, 512} {
		// The fast path is taken while the bound holds, and the bound rises
		// monotonically with the element count, so the frontier is bisectable.
		lo, hi := 1, protocol.MaxSnapshotGoroutines
		if _, _, fast := packedOnFastPath(snapshotOfWidth(lo, width)); !fast {
			continue // even one element is measured at this width
		}
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if _, _, fast := packedOnFastPath(snapshotOfWidth(mid, width)); fast {
				lo = mid
			} else {
				hi = mid - 1
			}
		}

		// Check at the frontier and just inside it: an under-estimate that only
		// bites a little below the edge would otherwise slip through.
		for _, n := range []int{lo, lo * 9 / 10, lo / 2} {
			if n < 1 {
				continue
			}
			out, report, fast := packedOnFastPath(snapshotOfWidth(n, width))
			if !fast {
				continue
			}
			checked++
			if report.Degraded {
				t.Fatalf("fast path degraded (n=%d, width=%d)", n, width)
			}
			if size := eventBytesPlain(t, protocol.EventGoroutineSnapshot, out); size > protocol.MaxGoroutineEventBytes {
				t.Fatalf("fast path emitted %d bytes, over the cap (n=%d, width=%d)",
					size, n, width)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no case took the fast path, so this proved nothing")
	}
}
