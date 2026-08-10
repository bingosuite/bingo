//go:build e2e

package debugger

import (
	"errors"
	"fmt"
)

// InFlightBreakpointReservationProbe captures the actor-serialized native
// interleaving that cannot be scheduled deterministically through separate
// public method calls once a hardware single-step can complete.
type InFlightBreakpointReservationProbe struct {
	FirstSetConflict   bool
	FirstSetError      string
	ClearSucceeded     bool
	ClearError         string
	ClearedSetConflict bool
	ClearedSetError    string
}

// ProbeInFlightBreakpointReservation runs only in native E2E builds. It uses
// the same resolved-address helper as SetBreakpoint while holding the engine
// actor across resume, Set, Clear, and Set so stop delivery cannot close the
// in-flight window between assertions.
func ProbeInFlightBreakpointReservation(
	d Debugger,
	file string,
	line int,
	breakpointID int,
) (InFlightBreakpointReservationProbe, error) {
	e, ok := d.(*engine)
	if !ok {
		return InFlightBreakpointReservationProbe{}, fmt.Errorf("unsupported debugger type %T", d)
	}

	var probe InFlightBreakpointReservationProbe
	err := e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		if e.dw == nil {
			return fmt.Errorf("no DWARF info")
		}
		addr, err := e.dw.PCForFileLine(file, line)
		if err != nil {
			return err
		}
		if !e.lastBP.matchesID(breakpointID) || e.lastBP.addr != addr {
			return fmt.Errorf("breakpoint %d is not parked at 0x%x", breakpointID, addr)
		}
		if err := e.resumeFromBreakpoint(bpResumeContinue, 0); err != nil {
			return err
		}
		e.emitContinued()

		_, setErr := e.setBreakpoint(file, line, addr)
		probe.FirstSetConflict = errors.Is(setErr, errBreakpointExists)
		if setErr != nil {
			probe.FirstSetError = setErr.Error()
		}

		clearErr := e.clearBreakpoint(breakpointID)
		probe.ClearSucceeded = clearErr == nil
		if clearErr != nil {
			probe.ClearError = clearErr.Error()
		}

		_, setErr = e.setBreakpoint(file, line, addr)
		probe.ClearedSetConflict = errors.Is(setErr, errBreakpointExists)
		if setErr != nil {
			probe.ClearedSetError = setErr.Error()
		}
		return nil
	})
	return probe, err
}
