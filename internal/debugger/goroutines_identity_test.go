package debugger

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// TestPackedSnapshotCannotCarryStopIdentity is the reason emitBreakpointHit and
// emitPaused take the current goroutine from the pre-pack scan rather than from
// the payload they stream.
//
// Packing decides what may be DELIVERED. When the required anchors alone cannot
// satisfy the byte, count or string contract it degrades to empty collections —
// a reachable outcome, since a goroutine's location strings come from DWARF and
// are not bounded at the source. Reading the identity back out of that result
// yields a zero goroutine, so a breakpoint or pause event would name nothing and
// a DAP client would fall back to a synthetic thread 1 while the debugger knew
// exactly where it was stopped.
func TestPackedSnapshotCannotCarryStopIdentity(t *testing.T) {
	// Longer than the contract allows, so the anchor itself is unpackable.
	hostile := strings.Repeat("a", protocol.MaxGoroutineStringLength+1)
	stopped := protocol.Goroutine{
		ID: 77, Status: "running", Current: true,
		CurrentLoc: protocol.Location{File: hostile, Line: 5},
	}

	packed, report := packForWire(protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{stopped},
		Current:    stopped.ID,
	}, false, false)

	if !report.Degraded {
		t.Fatal("expected an unpackable anchor to degrade; the rest of this test " +
			"asserts what degradation costs, so it proves nothing otherwise")
	}
	if len(packed.Goroutines) != 0 || packed.Current != 0 {
		t.Fatalf("expected a degraded pack to deliver nothing, got %d goroutines, current=%d",
			len(packed.Goroutines), packed.Current)
	}

	// The identity the engine reports comes from the scan, not from the packed
	// payload above. Taking it from the payload would lose the stop entirely.
	if stopped.ID != 77 || !stopped.Current {
		t.Fatalf("pre-pack identity was mutated by packing: %+v", stopped)
	}
}

// TestSnapshotIdentitySurvivesDegradedDelivery pins the composition: whatever
// packing does to the streamed payload, the identity handed to the stop event
// still names the goroutine the scan found.
func TestSnapshotIdentitySurvivesDegradedDelivery(t *testing.T) {
	hostile := strings.Repeat("b", protocol.MaxGoroutineStringLength+1)
	scanned := []protocol.Goroutine{
		{ID: 41, Status: "runnable"},
		{ID: 42, Status: "running", Current: true,
			CurrentLoc: protocol.Location{File: hostile, Line: 9}},
	}

	var current protocol.Goroutine
	for _, g := range scanned {
		if g.Current {
			current = g
		}
	}

	packed, _ := packForWire(protocol.GoroutineSnapshotPayload{
		Goroutines: scanned, Current: current.ID,
	}, false, false)

	// Skip-and-continue may drop the oversized anchor's siblings or degrade
	// entirely; either way the identity must be unaffected.
	if current.ID != 42 {
		t.Fatalf("identity lost: got G%d, want G42", current.ID)
	}
	for _, g := range packed.Goroutines {
		if g.ID == 42 && g.CurrentLoc.File != "" && len(g.CurrentLoc.File) > protocol.MaxGoroutineStringLength {
			t.Fatal("packer delivered a string beyond the contract the consumer enforces")
		}
	}
}

// TestFinishSnapshotKeepsIdentityWhenDeliveryDegrades is the discriminating
// case: a scan that SUCCEEDED, whose packed form degrades because the current
// goroutine's DWARF-derived location exceeds the string contract. The delivered
// payload then names nobody, and anything that recovers the identity from it
// reports a stop on no goroutine at all.
func TestFinishSnapshotKeepsIdentityWhenDeliveryDegrades(t *testing.T) {
	e := &engine{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	hostile := strings.Repeat("a", protocol.MaxGoroutineStringLength+1)
	scan := []protocol.Goroutine{
		{ID: 88, Status: "running", Current: true,
			CurrentLoc: protocol.Location{File: hostile, Line: 12}},
	}

	packed, current := e.finishSnapshot(
		goroutineWalkResult{Items: scan, Complete: true},
		threadWalkResult{Complete: true},
		0x1000,
	)

	if len(packed.Goroutines) != 0 {
		t.Fatalf("expected delivery to degrade so this test discriminates, got %d delivered",
			len(packed.Goroutines))
	}
	if current.ID != 88 {
		t.Fatalf("stop identity lost to a delivery bound: got G%d, want G88", current.ID)
	}
	if !current.Current {
		t.Fatal("identity no longer marked current")
	}
}

// TestFinishSnapshotIdentityIsTheScannedGoroutine guards the ordinary path: the
// identity is the entry the scan marked current, not merely the first delivered.
func TestFinishSnapshotIdentityIsTheScannedGoroutine(t *testing.T) {
	e := &engine{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	scan := []protocol.Goroutine{
		{ID: 5, Status: "runnable"},
		{ID: 6, Status: "waiting"},
		{ID: 7, Status: "running", Current: true},
	}

	_, current := e.finishSnapshot(
		goroutineWalkResult{Items: scan, Complete: true},
		threadWalkResult{Complete: true},
		0x1000,
	)
	if current.ID != 7 {
		t.Fatalf("identity is not the scanned current goroutine: got G%d, want G7", current.ID)
	}
}
