package main

import (
	"strings"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// TestCountsLine pins the observer's honesty contract: it must distinguish
// "this event is partial" from "the debugger's own scan stopped early", because
// they mean different things to someone reading the tree. See issue #194.
func TestCountsLine(t *testing.T) {
	snap := func(g, th int, totals *protocol.SnapshotTotals) protocol.GoroutineSnapshotPayload {
		return protocol.GoroutineSnapshotPayload{
			Goroutines: make([]protocol.Goroutine, g),
			Threads:    make([]protocol.Thread, th),
			Totals:     totals,
		}
	}

	tests := []struct {
		name     string
		snap     protocol.GoroutineSnapshotPayload
		contains []string
		absent   []string
	}{
		{
			name:     "complete snapshot claims completeness",
			snap:     snap(12, 4, nil),
			contains: []string{"goroutines=12", "threads=4", "complete"},
			absent:   []string{"omitted", "lower bound", "/"},
		},
		{
			name: "wire omission reports included out of total",
			snap: snap(5000, 32, &protocol.SnapshotTotals{Goroutines: 41203, Threads: 64}),
			contains: []string{
				"goroutines=5000/41203", "threads=32/64",
				"36203 goroutines omitted from this event",
				"32 threads omitted from this event",
			},
			absent: []string{"lower bound", "+"},
		},
		{
			// The four scan-clipping combinations. Each count must carry its own
			// marker: the ceilings are independent, so borrowing one flag for
			// both would call an exact count approximate.
			name:     "neither scan clipped",
			snap:     snap(10, 4, &protocol.SnapshotTotals{Goroutines: 12, Threads: 6}),
			contains: []string{"goroutines=10/12", "threads=4/6"},
			absent:   []string{"+", "lower bound", "hit its ceiling"},
		},
		{
			name: "goroutine scan clipped only",
			snap: snap(10, 4, &protocol.SnapshotTotals{
				Goroutines: 10, Threads: 4, GoroutinesClipped: true,
			}),
			contains: []string{
				"goroutines=10/10+", "threads=4/4",
				"goroutine scan hit its ceiling",
			},
			absent: []string{"threads=4/4+", "thread scan hit its ceiling"},
		},
		{
			name: "thread scan clipped only",
			snap: snap(10, 4, &protocol.SnapshotTotals{
				Goroutines: 10, Threads: 4, ThreadsClipped: true,
			}),
			contains: []string{
				"goroutines=10/10", "threads=4/4+",
				"thread scan hit its ceiling",
			},
			absent: []string{"goroutines=10/10+", "goroutine scan hit its ceiling"},
		},
		{
			name: "both scans clipped",
			snap: snap(10, 4, &protocol.SnapshotTotals{
				Goroutines: 10, Threads: 4, GoroutinesClipped: true, ThreadsClipped: true,
			}),
			contains: []string{
				"goroutines=10/10+", "threads=4/4+",
				"goroutine scan hit its ceiling", "thread scan hit its ceiling",
			},
			absent: []string{"omitted from this event"},
		},
		{
			name: "omission and clipping are reported separately",
			snap: snap(5000, 32, &protocol.SnapshotTotals{
				Goroutines: 8192, Threads: 64, GoroutinesClipped: true,
			}),
			contains: []string{
				"goroutines=5000/8192+", "threads=32/64",
				"3192 goroutines omitted from this event",
				"goroutine scan hit its ceiling",
			},
			absent: []string{"thread scan hit its ceiling"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := countsLine(tc.snap)
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("countsLine missing %q\ngot: %s", want, got)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(got, unwanted) {
					t.Errorf("countsLine unexpectedly contains %q\ngot: %s", unwanted, got)
				}
			}
		})
	}
}
