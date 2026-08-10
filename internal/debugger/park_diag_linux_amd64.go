//go:build linux && amd64

package debugger

// LinuxParkedStopCount reports how many foreign-thread stops the wait loop has
// held back for the duration of a single-step (and whether the Debugger is one
// with a linux backend at all). It is a linux-only test hook: the step-overlap
// regression test in test/integration drives the public Debugger surface and
// cannot otherwise reach the unexported backend.
//
// It exists to make that test's non-vacuity checkable rather than assumed. The
// overlap it provokes is inherently racy, so a run that never actually parked a
// stop proves nothing about the parking rule; asserting this count grew turns
// that silent hole into a failure. Returns (0, false) for a non-engine Debugger.
func LinuxParkedStopCount(d Debugger) (int, bool) {
	e, ok := d.(*engine)
	if !ok {
		return 0, false
	}
	b, ok := e.backend.(*linuxBackend)
	if !ok {
		return 0, false
	}
	return b.parkedCount(), true
}

// LinuxParkedSignalCount reports how many of the held-back stops were signal
// stops rather than breakpoint traps.
//
// This is the observable that proves an asynchronous interrupt was received by
// the backend *while a single-step was in flight*. The wait loop absorbs SIGURG,
// SIGCONT and a new thread's initial SIGSTOP before classification, so the only
// signal that can reach the park queue in the overlap target is the SIGSTOP that
// Pause directs at the main thread. Returns (0, false) for a non-engine
// Debugger.
func LinuxParkedSignalCount(d Debugger) (int, bool) {
	e, ok := d.(*engine)
	if !ok {
		return 0, false
	}
	b, ok := e.backend.(*linuxBackend)
	if !ok {
		return 0, false
	}
	return b.parkedSignalCount(), true
}

// LinuxStepRearmCount reports how many times the wait loop absorbed a stop on
// the thread it was single-stepping and re-armed that step instead of
// continuing the thread.
//
// This is the native observable for the absorb-resume rule. A plain continue
// there cancels the step while the step gate stays latched, after which no
// completion can ever arrive, every later foreign stop is held forever and the
// tracee freezes — a failure a test can otherwise only see as a timeout, which
// is indistinguishable from ordinary slowness. A positive count proves the rule
// ran against a real kernel. Returns (0, false) for a non-engine Debugger.
func LinuxStepRearmCount(d Debugger) (int, bool) {
	e, ok := d.(*engine)
	if !ok {
		return 0, false
	}
	b, ok := e.backend.(*linuxBackend)
	if !ok {
		return 0, false
	}
	return b.stepRearmCount(), true
}

// LinuxStepThreadExitCount reports how many single-steps lost their owning
// thread before completion. The dedicated native overlap spec uses it to prove
// that its breakpoint-ownership assertions crossed the reconciliation boundary
// rather than passing without a thread death.
func LinuxStepThreadExitCount(d Debugger) (int, bool) {
	e, ok := d.(*engine)
	if !ok {
		return 0, false
	}
	b, ok := e.backend.(*linuxBackend)
	if !ok {
		return 0, false
	}
	return b.stepExitCount(), true
}

// LinuxRetiredInternalBreakpointCount reports how many delayed sibling hits
// were recovered after another thread auto-cleared the one-shot sentinel.
//
// The hit may already be queued in the kernel before the sentinel is cleared,
// so it need not pass through the backend's parked-stop FIFO. Reading through
// dispatch keeps the loop-thread-owned counter race-free.
func LinuxRetiredInternalBreakpointCount(d Debugger) (int, bool) {
	e, ok := d.(*engine)
	if !ok {
		return 0, false
	}
	if _, ok := e.backend.(*linuxBackend); !ok {
		return 0, false
	}
	var count int
	if err := e.dispatch(func() error {
		count = e.retiredInternalBreakpointHits
		return nil
	}); err != nil {
		return 0, false
	}
	return count, true
}
