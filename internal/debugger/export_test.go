// Exposes internal symbols to debugger_test. Compiled only during `go test`.
package debugger

import (
	"fmt"
	"sort"
	"sync"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type ExportedGoRuntimeLayout struct {
	Allgs uint64
	Allm  uint64

	GStack        uint64
	GAtomicstatus uint64
	GGoid         uint64
	GParentGoid   uint64

	StackLo uint64
	StackHi uint64

	MProcid  uint64
	MCurg    uint64
	MAlllink uint64
}

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

func ExportedGoRuntimeLayoutFor(d Debugger) ExportedGoRuntimeLayout {
	e := d.(*engine)
	var out ExportedGoRuntimeLayout
	if err := e.dispatch(func() error {
		l, ok := e.getGoLayout()
		if !ok {
			return fmt.Errorf("no Go runtime layout")
		}
		allgs, ok := e.dw.runtimeVarAddr("runtime.allgs")
		if !ok {
			return fmt.Errorf("no runtime.allgs")
		}
		allm, ok := e.dw.runtimeVarAddr("runtime.allm")
		if !ok {
			return fmt.Errorf("no runtime.allm")
		}
		out = ExportedGoRuntimeLayout{
			Allgs: allgs,
			Allm:  allm,

			GStack:        uint64(l.gStack),
			GAtomicstatus: uint64(l.gAtomicstatus),
			GGoid:         uint64(l.gGoid),
			GParentGoid:   uint64(l.gParentGoid),

			StackLo: uint64(l.stackLo),
			StackHi: uint64(l.stackHi),

			MProcid:  uint64(l.mProcid),
			MCurg:    uint64(l.mCurg),
			MAlllink: uint64(l.mAlllink),
		}
		return nil
	}); err != nil {
		panic("ExportedGoRuntimeLayoutFor: " + err.Error())
	}
	return out
}

func ExportedGoroutineSnapshot(d Debugger) protocol.GoroutineSnapshotPayload {
	e := d.(*engine)
	var snap protocol.GoroutineSnapshotPayload
	if err := e.dispatch(func() error {
		snap = e.goroutineSnapshot()
		return nil
	}); err != nil {
		panic("ExportedGoroutineSnapshot: " + err.Error())
	}
	return snap
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

// ExportedForgetLastBreakpointTID zeroes lastBPTID while the engine stays
// parked on its breakpoint, reproducing a stop that named no thread. That is
// the only state in which resumeFromBreakpoint falls back to Backend.Threads
// to pick the step-off thread.
func ExportedForgetLastBreakpointTID(d Debugger) {
	e := d.(*engine)
	if err := e.dispatch(func() error {
		e.lastBPTID = 0
		return nil
	}); err != nil {
		panic("ExportedForgetLastBreakpointTID: " + err.Error())
	}
}

// ExportedSetBreakpointAt installs a BP at addr bypassing DWARF lookup.
// File is "<direct-addr>". Panics on failure.
func ExportedSetBreakpointAt(d Debugger, addr uint64) int {
	e := d.(*engine)
	var id int
	err := e.dispatch(func() error {
		entry, err := e.setBreakpoint("<direct-addr>", 0, addr)
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
		_, err := e.setBreakpoint("<direct-addr>", 0, addr)
		return err
	})
}

func ExportedClearAllBreakpoints(d Debugger) {
	e := d.(*engine)
	if err := e.dispatch(func() error {
		e.clearAllBreakpoints()
		return nil
	}); err != nil {
		panic("ExportedClearAllBreakpoints: " + err.Error())
	}
}

func ExportedSetStepOverBreakpointAt(d Debugger, addr uint64) {
	e := d.(*engine)
	if err := e.dispatch(func() error {
		_, err := e.bps.set(e.backend, stepOverNextFile, 0, addr)
		return err
	}); err != nil {
		panic("ExportedSetStepOverBreakpointAt: " + err.Error())
	}
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

// ExportedStepQueue exposes the production park-queue primitives to the
// external engine tests. It holds a real stepQueue and only delegates, so a
// test built on it exercises the shipped gating rules rather than a
// re-statement of them: mutating releasable, interruptStepIfStepped or
// stepExitBoundary changes what such a test observes.
//
// It exists because the queue's contract spans two layers — the backend decides
// WHEN a held stop may surface, the engine decides WHAT must be repaired before
// it does — and neither layer's own tests can observe the other's state.
// The production stepQueue is deliberately lock-free: in the linux backend it is
// reached only from Wait, on one goroutine. A test that also inspects it from
// the spec goroutine has to supply that serialization itself, so every method
// here takes mu. The guarded calls are the real production methods, so the
// gating rules under test are still the shipped ones.
type ExportedStepQueue struct {
	mu sync.Mutex
	q  stepQueue
}

func (e *ExportedStepQueue) BeginStep(tid int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.q.beginStep(tid)
}

func (e *ExportedStepQueue) EndStep() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.q.endStep()
}

// Stepping reports the in-flight single-step bookkeeping classifyUserStop needs.
func (e *ExportedStepQueue) Stepping() (bool, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.q.stepping, e.q.stepTID
}

// Classify runs the production park/surface decision for a user-visible stop.
// It reports the reason the engine should see and whether the stop must be held.
func (e *ExportedStepQueue) Classify(trap bool, tid int) (StopReason, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	reason, disp := classifyUserStop(trap, e.q.stepping, e.q.stepTID, tid)
	return reason, disp == parkStop
}

func (e *ExportedStepQueue) Park(ev StopEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.q.park(ev)
}

func (e *ExportedStepQueue) Releasable() (StopEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.q.releasable()
}

func (e *ExportedStepQueue) StepExitBoundary() (StopEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.q.stepExitBoundary()
}

func (e *ExportedStepQueue) InterruptStepIfStepped(tid int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.q.interruptStepIfStepped(tid)
}

func (e *ExportedStepQueue) CompleteStepThreadExit() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.q.completeStepThreadExit()
}

// HoldStepOwner retains tid as the reconciliation anchor, as Wait does when the
// step owner dies with nothing parked.
func (e *ExportedStepQueue) HoldStepOwner(tid int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.q.holdStepOwner(tid)
}

// HeldStepOwner reports the retained anchor, so a test can prove the obligation
// was dropped rather than leaked.
func (e *ExportedStepQueue) HeldStepOwner() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.q.heldStepOwner()
}

// ClearHeldStepOwner drops the anchor obligation, as the backend does once it
// has genuinely resumed that thread.
func (e *ExportedStepQueue) ClearHeldStepOwner() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.q.clearHeldStepOwner()
}

// StepExitPending reports whether a stepped-thread exit is still awaiting the
// engine's reconciliation, which is what the linux backend's resume primitives
// refuse on.
func (e *ExportedStepQueue) StepExitPending() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.q.stepExitPending
}

// AbsorbThreadExit applies the production absorb decision for a dying thread and
// reports whether that thread was retained as the reconciliation anchor. It
// mirrors the linux backend's applyAbsorb minus the ptrace call, so a
// cross-layer test exercises the real decision rather than restating it.
func (e *ExportedStepQueue) AbsorbThreadExit(tid int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	plan := e.q.planAbsorb(absorbThreadExit, tid, 0)
	if plan.stepThreadExits {
		e.q.interruptStepIfStepped(tid)
	}
	if plan.holdStepOwner {
		e.q.holdStepOwner(tid)
		return true
	}
	return false
}

func (e *ExportedStepQueue) ParkedDepth() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.q.parkedDepthForTest()
}

// ExportedGateBackend adapts a Backend to the engine's unexported
// stepThreadExitCompleter, which an external test package cannot implement
// directly. Complete receives the engine's acknowledgement that the dead step
// owner's breakpoint transaction has been reconciled — the only thing that
// opens the parked-stop gate and releases the thread held as its write anchor.
// Returning an error models a release that failed non-benignly.
type ExportedGateBackend struct {
	Backend
	Complete func() error
}

func (b *ExportedGateBackend) completeStepThreadExit() error {
	if b.Complete != nil {
		return b.Complete()
	}
	return nil
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
