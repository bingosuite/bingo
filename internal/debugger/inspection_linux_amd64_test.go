//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func inspectionBackend(t *testing.T, tids []int, results ...linuxWaitResult) (*linuxBackend, *[]linuxResumeCall) {
	t.Helper()
	source := newAttachWaitSource(results...)
	for _, tid := range tids {
		if _, err := source.register(tid); err != nil {
			t.Fatal(err)
		}
	}
	calls := &[]linuxResumeCall{}
	b := &linuxBackend{
		pid: tids[0], seized: true, tracer: newTracerThread(), waits: source,
		threadsFn:        func() ([]int, error) { return append([]int(nil), tids...), nil },
		ptraceSyscall6Fn: recordingPtraceSyscall(calls, 0),
		tgkillFn:         recordingTgkill(&[]linuxTgkillCall{}),
	}
	b.tracerPIDFn = func(int) (int, error) { return b.tracer.threadID(), nil }
	b.recordStop(tids[0])
	t.Cleanup(b.closeTracer)
	return b, calls
}

func TestLinuxInspectionHoldPersistsAndReplaysRealSignals(t *testing.T) {
	b, calls := inspectionBackend(t, []int{100, 101, 102},
		linuxWaitResult{tid: 101, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
		linuxWaitResult{tid: 102, status: stoppedAt(syscall.SIGUSR1, 0)},
	)
	for i := 0; i < 2; i++ {
		tids, err := b.inspectionThreads()
		if err != nil || !reflect.DeepEqual(tids, []int{100, 101, 102}) {
			t.Fatalf("hold %d = %v: %v", i, tids, err)
		}
		if len(*calls) != 2 || b.traceTID() != 100 {
			t.Fatalf("hold resumed threads or moved origin: %+v", *calls)
		}
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	event, err := b.Wait()
	if err != nil || event.TID != 102 || event.Signal != int(syscall.SIGUSR1) {
		t.Fatalf("replayed signal = %+v: %v", event, err)
	}
	if b.inspection != nil {
		t.Fatal("successful resume retained synthetic hold")
	}
	if got := (*calls)[3]; got.request != syscall.PTRACE_CONT || got.tid != 101 || got.signal != 0 {
		t.Fatalf("synthetic release = %+v", got)
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	if got := (*calls)[4]; got.tid != 102 || got.signal != uintptr(syscall.SIGUSR1) {
		t.Fatalf("real signal forwarded to wrong thread: %+v", got)
	}
}

func TestLinuxInspectionFailedResumeRetainsHold(t *testing.T) {
	b, calls := inspectionBackend(t, []int{100, 101},
		linuxWaitResult{tid: 101, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
	)
	if _, err := b.inspectionThreads(); err != nil {
		t.Fatal(err)
	}
	b.ptraceSyscall6Fn = recordingPtraceSyscall(calls, syscall.EPERM)
	if err := b.ContinueProcess(); err == nil {
		t.Fatal("failed resume succeeded")
	}
	if b.inspection.releasing || !b.inspection.synthetic[101] {
		t.Fatal("failed primary resume released siblings")
	}
	b.ptraceSyscall6Fn = recordingPtraceSyscall(calls, 0)
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	b.ptraceSyscall6Fn = recordingPtraceSyscall(calls, syscall.EPERM)
	if err := b.releaseInspectionThreads(); !errors.Is(err, ErrSessionInvalidated) {
		t.Fatalf("release error = %v", err)
	}
	if !b.inspection.synthetic[101] {
		t.Fatal("failed release forgot owned hold")
	}
}

func TestLinuxInspectionFailedStepRetainsHoldWithoutInventingStep(t *testing.T) {
	b, calls := inspectionBackend(t, []int{100, 101},
		linuxWaitResult{tid: 101, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
	)
	if _, err := b.inspectionThreads(); err != nil {
		t.Fatal(err)
	}
	b.ptraceSyscall6Fn = recordingPtraceSyscall(calls, syscall.EPERM)
	if err := b.SingleStep(100); err == nil {
		t.Fatal("failed step succeeded")
	}
	if b.stepping || b.stepTID != 0 || b.inspection.releasing {
		t.Fatal("failed step invented hardware ownership or released siblings")
	}
	if _, err := b.inspectionThreads(); err != nil {
		t.Fatalf("inspection after rejected step: %v", err)
	}
}

func TestLinuxInspectionReplayPreservesWaitErrors(t *testing.T) {
	b, _ := inspectionBackend(t, []int{100})
	b.inspectionReplay = []linuxWaitResult{{tid: 100, err: syscall.EIO}}
	result, err := b.waitAny(context.Background())
	if result.tid != 100 || !errors.Is(err, syscall.EIO) {
		t.Fatalf("error replay became exit status zero: %+v, %v", result, err)
	}
}

func TestLinuxFailedLaunchCleanupDoesNotReregisterRetiredPID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	b, _ := inspectionBackend(t, []int{cmd.Process.Pid})
	source := b.waits.(*attachWaitSource)
	source.registered = make(map[int]uint64)
	b.failedLaunch, b.failedLaunchRegistered = cmd, true
	if err := b.cleanupFailedLaunch(); err != nil {
		t.Fatal(err)
	}
	if len(source.registered) != 0 || b.failedLaunch != nil || b.pid != 0 {
		t.Fatal("cleanup registered a replacement PID generation")
	}
}

func TestLinuxInspectionPartialHoldRemainsRetryable(t *testing.T) {
	b, _ := inspectionBackend(t, []int{100, 101})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := b.holdInspectionThreads(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("partial hold = %v", err)
	}
	if !b.inspection.interrupt[101] || b.inspection.stopped[101] {
		t.Fatal("interrupt request was confused with stop acknowledgement")
	}
	source := b.waits.(*attachWaitSource)
	source.queue = append(source.queue, linuxWaitResult{tid: 101, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)})
	if _, err := b.inspectionThreads(); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxInspectionCloneCaptureSurvivesDetachFolding(t *testing.T) {
	b, _ := inspectionBackend(t, []int{100, 101})
	b.attachedTracees = make(map[int]*linuxTracee)
	for tid, generation := range b.waits.(*attachWaitSource).registered {
		b.registerAttachedClone(tid, generation)
	}
	b.inspection = &linuxInspectionHold{
		stopped: map[int]bool{100: true}, synthetic: make(map[int]bool),
		interrupt: make(map[int]bool), priorResume: make(map[int]bool),
	}
	reads := 0
	b.eventMsgFn = func(int) (uint, error) { reads++; return 101, nil }
	if err := b.recordInspectionResult(linuxWaitResult{tid: 100, status: stoppedAt(syscall.SIGTRAP, syscall.PTRACE_EVENT_CLONE)}); err != nil {
		t.Fatal(err)
	}
	b.eventMsgFn = func(int) (uint, error) { t.Fatal("clone message reread after capture"); return 0, nil }
	if err := b.foldInspectionForDetach(); err != nil || reads != 1 {
		t.Fatalf("clone fold = %v, reads=%d", err, reads)
	}
}

func TestLinuxInspectionRetirementCannotFabricateExitZero(t *testing.T) {
	b, _ := inspectionBackend(t, []int{100})
	b.inspection = &linuxInspectionHold{stopped: map[int]bool{100: true}}
	err := b.recordInspectionResult(linuxWaitResult{tid: 100, retired: true, err: syscall.ECHILD})
	var ended *inspectionEndedError
	if !errors.Is(err, ErrSessionInvalidated) || errors.As(err, &ended) {
		t.Fatalf("retirement fabricated terminal: %v", err)
	}
}

func TestLinuxInspectionLaunchRequiresGroupStopAndUnchangedContext(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable", true: "changed"}[changed], func(t *testing.T) {
			b, calls := inspectionBackend(t, []int{100},
				linuxWaitResult{tid: 100, status: stoppedAt(syscall.SIGTRAP, unix.PTRACE_EVENT_STOP)},
				linuxWaitResult{tid: 100, status: stoppedAt(syscall.SIGSTOP, 0)},
				linuxWaitResult{tid: 100, status: stoppedAt(syscall.SIGSTOP, unix.PTRACE_EVENT_STOP)},
			)
			b.seized = false
			b.startupSignalInfoFn = func(int) (int32, int32, error) { return 0, 100, nil }
			reads := 0
			b.startupRegistersFn = func(int) (Registers, error) {
				reads++
				pc := uint64(0x1000)
				if changed && reads == 2 {
					pc++
				}
				return Registers{PC: pc, SP: 0x8000}, nil
			}
			var signals []linuxTgkillCall
			b.tgkillFn = recordingTgkill(&signals)
			err := b.seizeLaunchedProcess(100)
			if changed && (err == nil || b.seized) {
				t.Fatal("changed entry context admitted")
			}
			if !changed && (err != nil || !b.seized) {
				t.Fatalf("stable launch rejected: %v", err)
			}
			want := []linuxResumeCall{
				wantLinuxResume(syscall.PTRACE_DETACH, 100, int(syscall.SIGSTOP)),
				wantLinuxResume(unix.PTRACE_SEIZE, 100, linuxPtraceOptions),
				wantLinuxResume(unix.PTRACE_INTERRUPT, 100, 0),
				wantLinuxResume(syscall.PTRACE_CONT, 100, 0),
				wantLinuxResume(syscall.PTRACE_CONT, 100, int(syscall.SIGSTOP)),
			}
			if !reflect.DeepEqual(*calls, want) || !reflect.DeepEqual(signals, []linuxTgkillCall{{100, 100, int(syscall.SIGCONT)}}) {
				t.Fatalf("startup ownership sequence: %+v, signals=%+v", *calls, signals)
			}
		})
	}
}

func TestLinuxInspectionLaunchRejectsUnprovenTrapBeforeDetach(t *testing.T) {
	b, calls := inspectionBackend(t, []int{100})
	b.seized = false
	b.startupSignalInfoFn = func(int) (int32, int32, error) { return 1, 100, nil }
	if err := b.seizeLaunchedProcess(100); err == nil || len(*calls) != 0 {
		t.Fatalf("unproven signal stop detached: %v, %+v", err, *calls)
	}
}
