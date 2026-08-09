package main

import (
	"strings"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// The streamed snapshot line is the one a user sees without asking, so it is the
// one most likely to be believed. Rendering a trimmed subset as the live
// population there — "5000 live" when 41,203 exist — is the exact dishonesty
// issue #194 is about, and it is not fixed by annotating the explicit commands.
func TestCountOfNeverPresentsATrimmedSubsetAsTheWhole(t *testing.T) {
	tests := []struct {
		name     string
		shown    int
		total    totalCount
		want     string
		unwanted string
	}{
		{
			name:  "no totals means nothing was omitted",
			shown: 12,
			total: totalCount{},
			want:  "12 live goroutines",
		},
		{
			name:  "the noun agrees with the count being reported",
			shown: 1,
			total: totalCount{},
			want:  "1 live goroutine",
		},
		{
			name:  "omission is stated as a fraction of the original",
			shown: 5000,
			total: totalCount{count: 41203},
			want:  "5000 of 41203 live goroutines",
		},
		{
			// The total is itself a floor here, so "of 8192" would read as an
			// exact census of something the debugger never finished counting.
			name:  "a clipped scan makes the total a lower bound",
			shown: 5000,
			total: totalCount{count: 8192, clipped: true},
			want:  "5000 of at least 8192 live goroutines",
		},
		{
			name:  "a clipped scan that omitted nothing still marks the count",
			shown: 8192,
			total: totalCount{count: 8192, clipped: true},
			want:  "8192+ live goroutines",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := countOf(tc.shown, "live goroutine", tc.total)
			if got != tc.want {
				t.Fatalf("countOf = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestPrintTotalsSaysNothingWhenNothingIsMissing(t *testing.T) {
	// Totals is present only on an incomplete result, so the common case must
	// stay silent: a warning on every snapshot trains users to ignore it.
	if totalsGoroutines(nil) != (totalCount{}) || totalsThreads(nil) != (totalCount{}) {
		t.Fatal("an absent totals block must not fabricate counts")
	}
}

// The goroutine-only payload carries no thread collection, so its totals must
// never be read as "every thread was omitted".
func TestGoroutineListTotalsCarryNoThreadClaim(t *testing.T) {
	packed, _ := protocol.PackGoroutines([]protocol.Goroutine{
		{ID: 1, Status: "running", Current: true},
	}, true)
	if packed.Totals == nil {
		t.Fatal("a clipped scan must attach totals")
	}
	if packed.Totals.Threads != 0 || packed.Totals.ThreadsClipped {
		t.Fatalf("Totals = %+v; the flat list shape has no threads to report", packed.Totals)
	}
	threads := totalsThreads(packed.Totals)
	if got := countOf(0, "thread", threads); strings.Contains(got, "of") {
		t.Errorf("countOf = %q; an absent collection must not read as an omission", got)
	}
}
