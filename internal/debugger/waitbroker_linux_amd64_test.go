//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

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
	if err := first.register(firstTID); err != nil {
		t.Fatal(err)
	}
	if err := second.register(secondTID); err != nil {
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
	if err := owner.register(tid); err != nil {
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
	if err := owner.register(tid); err != nil {
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
	if err := first.register(tid); err != nil {
		t.Fatal(err)
	}
	if err := second.register(tid); err == nil {
		t.Fatal("second owner registered a live TID already owned by the first")
	}

	wait4.add(tid, exitedWith(0))
	broker.scan()
	_ = nextBrokerResult(t, first)
	if err := second.register(tid); err != nil {
		t.Fatalf("register reused tid after retirement: %v", err)
	}
}

func TestLinuxWaitBrokerKeepsReapingAfterItsConsumerCloses(t *testing.T) {
	const tid = 11401
	wait4 := newScriptedExactWait4()
	broker := newLinuxWaitBrokerWithRunner(wait4.wait4, false)
	owner := broker.newOwner()
	if err := owner.register(tid); err != nil {
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
	if err := retiring.register(retiringTID); err != nil {
		t.Fatal(err)
	}
	retiring.close()
	if err := next.register(newTID); err != nil {
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

type recordingWaitSource struct {
	events  *[]string
	results []linuxWaitResult
}

func (s *recordingWaitSource) register(tid int) error {
	*s.events = append(*s.events, fmt.Sprintf("register %d", tid))
	return nil
}

func (s *recordingWaitSource) next(context.Context) (linuxWaitResult, error) {
	if len(s.results) == 0 {
		return linuxWaitResult{}, syscall.ECHILD
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
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
