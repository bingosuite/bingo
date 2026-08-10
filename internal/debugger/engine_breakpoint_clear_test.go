package debugger_test

import (
	"errors"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

var _ = Describe("Breakpoint clear state transitions", func() {
	const bpAddr = uint64(0x2800)

	var (
		fb       *fakeBackend
		d        debugger.Debugger
		id       int
		trap     []byte
		original []byte
	)

	waitForContinue := func() {
		select {
		case <-fb.continueCh:
		case <-time.After(eventTimeout):
			Fail("backend ContinueProcess was not called")
		}
	}

	expectOriginalInstruction := func() {
		ExpectWithOffset(1, fb.peekMem(bpAddr, len(trap))).To(Equal(original[:len(trap)]))
	}

	expectAddressReserved := func(addr uint64) {
		readsBefore := len(fb.readCalls)
		writesBefore := len(fb.writeCalls)
		err := debugger.ExportedSetBreakpointAtErr(d, addr)
		ExpectWithOffset(1, errors.Is(err, debugger.ExportedErrBreakpointExists)).To(BeTrue())
		ExpectWithOffset(1, fb.readCalls).To(HaveLen(readsBefore),
			"reserved SetBreakpoint must reject before backend ReadMemory")
		ExpectWithOffset(1, fb.writeCalls).To(HaveLen(writesBefore),
			"reserved SetBreakpoint must reject before backend WriteMemory")
	}

	parkOnBreakpoint := func(tid int) {
		hitPC := bpAddr
		if len(trap) == 1 {
			hitPC++
		}
		fb.tids = []int{1}
		if tid != 1 {
			fb.tids = append(fb.tids, tid)
		}
		fb.regs[tid] = debugger.Registers{PC: hitPC}

		continueAndConsumeContinued(d)
		waitForContinue()
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: tid, PC: bpAddr})

		evt := mustNextEvent(d)
		ExpectWithOffset(1, evt.Kind).To(Equal(protocol.EventBreakpointHit))
		var payload protocol.BreakpointHitPayload
		ExpectWithOffset(1, protocol.DecodeEventPayload(evt, &payload)).To(Succeed())
		ExpectWithOffset(1, payload.Breakpoint.ID).To(Equal(id))
		ExpectWithOffset(1, fb.regs[tid].PC).To(Equal(bpAddr),
			"the live PC must remain rewound when a parked breakpoint is cleared")
	}

	BeforeEach(func() {
		fb = newFakeBackend()
		d = debugger.NewWithBackend(fb, nil)
		trap = debugger.ExportedTrapInstruction()
		original = []byte{0x48, 0x89, 0xc0, 0x90}
		fb.seedMem(bpAddr, original)
		debugger.ExportedForceSuspended(d)
		id = debugger.ExportedSetBreakpointAt(d, bpAddr)
	})

	AfterEach(func() {
		_ = d.Kill()
		if !fb.stopped {
			close(fb.stopCh)
			fb.stopped = true
		}
	})

	It("clears the breakpoint currently parked on without scheduling a step-off", func() {
		parkOnBreakpoint(1)

		Expect(d.ClearBreakpoint(id)).To(Succeed())
		expectOriginalInstruction()
		stepsBeforeResume := len(fb.singleStepCalls)

		continueAndConsumeContinued(d)
		waitForContinue()
		Expect(fb.singleStepCalls).To(HaveLen(stepsBeforeResume))
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
	})

	It("cancels an in-flight reinstall and still performs the Continue action", func() {
		parkOnBreakpoint(1)

		continueAndConsumeContinued(d)
		Expect(fb.singleStepCalls).To(HaveLen(1))
		expectOriginalInstruction()

		Expect(d.ClearBreakpoint(id)).To(Succeed())
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
		expectAddressReserved(bpAddr)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + uint64(len(trap))})
		waitForContinue()

		expectOriginalInstruction()
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
		Expect(debugger.ExportedSetBreakpointAt(d, bpAddr)).To(Equal(id+1),
			"Continue completion must release the cancelled address reservation")
	})

	It("cancels an in-flight reinstall and still performs the StepInto action", func() {
		parkOnBreakpoint(1)

		Expect(d.StepInto()).To(Succeed())
		Expect(fb.singleStepCalls).To(HaveLen(1))
		Expect(d.ClearBreakpoint(id)).To(Succeed())
		expectAddressReserved(bpAddr)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + uint64(len(trap))})

		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventStepped))
		expectOriginalInstruction()
		select {
		case <-fb.continueCh:
			Fail("StepInto completion must suspend instead of continuing")
		default:
		}
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
		Expect(debugger.ExportedSetBreakpointAt(d, bpAddr)).To(Equal(id+1),
			"Step completion must release the cancelled address reservation")
	})

	It("preserves the parked entry when restoring its bytes fails", func() {
		parkOnBreakpoint(2)

		fb.writeErr = errors.New("injected restore failure")
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("injected restore failure")))
		Expect(fb.peekMem(bpAddr, len(trap))).To(Equal(trap))

		continueAndConsumeContinued(d)
		Expect(fb.singleStepCalls).To(ConsistOf(2),
			"a failed clear must leave lastBP intact for the normal step-off")
		expectOriginalInstruction()

		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + uint64(len(trap))})
		waitForContinue()
		Expect(fb.peekMem(bpAddr, len(trap))).To(Equal(trap))
		Expect(d.ClearBreakpoint(id)).To(Succeed(),
			"successful reinstall must retain the original breakpoint ID")
		expectOriginalInstruction()
	})

	It("reinstalls an in-flight breakpoint that was not cleared", func() {
		parkOnBreakpoint(1)

		continueAndConsumeContinued(d)
		Expect(fb.singleStepCalls).To(HaveLen(1))
		expectAddressReserved(bpAddr)
		const otherAddr = uint64(0x2900)
		fb.seedMem(otherAddr, original)
		otherID := debugger.ExportedSetBreakpointAt(d, otherAddr)
		Expect(otherID).To(Equal(id+1),
			"a rejected same-address Set must not consume an ID")
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + uint64(len(trap))})
		waitForContinue()

		Expect(fb.peekMem(bpAddr, len(trap))).To(Equal(trap))
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 1, PC: bpAddr})
		evt := mustNextEvent(d)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit))
		var payload protocol.BreakpointHitPayload
		Expect(protocol.DecodeEventPayload(evt, &payload)).To(Succeed())
		Expect(payload.Breakpoint.ID).To(Equal(id),
			"the un-cleared breakpoint must re-arm under its original ID")
		Expect(d.ClearBreakpoint(otherID)).To(Succeed(),
			"the different-address Set must retain independent table ownership")
	})

	It("does not reinstall a cleared in-flight breakpoint when stop-PC inspection fails", func() {
		parkOnBreakpoint(1)

		Expect(d.StepInto()).To(Succeed())
		Expect(d.ClearBreakpoint(id)).To(Succeed())
		expectAddressReserved(bpAddr)
		fb.getRegistersErr = errors.New("injected register failure")
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1})

		evt := mustNextEvent(d)
		Expect(evt.Kind).To(Equal(protocol.EventError))
		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventPaused),
			"an asynchronous stop-handling failure must re-enter the suspended state")
		expectOriginalInstruction()
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
		Expect(debugger.ExportedSetBreakpointAt(d, bpAddr)).To(Equal(id+1),
			"failed stop inspection must release step-off ownership")

		stepsBeforeRetry := len(fb.singleStepCalls)
		continueAndConsumeContinued(d)
		waitForContinue()
		Expect(fb.singleStepCalls).To(HaveLen(stepsBeforeRetry),
			"the rejected completion must leave a direct resume path, not resurrect the breakpoint")
	})

	It("does not reinstall a cleared in-flight breakpoint when a signal interrupts the step", func() {
		parkOnBreakpoint(1)

		continueAndConsumeContinued(d)
		Expect(d.ClearBreakpoint(id)).To(Succeed())
		expectAddressReserved(bpAddr)
		fb.pushStop(debugger.StopEvent{
			Reason: debugger.StopSignal,
			TID:    1,
			Signal: int(syscall.SIGUSR1),
		})
		waitForContinue()

		expectOriginalInstruction()
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
		Expect(debugger.ExportedSetBreakpointAt(d, bpAddr)).To(Equal(id+1),
			"signal completion must release step-off ownership")
	})

	It("releases the reservation when starting the single-step is rejected", func() {
		parkOnBreakpoint(1)

		fb.singleStepErr = errors.New("injected single-step failure")
		Expect(d.Continue()).To(MatchError(ContainSubstring("injected single-step failure")))
		expectAddressReserved(bpAddr)
		Expect(d.ClearBreakpoint(id)).To(Succeed(),
			"resume rollback must restore the original table owner")
		Expect(debugger.ExportedSetBreakpointAt(d, bpAddr)).To(Equal(id+1),
			"removing the rolled-back owner must expose no stale reservation")
	})

	It("releases the reservation when restoring the original instruction is rejected", func() {
		parkOnBreakpoint(1)

		fb.writeErr = errors.New("injected resume restore failure")
		Expect(d.Continue()).To(MatchError(ContainSubstring("injected resume restore failure")))
		expectAddressReserved(bpAddr)
		Expect(d.ClearBreakpoint(id)).To(Succeed(),
			"restore rollback must keep the original table owner retryable")
		Expect(debugger.ExportedSetBreakpointAt(d, bpAddr)).To(Equal(id+1),
			"removing the rolled-back owner must expose no stale reservation")
	})

	It("releases the reservation after a reinstall error ends step-off ownership", func() {
		parkOnBreakpoint(1)

		continueAndConsumeContinued(d)
		expectAddressReserved(bpAddr)
		fb.writeErr = errors.New("injected reinstall failure")
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + uint64(len(trap))})

		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventError))
		Expect(debugger.ExportedSetBreakpointAt(d, bpAddr)).To(Equal(id+1),
			"reinstall failure must not leave stale step-off ownership")
	})

	It("does not resurrect a cleared breakpoint in the stepped-thread death backstop", func() {
		const (
			siblingAddr = bpAddr + 0x100
			siblingTID  = 2
		)
		fb.seedMem(siblingAddr, original)
		siblingID := debugger.ExportedSetBreakpointAt(d, siblingAddr)
		parkOnBreakpoint(1)

		continueAndConsumeContinued(d)
		Expect(d.ClearBreakpoint(id)).To(Succeed())

		hitPC := siblingAddr
		if len(trap) == 1 {
			hitPC++
		}
		fb.tids = []int{1, siblingTID}
		fb.regs[siblingTID] = debugger.Registers{PC: hitPC}
		fb.pushStop(debugger.StopEvent{
			Reason: debugger.StopBreakpoint,
			TID:    siblingTID,
			PC:     siblingAddr,
		})

		evt := mustNextEvent(d)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit))
		var payload protocol.BreakpointHitPayload
		Expect(protocol.DecodeEventPayload(evt, &payload)).To(Succeed())
		Expect(payload.Breakpoint.ID).To(Equal(siblingID))
		expectOriginalInstruction()
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
	})

	It("rewinds a queued same-address hit after the in-flight breakpoint is cleared", func() {
		const siblingTID = 2
		parkOnBreakpoint(1)

		continueAndConsumeContinued(d)
		Expect(d.ClearBreakpoint(id)).To(Succeed())

		hitPC := bpAddr
		if len(trap) == 1 {
			hitPC++
		}
		fb.tids = []int{1, siblingTID}
		fb.regs[siblingTID] = debugger.Registers{PC: hitPC}
		fb.pushStop(debugger.StopEvent{
			Reason: debugger.StopBreakpoint,
			TID:    siblingTID,
			PC:     bpAddr,
		})

		waitForContinue()
		Expect(fb.regs[siblingTID].PC).To(Equal(bpAddr),
			"the delayed hit must execute the restored instruction rather than resume past it")
		expectOriginalInstruction()
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
		_, ok := nextEvent(d)
		Expect(ok).To(BeFalse(), "a delayed hit on a cleared breakpoint must remain silent")
	})

	It("makes Kill's clearAll path forget a parked breakpoint", func() {
		parkOnBreakpoint(1)

		debugger.ExportedClearAllBreakpoints(d)
		expectOriginalInstruction()
		stepsBeforeResume := len(fb.singleStepCalls)

		continueAndConsumeContinued(d)
		waitForContinue()
		Expect(fb.singleStepCalls).To(HaveLen(stepsBeforeResume))
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
	})

	It("makes Kill's clearAll path cancel an in-flight reinstall", func() {
		parkOnBreakpoint(1)

		Expect(d.StepInto()).To(Succeed())
		debugger.ExportedClearAllBreakpoints(d)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + uint64(len(trap))})

		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventStepped))
		expectOriginalInstruction()
		Expect(d.ClearBreakpoint(id)).To(MatchError(ContainSubstring("not found")))
	})
})
