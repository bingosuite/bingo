//go:build linux && amd64

package debugger

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

//go:embed engine.go
var engineSource []byte

func TestEngineWaitersStartOnlyThroughTheTrackedLifecycle(t *testing.T) {
	if bytes.Contains(engineSource, []byte("go e.waitLoop()")) {
		t.Fatal("engine.go starts an untracked backend waiter")
	}
}

type detachEngineBackend struct {
	mu sync.Mutex

	mem  map[uint64]byte
	regs map[int]Registers
	tids []int

	stops      []StopEvent
	operations []string
	waitCount  int
	waitReady  chan struct{}
	waitOnce   sync.Once
	waitErr    error
	writeErr   error
	detachErrs []error
	quiesceErr error
	quiesced   bool
	imageGone  bool
}

func newDetachEngineBackend() *detachEngineBackend {
	return &detachEngineBackend{
		mem:       make(map[uint64]byte),
		regs:      make(map[int]Registers),
		tids:      []int{1},
		waitReady: make(chan struct{}),
		quiesced:  true,
	}
}

func (b *detachEngineBackend) record(operation string) {
	b.mu.Lock()
	b.operations = append(b.operations, operation)
	b.mu.Unlock()
}

func (b *detachEngineBackend) ContinueProcess() error { b.record("continue"); return nil }
func (b *detachEngineBackend) SingleStep(int) error   { b.record("step"); return nil }
func (b *detachEngineBackend) StopProcess() error     { b.record("stop"); return nil }
func (*detachEngineBackend) PauseSignal() int         { return int(syscall.SIGSTOP) }

func (b *detachEngineBackend) ReadMemory(addr uint64, dst []byte) error {
	b.record("read-memory")
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range dst {
		dst[i] = b.mem[addr+uint64(i)]
	}
	return nil
}

func (b *detachEngineBackend) WriteMemory(addr uint64, src []byte) error {
	b.record("write-memory")
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.writeErr != nil {
		err := b.writeErr
		b.writeErr = nil
		return err
	}
	for i, value := range src {
		b.mem[addr+uint64(i)] = value
	}
	return nil
}

func (b *detachEngineBackend) GetRegisters(tid int) (Registers, error) {
	b.record("get-registers")
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.regs[tid], nil
}

func (b *detachEngineBackend) SetRegisters(tid int, regs Registers) error {
	b.record("set-registers")
	b.mu.Lock()
	b.regs[tid] = regs
	b.mu.Unlock()
	return nil
}

func (b *detachEngineBackend) Threads() ([]int, error) {
	return append([]int(nil), b.tids...), nil
}

func (b *detachEngineBackend) Wait() (StopEvent, error) {
	return b.wait(context.Background())
}

func (b *detachEngineBackend) wait(ctx context.Context) (StopEvent, error) {
	b.mu.Lock()
	b.waitCount++
	b.operations = append(b.operations, "wait")
	b.mu.Unlock()
	b.waitOnce.Do(func() { close(b.waitReady) })
	b.mu.Lock()
	err := b.waitErr
	b.waitErr = nil
	b.mu.Unlock()
	if err != nil {
		return StopEvent{}, err
	}
	<-ctx.Done()
	return StopEvent{}, ctx.Err()
}

func (b *detachEngineBackend) quiesceAttached(context.Context) (bool, error) {
	b.record("quiesce")
	return false, b.quiesceErr
}

func (*detachEngineBackend) retainsAttachedOwnership() bool { return true }

func (b *detachEngineBackend) attachedDetachStops() []StopEvent {
	return append([]StopEvent(nil), b.stops...)
}

func (b *detachEngineBackend) attachedQuiesced() bool { return b.quiesced }

func (b *detachEngineBackend) attachedImageReplaced() bool { return b.imageGone }

func (b *detachEngineBackend) selectAttachedWriteTID() (int, error) {
	b.record("select-write-tid")
	return 1, nil
}

func (b *detachEngineBackend) detachAttached() error {
	b.record("detach")
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.detachErrs) == 0 {
		return nil
	}
	err := b.detachErrs[0]
	b.detachErrs = b.detachErrs[1:]
	return err
}

func prepareRunningAttachedEngine(
	t *testing.T,
	b *detachEngineBackend,
	addr uint64,
	installed bool,
) *engine {
	t.Helper()
	e := newEngine(b, nil)
	if err := e.dispatch(func() error {
		e.proc = process{pid: 1, live: true, isAttached: true}
		e.setState(stateRunning)
		if installed {
			entry := &breakpointEntry{
				id:            1,
				addr:          addr,
				originalBytes: []byte{0x90},
				enabled:       true,
			}
			e.bps.byID[entry.id] = entry
			e.bps.byAddr[entry.addr] = entry
			b.mem[addr] = archTrapInstruction()[0]
		}
		e.startWait()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.waitReady:
	case <-time.After(time.Second):
		t.Fatal("engine waiter did not start")
	}
	return e
}

func TestAttachedEngineKillCancelsOneWaiterBeforeRewindRestoreAndDetach(t *testing.T) {
	const addr = uint64(0x401000)
	b := newDetachEngineBackend()
	b.regs[1] = Registers{PC: addr + 1}
	b.stops = []StopEvent{{Reason: StopBreakpoint, TID: 1}}
	e := prepareRunningAttachedEngine(t, b, addr, true)

	if err := e.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("engine did not close after successful detach")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waitCount != 1 {
		t.Fatalf("wait count = %d, want exactly one", b.waitCount)
	}
	if b.mem[addr] != 0x90 {
		t.Fatalf("restored byte = %#x, want 0x90", b.mem[addr])
	}
	if b.regs[1].PC != addr {
		t.Fatalf("detached PC = %#x, want %#x", b.regs[1].PC, addr)
	}
	wantOrder := []string{"wait", "quiesce", "select-write-tid", "get-registers", "set-registers", "write-memory", "detach"}
	if !reflect.DeepEqual(b.operations, wantOrder) {
		t.Fatalf("operations = %v, want %v", b.operations, wantOrder)
	}
}

func TestAttachedEngineWaitErrorStillRestoresAndDetachesBeforeExit(t *testing.T) {
	const addr = uint64(0x401800)
	b := newDetachEngineBackend()
	b.waitErr = errors.New("ordinary wait failure")
	b.regs[1] = Registers{PC: addr + 1}
	b.stops = []StopEvent{{Reason: StopBreakpoint, TID: 1}}
	e := prepareRunningAttachedEngine(t, b, addr, true)

	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("engine did not release attached ownership after the wait error")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mem[addr] != 0x90 {
		t.Fatalf("wait-error cleanup byte = %#x, want 0x90", b.mem[addr])
	}
	foundDetach := false
	for _, operation := range b.operations {
		if operation == "detach" {
			foundDetach = true
		}
	}
	if !foundDetach {
		t.Fatal("wait-error cleanup exited without detaching")
	}
}

func TestAttachedEngineKillRetainsRestoreFailureForRetry(t *testing.T) {
	const addr = uint64(0x402000)
	b := newDetachEngineBackend()
	b.regs[1] = Registers{PC: addr + 1}
	b.stops = []StopEvent{{Reason: StopBreakpoint, TID: 1}}
	b.writeErr = syscall.EIO
	e := prepareRunningAttachedEngine(t, b, addr, true)

	err := e.Kill()
	if !errors.Is(err, ErrAttachedDetachIncomplete) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("first Kill() error = %v", err)
	}
	if !e.proc.attached() || e.getState() != stateSuspended {
		t.Fatalf("failed detach lost retry state: attached=%v state=%v", e.proc.attached(), e.getState())
	}
	select {
	case event := <-e.events:
		if event.Kind != "Paused" {
			t.Fatalf("failure event = %s, want Paused", event.Kind)
		}
	case <-time.After(time.Second):
		t.Fatal("running detach failure did not emit a suspending event")
	}

	if err := e.Kill(); err != nil {
		t.Fatalf("retry Kill() error = %v", err)
	}
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("engine did not close after retry")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waitCount != 1 {
		t.Fatalf("retry started a competing waiter: %d", b.waitCount)
	}
	if b.mem[addr] != 0x90 {
		t.Fatalf("retry restored byte = %#x", b.mem[addr])
	}
}

func TestAttachedEngineKillDoesNotClaimSuspensionWhenQuiesceIsIncomplete(t *testing.T) {
	b := newDetachEngineBackend()
	b.quiesced = false
	b.quiesceErr = context.DeadlineExceeded
	e := prepareRunningAttachedEngine(t, b, 0, false)

	err := e.Kill()
	if !errors.Is(err, ErrAttachedDetachIncomplete) ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Kill() error = %v", err)
	}
	if e.getState() != stateRunning {
		t.Fatalf("incomplete quiesce state = %v, want running", e.getState())
	}
	select {
	case event := <-e.events:
		t.Fatalf("incomplete quiesce emitted false suspension event %s", event.Kind)
	default:
	}

	b.quiesceErr = nil
	b.quiesced = true
	if err := e.Kill(); err != nil {
		t.Fatalf("retry Kill() error = %v", err)
	}
}

func TestAttachedEngineKillDoesNotRewindAnAlreadyRewoundBreakpointTwice(t *testing.T) {
	const addr = uint64(0x402800)
	b := newDetachEngineBackend()
	b.regs[1] = Registers{PC: addr}
	b.stops = []StopEvent{{Reason: StopBreakpoint, TID: 1}}
	e := newEngine(b, nil)
	if err := e.dispatch(func() error {
		e.proc = process{pid: 1, live: true, isAttached: true}
		e.setState(stateSuspended)
		entry := &breakpointEntry{
			id:            1,
			addr:          addr,
			originalBytes: []byte{0x90},
			enabled:       true,
		}
		e.bps.byID[entry.id] = entry
		e.bps.byAddr[entry.addr] = entry
		e.lastBP = entry
		e.lastBPTID = 1
		b.mem[addr] = archTrapInstruction()[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := e.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.regs[1].PC != addr {
		t.Fatalf("already-rewound PC moved to %#x, want %#x", b.regs[1].PC, addr)
	}
	for _, operation := range b.operations {
		if operation == "set-registers" {
			t.Fatal("already-rewound breakpoint wrote registers again")
		}
	}
}

func TestAttachedEngineKillRetainsExactDetachFailureForRetry(t *testing.T) {
	const addr = uint64(0x403000)
	b := newDetachEngineBackend()
	b.regs[1] = Registers{PC: addr + 1}
	b.stops = []StopEvent{{Reason: StopBreakpoint, TID: 1}}
	b.detachErrs = []error{syscall.EPERM}
	e := prepareRunningAttachedEngine(t, b, addr, true)

	err := e.Kill()
	if !errors.Is(err, ErrAttachedDetachIncomplete) || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("first Kill() error = %v", err)
	}
	if !e.proc.attached() {
		t.Fatal("failed PTRACE_DETACH cleared process ownership")
	}
	if err := e.Kill(); err != nil {
		t.Fatalf("retry Kill() error = %v", err)
	}
}

func TestAttachedEngineKillNeverRestoresBytesAfterExecReplacedTheImage(t *testing.T) {
	const addr = uint64(0x403800)
	b := newDetachEngineBackend()
	b.imageGone = true
	b.regs[1] = Registers{PC: addr + 1}
	b.stops = []StopEvent{{Reason: StopBreakpoint, TID: 1}}
	e := prepareRunningAttachedEngine(t, b, addr, true)

	if err := e.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, operation := range b.operations {
		if operation == "write-memory" || operation == "set-registers" {
			t.Fatalf("replaced image was mutated during detach: %v", b.operations)
		}
	}
}

func TestAttachedEngineKillRewindsDelayedRetiredBreakpointWithOneWaiter(t *testing.T) {
	const addr = uint64(0x404000)
	b := newDetachEngineBackend()
	b.regs[1] = Registers{PC: addr + 1}
	b.mem[addr] = 0x90
	b.stops = []StopEvent{{Reason: StopBreakpoint, TID: 1}}
	e := prepareRunningAttachedEngine(t, b, addr, false)
	if err := e.dispatch(func() error {
		e.retiredInternalBreakpointBytes = map[uint64][][]byte{addr: {{0x90}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := e.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waitCount != 1 {
		t.Fatalf("wait count = %d, want one", b.waitCount)
	}
	if b.regs[1].PC != addr {
		t.Fatalf("retired breakpoint PC = %#x, want %#x", b.regs[1].PC, addr)
	}
}

func TestLaunchedKillFailureStillClosesTheEngine(t *testing.T) {
	b := newDetachEngineBackend()
	e := newEngine(b, nil)
	if err := e.dispatch(func() error {
		e.proc = process{pid: 1, live: true}
		e.setState(stateSuspended)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Kill(); err == nil {
		t.Fatal("Kill() unexpectedly succeeded")
	}
	select {
	case <-e.done:
	case <-time.After(time.Second):
		t.Fatal("launched kill failure leaked the engine loop")
	}
}
