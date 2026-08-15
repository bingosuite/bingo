//go:build linux && amd64

package debugger

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

//go:embed *linux*.go
var linuxSources embed.FS

type scriptedExactWait4 struct {
	mu      sync.Mutex
	pending map[int][]syscall.WaitStatus
	calls   []int
}

func newScriptedExactWait4() *scriptedExactWait4 {
	return &scriptedExactWait4{pending: make(map[int][]syscall.WaitStatus)}
}

func (s *scriptedExactWait4) add(tid int, statuses ...syscall.WaitStatus) {
	s.mu.Lock()
	s.pending[tid] = append(s.pending[tid], statuses...)
	s.mu.Unlock()
}

func (s *scriptedExactWait4) wait4(tid int, status *syscall.WaitStatus, options int, _ *syscall.Rusage) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, tid)
	if options != syscall.WNOHANG|syscall.WALL {
		return 0, fmt.Errorf("options = %x", options)
	}
	if len(s.pending[tid]) == 0 {
		return 0, nil
	}
	*status = s.pending[tid][0]
	s.pending[tid] = s.pending[tid][1:]
	return tid, nil
}

func nextBrokerResult(t *testing.T, owner *linuxWaitOwner) linuxWaitResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := owner.next(ctx)
	if err != nil {
		t.Fatalf("next() error = %v", err)
	}
	return result
}

func TestLinuxWaitBrokerRoutesExactTIDsWithoutConsumingUnrelatedChildren(t *testing.T) {
	const (
		firstTID     = 11001
		secondTID    = 11002
		unrelatedTID = 11003
	)
	wait4 := newScriptedExactWait4()
	wait4.add(firstTID, stoppedAt(syscall.SIGTRAP, 0))
	wait4.add(secondTID, stoppedAt(syscall.SIGUSR1, 0))
	wait4.add(unrelatedTID, exitedWith(77))

	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	first := broker.newOwner()
	second := broker.newOwner()
	if _, err := first.register(firstTID); err != nil {
		t.Fatal(err)
	}
	if _, err := second.register(secondTID); err != nil {
		t.Fatal(err)
	}

	if !broker.scan() {
		t.Fatal("scan reported no progress")
	}
	if got := nextBrokerResult(t, first); got.tid != firstTID {
		t.Fatalf("first owner received tid %d, want %d", got.tid, firstTID)
	}
	if got := nextBrokerResult(t, second); got.tid != secondTID {
		t.Fatalf("second owner received tid %d, want %d", got.tid, secondTID)
	}

	wait4.mu.Lock()
	calls := append([]int(nil), wait4.calls...)
	unrelated := append([]syscall.WaitStatus(nil), wait4.pending[unrelatedTID]...)
	wait4.mu.Unlock()
	if !reflect.DeepEqual(calls, []int{firstTID, secondTID}) {
		t.Fatalf("wait4 targets = %v, want only registered tids", calls)
	}
	if len(unrelated) != 1 || unrelated[0].ExitStatus() != 77 {
		t.Fatalf("unrelated child status was consumed: %v", unrelated)
	}
}

func TestLinuxWaitBrokerClaimsAStatusThatPredatesRegistration(t *testing.T) {
	const tid = 11101
	wait4 := newScriptedExactWait4()
	wait4.add(tid, stoppedAt(syscall.SIGTRAP, 0))

	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	owner := broker.newOwner()
	if _, err := owner.register(tid); err != nil {
		t.Fatal(err)
	}
	broker.scan()

	result := nextBrokerResult(t, owner)
	if result.tid != tid || !result.status.Stopped() {
		t.Fatalf("result = %+v, want pending initial stop for tid %d", result, tid)
	}
}

func TestLinuxWaitBrokerRetiresOnlyAfterTheFinalStatus(t *testing.T) {
	const tid = 11201
	wait4 := newScriptedExactWait4()
	wait4.add(tid,
		stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_EXIT),
		exitedWith(31),
	)
	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	owner := broker.newOwner()
	if _, err := owner.register(tid); err != nil {
		t.Fatal(err)
	}

	broker.scan()
	first := nextBrokerResult(t, owner)
	if first.status.TrapCause() != syscall.PTRACE_EVENT_EXIT {
		t.Fatalf("first status = %v, want PTRACE_EVENT_EXIT", first.status)
	}
	if broker.registrationCount() != 1 {
		t.Fatal("an event-exit stop retired the TID before its final wait status")
	}

	broker.scan()
	final := nextBrokerResult(t, owner)
	if !final.status.Exited() || final.status.ExitStatus() != 31 {
		t.Fatalf("final status = %v, want exit 31", final.status)
	}
	if broker.registrationCount() != 0 {
		t.Fatal("final status did not retire the TID")
	}
	if _, err := owner.next(context.Background()); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("next after retirement = %v, want ECHILD", err)
	}
}

func TestLinuxWaitBrokerRejectsLiveCrossOwnerAliasingAndAllowsReuseAfterRetirement(t *testing.T) {
	const tid = 11301
	wait4 := newScriptedExactWait4()
	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	first := broker.newOwner()
	second := broker.newOwner()
	if _, err := first.register(tid); err != nil {
		t.Fatal(err)
	}
	if _, err := second.register(tid); err == nil {
		t.Fatal("second owner registered a live TID already owned by the first")
	}

	wait4.add(tid, exitedWith(0))
	broker.scan()
	_ = nextBrokerResult(t, first)
	if _, err := second.register(tid); err != nil {
		t.Fatalf("register reused tid after retirement: %v", err)
	}
}

func TestLinuxWaitBrokerKeepsReapingAfterItsConsumerCloses(t *testing.T) {
	const tid = 11401
	wait4 := newScriptedExactWait4()
	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	owner := broker.newOwner()
	if _, err := owner.register(tid); err != nil {
		t.Fatal(err)
	}
	owner.close()
	wait4.add(tid, signaledBy(syscall.SIGKILL))
	broker.scan()

	if broker.registrationCount() != 0 {
		t.Fatal("closed owner abandoned a final status instead of reaping it")
	}
}

func TestLinuxWaitBrokerClosedOwnerCannotStealALaterInitialStop(t *testing.T) {
	const (
		retiringTID = 11411
		newTID      = 11412
	)
	wait4 := newScriptedExactWait4()
	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	retiring := broker.newOwner()
	next := broker.newOwner()
	if _, err := retiring.register(retiringTID); err != nil {
		t.Fatal(err)
	}
	retiring.close()
	if _, err := next.register(newTID); err != nil {
		t.Fatal(err)
	}
	wait4.add(newTID, stoppedAt(syscall.SIGTRAP, 0))

	broker.scan()
	result := nextBrokerResult(t, next)
	if result.tid != newTID || !result.status.Stopped() {
		t.Fatalf("new owner result = %+v, want initial stop for tid %d", result, newTID)
	}

	wait4.mu.Lock()
	calls := append([]int(nil), wait4.calls...)
	wait4.mu.Unlock()
	if !reflect.DeepEqual(calls, []int{retiringTID, newTID}) {
		t.Fatalf("wait4 targets = %v, want exact retiring and new tids", calls)
	}
}

func TestLinuxWaitOwnerReleaseIsGenerationCheckedAndPurgesQueuedStops(t *testing.T) {
	const tid = 11421
	wait4 := newScriptedExactWait4()
	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	owner := broker.newOwner()
	firstGeneration, err := owner.register(tid)
	if err != nil {
		t.Fatal(err)
	}
	wait4.add(tid, stoppedAt(syscall.SIGTRAP, 0))
	broker.scan()

	if owner.release(tid, firstGeneration+1) {
		t.Fatal("release accepted the wrong registration generation")
	}
	if !owner.release(tid, firstGeneration) {
		t.Fatal("release rejected the live registration generation")
	}
	if _, ok, err := owner.tryNext(); ok || !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("tryNext after release = ok:%v err:%v, want empty ECHILD", ok, err)
	}

	secondGeneration, err := owner.register(tid)
	if err != nil {
		t.Fatal(err)
	}
	if secondGeneration == firstGeneration {
		t.Fatal("re-registration reused the released generation")
	}
}

func TestLinuxWaitBrokerStaleScanCannotConsumeReplacementGenerationStop(t *testing.T) {
	const tid = 11426
	wait4 := newScriptedExactWait4()
	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	owner := broker.newOwner()
	firstGeneration, err := owner.register(tid)
	if err != nil {
		t.Fatal(err)
	}

	stale := broker.snapshot()
	if len(stale) != 1 {
		t.Fatalf("snapshot registrations = %d, want 1", len(stale))
	}
	scanPaused := make(chan struct{})
	resumeScan := make(chan struct{})
	scanResult := make(chan bool, 1)
	go func() {
		close(scanPaused)
		<-resumeScan
		scanResult <- broker.scanRegistration(stale[0])
	}()
	<-scanPaused

	if !owner.release(tid, firstGeneration) {
		t.Fatal("release rejected the original registration")
	}
	secondGeneration, err := owner.register(tid)
	if err != nil {
		t.Fatal(err)
	}
	wait4.add(tid, stoppedAt(syscall.SIGTRAP, 0))
	close(resumeScan)
	if <-scanResult {
		t.Fatal("stale scan reported progress for a replacement registration")
	}

	wait4.mu.Lock()
	staleCalls := append([]int(nil), wait4.calls...)
	wait4.mu.Unlock()
	if len(staleCalls) != 0 {
		t.Fatalf("stale scan called wait4 for replacement generation: %v", staleCalls)
	}

	if !broker.scan() {
		t.Fatal("fresh scan reported no progress")
	}
	result := nextBrokerResult(t, owner)
	if result.tid != tid || result.generation != secondGeneration || !result.status.Stopped() {
		t.Fatalf("replacement result = %+v, want stopped tid %d generation %d",
			result, tid, secondGeneration)
	}
}

func TestLinuxWaitBrokerReleaseCannotCrossValidatedWait(t *testing.T) {
	const tid = 11427
	wait4 := newScriptedExactWait4()
	waitEntered := make(chan struct{})
	releaseWait := make(chan struct{})
	var gateOnce sync.Once
	var broker *linuxWaitBroker
	gatedWait4 := func(pid int, status *syscall.WaitStatus, options int, rusage *syscall.Rusage) (int, error) {
		if broker.mu.TryLock() {
			broker.mu.Unlock()
			t.Error("broker lock was released between generation validation and wait4")
		}
		gateOnce.Do(func() {
			close(waitEntered)
			<-releaseWait
		})
		return wait4.wait4(pid, status, options, rusage)
	}
	broker = newLinuxWaitBrokerWithRunner(gatedWait4, false)
	owner := broker.newOwner()
	firstGeneration, err := owner.register(tid)
	if err != nil {
		t.Fatal(err)
	}

	scanDone := make(chan bool, 1)
	go func() { scanDone <- broker.scan() }()
	<-waitEntered

	releaseDone := make(chan bool, 1)
	releaseStarted := make(chan struct{})
	go func() {
		close(releaseStarted)
		releaseDone <- owner.release(tid, firstGeneration)
	}()
	<-releaseStarted
	select {
	case released := <-releaseDone:
		close(releaseWait)
		<-scanDone
		t.Fatalf("release completed across a validated wait4 call: %v", released)
	case <-time.After(250 * time.Millisecond):
	}
	close(releaseWait)
	if <-scanDone {
		t.Fatal("empty validated scan reported progress")
	}
	if !<-releaseDone {
		t.Fatal("release rejected the original registration after the scan completed")
	}

	secondGeneration, err := owner.register(tid)
	if err != nil {
		t.Fatal(err)
	}
	wait4.add(tid, stoppedAt(syscall.SIGTRAP, 0))
	if !broker.scan() {
		t.Fatal("replacement scan reported no progress")
	}
	result := nextBrokerResult(t, owner)
	if result.generation != secondGeneration || !result.status.Stopped() {
		t.Fatalf("replacement result = %+v, want generation %d stop",
			result, secondGeneration)
	}
}

func TestLinuxWaitBrokerReportsExactGenerationRetirement(t *testing.T) {
	const tid = 11431
	broker := newLinuxWaitBrokerWithRunner(newScriptedExactWait4().wait4, false)
	owner := broker.newOwner()
	generation, err := owner.register(tid)
	if err != nil {
		t.Fatal(err)
	}
	registration := broker.registrations[tid]
	broker.retire(tid, registration)

	result := nextBrokerResult(t, owner)
	if !result.retired || result.tid != tid || result.generation != generation {
		t.Fatalf("retirement = %+v, want tid %d generation %d", result, tid, generation)
	}
}

func TestLinuxProductionWait4CallsStayInsideTheBroker(t *testing.T) {
	files, err := linuxSources.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		name := file.Name()
		if strings.HasSuffix(name, "_test.go") || name == "waitbroker_linux_amd64.go" {
			continue
		}
		raw, err := linuxSources.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("syscall.Wait4(")) ||
			bytes.Contains(raw, []byte("unix.Wait4(")) {
			t.Fatalf("production Linux wait syscall escaped the broker: %s", name)
		}
	}
}

type recordingWaitSource struct {
	events  *[]string
	results []linuxWaitResult
}

func (s *recordingWaitSource) register(tid int) (uint64, error) {
	*s.events = append(*s.events, fmt.Sprintf("register %d", tid))
	return uint64(tid), nil
}

func (*recordingWaitSource) release(int, uint64) bool { return true }

func (s *recordingWaitSource) next(context.Context) (linuxWaitResult, error) {
	if len(s.results) == 0 {
		return linuxWaitResult{}, syscall.ECHILD
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func (s *recordingWaitSource) tryNext() (linuxWaitResult, bool, error) {
	if len(s.results) == 0 {
		return linuxWaitResult{}, false, nil
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, true, nil
}

func (*recordingWaitSource) close() {}

func TestLinuxWaitRegistersACloneBeforeResumingItsParent(t *testing.T) {
	const (
		parent = 11501
		child  = 11502
	)
	var events []string
	source := &recordingWaitSource{events: &events}
	b := &linuxBackend{
		pid:   parent,
		waits: source,
		eventMsgFn: func(int) (uint, error) {
			return child, nil
		},
		contFn: func(tid, _ int) error {
			events = append(events, fmt.Sprintf("continue %d", tid))
			return nil
		},
	}
	script := &scriptedWait{stops: []scriptedStop{
		{tid: parent, status: stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE)},
		{tid: parent, status: exitedWith(0)},
	}}
	b.waitFn = func(status *syscall.WaitStatus) (int, error) {
		if script.next >= len(script.stops) {
			return 0, syscall.ECHILD
		}
		stop := script.stops[script.next]
		script.next++
		*status = stop.status
		return stop.tid, nil
	}

	if _, err := b.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	want := []string{"register 11502", "continue 11501"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestLinuxSuspendedKillDrainsOwnedStopsAndRegistersLateClones(t *testing.T) {
	const (
		parent = 11601
		child  = 11602
	)
	var events []string
	source := &recordingWaitSource{
		events: &events,
		results: []linuxWaitResult{
			{tid: parent, status: stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE)},
			{tid: child, status: signaledBy(syscall.SIGKILL)},
			{tid: parent, status: signaledBy(syscall.SIGKILL)},
		},
	}
	b := &linuxBackend{
		pid:   parent,
		waits: source,
		eventMsgFn: func(int) (uint, error) {
			return child, nil
		},
		contFn: func(tid, _ int) error {
			events = append(events, fmt.Sprintf("continue %d", tid))
			return nil
		},
	}

	if err := b.reapAfterKill(); err != nil {
		t.Fatalf("reapAfterKill() error = %v", err)
	}
	want := []string{"register 11602", "continue 11601"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
