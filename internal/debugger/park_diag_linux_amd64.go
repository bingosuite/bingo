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
