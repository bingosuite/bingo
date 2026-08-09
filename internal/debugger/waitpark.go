package debugger

import (
	"bytes"
	"sync/atomic"
)

// parkedTrapMissing reports whether the trap that produced a held breakpoint
// stop has since been removed from the tracee.
//
// Holding a stop opens a window the engine never had before: it can clear the
// very trap the held stop refers to. The `<stepover-next>` sentinel is cleared
// the instant ANY thread reaches it, so a sibling parked at the same address is
// left holding a stop for a trap that no longer exists. Delivering that stop
// takes the engine's spurious-SIGTRAP path, which resumes the thread at the
// trap-return PC — and with the original bytes restored that address is
// mid-instruction on amd64, so the thread faults on every retry and the tracee
// silently stops making progress.
//
// trap is the architecture's trap encoding and trapAddr the address it occupied
// (the rewound PC). A byte-for-byte match means the trap is still armed — either
// still ours, or a genuine foreign trap that is part of the tracee's own code —
// and the stop is delivered exactly as an unheld one would be. An unreadable
// address reports "present" so the caller keeps the pre-existing behaviour
// rather than dropping a stop on a transient read failure.
func parkedTrapMissing(trapAddr uint64, trap []byte, read func(addr uint64, dst []byte) error) bool {
	if len(trap) == 0 {
		return false
	}
	got := make([]byte, len(trap))
	if read(trapAddr, got) != nil {
		return false
	}
	return !bytes.Equal(got, trap)
}

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

	// staleTotal counts held breakpoint stops that were dropped because the
	// engine cleared their trap while they waited (see parkedTrapMissing).
	// Atomic for the same reason as the counters above.
	staleTotal atomic.Int64
}

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

// staleParkedCount reports how many held stops were dropped because their trap
// had been removed by the time they could be released.
func (q *stepQueue) staleParkedCount() int { return int(q.staleTotal.Load()) }

// trapRestarter is the slice of a backend that stale-stop recovery needs. It
// exists so the recovery sequence is testable without a live tracee.
type trapRestarter interface {
	GetRegisters(tid int) (Registers, error)
	SetRegisters(tid int, reg Registers) error
	ReadMemory(addr uint64, dst []byte) error
	continueIfTraceeExists(tid int, signal int) error
}

// restartStaleParked resumes a thread whose held breakpoint stop names a trap
// that was removed while the stop waited, and reports that the stop was dropped
// rather than delivered.
//
// The thread is parked at the trap-return PC. Once the trap is gone that address
// is mid-instruction wherever the architecture advances past the trap, so the
// instruction is restarted from its first byte — exactly what would have
// happened had the breakpoint been cleared while the thread was running.
//
// Nothing is recorded as the current stop: the engine never sees this thread, so
// recording it would repoint every TID-less ptrace op at a thread that is
// running again. Every failure path leaves the stop deliverable, preserving the
// behaviour a backend had before stops could be held at all.
//
// trap and rewind are passed in rather than read from the arch globals so the
// rule can be exercised on any host.
func (q *stepQueue) restartStaleParked(r trapRestarter, ev StopEvent, trap []byte, rewind func(uint64) uint64) bool {
	if ev.Reason != StopBreakpoint {
		return false
	}
	regs, err := r.GetRegisters(ev.TID)
	if err != nil {
		return false
	}
	addr := rewind(regs.PC)
	if addr == regs.PC {
		// The architecture leaves the PC at the trap, so resuming re-executes
		// the instruction whether or not the trap is still installed.
		return false
	}
	if !parkedTrapMissing(addr, trap, r.ReadMemory) {
		return false
	}
	regs.PC = addr
	if err := r.SetRegisters(ev.TID, regs); err != nil {
		return false
	}
	q.staleTotal.Add(1)
	// A failure here means the thread died between the register write and the
	// resume; the stop is stale either way and must not reach the engine.
	_ = r.continueIfTraceeExists(ev.TID, 0)
	return true
}

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
