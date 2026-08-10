package debugger_test

import (
	"errors"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

var errSyntheticSentinelClear = errors.New("synthetic sentinel clear write failure")

type syntheticWriteCall struct {
	addr uint64
	data []byte
	err  error
}

// syntheticWriteFailBackend records writes while selectively failing a requested
// restore. It is an audit-only seam: unlike a native backend failure, it does
// not establish OS-level reachability.
type syntheticWriteFailBackend struct {
	*fakeBackend
	failAddr uint64
	fail     bool
	writes   []syntheticWriteCall
}

func (b *syntheticWriteFailBackend) WriteMemory(addr uint64, src []byte) error {
	data := append([]byte(nil), src...)
	var err error
	if b.fail && addr == b.failAddr {
		err = errSyntheticSentinelClear
	} else {
		err = b.fakeBackend.WriteMemory(addr, src)
	}
	b.writes = append(b.writes, syntheticWriteCall{addr: addr, data: data, err: err})
	return err
}

func TestSyntheticSentinelClearFailureCharacterization(t *testing.T) {
	const (
		currentPC = uint64(0x8000)
		currentSP = uint64(0x7fff0000)
		currentBP = currentSP + 16
		returnPC  = uint64(0x9000)
		original  = byte(0x90)
	)

	fake := newFakeBackend()
	fake.seedRegs(debugger.Registers{PC: currentPC, SP: currentSP, BP: currentBP})
	fake.seedMem(currentBP+8, le8(returnPC))
	fake.seedMem(returnPC, []byte{original})
	backend := &syntheticWriteFailBackend{fakeBackend: fake, failAddr: returnPC}
	d := debugger.NewWithBackend(backend, nil)
	defer func() {
		_ = d.Kill()
		if !fake.stopped {
			close(fake.stopCh)
			fake.stopped = true
		}
	}()

	debugger.ExportedForceSuspended(d)
	if err := d.StepOut(); err != nil {
		t.Fatalf("set StepOut sentinel: %v", err)
	}

	trap := debugger.ExportedTrapInstruction()
	if got := fake.peekMem(returnPC, len(trap)); string(got) != string(trap) {
		t.Fatalf("StepOut did not install sentinel: got %x want %x", got, trap)
	}

	backend.fail = true
	fake.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 1, PC: returnPC})
	first := mustAuditEvent(t, d)
	if first.Kind != protocol.EventStepped {
		t.Fatalf("synthetic clear failure emitted %s, want %s", first.Kind, protocol.EventStepped)
	}
	if got := fake.peekMem(returnPC, len(trap)); string(got) != string(trap) {
		t.Fatalf("failed clear unexpectedly removed sentinel: got %x want %x", got, trap)
	}
	if !hasSyntheticWriteFailure(backend.writes, returnPC) {
		t.Fatalf("did not observe synthetic failure restoring sentinel at 0x%x", returnPC)
	}

	backend.fail = false
	if err := d.Continue(); err != nil {
		t.Fatalf("continue after false Stepped: %v", err)
	}
	if continued := mustAuditEvent(t, d); continued.Kind != protocol.EventContinued {
		t.Fatalf("continue emitted %s, want %s", continued.Kind, protocol.EventContinued)
	}

	fake.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 1, PC: returnPC})
	second := mustAuditEvent(t, d)
	if second.Kind != protocol.EventStepped {
		t.Fatalf("re-hit sentinel emitted %s, want %s", second.Kind, protocol.EventStepped)
	}
	if got := fake.peekMem(returnPC, 1)[0]; got != original {
		t.Fatalf("successful second clear did not restore original byte: got %x want %x", got, original)
	}

	t.Logf("SYNTHETIC ONLY: ignored WriteMemory failure emitted Stepped, retained the trap/table entry, and allowed a re-hit")
}

func hasSyntheticWriteFailure(writes []syntheticWriteCall, addr uint64) bool {
	for _, write := range writes {
		if write.addr == addr && errors.Is(write.err, errSyntheticSentinelClear) {
			return true
		}
	}
	return false
}

func mustAuditEvent(t *testing.T, d debugger.Debugger) protocol.Event {
	t.Helper()
	select {
	case event := <-d.Events():
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for debugger event")
		return protocol.Event{}
	}
}
