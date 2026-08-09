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
	if !strings.Contains(full, "goroutines: 2  threads: 1  current: G1") {
		t.Fatalf("requested snapshot render = %q, want the full view", full)
	}
	if !strings.Contains(full, "created: [18]") {
		t.Fatalf("requested snapshot render = %q, want the lifecycle deltas", full)
	}
	if fullSnapshotNext.Load() {
		t.Fatal("fullSnapshotNext still armed after one snapshot")
	}

	summary := captureStdout(t, func() { printEvent(testSnapshotEvent(t, 3)) })
	if !strings.Contains(summary, "[goroutines] 2 live, 1 threads, current G1") {
		t.Fatalf("automatic snapshot render = %q, want the summary line", summary)
	}
	if strings.Contains(summary, "goroutines: 2  threads: 1") {
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
