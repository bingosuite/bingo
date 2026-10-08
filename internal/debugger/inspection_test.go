package debugger

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

type inspectionMemoryBackend struct {
	*goroutineMemoryBackend
	threadRegs map[int]Registers
	held       bool
	holdErr    error
	stop       StopEvent
}

func (b *inspectionMemoryBackend) inspectionThreads() ([]int, error) {
	b.held = b.holdErr == nil
	return []int{100, 101}, b.holdErr
}

func (b *inspectionMemoryBackend) GetRegisters(tid int) (Registers, error) {
	if !b.held {
		return Registers{}, errors.New("registers read without a proven hold")
	}
	return b.threadRegs[tid], nil
}

func (b *inspectionMemoryBackend) inspectionStop(tid int) (StopEvent, bool) {
	return b.stop, b.stop.TID == tid
}

func inspectionEngine() (*engine, *inspectionMemoryBackend) {
	b := &inspectionMemoryBackend{
		goroutineMemoryBackend: newGoroutineMemoryBackend(),
		threadRegs: map[int]Registers{
			100: {PC: 0x1000, SP: 0x7000},
			101: {PC: 0x2000, SP: 0x6000},
		},
	}
	l := goroutineTestLayout()
	seedTestGoroutine(b.goroutineMemoryBackend, l, 0x2000, 42, 0x8000, 0x9000)
	b.seedU32(0x2000, 4)
	b.seedU64(0x100, 1)
	b.seedU64(0x108, 0x1000)
	b.seedU64(0x1000, 0x2000)
	b.seedU64(0x2000+uint64(l.gSched), 0x3000)
	b.seedU64(0x2000+uint64(l.gSched)+8, 0x8100)
	b.seedU64(0x2000+uint64(l.gSched)+16, 0x8200)
	b.seedU64(0x8200, 0)
	b.seedU64(0x8208, 0)
	e := &engine{
		backend: b, state: stateSuspended, curTID: 100,
		goLayout: l, bps: newBreakpointTable(),
		dw: &dwarfReader{
			varAddrs: map[string]uint64{
				"runtime.allglen": 0x100, "runtime.allgptr": 0x108, "runtime.allgs": 0,
			},
			structOffs: map[string]structLayout{
				"runtime.gobuf": {found: true, offsets: map[string]int64{"pc": 0, "sp": 8, "bp": 16}},
				"runtime.g": {found: true, offsets: map[string]int64{
					"syscallpc": 80, "syscallsp": 88, "syscallbp": 96,
				}},
			},
		},
	}
	return e, b
}

func TestSelectedInspectionUsesSavedContextsOnlyUnderHold(t *testing.T) {
	for _, status := range []uint32{1, 4, 9} {
		e, b := inspectionEngine()
		b.seedU32(0x2000, status)
		got, err := e.goroutineRegisters(42)
		want := Registers{PC: 0x3000, SP: 0x8100, BP: 0x8200}
		if err != nil || !reflect.DeepEqual(got, want) || !b.held {
			t.Fatalf("status %d: registers=%+v, held=%v, error=%v", status, got, b.held, err)
		}
	}
	e, b := inspectionEngine()
	b.seedU32(0x2000, 3)
	b.seedU64(0x2050, 0x4000)
	b.seedU64(0x2058, 0x8100)
	b.seedU64(0x2060, 0x8200)
	got, err := e.goroutineRegisters(42)
	if err != nil || got.PC != 0x4000 {
		t.Fatalf("syscall registers=%+v: %v", got, err)
	}
}

func TestSelectedInspectionNeverBorrowsRunningGobuf(t *testing.T) {
	e, b := inspectionEngine()
	b.seedU32(0x2000, 2)
	if _, err := e.goroutineRegisters(42); err == nil || !strings.Contains(err.Error(), "scheduler or signal stack") {
		t.Fatalf("running goroutine borrowed saved gobuf: %v", err)
	}
	b.threadRegs[101] = Registers{PC: 0x4444, SP: 0x8100, BP: 0x8200}
	got, err := e.goroutineRegisters(42)
	if err != nil || got.PC != 0x4444 {
		t.Fatalf("selected stopped sibling registers=%+v: %v", got, err)
	}
}

func TestSelectedInspectionRejectsInvalidAndUnretiredContexts(t *testing.T) {
	for _, goid := range []int{-1, 0, 1 << 53} {
		e, b := inspectionEngine()
		if _, err := e.goroutineRegisters(goid); err == nil || b.held {
			t.Fatalf("invalid id %d acquired a hold: %v", goid, err)
		}
	}
	e, b := inspectionEngine()
	e.wait = &engineWait{}
	if _, err := e.goroutineRegisters(42); err == nil || b.held {
		t.Fatalf("unretired waiter admitted inspection: %v", err)
	}
	e.wait = nil
	b.fails[0x2018] = 1
	if _, err := e.goroutineRegisters(42); err == nil {
		t.Fatal("unreadable stack bound accepted")
	}
	e, b = inspectionEngine()
	b.holdErr = ErrSessionInvalidated
	if _, err := e.goroutineRegisters(42); !errors.Is(err, ErrSessionInvalidated) ||
		e.inspectionOutcome == nil || e.inspectionFailure == nil || b.held {
		t.Fatalf("fatal hold did not retain its loop-owned outcome: %v", err)
	}
}

func TestSelectedInspectionNormalizesRetiredTrapCopyOnly(t *testing.T) {
	e, b := inspectionEngine()
	b.seedU32(0x2000, 2)
	pc := uint64(0x4444)
	if runtime.GOARCH == "amd64" {
		pc++
	}
	b.threadRegs[101] = Registers{PC: pc, SP: 0x8100, BP: 0x8200}
	b.stop = StopEvent{Reason: StopBreakpoint, TID: 101}
	restored := []byte{0x90, 0x90, 0x90, 0x90}
	if runtime.GOARCH == "arm64" {
		restored = []byte{0x1f, 0x20, 0x03, 0xd5}
	}
	for i, value := range restored {
		b.mem[0x4444+uint64(i)] = value
	}
	e.retiredClearedBreakpointBytes = map[uint64][][]byte{0x4444: {restored}}
	got, err := e.goroutineRegisters(42)
	if err != nil || got.PC != 0x4444 || b.threadRegs[101].PC != pc {
		t.Fatalf("retired trap copy=%+v, live=%+v: %v", got, b.threadRegs[101], err)
	}
	for i, value := range archTrapInstruction() {
		b.mem[0x4444+uint64(i)] = value
	}
	got, err = e.goroutineRegisters(42)
	if err != nil || got.PC != pc {
		t.Fatalf("genuine live trap was treated as retired: %+v: %v", got, err)
	}
}

func TestSelectedInspectionRejectsBadFrameChains(t *testing.T) {
	e, b := inspectionEngine()
	regs := Registers{PC: 0x4444, SP: 0x8100, BP: 0x8200}
	b.seedU64(0x8200, 0x8200)
	b.seedU64(0x8208, 0x1234)
	if _, err := e.checkedInspectionStack(42, regs, 0x8000, 0x9000); err == nil {
		t.Fatal("cyclic chain accepted")
	}
	regs.BP = 0x8ff8
	if _, err := e.checkedInspectionStack(42, regs, 0x8000, 0x9000); err == nil {
		t.Fatal("partial out-of-bounds frame accepted")
	}
	regs.BP = 0x8200
	b.seedU64(0x8200, 0)
	b.fails[0x8200] = 1
	if _, err := e.checkedInspectionStack(42, regs, 0x8000, 0x9000); err == nil {
		t.Fatal("unreadable chain accepted")
	}
}
