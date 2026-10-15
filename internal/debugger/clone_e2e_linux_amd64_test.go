//go:build e2e && linux && amd64

package debugger

import (
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestLinuxNativeCloneMetadataLostDuringFatalTeardown(t *testing.T) {
	bin, _ := buildNativeInspectionTarget(t)
	b, pid, _ := startObservedInspectionProcess(t, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readMessage := func(tid int) (msg uint, err error) {
		b.execPtrace(func() { msg, err = syscall.PtraceGetEventMsg(tid) })
		return
	}
	child, lost := 0, false
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
		// The first read is only the test's child-identity oracle. Production
		// receives the actual kernel ESRCH after SIGKILL invalidates this stop.
		for {
			msg, err = readMessage(tid)
			if errors.Is(err, syscall.ESRCH) {
				lost = true
				return msg, err
			}
			if err != nil || ctx.Err() != nil {
				t.Fatalf("clone stop did not become unavailable: %v, %v", err, ctx.Err())
			}
			time.Sleep(time.Millisecond)
		}
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
		t.Fatalf("race recovery was vacuous: kernel ESRCH=%t child=%d discovered=%t", lost, child, discovered)
	}
	if err := b.reapAfterKill(); err != nil {
		t.Fatal(err)
	}
}
