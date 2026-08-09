//go:build linux && amd64

package debugger

import "testing"

func TestLinuxBackendTraceTIDDefaultsToPID(t *testing.T) {
	const pid = 1001

	b := &linuxBackend{pid: pid}
	if got := b.traceTID(); got != pid {
		t.Fatalf("traceTID() = %d, want pid %d", got, pid)
	}
}

func TestLinuxBackendTraceTIDUsesLastStoppedTID(t *testing.T) {
	const (
		pid = 1001
		tid = 1002
	)

	b := &linuxBackend{pid: pid}
	b.recordStop(tid)

	if got := b.traceTID(); got != tid {
		t.Fatalf("traceTID() = %d, want stopped tid %d", got, tid)
	}
}

func TestLinuxBackendSetPIDSeedsLastStoppedTID(t *testing.T) {
	const pid = 1001

	b := &linuxBackend{}
	b.setPID(pid)

	if got := b.traceTID(); got != pid {
		t.Fatalf("traceTID() = %d, want pid %d", got, pid)
	}
}

type stopTIDRaceBackend struct {
	*linuxBackend
	waitStarted chan struct{}
	stopWriter  chan struct{}
}

func newStopTIDRaceBackend(t *testing.T) *stopTIDRaceBackend {
	t.Helper()
	b := &stopTIDRaceBackend{
		linuxBackend: &linuxBackend{
			pid:    1_000_001,
			tracer: newTracerThread(),
		},
		waitStarted: make(chan struct{}),
		stopWriter:  make(chan struct{}),
	}
	b.recordStop(b.pid)
	t.Cleanup(b.closeTracer)
	return b
}

func (b *stopTIDRaceBackend) Wait() (StopEvent, error) {
	close(b.waitStarted)
	for tid := b.pid + 1; ; tid++ {
		select {
		case <-b.stopWriter:
			return StopEvent{Reason: StopExited, TID: b.pid}, nil
		default:
			b.recordStop(tid)
		}
	}
}

func seedBreakpointEntries(e *engine, count int) {
	for i := 0; i < count; i++ {
		id := i + 1
		addr := uint64(0x400000 + i)
		entry := &breakpointEntry{
			id:            id,
			addr:          addr,
			originalBytes: []byte{0x90},
			enabled:       true,
		}
		e.bps.byID[id] = entry
		e.bps.byAddr[addr] = entry
	}
}

// TestLinuxBackendRunningKillStopTIDConcurrentAccess is a production-path race
// regression:
// waitLoop publishes stops through recordStop while the engine actor executes
// running Kill -> breakpointTable.clearAll -> WriteMemory -> traceTID.
//
// Keep this test focused on the ownership boundary. It deliberately uses fake
// TIDs so ptrace writes fail quickly after reading traceTID; clearAll's
// best-effort contract leaves every entry present, giving the race detector
// many reads against the live wait-loop writer.
func TestLinuxBackendRunningKillStopTIDConcurrentAccess(t *testing.T) {
	b := newStopTIDRaceBackend(t)
	e := newEngine(b, nil)

	if err := e.dispatch(func() error {
		e.proc = process{pid: b.pid, live: true}
		e.setState(stateRunning)
		seedBreakpointEntries(e, 16)
		go e.waitLoop()
		return nil
	}); err != nil {
		t.Fatalf("prepare running engine: %v", err)
	}
	<-b.waitStarted

	if err := e.Kill(); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	close(b.stopWriter)
	<-e.done
}

// TestLinuxBackendRunningMemoryStopTIDConcurrentAccess covers the other two
// traceTID readers. WriteMemory is reachable from running SetBreakpoint/
// ClearBreakpoint; ReadMemory's ptrace fallback is reachable from running
// SetBreakpoint when process_vm_readv is unavailable or short-reads.
func TestLinuxBackendRunningMemoryStopTIDConcurrentAccess(t *testing.T) {
	tests := []struct {
		name string
		read func(*linuxBackend) error
	}{
		{
			name: "write-memory",
			read: func(b *linuxBackend) error {
				return b.WriteMemory(0x400000, []byte{0x90})
			},
		},
		{
			name: "read-memory-fallback",
			read: func(b *linuxBackend) error {
				var dst [1]byte
				return b.ReadMemory(0x400000, dst[:])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newStopTIDRaceBackend(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = b.Wait()
			}()
			<-b.waitStarted

			for i := 0; i < 16; i++ {
				_ = tt.read(b.linuxBackend)
			}
			close(b.stopWriter)
			<-done
		})
	}
}

// TestLinuxBackendStoppedMemoryStopTIDOrdered is the stopped-state control.
// Receiving Wait's result orders recordStop before every engine-side memory
// operation, matching the real waitLoop -> stopCh -> engine-loop handoff.
func TestLinuxBackendStoppedMemoryStopTIDOrdered(t *testing.T) {
	tests := []struct {
		name string
		read func(*linuxBackend) error
	}{
		{
			name: "write-memory",
			read: func(b *linuxBackend) error {
				return b.WriteMemory(0x400000, []byte{0x90})
			},
		},
		{
			name: "read-memory-fallback",
			read: func(b *linuxBackend) error {
				var dst [1]byte
				return b.ReadMemory(0x400000, dst[:])
			},
		},
		{
			name: "continue",
			read: func(b *linuxBackend) error {
				return b.ContinueProcess()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := newStopTIDRaceBackend(t)
			result := make(chan error)
			go func() {
				b.recordStop(b.pid + 1)
				result <- nil
			}()
			if err := <-result; err != nil {
				t.Fatal(err)
			}

			if err := tt.read(b.linuxBackend); err == nil {
				t.Fatalf("%s unexpectedly succeeded against fake tid %d", tt.name, b.traceTID())
			} else {
				t.Logf("%s reached the ordered stopped tid: %v", tt.name, err)
			}
		})
	}
}
