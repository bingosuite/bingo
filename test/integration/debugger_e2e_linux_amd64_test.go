//go:build e2e && linux && amd64

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
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
	declareStepOverlapStepIntoSpec()
	declareStepOverlapSignalSpec()
	declareStepOverlapPauseSpec()
	declareStepOverlapKillSpec()
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
// trap complete a step, so the linux park queue has no darwin counterpart to
// exercise and the spec would be vacuous there.
//
// What it asserts is deliberately NOT the per-cycle outcome. Whether a given
// StepOver completes as Stepped or as a sibling's BreakpointHit — and how many
// siblings trap in any window — is inherently racy, and the trap really is
// temporarily absent from the tracee's memory during a step. Pinning hit counts
// would make this flaky without testing the fix. Instead it pins the invariants
// the park queue guarantees:
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

		// Sized against the target's own 180s watchdog, not against wall-clock
		// preference: a cycle costs ~1.05s under -race on a fast hosted runner
		// and ~1.26s on a slow one (runners vary by ~20%; see run 31330091994,
		// where every spec was that much slower). 150 cycles fits the fast case
		// at ~158s and blows the watchdog at ~190s on the slow one, which would
		// surface as a spurious ProcessExited rather than a real regression. 90
		// keeps ~37% headroom at the slow rate while still resolving dozens of
		// step-overs as a parked sibling's breakpoint — the path under test.
		iters := envInt("BINGO_E2E_OVERLAP_ITERS", 90)
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
			// is the window in which a sibling stop must be held back.
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

		// The decisive one: assert the parking path actually ran. Everything
		// above stays green if foreign stops are never held back at all, so
		// without this the spec could pass on a run that never reproduced the
		// overlap — or against a build with the rule removed.
		parked, ok := debugger.LinuxParkedStopCount(h.d)
		Expect(ok).To(BeTrue(), "parked-stop hook unavailable")
		Expect(parked).To(BeNumerically(">", 0),
			"no foreign stop was ever parked across %d cycles: this run never "+
				"exercised the rule under test", iters)

		AddReportEntry("overlap-iterations", iters)
		AddReportEntry("overlap-hits-A", hits[bpA.ID])
		AddReportEntry("overlap-hits-B", hits[bpB.ID])
		AddReportEntry("overlap-goroutines", len(goroutines))
		AddReportEntry("overlap-stepover-resolved-as-breakpoint", stepOverDeliveredForeign)
		AddReportEntry("overlap-parked-stops", parked)
	})
}

// overlapSignalTargetSrc adds a process-directed signal storm to the overlap
// workload. A process-directed signal is delivered to an arbitrary thread that
// is not blocking it, so it regularly lands on a spinner in the exact window
// where another spinner is single-stepping off a trap — the foreign StopSignal
// case, as opposed to the foreign SIGTRAP the plain overlap target produces.
//
// SIGUSR1 is used because ptrace intercepts it before delivery and the engine
// resumes with signal 0, so the tracee never observes it: the storm changes the
// debugger's workload without changing the target's behaviour.
const overlapSignalTargetSrc = `package main

import (
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
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
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	runtime.GOMAXPROCS(4)
	for i := int64(0); i < 6; i++ {
		go spin(i)
	}
	pid := os.Getpid()
	go func() {
		for {
			_ = syscall.Kill(pid, syscall.SIGUSR1)
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for {
		time.Sleep(10 * time.Millisecond)
	}
}
`

// overlapProbe bundles the per-stop bookkeeping the overlap specs share: the
// liveness probe that both logical breakpoints are still tracked, and the tally
// that proves the run was not vacuous.
type overlapProbe struct {
	h          *e2eHarness
	file       string
	lines      []int
	known      map[int]bool
	hits       map[int]int
	goroutines map[int]bool
	// lateGoroutines counts distinct goroutines still hitting breakpoints in
	// the final stretch of a run. A thread that was resumed incorrectly stays
	// ptrace-stopped forever, so continued breadth here is the observable proof
	// that no thread was stranded.
	lateGoroutines map[int]bool
	signalOutputs  int
}

func newOverlapProbe(h *e2eHarness, file string, ids []int, lines []int) *overlapProbe {
	known := map[int]bool{}
	for _, id := range ids {
		known[id] = true
	}
	return &overlapProbe{
		h: h, file: file, lines: lines, known: known,
		hits: map[int]int{}, goroutines: map[int]bool{},
		lateGoroutines: map[int]bool{},
	}
}

// assertArmed re-sets each breakpoint line and requires the engine to reject it
// as already installed. Re-setting an installed address never touches tracee
// memory, so this is a side-effect-free way to ask whether the entry survived
// the last stop. Under #199 the stepped-over entry was dropped from byID/byAddr
// with its trap disarmed, and this probe would succeed instead of failing.
func (p *overlapProbe) assertArmed(where string) {
	GinkgoHelper()
	for _, ln := range p.lines {
		_, err := p.h.d.SetBreakpoint(p.file, ln)
		Expect(err).To(HaveOccurred(), "%s: breakpoint at line %d vanished from the table", where, ln)
		Expect(err.Error()).To(ContainSubstring("already installed"),
			"%s: unexpected error re-setting line %d", where, ln)
	}
}

func (p *overlapProbe) record(evt protocol.Event, where string, late bool) {
	GinkgoHelper()
	if evt.Kind != protocol.EventBreakpointHit {
		return
	}
	var hit protocol.BreakpointHitPayload
	Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode BreakpointHit")
	Expect(p.known).To(HaveKey(hit.Breakpoint.ID),
		"%s: hit reports unknown breakpoint id %d (an entry was overwritten or lost)", where, hit.Breakpoint.ID)
	p.hits[hit.Breakpoint.ID]++
	p.goroutines[hit.Goroutine.ID] = true
	if late {
		p.lateGoroutines[hit.Goroutine.ID] = true
	}
}

// await drains until one of kinds arrives, tallying the engine's signal reports
// so a spec can prove the foreign-signal path was actually exercised.
func (p *overlapProbe) await(timeout time.Duration, kinds ...protocol.EventKind) protocol.Event {
	GinkgoHelper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-p.h.d.Events():
			if !ok {
				Fail(fmt.Sprintf("events channel closed while waiting for %v", kinds))
			}
			if evt.Kind == protocol.EventOutput {
				var out protocol.OutputPayload
				if json.Unmarshal(evt.Payload, &out) == nil && strings.HasPrefix(out.Content, "signal ") {
					p.signalOutputs++
				}
			}
			for _, k := range kinds {
				if evt.Kind == k {
					return evt
				}
			}
		case <-deadline:
			Fail(fmt.Sprintf("TIMEOUT after %s waiting for %v (possible hang)", timeout, kinds))
		}
	}
}

// setupOverlap launches src, sets breakpoints on both markers and returns a
// probe over them, already parked at the launch stop.
func setupOverlap(name, src string) (*overlapProbe, int, int) {
	GinkgoHelper()
	lineA := markerLine(src, "// OVERLAP_A")
	lineB := markerLine(src, "// OVERLAP_B")
	bin := buildTarget(name, src)

	h := newE2EHarness(bin)
	h.waitFor(20*time.Second, protocol.EventStepped)

	bpA, err := h.d.SetBreakpoint(name+".go", lineA)
	Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A")
	bpB, err := h.d.SetBreakpoint(name+".go", lineB)
	Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B")

	return newOverlapProbe(h, name+".go", []int{bpA.ID, bpB.ID}, []int{lineA, lineB}), bpA.ID, bpB.ID
}

// declareStepOverlapStepIntoSpec covers the machine-granularity step
// (bpResumeStep) rather than the source-level step-over: StepInto off an armed
// trap issues the same restore → single-step → reinstall dance, but suspends on
// completion instead of resuming, so any sibling stop held back during the step
// stays held until the NEXT resume. That is the path where a premature release
// would surface a breakpoint hit the user never continued into.
func declareStepOverlapStepIntoSpec() {
	It("holds a sibling trap across a machine step and never doubles the step completion",
		Label("overlap"), func() {
			p, _, _ := setupOverlap("overlap_stepinto_target", overlapTargetSrc)

			iters := envInt("BINGO_E2E_OVERLAP_STEPINTO_ITERS", 100)
			for i := 0; i < iters; i++ {
				where := fmt.Sprintf("cycle #%d", i)

				Expect(p.h.d.Continue()).To(Succeed(), "Continue %s", where)
				evt := p.await(30*time.Second,
					protocol.EventBreakpointHit, protocol.EventStepped,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Or(Equal(protocol.EventBreakpointHit), Equal(protocol.EventStepped)),
					"Continue %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, i >= iters*3/4)
				p.assertArmed(where)

				// StepInto off the armed trap. Exactly one stop must answer it:
				// a released sibling arriving as a second stop here would mean
				// the queue drained before the step completed.
				Expect(p.h.d.StepInto()).To(Succeed(), "StepInto %s", where)
				evt = p.await(30*time.Second,
					protocol.EventStepped, protocol.EventBreakpointHit,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Or(Equal(protocol.EventStepped), Equal(protocol.EventBreakpointHit)),
					"StepInto %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, i >= iters*3/4)
				p.assertArmed(where)
			}

			Expect(len(p.goroutines)).To(BeNumerically(">=", 2),
				"all hits came from one goroutine — no cross-thread overlap was exercised")
			AddReportEntry("overlap-stepinto-iterations", iters)
			AddReportEntry("overlap-stepinto-goroutines", len(p.goroutines))
			if parked, ok := debugger.LinuxParkedStopCount(p.h.d); ok {
				// Reported, not asserted: a plain machine step is a much
				// narrower window than the step-over spec's, so a run with no
				// park here is plausible rather than a signal that the rule
				// stopped working. The two specs that gate on it are enough.
				AddReportEntry("overlap-stepinto-parked-stops", parked)
			}
		})
}

// declareStepOverlapSignalSpec drives the foreign StopSignal case: an ordinary
// (non-trap) signal landing on a thread that is not the one being stepped.
//
// The invariant under test is that the thread the engine resumes is the thread
// that actually stopped. A held-back signal must not move the backend's resume
// target while the step is outstanding, and must move it when it is finally
// delivered. Getting that wrong resumes a thread that was never stopped and
// strands the one that was — which is why the assertion is breadth of progress
// in the final quarter of the run, not a per-stop equality.
func declareStepOverlapSignalSpec() {
	It("resumes the thread that stopped when a foreign signal lands mid-step",
		Label("overlap"), func() {
			p, bpA, bpB := setupOverlap("overlap_signal_target", overlapSignalTargetSrc)

			iters := envInt("BINGO_E2E_OVERLAP_SIGNAL_ITERS", 80)
			for i := 0; i < iters; i++ {
				where := fmt.Sprintf("cycle #%d", i)
				late := i >= iters*3/4

				Expect(p.h.d.Continue()).To(Succeed(), "Continue %s", where)
				evt := p.await(30*time.Second,
					protocol.EventBreakpointHit, protocol.EventStepped,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Or(Equal(protocol.EventBreakpointHit), Equal(protocol.EventStepped)),
					"Continue %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, late)
				p.assertArmed(where)

				Expect(p.h.d.StepOver()).To(Succeed(), "StepOver %s", where)
				evt = p.await(30*time.Second,
					protocol.EventStepped, protocol.EventBreakpointHit,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Or(Equal(protocol.EventStepped), Equal(protocol.EventBreakpointHit)),
					"StepOver %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, late)
				p.assertArmed(where)
			}

			Expect(p.h.d.ClearBreakpoint(bpA)).To(Succeed(), "ClearBreakpoint A after the run")
			Expect(p.h.d.ClearBreakpoint(bpB)).To(Succeed(), "ClearBreakpoint B after the run")

			// Non-vacuity: the signal storm must have reached the debugger,
			// otherwise this ran as a plain overlap spec.
			Expect(p.signalOutputs).To(BeNumerically(">", 0),
				"no signal stops were reported — the foreign-signal path was never exercised")
			// And the parking rule itself must have run. Asserted here as well
			// as in the step-over spec because this is the variant that carries
			// foreign non-suspending signal stops through the queue, not just
			// breakpoint stops.
			parked, ok := debugger.LinuxParkedStopCount(p.h.d)
			Expect(ok).To(BeTrue(), "parked-stop hook unavailable")
			Expect(parked).To(BeNumerically(">", 0),
				"no foreign stop was ever parked across %d cycles: this run never "+
					"exercised the rule under test", iters)
			// Liveness: threads are still making progress at the end of the
			// run. A thread resumed while another was left stopped would drop
			// out permanently, since every spinner is LockOSThread'd.
			Expect(len(p.lateGoroutines)).To(BeNumerically(">=", 2),
				"fewer than two goroutines still hitting breakpoints in the final quarter — a thread was stranded")

			AddReportEntry("overlap-signal-iterations", iters)
			AddReportEntry("overlap-signal-stops", p.signalOutputs)
			AddReportEntry("overlap-signal-goroutines", len(p.goroutines))
			AddReportEntry("overlap-signal-late-goroutines", len(p.lateGoroutines))
			AddReportEntry("overlap-signal-parked-stops", parked)
		})
}

// declareStepOverlapPauseSpec races Pause against an in-flight step. Pause is a
// process-directed-by-way-of-main-thread SIGSTOP, so when the stepped thread is
// a spinner the interrupt arrives on a foreign thread and is held back until the
// step completes. The session must still end up suspended and resumable — a
// dropped or prematurely released interrupt shows up here as a hang.
func declareStepOverlapPauseSpec() {
	It("stays resumable when a Pause interrupt races an in-flight step",
		Label("overlap"), func() {
			p, _, _ := setupOverlap("overlap_pause_target", overlapTargetSrc)

			iters := envInt("BINGO_E2E_OVERLAP_PAUSE_ITERS", 40)
			paused := 0
			for i := 0; i < iters; i++ {
				where := fmt.Sprintf("cycle #%d", i)

				Expect(p.h.d.Continue()).To(Succeed(), "Continue %s", where)
				evt := p.await(30*time.Second,
					protocol.EventBreakpointHit, protocol.EventStepped,
					protocol.EventPaused, protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).NotTo(Or(Equal(protocol.EventProcessExited), Equal(protocol.EventError)),
					"Continue %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, false)
				p.assertArmed(where)

				Expect(p.h.d.StepOver()).To(Succeed(), "StepOver %s", where)
				// Pause immediately: the step may already have completed and
				// suspended, in which case Pause is legitimately rejected.
				if err := p.h.d.Pause(); err != nil {
					Expect(err).To(MatchError(debugger.ErrNotRunning),
						"Pause %s failed for an unexpected reason", where)
				} else {
					paused++
				}

				evt = p.await(30*time.Second,
					protocol.EventStepped, protocol.EventBreakpointHit, protocol.EventPaused,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).NotTo(Or(Equal(protocol.EventProcessExited), Equal(protocol.EventError)),
					"StepOver+Pause %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, false)
				p.assertArmed(where)
			}

			// Still fully resumable after all that racing.
			Expect(p.h.d.Continue()).To(Succeed(), "final Continue")
			evt := p.await(30*time.Second,
				protocol.EventBreakpointHit, protocol.EventStepped, protocol.EventPaused,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).NotTo(Or(Equal(protocol.EventProcessExited), Equal(protocol.EventError)),
				"final Continue unexpected %s: %s", evt.Kind, evt.Payload)

			AddReportEntry("overlap-pause-iterations", iters)
			AddReportEntry("overlap-pause-accepted", paused)
		})
}

// declareStepOverlapKillSpec tears the session down at the worst moment: right
// after a step was armed, while sibling stops are most likely being held. Kill
// must return promptly and the held stops must be discarded rather than acted
// on — every parked thread is dead by then, so any post-exit work against one
// would be a ptrace call into a dead TID.
func declareStepOverlapKillSpec() {
	It("tears down cleanly when killed with sibling stops held back",
		Label("overlap"), func() {
			p, _, _ := setupOverlap("overlap_kill_target", overlapTargetSrc)

			// Warm up so several threads are cycling through both traps and the
			// queue is genuinely in play by the time we kill.
			warmup := envInt("BINGO_E2E_OVERLAP_KILL_WARMUP", 15)
			for i := 0; i < warmup; i++ {
				where := fmt.Sprintf("warmup #%d", i)
				Expect(p.h.d.Continue()).To(Succeed(), "Continue %s", where)
				evt := p.await(30*time.Second,
					protocol.EventBreakpointHit, protocol.EventStepped,
					protocol.EventProcessExited, protocol.EventError)
				p.record(evt, where, false)
				Expect(p.h.d.StepOver()).To(Succeed(), "StepOver %s", where)
				evt = p.await(30*time.Second,
					protocol.EventStepped, protocol.EventBreakpointHit,
					protocol.EventProcessExited, protocol.EventError)
				p.record(evt, where, false)
			}

			// Arm one more step and kill without waiting for it to land.
			Expect(p.h.d.StepOver()).To(Succeed(), "StepOver before kill")

			done := make(chan error, 1)
			go func() { done <- p.h.d.Kill() }()
			select {
			case err := <-done:
				Expect(err).NotTo(HaveOccurred(), "Kill with held stops")
			case <-time.After(15 * time.Second):
				Fail("Kill did not return within 15s — teardown wedged with stops held back")
			}

			// The engine closes its event stream once teardown completes.
			deadline := time.After(15 * time.Second)
			for closed := false; !closed; {
				select {
				case _, ok := <-p.h.d.Events():
					if !ok {
						closed = true
					}
				case <-deadline:
					Fail("event stream still open 15s after Kill — engine did not shut down")
				}
			}

			Expect(len(p.goroutines)).To(BeNumerically(">=", 2),
				"all hits came from one goroutine — no cross-thread overlap was exercised before the kill")
			AddReportEntry("overlap-kill-warmup", warmup)
			AddReportEntry("overlap-kill-goroutines", len(p.goroutines))
		})
}
