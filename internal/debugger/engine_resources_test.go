package debugger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type resourceTestBackend struct {
	legacyAttachedBackend
	latched   atomic.Bool
	acked     atomic.Bool
	failAck   atomic.Bool
	failClose atomic.Bool
	released  atomic.Bool
	releases  atomic.Int32
	stepEnds  atomic.Int32
	waited    atomic.Bool
	waitDone  atomic.Bool
	stop      chan struct{}
	waitErr   error
}

func (b *resourceTestBackend) beginTeardown() error {
	b.latched.Store(true)
	return nil
}

func (b *resourceTestBackend) teardownLatched() bool { return b.latched.Load() }

func (b *resourceTestBackend) acknowledgeWait(context.Context) error {
	if b.failAck.Load() {
		return errors.New("diagnostic acknowledgement still pending")
	}
	if !b.latched.Load() || (b.waited.Load() && !b.waitDone.Load()) {
		return errors.New("acknowledgement preceded the exact waiter")
	}
	b.acked.Store(true)
	return nil
}

func (b *resourceTestBackend) releaseBackendResources() error {
	if !b.acked.Load() || !b.latched.Load() {
		return errors.New("namespace release preceded COMPLETE")
	}
	if b.released.Load() {
		return nil
	}
	b.releases.Add(1)
	if b.failClose.Load() {
		return errors.New("owned right still retained")
	}
	b.released.Store(true)
	return nil
}

func (b *resourceTestBackend) singleStepThread(int, uint64) error { return nil }
func (b *resourceTestBackend) endThreadStep()                     { b.stepEnds.Add(1) }

func (b *resourceTestBackend) wait(ctx context.Context) (StopEvent, error) {
	b.waited.Store(true)
	defer b.waitDone.Store(true)
	select {
	case <-ctx.Done():
		return StopEvent{}, ctx.Err()
	case <-b.stop:
		return StopEvent{Reason: StopExited, ExitCode: 23}, b.waitErr
	}
}

func newResourceTestEngine(t *testing.T, b *resourceTestBackend) *engine {
	t.Helper()
	e := newEngine(b, nil)
	t.Cleanup(func() {
		b.failAck.Store(false)
		b.failClose.Store(false)
		done := make(chan error, 1)
		go func() { done <- e.Kill() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("cleanup engine: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("cleanup retained a resource-test engine")
		}
	})
	return e
}

func awaitResourceEngineClosed(t *testing.T, e *engine) {
	t.Helper()
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("engine did not finish after successful resource release")
	}
}

func TestBackendResourcesRetainTheEngineAndAllowOnlyCleanup(t *testing.T) {
	b := &resourceTestBackend{}
	b.failClose.Store(true)
	e := newResourceTestEngine(t, b)
	if err := e.Kill(); !errors.Is(err, ErrBackendCleanupIncomplete) {
		t.Fatalf("Kill = %v, want retained namespace failure", err)
	}
	select {
	case <-e.done:
		t.Fatal("namespace failure closed the owning engine")
	default:
	}
	for name, command := range map[string]func() error{
		"launch":   func() error { return e.Launch("must-not-be-opened", nil, nil) },
		"attach":   func() error { return e.Attach(17, "") },
		"continue": e.Continue,
		"pause":    e.Pause,
		"step":     e.StepInto,
		"set":      func() error { _, err := e.SetBreakpoint("unused", 1); return err },
		"clear":    func() error { return e.ClearBreakpoint(1) },
		"frames":   func() error { _, err := e.StackFrames(); return err },
		"locals":   func() error { _, err := e.Locals(0); return err },
	} {
		if err := command(); !errors.Is(err, ErrBackendCleanupIncomplete) {
			t.Errorf("%s bypassed cleanup-only admission: %v", name, err)
		}
	}
	stepEnds := b.stepEnds.Load()
	b.failClose.Store(false)
	if err := e.Kill(); err != nil {
		t.Fatalf("retry namespace release: %v", err)
	}
	awaitResourceEngineClosed(t, e)
	if b.stepEnds.Load() != stepEnds {
		t.Fatal("namespace-only retry touched the old victim's step registers")
	}
	if !b.released.Load() || b.releases.Load() != 2 {
		t.Fatalf("release attempts=%d, released=%v", b.releases.Load(), b.released.Load())
	}
	if err := e.Kill(); err != nil {
		t.Fatalf("idempotent Kill: %v", err)
	}
}

func TestBackendResourcesWaitForAcknowledgementBeforeAnyRelease(t *testing.T) {
	b := &resourceTestBackend{}
	b.failAck.Store(true)
	e := newResourceTestEngine(t, b)
	if err := e.Kill(); !errors.Is(err, ErrBackendCleanupIncomplete) {
		t.Fatalf("Kill = %v, want retained acknowledgement failure", err)
	}
	if b.releases.Load() != 0 || b.stepEnds.Load() != 0 {
		t.Fatal("failed acknowledgement allowed namespace or victim operations")
	}
	b.failAck.Store(false)
	if err := e.Kill(); err != nil {
		t.Fatal(err)
	}
	awaitResourceEngineClosed(t, e)
	if !b.released.Load() {
		t.Fatal("retry skipped resource reclamation")
	}
}

func TestBackendResourcesRetireNaturalExitAndOrdinaryWaitFailure(t *testing.T) {
	for _, failWait := range []bool{false, true} {
		for _, failRelease := range []bool{false, true} {
			t.Run(fmt.Sprintf("wait-error=%v release-error=%v", failWait, failRelease), func(t *testing.T) {
				b := &resourceTestBackend{stop: make(chan struct{})}
				if failWait {
					b.waitErr = errors.New("native receive failure")
				}
				b.failClose.Store(failRelease)
				e := newResourceTestEngine(t, b)
				if err := e.dispatch(func() error {
					e.setState(stateRunning)
					e.startWait()
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				close(b.stop)
				var events []protocol.Event
				deadline := time.After(time.Second)
			read:
				for {
					select {
					case evt, ok := <-e.events:
						if !ok {
							break read
						}
						events = append(events, evt)
						if failRelease && evt.Kind == protocol.EventError {
							var payload protocol.ErrorPayload
							if err := protocol.DecodeEventPayload(evt, &payload); err != nil {
								t.Fatal(err)
							}
							if strings.Contains(payload.Message, ErrBackendCleanupIncomplete.Error()) {
								break read
							}
						}
					case <-deadline:
						t.Fatal("native terminal did not retire or report retained cleanup")
					}
				}
				if failRelease {
					select {
					case <-e.done:
						t.Fatal("natural-exit cleanup failure closed its owner")
					default:
					}
					b.failClose.Store(false)
					if err := e.Kill(); err != nil {
						t.Fatal(err)
					}
					awaitResourceEngineClosed(t, e)
					for evt := range e.events {
						events = append(events, evt)
					}
				}
				exits := 0
				for _, evt := range events {
					if evt.Kind == protocol.EventProcessExited {
						exits++
						var payload protocol.ProcessExitedPayload
						if err := protocol.DecodeEventPayload(evt, &payload); err != nil || payload.ExitCode != 23 {
							t.Fatalf("real exit status lost: %+v %v", payload, err)
						}
					}
				}
				want := 1
				if failWait {
					want = 0
				}
				if exits != want || !b.released.Load() || !b.waitDone.Load() {
					t.Fatalf("exits=%d want=%d, released=%v waiter=%v",
						exits, want, b.released.Load(), b.waitDone.Load())
				}
			})
		}
	}
}

func TestBackendResourcesRetainPartialStartupFailure(t *testing.T) {
	b := &resourceTestBackend{}
	b.failClose.Store(true)
	e := newResourceTestEngine(t, b)
	if err := e.dispatch(func() error {
		return fmt.Errorf("launch failed: %w", ErrBackendCleanupIncomplete)
	}); !errors.Is(err, ErrBackendCleanupIncomplete) {
		t.Fatal(err)
	}
	if err := e.Attach(17, ""); !errors.Is(err, ErrBackendCleanupIncomplete) {
		t.Fatalf("a second startup could overwrite retained namespace ownership: %v", err)
	}
	if err := e.Kill(); !errors.Is(err, ErrBackendCleanupIncomplete) {
		t.Fatalf("partial-startup disposal = %v", err)
	}
	b.failClose.Store(false)
	if err := e.Kill(); err != nil {
		t.Fatal(err)
	}
	awaitResourceEngineClosed(t, e)
}

type heldResourceTestBackend struct {
	*retainedWaitBackend
	released atomic.Bool
}

func (b *heldResourceTestBackend) releaseBackendResources() error {
	if !b.acknowledged.Load() {
		return errors.New("resource release preceded the real waiter's acknowledgement")
	}
	b.released.Store(true)
	return nil
}

func TestBackendResourcesSuppressALateStopAfterLaunchedJoinTimeout(t *testing.T) {
	previousTimeout := attachedDetachTimeout
	attachedDetachTimeout = 20 * time.Millisecond
	defer func() { attachedDetachTimeout = previousTimeout }()
	b := &heldResourceTestBackend{
		retainedWaitBackend: &retainedWaitBackend{heldWaitBackend: newHeldWaitBackend()},
	}
	e := newEngine(b, nil)
	defer func() {
		b.unblock()
		done := make(chan error, 1)
		go func() { done <- e.Kill() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("cleanup late native waiter: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("late native waiter cleanup did not finish")
		}
	}()
	if err := e.dispatch(func() error {
		e.setState(stateRunning)
		e.startWait()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-b.entered
	if err := e.Kill(); !errors.Is(err, ErrBackendCleanupIncomplete) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Kill = %v, want a retained exact waiter", err)
	}
	b.unblock()
	deadline := time.Now().Add(time.Second)
	for {
		retired := false
		if err := e.dispatchCommand(func() error {
			retired = e.wait == nil
			return nil
		}, true); err != nil {
			t.Fatal(err)
		}
		if retired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late native waiter was not retired")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case evt := <-e.events:
		t.Fatalf("late stop re-entered the teardown-latched backend: %s", evt.Kind)
	default:
	}
	if b.released.Load() || b.acknowledged.Load() {
		t.Fatal("late result alone claimed namespace release")
	}
	if err := e.Kill(); err != nil {
		t.Fatal(err)
	}
	awaitResourceEngineClosed(t, e)
	if !b.released.Load() {
		t.Fatal("retry skipped namespace retirement")
	}
}
