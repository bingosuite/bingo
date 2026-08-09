//go:build e2e && linux && amd64

package integration

import (
	"encoding/json"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// Linux/amd64 debugger acceptance suite. Drives the real ptrace backend
// (single tracer thread, PC rewind, clone tracing, stepTID disambiguation).
// Runs on native ubuntu runners; see .github/workflows/debugger-e2e.yml.
var _ = Describe("Linux amd64 debugger backend (ptrace) E2E", Label("linux"), func() {
	declareBasicStepOverSpec()
	declareChurnSpec()
	declarePauseSpec()
	declareStepIntoSpec()
	declareStepOutSpec()
	declareInspectSpec()
	declareTypedLocalsSpec()
	declareClearBreakpointSpec()
	declareKillRunningSpec()
	declareExitCodeSpec()
	declareAttachSpec()
	declareConcurrencySpec()
	declareFullStackSpec()
	declareRestartSpec()
	declareDAPSpec()
	declareDAPEvaluateSpec()
	declareDAPExitSpec()
	declareDAPMultiClientSpec()
	declareDAPJoinSpec()
	declareStepOverlapSpec()
})

// overlapTargetSrc pounds two breakpoint-able lines from several
// LockOSThread'd goroutines at once, so a sibling thread's SIGTRAP reliably
// surfaces from Wait4(-1, …, WALL) while another thread is single-stepping off
// one of those very traps. Every spinner owns its own M and runs the SAME hot
// loop, which produces both #199 variants:
//
//   - a DISTINCT sibling (a thread trapping on OVERLAP_B while another steps
//     off OVERLAP_A), and
//   - a SAME-ADDRESS sibling (a thread trapping on OVERLAP_A in the window
//     where that trap has been restored to its original bytes for the step).
//
// OVERLAP_A contains a call so a source-level StepOver over it really has to
// step off the armed trap; OVERLAP_B is a plain statement. The per-iteration
// sleep keeps the trap rate high but bounded — a thread that has trapped stays
// ptrace-stopped until the debugger resumes it, so the number of stops in
// flight can never exceed the spinner count.
const overlapTargetSrc = `package main

import (
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

var sink int64

func work(n int64) int64 {
	s := int64(0)
	for i := int64(0); i < n; i++ {
		s += i
	}
	return s
}

func spin(id int64) {
	runtime.LockOSThread()
	x := id
	for {
		x += work(x % 5) // OVERLAP_A
		x++              // OVERLAP_B
		atomic.AddInt64(&sink, x)
		time.Sleep(300 * time.Microsecond)
	}
}

func main() {
	// Safety net: self-exit if the debugger abandons us while running.
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	runtime.GOMAXPROCS(4)
	for i := int64(0); i < 6; i++ {
		go spin(i)
	}
	for {
		time.Sleep(10 * time.Millisecond)
	}
}
`

// declareStepOverlapSpec is the permanent regression gate for issue #199: a
// foreign thread's breakpoint stop surfacing while another thread single-steps
// off a software breakpoint.
//
// Linux-only by construction. It is the ptrace Wait4(-1, …, WALL) semantics
// that let an arbitrary thread's stop arrive mid-step; the darwin backend's
// receive loop already parks a straggler and only lets the stepping thread's
// trap complete a step, so the engine's deferral queue stays empty there and
// the spec would be vacuous.
//
// What it asserts is deliberately NOT the per-cycle outcome. Whether a given
// StepOver completes as Stepped or as a sibling's BreakpointHit — and how many
// siblings trap in any window — is inherently racy, and the trap really is
// temporarily absent from the tracee's memory during a step. Pinning hit counts
// would make this flaky without testing the fix. Instead it pins the invariants
// the deferral guarantees:
//
//  1. Both logical breakpoints survive every stop. Re-setting either line must
//     keep reporting "already installed": under the bug the stepped-over entry
//     was dropped from byID/byAddr with its trap disarmed, so a re-set would
//     succeed and the original id became unclearable.
//  2. Every reported BreakpointHit belongs to one of the two ids we set — a
//     lost/overwritten entry surfaces as an unknown id.
//  3. No EventError and no unexpected process exit across the whole run.
//  4. Both ids are still clearable at the end (the direct user-visible symptom
//     in #199).
//
// Non-vacuity is asserted separately: hits must be observed on BOTH lines and
// from more than one goroutine, otherwise the run never created the overlap the
// spec exists to cover.
func declareStepOverlapSpec() {
	It("keeps both breakpoints armed when a sibling thread traps mid-step", Label("overlap"), func() {
		lineA := markerLine(overlapTargetSrc, "// OVERLAP_A")
		lineB := markerLine(overlapTargetSrc, "// OVERLAP_B")
		bin := buildTarget("overlap_target", overlapTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(20*time.Second, protocol.EventStepped) // initial launch stop

		bpA, err := h.d.SetBreakpoint("overlap_target.go", lineA)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A")
		bpB, err := h.d.SetBreakpoint("overlap_target.go", lineB)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B")
		known := map[int]bool{bpA.ID: true, bpB.ID: true}

		hits := map[int]int{}
		goroutines := map[int]bool{}
		stepOverDeliveredForeign := 0

		// assertBothStillArmed is the live probe for invariant 1. Re-setting an
		// installed address is rejected without touching tracee memory, so it
		// is a safe, side-effect-free way to ask the engine whether it still
		// tracks each breakpoint at this stop.
		assertBothStillArmed := func(where string) {
			GinkgoHelper()
			for _, ln := range []int{lineA, lineB} {
				_, setErr := h.d.SetBreakpoint("overlap_target.go", ln)
				Expect(setErr).To(HaveOccurred(),
					"%s: breakpoint at line %d vanished from the table", where, ln)
				Expect(setErr.Error()).To(ContainSubstring("already installed"),
					"%s: unexpected error re-setting line %d", where, ln)
			}
		}

		// record asserts the shared per-stop invariants and tallies coverage.
		record := func(evt protocol.Event, where string) {
			GinkgoHelper()
			if evt.Kind != protocol.EventBreakpointHit {
				return
			}
			var hit protocol.BreakpointHitPayload
			Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode BreakpointHit")
			Expect(known).To(HaveKey(hit.Breakpoint.ID),
				"%s: hit reports breakpoint id %d, which is neither %d nor %d "+
					"(an entry was overwritten or lost)", where, hit.Breakpoint.ID, bpA.ID, bpB.ID)
			hits[hit.Breakpoint.ID]++
			goroutines[hit.Goroutine.ID] = true
		}

		iters := envInt("BINGO_E2E_OVERLAP_ITERS", 150)
		for i := 0; i < iters; i++ {
			Expect(h.d.Continue()).To(Succeed(), "Continue #%d", i)
			evt := h.waitFor(30*time.Second,
				protocol.EventBreakpointHit, protocol.EventStepped,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Or(Equal(protocol.EventBreakpointHit), Equal(protocol.EventStepped)),
				"Continue #%d unexpected %s: %s", i, evt.Kind, evt.Payload)
			record(evt, fmt.Sprintf("after Continue #%d", i))
			assertBothStillArmed(fmt.Sprintf("after Continue #%d", i))

			// StepOver single-steps off whichever trap we are parked on; this
			// is the window in which a sibling stop must be deferred.
			Expect(h.d.StepOver()).To(Succeed(), "StepOver #%d", i)
			evt = h.waitFor(30*time.Second,
				protocol.EventStepped, protocol.EventBreakpointHit,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Or(Equal(protocol.EventStepped), Equal(protocol.EventBreakpointHit)),
				"StepOver #%d unexpected %s: %s", i, evt.Kind, evt.Payload)
			if evt.Kind == protocol.EventBreakpointHit {
				stepOverDeliveredForeign++
			}
			record(evt, fmt.Sprintf("after StepOver #%d", i))
			assertBothStillArmed(fmt.Sprintf("after StepOver #%d", i))
		}

		// Invariant 4: the user-visible symptom in #199 was that the original
		// id could no longer be cleared.
		Expect(h.d.ClearBreakpoint(bpA.ID)).To(Succeed(), "ClearBreakpoint A after the run")
		Expect(h.d.ClearBreakpoint(bpB.ID)).To(Succeed(), "ClearBreakpoint B after the run")

		// Non-vacuity: without hits on both lines from several threads the run
		// never produced the concurrent stops this spec exists to cover.
		Expect(hits[bpA.ID]).To(BeNumerically(">", 0), "no hits on OVERLAP_A")
		Expect(hits[bpB.ID]).To(BeNumerically(">", 0), "no hits on OVERLAP_B")
		Expect(len(goroutines)).To(BeNumerically(">=", 2),
			"all hits came from one goroutine — no cross-thread overlap was exercised")

		AddReportEntry("overlap-iterations", iters)
		AddReportEntry("overlap-hits-A", hits[bpA.ID])
		AddReportEntry("overlap-hits-B", hits[bpB.ID])
		AddReportEntry("overlap-goroutines", len(goroutines))
		AddReportEntry("overlap-stepover-resolved-as-breakpoint", stepOverDeliveredForeign)
	})
}
