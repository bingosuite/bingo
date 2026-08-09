package debugger_test

import (
	"reflect"
	"testing"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

func goroutineSet(ids ...int) []protocol.Goroutine {
	gs := make([]protocol.Goroutine, 0, len(ids))
	for _, id := range ids {
		gs = append(gs, protocol.Goroutine{ID: id, Status: "running"})
	}
	return gs
}

func newLifecycleEngine(t *testing.T) debugger.Debugger {
	t.Helper()

	d := debugger.NewWithBackend(newFakeBackend(), nil)
	t.Cleanup(func() { _ = d.Kill() })
	return d
}

// A query between two automatic snapshots must neither consume the pending
// deltas nor fabricate its own: the second automatic snapshot has to report
// everything that changed since the first one.
// The wiring — that engine.GoroutineSnapshot actually reaches the query path —
// is pinned by the concurrency E2E (declareBaselineOwnershipSpec), which is the
// only place a query can observe a live set the automatic baseline has not
// adopted. These tests pin the seam's semantics; they cannot catch a rewiring.
func TestQuerySnapshotDoesNotConsumeLifecycleDeltas(t *testing.T) {
	d := newLifecycleEngine(t)

	first := debugger.ExportedSnapshotFrom(d, goroutineSet(1, 2), true)
	if len(first.Created) != 0 || len(first.Exited) != 0 {
		t.Fatalf("first automatic snapshot deltas = %v/%v, want empty baseline",
			first.Created, first.Exited)
	}
	if got := debugger.ExportedPrevGoids(d); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("baseline after first automatic snapshot = %v, want [1 2]", got)
	}

	// On-demand query taken at a moment when 2 is gone and 3 exists.
	query := debugger.ExportedSnapshotFrom(d, goroutineSet(1, 3), false)
	if len(query.Created) != 0 || len(query.Exited) != 0 {
		t.Fatalf("query snapshot deltas = %v/%v, want none", query.Created, query.Exited)
	}
	if !reflect.DeepEqual(query.Goroutines, goroutineSet(1, 3)) {
		t.Fatalf("query snapshot goroutines = %+v, want the live set", query.Goroutines)
	}
	if got := debugger.ExportedPrevGoids(d); !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("baseline after query = %v, want it untouched at [1 2]", got)
	}

	// The next automatic snapshot still reports everything since the first one.
	second := debugger.ExportedSnapshotFrom(d, goroutineSet(1, 3), true)
	if !reflect.DeepEqual(second.Created, []int{3}) {
		t.Fatalf("second automatic snapshot created = %v, want [3]", second.Created)
	}
	if !reflect.DeepEqual(second.Exited, []int{2}) {
		t.Fatalf("second automatic snapshot exited = %v, want [2]", second.Exited)
	}
	if got := debugger.ExportedPrevGoids(d); !reflect.DeepEqual(got, []int{1, 3}) {
		t.Fatalf("baseline after second automatic snapshot = %v, want [1 3]", got)
	}
}

// Repeated queries stay read-only: no query may seed the baseline either, or
// the first automatic snapshot after a fresh session would lose its deltas.
func TestQuerySnapshotNeverSeedsLifecycleBaseline(t *testing.T) {
	d := newLifecycleEngine(t)

	for i := 0; i < 3; i++ {
		if snap := debugger.ExportedSnapshotFrom(d, goroutineSet(1, 2), false); len(snap.Created)+len(snap.Exited) != 0 {
			t.Fatalf("query %d deltas = %v/%v, want none", i, snap.Created, snap.Exited)
		}
	}
	if got := debugger.ExportedPrevGoids(d); got != nil {
		t.Fatalf("baseline after queries = %v, want nil (unseeded)", got)
	}

	// The first automatic snapshot is still a baseline, not a delta report.
	first := debugger.ExportedSnapshotFrom(d, goroutineSet(1, 2), true)
	if len(first.Created)+len(first.Exited) != 0 {
		t.Fatalf("first automatic snapshot deltas = %v/%v, want empty baseline",
			first.Created, first.Exited)
	}
	next := debugger.ExportedSnapshotFrom(d, goroutineSet(1, 2, 5), true)
	if !reflect.DeepEqual(next.Created, []int{5}) {
		t.Fatalf("automatic snapshot created = %v, want [5]", next.Created)
	}
}

// The on-demand command must still be answerable and must still refuse to read
// a process that isn't suspended.
func TestGoroutineSnapshotCommandRequiresSuspended(t *testing.T) {
	d := newLifecycleEngine(t)

	if _, err := d.GoroutineSnapshot(); err == nil {
		t.Fatal("GoroutineSnapshot while not suspended: want error")
	}

	debugger.ExportedForceSuspended(d)
	snap, err := d.GoroutineSnapshot()
	if err != nil {
		t.Fatalf("GoroutineSnapshot while suspended: %v", err)
	}
	// Without DWARF the runtime is unreadable, so the query degrades to the
	// synthetic goroutine — and a degraded read must not touch the baseline.
	if len(snap.Goroutines) != 1 {
		t.Fatalf("degraded query goroutines = %+v, want the synthetic one", snap.Goroutines)
	}
	if len(snap.Created)+len(snap.Exited) != 0 {
		t.Fatalf("degraded query deltas = %v/%v, want none", snap.Created, snap.Exited)
	}
	if got := debugger.ExportedPrevGoids(d); got != nil {
		t.Fatalf("baseline after degraded query = %v, want nil", got)
	}
}
