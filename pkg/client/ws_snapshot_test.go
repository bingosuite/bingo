package client

import (
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func snapshotEvent(t *testing.T, seq uint64, current int, created []int) protocol.Event {
	t.Helper()

	return protocol.MustEvent(protocol.EventGoroutineSnapshot, seq, protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{{ID: current, Current: true}},
		Current:    current,
		Created:    created,
	})
}

func awaitSnapshot(t *testing.T, events <-chan protocol.Event) protocol.GoroutineSnapshotPayload {
	t.Helper()

	evt := awaitEvent(t, events)
	if evt.Kind != protocol.EventGoroutineSnapshot {
		t.Fatalf("event kind = %s, want %s", evt.Kind, protocol.EventGoroutineSnapshot)
	}
	var p protocol.GoroutineSnapshotPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return p
}

func pendingCount(c *wsClient) int {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return len(c.pending)
}

// The request must return as soon as the command is on the wire — before any
// reply — and register nothing that a later event could be matched against.
func TestRequestGoroutineSnapshotReturnsAfterWireWrite(t *testing.T) {
	useSyncTimeout(t, 100*time.Millisecond)
	h := newLoopbackClient(t)

	done := make(chan error, 1)
	go func() { done <- h.client.RequestGoroutineSnapshot() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RequestGoroutineSnapshot: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RequestGoroutineSnapshot did not return before its reply")
	}
	assertCommandKind(t, h.readCommand(t), protocol.CmdGoroutineSnapshot)
	if got := pendingCount(h.client); got != 0 {
		t.Fatalf("pending entries after request = %d, want 0", got)
	}

	// Long past any synchronous timeout, the reply still arrives as an event.
	time.Sleep(2 * syncTimeout)
	h.writeEvent(t, snapshotEvent(t, 2, 7, nil))
	if snap := awaitSnapshot(t, h.client.Events()); snap.Current != 7 {
		t.Fatalf("snapshot current = %d, want 7", snap.Current)
	}
	if got := pendingCount(h.client); got != 0 {
		t.Fatalf("pending entries after reply = %d, want 0", got)
	}
}

// Automatic pushes around a requested snapshot must all be delivered, in order,
// with none consumed as a reply and none duplicated.
func TestAutomaticSnapshotsAroundRequestAllReachEvents(t *testing.T) {
	h := newLoopbackClient(t)

	h.writeEvent(t, snapshotEvent(t, 2, 1, nil)) // automatic: entry stop
	if err := h.client.RequestGoroutineSnapshot(); err != nil {
		t.Fatalf("RequestGoroutineSnapshot: %v", err)
	}
	assertCommandKind(t, h.readCommand(t), protocol.CmdGoroutineSnapshot)
	h.writeEvent(t, snapshotEvent(t, 3, 2, nil))      // the query's reply
	h.writeEvent(t, snapshotEvent(t, 4, 3, []int{3})) // automatic: breakpoint hit

	for _, want := range []int{1, 2, 3} {
		if snap := awaitSnapshot(t, h.client.Events()); snap.Current != want {
			t.Fatalf("snapshot current = %d, want %d", snap.Current, want)
		}
	}
	if got := pendingCount(h.client); got != 0 {
		t.Fatalf("pending entries = %d, want 0", got)
	}
}

// Reply debt from an unrelated timed-out synchronous call must never claim a
// snapshot: snapshots are not correlated confirmations.
func TestReplyDebtDoesNotSwallowAutomaticSnapshot(t *testing.T) {
	useSyncTimeout(t, 100*time.Millisecond)
	h := newLoopbackClient(t)

	first := callSetBreakpoint(h.client, "first.go", 10)
	assertCommandKind(t, h.readCommand(t), protocol.CmdSetBreakpoint)
	if result := awaitBreakpoint(t, first); result.err == nil {
		t.Fatal("first SetBreakpoint: want timeout")
	}
	if got := pendingCount(h.client); got != 1 {
		t.Fatalf("retired reply debt = %d entries, want 1", got)
	}

	h.writeEvent(t, snapshotEvent(t, 2, 5, []int{5}))
	snap := awaitSnapshot(t, h.client.Events())
	if snap.Current != 5 || len(snap.Created) != 1 {
		t.Fatalf("snapshot = %+v, want the automatic push intact", snap)
	}

	// The debt is still outstanding and still owned by SetBreakpoint alone.
	if got := pendingCount(h.client); got != 1 {
		t.Fatalf("reply debt after snapshot = %d entries, want it untouched at 1", got)
	}
	h.writeEvent(t, protocol.MustEvent(protocol.EventBreakpointSet, 3, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 1, Location: protocol.Location{File: "first.go", Line: 10}},
	}))
	deadline := time.Now().Add(2 * time.Second)
	for pendingCount(h.client) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("stale SetBreakpoint confirmation never consumed its reply debt")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A snapshot request must not disturb an in-flight synchronous call of another
// kind, and an automatic push landing in between must not satisfy it.
func TestSnapshotTrafficDoesNotDisturbSynchronousCall(t *testing.T) {
	h := newLoopbackClient(t)

	result := callSetBreakpoint(h.client, "main.go", 42)
	assertCommandKind(t, h.readCommand(t), protocol.CmdSetBreakpoint)

	if err := h.client.RequestGoroutineSnapshot(); err != nil {
		t.Fatalf("RequestGoroutineSnapshot: %v", err)
	}
	assertCommandKind(t, h.readCommand(t), protocol.CmdGoroutineSnapshot)
	h.writeEvent(t, snapshotEvent(t, 2, 9, nil))
	h.writeEvent(t, protocol.MustEvent(protocol.EventBreakpointSet, 3, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 4, Location: protocol.Location{File: "main.go", Line: 42}},
	}))

	got := awaitBreakpoint(t, result)
	if got.err != nil {
		t.Fatalf("SetBreakpoint: %v", got.err)
	}
	if got.breakpoint.ID != 4 {
		t.Fatalf("breakpoint = %+v, want its own confirmation", got.breakpoint)
	}
	if snap := awaitSnapshot(t, h.client.Events()); snap.Current != 9 {
		t.Fatalf("snapshot current = %d, want 9", snap.Current)
	}
}
