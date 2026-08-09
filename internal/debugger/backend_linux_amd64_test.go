//go:build linux && amd64

package debugger

import "testing"

func TestLinuxBackendTraceTIDDefaultsToPID(t *testing.T) {
	const pid = 1001

	b := &linuxBackend{pid: pid}
	if got := b.traceTID(); got != pid {
		t.Fatalf("traceTID() = %d, want pid %d", got, pid)
	}
}

func TestLinuxBackendTraceTIDUsesLastStoppedTID(t *testing.T) {
	const (
		pid = 1001
		tid = 1002
	)

	b := &linuxBackend{pid: pid}
	b.recordStop(tid)

	if got := b.traceTID(); got != tid {
		t.Fatalf("traceTID() = %d, want stopped tid %d", got, tid)
	}
}

func TestLinuxBackendSetPIDSeedsLastStoppedTID(t *testing.T) {
	const pid = 1001

	b := &linuxBackend{}
	b.setPID(pid)

	if got := b.traceTID(); got != pid {
		t.Fatalf("traceTID() = %d, want pid %d", got, pid)
	}
}

// The tests below pin the part of the wait-side park queue that is specific to
// this backend: WHEN lastStopTID moves. The ordering and gating rules live on
// the platform-neutral stepQueue and are covered in waitpark_test.go.

func TestLinuxBackendParkDoesNotRecordStop(t *testing.T) {
	const (
		pid     = 1001
		stepped = 1002
		foreign = 1003
	)

	b := &linuxBackend{pid: pid}
	b.beginStep(stepped)
	b.recordStop(stepped)

	b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	// Parking must leave the resume target alone: the parked thread is not the
	// thread the engine is working on, and traceTID feeds every TID-less ptrace
	// op the engine issues next.
	if got := b.traceTID(); got != stepped {
		t.Fatalf("traceTID() = %d after parking tid %d, want stepped tid %d", got, foreign, stepped)
	}
}

func TestLinuxBackendDrainParkedRecordsOnDeliveryOnly(t *testing.T) {
	const (
		pid     = 1001
		stepped = 1002
		first   = 1003
		second  = 1004
	)

	b := &linuxBackend{pid: pid}
	b.beginStep(stepped)
	b.recordStop(stepped)
	b.park(StopEvent{Reason: StopBreakpoint, TID: first})
	b.park(StopEvent{Reason: StopSignal, TID: second, Signal: 11})

	if ev, ok := b.drainParked(); ok {
		t.Fatalf("drainParked() released %+v while a step was in flight", ev)
	}
	if got := b.traceTID(); got != stepped {
		t.Fatalf("traceTID() = %d while stepping, want stepped tid %d", got, stepped)
	}

	// The step completing is what releases the queue.
	b.endStep()

	ev, ok := b.drainParked()
	if !ok {
		t.Fatal("drainParked() withheld a stop after the step completed")
	}
	if ev.TID != first {
		t.Fatalf("first drained tid = %d, want %d", ev.TID, first)
	}
	if got := b.traceTID(); got != first {
		t.Fatalf("traceTID() = %d after delivering tid %d, want the delivered thread", got, first)
	}

	ev, ok = b.drainParked()
	if !ok {
		t.Fatal("drainParked() dropped the second held stop")
	}
	if ev.Reason != StopSignal || ev.Signal != 11 {
		t.Fatalf("second drained stop = {%v, signal %d}, want {StopSignal, signal 11}", ev.Reason, ev.Signal)
	}
	if got := b.traceTID(); got != second {
		t.Fatalf("traceTID() = %d, want the delivered thread %d", got, second)
	}
	if _, ok := b.drainParked(); ok {
		t.Fatal("drainParked() returned a stop from an empty queue")
	}
}

// TestLinuxBackendHeldSignalSurfacesOnlyWithItsOwnStop pins the ordering issue
// #204 raises for pending signals. A held stop's signal must not become
// observable before the stop it belongs to, and it must arrive in the same
// operation that repoints traceTID at the signalled thread — otherwise the
// engine would report or suppress a signal against whichever thread happened to
// be the resume target at the time.
//
// The queue is immune to that class by construction rather than by ordering
// discipline: the signal is a field of the parked StopEvent, so there is no
// separate signal record that could be written at park time and later
// overwritten. This test is what stops a future refactor from hoisting the
// signal into backend state, where it would need — and could silently lose —
// that discipline.
func TestLinuxBackendHeldSignalSurfacesOnlyWithItsOwnStop(t *testing.T) {
	const (
		pid      = 2001
		stepped  = 2002
		signaled = 2003
		later    = 2004
	)

	b := &linuxBackend{pid: pid}
	b.beginStep(stepped)
	b.recordStop(stepped)

	b.park(StopEvent{Reason: StopSignal, TID: signaled, Signal: 11})
	b.park(StopEvent{Reason: StopBreakpoint, TID: later})

	// While the step is in flight the signalled thread is invisible: the engine
	// is still working on, and must still resume, the stepped thread.
	if got := b.traceTID(); got != stepped {
		t.Fatalf("traceTID() = %d with a signal held, want stepped tid %d", got, stepped)
	}

	b.endStep()

	ev, ok := b.drainParked()
	if !ok {
		t.Fatal("drainParked() withheld the signal stop after the step completed")
	}
	if ev.TID != signaled || ev.Reason != StopSignal || ev.Signal != 11 {
		t.Fatalf("delivered %+v, want {StopSignal, tid %d, signal 11}", ev, signaled)
	}
	// The signal and the resume target must move together. If traceTID still
	// named the stepped thread here, the engine would handle signalled's stop
	// but resume — or POKEDATA into — the wrong thread.
	if got := b.traceTID(); got != signaled {
		t.Fatalf("traceTID() = %d when delivering the signal, want the signalled thread %d", got, signaled)
	}

	// The next held stop carries no signal, and delivering it must not leave the
	// previous signal attached to it.
	ev, ok = b.drainParked()
	if !ok {
		t.Fatal("drainParked() dropped the stop queued behind the signal")
	}
	if ev.TID != later || ev.Reason != StopBreakpoint || ev.Signal != 0 {
		t.Fatalf("delivered %+v, want {StopBreakpoint, tid %d, signal 0}", ev, later)
	}
}

// TestLinuxBackendWaitDrainsBeforeBlocking pins the wiring: Wait must consult
// the queue at the top of its loop, before blocking in wait4, so a held stop
// becomes the engine's next stop as soon as the step it collided with is done.
func TestLinuxBackendWaitDrainsBeforeBlocking(t *testing.T) {
	const (
		pid     = 1001
		foreign = 1003
	)

	b := &linuxBackend{pid: pid}
	b.recordStop(pid)
	b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	ev, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if ev.TID != foreign || ev.Reason != StopBreakpoint {
		t.Fatalf("Wait() = %+v, want the held breakpoint on tid %d", ev, foreign)
	}
	if got := b.traceTID(); got != foreign {
		t.Fatalf("traceTID() = %d, want the delivered thread %d", got, foreign)
	}
}
