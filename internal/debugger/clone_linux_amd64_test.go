//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

type cloneScanningWaitSource struct {
	linuxWaitSource
	broker *linuxWaitBroker
}

func (s *cloneScanningWaitSource) next(ctx context.Context) (linuxWaitResult, error) {
	s.broker.scan()
	return s.linuxWaitSource.next(ctx)
}

func cloneRecoveryBackend(t *testing.T, script *scriptedExactWait4) (*linuxBackend, *linuxWaitBroker, *linuxWaitOwner) {
	t.Helper()
	broker := newLinuxWaitBrokerWithRunner(script.wait4, false)
	owner := broker.newOwner()
	if _, err := owner.register(100); err != nil {
		t.Fatal(err)
	}
	b := &linuxBackend{
		pid: 100, tracer: newTracerThread(),
		waits:      &cloneScanningWaitSource{linuxWaitSource: owner, broker: broker},
		eventMsgFn: func(int) (uint, error) { return 0, syscall.ESRCH },
		contFn:     func(int, int) error { return nil },
		threadsFn:  func() ([]int, error) { return []int{100, 101}, nil },
		tracedTIDsFn: func(context.Context) ([]int, error) {
			return []int{100, 101}, nil
		},
	}
	b.tracer.execPtrace(func() {})
	t.Cleanup(b.closeTracer)
	return b, broker, owner
}

func TestLinuxCloneReadESRCHReconcilesOwnershipAndKeepsRealSignalDeath(t *testing.T) {
	script := newScriptedExactWait4()
	script.add(100, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE))
	script.add(101, stoppedAt(syscall.SIGSTOP, 0))
	script.add(200, exitedWith(77))
	b, broker, owner := cloneRecoveryBackend(t, script)
	other := broker.newOwner()
	if _, err := other.register(200); err != nil {
		t.Fatal(err)
	}
	readClone := false
	sawLeaderExit := false
	b.eventMsgFn = func(tid int) (uint, error) {
		if readClone {
			sawLeaderExit = tid == 100
			return 0, nil
		}
		readClone = true
		script.add(100, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_EXIT))
		script.add(101, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_EXIT))
		broker.scan()
		return 0, syscall.ESRCH
	}
	b.contFn = func(tid, _ int) error {
		switch tid {
		case 100:
			if !sawLeaderExit {
				t.Fatal("resumed the invalidated clone stop")
			}
			script.add(100, signaledBy(syscall.SIGABRT))
		case 101:
			script.add(101, signaledBy(syscall.SIGKILL))
		}
		if _, known := owner.knownTIDs()[101]; !known {
			t.Fatal("resumed a stop before admitting the unknown clone")
		}
		return nil
	}
	stop, err := b.Wait()
	if err != nil || stop.Reason != StopKilled || stop.TID != 100 {
		t.Fatalf("lost real fatal terminal: %+v, %v", stop, err)
	}
	if b.cloneRecovery != nil {
		t.Fatal("real terminal retained the clone error")
	}
	if got := nextBrokerResult(t, other); got.tid != 200 || got.status.ExitStatus() != 77 {
		t.Fatalf("another session's terminal was consumed: %+v", got)
	}
}

func TestLinuxCloneRecoveryDoesNotReregisterQueuedTerminals(t *testing.T) {
	script := newScriptedExactWait4()
	script.add(100, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE), signaledBy(syscall.SIGABRT))
	script.add(101, signaledBy(syscall.SIGKILL))
	b, broker, owner := cloneRecoveryBackend(t, script)
	if _, err := owner.register(101); err != nil {
		t.Fatal(err)
	}
	b.eventMsgFn = func(int) (uint, error) {
		broker.scan()
		return 0, syscall.ESRCH
	}
	b.tracedTIDsFn = func(context.Context) ([]int, error) { return nil, nil }
	stop, err := b.Wait()
	if err != nil || stop.Reason != StopKilled {
		t.Fatalf("queued terminal was erased: %+v, %v", stop, err)
	}
	if got := broker.registrationCount(); got != 0 {
		t.Fatalf("retired identity registered again: %d registrations", got)
	}
}

func TestLinuxCloneDiscoveryNeverErasesRetiredEvidenceOrReusesItsGeneration(t *testing.T) {
	script := newScriptedExactWait4()
	script.add(100, signaledBy(syscall.SIGABRT))
	broker := newLinuxWaitBrokerWithRunner(script.wait4, false)
	owner := broker.newOwner()
	generation, err := owner.register(100)
	if err != nil {
		t.Fatal(err)
	}
	broker.scan()
	for _, previous := range []uint64{0, generation} {
		if _, err := owner.discover(100, previous); !errors.Is(err, errLinuxDiscoveryRetired) {
			t.Fatalf("discovery replaced a queued terminal: %v", err)
		}
	}
	if result := nextBrokerResult(t, owner); !result.status.Signaled() || result.status.Signal() != syscall.SIGABRT {
		t.Fatalf("discovery erased the fatal status: %+v", result)
	}
	if _, err := owner.discover(100, generation); !errors.Is(err, errLinuxDiscoveryRetired) {
		t.Fatalf("discovery reused a consumed identity: %v", err)
	}
	replacement := broker.newOwner()
	newGeneration, err := replacement.register(100)
	if err != nil {
		t.Fatal(err)
	}
	replacement.close()
	if _, err := owner.discover(100, 0); err == nil {
		t.Fatal("discovery stole a closed owner's retained registration")
	}
	if newGeneration == generation {
		t.Fatal("replacement reused its generation")
	}
}

func TestLinuxCloneRecoveryPreservesAnInterveningSignal(t *testing.T) {
	script := newScriptedExactWait4()
	script.add(100, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE))
	script.add(101, stoppedAt(syscall.SIGUSR1, 0))
	b, _, _ := cloneRecoveryBackend(t, script)
	stop, err := b.Wait()
	if err != nil || stop.TID != 101 || stop.Signal != int(syscall.SIGUSR1) {
		t.Fatalf("intervening signal lost: %+v, %v", stop, err)
	}
	forwarded := 0
	b.ptraceSyscall6Fn = func(trap, request, tid, addr, signal, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		if request != syscall.PTRACE_CONT || tid != 101 {
			t.Fatalf("signal resume request=%d tid=%d", request, tid)
		}
		forwarded = int(signal)
		script.add(100, signaledBy(syscall.SIGABRT))
		return 0, 0, 0
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	stop, err = b.Wait()
	if err != nil || stop.Reason != StopKilled || forwarded != int(syscall.SIGUSR1) {
		t.Fatalf("signal/terminal = %d, %+v, %v", forwarded, stop, err)
	}
}

func TestLinuxCloneReadESRCHRequiresARealTerminalWithinItsDeadline(t *testing.T) {
	for _, retirement := range []bool{false, true} {
		t.Run(fmt.Sprintf("ownership-retired=%t", retirement), func(t *testing.T) {
			script := newScriptedExactWait4()
			script.add(100, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE))
			b, broker, owner := cloneRecoveryBackend(t, script)
			b.tracedTIDsFn = func(context.Context) ([]int, error) {
				b.cloneRecovery.deadline = time.Now().Add(5 * time.Millisecond)
				if retirement {
					generation := owner.knownTIDs()[100]
					broker.retire(100, linuxWaitRegistration{owner: owner, generation: generation})
				}
				return nil, nil
			}
			start := time.Now()
			stop, err := b.Wait()
			if !errors.Is(err, ErrSessionInvalidated) || !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("ESRCH became a terminal: %+v, %v", stop, err)
			}
			if !retirement && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("missing deadline: %v", err)
			}
			if retirement && !errors.Is(err, syscall.ECHILD) {
				t.Fatalf("bare ownership retirement fabricated death: %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("clone proof wait exceeded its bound")
			}
		})
	}
}

func TestLinuxCloneRecoveryKeepsStrictFailureBoundaries(t *testing.T) {
	for _, kind := range []string{"not-esrch", "attached", "census-error", "ownership-conflict"} {
		t.Run(kind, func(t *testing.T) {
			script := newScriptedExactWait4()
			script.add(100, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE))
			b, broker, _ := cloneRecoveryBackend(t, script)
			want := error(syscall.ESRCH)
			switch kind {
			case "not-esrch":
				want = syscall.EPERM
				b.eventMsgFn = func(int) (uint, error) { return 0, want }
				b.tracedTIDsFn = func(context.Context) ([]int, error) { t.Fatal("unexpected census"); return nil, nil }
			case "attached":
				b.attachedTracees = map[int]*linuxTracee{100: {generation: 1}}
				b.tracedTIDsFn = func(context.Context) ([]int, error) {
					t.Fatal("foreign target used launched recovery")
					return nil, nil
				}
			case "census-error":
				b.tracedTIDsFn = func(context.Context) ([]int, error) { return nil, syscall.EACCES }
			case "ownership-conflict":
				if _, err := broker.newOwner().register(101); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := b.Wait(); !errors.Is(err, ErrSessionInvalidated) || !errors.Is(err, want) {
				t.Fatalf("strict failure was suppressed: %v", err)
			}
		})
	}
}

func TestLinuxKilledCloneMetadataLossStillDrainsTheExactOwner(t *testing.T) {
	script := newScriptedExactWait4()
	script.add(100, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE), signaledBy(syscall.SIGKILL))
	script.add(101, stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_EXIT), signaledBy(syscall.SIGKILL))
	b, broker, owner := cloneRecoveryBackend(t, script)
	b.cloneRecovery = &linuxCloneRecovery{
		cause:    &linuxCloneReadError{parentTID: 100, cause: syscall.ESRCH},
		deadline: time.Now().Add(-time.Second), known: owner.knownTIDs(),
	}
	if err := b.reapAfterKill(); err != nil {
		t.Fatalf("killed clone recovery: %v", err)
	}
	if broker.registrationCount() != 0 {
		t.Fatal("killed clone retained wait ownership")
	}
}

func TestLinuxTracerCensusUsesExactTracerAcrossThreadGroups(t *testing.T) {
	proc := t.TempDir()
	for _, task := range []struct{ pid, tid, tracer int }{
		{100, 100, 11}, {100, 101, 11}, {200, 200, 22}, {300, 300, 11}, {400, 400, 0},
	} {
		dir := filepath.Join(proc, fmt.Sprint(task.pid), "task", fmt.Sprint(task.tid))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(fmt.Sprintf("TracerPid:\t%d\n", task.tracer)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tids, err := linuxTracerTIDs(context.Background(), proc, 11)
	if err != nil || !reflect.DeepEqual(tids, []int{100, 101, 300}) {
		t.Fatalf("census stole another tracer's tasks or missed a child process: %v, %v", tids, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := linuxTracerTIDs(ctx, proc, 11); !errors.Is(err, context.Canceled) {
		t.Fatalf("census ignored cancellation: %v", err)
	}
}

func TestLinuxTracerCensusDoesNotGuessWhenAGroupCannotBeEnumerated(t *testing.T) {
	proc := t.TempDir()
	if err := os.Mkdir(filepath.Join(proc, "100"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxTracerTIDs(context.Background(), proc, 11); err == nil {
		t.Fatal("a missing task directory was treated as proof of group death")
	}
	if _, err := linuxTracerTIDs(context.Background(), proc, 0); err == nil {
		t.Fatal("a missing tracer admitted untraced tasks")
	}
	task := filepath.Join(proc, "100", "task", "100")
	if err := os.MkdirAll(task, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task, "status"), []byte("TracerPid: not-a-pid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxTracerTIDs(context.Background(), proc, 11); err == nil {
		t.Fatal("malformed ownership evidence was ignored")
	}
}
