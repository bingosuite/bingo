// Exposes internal symbols to debugger_test. Compiled only during `go test`.
package debugger

func ExportedTrapInstruction() []byte {
	return archTrapInstruction()
}

func ExportedFileMatches(candidate, target string) bool {
	return fileMatches(candidate, target)
}

var ExportedErrBreakpointExists = errBreakpointExists

// ExportedForceSuspended forces stateSuspended with proc.live=true so tests
// can exercise suspended-state behaviour without launching a real process.
func ExportedForceSuspended(d Debugger) {
	e := d.(*engine)
	e.dispatch(func() error {
		e.proc.live = true
		e.proc.pid = 0
		e.setState(stateSuspended)
		return nil
	})
}

func ExportedForceRunning(d Debugger) {
	e := d.(*engine)
	e.dispatch(func() error {
		e.proc.live = true
		e.proc.pid = 0
		e.setState(stateRunning)
		return nil
	})
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
