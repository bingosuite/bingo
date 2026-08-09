// Exposes internal symbols to debugger_test. Compiled only during `go test`.
package debugger

import "fmt"

func ExportedTrapInstruction() []byte {
	return archTrapInstruction()
}

// ExportedLoadDWARF loads DWARF from binaryPath into the engine, bypassing a
// real Launch/Attach so DWARF-dependent inspection paths (Locals, StackFrames,
// Goroutines) can be exercised against the fakeBackend. Panics on failure.
func ExportedLoadDWARF(d Debugger, binaryPath string) {
	e := d.(*engine)
	if err := e.dispatch(func() error {
		e.loadDWARF(binaryPath)
		if e.dw == nil {
			return fmt.Errorf("no DWARF for %s", binaryPath)
		}
		return nil
	}); err != nil {
		panic("ExportedLoadDWARF: " + err.Error())
	}
}

// ExportedPCForFileLine resolves file:line to a runtime PC via the loaded DWARF.
func ExportedPCForFileLine(d Debugger, file string, line int) (uint64, error) {
	e := d.(*engine)
	var pc uint64
	err := e.dispatch(func() error {
		if e.dw == nil {
			return fmt.Errorf("no DWARF loaded")
		}
		var lookupErr error
		pc, lookupErr = e.dw.PCForFileLine(file, line)
		return lookupErr
	})
	return pc, err
}

func ExportedFileMatches(candidate, target string) bool {
	return fileMatches(candidate, target)
}

var ExportedErrBreakpointExists = errBreakpointExists

// ExportedForceSuspended forces stateSuspended with proc.live=true so tests
// can exercise suspended-state behaviour without launching a real process.
func ExportedForceSuspended(d Debugger) {
	e := d.(*engine)
	if err := e.dispatch(func() error {
		e.proc.live = true
		e.proc.pid = 0
		e.setState(stateSuspended)
		return nil
	}); err != nil {
		panic("ExportedForceSuspended: " + err.Error())
	}
}

func ExportedForceRunning(d Debugger) {
	e := d.(*engine)
	if err := e.dispatch(func() error {
		e.proc.live = true
		e.proc.pid = 0
		e.setState(stateRunning)
		return nil
	}); err != nil {
		panic("ExportedForceRunning: " + err.Error())
	}
}

// ExportedSetBreakpointAt installs a BP at addr bypassing DWARF lookup.
// File is "<direct-addr>". Panics on failure.
func ExportedSetBreakpointAt(d Debugger, addr uint64) int {
	e := d.(*engine)
	var id int
	err := e.dispatch(func() error {
		entry, err := e.bps.set(e.backend, "<direct-addr>", 0, addr)
		if err != nil {
			return err
		}
		id = entry.id
		return nil
	})
	if err != nil {
		panic("ExportedSetBreakpointAt: " + err.Error())
	}
	return id
}

func ExportedSetBreakpointAtErr(d Debugger, addr uint64) error {
	e := d.(*engine)
	return e.dispatch(func() error {
		_, err := e.bps.set(e.backend, "<direct-addr>", 0, addr)
		return err
	})
}

// ExportedFillEventBuffer emits filler events until exactly free ORDINARY slots
// remain in the engine's event buffer. Ordinary emits can never occupy the
// reserved halt slot, so free=0 means the buffer is full for every normal event
// while the reserved slot is still open. Deterministic only while nothing is
// draining Events().
func ExportedFillEventBuffer(d Debugger, free int) {
	e := d.(*engine)
	if err := e.dispatch(func() error {
		for eventBufSize-len(e.events) > free {
			e.emitOutput("stdout", "filler")
		}
		return nil
	}); err != nil {
		panic("ExportedFillEventBuffer: " + err.Error())
	}
}

// deferralState is the loop-owned foreign-stop deferral bookkeeping, read
// through the engine loop so tests observe it at a quiescent point.
type ExportedDeferralState struct {
	Deferred        int
	StepTID         int
	ParkedTID       int
	InternalStepTID int
	LastWaitTID     int
}

// ExportedDeferral snapshots the deferral bookkeeping. Dispatching through the
// loop doubles as a barrier: it can only run once the loop is idle, so callers
// may safely inspect the fake backend afterwards.
func ExportedDeferral(d Debugger) ExportedDeferralState {
	e := d.(*engine)
	var st ExportedDeferralState
	if err := e.dispatch(func() error {
		st = ExportedDeferralState{
			Deferred:        len(e.deferredStops),
			StepTID:         e.stepTID,
			ParkedTID:       e.parkedTID,
			InternalStepTID: e.internalStepTID,
			LastWaitTID:     e.lastWaitTID,
		}
		return nil
	}); err != nil {
		panic("ExportedDeferral: " + err.Error())
	}
	return st
}

// ExportedBreakpointArmedAt reports whether a breakpoint entry is tracked at
// addr — i.e. whether the table still knows about it and advertises it as
// installed.
func ExportedBreakpointArmedAt(d Debugger, addr uint64) bool {
	e := d.(*engine)
	var armed bool
	if err := e.dispatch(func() error {
		armed = e.bps.atAddr(addr) != nil
		return nil
	}); err != nil {
		panic("ExportedBreakpointArmedAt: " + err.Error())
	}
	return armed
}

// ExportedBreakpointIDs returns the ids the table currently tracks.
func ExportedBreakpointIDs(d Debugger) []int {
	e := d.(*engine)
	var ids []int
	if err := e.dispatch(func() error {
		for id := range e.bps.byID {
			ids = append(ids, id)
		}
		return nil
	}); err != nil {
		panic("ExportedBreakpointIDs: " + err.Error())
	}
	return ids
}
