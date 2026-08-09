package main

import (
	"strings"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// wsmon no longer blocks on a snapshot reply: its request is fire-and-forget and
// every snapshot — requested or automatic — arrives on the event stream. This
// pins the stream path that both the live redraw and -once now depend on.
func TestApplyEventRendersSnapshotsFromTheEventStream(t *testing.T) {
	m := &monitor{clients: -1, lastEvent: "joined"}

	evt := protocol.MustEvent(protocol.EventGoroutineSnapshot, 2, protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{{ID: 1, Current: true}, {ID: 9, ParentID: 1}},
		Threads:    []protocol.Thread{{ID: 7, GoID: 1, Current: true}},
		Current:    1,
		Created:    []int{9},
	})

	renderedSnapshot, redraw := m.applyEvent(evt)
	if !renderedSnapshot || !redraw {
		t.Fatalf("applyEvent(snapshot) = (%v, %v), want (true, true)", renderedSnapshot, redraw)
	}
	if !m.hasSnapshot || m.snapshot.Current != 1 || len(m.snapshot.Goroutines) != 2 {
		t.Fatalf("monitor snapshot = %+v, want the streamed payload", m.snapshot)
	}

	// A later automatic snapshot replaces it; nothing is consumed as a reply.
	next := protocol.MustEvent(protocol.EventGoroutineSnapshot, 3, protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{{ID: 1, Current: true}},
		Current:    1,
		Exited:     []int{9},
	})
	if renderedSnapshot, redraw = m.applyEvent(next); !renderedSnapshot || !redraw {
		t.Fatalf("applyEvent(second snapshot) = (%v, %v), want (true, true)", renderedSnapshot, redraw)
	}
	if len(m.snapshot.Exited) != 1 || m.snapshot.Exited[0] != 9 {
		t.Fatalf("monitor exited delta = %v, want [9]", m.snapshot.Exited)
	}
}

// Only snapshots satisfy -once; other traffic redraws at most.
func TestApplyEventOnlySnapshotsCountAsRendered(t *testing.T) {
	m := &monitor{clients: -1}

	state := protocol.MustEvent(protocol.EventSessionState, 2, protocol.SessionStatePayload{
		SessionID: "s1",
		State:     protocol.StateSuspended,
		Clients:   2,
	})
	if renderedSnapshot, redraw := m.applyEvent(state); renderedSnapshot || !redraw {
		t.Fatalf("applyEvent(sessionState) = (%v, %v), want (false, true)", renderedSnapshot, redraw)
	}
	if m.state != protocol.StateSuspended || m.clients != 2 {
		t.Fatalf("monitor state = %s clients = %d, want suspended/2", m.state, m.clients)
	}

	continued := protocol.MustEvent(protocol.EventContinued, 3, protocol.ContinuedPayload{})
	if renderedSnapshot, redraw := m.applyEvent(continued); renderedSnapshot || redraw {
		t.Fatalf("applyEvent(continued) = (%v, %v), want (false, false)", renderedSnapshot, redraw)
	}
	if m.hasSnapshot {
		t.Fatal("monitor recorded a snapshot it never received")
	}
}

func snapshotErrorEvent(t *testing.T, seq uint64) protocol.Event {
	t.Helper()

	return protocol.MustEvent(protocol.EventError, seq, protocol.ErrorPayload{
		Command: protocol.CmdGoroutineSnapshot,
		Message: "process not suspended",
	})
}

// -once used to wait forever when the server rejected the request: the reply it
// was blocked on never came and EventError was ignored. It must now fail fast
// with the server's reason so the caller can exit nonzero.
func TestRunOnceFailsOnRejectedSnapshotRequest(t *testing.T) {
	events := make(chan protocol.Event, 2)
	events <- snapshotErrorEvent(t, 2)
	close(events)

	m := &monitor{clients: -1}
	renders := 0
	err := m.run(events, true, func() { renders++ })
	if err == nil {
		t.Fatal("run(-once) returned nil for a rejected snapshot request")
	}
	if !strings.Contains(err.Error(), "process not suspended") {
		t.Fatalf("run error = %v, want the server's reason", err)
	}
	if renders != 0 {
		t.Fatalf("renders = %d, want 0 (nothing to draw)", renders)
	}
}

// A closed stream under -once is also a failure: the promised snapshot never
// arrived, so exiting 0 would report success for empty output.
func TestRunOnceFailsWhenStreamClosesWithoutSnapshot(t *testing.T) {
	events := make(chan protocol.Event)
	close(events)

	m := &monitor{clients: -1}
	err := m.run(events, true, func() {})
	if err == nil || !strings.Contains(err.Error(), "connection closed") {
		t.Fatalf("run error = %v, want a closed-stream failure", err)
	}
}

// -once still succeeds on the happy path, rendering exactly the snapshot.
func TestRunOnceRendersFirstSnapshotThenStops(t *testing.T) {
	events := make(chan protocol.Event, 3)
	events <- protocol.MustEvent(protocol.EventSessionState, 2, protocol.SessionStatePayload{
		SessionID: "s1",
		State:     protocol.StateSuspended,
	})
	events <- protocol.MustEvent(protocol.EventGoroutineSnapshot, 3, protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{{ID: 1, Current: true}},
		Current:    1,
	})
	close(events)

	m := &monitor{clients: -1}
	renders := 0
	if err := m.run(events, true, func() { renders++ }); err != nil {
		t.Fatalf("run(-once): %v", err)
	}
	if renders != 1 {
		t.Fatalf("renders = %d, want exactly 1 (the snapshot)", renders)
	}
	if !m.hasSnapshot || m.snapshot.Current != 1 {
		t.Fatalf("monitor snapshot = %+v, want the delivered one", m.snapshot)
	}
}

// Live mode must not die on a rejection — the next stop pushes a snapshot on its
// own — but it must surface the reason instead of swallowing it.
func TestRunLiveSurvivesRejectionAndShowsReason(t *testing.T) {
	events := make(chan protocol.Event, 2)
	events <- snapshotErrorEvent(t, 2)
	events <- protocol.MustEvent(protocol.EventGoroutineSnapshot, 3, protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{{ID: 1, Current: true}},
		Current:    1,
	})
	close(events)

	m := &monitor{clients: -1}
	renders := 0
	if err := m.run(events, false, func() { renders++ }); err != nil {
		t.Fatalf("run(live): %v", err)
	}
	if renders != 2 {
		t.Fatalf("renders = %d, want 2 (the error line and the snapshot)", renders)
	}
	if !strings.Contains(m.lastEvent, "process not suspended") {
		t.Fatalf("lastEvent = %q, want the rejection reason", m.lastEvent)
	}
	if !m.hasSnapshot {
		t.Fatal("live mode dropped the snapshot that followed the rejection")
	}
}
