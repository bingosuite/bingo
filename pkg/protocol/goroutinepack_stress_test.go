package protocol_test

import (
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
		if rep.Bytes != actual {
			t.Fatalf("round %d: reported %d, actual %d", round, rep.Bytes, actual)
		}

		list, rep2 := protocol.PackGoroutines(gs, false)
		actual2 := eventBytesPlain(t, protocol.EventGoroutines, list)
		if actual2 > protocol.MaxGoroutineEventBytes {
			t.Fatalf("round %d: list %d bytes exceeds cap", round, actual2)
		}
		if rep2.Bytes != actual2 {
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
		if rep.Bytes != actual {
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

// TestPackTwoPassInvariantsUnderRandomInput mixes deep ancestor chains, threads,
// lifecycle deltas, scan clipping, and near-cap element sizes. It pins the three
// invariants that cannot be verified by inspection: the result never exceeds the
// cap, the reported size is the real one, Totals is present exactly when the
// result is incomplete, and every anchor survives a non-degraded pack.
func TestPackTwoPassInvariantsUnderRandomInput(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	for round := 0; round < 600; round++ {
		n := 1 + rng.Intn(60)
		gs := make([]protocol.Goroutine, 0, n)
		for i := 1; i <= n; i++ {
			g := protocol.Goroutine{
				ID: i, Status: "waiting",
				WaitReason: strings.Repeat("w", rng.Intn(70000)),
			}
			if i > 1 && rng.Intn(3) > 0 {
				g.ParentID = i - 1 // deep chain
			}
			gs = append(gs, g)
		}
		cur := 1 + rng.Intn(n)
		ts := make([]protocol.Thread, 0, 8)
		for i := 0; i < rng.Intn(8); i++ {
			ts = append(ts, protocol.Thread{ID: i, MID: rng.Intn(50), Current: i == 0})
		}
		created := make([]int, rng.Intn(200))
		for i := range created {
			created[i] = math.MaxInt64 - i
		}
		clipped := rng.Intn(2) == 0

		out, rep := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
			Goroutines: gs, Threads: ts, Current: cur, Created: created,
		}, clipped, clipped)

		actual := eventBytesPlain(t, protocol.EventGoroutineSnapshot, out)
		if !rep.Oversized && actual > protocol.MaxGoroutineEventBytes {
			t.Fatalf("round %d: %d bytes over cap (degraded=%v)", round, actual, rep.Degraded)
		}
		if rep.Bytes != actual {
			t.Fatalf("round %d: report %d actual %d", round, rep.Bytes, actual)
		}
		// Totals presence must agree with reality.
		wantTotals := rep.Omitted() || clipped
		if wantTotals != (out.Totals != nil) {
			t.Fatalf("round %d: totals presence %v want %v (omitted=%v clipped=%v)",
				round, out.Totals != nil, wantTotals, rep.Omitted(), clipped)
		}
		// Anchors must be present in every non-degraded result.
		if !rep.Degraded {
			have := map[int]bool{}
			for _, g := range out.Goroutines {
				have[g.ID] = true
			}
			byID := map[int]protocol.Goroutine{}
			for _, g := range gs {
				if _, dup := byID[g.ID]; !dup {
					byID[g.ID] = g
				}
			}
			seen := map[int]bool{}
			for id := cur; id != 0 && !seen[id]; {
				seen[id] = true
				if _, ok := byID[id]; !ok {
					break
				}
				if !have[id] {
					t.Fatalf("round %d: anchor g%d missing from non-degraded result", round, id)
				}
				id = byID[id].ParentID
			}
			if len(ts) > 0 && len(out.Threads) == 0 {
				t.Fatalf("round %d: current thread dropped", round)
			}
		}
	}
}
