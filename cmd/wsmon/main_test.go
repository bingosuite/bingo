package main

import (
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
