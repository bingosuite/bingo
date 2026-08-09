// Exposes internal symbols to debugger_test. Compiled only during `go test`.
package debugger

import (
	"fmt"

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

// ExportedRuntimeVarAddr resolves a runtime package variable's address through
// the loaded DWARF, so a test can plant a synthetic runtime.allgs / runtime.allm
// in the fake backend's memory at exactly the address the reader will look at.
func ExportedRuntimeVarAddr(d Debugger, name string) (uint64, bool) {
	e := d.(*engine)
	var (
		addr  uint64
		found bool
	)
	_ = e.dispatch(func() error {
		if e.dw == nil {
			return nil
		}
		addr, found = e.dw.runtimeVarAddr(name)
		return nil
	})
	return addr, found
}

// ExportedGoOffsets returns the DWARF-resolved runtime struct offsets the
// snapshot reader uses, keyed "<struct>.<field>". A test fabricating g and m
// structs must lay them out at these offsets rather than hardcoding any, for
// the same reason the reader does: they shift between Go versions.
func ExportedGoOffsets(d Debugger) (map[string]int64, bool) {
	e := d.(*engine)
	out := make(map[string]int64)
	ok := false
	_ = e.dispatch(func() error {
		l, valid := e.getGoLayout()
		if !valid {
			return nil
		}
		ok = true
		out["g.stack"] = l.gStack
		out["g.m"] = l.gM
		out["g.sched"] = l.gSched
		out["g.atomicstatus"] = l.gAtomicstatus
		out["g.goid"] = l.gGoid
		out["g.waitreason"] = l.gWaitreason
		out["g.parentGoid"] = l.gParentGoid
		out["g.gopc"] = l.gGopc
		out["g.startpc"] = l.gStartpc
		out["stack.lo"] = l.stackLo
		out["stack.hi"] = l.stackHi
		out["gobuf.pc"] = l.bufPC
		out["m.procid"] = l.mProcid
		out["m.curg"] = l.mCurg
		out["m.id"] = l.mID
		out["m.alllink"] = l.mAlllink
		out["m.spinning"] = l.mSpinning
		return nil
	})
	return out, ok
}

// ExportedEmitSnapshot drives the streamed-snapshot path with a synthesised
// pack report. The producer's scan ceilings make a non-conforming report
// unreachable in production (see the lifecycle-delta bound), so this is the only
// way to pin what the engine does with one.
func ExportedEmitSnapshot(d Debugger, payload protocol.GoroutineSnapshotPayload, report protocol.GoroutinePackReport) {
	d.(*engine).emitSnapshot(snapshotResult{payload: payload, report: report})
}

// ExportedMaxGoroutineScan and ExportedMaxThreadScan expose the reader's walk
// ceilings so a test can prove the lifecycle deltas they bound cannot overflow
// the wire contract.
const (
	ExportedMaxGoroutineScan = maxGoroutineScan
	ExportedMaxThreadScan    = maxThreadScan
)
