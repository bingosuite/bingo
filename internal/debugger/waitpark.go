package debugger

import "sync/atomic"

// stopDisposition is the decision a backend's wait loop makes about a stop that
// would otherwise be handed straight to the engine.
type stopDisposition uint8

const (
	// surfaceStop returns the stop to the engine now.
	surfaceStop stopDisposition = iota
	// parkStop holds the stop until the in-flight single-step has completed.
	parkStop
)

// classifyUserStop resolves a user-visible tracee stop into the reason the
// engine should see and whether it may be surfaced yet.
//
// It exists as a free function, with no platform build tag and no backend
// receiver, so the linux wait loop's park/surface decision is table-testable
// without a real tracee — the surrounding wait loop is not.
//
// trap distinguishes a bare SIGTRAP (a software breakpoint or a completed
// single-step) from any other signal. stepping/stepTID are the backend's
// in-flight single-step bookkeeping.
//
// The rules:
//
//   - The stepped thread's own stops always surface. A bare SIGTRAP there is the
//     step completing; any other signal there is the step's outcome too, and the
//     engine has to see it so it can reinstall the trap it stepped off.
//   - Any other thread's user-visible stop is parked for the duration of the
//     step. Surfacing it would make the engine act on a thread that is not the
//     one the backend will resume next, which is what corrupts the step-over
//     state machine (issue #199).
//   - With no step in flight everything surfaces, exactly as before.
//
// A step is only treated as in flight when stepTID is known: parking against an
// unidentifiable step would wait for a completion that can never be recognised.
func classifyUserStop(trap, stepping bool, stepTID, tid int) (StopReason, stopDisposition) {
	inFlight := stepping && stepTID != 0

	if !inFlight {
		if trap {
			return StopBreakpoint, surfaceStop
		}
		return StopSignal, surfaceStop
	}

	if tid == stepTID {
		if trap {
			return StopSingleStep, surfaceStop
		}
		return StopSignal, surfaceStop
	}

	if trap {
		return StopBreakpoint, parkStop
	}
	return StopSignal, parkStop
}

// stepQueue is the single-step bookkeeping plus the stops held back because of
// it. A backend whose wait primitive reports stops for ANY thread (linux
// Wait4(-1, …, WALL)) must not hand the engine a stop from a thread other than
// the one it is stepping; those stops are queued here instead and released once
// the step has completed.
//
// It is deliberately platform-neutral and free of any backend dependency so the
// ordering and gating rules are unit-testable without a tracee. It carries NO
// lock: the owning backend must confine every method to its wait loop, which
// runs one call at a time (see the linux Wait doc comment for the happens-before
// argument).
type stepQueue struct {
	stepping bool // true after SingleStep; classifies the next SIGTRAP
	stepTID  int  // the exact thread SingleStep was issued against

	parked []StopEvent // held foreign stops, in arrival order

	// parkedTotal counts every stop ever held back. It exists solely so the
	// native regression test can assert it actually exercised the parking path
	// instead of passing vacuously on a run where the overlap never occurred.
	// Atomic because that test reads it through a public hook from its own
	// goroutine (see park_diag_linux_amd64.go).
	parkedTotal atomic.Int64

	// parkedSignalTotal counts the subset of those that were signal stops, so
	// the same test can distinguish a held asynchronous interrupt from a held
	// sibling trap. Atomic for the same reason.
	parkedSignalTotal atomic.Int64

	// stepRearmTotal counts how often an absorbed stop landed on the thread
	// being stepped and the step therefore had to be re-armed rather than
	// continued. It is the only observable that proves the resumeFor rule fired
	// against a real kernel: the failure it prevents is a freeze, which a test
	// can otherwise only detect as a timeout. Atomic for the same reason.
	stepRearmTotal atomic.Int64
}

// countStepRearm records that an absorbed stop consumed an in-flight step.
func (q *stepQueue) countStepRearm() { q.stepRearmTotal.Add(1) }

// stepRearmCount reports how many absorbed stops re-armed a single step.
func (q *stepQueue) stepRearmCount() int { return int(q.stepRearmTotal.Load()) }

// park holds a foreign stop until the in-flight step completes. It deliberately
// does not touch the backend's current-stop bookkeeping: that must keep naming
// the thread the backend will actually resume next, and this thread is not it.
func (q *stepQueue) park(ev StopEvent) {
	q.parked = append(q.parked, ev)
	q.parkedTotal.Add(1)
	if ev.Reason == StopSignal {
		q.parkedSignalTotal.Add(1)
	}
}

// parkedCount reports how many stops have been held back over the backend's
// lifetime.
func (q *stepQueue) parkedCount() int { return int(q.parkedTotal.Load()) }

// parkedSignalCount reports how many of those held stops were signal stops
// rather than breakpoint traps.
//
// It is split out because it is the only observable that distinguishes "an
// asynchronous interrupt reached the backend while a step was in flight" from
// "a sibling happened to trap". The linux wait loop absorbs SIGURG, SIGCONT and
// a new thread's initial SIGSTOP before classification, so a parked signal stop
// is a genuine externally-directed interrupt — in practice the SIGSTOP that
// Pause sends at the main thread.
func (q *stepQueue) parkedSignalCount() int { return int(q.parkedSignalTotal.Load()) }

// releasable pops the oldest held stop if one may be surfaced now.
//
// Nothing is released while a single-step is outstanding: the engine must see
// the step's own completion first, so that the trap it stepped off is
// reinstalled before any sibling stop at the same address is acted on. Delivery
// is FIFO so the engine observes sibling stops in the order the kernel reported
// them.
func (q *stepQueue) releasable() (StopEvent, bool) {
	if q.stepping || len(q.parked) == 0 {
		return StopEvent{}, false
	}
	ev := q.parked[0]
	q.parked = q.parked[1:]
	return ev, true
}

// purge drops every held stop. Called only where the tracee is known to be
// gone: releasing a held stop afterwards would make the engine issue ptrace ops
// against a dead thread.
func (q *stepQueue) purge() { q.parked = nil }

// beginStep records that tid is being single-stepped.
func (q *stepQueue) beginStep(tid int) {
	q.stepping = true
	q.stepTID = tid
}

// endStep clears the step bookkeeping, which also lifts the release gate.
func (q *stepQueue) endStep() {
	q.stepping = false
	q.stepTID = 0
}

// clearStepIfStepped ends the step when the stepped thread itself dies. Without
// it stepping stays true forever, the queue is never released, and the wait loop
// blocks for a step completion that can no longer happen.
func (q *stepQueue) clearStepIfStepped(tid int) {
	if q.stepping && tid == q.stepTID {
		q.endStep()
	}
}

// stepResume says how a thread must be resumed when the wait loop absorbs its
// stop instead of reporting it.
type stepResume int

const (
	// resumeContinue lets the thread run on, which is what absorbing a stop
	// has always meant.
	resumeContinue stepResume = iota
	// resumeSingleStep re-arms the single step the absorbed stop consumed.
	resumeSingleStep
)

// resumeFor reports how an absorbed stop's thread must be resumed.
//
// Absorbing a stop means resuming its thread inline and waiting again, and for
// every thread but one that is simply a continue. The exception is the thread
// the engine is single-stepping: the kernel delivers only ONE stop per resume,
// so an absorbed event on that thread has consumed the pending step. Continuing
// it there cancels the step while `stepping`/`stepTID` stay latched, and then
// nothing can ever lift the gate — no step completion is coming, so every later
// foreign stop parks forever and `Wait` never returns again. That turns a stop
// the wait loop merely wanted to ignore into a frozen tracee, so the step is
// re-armed instead and its completion arrives one instruction later than the
// engine asked for.
func (q *stepQueue) resumeFor(tid int) stepResume {
	if q.stepping && tid == q.stepTID {
		return resumeSingleStep
	}
	return resumeContinue
}

// absorbKind names the wait-loop branch that absorbed a stop. Every branch that
// resumes a thread inline names itself here rather than reaching for a ptrace
// primitive directly, so what a branch does to an in-flight step is decided in
// one pure, table-tested place instead of seven hand-written call sites. A
// mutation to any of those decisions changes a planAbsorb row and fails a test;
// before this seam existed, silently turning a step re-arm back into a plain
// continue — the exact freeze rule 7 exists to prevent — passed every unit test.
type absorbKind int

const (
	// absorbClone is the parent's PTRACE_EVENT_CLONE stop.
	absorbClone absorbKind = iota
	// absorbNewThread is a freshly cloned thread's initial group-stop SIGSTOP.
	absorbNewThread
	// absorbPreempt is SIGURG, Go's async-preemption signal.
	absorbPreempt
	// absorbContinued is SIGCONT.
	absorbContinued
	// absorbThreadExit is a non-main thread's PTRACE_EVENT_EXIT.
	absorbThreadExit
	// absorbExec is PTRACE_EVENT_EXEC, which replaces the image the stepped-over
	// breakpoint lived in.
	absorbExec
	// absorbUnknownEvent is a PTRACE_EVENT we never enabled.
	absorbUnknownEvent
)

// absorbPlan is what a wait-loop branch must do with the thread it absorbed:
// whether the stop can be absorbed at all, the primitive to resume with, the
// signal that primitive delivers, and whether the step gate has to be released.
type absorbPlan struct {
	// fail marks a stop that cannot be absorbed because the step it consumed can
	// neither be completed nor re-armed. The caller aborts the step and errors.
	fail bool
	// mode is the ptrace primitive to resume the thread with.
	mode stepResume
	// signal is the signal that primitive delivers.
	signal int
	// clearStep releases the gate because the stepped thread is going away and
	// no step completion can ever arrive for it.
	clearStep bool
}

// planAbsorb decides, for one absorbed stop, every effect that stop has on the
// step gate — so no branch can cancel a hardware single step while leaving
// stepping/stepTID latched, which would park every later foreign stop forever
// and freeze the tracee inside Wait.
//
// Three shapes fall out of that:
//
//   - A dying thread (absorbThreadExit) is never re-armed — there is nothing
//     left to step — and releases the gate instead, so held stops can drain.
//   - absorbExec and absorbUnknownEvent cannot be absorbed on the stepped thread
//     at all: exec throws away the memory the saved instruction bytes and the
//     trap live in, and an event we never enabled has no understood stop shape.
//     Re-arming would assume that shape and continuing would resume a tracee
//     whose software breakpoint is still out of memory, so the caller aborts the
//     step and surfaces an error rather than guessing.
//   - Everything else re-arms the step it consumed.
//
// The stepped thread is re-armed WITHOUT its signal, and that asymmetry is
// deliberate. PTRACE_SINGLESTEP with a signal makes the kernel build the signal
// frame first, so the one instruction that then executes is the handler's, not
// the instruction the engine asked to step over. The engine would take that stop
// as the step completing, reinstall the trap at the breakpoint address and
// continue — but the original instruction never ran, so returning from the
// handler lands back on the re-armed trap and reports the same breakpoint again.
// Dropping the signal instead costs one delivery; SIGURG is the only signal that
// can reach here, it is Go's async-preemption hint, and the runtime has already
// set the goroutine's preempt flag before sending it, so the preemption still
// happens at the next stack check and sysmon retries regardless.
//
// This preserves the pre-existing linux behaviour exactly: before the park queue
// the SIGURG branch chose between a bare singleStep on the stepped thread and a
// signal-carrying continue on any other, and SIGURG remains the only caller that
// passes a non-zero signal. Forwarding it on the step path is out of scope here
// and belongs with the per-TID signal-forwarding work.
func (q *stepQueue) planAbsorb(kind absorbKind, tid int, signal int) absorbPlan {
	switch kind {
	case absorbThreadExit:
		return absorbPlan{mode: resumeContinue, signal: 0, clearStep: true}
	case absorbExec, absorbUnknownEvent:
		if q.resumeFor(tid) == resumeSingleStep {
			return absorbPlan{fail: true}
		}
		return absorbPlan{mode: resumeContinue, signal: 0}
	default:
		if q.resumeFor(tid) == resumeSingleStep {
			return absorbPlan{mode: resumeSingleStep, signal: 0}
		}
		return absorbPlan{mode: resumeContinue, signal: signal}
	}
}

// abortStep resolves a step that can no longer complete, for the cases where
// re-arming it would be wrong rather than merely late.
//
// It clears the gate AND drops the held stops, because both only make sense
// while the step they are waiting on can still finish. Callers must follow it
// with an error: un-latching alone would resume a tracee whose stepped-over
// software breakpoint is still absent from memory, which is the very corruption
// the queue exists to prevent.
func (q *stepQueue) abortStep() {
	q.endStep()
	q.purge()
}

// parkedDepthForTest reports how many stops are held right now. parkedCount is
// cumulative and cannot distinguish "still held" from "already delivered".
func (q *stepQueue) parkedDepthForTest() int { return len(q.parked) }
