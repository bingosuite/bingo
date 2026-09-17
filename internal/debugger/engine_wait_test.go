package debugger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type heldWaitBackend struct {
	legacyAttachedBackend
	entered     chan struct{}
	canceled    chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	returned    atomic.Bool
	stop        StopEvent
}

func (b *heldWaitBackend) unblock() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func cleanupHeldWait(t *testing.T, e *engine, b *heldWaitBackend) {
	t.Helper()
	t.Cleanup(func() {
		b.unblock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, _, err := e.cancelAndJoinWait(ctx); err != nil {
			t.Errorf("cleanup waiter: %v", err)
		}
	})
}

func newHeldWaitBackend() *heldWaitBackend {
	return &heldWaitBackend{
		entered:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
		stop:     StopEvent{Reason: StopBreakpoint, TID: 17},
	}
}

func (b *heldWaitBackend) wait(ctx context.Context) (StopEvent, error) {
	defer b.returned.Store(true)
	close(b.entered)
	<-ctx.Done()
	close(b.canceled)
	<-b.release
	return b.stop, nil
}

func TestWaitJoinDrainsAFullStopChannelBeforeAcknowledgingReturn(t *testing.T) {
	b := newHeldWaitBackend()
	e := &engine{backend: b, stopCh: make(chan stopResult, 1), done: make(chan struct{})}
	cleanupHeldWait(t, e, b)
	e.stopCh <- stopResult{evt: StopEvent{Reason: StopExited}}
	e.startWait()
	wait := e.wait
	<-b.entered
	b.unblock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, joined, err := e.cancelAndJoinWait(ctx)
	if err != nil || !joined || result.evt.TID != 17 {
		t.Fatalf("join = (%+v, %v, %v), want the exact waiter's stop", result, joined, err)
	}
	select {
	case <-wait.done:
	default:
		t.Fatal("join returned before the waiter acknowledged completion")
	}
	if e.wait != nil || len(e.stopCh) != 0 {
		t.Fatal("join retained a waiter or a stale queued stop")
	}
}

func TestWaitJoinTimeoutRetainsTheExactWaiterForRetry(t *testing.T) {
	b := newHeldWaitBackend()
	e := &engine{backend: b, stopCh: make(chan stopResult, 1), done: make(chan struct{})}
	cleanupHeldWait(t, e, b)
	e.startWait()
	wait := e.wait
	<-b.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, joined, err := e.cancelAndJoinWait(ctx); joined || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("join = (%v, %v), want retained timeout", joined, err)
	}
	if e.wait != wait || b.returned.Load() {
		t.Fatal("a canceled but unacknowledged waiter was discarded")
	}
	b.unblock()
	retry, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if result, joined, err := e.cancelAndJoinWait(retry); err != nil || !joined || result.evt.TID != 17 {
		t.Fatalf("retry join = (%+v, %v, %v)", result, joined, err)
	}
}

type retainedWaitBackend struct {
	*heldWaitBackend
	latched      atomic.Bool
	acknowledged atomic.Bool
	detached     atomic.Bool
}

func (b *retainedWaitBackend) beginTeardown() error {
	b.latched.Store(true)
	return nil
}

func (b *retainedWaitBackend) teardownLatched() bool { return b.latched.Load() }

func (b *retainedWaitBackend) acknowledgeWait(context.Context) error {
	if !b.latched.Load() || !b.returned.Load() {
		return errors.New("acknowledgement preceded the latch or waiter return")
	}
	b.acknowledged.Store(true)
	return nil
}

func (b *retainedWaitBackend) retainsAttachedOwnership() bool { return !b.detached.Load() }
func (*retainedWaitBackend) attachedDetachStops() []StopEvent { return nil }
func (*retainedWaitBackend) attachedQuiesced() bool           { return false }
func (*retainedWaitBackend) attachedImageReplaced() bool      { return false }
func (*retainedWaitBackend) selectAttachedWriteTID() (int, error) {
	return 17, nil
}

func (b *retainedWaitBackend) quiesceAttached(context.Context) (bool, error) {
	if !b.acknowledged.Load() {
		return false, errors.New("quiesce preceded waiter acknowledgement")
	}
	return false, nil
}

func (b *retainedWaitBackend) detachAttached() error {
	if !b.acknowledged.Load() {
		return errors.New("detach preceded waiter acknowledgement")
	}
	b.detached.Store(true)
	return nil
}

func TestWaitJoinRetiresALaunchedNativeTerminalBeforeKillCanReuseItsPID(t *testing.T) {
	b := &retainedWaitBackend{heldWaitBackend: newHeldWaitBackend()}
	b.stop = StopEvent{Reason: StopExited, TID: 17}
	e := &engine{
		backend: b, proc: process{pid: 17, live: true},
		stopCh: make(chan stopResult, 1), done: make(chan struct{}),
	}
	cleanupHeldWait(t, e, b.heldWaitBackend)
	e.startWait()
	<-b.entered
	b.unblock()
	if err := e.joinBackendTeardown(); err != nil {
		t.Fatal(err)
	}
	if e.proc.live || e.proc.pid != 0 || !b.acknowledged.Load() {
		t.Fatalf("joined native terminal retained a recyclable PID: %+v", e.proc)
	}
}

func TestAttachedWaitTimeoutKeepsTheEngineAndSuppressesTheLateStop(t *testing.T) {
	previousTimeout := attachedDetachTimeout
	attachedDetachTimeout = 20 * time.Millisecond
	defer func() { attachedDetachTimeout = previousTimeout }()
	b := &retainedWaitBackend{heldWaitBackend: newHeldWaitBackend()}
	e := newEngine(b, nil)
	defer func() {
		b.unblock()
		cleanup := make(chan error, 1)
		go func() { cleanup <- e.Kill() }()
		select {
		case err := <-cleanup:
			if err != nil {
				t.Errorf("cleanup engine: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("cleanup engine did not finish")
		}
	}()
	if err := e.dispatch(func() error {
		e.proc = process{pid: 17, live: true, isAttached: true}
		e.setState(stateRunning)
		e.startWait()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-b.entered
	if err := e.Kill(); !errors.Is(err, ErrAttachedDetachIncomplete) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Kill = %v, want retained waiter timeout", err)
	}
	select {
	case <-e.done:
		t.Fatal("an unacknowledged waiter closed the engine")
	default:
	}
	if b.detached.Load() || b.acknowledged.Load() {
		t.Fatal("timed-out detach claimed completion")
	}
	if err := e.Continue(); !errors.Is(err, ErrAttachedDetachIncomplete) {
		t.Fatalf("Continue = %v, want cleanup-only admission", err)
	}

	b.unblock()
	deadline := time.Now().Add(time.Second)
	for {
		retired := false
		if err := e.dispatch(func() error {
			retired = e.wait == nil
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if retired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late waiter result was not retired")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case evt := <-e.events:
		t.Fatalf("late stop re-entered ordinary handling: %s", evt.Kind)
	default:
	}
	if err := e.Kill(); err != nil {
		t.Fatalf("retry Kill = %v", err)
	}
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("successful retry did not close the engine")
	}
	if !b.detached.Load() {
		t.Fatal("retry abandoned the attached target")
	}
}
