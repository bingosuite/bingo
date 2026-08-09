// Exposes internal symbols to debugger_test. Compiled only during `go test`.
package debugger

import (
	"fmt"
	"sort"

	"github.com/bingosuite/bingo/pkg/protocol"
)

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

// ExportedSnapshotFrom assembles a snapshot payload around a pre-built
// goroutine list, bypassing the runtime memory scan, so tests can pin the
// lifecycle split: trackLifecycle=true is the automatic stop path (diffs and
// adopts the baseline), false is the on-demand query path. Runs on the engine
// loop thread, like the real callers.
func ExportedSnapshotFrom(d Debugger, gs []protocol.Goroutine, trackLifecycle bool) protocol.GoroutineSnapshotPayload {
	e := d.(*engine)
	var snap protocol.GoroutineSnapshotPayload
	if err := e.dispatch(func() error {
		snap = e.snapshotFrom(gs, 0, trackLifecycle)
		return nil
	}); err != nil {
		panic("ExportedSnapshotFrom: " + err.Error())
	}
	return snap
}

// ExportedPrevGoids returns the remembered live goid set (the lifecycle-delta
// baseline), sorted. Nil before the first automatic snapshot.
func ExportedPrevGoids(d Debugger) []int {
	e := d.(*engine)
	var ids []int
	if err := e.dispatch(func() error {
		for id := range e.prevGoids {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		return nil
	}); err != nil {
		panic("ExportedPrevGoids: " + err.Error())
	}
	return ids
}
