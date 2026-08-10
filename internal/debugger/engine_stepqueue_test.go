package debugger_test

import (
	"bytes"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// rawStop is a kernel-level event, before the park queue has classified it —
// the shape wait4 reports, not the shape the engine consumes.
type rawStop struct {
	tid   int
	trap  bool // a bare SIGTRAP: a software breakpoint or a completed step
	pc    uint64
	death bool // this thread died rather than stopped
}

// queueBackend drives a real stepQueue between a synthetic kernel and a real
// engine.
//
// Every park/surface decision, the death transition, the reconciliation
// boundary and the release gate are the PRODUCTION functions; only the source
// of raw events is synthetic. That is the point: the backend's own tests can
// prove the queue holds a stop, and the engine's own tests can prove it repairs
// a breakpoint, but neither can observe the other's state, so neither can show
// that the repair actually happens BEFORE the held stop is acted on.
//
// Wait mirrors the linux wait loop's top-of-loop order exactly: report the
// death boundary first, then release a held stop, and only then consume the
// next kernel event.
type queueBackend struct {
	*fakeBackend
	q   *debugger.ExportedStepQueue
	raw chan rawStop

	// watchAddr/order record the actual interleaving of the two operations
	// whose order is the whole invariant: re-arming the dead step owner's trap,
	// and releasing the held sibling. Polling either side separately is racy —
	// the drain follows the reinstall closely enough that a poller can observe
	// only the end state — so the ordering is recorded as it happens.
	mu        sync.Mutex
	watchAddr uint64
	order     []string
}

func newQueueBackend(fb *fakeBackend) *queueBackend {
	return &queueBackend{
		fakeBackend: fb,
		q:           &debugger.ExportedStepQueue{},
		raw:         make(chan rawStop, 8),
	}
}

// SingleStep records the step the engine is issuing, exactly as the linux
// backend does before handing the request to the kernel.
func (b *queueBackend) SingleStep(tid int) error {
	b.q.BeginStep(tid)
	return b.fakeBackend.SingleStep(tid)
}

func (b *queueBackend) note(what string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = append(b.order, what)
}

func (b *queueBackend) resetOrder() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.order = nil
}

func (b *queueBackend) events() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.order...)
}

// WriteMemory notes the moment the watched breakpoint's trap is written back.
func (b *queueBackend) WriteMemory(addr uint64, src []byte) error {
	err := b.fakeBackend.WriteMemory(addr, src)
	if err == nil && addr == b.watchAddr && bytes.Equal(src, debugger.ExportedTrapInstruction()) {
		b.note("reinstall")
	}
	return err
}

func (b *queueBackend) Wait() (debugger.StopEvent, error) {
	for {
		if ev, ok := b.q.StepExitBoundary(); ok {
			return ev, nil
		}
		if ev, ok := b.q.Releasable(); ok {
			b.note("release")
			return ev, nil
		}

		raw, ok := <-b.raw
		if !ok {
			return debugger.StopEvent{}, debugger.ErrProcessExited
		}
		if raw.death {
			b.q.InterruptStepIfStepped(raw.tid)
			continue
		}

		reason, park := b.q.Classify(raw.trap, raw.tid)
		ev := debugger.StopEvent{Reason: reason, TID: raw.tid, PC: raw.pc}
		if reason == debugger.StopSingleStep {
			b.q.EndStep()
		}
		if park {
			b.q.Park(ev)
			continue
		}
		return ev, nil
	}
}

func (b *queueBackend) push(r rawStop) { b.raw <- r }

// noUserEventWithin reports whether the client saw no user-visible event for
// the whole window. Goroutine snapshots are not user-visible stops and are
// skipped, matching nextEvent.
func noUserEventWithin(d debugger.Debugger, window time.Duration) bool {
	deadline := time.After(window)
	for {
		select {
		case evt, ok := <-d.Events():
			if !ok {
				return true
			}
			if evt.Kind == protocol.EventGoroutineSnapshot {
				continue
			}
			return false
		case <-deadline:
			return true
		}
	}
}

var _ = Describe("Engine over the production park queue", func() {
	const (
		bpAddrA    = uint64(0x4100)
		bpAddrB    = uint64(0x4200)
		steppedTID = 1
		siblingTID = 2
	)

	var (
		fb   *fakeBackend
		qb   *queueBackend
		d    debugger.Debugger
		trap []byte
	)

	BeforeEach(func() {
		trap = debugger.ExportedTrapInstruction()
		fb = newFakeBackend()
		fb.tids = []int{steppedTID, siblingTID}
		fb.seedMem(bpAddrA, make([]byte, len(trap)))
		fb.seedMem(bpAddrB, make([]byte, len(trap)))
		fb.regs[steppedTID] = debugger.Registers{}
		fb.regs[siblingTID] = debugger.Registers{}

		qb = newQueueBackend(fb)
		qb.watchAddr = bpAddrA
		gated := &debugger.ExportedGateBackend{
			Backend:  qb,
			Complete: qb.q.CompleteStepThreadExit,
		}
		d = debugger.NewWithBackend(gated, nil)
		debugger.ExportedForceSuspended(d)
	})

	AfterEach(func() {
		close(qb.raw)
		drainEvents(d)
		_ = d.Kill()
	})

	// This is the exact live-process sequence raised against this design: the
	// stepped thread dies while a sibling is already held at a DIFFERENT real
	// breakpoint, with no process exit and no dead-TID access anywhere. The
	// claim was that the death lifts the gate, the sibling is delivered, and
	// the engine's one-slot steppingOverBP is then overwritten by the next
	// resume — permanently losing a breakpoint that is also absent from memory.
	//
	// It cannot happen, and this pins why: the death does not lift the gate.
	// interruptStepIfStepped closes step ownership but leaves stepExitPending
	// set, releasable refuses to drain while it is, and only the engine's
	// acknowledgement after a successful reinstall clears it.
	It("reinstalls the dead step owner's breakpoint before releasing a sibling held at another breakpoint", func() {
		idA := debugger.ExportedSetBreakpointAt(d, bpAddrA)
		idB := debugger.ExportedSetBreakpointAt(d, bpAddrB)
		Expect(idA).NotTo(Equal(idB))

		continueAndConsumeContinued(d)
		qb.push(rawStop{tid: steppedTID, trap: true, pc: bpAddrA})
		hitA := mustNextEvent(d)
		Expect(hitA.Kind).To(Equal(protocol.EventBreakpointHit))
		var firstHit protocol.BreakpointHitPayload
		Expect(protocol.DecodeEventPayload(hitA, &firstHit)).To(Succeed())
		Expect(firstHit.Breakpoint.ID).To(Equal(idA))

		// Resuming starts the step-over: A's trap is lifted out of the tracee
		// and its entry leaves the table, owned only by steppingOverBP.
		continueAndConsumeContinued(d)
		Eventually(func() []byte { return fb.peekMem(bpAddrA, len(trap)) }).
			ShouldNot(Equal(trap), "the step-over transaction must own A's lifted trap")

		// The sibling traps at B while that step is in flight. The production
		// classifier must hold it: acting on it now is issue #199.
		qb.push(rawStop{tid: siblingTID, trap: true, pc: bpAddrB})
		Eventually(qb.q.ParkedDepth).Should(Equal(1),
			"a foreign breakpoint during a step must be held, not surfaced")
		Expect(noUserEventWithin(d, 150*time.Millisecond)).To(BeTrue(),
			"no user-visible event may reach the client while the step is in flight")

		// The stepped thread now dies. Its completion will never arrive.
		// Only the reinstall/release pair that follows the death is the
		// ordering under test; the initial arming of A is setup.
		qb.resetOrder()
		qb.push(rawStop{tid: steppedTID, death: true})

		// A must be armed again before B's held stop is released.
		Eventually(func() []byte { return fb.peekMem(bpAddrA, len(trap)) }).
			Should(Equal(trap), "the death boundary must reinstall A")

		hitB := mustNextEvent(d)
		Expect(hitB.Kind).To(Equal(protocol.EventBreakpointHit))
		var secondHit protocol.BreakpointHitPayload
		Expect(protocol.DecodeEventPayload(hitB, &secondHit)).To(Succeed())
		Expect(secondHit.Breakpoint.ID).To(Equal(idB),
			"the released sibling must resolve to its own breakpoint")
		Expect(qb.events()).To(Equal([]string{"reinstall", "release"}),
			"A's trap must be written back BEFORE the held sibling is released")

		// Both logical breakpoints survive the transaction: A is armed in the
		// tracee and still clearable by the id the user was given, which is the
		// user-visible symptom issue #199 reports.
		Expect(fb.peekMem(bpAddrA, len(trap))).To(Equal(trap))
		Expect(fb.peekMem(bpAddrB, len(trap))).To(Equal(trap))
		Expect(d.ClearBreakpoint(idA)).To(Succeed(),
			"the dead step owner's breakpoint must still be tracked by id")
		Expect(d.ClearBreakpoint(idB)).To(Succeed())
	})

	// The same death with a sibling held at the SAME address. Before reinstall
	// that address has no trap and no table entry, so delivering it early makes
	// the engine take the spurious-SIGTRAP path and resume — which both loses
	// the breakpoint and silently skips the user's stop.
	It("resolves a sibling held at the dead owner's own address to the reinstalled breakpoint", func() {
		idA := debugger.ExportedSetBreakpointAt(d, bpAddrA)

		continueAndConsumeContinued(d)
		qb.push(rawStop{tid: steppedTID, trap: true, pc: bpAddrA})
		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventBreakpointHit))

		continueAndConsumeContinued(d)
		qb.push(rawStop{tid: siblingTID, trap: true, pc: bpAddrA})
		Eventually(qb.q.ParkedDepth).Should(Equal(1))

		qb.push(rawStop{tid: steppedTID, death: true})

		hit := mustNextEvent(d)
		Expect(hit.Kind).To(Equal(protocol.EventBreakpointHit),
			"a same-address sibling must become a real hit, not a spurious trap")
		var payload protocol.BreakpointHitPayload
		Expect(protocol.DecodeEventPayload(hit, &payload)).To(Succeed())
		Expect(payload.Breakpoint.ID).To(Equal(idA),
			"it must resolve to the original breakpoint, not a new or absent one")
		Expect(fb.peekMem(bpAddrA, len(trap))).To(Equal(trap))
	})
})
