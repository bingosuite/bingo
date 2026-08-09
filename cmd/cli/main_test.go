package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	original := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = original }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

func testSnapshotEvent(t *testing.T, seq uint64) protocol.Event {
	t.Helper()

	return protocol.MustEvent(protocol.EventGoroutineSnapshot, seq, protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{
			{ID: 1, Status: "running", Current: true},
			{ID: 18, Status: "waiting", ParentID: 1},
		},
		Threads: []protocol.Thread{{ID: 4242, MID: 3, GoID: 1, Current: true}},
		Current: 1,
		Created: []int{18},
	})
}

// The `snapshot` command arms a one-shot full render; every other snapshot —
// including the automatic pushes — stays a one-line summary. Both are printed:
// the flag only picks the renderer, it never consumes or correlates an event.
func TestFullSnapshotNextRendersOnceThenSummarises(t *testing.T) {
	t.Cleanup(func() { fullSnapshotNext.Store(false) })

	fullSnapshotNext.Store(true)
	full := captureStdout(t, func() { printEvent(testSnapshotEvent(t, 2)) })
	if !strings.Contains(full, "goroutines: 2 live  threads: 1 live  current: G1") {
		t.Fatalf("requested snapshot render = %q, want the full view", full)
	}
	if !strings.Contains(full, "created: [18]") {
		t.Fatalf("requested snapshot render = %q, want the lifecycle deltas", full)
	}
	if fullSnapshotNext.Load() {
		t.Fatal("fullSnapshotNext still armed after one snapshot")
	}

	summary := captureStdout(t, func() { printEvent(testSnapshotEvent(t, 3)) })
	if !strings.Contains(summary, "[goroutines] 2 live, 1 live threads, current G1") {
		t.Fatalf("automatic snapshot render = %q, want the summary line", summary)
	}
	if strings.Contains(summary, "goroutines: 2 live") {
		t.Fatalf("automatic snapshot render = %q, want no full view", summary)
	}
}

// A rejected request (e.g. the process isn't suspended) must disarm the full
// render, or the next automatic push would be expanded in its place.
func TestSnapshotErrorDisarmsFullRender(t *testing.T) {
	t.Cleanup(func() { fullSnapshotNext.Store(false) })

	fullSnapshotNext.Store(true)
	rejected := protocol.MustEvent(protocol.EventError, 2, protocol.ErrorPayload{
		Command: protocol.CmdGoroutineSnapshot,
		Message: "process not suspended",
	})
	if out := captureStdout(t, func() { printEvent(rejected) }); !strings.Contains(out, "process not suspended") {
		t.Fatalf("error render = %q, want the server message", out)
	}
	if fullSnapshotNext.Load() {
		t.Fatal("fullSnapshotNext still armed after a rejected request")
	}

	summary := captureStdout(t, func() { printEvent(testSnapshotEvent(t, 3)) })
	if !strings.Contains(summary, "[goroutines] 2 live") {
		t.Fatalf("automatic snapshot render = %q, want the summary line", summary)
	}
}

// TestCountOf pins the CLI's honesty contract. Goroutine events are bounded by
// the wire contract, so printing the delivered length alone would state a
// truncated event as the live truth. The two collections have independent scan
// ceilings, so neither may borrow the other's lower-bound marker.
func TestCountOf(t *testing.T) {
	tests := []struct {
		name   string
		shown  int
		totals *protocol.SnapshotTotals
		which  collection
		want   string
	}{
		{
			name:  "complete list reports a live count",
			shown: 12, totals: nil, which: totalGoroutines,
			want: "12 live",
		},
		{
			name:   "omitted goroutines report included out of total",
			shown:  5000,
			totals: &protocol.SnapshotTotals{Goroutines: 41203, Threads: 64},
			which:  totalGoroutines,
			want:   "5000 of 41203",
		},
		{
			name:   "omitted threads report their own total",
			shown:  32,
			totals: &protocol.SnapshotTotals{Goroutines: 41203, Threads: 64},
			which:  totalThreads,
			want:   "32 of 64",
		},
		{
			name:   "a clipped goroutine scan marks its total a floor",
			shown:  10,
			totals: &protocol.SnapshotTotals{Goroutines: 10, Threads: 4, GoroutinesClipped: true},
			which:  totalGoroutines,
			want:   "10 of 10+",
		},
		{
			name:   "and does not mark the thread total",
			shown:  4,
			totals: &protocol.SnapshotTotals{Goroutines: 10, Threads: 4, GoroutinesClipped: true},
			which:  totalThreads,
			want:   "4 live",
		},
		{
			name:   "a clipped thread scan marks only threads",
			shown:  4,
			totals: &protocol.SnapshotTotals{Goroutines: 10, Threads: 4, ThreadsClipped: true},
			which:  totalThreads,
			want:   "4 of 4+",
		},
		{
			name:   "complete totals still read as live",
			shown:  10,
			totals: &protocol.SnapshotTotals{Goroutines: 10, Threads: 4},
			which:  totalGoroutines,
			want:   "10 live",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOf(tc.shown, tc.totals, tc.which); got != tc.want {
				t.Errorf("countOf() = %q, want %q", got, tc.want)
			}
		})
	}
}
