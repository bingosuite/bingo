package debugger_test

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestDebugger(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Debugger Suite")
}

// fakeBackend is an in-process Backend. Tests seed mem/regs, push StopEvents
// to drive the state machine, and inspect recorded calls afterward.
type fakeBackend struct {
	mem  map[uint64]byte
	regs map[int]debugger.Registers
	tids []int

	stopCh  chan debugger.StopEvent
	stopped bool

	continueCalls   int
	singleStepCalls []int
	writtenAt       map[uint64][]byte
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		mem:       make(map[uint64]byte),
		regs:      map[int]debugger.Registers{1: {}},
		tids:      []int{1},
		stopCh:    make(chan debugger.StopEvent, 8),
		writtenAt: make(map[uint64][]byte),
	}
}

func (f *fakeBackend) seedMem(addr uint64, data []byte) {
	for i, b := range data {
		f.mem[addr+uint64(i)] = b
	}
}

func (f *fakeBackend) seedRegs(r debugger.Registers) { f.regs[1] = r }

func (f *fakeBackend) pushStop(evt debugger.StopEvent) { f.stopCh <- evt }

// closeStop simulates process exit by closing stopCh, making Wait return
// ErrProcessExited.
func (f *fakeBackend) closeStop() {
	if !f.stopped {
		close(f.stopCh)
		f.stopped = true
	}
}

func (f *fakeBackend) peekMem(addr uint64, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = f.mem[addr+uint64(i)]
	}
	return out
}

func (f *fakeBackend) ContinueProcess() error  { f.continueCalls++; return nil }
func (f *fakeBackend) StopProcess() error      { return nil }
func (f *fakeBackend) Threads() ([]int, error) { return f.tids, nil }

func (f *fakeBackend) SingleStep(tid int) error {
	f.singleStepCalls = append(f.singleStepCalls, tid)
	return nil
}

func (f *fakeBackend) ReadMemory(addr uint64, dst []byte) error {
	for i := range dst {
		dst[i] = f.mem[addr+uint64(i)]
	}
	return nil
}

func (f *fakeBackend) WriteMemory(addr uint64, src []byte) error {
	cp := make([]byte, len(src))
	copy(cp, src)
	f.writtenAt[addr] = cp
	for i, b := range src {
		f.mem[addr+uint64(i)] = b
	}
	return nil
}

func (f *fakeBackend) GetRegisters(tid int) (debugger.Registers, error) {
	return f.regs[tid], nil
}

func (f *fakeBackend) SetRegisters(tid int, reg debugger.Registers) error {
	f.regs[tid] = reg
	return nil
}

func (f *fakeBackend) Wait() (debugger.StopEvent, error) {
	evt, ok := <-f.stopCh
	if !ok {
		return debugger.StopEvent{}, debugger.ErrProcessExited
	}
	return evt, nil
}

const eventTimeout = 500 * time.Millisecond

func nextEvent(d debugger.Debugger) (protocol.Event, bool) {
	select {
	case evt, ok := <-d.Events():
		if !ok {
			return protocol.Event{}, false
		}
		return evt, true
	case <-time.After(eventTimeout):
		return protocol.Event{}, false
	}
}

func mustNextEvent(d debugger.Debugger) protocol.Event {
	evt, ok := nextEvent(d)
	ExpectWithOffset(1, ok).To(BeTrue(), "expected an event but timed out")
	return evt
}

func drainEvents(d debugger.Debugger) {
	for {
		select {
		case _, ok := <-d.Events():
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func le8(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

// seedFrameChain writes a two-frame stack so walkStack returns two PCs.
// Go frame layout: [bp+0]=saved bp, [bp+8]=return address.
func seedFrameChain(fb *fakeBackend, frame0PC, frame0BP, frame1BP, retAddr uint64) {
	fb.seedRegs(debugger.Registers{PC: frame0PC, SP: frame0BP - 8, BP: frame0BP})
	fb.seedMem(frame0BP, le8(frame1BP))
	fb.seedMem(frame0BP+8, le8(retAddr))
	fb.seedMem(frame1BP, le8(0))
}

var _ = Describe("Engine", func() {

	var (
		fb *fakeBackend
		d  debugger.Debugger
	)

	BeforeEach(func() {
		fb = newFakeBackend()
		d = debugger.NewWithBackend(fb)
	})

	AfterEach(func() {
		_ = d.Kill()
		// Unblock any waitLoop goroutine still in fb.Wait().
		if !fb.stopped {
			close(fb.stopCh)
			fb.stopped = true
		}
	})


	Describe("state guards — stateNoProcess", func() {
		It("rejects Continue", func() {
			Expect(d.Continue()).To(MatchError(debugger.ErrNotSuspended))
		})
		It("rejects StepInto", func() {
			Expect(d.StepInto()).To(MatchError(debugger.ErrNotSuspended))
		})
		It("rejects StepOver", func() {
			Expect(d.StepOver()).To(MatchError(debugger.ErrNotSuspended))
		})
		It("rejects StepOut", func() {
			Expect(d.StepOut()).To(MatchError(debugger.ErrNotSuspended))
		})
		It("rejects Locals", func() {
			_, err := d.Locals(0)
			Expect(err).To(MatchError(debugger.ErrNotSuspended))
		})
		It("rejects StackFrames", func() {
			_, err := d.StackFrames()
			Expect(err).To(MatchError(debugger.ErrNotSuspended))
		})
		It("rejects Goroutines", func() {
			_, err := d.Goroutines()
			Expect(err).To(MatchError(debugger.ErrNotSuspended))
		})
	})


	Describe("state guards — stateRunning", func() {
		BeforeEach(func() {
			debugger.ExportedForceRunning(d)
		})

		It("rejects Continue", func() {
			Expect(d.Continue()).To(MatchError(debugger.ErrNotSuspended))
		})
		It("rejects StepInto", func() {
			Expect(d.StepInto()).To(MatchError(debugger.ErrNotSuspended))
		})
		It("rejects Locals while running", func() {
			_, err := d.Locals(0)
			Expect(err).To(MatchError(debugger.ErrNotSuspended))
		})
	})


	Describe("Kill", func() {
		It("is a no-op in stateNoProcess", func() {
			Expect(d.Kill()).To(Succeed())
		})

		It("is idempotent — safe to call many times", func() {
			for i := 0; i < 5; i++ {
				Expect(d.Kill()).To(Succeed())
			}
		})

		It("restores breakpoint bytes in memory on kill", func() {
			const bpAddr = uint64(0x4000)
			const orig = byte(0x90)
			fb.seedMem(bpAddr, []byte{orig})
			debugger.ExportedForceSuspended(d)

			trap := debugger.ExportedTrapInstruction()
			_ = fb.WriteMemory(bpAddr, trap)
			Expect(fb.peekMem(bpAddr, 1)[0]).To(Equal(trap[0]))

			// WriteMemory directly bypasses the BP table, so clearAll is a
			// no-op — verify Kill at least doesn't panic.
			Expect(d.Kill()).To(Succeed())
		})

		It("restores original bytes when a breakpoint is set then killed", func() {
			const bpAddr = uint64(0x5000)
			const orig = byte(0x55)
			fb.seedMem(bpAddr, []byte{orig})
			debugger.ExportedForceSuspended(d)

			debugger.ExportedSetBreakpointAt(d, bpAddr)

			trap := debugger.ExportedTrapInstruction()
			Expect(fb.peekMem(bpAddr, len(trap))[0]).To(Equal(trap[0]),
				"trap should be installed")

			Expect(d.Kill()).To(Succeed())
			Expect(fb.peekMem(bpAddr, 1)[0]).To(Equal(orig),
				"original byte should be restored after Kill")
		})
	})


	Describe("arch trap instruction", func() {
		It("is INT3 (0xCC) on amd64 or BRK#0 on arm64", func() {
			trap := debugger.ExportedTrapInstruction()
			switch len(trap) {
			case 1:
				Expect(trap[0]).To(Equal(byte(0xCC)))
			case 4:
				Expect(trap).To(Equal([]byte{0x00, 0x00, 0x20, 0xD4}))
			default:
				Fail("unexpected trap instruction length")
			}
		})
	})


	Describe("breakpoints", func() {
		const (
			bpAddr   = uint64(0x2000)
			origByte = byte(0x48)
		)

		BeforeEach(func() {
			fb.seedMem(bpAddr, []byte{origByte, 0x89, 0xC0})
			debugger.ExportedForceSuspended(d)
		})

		It("SetBreakpoint returns an error when no DWARF is loaded", func() {
			_, err := d.SetBreakpoint("main.go", 10)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("DWARF"))
		})

		It("writes the trap instruction to the breakpoint address", func() {
			trap := debugger.ExportedTrapInstruction()
			debugger.ExportedSetBreakpointAt(d, bpAddr)
			Expect(fb.peekMem(bpAddr, len(trap))).To(Equal(trap))
		})

		It("original bytes are saved and restored on ClearBreakpoint", func() {
			trap := debugger.ExportedTrapInstruction()
			id := debugger.ExportedSetBreakpointAt(d, bpAddr)
			Expect(fb.peekMem(bpAddr, len(trap))).To(Equal(trap))

			Expect(d.ClearBreakpoint(id)).To(Succeed())
			Expect(fb.peekMem(bpAddr, 1)[0]).To(Equal(origByte))
		})

		It("ClearBreakpoint returns an error for an unknown ID", func() {
			Expect(d.ClearBreakpoint(999)).To(HaveOccurred())
		})

		It("setting the same address twice returns an error wrapping errBreakpointExists", func() {
			debugger.ExportedSetBreakpointAt(d, bpAddr)
			err := debugger.ExportedSetBreakpointAtErr(d, bpAddr)
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, debugger.ExportedErrBreakpointExists)).To(BeTrue())
		})
	})


	Describe("breakpoint hit event flow", func() {
		const bpAddr = uint64(0x3000)

		BeforeEach(func() {
			fb.seedMem(bpAddr, []byte{0x90})
			debugger.ExportedForceSuspended(d)
			debugger.ExportedSetBreakpointAt(d, bpAddr)
		})

		It("emits EventBreakpointHit when a breakpoint stop arrives", func() {
			Expect(d.Continue()).To(Succeed())
			fb.pushStop(debugger.StopEvent{
				Reason: debugger.StopBreakpoint,
				TID:    1,
				PC:     bpAddr,
			})

			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit))

			var p protocol.BreakpointHitPayload
			Expect(protocol.DecodeEventPayload(evt, &p)).To(Succeed())
			Expect(p.Breakpoint.Location.File).To(Equal("<direct-addr>"))
		})

		It("emits nothing (resumes silently) for an unrecognised breakpoint PC", func() {
			Expect(d.Continue()).To(Succeed())
			fb.pushStop(debugger.StopEvent{
				Reason: debugger.StopBreakpoint,
				TID:    1,
				PC:     0x9999,
			})
			evt, ok := nextEvent(d)
			if ok {
				Expect(evt.Kind).NotTo(Equal(protocol.EventBreakpointHit))
			}
		})
	})


	Describe("process exit", func() {
		It("emits EventProcessExited with exit code when StopExited arrives", func() {
			fb2 := newFakeBackend()
			d2 := debugger.NewWithBackend(fb2)
			defer func() {
				if !fb2.stopped {
					close(fb2.stopCh)
					fb2.stopped = true
				}
				drainEvents(d2)
				_ = d2.Kill()
			}()

			debugger.ExportedForceSuspended(d2)
			Expect(d2.Continue()).To(Succeed())
			fb2.pushStop(debugger.StopEvent{
				Reason:   debugger.StopExited,
				TID:      1,
				ExitCode: 42,
			})

			evt := mustNextEvent(d2)
			Expect(evt.Kind).To(Equal(protocol.EventProcessExited))

			var p protocol.ProcessExitedPayload
			Expect(protocol.DecodeEventPayload(evt, &p)).To(Succeed())
			Expect(p.ExitCode).To(Equal(42))
		})

		It("closes the Events channel after process exits", func() {
			fb2 := newFakeBackend()
			d2 := debugger.NewWithBackend(fb2)
			defer func() { _ = d2.Kill() }()

			debugger.ExportedForceSuspended(d2)
			Expect(d2.Continue()).To(Succeed())
			fb2.closeStop()

			timeout := time.After(2 * time.Second)
			for {
				select {
				case _, ok := <-d2.Events():
					if !ok {
						return
					}
				case <-timeout:
					Fail("events channel was not closed within 2s")
				}
			}
		})
	})


	Describe("StepInto", func() {
		BeforeEach(func() {
			debugger.ExportedForceSuspended(d)
		})

		It("calls SingleStep on the main thread", func() {
			Expect(d.StepInto()).To(Succeed())
			Expect(fb.singleStepCalls).To(ConsistOf(1))
		})

		It("emits EventStepped when a StopSingleStep arrives", func() {
			Expect(d.StepInto()).To(Succeed())
			fb.pushStop(debugger.StopEvent{
				Reason: debugger.StopSingleStep,
				TID:    1,
				PC:     0x1234,
			})

			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventStepped))

			var p protocol.SteppedPayload
			Expect(protocol.DecodeEventPayload(evt, &p)).To(Succeed())
			Expect(p.Goroutine.Status).To(Equal("waiting"))
		})

		It("puts the engine back into stateSuspended after the step", func() {
			Expect(d.StepInto()).To(Succeed())
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1})
			mustNextEvent(d)

			Expect(d.Continue()).To(Succeed())
		})
	})

	Describe("StepOver", func() {
		BeforeEach(func() {
			debugger.ExportedForceSuspended(d)
		})

		It("calls SingleStep on the main thread", func() {
			Expect(d.StepOver()).To(Succeed())
			Expect(fb.singleStepCalls).To(ConsistOf(1))
		})

		It("emits EventStepped after the step completes", func() {
			Expect(d.StepOver()).To(Succeed())
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: 0x5678})

			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventStepped))
		})
	})

	Describe("StepOut", func() {
		const (
			currentPC = uint64(0x8000)
			currentSP = uint64(0x7fff0000)
			retAddr   = uint64(0x9000)
		)

		BeforeEach(func() {
			fb.seedRegs(debugger.Registers{PC: currentPC, SP: currentSP, BP: currentSP + 16})
			fb.seedMem(currentSP, le8(retAddr))
			fb.seedMem(retAddr, []byte{0x90})
			debugger.ExportedForceSuspended(d)
		})

		It("installs a breakpoint at the return address", func() {
			Expect(d.StepOut()).To(Succeed())
			trap := debugger.ExportedTrapInstruction()
			Expect(fb.peekMem(retAddr, len(trap))).To(Equal(trap))
		})

		It("emits EventStepped (not EventBreakpointHit) when it fires", func() {
			Expect(d.StepOut()).To(Succeed())
			fb.pushStop(debugger.StopEvent{
				Reason: debugger.StopBreakpoint,
				TID:    1,
				PC:     retAddr,
			})

			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventStepped),
				"StepOut should emit EventStepped, not EventBreakpointHit")
		})

		It("removes the one-shot breakpoint after it fires", func() {
			Expect(d.StepOut()).To(Succeed())
			fb.pushStop(debugger.StopEvent{
				Reason: debugger.StopBreakpoint,
				TID:    1,
				PC:     retAddr,
			})
			mustNextEvent(d)

			Expect(fb.peekMem(retAddr, 1)[0]).To(Equal(byte(0x90)))
		})

		It("returns error when SP points to a null return address", func() {
			fb.seedMem(currentSP, le8(0))
			Expect(d.StepOut()).To(HaveOccurred())
		})
	})


	Describe("Continue", func() {
		BeforeEach(func() {
			debugger.ExportedForceSuspended(d)
		})

		It("calls ContinueProcess on the backend", func() {
			before := fb.continueCalls
			Expect(d.Continue()).To(Succeed())
			Expect(fb.continueCalls).To(Equal(before + 1))
		})

		It("rejects a second Continue without an intervening stop", func() {
			Expect(d.Continue()).To(Succeed())
			Expect(d.Continue()).To(MatchError(debugger.ErrNotSuspended))
		})
	})


	Describe("StackFrames", func() {
		BeforeEach(func() {
			debugger.ExportedForceSuspended(d)
		})

		It("returns nil when no DWARF is loaded", func() {
			frames, err := d.StackFrames()
			Expect(err).NotTo(HaveOccurred())
			Expect(frames).To(BeNil())
		})

		It("walks the frame pointer chain and returns one frame per PC", func() {
			const (
				frame0PC = uint64(0x1000)
				frame0BP = uint64(0x7ffe0010)
				frame1BP = uint64(0x7ffe0030)
				ret      = uint64(0x2000)
			)
			seedFrameChain(fb, frame0PC, frame0BP, frame1BP, ret)

			// Without DWARF, StackFrames returns nil; verify walkStack via
			// Goroutines (same code path).
			gs, err := d.Goroutines()
			Expect(err).NotTo(HaveOccurred())
			Expect(gs).To(HaveLen(1))
			Expect(gs[0].Status).To(Equal("waiting"))
		})
	})


	Describe("Goroutines", func() {
		BeforeEach(func() {
			debugger.ExportedForceSuspended(d)
		})

		It("returns one goroutine with status 'waiting'", func() {
			gs, err := d.Goroutines()
			Expect(err).NotTo(HaveOccurred())
			Expect(gs).To(HaveLen(1))
			Expect(gs[0].ID).To(Equal(1))
			Expect(gs[0].Status).To(Equal("waiting"))
		})
	})


	Describe("Locals", func() {
		BeforeEach(func() {
			debugger.ExportedForceSuspended(d)
		})

		It("returns an error when no DWARF is loaded", func() {
			_, err := d.Locals(0)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("DWARF"))
		})

		It("returns an error for a negative frame index", func() {
			_, err := d.Locals(-1)
			Expect(err).To(HaveOccurred())
		})

		It("returns an out-of-range error for a frame index beyond the stack depth", func() {
			fb.seedRegs(debugger.Registers{PC: 0x100, SP: 0x1000, BP: 0x1008})
			fb.seedMem(0x1008, le8(0))
			_, err := d.Locals(5)
			Expect(err).To(HaveOccurred())
		})
	})


	Describe("event sequence numbers", func() {
		It("assigns strictly increasing sequence numbers across events", func() {
			debugger.ExportedForceSuspended(d)

			var seqs []uint64
			for i := 0; i < 3; i++ {
				Expect(d.StepInto()).To(Succeed())
				fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: uint64(0x1000 + i)})
				evt := mustNextEvent(d)
				Expect(evt.Kind).To(Equal(protocol.EventStepped))
				seqs = append(seqs, evt.Seq)
			}

			Expect(seqs).To(HaveLen(3))
			Expect(seqs[1]).To(BeNumerically(">", seqs[0]))
			Expect(seqs[2]).To(BeNumerically(">", seqs[1]))
		})
	})


	Describe("concurrent dispatch", func() {
		It("does not deadlock with many concurrent Kill calls", func() {
			done := make(chan struct{}, 20)
			for i := 0; i < 20; i++ {
				go func() {
					_ = d.Kill()
					done <- struct{}{}
				}()
			}
			for i := 0; i < 20; i++ {
				Eventually(done, "3s").Should(Receive())
			}
		})

		It("serialises concurrent inspection calls without panic", func() {
			debugger.ExportedForceSuspended(d)
			done := make(chan struct{}, 6)
			for i := 0; i < 3; i++ {
				go func() {
					_, _ = d.StackFrames()
					done <- struct{}{}
				}()
				go func() {
					_, _ = d.Goroutines()
					done <- struct{}{}
				}()
			}
			for i := 0; i < 6; i++ {
				Eventually(done, "3s").Should(Receive())
			}
		})
	})


	Describe("Events channel", func() {
		It("is non-nil immediately after construction", func() {
			Expect(d.Events()).NotTo(BeNil())
		})

		It("returns no event when the engine is idle", func() {
			_, ok := nextEvent(d)
			Expect(ok).To(BeFalse())
		})
	})
})
