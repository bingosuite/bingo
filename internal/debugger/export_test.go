// export_test.go exposes internal symbols to the debugger_test package.
// Only compiled during `go test`.
package debugger

// ExportedTrapInstruction exposes archTrapInstruction so tests can verify
// the correct bytes are written to tracee memory.
func ExportedTrapInstruction() []byte {
	return archTrapInstruction()
}

// ExportedFileMatches exposes fileMatches for DWARF path-matching tests.
func ExportedFileMatches(candidate, target string) bool {
	return fileMatches(candidate, target)
}

// ExportedErrBreakpointExists exposes the errBreakpointExists sentinel.
var ExportedErrBreakpointExists = errBreakpointExists

// ExportedForceSuspended puts d into stateSuspended with proc.live=true so
// that tests can exercise suspended-state engine behaviour (stepping,
// inspection, breakpoint operations) without launching a real OS process.
//
// This is only safe in tests — it bypasses ptrace/Win32 entirely.
func ExportedForceSuspended(d Debugger) {
	e := d.(*engine)
	e.dispatch(func() error {
		e.proc.live = true
		e.proc.pid = 0 // pid=0 causes process.kill to be a no-op (guards against it)
		e.setState(stateSuspended)
		return nil
	})
}

// ExportedForceRunning puts d into stateRunning. Used to verify that
// suspended-only commands are rejected when the process is running.
func ExportedForceRunning(d Debugger) {
	e := d.(*engine)
	e.dispatch(func() error {
		e.proc.live = true
		e.proc.pid = 0
		e.setState(stateRunning)
		return nil
	})
}

// ExportedSetBreakpointAt installs a breakpoint at the given address using
// the engine's internal breakpoint table, bypassing DWARF lookup entirely.
// The file is recorded as "<direct-addr>" so tests can identify it.
// Returns the assigned breakpoint ID. Panics if the installation fails.
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

// ExportedSetBreakpointAtErr is like ExportedSetBreakpointAt but returns
// the error instead of panicking. Used to test duplicate-address detection.
func ExportedSetBreakpointAtErr(d Debugger, addr uint64) error {
	e := d.(*engine)
	return e.dispatch(func() error {
		_, err := e.bps.set(e.backend, "<direct-addr>", 0, addr)
		return err
	})
}
