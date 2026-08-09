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
			name: "runtime clipping marks only the goroutine total as a floor",
			snap: snap(10, 4, &protocol.SnapshotTotals{Goroutines: 10, Threads: 4, Clipped: true}),
			contains: []string{
				"goroutines=10/10+", "threads=4/4",
				"scan hit its ceiling", "lower bound",
			},
			absent: []string{"threads=4/4+", "omitted from this event"},
		},
		{
			name: "both causes are reported separately",
			snap: snap(5000, 32, &protocol.SnapshotTotals{Goroutines: 8192, Threads: 64, Clipped: true}),
			contains: []string{
				"goroutines=5000/8192+",
				"3192 goroutines omitted from this event",
				"scan hit its ceiling",
			},
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
