//go:build linux && amd64

package debugger

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
)

type linuxResumeCall struct {
	trap    uintptr
	request uintptr
	tid     uintptr
	addr    uintptr
	signal  uintptr
	a5      uintptr
	a6      uintptr
}

func wantLinuxResume(request, tid, signal int) linuxResumeCall {
	return linuxResumeCall{
		trap:    syscall.SYS_PTRACE,
		request: uintptr(request),
		tid:     uintptr(tid),
		signal:  uintptr(signal),
	}
}

func newRecordingLinuxBackend(t *testing.T, pid int) (*linuxBackend, *[]linuxResumeCall) {
	t.Helper()
	calls := &[]linuxResumeCall{}
	b := &linuxBackend{
		pid:    pid,
		tracer: newTracerThread(),
		ptraceSyscall6Fn: func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
			*calls = append(*calls, linuxResumeCall{
				trap: trap, request: a1, tid: a2, addr: a3, signal: a4, a5: a5, a6: a6,
			})
			return 0, 0, 0
		},
	}
	t.Cleanup(b.closeTracer)
	return b, calls
}

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
}

func newStopTIDRaceBackend(t *testing.T) *stopTIDRaceBackend {
	t.Helper()
	b := &stopTIDRaceBackend{
		linuxBackend: &linuxBackend{
			pid:    1_000_001,
			tracer: newTracerThread(),
		},
		waitStarted: make(chan struct{}),
		stopWriter:  make(chan struct{}),
	}
	b.recordStop(b.pid)
	t.Cleanup(b.closeTracer)
	return b
}

func (b *stopTIDRaceBackend) Wait() (StopEvent, error) {
	close(b.waitStarted)
	for tid := b.pid + 1; ; tid++ {
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

// TestLinuxBackendRunningKillStopTIDConcurrentAccess is a production-path race
// regression:
// waitLoop publishes stops through recordStop while the engine actor executes
// running Kill -> breakpointTable.clearAll -> WriteMemory -> traceTID.
//
// Keep this test focused on the ownership boundary. It deliberately uses fake
// TIDs so ptrace writes fail quickly after reading traceTID; clearAll's
// best-effort contract leaves every entry present, giving the race detector
// many reads against the live wait-loop writer.
func TestLinuxBackendRunningKillStopTIDConcurrentAccess(t *testing.T) {
	b := newStopTIDRaceBackend(t)
	e := newEngine(b, nil)

	if err := e.dispatch(func() error {
		// Keep this on launched teardown when attached Kill gains its own
		// quiesce-before-clear transaction.
		e.proc = process{
			pid:  b.pid,
			cmd:  &exec.Cmd{Process: &os.Process{Pid: b.pid}},
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
	close(b.stopWriter)
	<-e.done
}

// TestLinuxBackendRunningMemoryStopTIDConcurrentAccess covers the other two
// traceTID readers. WriteMemory is reachable from running SetBreakpoint/
// ClearBreakpoint; ReadMemory's ptrace fallback is reachable from running
// SetBreakpoint when process_vm_readv is unavailable or short-reads.
func TestLinuxBackendRunningMemoryStopTIDConcurrentAccess(t *testing.T) {
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
			<-b.waitStarted

			for i := 0; i < 16; i++ {
				_ = tt.read(b.linuxBackend)
			}
			close(b.stopWriter)
			<-done
		})
	}
}

// TestLinuxBackendStoppedMemoryStopTIDOrdered is the stopped-state control.
// Receiving Wait's result orders recordStop before every engine-side memory
// operation, matching the real waitLoop -> stopCh -> engine-loop handoff.
func TestLinuxBackendStoppedMemoryStopTIDOrdered(t *testing.T) {
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
				b.recordStop(b.pid + 1)
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

func TestLinuxBackendContinueForwardsDeliveredSignalOnce(t *testing.T) {
	const tid = 1002
	b, calls := newRecordingLinuxBackend(t, 1001)
	b.recordDeliveredStop(StopEvent{Reason: StopSignal, TID: tid, Signal: int(syscall.SIGSEGV)})

	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("ContinueProcess() error = %v", err)
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("second ContinueProcess() error = %v", err)
	}

	want := []linuxResumeCall{
		wantLinuxResume(syscall.PTRACE_CONT, tid, int(syscall.SIGSEGV)),
		wantLinuxResume(syscall.PTRACE_CONT, tid, 0),
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("resume calls = %+v, want %+v", *calls, want)
	}
}

func TestLinuxBackendPendingSignalDoesNotTransferOrGetClearedByAnotherStop(t *testing.T) {
	const (
		signalled = 2002
		other     = 2003
	)
	b, calls := newRecordingLinuxBackend(t, 2001)
	b.recordDeliveredStop(StopEvent{Reason: StopSignal, TID: signalled, Signal: int(syscall.SIGABRT)})

	b.recordStop(other)
	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("ContinueProcess(other) error = %v", err)
	}
	b.recordStop(signalled)
	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("ContinueProcess(signalled) error = %v", err)
	}

	want := []linuxResumeCall{
		wantLinuxResume(syscall.PTRACE_CONT, other, 0),
		wantLinuxResume(syscall.PTRACE_CONT, signalled, int(syscall.SIGABRT)),
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("resume calls = %+v, want %+v", *calls, want)
	}
}

func TestLinuxBackendPauseSignalIsNeverForwarded(t *testing.T) {
	const tid = 3001
	b, calls := newRecordingLinuxBackend(t, tid)
	b.recordDeliveredStop(StopEvent{Reason: StopSignal, TID: tid, Signal: b.PauseSignal()})

	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("ContinueProcess() error = %v", err)
	}

	want := []linuxResumeCall{wantLinuxResume(syscall.PTRACE_CONT, tid, 0)}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("resume calls = %+v, want %+v", *calls, want)
	}
}

func TestLinuxBackendSingleStepForwardsOnlyMatchingTIDSignal(t *testing.T) {
	const (
		signalled = 4002
		other     = 4003
	)
	b, calls := newRecordingLinuxBackend(t, 4001)
	b.recordDeliveredStop(StopEvent{Reason: StopSignal, TID: signalled, Signal: int(syscall.SIGUSR1)})

	if err := b.SingleStep(other); err != nil {
		t.Fatalf("SingleStep(other) error = %v", err)
	}
	b.endStep()
	if err := b.SingleStep(signalled); err != nil {
		t.Fatalf("SingleStep(signalled) error = %v", err)
	}

	want := []linuxResumeCall{
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, other, 0),
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, signalled, int(syscall.SIGUSR1)),
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("resume calls = %+v, want %+v", *calls, want)
	}
}

func TestLinuxBackendFailedResumeStillConsumesPendingSignal(t *testing.T) {
	const tid = 5001
	errInjected := syscall.EIO
	b, calls := newRecordingLinuxBackend(t, tid)
	b.recordDeliveredStop(StopEvent{Reason: StopSignal, TID: tid, Signal: int(syscall.SIGSEGV)})
	b.ptraceSyscall6Fn = func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		*calls = append(*calls, linuxResumeCall{
			trap: trap, request: a1, tid: a2, addr: a3, signal: a4, a5: a5, a6: a6,
		})
		return 0, 0, errInjected
	}

	if err := b.ContinueProcess(); !errors.Is(err, errInjected) {
		t.Fatalf("ContinueProcess() error = %v, want %v", err, errInjected)
	}
	b.ptraceSyscall6Fn = func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		*calls = append(*calls, linuxResumeCall{
			trap: trap, request: a1, tid: a2, addr: a3, signal: a4, a5: a5, a6: a6,
		})
		return 0, 0, 0
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("retry ContinueProcess() error = %v", err)
	}

	want := []linuxResumeCall{
		wantLinuxResume(syscall.PTRACE_CONT, tid, int(syscall.SIGSEGV)),
		wantLinuxResume(syscall.PTRACE_CONT, tid, 0),
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("resume calls = %+v, want %+v", *calls, want)
	}
}

func TestLinuxBackendInternalResumesClearOnlyTheirPendingSignal(t *testing.T) {
	const (
		tid             = 6001
		unrelatedTID    = 6002
		unrelatedSignal = int(syscall.SIGUSR2)
	)
	b, calls := newRecordingLinuxBackend(t, tid)

	b.recordDeliveredStop(StopEvent{Reason: StopSignal, TID: tid, Signal: int(syscall.SIGSEGV)})
	b.pendingSignals.set(unrelatedTID, unrelatedSignal)
	if err := b.continueIfTraceeExists(tid, int(syscall.SIGURG)); err != nil {
		t.Fatalf("continueIfTraceeExists() error = %v", err)
	}
	if got := b.pendingSignals.take(unrelatedTID); got != unrelatedSignal {
		t.Fatalf("internal continue changed unrelated TID's signal: got %d, want %d", got, unrelatedSignal)
	}
	b.recordStop(tid)
	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("ContinueProcess() after internal continue error = %v", err)
	}

	b.recordDeliveredStop(StopEvent{Reason: StopSignal, TID: tid, Signal: int(syscall.SIGABRT)})
	b.pendingSignals.set(unrelatedTID, unrelatedSignal)
	if err := b.singleStepIfTraceeExists(tid); err != nil {
		t.Fatalf("singleStepIfTraceeExists() error = %v", err)
	}
	if got := b.pendingSignals.take(unrelatedTID); got != unrelatedSignal {
		t.Fatalf("internal single-step changed unrelated TID's signal: got %d, want %d", got, unrelatedSignal)
	}
	b.recordStop(tid)
	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("ContinueProcess() after internal step error = %v", err)
	}

	want := []linuxResumeCall{
		wantLinuxResume(syscall.PTRACE_CONT, tid, int(syscall.SIGURG)),
		wantLinuxResume(syscall.PTRACE_CONT, tid, 0),
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, tid, 0),
		wantLinuxResume(syscall.PTRACE_CONT, tid, 0),
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("resume calls = %+v, want %+v", *calls, want)
	}
}

func TestLinuxEngineCleanupDoesNotTouchWaitOwnedQueue(t *testing.T) {
	const tid = 7001
	b, _ := newRecordingLinuxBackend(t, tid)
	b.pendingSignals.set(tid, int(syscall.SIGSEGV))
	b.park(StopEvent{Reason: StopBreakpoint, TID: tid + 1})

	if err := killProcess(b, tid, nil, true); err != nil {
		t.Fatalf("killProcess() error = %v", err)
	}
	if got := b.pendingSignals.take(tid); got != 0 {
		t.Fatalf("pending signal after kill cleanup = %d, want 0", got)
	}
	if got := len(b.parked); got != 1 {
		t.Fatalf("parked stops after kill cleanup = %d, want 1 retained for the wait loop", got)
	}
	b.closeTracer()
	if got := len(b.parked); got != 1 {
		t.Fatalf("parked stops after tracer cleanup = %d, want 1 retained for the wait loop", got)
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
// #206 raises for pending signals. A held stop's signal must not become
// observable before the stop it belongs to, and it must arrive in the same
// operation that repoints traceTID at the signalled thread — otherwise the
// engine would report or suppress a signal against whichever thread happened to
// be the resume target at the time.
//
// While parked, the signal stays in the StopEvent. Delivery installs it in the
// per-TID pending map before publishing traceTID; the final resume assertion
// pins that handoff end to end.
func TestLinuxBackendHeldSignalSurfacesOnlyWithItsOwnStop(t *testing.T) {
	const (
		pid      = 2001
		stepped  = 2002
		signaled = 2003
		later    = 2004
	)

	b, calls := newRecordingLinuxBackend(t, pid)
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
	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("ContinueProcess() for delivered signal error = %v", err)
	}
	wantResume := []linuxResumeCall{wantLinuxResume(syscall.PTRACE_CONT, signaled, 11)}
	if !reflect.DeepEqual(*calls, wantResume) {
		t.Fatalf("resume calls = %+v, want delivered signal on its own tid: %+v", *calls, wantResume)
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

// TestLinuxBackendSteppedThreadDeathDeliversHeldStopBeforeExit pins the
// precedence between the two events that can end a step abnormally: the stepped
// thread dying (which lifts the gate) and the main thread exiting (which purges).
//
// Because the drain runs at the top of the Wait loop, *before* blocking in
// wait4, a stop held behind a step whose thread then died is delivered on the
// next Wait, even if the main thread's exit is already queued in the kernel
// behind it. That ordering is deliberate and is the same property that makes a
// same-address sibling resolve only after the reinstall; inverting it to peek at
// the wait queue first would defeat the fix. It is safe because a parked thread
// is still ptrace-stopped when it is delivered, and because a delivery that does
// race a dying process degrades through the engine's halt path rather than
// hanging. Once the main exit *is* observed, purge wins and nothing is delivered
// afterwards, so the engine never acts on a dead thread.
func TestLinuxBackendSteppedThreadDeathDeliversHeldStopBeforeExit(t *testing.T) {
	const (
		pid     = 1001
		stepped = 1002
		foreign = 1003
	)

	b := &linuxBackend{pid: pid}
	b.recordStop(pid)
	b.beginStep(stepped)
	b.park(StopEvent{Reason: StopBreakpoint, TID: foreign})

	// The stepped thread exits before its step completes. Wait observes that as
	// a non-main thread exit and clears the step bookkeeping.
	b.clearStepIfStepped(stepped)

	// The held stop must now be delivered rather than stranded or dropped: it is
	// a real breakpoint on a thread that is still ptrace-stopped.
	ev, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if ev.TID != foreign || ev.Reason != StopBreakpoint {
		t.Fatalf("Wait() = %+v, want the held breakpoint on tid %d after the stepped thread died", ev, foreign)
	}
	if got := b.traceTID(); got != foreign {
		t.Fatalf("traceTID() = %d, want the delivered thread %d", got, foreign)
	}

	// The main thread exiting afterwards purges anything still held, so no
	// delivery can follow the exit and target a dead thread.
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
