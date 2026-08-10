package debugger

import "testing"

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
// to solve (see issue #206): the per-TID pending state is installed only after
// this queue releases the event for delivery. If a refactor ever moves the
// signal out of the parked event, this test fails.
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

// TestStepQueueSteppedThreadDeathRequiresReconciliation pins the transaction
// boundary: hardware-step ownership ends with the thread, but a held sibling
// remains gated until the engine acknowledges that the removed trap is back.
func TestStepQueueSteppedThreadDeathRequiresReconciliation(t *testing.T) {
	const (
		stepped = 11
		other   = 12
		foreign = 22
	)

	var q stepQueue
	q.beginStep(stepped)
	q.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	// An unrelated thread dying changes nothing.
	q.interruptStepIfStepped(other)
	if !q.stepping || q.stepTID != stepped {
		t.Fatalf("stepping=%v stepTID=%d after an unrelated exit, want the step untouched", q.stepping, q.stepTID)
	}
	if _, ok := q.releasable(); ok {
		t.Fatal("releasable() released a stop while the step was still in flight")
	}

	q.interruptStepIfStepped(stepped)
	if q.stepping || q.stepTID != 0 {
		t.Fatalf("stepping=%v stepTID=%d after the stepped thread died, want cleared", q.stepping, q.stepTID)
	}
	if _, ok := q.releasable(); ok {
		t.Fatal("releasable() exposed a sibling before engine reconciliation")
	}
	boundary, ok := q.stepExitBoundary()
	if !ok || boundary.Reason != StopStepThreadExited || boundary.TID != foreign {
		t.Fatalf("stepExitBoundary() = (%+v, %v), want a boundary anchored to tid %d", boundary, ok, foreign)
	}
	if q.parkedDepthForTest() != 1 {
		t.Fatal("the boundary dequeued the sibling stop before reconciliation")
	}
	if _, ok := q.stepExitBoundary(); ok {
		t.Fatal("stepExitBoundary() reported the same transaction twice")
	}
	q.completeStepThreadExit()
	if ev, ok := q.releasable(); !ok || ev.TID != foreign {
		t.Fatalf("releasable() after reconciliation = (%+v, %v), want tid %d", ev, ok, foreign)
	}
}

// TestStepQueueAnchorsOnTheHeldOwnerWhenNothingIsParked is the empty-queue half
// of the same boundary. The dying owner is retained at its exit stop and becomes
// the anchor itself, so the reinstall no longer waits for an unrelated thread to
// stop — which, before this, could be arbitrarily long or never.
func TestStepQueueAnchorsOnTheHeldOwnerWhenNothingIsParked(t *testing.T) {
	const stepped = 31

	var q stepQueue
	q.beginStep(stepped)
	q.interruptStepIfStepped(stepped)
	if _, ok := q.stepExitBoundary(); ok {
		t.Fatal("stepExitBoundary() reported a boundary with no anchor at all")
	}

	q.holdStepOwner(stepped)
	boundary, ok := q.stepExitBoundary()
	if !ok || boundary.Reason != StopStepThreadExited || boundary.TID != stepped {
		t.Fatalf("stepExitBoundary() = (%+v, %v), want a boundary anchored to the held owner %d",
			boundary, ok, stepped)
	}
	if _, ok := q.stepExitBoundary(); ok {
		t.Fatal("stepExitBoundary() reported the same transaction twice")
	}
	if tid, held := q.heldStepOwner(); !held || tid != stepped {
		t.Fatalf("heldStepOwner() = (%d, %v) after the boundary, want the owner still held "+
			"until the engine acknowledges", tid, held)
	}
	if got := q.heldStepOwnerCount(); got != 1 {
		t.Fatalf("heldStepOwnerCount() = %d, want 1: the native spec's non-vacuity "+
			"gate depends on this counter advancing", got)
	}
}

// TestStepQueuePrefersTheHeldOwnerOverAParkedSibling pins which thread anchors
// the boundary when both are available. The held owner is the one the backend
// is keeping alive for this and the one it will release; a parked sibling's stop
// belongs to the engine's normal delivery order and must not be conflated with
// the anchor.
func TestStepQueuePrefersTheHeldOwnerOverAParkedSibling(t *testing.T) {
	const (
		stepped = 41
		foreign = 42
	)

	var q stepQueue
	q.beginStep(stepped)
	q.interruptStepIfStepped(stepped)
	q.holdStepOwner(stepped)
	q.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	boundary, ok := q.stepExitBoundary()
	if !ok || boundary.TID != stepped {
		t.Fatalf("stepExitBoundary() = (%+v, %v), want the held owner %d", boundary, ok, stepped)
	}
	if q.parkedDepthForTest() != 1 {
		t.Fatal("the boundary consumed the parked sibling instead of the held owner")
	}
}

// TestStepQueueReleasesTheHeldOwnerOnlyOnce covers the clear/release pairing.
// The backend releases the thread and then clears the hold; a second
// acknowledgement must find nothing to release, or a dead tid would be
// PTRACE_CONTed again.
func TestStepQueueReleasesTheHeldOwnerOnlyOnce(t *testing.T) {
	const stepped = 51

	var q stepQueue
	q.beginStep(stepped)
	q.interruptStepIfStepped(stepped)
	q.holdStepOwner(stepped)

	if tid, ok := q.heldStepOwner(); !ok || tid != stepped {
		t.Fatalf("heldStepOwner() = (%d, %v), want the owner held", tid, ok)
	}
	q.clearHeldStepOwner()
	q.completeStepThreadExit()

	if tid, ok := q.heldStepOwner(); ok {
		t.Fatalf("heldStepOwner() = (%d, %v) after release, want nothing held", tid, ok)
	}
}

// TestStepQueuePurgeDropsTheHeldOwnerObligation covers teardown. purge runs only
// where the threads it names are gone or unreachable, so the hold must not
// survive it: an obligation to resume a thread that no longer exists would be
// carried forever and, worse, would keep the boundary claiming a dead anchor.
func TestStepQueuePurgeDropsTheHeldOwnerObligation(t *testing.T) {
	const stepped = 61

	var q stepQueue
	q.beginStep(stepped)
	q.interruptStepIfStepped(stepped)
	q.holdStepOwner(stepped)
	q.park(StopEvent{Reason: StopBreakpoint, TID: stepped + 1})

	q.purge()

	if tid, ok := q.heldStepOwner(); ok {
		t.Fatalf("heldStepOwner() = (%d, %v) after purge, want nothing held", tid, ok)
	}
	if _, ok := q.stepExitBoundary(); ok {
		t.Fatal("stepExitBoundary() still reported an anchor after purge")
	}
}

func TestStepQueueEmptyIsInert(t *testing.T) {
	var q stepQueue
	if ev, ok := q.releasable(); ok {
		t.Fatalf("releasable() = %+v on a fresh queue, want nothing", ev)
	}
	q.endStep()
	q.interruptStepIfStepped(1)
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

// TestStepQueueResumeForRearmsOnlyTheSteppedThread pins the rule that decides
// how an absorbed stop's thread is resumed. Getting this wrong on the stepped
// thread cancels its single step while the gate stays latched, which freezes the
// tracee forever: no completion can arrive, so every later foreign stop parks
// and Wait never returns.
func TestStepQueueResumeForRearmsOnlyTheSteppedThread(t *testing.T) {
	const (
		stepped = 4100
		foreign = 4200
	)

	cases := []struct {
		name     string
		stepping bool
		stepTID  int
		tid      int
		want     stepResume
	}{
		{"idle queue continues", false, 0, foreign, resumeContinue},
		{"idle queue continues even the last stepped tid", false, 0, stepped, resumeContinue},
		{"stepped thread re-arms its step", true, stepped, stepped, resumeSingleStep},
		{"foreign thread continues mid-step", true, stepped, foreign, resumeContinue},
		{"another thread's tid does not match a live step", true, stepped, stepped + 1, resumeContinue},
		// stepping is the authority, not stepTID. endStep happens to zero the
		// tid today, so a guard that dropped the stepping term would behave the
		// same; pin the conjunction so that coupling cannot become load-bearing
		// without this failing.
		{"a leftover stepTID without a live step continues", false, stepped, stepped, resumeContinue},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &stepQueue{stepping: tc.stepping, stepTID: tc.stepTID}
			if got := q.resumeFor(tc.tid); got != tc.want {
				t.Fatalf("resumeFor(%d) = %v, want %v", tc.tid, got, tc.want)
			}
		})
	}
}

// TestStepQueueResumeForFollowsTheLiveStep checks the decision tracks the step
// rather than a one-off snapshot: a thread that was being stepped must go back
// to a plain continue once its step is over, or an absorbed stop would arm a
// single step the engine never asked for.
func TestStepQueueResumeForFollowsTheLiveStep(t *testing.T) {
	const stepped = 4300

	q := &stepQueue{}
	q.beginStep(stepped)
	if got := q.resumeFor(stepped); got != resumeSingleStep {
		t.Fatalf("resumeFor during the step = %v, want resumeSingleStep", got)
	}

	q.endStep()
	if got := q.resumeFor(stepped); got != resumeContinue {
		t.Fatalf("resumeFor after the step = %v, want resumeContinue", got)
	}

	q.beginStep(stepped)
	q.interruptStepIfStepped(stepped)
	if got := q.resumeFor(stepped); got != resumeContinue {
		t.Fatalf("resumeFor after a dead stepped thread = %v, want resumeContinue", got)
	}
	q.completeStepThreadExit()
}

// TestStepQueueAbortStepLiftsTheGateAndDropsHeldStops covers the absorb cases
// that cannot re-arm the step — an unknown ptrace event has no safe
// interpretation, and an exec has thrown the whole image away. Both must leave
// the queue inert, holding neither a stop nor an anchor, so the failing Wait
// tears the session down instead of stranding state behind a step that can never
// finish.
func TestStepQueueAbortStepLiftsTheGateAndDropsHeldStops(t *testing.T) {
	const (
		stepped = 4400
		foreign = 4500
	)

	q := &stepQueue{}
	q.beginStep(stepped)
	q.park(StopEvent{Reason: StopBreakpoint, TID: foreign})
	q.park(StopEvent{Reason: StopSignal, TID: foreign + 1, Signal: 11})
	q.interruptStepIfStepped(stepped)
	q.holdStepOwner(stepped)

	q.abortStep()

	if q.stepping {
		t.Fatal("abortStep left the step gate latched — every later foreign stop would park forever")
	}
	if q.stepExitPending {
		t.Fatal("abortStep left the reconciliation gate closed")
	}
	if got := len(q.parked); got != 0 {
		t.Fatalf("%d stops still held after abortStep, want 0 — they name threads the engine must not act on", got)
	}
	if tid, ok := q.heldStepOwner(); ok {
		t.Fatalf("heldStepOwner() = (%d, %v) after abortStep, want the obligation dropped", tid, ok)
	}
	if _, ok := q.releasable(); ok {
		t.Fatal("abortStep left a releasable stop behind")
	}
	if got := q.resumeFor(stepped); got != resumeContinue {
		t.Fatalf("resumeFor after abortStep = %v, want resumeContinue", got)
	}
}

// A signal carried by an absorbed stop must remain in the plan even when that
// plan re-arms a step. applyAbsorb stores it for a later continue while the
// PTRACE_SINGLESTEP itself still uses signal zero.
//
// SIGURG is the only signal any absorb site passes here, and it must reach the
// thread again on the continue path or Go loses a preemption it was told to
// take. Injecting it on the re-arm would execute the handler's first instruction
// instead of the instruction under the step, so primitive selection and signal
// retention are deliberately separate decisions.
// TestStepQueuePlanAbsorbCoversEveryWaitBranch pins what each wait-loop branch
// does to an in-flight step. Every branch that resumes a thread inline routes
// through planAbsorb, so this table is the gate on all of them at once: before
// the seam existed, turning any branch's step re-arm back into a plain continue
// — the freeze this whole queue exists to prevent — passed the entire suite.
// Together these pin every branch of Wait that absorbs a stop without returning
// it, one test per outcome class. They are deliberately separate top-level
// functions rather than subtests of one: a single function covering all five
// classes trips the repo's cognitive-complexity limit.
const (
	absorbSteppedTID = 5100
	absorbForeignTID = 5200
	absorbSignal     = 23
)

// absorbRearmingKinds are the branches that resume the thread and keep going.
func absorbRearmingKinds() []absorbKind {
	return []absorbKind{absorbClone, absorbNewThread, absorbPreempt, absorbContinued}
}

// absorbFatalKinds are the branches Wait cannot absorb under an in-flight step.
//
// PTRACE_EVENT_EXEC is deliberately NOT one of them and has no absorb row at
// all: it invalidates the image for every thread, so Wait fails on it
// unconditionally rather than asking what to do with the stop. That contract is
// pinned at the wait-loop level (TestLinuxWaitAlwaysFailsOnExec), because a
// table row here could only ever describe a decision production no longer makes.
func absorbFatalKinds() []absorbKind {
	return []absorbKind{absorbUnknownEvent}
}

func TestPlanAbsorbRearmsTheStepItConsumedOnTheSteppedThread(t *testing.T) {
	for _, kind := range absorbRearmingKinds() {
		q := &stepQueue{stepping: true, stepTID: absorbSteppedTID}
		got := q.planAbsorb(kind, absorbSteppedTID, absorbSignal)
		if got.fail || got.mode != resumeSingleStep {
			t.Fatalf("kind %d on the stepped thread = %+v, want a re-armed step", kind, got)
		}
		if got.signal != absorbSignal {
			t.Fatalf("kind %d re-armed the step after losing signal %d: got %+v", kind, absorbSignal, got)
		}
		if got.stepThreadExits {
			t.Fatalf("kind %d marked the live stepped thread as exiting", kind)
		}
	}
}

func TestPlanAbsorbContinuesAnyOtherThreadWithItsSignal(t *testing.T) {
	for _, kind := range absorbRearmingKinds() {
		q := &stepQueue{stepping: true, stepTID: absorbSteppedTID}
		got := q.planAbsorb(kind, absorbForeignTID, absorbSignal)
		if got.fail || got.mode != resumeContinue || got.signal != absorbSignal {
			t.Fatalf("kind %d on a foreign thread = %+v, want a continue carrying %d", kind, got, absorbSignal)
		}
		if got.stepThreadExits {
			t.Fatalf("kind %d marked a foreign thread as the dying step owner", kind)
		}
	}
}

// TestPlanAbsorbClosesHardwareStepForADyingSteppedThread covers the dying step
// owner with a sibling already parked: that sibling is the anchor, so the owner
// is resumed and left to finish dying.
func TestPlanAbsorbClosesHardwareStepForADyingSteppedThread(t *testing.T) {
	q := &stepQueue{stepping: true, stepTID: absorbSteppedTID}
	q.park(StopEvent{Reason: StopBreakpoint, TID: absorbForeignTID})
	got := q.planAbsorb(absorbThreadExit, absorbSteppedTID, absorbSignal)
	if !got.stepThreadExits {
		t.Fatalf("absorbThreadExit = %+v, want the dead step owner marked for reconciliation", got)
	}
	if got.fail {
		t.Fatalf("absorbThreadExit = %+v, want a resumable plan", got)
	}
	if got.holdStepOwner {
		t.Fatalf("absorbThreadExit = %+v, want the parked sibling to anchor the "+
			"boundary rather than holding the owner as well", got)
	}
	if got.mode == resumeSingleStep {
		t.Fatal("absorbThreadExit re-armed a step on a thread that is exiting")
	}
	if got.signal != 0 {
		t.Fatalf("absorbThreadExit delivered signal %d to an exiting thread", got.signal)
	}
}

// TestPlanAbsorbHoldsTheDyingStepOwnerWhenNothingIsParked is the empty-queue
// half of the same decision.
//
// The engine still owns a breakpoint whose trap is out of the tracee, and
// PTRACE_POKEDATA needs a thread that is genuinely ptrace-stopped to write it
// back through. With nothing parked, the dying owner is the only such thread —
// it is stopped at PTRACE_EVENT_EXIT, before exit_mm, so its address space is
// still mapped. Resuming it there leaves no anchor at all and the reinstall
// waits on whatever stop happens to arrive next.
func TestPlanAbsorbHoldsTheDyingStepOwnerWhenNothingIsParked(t *testing.T) {
	q := &stepQueue{stepping: true, stepTID: absorbSteppedTID}
	got := q.planAbsorb(absorbThreadExit, absorbSteppedTID, absorbSignal)
	if !got.holdStepOwner {
		t.Fatalf("absorbThreadExit with an empty queue = %+v, want the owner held "+
			"as the reconciliation anchor", got)
	}
	if !got.stepThreadExits {
		t.Fatalf("absorbThreadExit = %+v, want the dead step owner marked for reconciliation", got)
	}
	if got.fail || got.mode == resumeSingleStep {
		t.Fatalf("absorbThreadExit = %+v, want no resume at all for a held owner", got)
	}
}

// TestPlanAbsorbNeverHoldsAThreadThatIsNotTheStepOwner keeps the hold narrow. A
// thread that is merely exiting has no breakpoint transaction attached to it, so
// holding it would freeze an unrelated thread forever.
func TestPlanAbsorbNeverHoldsAThreadThatIsNotTheStepOwner(t *testing.T) {
	cases := []struct {
		name     string
		stepping bool
		tid      int
	}{
		{"a foreign thread during a step", true, absorbForeignTID},
		{"the last stepped tid with no step in flight", false, absorbSteppedTID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &stepQueue{stepping: tc.stepping, stepTID: absorbSteppedTID}
			got := q.planAbsorb(absorbThreadExit, tc.tid, 0)
			if got.holdStepOwner {
				t.Fatalf("absorbThreadExit(%d) = %+v, want no hold", tc.tid, got)
			}
			if got.mode != resumeContinue {
				t.Fatalf("absorbThreadExit(%d) = %+v, want a plain continue", tc.tid, got)
			}
		})
	}
}

// TestPlanAbsorbRefusesUnknownEventsOnTheSteppedThread pins the one remaining
// TID-keyed refusal. An event we never enabled has no understood stop shape, so
// it is fatal on the thread under the step and harmless anywhere else.
//
// exec used to share this row and no longer does: it is fatal for every thread
// unconditionally, so the two contracts are tested separately on purpose. A
// mutation that re-couples them — making unknown events unconditionally fatal,
// or making exec TID-keyed again — fails one of the two.
func TestPlanAbsorbRefusesUnknownEventsOnTheSteppedThread(t *testing.T) {
	for _, kind := range absorbFatalKinds() {
		q := &stepQueue{stepping: true, stepTID: absorbSteppedTID}
		if got := q.planAbsorb(kind, absorbSteppedTID, 0); !got.fail {
			t.Fatalf("kind %d on the stepped thread = %+v, want a refusal", kind, got)
		}
		q = &stepQueue{stepping: true, stepTID: absorbSteppedTID}
		got := q.planAbsorb(kind, absorbForeignTID, absorbSignal)
		if got.fail || got.mode != resumeContinue {
			t.Fatalf("kind %d on a foreign thread = %+v, want a plain continue", kind, got)
		}
	}
}

func TestPlanAbsorbNeverRearmsOrRefusesWithNoStepInFlight(t *testing.T) {
	for _, kind := range append(absorbRearmingKinds(), absorbFatalKinds()...) {
		q := &stepQueue{stepping: false, stepTID: absorbSteppedTID}
		got := q.planAbsorb(kind, absorbSteppedTID, absorbSignal)
		if got.fail || got.mode != resumeContinue {
			t.Fatalf("kind %d with no step in flight = %+v, want a plain continue", kind, got)
		}
	}
}

func TestStepQueuePlanAbsorbRetainsSignalsAcrossStepRearm(t *testing.T) {
	const (
		stepped = 4600
		foreign = 4700
		sigurg  = 23
	)

	cases := []struct {
		name       string
		stepping   bool
		stepTID    int
		tid        int
		signal     int
		wantMode   stepResume
		wantSignal int
	}{
		{"foreign thread mid-step keeps its signal", true, stepped, foreign, sigurg, resumeContinue, sigurg},
		{"stepped thread retains its signal for delayed delivery", true, stepped, stepped, sigurg, resumeSingleStep, sigurg},
		{"idle queue keeps the signal", false, 0, foreign, sigurg, resumeContinue, sigurg},
		{"idle queue keeps it even for the last stepped tid", false, stepped, stepped, sigurg, resumeContinue, sigurg},
		{"a zero signal stays zero on continue", true, stepped, foreign, 0, resumeContinue, 0},
		{"a zero signal stays zero on re-arm", true, stepped, stepped, 0, resumeSingleStep, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &stepQueue{stepping: tc.stepping, stepTID: tc.stepTID}
			got := q.planAbsorb(absorbPreempt, tc.tid, tc.signal)
			if got.mode != tc.wantMode {
				t.Fatalf("planAbsorb(%d, %d).mode = %v, want %v", tc.tid, tc.signal, got.mode, tc.wantMode)
			}
			if got.signal != tc.wantSignal {
				t.Fatalf("planAbsorb(%d, %d).signal = %d, want %d", tc.tid, tc.signal, got.signal, tc.wantSignal)
			}
		})
	}
}

// TestStepQueuePlanAbsorbAgreesWithResumeFor stops the two decisions drifting
// apart: planAbsorb owns the signal, but the primitive it picks must remain
// exactly what resumeFor says, so a future change to one cannot silently
// contradict the other.
func TestStepQueuePlanAbsorbAgreesWithResumeFor(t *testing.T) {
	const (
		stepped = 4800
		foreign = 4900
	)

	for _, stepping := range []bool{false, true} {
		for _, tid := range []int{stepped, foreign} {
			q := &stepQueue{stepping: stepping, stepTID: stepped}
			if got, want := q.planAbsorb(absorbClone, tid, 7).mode, q.resumeFor(tid); got != want {
				t.Fatalf("stepping=%v tid=%d: planAbsorb mode %v, resumeFor %v", stepping, tid, got, want)
			}
		}
	}
}

// The gate contract is stated independently of Wait's call order: once a step
// owner has died, no held stop may be released until the engine acknowledges
// the reconciliation boundary. Wait happens to consult the boundary first, so
// this is defence in depth — pinning it here keeps the queue correct if that
// ordering is ever changed.
func TestStepQueueHoldsReleaseUntilReconciliationIsAcknowledged(t *testing.T) {
	var q stepQueue
	q.beginStep(7)
	q.park(StopEvent{Reason: StopBreakpoint, TID: 9})
	q.interruptStepIfStepped(7)

	if _, ok := q.releasable(); ok {
		t.Fatal("held stop released before the reconciliation boundary was reported")
	}
	if _, ok := q.stepExitBoundary(); !ok {
		t.Fatal("reconciliation boundary was not reported")
	}
	if _, ok := q.releasable(); ok {
		t.Fatal("held stop released after the boundary but before the engine acknowledged it")
	}

	q.completeStepThreadExit()
	ev, ok := q.releasable()
	if !ok || ev.TID != 9 {
		t.Fatalf("held stop not released after acknowledgement: %+v ok=%v", ev, ok)
	}
}
