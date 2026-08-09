package main

import (
	"flag"
	"io"
	"strings"
	"testing"
	"time"

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

func snapshotPushEvent(t *testing.T, seq uint64, current int) protocol.Event {
	t.Helper()

	return protocol.MustEvent(protocol.EventGoroutineSnapshot, seq, protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{{ID: current, Current: true}},
		Current:    current,
	})
}

// The adversarial ordering: another client's snapshot request is rejected while
// the target runs, then the target reaches a stop and pushes a snapshot to
// everyone. EventError carries no requester, so treating it as OUR answer would
// be correlation by kind — the very thing this command stopped doing. A later
// snapshot must win.
func TestRunOnceSucceedsWhenSnapshotFollowsBroadcastRejection(t *testing.T) {
	events := make(chan protocol.Event, 3)
	events <- snapshotErrorEvent(t, 2)
	events <- snapshotPushEvent(t, 3, 7)
	close(events)

	m := &monitor{clients: -1}
	renders := 0
	if err := m.run(events, true, time.Minute, func() { renders++ }); err != nil {
		t.Fatalf("run(-once) failed on an unrelated broadcast rejection: %v", err)
	}
	if renders != 1 {
		t.Fatalf("renders = %d, want exactly 1 (the snapshot)", renders)
	}
	if !m.hasSnapshot || m.snapshot.Current != 7 {
		t.Fatalf("monitor snapshot = %+v, want the pushed one", m.snapshot)
	}
}

// When no snapshot follows, -once must still terminate — bounded by the
// deadline, not by guessing that the broadcast error was ours — and report the
// rejection as context.
func TestRunOnceDeadlineReportsObservedRejection(t *testing.T) {
	events := make(chan protocol.Event, 1)
	events <- snapshotErrorEvent(t, 2)
	// Left open: the stream stays alive, so only the deadline can end the wait.

	m := &monitor{clients: -1}
	err := m.run(events, true, 50*time.Millisecond, func() {})
	if err == nil {
		t.Fatal("run(-once) returned nil despite never receiving a snapshot")
	}
	if !strings.Contains(err.Error(), "no snapshot within") {
		t.Fatalf("run error = %v, want a deadline failure", err)
	}
	if !strings.Contains(err.Error(), "process not suspended") {
		t.Fatalf("run error = %v, want the observed rejection as context", err)
	}
}

// The deadline also bounds a silent server, with no rejection to report.
func TestRunOnceDeadlineWithoutRejection(t *testing.T) {
	events := make(chan protocol.Event)

	m := &monitor{clients: -1}
	err := m.run(events, true, 50*time.Millisecond, func() {})
	if err == nil || !strings.Contains(err.Error(), "no snapshot within") {
		t.Fatalf("run error = %v, want a deadline failure", err)
	}
	if strings.Contains(err.Error(), "rejected") {
		t.Fatalf("run error = %v, want no rejection context when none was seen", err)
	}
}

// A closed stream under -once is a failure too: the promised snapshot never
// arrived, so exiting 0 would report success for empty output.
func TestRunOnceFailsWhenStreamClosesWithoutSnapshot(t *testing.T) {
	events := make(chan protocol.Event, 1)
	events <- snapshotErrorEvent(t, 2)
	close(events)

	m := &monitor{clients: -1}
	err := m.run(events, true, time.Minute, func() {})
	if err == nil || !strings.Contains(err.Error(), "connection closed") {
		t.Fatalf("run error = %v, want a closed-stream failure", err)
	}
	if !strings.Contains(err.Error(), "process not suspended") {
		t.Fatalf("run error = %v, want the observed rejection as context", err)
	}
}

// -once still succeeds on the happy path, rendering exactly the snapshot.
func TestRunOnceRendersFirstSnapshotThenStops(t *testing.T) {
	events := make(chan protocol.Event, 3)
	events <- protocol.MustEvent(protocol.EventSessionState, 2, protocol.SessionStatePayload{
		SessionID: "s1",
		State:     protocol.StateSuspended,
	})
	events <- snapshotPushEvent(t, 3, 1)
	close(events)

	m := &monitor{clients: -1}
	renders := 0
	if err := m.run(events, true, time.Minute, func() { renders++ }); err != nil {
		t.Fatalf("run(-once): %v", err)
	}
	if renders != 1 {
		t.Fatalf("renders = %d, want exactly 1 (the snapshot)", renders)
	}
	if !m.hasSnapshot || m.snapshot.Current != 1 {
		t.Fatalf("monitor snapshot = %+v, want the delivered one", m.snapshot)
	}
}

// Live mode never applies a deadline and never dies on a rejection: it displays
// the reason and keeps observing, because the next stop pushes a snapshot.
func TestRunLiveSurvivesRejectionAndShowsReason(t *testing.T) {
	events := make(chan protocol.Event, 2)
	events <- snapshotErrorEvent(t, 2)
	events <- snapshotPushEvent(t, 3, 1)
	close(events)

	m := &monitor{clients: -1}
	renders := 0
	if err := m.run(events, false, time.Millisecond, func() { renders++ }); err != nil {
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

// The flag package accepts a negative duration, and run's `timeout > 0` guard
// would then treat it exactly like 0 — an unbounded wait, which is the opposite
// of what -timeout documents and what -once needs. Parse through the real flag
// definitions so this cannot drift from main.
func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "defaults with a session", args: []string{"-session", "s1"}},
		{name: "zero waits forever", args: []string{"-session", "s1", "-timeout", "0"}},
		{name: "positive timeout", args: []string{"-session", "s1", "-timeout", "5s"}},
		{
			name:    "negative timeout",
			args:    []string{"-session", "s1", "-timeout", "-30s"},
			wantErr: "-timeout must be >= 0",
		},
		{
			name:    "negative nanosecond",
			args:    []string{"-session", "s1", "-timeout", "-1ns"},
			wantErr: "-timeout must be >= 0",
		},
		{
			name:    "missing session",
			args:    []string{"-once"},
			wantErr: "-session is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("wsmon", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			cfg := bindFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}

			err := cfg.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// A negative duration must never reach run: it would disarm the deadline and
// hang -once forever. This pins the guard the validation protects.
func TestRunTreatsOnlyZeroAsUnbounded(t *testing.T) {
	fs := flag.NewFlagSet("wsmon", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := bindFlags(fs)
	if err := fs.Parse([]string{"-session", "s1", "-once", "-timeout", "-30s"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.timeout >= 0 {
		t.Fatalf("timeout = %s, want the negative value the flag package accepts", cfg.timeout)
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("validate() accepted a negative -timeout, which run would treat as unbounded")
	}
}

// -timeout 0 documents "wait forever", so run must arm no timer at all. A naive
// implementation that always created one would call time.NewTimer(0), which
// fires immediately and would fail this test with "no snapshot within 0s". The
// snapshot is delivered only after a delay far longer than the deadlines the
// other tests use, and the whole thing is bounded so a regression reports a
// failure instead of hanging the suite.
func TestRunOnceZeroTimeoutArmsNoDeadline(t *testing.T) {
	events := make(chan protocol.Event)
	renders := make(chan struct{}, 1)
	result := make(chan error, 1)

	go func() {
		result <- m0().run(events, true, 0, func() { renders <- struct{}{} })
	}()

	select {
	case err := <-result:
		t.Fatalf("run returned early with %v; -timeout 0 must not arm a deadline", err)
	case <-time.After(150 * time.Millisecond):
	}

	events <- snapshotPushEvent(t, 2, 3)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("run(-once, timeout 0): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return after the snapshot arrived")
	}
	if len(renders) != 1 {
		t.Fatalf("renders = %d, want exactly 1", len(renders))
	}
}

// Live mode never arms a deadline either, whatever -timeout says.
func TestRunLiveIgnoresTimeout(t *testing.T) {
	events := make(chan protocol.Event)
	result := make(chan error, 1)

	go func() {
		result <- m0().run(events, false, time.Nanosecond, func() {})
	}()

	select {
	case err := <-result:
		t.Fatalf("live run returned early with %v; -timeout must not apply without -once", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(events)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("live run after stream close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live run did not return after the stream closed")
	}
}

func m0() *monitor { return &monitor{clients: -1} }
