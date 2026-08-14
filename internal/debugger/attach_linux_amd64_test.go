//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type attachWaitSource struct {
	mu         sync.Mutex
	nextGen    uint64
	registered map[int]uint64
	queue      []linuxWaitResult
	events     []string
}

func newAttachWaitSource(results ...linuxWaitResult) *attachWaitSource {
	return &attachWaitSource{
		registered: make(map[int]uint64),
		queue:      append([]linuxWaitResult(nil), results...),
	}
}

func (s *attachWaitSource) register(tid int) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation := s.registered[tid]; generation != 0 {
		return generation, nil
	}
	s.nextGen++
	s.registered[tid] = s.nextGen
	s.events = append(s.events, fmt.Sprintf("register:%d:%d", tid, s.nextGen))
	return s.nextGen, nil
}

func (s *attachWaitSource) release(tid int, generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registered[tid] != generation {
		return false
	}
	delete(s.registered, tid)
	s.events = append(s.events, fmt.Sprintf("release:%d:%d", tid, generation))
	return true
}

func (s *attachWaitSource) next(ctx context.Context) (linuxWaitResult, error) {
	for {
		result, ok, err := s.tryNext()
		if err != nil || ok {
			return result, err
		}
		select {
		case <-ctx.Done():
			return linuxWaitResult{}, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (s *attachWaitSource) tryNext() (linuxWaitResult, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) != 0 {
		result := s.queue[0]
		s.queue = s.queue[1:]
		if result.generation == 0 {
			result.generation = s.registered[result.tid]
		}
		return result, true, result.err
	}
	if len(s.registered) == 0 {
		return linuxWaitResult{}, false, syscall.ECHILD
	}
	return linuxWaitResult{}, false, nil
}

func (*attachWaitSource) close() {}

func TestLinuxAttachedQuiesceRegistersPreexistingThreadsAndLateClones(t *testing.T) {
	const (
		leader = 21001
		worker = 21002
		child  = 21003
	)
	source := newAttachWaitSource(
		linuxWaitResult{tid: leader, status: stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE)},
		linuxWaitResult{tid: worker, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
		linuxWaitResult{tid: child, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
	)
	var ptraceCalls []linuxResumeCall
	threadScans := 0
	b := &linuxBackend{
		pid:              leader,
		tracer:           newTracerThread(),
		waits:            source,
		attachedTracees:  make(map[int]*linuxTracee),
		ptraceSyscall6Fn: recordingPtraceSyscall(&ptraceCalls, 0),
		eventMsgFn: func(int) (uint, error) {
			return child, nil
		},
		threadsFn: func() ([]int, error) {
			threadScans++
			if threadScans == 1 {
				return []int{leader, worker}, nil
			}
			return []int{leader, worker, child}, nil
		},
		processExistsFn: func() bool { return true },
		tidExistsFn:     func(int) bool { return true },
	}
	t.Cleanup(b.closeTracer)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	gone, err := b.quiesceAttached(ctx)
	if err != nil {
		t.Fatalf("quiesceAttached() error = %v", err)
	}
	if gone {
		t.Fatal("live attached group was reported gone")
	}
	if got := b.attachedTIDs(); !reflect.DeepEqual(got, []int{leader, worker, child}) {
		t.Fatalf("attached tids = %v", got)
	}
	if !b.allAttachedStopped() {
		t.Fatal("quiesce completed before every pre-existing and cloned TID stopped")
	}

	var seized, interrupted []int
	for _, call := range ptraceCalls {
		switch int(call.request) {
		case unix.PTRACE_SEIZE:
			seized = append(seized, int(call.tid))
		case unix.PTRACE_INTERRUPT:
			interrupted = append(interrupted, int(call.tid))
		}
	}
	sort.Ints(seized)
	sort.Ints(interrupted)
	if !reflect.DeepEqual(seized, []int{leader, worker}) {
		t.Fatalf("seized tids = %v", seized)
	}
	if !reflect.DeepEqual(interrupted, []int{leader, worker}) {
		t.Fatalf("interrupted tids = %v", interrupted)
	}
	if source.registered[child] == 0 {
		t.Fatal("late clone was not registered with the existing wait owner")
	}
}

func TestLinuxAttachedQuiesceAdoptsACloneVisibleBeforeItsCloneEvent(t *testing.T) {
	const (
		leader = 21101
		child  = 21102
	)
	source := newAttachWaitSource(
		linuxWaitResult{tid: leader, status: stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE)},
		linuxWaitResult{tid: child, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
	)
	b := &linuxBackend{
		pid:             leader,
		tracer:          newTracerThread(),
		waits:           source,
		attachedTracees: make(map[int]*linuxTracee),
		eventMsgFn:      func(int) (uint, error) { return child, nil },
		threadsFn:       func() ([]int, error) { return []int{leader, child}, nil },
		processExistsFn: func() bool { return true },
		tidExistsFn:     func(int) bool { return true },
	}
	b.tracerPIDFn = func(int) (int, error) { return b.tracer.threadID(), nil }
	b.ptraceSyscall6Fn = func(_, request, tid, _, _, _, _ uintptr) (uintptr, uintptr, syscall.Errno) {
		if int(request) == unix.PTRACE_SEIZE && int(tid) == child {
			return 0, 0, syscall.EPERM
		}
		return 0, 0, 0
	}
	t.Cleanup(b.closeTracer)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := b.quiesceAttached(ctx); err != nil {
		t.Fatalf("quiesceAttached() error = %v", err)
	}
	if state := b.attachedTracees[child]; state == nil || !state.stopped {
		t.Fatalf("auto-attached child state = %+v, want registered and stopped", state)
	}
}

func TestLinuxAttachedPartialDetachReseizesAndRemainsRetryable(t *testing.T) {
	const (
		leader = 22001
		worker = 22002
	)
	source := newAttachWaitSource(
		linuxWaitResult{tid: worker, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
	)
	leaderGeneration, _ := source.register(leader)
	workerGeneration, _ := source.register(worker)
	detachFailures := 1
	var ptraceCalls []linuxResumeCall
	b := &linuxBackend{
		pid:    leader,
		tracer: newTracerThread(),
		waits:  source,
		attachedTracees: map[int]*linuxTracee{
			leader: {generation: leaderGeneration, stopped: true, resumeAllowed: true},
			worker: {generation: workerGeneration, stopped: true, resumeAllowed: true},
		},
		threadsFn: func() ([]int, error) {
			return []int{leader, worker}, nil
		},
		processExistsFn: func() bool { return true },
		tidExistsFn:     func(int) bool { return true },
	}
	b.ptraceSyscall6Fn = func(trap, request, tid, addr, signal, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		ptraceCalls = append(ptraceCalls, linuxResumeCall{
			trap: trap, request: request, tid: tid, addr: addr, signal: signal, a5: a5, a6: a6,
		})
		if int(request) == syscall.PTRACE_DETACH && int(tid) == leader && detachFailures > 0 {
			detachFailures--
			return 0, 0, syscall.EIO
		}
		return 0, 0, 0
	}
	t.Cleanup(b.closeTracer)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := b.detachAttachedWithContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "PTRACE_DETACH tid 22001") {
		t.Fatalf("first detach error = %v", err)
	}
	if !b.attached() || !b.attachCleanup || !b.allAttachedStopped() {
		t.Fatalf("failed detach was not retained quiesced: attached=%v cleanup=%v stopped=%v",
			b.attached(), b.attachCleanup, b.allAttachedStopped())
	}

	reseizedWorker := false
	for _, call := range ptraceCalls {
		if int(call.request) == unix.PTRACE_SEIZE && int(call.tid) == worker {
			reseizedWorker = true
		}
	}
	if !reseizedWorker {
		t.Fatal("successfully detached worker was not re-seized after the leader failed")
	}

	if err := b.detachAttachedWithContext(ctx); err != nil {
		t.Fatalf("retry detach error = %v", err)
	}
	if b.attached() || b.attachCleanup {
		t.Fatal("successful retry retained attached ownership")
	}
}

func TestLinuxAttachedDetachPreservesSignalBatchByStopProvenance(t *testing.T) {
	const (
		pid = 23001
		tid = 23002
	)
	tests := []struct {
		name           string
		stop           StopEvent
		signalDelivery bool
		groupStop      int
		seed           func(*pendingSignals)
		wantRequeues   []int
		wantDetach     int
	}{
		{
			name:           "genuine fatal signal remains the detach argument",
			stop:           StopEvent{Reason: StopSignal, TID: tid, Signal: int(syscall.SIGSEGV)},
			signalDelivery: true,
			seed: func(p *pendingSignals) {
				p.set(tid, int(syscall.SIGUSR1))
				p.set(tid, int(syscall.SIGSEGV))
				p.delay(tid, int(syscall.SIGURG))
			},
			wantRequeues: []int{int(syscall.SIGUSR1), int(syscall.SIGURG)},
			wantDetach:   int(syscall.SIGSEGV),
		},
		{
			name: "interrupt stop requeues the current signal",
			stop: StopEvent{TID: tid},
			seed: func(p *pendingSignals) {
				p.set(tid, int(syscall.SIGTERM))
			},
			wantRequeues: []int{int(syscall.SIGTERM)},
		},
		{
			name:           "pause SIGSTOP remains detach-only suppressed",
			stop:           StopEvent{Reason: StopSignal, TID: tid, Signal: int(syscall.SIGSTOP)},
			signalDelivery: true,
			seed:           func(*pendingSignals) {},
		},
		{
			name:         "pre-existing group stop remains stopped after detach",
			stop:         StopEvent{Reason: StopSignal, TID: tid, Signal: int(syscall.SIGSTOP)},
			groupStop:    int(syscall.SIGSTOP),
			seed:         func(*pendingSignals) {},
			wantRequeues: []int{int(syscall.SIGSTOP)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resumeCalls []linuxResumeCall
			var tgkillCalls []linuxTgkillCall
			b := &linuxBackend{
				pid:              pid,
				tracer:           newTracerThread(),
				ptraceSyscall6Fn: recordingPtraceSyscall(&resumeCalls, 0),
				tgkillFn:         recordingTgkill(&tgkillCalls),
			}
			t.Cleanup(b.closeTracer)
			tt.seed(&b.pendingSignals)
			state := &linuxTracee{
				stopped:         true,
				stop:            tt.stop,
				signalDelivery:  tt.signalDelivery,
				groupStopSignal: tt.groupStop,
			}

			if err := b.detachAttachedTID(tid, state); err != nil {
				t.Fatalf("detachAttachedTID() error = %v", err)
			}
			gotSignals := make([]int, 0, len(tgkillCalls))
			for _, call := range tgkillCalls {
				gotSignals = append(gotSignals, call.signal)
			}
			if !slices.Equal(gotSignals, tt.wantRequeues) {
				t.Fatalf("requeued signals = %v, want %v", gotSignals, tt.wantRequeues)
			}
			if len(resumeCalls) != 1 {
				t.Fatalf("ptrace calls = %+v", resumeCalls)
			}
			if got := int(resumeCalls[0].signal); got != tt.wantDetach {
				t.Fatalf("detach signal = %d, want %d", got, tt.wantDetach)
			}
		})
	}
}

func TestLinuxAttachedDetachKeepsDeadStepOwnerAndParkedStopsOwned(t *testing.T) {
	const (
		owner   = 24001
		sibling = 24002
	)
	tests := []struct {
		name   string
		setup  func(*linuxBackend)
		want   []int
		signal int
	}{
		{
			name: "empty queue uses the held owner",
			setup: func(b *linuxBackend) {
				b.beginStep(owner)
				b.interruptStepIfStepped(owner)
				b.holdStepOwner(owner)
			},
			want: []int{owner},
		},
		{
			name: "populated queue retains the sibling provenance",
			setup: func(b *linuxBackend) {
				b.beginStep(owner)
				b.interruptStepIfStepped(owner)
				b.park(StopEvent{Reason: StopSignal, TID: sibling, Signal: int(syscall.SIGUSR1)})
			},
			want:   []int{sibling},
			signal: int(syscall.SIGUSR1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &linuxBackend{
				attachedTracees: map[int]*linuxTracee{
					owner:   {},
					sibling: {},
				},
			}
			tt.setup(b)
			b.collectStepQueueForDetach()
			var stopped []int
			for _, tid := range b.attachedTIDs() {
				if b.attachedTracees[tid].stopped {
					stopped = append(stopped, tid)
				}
			}
			if !reflect.DeepEqual(stopped, tt.want) {
				t.Fatalf("stopped provenance = %v, want %v", stopped, tt.want)
			}
			if b.stepExitPending || b.stepping || len(b.parked) != 0 {
				t.Fatal("detach collection left the step gate active")
			}
			if tt.signal != 0 {
				if got := b.pendingSignals.take(sibling); got != tt.signal {
					t.Fatalf("parked signal = %d, want %d", got, tt.signal)
				}
			}
		})
	}
}

func TestLinuxAttachedDetachPropagatesExactPtraceError(t *testing.T) {
	const tid = 25001
	b := &linuxBackend{
		pid:              tid,
		tracer:           newTracerThread(),
		ptraceSyscall6Fn: recordingPtraceSyscall(&[]linuxResumeCall{}, syscall.EPERM),
	}
	t.Cleanup(b.closeTracer)
	err := b.detachAttachedTID(tid, &linuxTracee{stopped: true})
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("detach error = %v, want EPERM", err)
	}
	if !strings.Contains(err.Error(), "PTRACE_DETACH tid 25001 signal 0") {
		t.Fatalf("detach error lost exact operation: %v", err)
	}
}

func TestLinuxAttachedDetachNeverTreatsAClosedTracerAsSuccess(t *testing.T) {
	const tid = 25011
	b := &linuxBackend{pid: tid, tracer: newTracerThread()}
	b.closeTracer()
	err := b.detachAttachedTID(tid, &linuxTracee{stopped: true})
	if err == nil || !strings.Contains(err.Error(), "tracer thread is closed") {
		t.Fatalf("detach with closed tracer error = %v", err)
	}
}

func TestLinuxAttachedEventStopNeverBecomesInjectableSIGTRAP(t *testing.T) {
	const tid = 25021
	b := &linuxBackend{
		pid:    tid,
		tracer: newTracerThread(),
		attachedTracees: map[int]*linuxTracee{
			tid: {generation: 1},
		},
	}
	t.Cleanup(b.closeTracer)
	script := &scriptedWait{stops: []scriptedStop{
		{tid: tid, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
		{tid: tid, status: stoppedAt(syscall.SIGUSR1, 0)},
	}}
	script.install(b)

	event, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if event.Reason != StopSignal || event.Signal != int(syscall.SIGUSR1) {
		t.Fatalf("Wait() event = %+v", event)
	}
	if len(script.ops) != 1 || script.ops[0].step || script.ops[0].signal != 0 {
		t.Fatalf("event-stop resumes = %+v, want one signal-zero continue", script.ops)
	}
	if batch := b.pendingSignals.takeForContinue(tid); batch.current != int(syscall.SIGUSR1) {
		t.Fatalf("pending batch = %+v, want only SIGUSR1", batch)
	}
}

func TestLinuxAttachedQuiescePreservesARealGroupStop(t *testing.T) {
	const (
		pid = 25031
		tid = 25032
	)
	var ptraceCalls []linuxResumeCall
	var tgkillCalls []linuxTgkillCall
	b := &linuxBackend{
		pid:    pid,
		tracer: newTracerThread(),
		attachedTracees: map[int]*linuxTracee{
			tid: {generation: 1},
		},
		ptraceSyscall6Fn: recordingPtraceSyscall(&ptraceCalls, 0),
		tgkillFn:         recordingTgkill(&tgkillCalls),
	}
	t.Cleanup(b.closeTracer)

	result := linuxWaitResult{
		tid:    tid,
		status: stoppedAt(syscall.SIGSTOP, unix.PTRACE_EVENT_STOP),
	}
	if err := b.recordAttachedQuiesceResult(result); err != nil {
		t.Fatalf("recordAttachedQuiesceResult() error = %v", err)
	}
	state := b.attachedTracees[tid]
	if state.groupStopSignal != int(syscall.SIGSTOP) || state.signalDelivery {
		t.Fatalf("group-stop state = %+v", state)
	}
	if pending := b.pendingSignals.takeForContinue(tid); pending.current != 0 || len(pending.deferred) != 0 {
		t.Fatalf("group stop entered delivery-signal state: %+v", pending)
	}

	if err := b.detachAttachedTID(tid, state); err != nil {
		t.Fatalf("detachAttachedTID() error = %v", err)
	}
	if len(tgkillCalls) != 1 || tgkillCalls[0].signal != int(syscall.SIGSTOP) {
		t.Fatalf("group-stop requeues = %+v", tgkillCalls)
	}
	if len(ptraceCalls) != 1 || int(ptraceCalls[0].signal) != 0 {
		t.Fatalf("group-stop detach calls = %+v, want signal-zero detach", ptraceCalls)
	}
	if state.groupStopSignal != 0 {
		t.Fatal("successful group-stop requeue remained pending for a retry")
	}
}

func TestLinuxAttachedWaitPreservesARealGroupStop(t *testing.T) {
	const tid = 25041
	b := &linuxBackend{
		pid:    tid,
		tracer: newTracerThread(),
		attachedTracees: map[int]*linuxTracee{
			tid: {generation: 1},
		},
	}
	t.Cleanup(b.closeTracer)
	script := &scriptedWait{stops: []scriptedStop{
		{tid: tid, status: stoppedAt(syscall.SIGTSTP, unix.PTRACE_EVENT_STOP)},
	}}
	script.install(b)

	event, err := b.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if event.Reason != StopSignal || event.Signal != int(syscall.SIGTSTP) {
		t.Fatalf("Wait() event = %+v", event)
	}
	state := b.attachedTracees[tid]
	if state.groupStopSignal != int(syscall.SIGTSTP) || state.signalDelivery {
		t.Fatalf("running group-stop state = %+v", state)
	}
	if pending := b.pendingSignals.takeForContinue(tid); pending.current != 0 || len(pending.deferred) != 0 {
		t.Fatalf("running group stop entered delivery-signal state: %+v", pending)
	}
}
