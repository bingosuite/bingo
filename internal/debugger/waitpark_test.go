package debugger

import (
	"errors"
	"testing"
)

// errFake stands in for any backend failure the recovery path must tolerate.
var errFake = errors.New("fake backend failure")

// TestClassifyUserStop pins the park/surface decision the linux wait loop makes
// for every combination of step state and stopping thread. It is the pure core
// of the issue #199 fix: a foreign user-visible stop must never be surfaced
// while a single-step is in flight, and the stepped thread's own stops must
// never be withheld.
func TestClassifyUserStop(t *testing.T) {
	const (
		stepped = 11
		foreign = 22
	)

	cases := []struct {
		name       string
		trap       bool
		stepping   bool
		stepTID    int
		tid        int
		wantReason StopReason
		wantDisp   stopDisposition
	}{
		{
			name: "no step: trap is a breakpoint and surfaces",
			trap: true, stepping: false, stepTID: 0, tid: foreign,
			wantReason: StopBreakpoint, wantDisp: surfaceStop,
		},
		{
			name: "no step: signal surfaces",
			trap: false, stepping: false, stepTID: 0, tid: foreign,
			wantReason: StopSignal, wantDisp: surfaceStop,
		},
		{
			name: "stepped thread's trap is the step completing",
			trap: true, stepping: true, stepTID: stepped, tid: stepped,
			wantReason: StopSingleStep, wantDisp: surfaceStop,
		},
		{
			name: "stepped thread's signal surfaces so the engine can reinstall",
			trap: false, stepping: true, stepTID: stepped, tid: stepped,
			wantReason: StopSignal, wantDisp: surfaceStop,
		},
		{
			name: "foreign trap during a step is parked as a breakpoint",
			trap: true, stepping: true, stepTID: stepped, tid: foreign,
			wantReason: StopBreakpoint, wantDisp: parkStop,
		},
		{
			name: "foreign signal during a step is parked",
			trap: false, stepping: true, stepTID: stepped, tid: foreign,
			wantReason: StopSignal, wantDisp: parkStop,
		},
		{
			// Parking against a step we cannot recognise the completion of
			// would hang the wait loop forever, so an unidentified step is
			// treated as no step at all.
			name: "stepping with no stepTID never parks",
			trap: true, stepping: true, stepTID: 0, tid: foreign,
			wantReason: StopBreakpoint, wantDisp: surfaceStop,
		},
		{
			name: "stepping with no stepTID surfaces signals too",
			trap: false, stepping: true, stepTID: 0, tid: foreign,
			wantReason: StopSignal, wantDisp: surfaceStop,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, disp := classifyUserStop(tc.trap, tc.stepping, tc.stepTID, tc.tid)
			if reason != tc.wantReason {
				t.Errorf("reason = %v, want %v", reason, tc.wantReason)
			}
			if disp != tc.wantDisp {
				t.Errorf("disposition = %v, want %v", disp, tc.wantDisp)
			}
		})
	}
}

// TestClassifyUserStopNeverParksSteppedThread is the invariant behind the
// completion guarantee: whatever the stepped thread reports has to reach the
// engine, or the step never completes and nothing can ever be drained.
func TestClassifyUserStopNeverParksSteppedThread(t *testing.T) {
	for _, trap := range []bool{true, false} {
		for _, tid := range []int{1, 7, 4096} {
			if _, disp := classifyUserStop(trap, true, tid, tid); disp != surfaceStop {
				t.Errorf("trap=%v tid=%d: stepped thread was parked", trap, tid)
			}
		}
	}
}

// TestStepQueueHoldsUntilStepCompletes is the core gating rule: a stop parked
// during a step must not be released until the step is over, because the engine
// reinstalls the trap it stepped off as part of handling the step's completion.
// Releasing early is what lets a same-address sibling be seen while no
// breakpoint is registered at that address (issue #199).
func TestStepQueueHoldsUntilStepCompletes(t *testing.T) {
	const (
		stepped = 11
		foreign = 22
	)

	var q stepQueue
	q.beginStep(stepped)
	q.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	if ev, ok := q.releasable(); ok {
		t.Fatalf("releasable() returned %+v while a step was in flight", ev)
	}
	if len(q.parked) != 1 {
		t.Fatalf("len(parked) = %d, want the held stop retained", len(q.parked))
	}

	q.endStep()

	ev, ok := q.releasable()
	if !ok {
		t.Fatal("releasable() withheld a stop after the step completed")
	}
	if ev.TID != foreign || ev.Reason != StopBreakpoint {
		t.Fatalf("released %+v, want the breakpoint on tid %d", ev, foreign)
	}
}

func TestStepQueueReleasesFIFO(t *testing.T) {
	var q stepQueue
	q.beginStep(11)
	for _, tid := range []int{21, 22, 23} {
		q.park(StopEvent{Reason: StopBreakpoint, TID: tid})
	}
	q.endStep()

	for _, want := range []int{21, 22, 23} {
		ev, ok := q.releasable()
		if !ok {
			t.Fatalf("releasable() ran dry before tid %d", want)
		}
		if ev.TID != want {
			t.Fatalf("released tid %d, want %d — delivery is not FIFO", ev.TID, want)
		}
	}
	if ev, ok := q.releasable(); ok {
		t.Fatalf("releasable() returned %+v from an empty queue", ev)
	}
}

// TestStepQueuePreservesEveryStopField pins that a held stop is delivered whole.
// The signal in particular travels inside the parked StopEvent rather than in
// backend state, which is why the queue has no pending-signal ordering problem
// to solve (see issue #204): there is no separate signal record that could be
// written when the stop is parked and read against a different thread later.
// If a refactor ever moves the signal out of the event, this test fails.
func TestStepQueuePreservesEveryStopField(t *testing.T) {
	want := []StopEvent{
		{Reason: StopSignal, TID: 21, Signal: 11},
		{Reason: StopBreakpoint, TID: 22},
		{Reason: StopSignal, TID: 23, Signal: 17},
	}

	var q stepQueue
	q.beginStep(11)
	for _, ev := range want {
		q.park(ev)
	}
	q.endStep()

	for i, w := range want {
		got, ok := q.releasable()
		if !ok {
			t.Fatalf("releasable() ran dry at index %d", i)
		}
		if got != w {
			t.Fatalf("released %+v at index %d, want %+v", got, i, w)
		}
	}
}

func TestStepQueuePurgeDiscardsHeldStops(t *testing.T) {
	var q stepQueue
	q.park(StopEvent{Reason: StopBreakpoint, TID: 21})
	q.park(StopEvent{Reason: StopSignal, TID: 22, Signal: 5})

	q.purge()

	if len(q.parked) != 0 {
		t.Fatalf("len(parked) = %d after purge, want 0", len(q.parked))
	}
	if ev, ok := q.releasable(); ok {
		t.Fatalf("releasable() returned %+v after purge — a dead thread would be acted on", ev)
	}
}

// TestStepQueueSteppedThreadDeathLiftsTheGate covers the liveness hole: if the
// thread being stepped dies, its completion can never arrive, so without
// clearing the step bookkeeping the held stops are stranded and the wait loop
// blocks forever.
func TestStepQueueSteppedThreadDeathLiftsTheGate(t *testing.T) {
	const (
		stepped = 11
		other   = 12
		foreign = 22
	)

	var q stepQueue
	q.beginStep(stepped)
	q.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	// An unrelated thread dying changes nothing.
	q.clearStepIfStepped(other)
	if !q.stepping || q.stepTID != stepped {
		t.Fatalf("stepping=%v stepTID=%d after an unrelated exit, want the step untouched", q.stepping, q.stepTID)
	}
	if _, ok := q.releasable(); ok {
		t.Fatal("releasable() released a stop while the step was still in flight")
	}

	q.clearStepIfStepped(stepped)
	if q.stepping || q.stepTID != 0 {
		t.Fatalf("stepping=%v stepTID=%d after the stepped thread died, want cleared", q.stepping, q.stepTID)
	}
	if _, ok := q.releasable(); !ok {
		t.Fatal("releasable() withheld a stop after the stepped thread died — the queue is stranded")
	}
}

func TestStepQueueEmptyIsInert(t *testing.T) {
	var q stepQueue
	if ev, ok := q.releasable(); ok {
		t.Fatalf("releasable() = %+v on a fresh queue, want nothing", ev)
	}
	q.endStep()
	q.clearStepIfStepped(1)
	q.purge()
	if ev, ok := q.releasable(); ok {
		t.Fatalf("releasable() = %+v, want nothing", ev)
	}
}

// TestStepQueueCountsEveryPark pins the counter the native overlap regression
// asserts on. If it stopped advancing, that spec would silently go vacuous
// rather than fail, which is exactly the hole it exists to close.
func TestStepQueueCountsEveryPark(t *testing.T) {
	var q stepQueue
	if got := q.parkedCount(); got != 0 {
		t.Fatalf("fresh queue: parkedCount = %d, want 0", got)
	}

	q.beginStep(11)
	q.park(StopEvent{Reason: StopBreakpoint, TID: 22})
	q.park(StopEvent{Reason: StopSignal, TID: 33})
	if got := q.parkedCount(); got != 2 {
		t.Fatalf("after two parks: parkedCount = %d, want 2", got)
	}

	// The count is cumulative: draining and purging describe what is still
	// held, not what was ever held, so neither may roll it back.
	q.endStep()
	if _, ok := q.releasable(); !ok {
		t.Fatal("expected a released stop")
	}
	q.purge()
	if got := q.parkedCount(); got != 2 {
		t.Fatalf("after drain and purge: parkedCount = %d, want 2 (cumulative)", got)
	}
}

// TestStepQueueCountsHeldSignalsSeparately pins the observable the native pause
// spec relies on: a held asynchronous interrupt must be distinguishable from a
// held sibling trap. Without the split counter the spec cannot tell "Pause
// raced an in-flight step" from "a sibling happened to trap during one".
func TestStepQueueCountsHeldSignalsSeparately(t *testing.T) {
	const (
		stepped = 41
		foreign = 42
	)

	var q stepQueue
	q.beginStep(stepped)

	if got := q.parkedSignalCount(); got != 0 {
		t.Fatalf("parkedSignalCount() = %d on a fresh queue, want 0", got)
	}

	q.park(StopEvent{Reason: StopBreakpoint, TID: foreign})
	if got := q.parkedSignalCount(); got != 0 {
		t.Fatalf("parkedSignalCount() = %d after a held breakpoint, want 0 — a trap must not count as an interrupt", got)
	}
	if got := q.parkedCount(); got != 1 {
		t.Fatalf("parkedCount() = %d, want 1", got)
	}

	q.park(StopEvent{Reason: StopSignal, TID: foreign, Signal: 19})
	if got := q.parkedSignalCount(); got != 1 {
		t.Fatalf("parkedSignalCount() = %d after a held signal, want 1", got)
	}
	if got := q.parkedCount(); got != 2 {
		t.Fatalf("parkedCount() = %d, want 2 — the total must still count both", got)
	}

	// Counters are cumulative: draining and purging must not roll them back, or
	// the spec's non-vaciuty gate could be reset to zero by ordinary progress.
	q.endStep()
	if _, ok := q.releasable(); !ok {
		t.Fatal("releasable() withheld a stop after the step ended")
	}
	q.purge()
	if got := q.parkedSignalCount(); got != 1 {
		t.Fatalf("parkedSignalCount() = %d after drain+purge, want the cumulative 1", got)
	}
}

// fakeRestarter is a trapRestarter over an in-memory tracee.
type fakeRestarter struct {
	pc       map[int]uint64
	mem      map[uint64]byte
	resumed  []int
	getErr   error
	setErr   error
	readErr  error
	contErr  error
	setCalls int
}

func (f *fakeRestarter) GetRegisters(tid int) (Registers, error) {
	if f.getErr != nil {
		return Registers{}, f.getErr
	}
	return Registers{PC: f.pc[tid]}, nil
}

func (f *fakeRestarter) SetRegisters(tid int, reg Registers) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls++
	f.pc[tid] = reg.PC
	return nil
}

func (f *fakeRestarter) ReadMemory(addr uint64, dst []byte) error {
	if f.readErr != nil {
		return f.readErr
	}
	for i := range dst {
		dst[i] = f.mem[addr+uint64(i)]
	}
	return nil
}

func (f *fakeRestarter) continueIfTraceeExists(tid int, signal int) error {
	f.resumed = append(f.resumed, tid)
	return f.contErr
}

// rewindOne models an architecture that advances the PC past a one-byte trap.
func rewindOne(pc uint64) uint64 { return pc - 1 }

const testTrapByte = 0xCC

// TestRestartStaleParkedDropsRemovedTrap pins the recovery that keeps a held
// stop from resuming its thread mid-instruction once the engine has cleared the
// trap the stop refers to.
func TestRestartStaleParkedDropsRemovedTrap(t *testing.T) {
	trap := []byte{testTrapByte}

	t.Run("trap removed while parked restarts the instruction", func(t *testing.T) {
		f := &fakeRestarter{
			pc:  map[int]uint64{7: 0x1001},
			mem: map[uint64]byte{0x1000: 0x48}, // original byte is back
		}
		var q stepQueue
		if !q.restartStaleParked(f, StopEvent{TID: 7, Reason: StopBreakpoint}, trap, rewindOne) {
			t.Fatal("stale stop was delivered instead of dropped")
		}
		if got := f.pc[7]; got != 0x1000 {
			t.Fatalf("PC = 0x%x, want the instruction restarted at 0x1000", got)
		}
		if len(f.resumed) != 1 || f.resumed[0] != 7 {
			t.Fatalf("resumed = %v, want the parked thread resumed once", f.resumed)
		}
		if q.staleParkedCount() != 1 {
			t.Fatalf("staleParkedCount = %d, want 1", q.staleParkedCount())
		}
	})

	t.Run("trap still armed is delivered untouched", func(t *testing.T) {
		f := &fakeRestarter{
			pc:  map[int]uint64{7: 0x1001},
			mem: map[uint64]byte{0x1000: testTrapByte},
		}
		var q stepQueue
		if q.restartStaleParked(f, StopEvent{TID: 7, Reason: StopBreakpoint}, trap, rewindOne) {
			t.Fatal("a live trap must still be delivered to the engine")
		}
		if f.setCalls != 0 || len(f.resumed) != 0 {
			t.Fatalf("backend was mutated: setCalls=%d resumed=%v", f.setCalls, f.resumed)
		}
		if q.staleParkedCount() != 0 {
			t.Fatalf("staleParkedCount = %d, want 0", q.staleParkedCount())
		}
	})

	t.Run("signal stops are never restarted", func(t *testing.T) {
		f := &fakeRestarter{pc: map[int]uint64{7: 0x1001}, mem: map[uint64]byte{}}
		var q stepQueue
		if q.restartStaleParked(f, StopEvent{TID: 7, Reason: StopSignal}, trap, rewindOne) {
			t.Fatal("a held signal has no trap to go stale")
		}
	})

	t.Run("identity rewind never restarts", func(t *testing.T) {
		f := &fakeRestarter{pc: map[int]uint64{7: 0x1000}, mem: map[uint64]byte{}}
		var q stepQueue
		same := func(pc uint64) uint64 { return pc }
		if q.restartStaleParked(f, StopEvent{TID: 7, Reason: StopBreakpoint}, trap, same) {
			t.Fatal("an architecture that stops AT the trap re-executes it safely")
		}
	})

	t.Run("failures leave the stop deliverable", func(t *testing.T) {
		for name, f := range map[string]*fakeRestarter{
			"registers unreadable": {pc: map[int]uint64{7: 0x1001}, mem: map[uint64]byte{}, getErr: errFake},
			"memory unreadable":    {pc: map[int]uint64{7: 0x1001}, mem: map[uint64]byte{}, readErr: errFake},
			"registers unwritable": {pc: map[int]uint64{7: 0x1001}, mem: map[uint64]byte{0x1000: 0x48}, setErr: errFake},
		} {
			var q stepQueue
			if q.restartStaleParked(f, StopEvent{TID: 7, Reason: StopBreakpoint}, trap, rewindOne) {
				t.Fatalf("%s: dropped the stop instead of falling back to delivery", name)
			}
			if q.staleParkedCount() != 0 {
				t.Fatalf("%s: counted a drop that did not happen", name)
			}
		}
	})

	t.Run("a dead thread is still dropped", func(t *testing.T) {
		f := &fakeRestarter{
			pc:      map[int]uint64{7: 0x1001},
			mem:     map[uint64]byte{0x1000: 0x48},
			contErr: errFake,
		}
		var q stepQueue
		if !q.restartStaleParked(f, StopEvent{TID: 7, Reason: StopBreakpoint}, trap, rewindOne) {
			t.Fatal("a stop whose thread is gone must not reach the engine")
		}
	})
}
