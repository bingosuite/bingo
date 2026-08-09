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
