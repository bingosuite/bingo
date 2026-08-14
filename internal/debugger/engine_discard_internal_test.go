package debugger

import (
	"sync"
	"testing"
)

type legacyAttachedBackend struct {
	mu     sync.Mutex
	writes int
}

func (*legacyAttachedBackend) ContinueProcess() error              { return nil }
func (*legacyAttachedBackend) SingleStep(int) error                { return nil }
func (*legacyAttachedBackend) StopProcess() error                  { return nil }
func (*legacyAttachedBackend) PauseSignal() int                    { return 0 }
func (*legacyAttachedBackend) ReadMemory(uint64, []byte) error     { return nil }
func (*legacyAttachedBackend) GetRegisters(int) (Registers, error) { return Registers{}, nil }
func (*legacyAttachedBackend) SetRegisters(int, Registers) error   { return nil }
func (*legacyAttachedBackend) Threads() ([]int, error)             { return []int{1}, nil }
func (*legacyAttachedBackend) Wait() (StopEvent, error)            { return StopEvent{}, ErrProcessExited }

func (b *legacyAttachedBackend) WriteMemory(uint64, []byte) error {
	b.mu.Lock()
	b.writes++
	b.mu.Unlock()
	return nil
}

func TestDiscardTraceeKeepsLegacyAttachedBackendCleanup(t *testing.T) {
	backend := &legacyAttachedBackend{}
	e := newEngine(backend, nil)
	if err := e.dispatch(func() error {
		e.proc = process{live: true, isAttached: true}
		entry := &breakpointEntry{
			id:            1,
			addr:          0x401000,
			originalBytes: []byte{0x90},
			enabled:       true,
		}
		e.bps.byID[entry.id] = entry
		e.bps.byAddr[entry.addr] = entry
		return e.discardTracee(true)
	}); err != nil {
		t.Fatalf("discardTracee() error = %v", err)
	}
	backend.mu.Lock()
	writes := backend.writes
	backend.mu.Unlock()
	if writes != 1 {
		t.Fatalf("legacy attached restore writes = %d, want 1", writes)
	}
	if e.proc.live {
		t.Fatal("legacy attached process remained live after cleanup")
	}
	_ = e.Kill()
}
