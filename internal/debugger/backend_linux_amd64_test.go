//go:build linux && amd64

package debugger

import (
	"os/exec"
	"sync"
	"syscall"
	"testing"
)

func TestLinuxBackendTraceTIDDefaultsToPID(t *testing.T) {
	const pid = 1001

	b := &linuxBackend{pid: pid}
	if got := b.traceTID(); got != pid {
		t.Fatalf("traceTID() = %d, want pid %d", got, pid)
	}
}

func TestLinuxBackendTraceTIDUsesLastStoppedTID(t *testing.T) {
	const (
		pid = 1001
		tid = 1002
	)

	b := &linuxBackend{pid: pid}
	b.recordStop(tid)

	if got := b.traceTID(); got != tid {
		t.Fatalf("traceTID() = %d, want stopped tid %d", got, tid)
	}
}

func TestLinuxBackendSetPIDSeedsLastStoppedTID(t *testing.T) {
	const pid = 1001

	b := &linuxBackend{}
	b.setPID(pid)

	if got := b.traceTID(); got != pid {
		t.Fatalf("traceTID() = %d, want pid %d", got, pid)
	}
}

type stopTIDRaceBackend struct {
	*linuxBackend
	waitStarted chan struct{}
	stopWriter  chan struct{}
	stopOnce    sync.Once
}

func newStopTIDRaceBackend(t *testing.T) *stopTIDRaceBackend {
	t.Helper()
	b := &stopTIDRaceBackend{
		linuxBackend: &linuxBackend{
			// Negative IDs cannot name a process, and also force ReadMemory
			// through the ptrace fallback without attempting process_vm_readv.
			pid:    -1,
			tracer: newTracerThread(),
		},
		waitStarted: make(chan struct{}),
		stopWriter:  make(chan struct{}),
	}
	b.recordStop(b.pid)
	t.Cleanup(func() {
		b.stop()
		b.closeTracer()
	})
	return b
}

func (b *stopTIDRaceBackend) stop() {
	b.stopOnce.Do(func() { close(b.stopWriter) })
}

func (b *stopTIDRaceBackend) Wait() (StopEvent, error) {
	close(b.waitStarted)
	for tid := b.pid - 1; ; tid-- {
		select {
		case <-b.stopWriter:
			return StopEvent{Reason: StopExited, TID: b.pid}, nil
		default:
			b.recordStop(tid)
		}
	}
}

func seedBreakpointEntries(e *engine, count int) {
	for i := 0; i < count; i++ {
		id := i + 1
		addr := uint64(0x400000 + i)
		entry := &breakpointEntry{
			id:            id,
			addr:          addr,
			originalBytes: []byte{0x90},
			enabled:       true,
		}
		e.bps.byID[id] = entry
		e.bps.byAddr[addr] = entry
	}
}

func TestLinuxBackendStopTIDRaceRegressions(t *testing.T) {
	t.Run("running-kill", testLinuxBackendRunningKillStopTIDConcurrentAccess)
	t.Run("running-memory", testLinuxBackendRunningMemoryStopTIDConcurrentAccess)
	t.Run("stopped-memory-ordered", testLinuxBackendStoppedMemoryStopTIDOrdered)
}

// testLinuxBackendRunningKillStopTIDConcurrentAccess is a production-path race
// regression:
// waitLoop publishes stops through recordStop while the engine actor executes
// running Kill -> breakpointTable.clearAll -> WriteMemory -> traceTID.
//
// Keep this test focused on the ownership boundary. It deliberately uses fake
// TIDs so ptrace writes fail quickly after reading traceTID; clearAll's
// best-effort contract leaves every entry present, giving the race detector
// many reads against the live wait-loop writer. The launched marker remains
// non-nil for #204's stacked ownership split, but pid zero makes process.kill
// stop before any OS signal.
func testLinuxBackendRunningKillStopTIDConcurrentAccess(t *testing.T) {
	b := newStopTIDRaceBackend(t)
	e := newEngine(b, nil)
	t.Cleanup(func() {
		b.stop()
		<-e.done
	})

	if err := e.dispatch(func() error {
		// Keep this on launched teardown when attached Kill gains its own
		// quiesce-before-clear transaction.
		e.proc = process{
			pid:  0,
			cmd:  &exec.Cmd{},
			live: true,
		}
		e.setState(stateRunning)
		seedBreakpointEntries(e, 16)
		go e.waitLoop()
		return nil
	}); err != nil {
		t.Fatalf("prepare running engine: %v", err)
	}
	<-b.waitStarted

	if err := e.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	b.stop()
	<-e.done
}

// testLinuxBackendRunningMemoryStopTIDConcurrentAccess covers the other two
// traceTID readers. WriteMemory is reachable from running SetBreakpoint/
// ClearBreakpoint; ReadMemory's ptrace fallback is reachable from running
// SetBreakpoint when process_vm_readv is unavailable or short-reads.
func testLinuxBackendRunningMemoryStopTIDConcurrentAccess(t *testing.T) {
	tests := []struct {
		name string
		read func(*linuxBackend) error
	}{
		{
			name: "write-memory",
			read: func(b *linuxBackend) error {
				return b.WriteMemory(0x400000, []byte{0x90})
			},
		},
		{
			name: "read-memory-fallback",
			read: func(b *linuxBackend) error {
				var dst [1]byte
				return b.ReadMemory(0x400000, dst[:])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newStopTIDRaceBackend(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = b.Wait()
			}()
			t.Cleanup(func() {
				b.stop()
				<-done
			})
			<-b.waitStarted

			for i := 0; i < 16; i++ {
				_ = tt.read(b.linuxBackend)
			}
			b.stop()
			<-done
		})
	}
}

// testLinuxBackendStoppedMemoryStopTIDOrdered is the stopped-state control.
// Receiving Wait's result orders recordStop before every engine-side memory
// operation, matching the real waitLoop -> stopCh -> engine-loop handoff.
func testLinuxBackendStoppedMemoryStopTIDOrdered(t *testing.T) {
	tests := []struct {
		name string
		read func(*linuxBackend) error
	}{
		{
			name: "write-memory",
			read: func(b *linuxBackend) error {
				return b.WriteMemory(0x400000, []byte{0x90})
			},
		},
		{
			name: "read-memory-fallback",
			read: func(b *linuxBackend) error {
				var dst [1]byte
				return b.ReadMemory(0x400000, dst[:])
			},
		},
		{
			name: "continue",
			read: func(b *linuxBackend) error {
				return b.ContinueProcess()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newStopTIDRaceBackend(t)
			result := make(chan error)
			go func() {
				b.recordStop(b.pid - 1)
				result <- nil
			}()
			if err := <-result; err != nil {
				t.Fatal(err)
			}

			if err := tt.read(b.linuxBackend); err == nil {
				t.Fatalf("%s unexpectedly succeeded against fake tid %d", tt.name, b.traceTID())
			} else {
				t.Logf("%s reached the ordered stopped tid: %v", tt.name, err)
			}
		})
	}
}

// The tests below pin the part of the wait-side park queue that is specific to
// this backend: WHEN lastStopTID moves. The ordering and gating rules live on
// the platform-neutral stepQueue and are covered in waitpark_test.go.

func TestLinuxBackendParkDoesNotRecordStop(t *testing.T) {
	const (
		pid     = 1001
		stepped = 1002
		foreign = 1003
	)

	b := &linuxBackend{pid: pid}
	b.beginStep(stepped)
	b.recordStop(stepped)

	b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	// Parking must leave the resume target alone: the parked thread is not the
	// thread the engine is working on, and traceTID feeds every TID-less ptrace
	// op the engine issues next.
	if got := b.traceTID(); got != stepped {
		t.Fatalf("traceTID() = %d after parking tid %d, want stepped tid %d", got, foreign, stepped)
	}
}

func TestLinuxBackendDrainParkedRecordsOnDeliveryOnly(t *testing.T) {
	const (
		pid     = 1001
		stepped = 1002
		first   = 1003
		second  = 1004
	)

	b := &linuxBackend{pid: pid}
	b.beginStep(stepped)
	b.recordStop(stepped)
	b.park(StopEvent{Reason: StopBreakpoint, TID: first})
	b.park(StopEvent{Reason: StopSignal, TID: second, Signal: 11})

	if ev, ok := b.drainParked(); ok {
		t.Fatalf("drainParked() released %+v while a step was in flight", ev)
	}
	if got := b.traceTID(); got != stepped {
		t.Fatalf("traceTID() = %d while stepping, want stepped tid %d", got, stepped)
	}

	// The step completing is what releases the queue.
	b.endStep()

	ev, ok := b.drainParked()
	if !ok {
		t.Fatal("drainParked() withheld a stop after the step completed")
	}
	if ev.TID != first {
		t.Fatalf("first drained tid = %d, want %d", ev.TID, first)
	}
	if got := b.traceTID(); got != first {
		t.Fatalf("traceTID() = %d after delivering tid %d, want the delivered thread", got, first)
	}

	ev, ok = b.drainParked()
	if !ok {
		t.Fatal("drainParked() dropped the second held stop")
	}
	if ev.Reason != StopSignal || ev.Signal != 11 {
		t.Fatalf("second drained stop = {%v, signal %d}, want {StopSignal, signal 11}", ev.Reason, ev.Signal)
	}
	if got := b.traceTID(); got != second {
		t.Fatalf("traceTID() = %d, want the delivered thread %d", got, second)
	}
	if _, ok := b.drainParked(); ok {
		t.Fatal("drainParked() returned a stop from an empty queue")
	}
}

// TestLinuxBackendHeldSignalSurfacesOnlyWithItsOwnStop pins the ordering issue
// #204 raises for pending signals. A held stop's signal must not become
// observable before the stop it belongs to, and it must arrive in the same
// operation that repoints traceTID at the signalled thread — otherwise the
// engine would report or suppress a signal against whichever thread happened to
// be the resume target at the time.
//
// The queue is immune to that class by construction rather than by ordering
// discipline: the signal is a field of the parked StopEvent, so there is no
// separate signal record that could be written at park time and later
// overwritten. This test is what stops a future refactor from hoisting the
// signal into backend state, where it would need — and could silently lose —
// that discipline.
func TestLinuxBackendHeldSignalSurfacesOnlyWithItsOwnStop(t *testing.T) {
	const (
		pid      = 2001
		stepped  = 2002
		signaled = 2003
		later    = 2004
	)

	b := &linuxBackend{pid: pid}
	b.beginStep(stepped)
	b.recordStop(stepped)

	b.park(StopEvent{Reason: StopSignal, TID: signaled, Signal: 11})
	b.park(StopEvent{Reason: StopBreakpoint, TID: later})

	// While the step is in flight the signalled thread is invisible: the engine
	// is still working on, and must still resume, the stepped thread.
	if got := b.traceTID(); got != stepped {
		t.Fatalf("traceTID() = %d with a signal held, want stepped tid %d", got, stepped)
	}

	b.endStep()

	ev, ok := b.drainParked()
	if !ok {
		t.Fatal("drainParked() withheld the signal stop after the step completed")
	}
	if ev.TID != signaled || ev.Reason != StopSignal || ev.Signal != 11 {
		t.Fatalf("delivered %+v, want {StopSignal, tid %d, signal 11}", ev, signaled)
	}
	// The signal and the resume target must move together. If traceTID still
	// named the stepped thread here, the engine would handle signalled's stop
	// but resume — or POKEDATA into — the wrong thread.
	if got := b.traceTID(); got != signaled {
		t.Fatalf("traceTID() = %d when delivering the signal, want the signalled thread %d", got, signaled)
	}

	// The next held stop carries no signal, and delivering it must not leave the
	// previous signal attached to it.
	ev, ok = b.drainParked()
	if !ok {
		t.Fatal("drainParked() dropped the stop queued behind the signal")
	}
	if ev.TID != later || ev.Reason != StopBreakpoint || ev.Signal != 0 {
		t.Fatalf("delivered %+v, want {StopBreakpoint, tid %d, signal 0}", ev, later)
	}
}

// TestLinuxBackendSteppedThreadDeathReconcilesBeforeDelivery pins the exact-TID
// boundary: the held thread becomes the memory-write anchor, but its real stop
// cannot be dequeued until the engine confirms the removed trap was reconciled.
func TestLinuxBackendSteppedThreadDeathReconcilesBeforeDelivery(t *testing.T) {
	const (
		pid     = 1001
		stepped = 1002
		foreign = 1003
	)

	b := &linuxBackend{pid: pid}
	b.recordStop(pid)
	b.beginStep(stepped)
	b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	b.interruptStepIfStepped(stepped)

	ev, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if ev.TID != foreign || ev.Reason != StopStepThreadExited {
		t.Fatalf("Wait() = %+v, want the reconciliation boundary on tid %d", ev, foreign)
	}
	if got := b.traceTID(); got != foreign {
		t.Fatalf("traceTID() = %d, want reconciliation anchored to held tid %d", got, foreign)
	}
	if b.parkedDepthForTest() != 1 {
		t.Fatal("the reconciliation boundary dequeued the held stop")
	}

	b.completeStepThreadExit()
	ev, err = b.Wait()
	if err != nil {
		t.Fatalf("Wait() after reconciliation error = %v", err)
	}
	if ev.TID != foreign || ev.Reason != StopBreakpoint {
		t.Fatalf("Wait() after reconciliation = %+v, want held breakpoint on tid %d", ev, foreign)
	}

	b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})
	b.purge()
	if ev, ok := b.drainParked(); ok {
		t.Fatalf("drainParked() released %+v after the main-thread purge", ev)
	}
}

// TestLinuxBackendWaitDrainsBeforeBlocking pins the wiring: Wait must consult
// the queue at the top of its loop, before blocking in wait4, so a held stop
// becomes the engine's next stop as soon as the step it collided with is done.
func TestLinuxBackendWaitDrainsBeforeBlocking(t *testing.T) {
	const (
		pid     = 1001
		foreign = 1003
	)

	b := &linuxBackend{pid: pid}
	b.recordStop(pid)
	b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	ev, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if ev.TID != foreign || ev.Reason != StopBreakpoint {
		t.Fatalf("Wait() = %+v, want the held breakpoint on tid %d", ev, foreign)
	}
	if got := b.traceTID(); got != foreign {
		t.Fatalf("traceTID() = %d, want the delivered thread %d", got, foreign)
	}
}

// TestLinuxBackendApplyAbsorbCarriesOutThePlan pins the half of the absorb path
// that planAbsorb cannot: that Wait actually acts on the plan it was given.
//
// The observable consequences are the reconciliation boundary, the step gate,
// and the re-arm counter, so this asserts all three. Dropping the
// stepThreadExits handling latches the hardware-step gate forever — held stops
// never drain and Wait never returns again — opening the gate at thread death
// lets a held stop bypass breakpoint reconciliation, and dropping the re-arm
// leaves a cancelled hardware step behind the same latch.
//
// The resumes are issued against thread ids that do not exist, so ptrace fails
// with ESRCH and the backend swallows it; the bookkeeping under test still runs.
func TestLinuxBackendApplyAbsorbCarriesOutThePlan(t *testing.T) {
	const (
		pid     = 7001
		stepped = 7002
		foreign = 7003
		sigurg  = 23
	)

	newBackend := func() *linuxBackend {
		b := &linuxBackend{pid: pid, tracer: newTracerThread()}
		t.Cleanup(b.closeTracer)
		b.recordStop(pid)
		b.beginStep(stepped)
		b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})
		return b
	}

	// A dead step owner does not simply release the gate: the engine still owns
	// the breakpoint that was lifted for that step, and delivering the held stop
	// before it is reinstalled loses the trap for good. The handoff is therefore
	// three-phase, and each phase is asserted here because skipping any one of
	// them either freezes the wait loop or drops a breakpoint silently. In
	// particular, reporting the boundary must NOT release the stop on its own:
	// only the engine's acknowledgement may, or the reinstall is racing a
	// sibling that has already been handed to the engine.
	t.Run("a dying stepped thread hands the gate to engine reconciliation", func(t *testing.T) {
		b := newBackend()
		if err := b.applyAbsorb(b.planAbsorb(absorbThreadExit, stepped, 0), stepped); err != nil {
			t.Fatalf("applyAbsorb(absorbThreadExit) error = %v", err)
		}
		if b.stepping {
			t.Fatal("hardware-step ownership survived its thread's death")
		}
		if ev, ok := b.releasable(); ok {
			t.Fatalf("released %+v before breakpoint reconciliation", ev)
		}

		boundary, ok := b.stepExitBoundary()
		if !ok || boundary.Reason != StopStepThreadExited || boundary.TID != foreign {
			t.Fatalf("stepExitBoundary() = (%+v, %t), want StopStepThreadExited anchored to the held stop's tid %d",
				boundary, ok, foreign)
		}
		if ev, ok := b.releasable(); ok {
			t.Fatalf("reporting the boundary released %+v; only the engine's acknowledgement may", ev)
		}

		b.completeStepThreadExit()
		ev, ok := b.releasable()
		if !ok || ev.TID != foreign {
			t.Fatalf("released (%+v, %t), want the held stop on tid %d after reconciliation", ev, ok, foreign)
		}
	})

	t.Run("absorbing on the stepped thread re-arms the step and holds the gate", func(t *testing.T) {
		b := newBackend()
		before := b.stepRearmCount()
		if err := b.applyAbsorb(b.planAbsorb(absorbPreempt, stepped, sigurg), stepped); err != nil {
			t.Fatalf("applyAbsorb(absorbPreempt) error = %v", err)
		}
		if after := b.stepRearmCount(); after != before+1 {
			t.Fatalf("step re-arms = %d, want %d: the consumed step was not re-armed", after, before+1)
		}
		if _, ok := b.releasable(); ok {
			t.Fatal("the gate opened while the re-armed step is still in flight")
		}
	})

	t.Run("absorbing on any other thread neither re-arms nor opens the gate", func(t *testing.T) {
		b := newBackend()
		before := b.stepRearmCount()
		if err := b.applyAbsorb(b.planAbsorb(absorbPreempt, foreign, sigurg), foreign); err != nil {
			t.Fatalf("applyAbsorb(absorbPreempt) error = %v", err)
		}
		if after := b.stepRearmCount(); after != before {
			t.Fatalf("step re-arms = %d, want %d: a foreign thread must not re-arm the step", after, before)
		}
		if _, ok := b.releasable(); ok {
			t.Fatal("a foreign absorb opened the step gate")
		}
	})
}

// resumeOp records one ptrace resume the wait loop issued.
type resumeOp struct {
	step   bool
	tid    int
	signal int
}

// scriptedWait drives linuxBackend.Wait over a fixed list of wait statuses,
// capturing the resume each branch performs instead of touching the kernel.
type scriptedWait struct {
	stops []scriptedStop
	next  int
	ops   []resumeOp
}

type scriptedStop struct {
	tid    int
	status syscall.WaitStatus
}

func (s *scriptedWait) install(b *linuxBackend) {
	b.waitFn = func(ws *syscall.WaitStatus) (int, error) {
		if s.next >= len(s.stops) {
			return 0, syscall.ECHILD
		}
		stop := s.stops[s.next]
		s.next++
		*ws = stop.status
		return stop.tid, nil
	}
	b.contFn = func(tid int, signal int) error {
		s.ops = append(s.ops, resumeOp{tid: tid, signal: signal})
		return nil
	}
	b.stepFn = func(tid int) error {
		s.ops = append(s.ops, resumeOp{step: true, tid: tid})
		return nil
	}
}

// stoppedAt builds the wait status the kernel reports for a ptrace-stop.
// PTRACE_EVENT stops ride SIGTRAP with the event number in the high half.
func stoppedAt(sig syscall.Signal, event int) syscall.WaitStatus {
	return syscall.WaitStatus(uint32(event)<<16 | uint32(sig)<<8 | 0x7f)
}

func exitedWith(code int) syscall.WaitStatus {
	return syscall.WaitStatus(uint32(code) << 8)
}

// TestLinuxWaitResumesTheSteppedThreadWithASingleStep executes every wait-loop
// branch that resumes a thread inline and pins the primitive it used.
//
// This is rule 7's regression gate at the call site. planAbsorb decides that a
// stop absorbed on the stepped thread must be re-armed with PTRACE_SINGLESTEP
// rather than continued, but a branch is free to ignore it: before these cases
// existed, rewriting any of them back into a bare continueIfTraceeExists passed
// the whole unit suite, and the cost is a hung tracee — the cancelled step never
// completes, stepping/stepTID stay latched, and every later foreign stop is held
// forever inside Wait.
//
// Each case scripts one absorbed stop on the thread being stepped, then the main
// thread exiting so Wait terminates.
func TestLinuxWaitResumesTheSteppedThreadWithASingleStep(t *testing.T) {
	const (
		pid     = 9001
		stepped = 9002
	)

	cases := []struct {
		name   string
		status syscall.WaitStatus
	}{
		{"clone", stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE)},
		{"async preemption SIGURG", stoppedAt(syscall.SIGURG, 0)},
		{"SIGCONT", stoppedAt(syscall.SIGCONT, 0)},
		// The non-main SIGSTOP branch reads as a brand-new thread's initial
		// group-stop, and a brand-new thread is never the one being stepped.
		// That is an argument about how the branch is normally reached, not a
		// guard: it keys off `tid != pid`, so any non-main SIGSTOP lands here,
		// including one on the thread under an active step. The branch is
		// deliberately routed through the shared helper for that reason, and
		// this case is what keeps it there.
		{"non-main SIGSTOP", stoppedAt(syscall.SIGSTOP, 0)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &linuxBackend{pid: pid}
			script := &scriptedWait{stops: []scriptedStop{
				{tid: stepped, status: tc.status},
				{tid: pid, status: exitedWith(0)},
			}}
			script.install(b)
			b.beginStep(stepped)

			ev, err := b.Wait()
			if err != nil {
				t.Fatalf("Wait() error = %v", err)
			}
			if ev.Reason != StopExited {
				t.Fatalf("Wait() = %+v, want the main thread's exit", ev)
			}
			if len(script.ops) != 1 {
				t.Fatalf("resume ops = %+v, want exactly one", script.ops)
			}
			if op := script.ops[0]; !op.step || op.tid != stepped {
				t.Fatalf("resumed with %+v; absorbing on the stepped thread must "+
					"re-arm its single step, or the step is cancelled while the "+
					"gate stays latched and Wait hangs", op)
			}
		})
	}
}

// TestLinuxWaitPreservesTheSignalOfAHeldStop executes the park site's signal
// capture rather than seeding a queue entry that already carries one.
//
// A held stop is replayed to the engine verbatim once the step completes, so the
// signal number has to survive being held. Every other test of this reaches the
// queue through park() with a StopEvent it built itself, which cannot tell
// whether production copied the signal off the wait status or dropped it: the
// value under test is supplied by the test. Driving Wait means the number comes
// from the scripted wait status through the real `Signal: int(sig)` copy, so
// zeroing that field fails here.
//
// The exact value is asserted, not merely "nonzero": a stop reported with the
// wrong signal is as wrong as one reported with none.
func TestLinuxWaitPreservesTheSignalOfAHeldStop(t *testing.T) {
	const (
		pid     = 9501
		stepped = 9502
		foreign = 9503
	)

	b := &linuxBackend{pid: pid}
	script := &scriptedWait{stops: []scriptedStop{
		{tid: foreign, status: stoppedAt(syscall.SIGUSR1, 0)},
		{tid: stepped, status: stoppedAt(syscall.SIGTRAP, 0)},
	}}
	script.install(b)
	b.recordStop(pid)
	b.beginStep(stepped)

	ev, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if ev.Reason != StopSingleStep || ev.TID != stepped {
		t.Fatalf("Wait() = %+v, want the step completing on tid %d", ev, stepped)
	}

	ev, err = b.Wait()
	if err != nil {
		t.Fatalf("second Wait() error = %v", err)
	}
	if ev.TID != foreign {
		t.Fatalf("second Wait() = %+v, want the held stop on tid %d", ev, foreign)
	}
	if ev.Signal != int(syscall.SIGUSR1) {
		t.Fatalf("held stop replayed with signal %d, want %d (SIGUSR1): the "+
			"signal a thread stopped with must survive being held, or the "+
			"engine decides what to do about a signal it can no longer see",
			ev.Signal, int(syscall.SIGUSR1))
	}
}

// TestLinuxWaitDeliversTheSignalItStoppedOn gates the signal capture on the
// path that returns a stop straight to the engine instead of holding it. That
// value is not decoration: the engine tells a Pause interrupt from an ordinary
// signal by comparing it against PauseSignal(), so a stop delivered with its
// signal zeroed is not a vaguer version of the truth, it is a different stop.
// The held-stop capture is a separate line with its own gate, above.
func TestLinuxWaitDeliversTheSignalItStoppedOn(t *testing.T) {
	const (
		pid     = 9601
		stepped = 9602
		worker  = 9603
	)

	tests := []struct {
		name   string
		inStep bool
		tid    int
		signal syscall.Signal
	}{
		{name: "no step in flight", tid: worker, signal: syscall.SIGUSR1},
		// G6: the stepped thread's own signal is the outcome of its step, so it
		// is delivered rather than held — and it has to arrive with its signal
		// or the engine cannot tell what stopped it.
		{name: "the stepped thread's own signal", inStep: true, tid: stepped, signal: syscall.SIGUSR2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &linuxBackend{pid: pid}
			script := &scriptedWait{stops: []scriptedStop{
				{tid: tc.tid, status: stoppedAt(tc.signal, 0)},
			}}
			script.install(b)
			b.recordStop(pid)
			if tc.inStep {
				b.beginStep(stepped)
			}

			ev, err := b.Wait()
			if err != nil {
				t.Fatalf("Wait() error = %v", err)
			}
			if ev.Reason != StopSignal || ev.TID != tc.tid {
				t.Fatalf("Wait() = %+v, want a signal stop on tid %d", ev, tc.tid)
			}
			if ev.Signal != int(tc.signal) {
				t.Fatalf("Wait() delivered signal %d, want %d: the engine "+
					"compares this against PauseSignal() to tell a Pause "+
					"interrupt from an ordinary signal", ev.Signal, int(tc.signal))
			}
		})
	}
}

// TestLinuxWaitContinuesThreadsThatAreNotBeingStepped is the converse gate: the
// same branches must NOT single-step a thread that is not the stepped one, and
// must forward the signal that thread stopped with.
func TestLinuxWaitContinuesThreadsThatAreNotBeingStepped(t *testing.T) {
	const (
		pid     = 9101
		stepped = 9102
		other   = 9103
	)

	cases := []struct {
		name   string
		status syscall.WaitStatus
		signal int
	}{
		{"clone", stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE), 0},
		{"new thread group-stop", stoppedAt(syscall.SIGSTOP, 0), 0},
		{"async preemption SIGURG", stoppedAt(syscall.SIGURG, 0), int(syscall.SIGURG)},
		{"SIGCONT", stoppedAt(syscall.SIGCONT, 0), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &linuxBackend{pid: pid}
			script := &scriptedWait{stops: []scriptedStop{
				{tid: other, status: tc.status},
				{tid: pid, status: exitedWith(0)},
			}}
			script.install(b)
			b.beginStep(stepped)

			if _, err := b.Wait(); err != nil {
				t.Fatalf("Wait() error = %v", err)
			}
			if len(script.ops) != 1 {
				t.Fatalf("resume ops = %+v, want exactly one", script.ops)
			}
			op := script.ops[0]
			if op.step {
				t.Fatalf("resumed tid %d with a single step: only the stepped "+
					"thread may be re-armed", op.tid)
			}
			if op.tid != other || op.signal != tc.signal {
				t.Fatalf("resumed %+v, want a continue of tid %d with signal %d",
					op, other, tc.signal)
			}
		})
	}
}

// TestLinuxWaitAbortsTheStepItCannotResume covers the two branches that must
// refuse to absorb a stop on the stepped thread. exec throws away the memory
// holding the saved instruction bytes and the trap; an event we never enabled
// has no understood stop shape. Re-arming would assume that shape and continuing
// would resume a tracee whose software breakpoint is still out of memory, so
// Wait clears the step and surfaces the error instead of guessing.
func TestLinuxWaitAbortsTheStepItCannotResume(t *testing.T) {
	const (
		pid     = 9201
		stepped = 9202
		foreign = 9203
	)

	cases := []struct {
		name  string
		event int
	}{
		{"exec", syscall.PTRACE_EVENT_EXEC},
		{"an event we never enabled", syscall.PTRACE_EVENT_VFORK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &linuxBackend{pid: pid}
			script := &scriptedWait{stops: []scriptedStop{
				{tid: stepped, status: stoppedAt(syscall.SIGTRAP, tc.event)},
			}}
			script.install(b)
			b.beginStep(stepped)
			b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

			if _, err := b.Wait(); err == nil {
				t.Fatal("Wait() succeeded; a step that can be neither completed " +
					"nor re-armed must surface an error")
			}
			if len(script.ops) != 0 {
				t.Fatalf("resume ops = %+v, want none: the tracee must not run on "+
					"with its software breakpoint still out of memory", script.ops)
			}
			if b.stepping {
				t.Fatal("the step is still latched after the abort: every later " +
					"foreign stop would be held forever")
			}
			if b.parkedDepthForTest() != 0 {
				t.Fatal("held stops survived the abort")
			}
		})
	}
}

// TestLinuxWaitReportsReconciliationWhenTheSteppedThreadDies executes both
// kernel death shapes and requires the internal boundary before held delivery.
func TestLinuxWaitReportsReconciliationWhenTheSteppedThreadDies(t *testing.T) {
	const (
		pid     = 9301
		stepped = 9302
		foreign = 9303
	)

	cases := []struct {
		name   string
		status syscall.WaitStatus
	}{
		{"reported by PTRACE_EVENT_EXIT", stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_EXIT)},
		{"reaped", exitedWith(0)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &linuxBackend{pid: pid}
			script := &scriptedWait{stops: []scriptedStop{
				{tid: stepped, status: tc.status},
			}}
			script.install(b)
			b.beginStep(stepped)
			b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

			ev, err := b.Wait()
			if err != nil {
				t.Fatalf("Wait() error = %v", err)
			}
			if ev.TID != foreign || ev.Reason != StopStepThreadExited {
				t.Fatalf("Wait() = %+v, want a reconciliation boundary anchored to tid %d", ev, foreign)
			}
			if b.parkedDepthForTest() != 1 {
				t.Fatal("held stop drained before engine reconciliation")
			}
			if b.stepExitCount() != 1 {
				t.Fatalf("stepExitCount() = %d, want 1", b.stepExitCount())
			}

			b.completeStepThreadExit()
			ev, err = b.Wait()
			if err != nil {
				t.Fatalf("Wait() after reconciliation error = %v", err)
			}
			if ev.TID != foreign || ev.Reason != StopBreakpoint {
				t.Fatalf("Wait() after reconciliation = %+v, want held stop on tid %d", ev, foreign)
			}
		})
	}
}

// TestLinuxWaitHoldsTheFirstStopAfterTheStepOwnerDies covers the no-queue-yet
// ordering: a later user stop must become the reconciliation anchor rather than
// slipping through because hardware stepping was already cleared.
func TestLinuxWaitHoldsTheFirstStopAfterTheStepOwnerDies(t *testing.T) {
	const (
		pid     = 9401
		stepped = 9402
		foreign = 9403
	)

	b := &linuxBackend{pid: pid}
	script := &scriptedWait{stops: []scriptedStop{
		{tid: stepped, status: stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_EXIT)},
		{tid: foreign, status: stoppedAt(syscall.SIGTRAP, 0)},
	}}
	script.install(b)
	b.beginStep(stepped)

	ev, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if ev.Reason != StopStepThreadExited || ev.TID != foreign {
		t.Fatalf("Wait() = %+v, want reconciliation anchored to first later stop on tid %d", ev, foreign)
	}
	if b.parkedDepthForTest() != 1 {
		t.Fatal("first post-death user stop was not retained behind reconciliation")
	}
}

// TestLinuxWaitClaimsTheResumeTargetOnlyOnDelivery pins G4 through the wait loop
// itself: parking a stop must not make its thread the current stop.
//
// lastStopTID is what the next TID-less ContinueProcess and every memory write
// target. A parked thread is stopped but the engine has not been told about it,
// so claiming it at park time points those operations at a thread the engine is
// not working on — while the thread it IS working on, the one mid-step, is left
// without a resume target. Recording only on delivery keeps the invariant that
// the delivered stop's TID is the resume target.
//
// The resume target is sampled from inside the wait loop, because a claim made
// at park time is otherwise overwritten by the next delivery before any test
// could see it — which is exactly why this went unnoticed.
func TestLinuxWaitClaimsTheResumeTargetOnlyOnDelivery(t *testing.T) {
	const (
		pid     = 9401
		stepped = 9402
		foreign = 9403
	)

	b := &linuxBackend{pid: pid}
	script := &scriptedWait{stops: []scriptedStop{
		{tid: foreign, status: stoppedAt(syscall.SIGTRAP, 0)},
		{tid: stepped, status: stoppedAt(syscall.SIGTRAP, 0)},
	}}
	script.install(b)

	var targets []int
	inner := b.waitFn
	b.waitFn = func(ws *syscall.WaitStatus) (int, error) {
		targets = append(targets, b.traceTID())
		return inner(ws)
	}

	b.recordStop(pid)
	b.beginStep(stepped)

	ev, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if ev.Reason != StopSingleStep || ev.TID != stepped {
		t.Fatalf("Wait() = %+v, want the step completing on tid %d; the foreign "+
			"breakpoint must stay held", ev, stepped)
	}
	if len(targets) != 2 {
		t.Fatalf("sampled %d resume targets, want 2", len(targets))
	}
	if targets[1] == foreign {
		t.Fatalf("parking the stop on tid %d made it the resume target: the "+
			"engine would resume a thread it was never told about, and the "+
			"thread it is stepping would be left without one", foreign)
	}
	if targets[1] != pid {
		t.Fatalf("resume target after parking = %d, want it unchanged at %d",
			targets[1], pid)
	}
	if got := b.traceTID(); got != stepped {
		t.Fatalf("resume target = %d, want %d after the step was delivered", got, stepped)
	}
	if b.parkedDepthForTest() != 1 {
		t.Fatalf("held stops = %d, want the foreign breakpoint still held",
			b.parkedDepthForTest())
	}

	ev, err = b.Wait()
	if err != nil {
		t.Fatalf("second Wait() error = %v", err)
	}
	if ev.TID != foreign || ev.Reason != StopBreakpoint {
		t.Fatalf("second Wait() = %+v, want the held breakpoint on tid %d", ev, foreign)
	}
	if got := b.traceTID(); got != foreign {
		t.Fatalf("resume target = %d, want %d: a delivered stop must become the "+
			"resume target", got, foreign)
	}
}
