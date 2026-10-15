//go:build e2e && linux && amd64

package debugger

import (
	"context"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestLinuxNativeCloneMetadataLossRecoversFatalTeardown(t *testing.T) {
	bin, _ := buildNativeInspectionTarget(t)
	b, pid, _ := startObservedInspectionProcess(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readMessage := func(tid int) (msg uint, err error) {
		b.execPtrace(func() { msg, err = syscall.PtraceGetEventMsg(tid) })
		return
	}
	child, lost, completed := 0, false, false
	t.Cleanup(func() {
		b.eventMsgFn = nil
		if !completed {
			if err := b.registerThreadGroup(); err != nil {
				t.Errorf("register exact test-owned group for cleanup: %v", err)
			}
		}
	})
	b.eventMsgFn = func(tid int) (uint, error) {
		if lost {
			return readMessage(tid)
		}
		msg, err := readMessage(tid)
		if err != nil {
			return msg, err
		}
		child = int(msg)
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		// Fault injection makes metadata loss deterministic; it does not claim
		// to reproduce the kernel timing. The child stays unknown to production.
		lost = true
		return 0, syscall.ESRCH
	}
	discovered := false
	b.tracedTIDsFn = func(ctx context.Context) ([]int, error) {
		tids, err := linuxTracerTIDs(ctx, "/proc", b.tracer.threadID())
		discovered = discovered || slices.Contains(tids, child)
		return tids, err
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	stop, err := b.wait(ctx)
	if err != nil || stop.Reason != StopKilled || stop.TID != pid {
		t.Fatalf("fatal clone race lost real terminal: %+v, %v", stop, err)
	}
	if !lost || child <= 0 || !discovered {
		t.Fatalf("race recovery was vacuous: injected ESRCH=%t child=%d discovered=%t", lost, child, discovered)
	}
	if err := b.reapAfterKill(); err != nil {
		t.Fatal(err)
	}
	completed = true
}
