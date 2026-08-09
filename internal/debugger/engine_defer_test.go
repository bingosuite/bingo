package debugger_test

import (
	"fmt"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// Foreign-thread stop deferral (issue #199).
//
// On linux Wait4(-1, …, WALL) reports any thread's stop, so a sibling
// breakpoint can surface while another thread is single-stepping off a
// disarmed software breakpoint. Handling it there destroys the step-over state
// machine: the stepped-over entry is out of the table with its trap removed, so
// a distinct sibling breakpoint overwrites lastBP/steppingOverBP and loses it
// permanently, while a same-address sibling finds no entry, takes the
// spurious-SIGTRAP path and calls ContinueProcess — clearing the backend's step
// bookkeeping.
//
// These specs drive the engine's stop machine directly through the fake
// backend, so they are deterministic and platform-independent.
// opsFrom returns the backend calls recorded after index n.
func opsFrom(all []string, n int) []string {
	if n > len(all) {
		return nil
	}
	return all[n:]
}

var _ = Describe("Engine foreign-thread stop deferral", func() {
	const (
		addrA = uint64(0x4000)
		addrB = uint64(0x5000)
		tidA  = 1
		tidB  = 2
	)

	var (
		fb   *fakeBackend
		d    debugger.Debugger
		trap []byte
	)

	BeforeEach(func() {
		trap = debugger.ExportedTrapInstruction()
		fb = newFakeBackend()
		fb.tids = []int{tidA, tidB}
		fb.regs[tidA] = debugger.Registers{PC: addrA}
		fb.regs[tidB] = debugger.Registers{PC: addrB}
		d = debugger.NewWithBackend(fb, nil)
		debugger.ExportedForceSuspended(d)
	})

	AfterEach(func() {
		_ = d.Kill()
		if !fb.stopped {
			close(fb.stopCh)
			fb.stopped = true
		}
	})

	// parkAtBreakpointA installs a breakpoint at addrA (and optionally addrB),
	// runs the tracee, and leaves it suspended at addrA on tidA so the next
	// Continue goes through the step-over sequence.
	parkAtBreakpointA := func(extraAddrs ...uint64) (idA int, extraIDs []int) {
		idA = debugger.ExportedSetBreakpointAt(d, addrA)
		for _, a := range extraAddrs {
			extraIDs = append(extraIDs, debugger.ExportedSetBreakpointAt(d, a))
		}
		continueAndConsumeContinued(d)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: tidA, PC: addrA})
		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventBreakpointHit))
		return idA, extraIDs
	}

	// startStepOver resumes from the breakpoint at addrA, which disarms the
	// trap and arms an exact-TID single step on tidA.
	startStepOver := func() {
		Expect(d.Continue()).To(Succeed())
		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventContinued))
		st := debugger.ExportedDeferral(d)
		Expect(st.StepTID).To(Equal(tidA), "a single step must be armed on the breakpoint thread")
		Expect(fb.peekMem(addrA, len(trap))).NotTo(Equal(trap), "the trap must be disarmed for the step")
	}

	Describe("a distinct sibling breakpoint arriving mid-step", func() {
		It("is withheld until the stepped breakpoint is reinstalled, and loses neither entry", func() {
			idA, extra := parkAtBreakpointA(addrB)
			idB := extra[0]
			startStepOver()

			resumesBefore := fb.continueCount()
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: tidB, PC: addrB})

			// Nothing user-visible may happen while the step is in flight.
			_, ok := nextEvent(d)
			Expect(ok).To(BeFalse(), "a foreign breakpoint must not be reported during a single step")

			st := debugger.ExportedDeferral(d)
			Expect(st.Deferred).To(Equal(1))
			Expect(st.StepTID).To(Equal(tidA), "the step must still be in flight")
			Expect(fb.continueCount()).To(Equal(resumesBefore),
				"the tracee must not be resumed while a step is in flight")

			// The step completes: the trap goes back before anything else.
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: tidA, PC: addrA + 1})

			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit))
			var hit protocol.BreakpointHitPayload
			Expect(protocol.DecodeEventPayload(evt, &hit)).To(Succeed())
			Expect(hit.Breakpoint.ID).To(Equal(idB), "the deferred stop must resolve to the sibling breakpoint")

			// ExportedDeferral doubles as a loop barrier before reading fb.
			st = debugger.ExportedDeferral(d)
			Expect(st.Deferred).To(BeZero())
			Expect(st.StepTID).To(BeZero())
			Expect(st.ParkedTID).To(Equal(tidA), "the stepped thread stays parked until the next resume")

			Expect(fb.peekMem(addrA, len(trap))).To(Equal(trap),
				"the stepped-over trap must be back in memory before the deferred stop is handled")
			Expect(debugger.ExportedBreakpointArmedAt(d, addrA)).To(BeTrue())
			Expect(debugger.ExportedBreakpointArmedAt(d, addrB)).To(BeTrue())
			Expect(debugger.ExportedBreakpointIDs(d)).To(ConsistOf(idA, idB))

			// Both logical breakpoints survive as user-clearable entries.
			Expect(d.ClearBreakpoint(idA)).To(Succeed())
			Expect(d.ClearBreakpoint(idB)).To(Succeed())
		})

		It("releases the parked stepped thread on the next resume, then steps off the new breakpoint", func() {
			parkAtBreakpointA(addrB)
			startStepOver()
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: tidB, PC: addrB})
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: tidA, PC: addrA + 1})
			Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventBreakpointHit))

			stepsBefore := len(fb.singleStepCalls)
			resumesBefore := fb.continueCount()
			Expect(d.Continue()).To(Succeed())
			Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventContinued))

			st := debugger.ExportedDeferral(d)
			Expect(st.ParkedTID).To(BeZero(), "the owed resume must be discharged")
			Expect(st.StepTID).To(Equal(tidB), "the new breakpoint's own step-over runs on its thread")
			Expect(fb.continueCount()).To(Equal(resumesBefore+1),
				"exactly one continue releases the parked thread")
			Expect(fb.singleStepCalls[stepsBefore:]).To(Equal([]int{tidB}))
		})
	})

	Describe("a same-address sibling arriving mid-step", func() {
		It("is not mistaken for a spurious trap and becomes a real hit after reinstall", func() {
			idA, _ := parkAtBreakpointA()
			startStepOver()

			resumesBefore := fb.continueCount()
			regsBefore := fb.regs[tidB]
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: tidB, PC: addrA})

			_, ok := nextEvent(d)
			Expect(ok).To(BeFalse())

			st := debugger.ExportedDeferral(d)
			Expect(st.Deferred).To(Equal(1))
			Expect(fb.continueCount()).To(Equal(resumesBefore),
				"the spurious-trap path must not resume the tracee mid-step")
			Expect(fb.regs[tidB]).To(Equal(regsBefore),
				"the spurious-trap path must not rewrite the sibling's PC")

			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: tidA, PC: addrA + 1})

			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit))
			var hit protocol.BreakpointHitPayload
			Expect(protocol.DecodeEventPayload(evt, &hit)).To(Succeed())
			Expect(hit.Breakpoint.ID).To(Equal(idA),
				"once reinstalled, the same-address stop resolves to the original breakpoint")

			st = debugger.ExportedDeferral(d)
			Expect(st.Deferred).To(BeZero())
			Expect(fb.peekMem(addrA, len(trap))).To(Equal(trap))
			Expect(debugger.ExportedBreakpointArmedAt(d, addrA)).To(BeTrue())
		})
	})

	Describe("a foreign signal arriving mid-step", func() {
		It("does not reinstall early and emits no phantom step completion", func() {
			parkAtBreakpointA()
			startStepOver()

			fb.pushStop(debugger.StopEvent{
				Reason: debugger.StopSignal,
				TID:    tidB,
				Signal: int(syscall.SIGCHLD),
			})

			_, ok := nextEvent(d)
			Expect(ok).To(BeFalse(), "a foreign signal must produce nothing while a step is in flight")

			st := debugger.ExportedDeferral(d)
			Expect(st.Deferred).To(Equal(1))
			Expect(st.StepTID).To(Equal(tidA))
			Expect(fb.peekMem(addrA, len(trap))).NotTo(Equal(trap),
				"the trap must not be reinstalled by a foreign signal")

			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: tidA, PC: addrA + 1})

			// The deferred signal is delivered after the reinstall, as output.
			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventOutput))

			st = debugger.ExportedDeferral(d)
			Expect(st.Deferred).To(BeZero())
			Expect(debugger.ExportedBreakpointArmedAt(d, addrA)).To(BeTrue())
			Expect(fb.peekMem(addrA, len(trap))).To(Equal(trap))
		})
	})

	// Releasing the parked thread spends the TID-less continue, so the thread
	// the deferred stop belongs to can only be resumed with SingleStep. If that
	// thread is sitting ON an armed trap the step EXECUTES it, and the backend
	// reports the resulting SIGTRAP as the step's own completion (it keys on
	// stepping && tid == stepTID and cannot tell the two apart) — which would
	// swallow the hit and resume the thread from mid-instruction. The resume
	// must therefore step the thread OVER the trap instead of through it.
	Describe("resuming a deferred thread that is parked on an armed trap", func() {
		It("steps it over the trap rather than through it, and never swallows the hit", func() {
			idA, extra := parkAtBreakpointA(addrB)
			idB := extra[0]
			startStepOver()

			// tidB takes an ordinary signal while sitting exactly on addrB's
			// trap. This is the auto-resume path, which does not go through
			// resumeFromBreakpoint.
			fb.pushStop(debugger.StopEvent{
				Reason: debugger.StopSignal, TID: tidB, PC: addrB, Signal: int(syscall.SIGCHLD),
			})
			Eventually(func() int { return debugger.ExportedDeferral(d).Deferred }).Should(Equal(1))

			opsBefore := len(fb.ops())
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: tidA, PC: addrA + 1})
			Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventOutput))

			st := debugger.ExportedDeferral(d)
			Expect(st.InternalStepTID).To(BeZero(),
				"a thread on an armed trap must not be resumed by the raw internal step")
			Expect(st.StepTID).To(Equal(tidB), "it must be stepped over its own breakpoint")
			Expect(fb.peekMem(addrB, len(trap))).NotTo(Equal(trap),
				"the trap under the resumed thread must be disarmed for the step")
			Expect(fb.peekMem(addrA, len(trap))).To(Equal(trap),
				"the stepped-over breakpoint stays armed throughout")

			// Ordering is load-bearing at two points: the stepped-over trap goes
			// back before anything else runs, and the disarming write for the
			// resumed thread targets the backend's current stop thread — still
			// the parked one. Releasing first would aim that write at a live
			// thread (ESRCH on linux).
			Expect(opsFrom(fb.ops(), opsBefore)).To(Equal([]string{
				fmt.Sprintf("write:0x%x", addrA),
				fmt.Sprintf("write:0x%x", addrB),
				"continue",
				fmt.Sprintf("step:%d", tidB),
			}), "reinstall, then disarm, then release the parked thread, then step")

			resumesBefore := fb.continueCount()
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: tidB, PC: addrB + 1})

			_, ok := nextEvent(d)
			Expect(ok).To(BeFalse(), "an internal resume must stay invisible to clients")

			st = debugger.ExportedDeferral(d)
			Expect(st.StepTID).To(BeZero())
			Expect(st.InternalStepTID).To(BeZero())
			Expect(fb.continueCount()).To(Equal(resumesBefore+1),
				"the thread is continued only after its trap is back")
			Expect(fb.peekMem(addrB, len(trap))).To(Equal(trap),
				"the trap must be reinstalled before the thread runs on")
			Expect(debugger.ExportedBreakpointArmedAt(d, addrA)).To(BeTrue())
			Expect(debugger.ExportedBreakpointArmedAt(d, addrB)).To(BeTrue())
			Expect(debugger.ExportedBreakpointIDs(d)).To(ConsistOf(idA, idB))
		})
	})

	Describe("a foreign breakpoint arriving during a plain StepInto", func() {
		It("still completes exactly one step, then reports the sibling on the next resume", func() {
			idB := debugger.ExportedSetBreakpointAt(d, addrB)

			Expect(d.StepInto()).To(Succeed())
			st := debugger.ExportedDeferral(d)
			Expect(st.StepTID).To(Equal(tidA),
				"a plain StepInto arms an exact-TID step and must guard it too")

			fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: tidB, PC: addrB})
			_, ok := nextEvent(d)
			Expect(ok).To(BeFalse())
			Expect(debugger.ExportedDeferral(d).Deferred).To(Equal(1))

			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: tidA, PC: addrA + 4})
			Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventStepped))

			// Exactly one step completion: nothing else is emitted until a resume.
			_, ok = nextEvent(d)
			Expect(ok).To(BeFalse(), "the deferred breakpoint must not double up on the step completion")

			resumesBefore := fb.continueCount()
			Expect(d.Continue()).To(Succeed())
			Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventContinued))

			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit))
			var hit protocol.BreakpointHitPayload
			Expect(protocol.DecodeEventPayload(evt, &hit)).To(Succeed())
			Expect(hit.Breakpoint.ID).To(Equal(idB))

			Expect(fb.continueCount()).To(Equal(resumesBefore),
				"a resume with stops still parked must deliver them instead of running the world on")
			Expect(debugger.ExportedDeferral(d).ParkedTID).To(Equal(tidA))
		})
	})

	Describe("a Pause interrupt surfacing on a foreign thread mid-step", func() {
		It("is withheld until the step completes, then still reports EventPaused", func() {
			parkAtBreakpointA()
			startStepOver()

			Expect(d.Pause()).To(Succeed())
			fb.pushStop(debugger.StopEvent{
				Reason: debugger.StopSignal, TID: tidB, PC: addrB, Signal: fb.PauseSignal(),
			})

			_, ok := nextEvent(d)
			Expect(ok).To(BeFalse(), "a Pause must not be reported while a step is in flight")
			Expect(debugger.ExportedDeferral(d).Deferred).To(Equal(1))
			Expect(fb.peekMem(addrA, len(trap))).NotTo(Equal(trap),
				"a deferred Pause must not trigger an early reinstall")

			fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: tidA, PC: addrA + 1})

			evt := mustNextEvent(d)
			Expect(evt.Kind).To(Equal(protocol.EventPaused),
				"the pending Pause must still be honoured after the step completes")
			Expect(fb.peekMem(addrA, len(trap))).To(Equal(trap))
			Expect(debugger.ExportedBreakpointArmedAt(d, addrA)).To(BeTrue())

			// The stepped thread is still parked; the resume that ends the
			// pause has to discharge it rather than strand it.
			st := debugger.ExportedDeferral(d)
			Expect(st.Deferred).To(BeZero())
			Expect(st.ParkedTID).To(Equal(tidA))

			resumesBefore := fb.continueCount()
			Expect(d.Continue()).To(Succeed())
			Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventContinued))
			Eventually(fb.continueCount).Should(BeNumerically(">", resumesBefore))
			Expect(debugger.ExportedDeferral(d).ParkedTID).To(BeZero())
		})
	})

	Describe("teardown with stops still parked", func() {
		It("purges the queue on Kill and never touches the parked threads again", func() {
			parkAtBreakpointA(addrB)
			startStepOver()
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: tidB, PC: addrB})
			Eventually(func() int { return debugger.ExportedDeferral(d).Deferred }).Should(Equal(1))

			Expect(d.Kill()).To(Succeed())

			var kinds []protocol.EventKind
			for {
				evt, ok := nextEvent(d)
				if !ok {
					break
				}
				kinds = append(kinds, evt.Kind)
			}
			Expect(kinds).NotTo(ContainElement(protocol.EventBreakpointHit),
				"a parked stop must never be replayed after teardown")

			// The loop is gone: no dispatch can run, so no backend call can be
			// issued at a dead TID.
			Eventually(d.Events()).Should(BeClosed())
			Expect(d.Continue()).To(MatchError(debugger.ErrProcessExited))
		})

		It("purges the queue when the process exits underneath a step", func() {
			parkAtBreakpointA(addrB)
			startStepOver()
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: tidB, PC: addrB})
			Eventually(func() int { return debugger.ExportedDeferral(d).Deferred }).Should(Equal(1))

			fb.pushStop(debugger.StopEvent{Reason: debugger.StopExited, TID: tidA, ExitCode: 0})

			var kinds []protocol.EventKind
			for {
				evt, ok := nextEvent(d)
				if !ok {
					break
				}
				kinds = append(kinds, evt.Kind)
			}
			Expect(kinds).To(ContainElement(protocol.EventProcessExited))
			Expect(kinds).NotTo(ContainElement(protocol.EventBreakpointHit))
		})
	})
})
