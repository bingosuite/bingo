//go:build e2e && ((linux && amd64) || (darwin && arm64 && bingonative))

// Real end-to-end acceptance specs that drive the ACTUAL native debugger
// backend (ptrace on linux/amd64, ptrace+Mach on darwin/arm64) against a
// freshly-built target process. Unlike the unit tests in internal/debugger
// (which swap in the in-process fakeBackend) these exercise the real low-level
// path and are the acceptance gate for the backend fixes.
//
// These require a real OS kernel — they cannot run under emulation or with
// fakes — so they are gated behind the `e2e` build tag and kept out of the
// default `go test ./...`. Darwin additionally needs the `bingonative` tag and
// a debugger-entitled (codesigned) test binary.
//
// This file holds the cross-platform pieces: the harness, the embedded target
// sources, and the reusable spec bodies. The platform files
// (debugger_e2e_{linux_amd64,darwin_arm64}_test.go) wire them into a
// per-OS Ginkgo container so build tags select exactly one at compile time.
//
// Tuning env vars:
//
//	BINGO_E2E_ITERS        (default 25)   basic continue+stepover iterations
//	BINGO_E2E_CHURN_ITERS  (default 200)  iterations under thread churn
//	BINGO_E2E_DEBUG        (unset)        route engine debug logs + every event to stderr

package integration

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// e2eDebug routes engine debug logs and a per-event trace to stderr — invaluable
// when diagnosing a hang or a wrong-thread step.
var e2eDebug = os.Getenv("BINGO_E2E_DEBUG") != ""

func init() {
	if e2eDebug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}
}

// basicTargetSrc is a quiet, deterministic target: a hot loop with a clearly
// identifiable breakpoint line that calls a function (so step-over must step
// OVER the call) and a distinct following line (so we can assert progress).
// It intentionally prints nothing to avoid stdout back-pressure on the tracee.
const basicTargetSrc = `package main

import (
	"os"
	"time"
)

func compute(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += i
	}
	return s
}

func main() {
	// Safety net: self-exit if the debugger abandons us while running.
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	x := 0
	for i := 0; i < 1000000; i++ {
		x += compute(i % 10) // BP
		x++
		time.Sleep(time.Millisecond)
		_ = x
	}
}
`

// churnTargetSrc forces continuous OS-thread creation/teardown (LockOSThread +
// short sleeps across GOMAXPROCS worker goroutines) so that breakpoint stops
// and single-steps happen in a genuinely multi-threaded context. This is the
// scenario that reproduces the darwin single-step race and stresses linux
// clone/thread-exit handling.
const churnTargetSrc = `package main

import (
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

var sink int64

func churn() {
	for {
		runtime.LockOSThread()
		var x int64
		for i := 0; i < 2000; i++ {
			x += int64(i)
		}
		atomic.AddInt64(&sink, x)
		runtime.UnlockOSThread()
		time.Sleep(50 * time.Microsecond)
	}
}

func work(n int) int64 {
	var s int64
	for i := 0; i < n; i++ {
		s += int64(i)
	}
	return s
}

func main() {
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	runtime.GOMAXPROCS(4)
	for i := 0; i < 8; i++ {
		go churn()
	}
	x := int64(0)
	for i := 0; i < 1000000; i++ {
		x += work(i % 50) // BP
		x++
		time.Sleep(time.Millisecond)
		_ = x
	}
}
`

// callTargetSrc is a small, quiet program with a known call chain
// (main -> outer -> inner) and named locals in each frame, built with -N -l so
// every local is stack-allocated (DWARF-readable) and every call is a real
// call. It drives the stepping (StepInto/StepOut) and inspect
// (StackFrames/Locals/Goroutines) specs. Markers: BPINNER sits on a line inside
// inner (so a stop there has inner->outer->main on the stack with locals q/b);
// CALLINNER sits on the call to inner (so a StepInto there crosses into it).
const callTargetSrc = `package main

import (
	"os"
	"time"
)

func inner(b int) int {
	q := b * 2 // BPINNER
	return q + 1
}

func outer(a int) int {
	p := a + 100
	r := inner(p) // CALLINNER
	return r
}

func main() {
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	x := 0
	for i := 0; i < 1000000; i++ {
		x += outer(i % 5)
		time.Sleep(time.Millisecond)
		_ = x
	}
}
`

// twoBPTargetSrc has two distinct breakpoint-able lines in its hot loop (BP_A
// then BP_B). The ClearBreakpoint spec sets both, then clears A while stopped at
// B and asserts subsequent Continues only ever stop at B — proving the cleared
// breakpoint really stops firing. Neither marked line contains a call, so the
// spec exercises only Continue (never the darwin-fragile step-over path).
const twoBPTargetSrc = `package main

import (
	"os"
	"time"
)

func main() {
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	x := 0
	for i := 0; i < 1000000; i++ {
		x += i // BP_A
		x *= 3 // BP_B
		time.Sleep(time.Millisecond)
		_ = x
	}
}
`

// declareBasicStepOverSpec adds the continue+step-over acceptance spec to the
// enclosing Ginkgo container. It is the correctness gate: set a breakpoint on a
// line that calls a function, repeatedly Continue to it and StepOver the call,
// asserting the tracee reaches the breakpoint every time and the step advances
// past the BP line (never hangs, errors, or exits early). Runs on both linux and
// darwin: each StepOver single-steps *off* the armed breakpoint, a path made
// reliable on darwin by the Mach-exception model (see the scoping note above).
func declareBasicStepOverSpec() {
	It("continues to a breakpoint and steps over a call, repeatedly", Label("basic"), func() {
		line := markerLine(basicTargetSrc, "// BP")
		bin := buildTarget("basic_target", basicTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped) // initial launch stop

		bp, err := h.d.SetBreakpoint("basic_target.go", line)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint")
		Expect(bp.Location.Line).To(Equal(line), "breakpoint resolved to the requested line")

		iters := envInt("BINGO_E2E_ITERS", 25)
		assertContinueStepOver(h.d, line, iters)
		AddReportEntry("basic-iterations", iters)
	})
}

// declareChurnSpec adds the multi-thread robustness spec. Under continuous
// thread churn a StepOver may legitimately surface as either Stepped or a
// different thread's BreakpointHit; what must NOT happen is a hang (waitFor
// timeout), an error, or an unexpected process exit.
//
// Runs on both linux and darwin. It hammers StepOver hundreds of times while the
// target spawns threads continuously — the heaviest step-off-an-armed-trap
// stress in the suite. On darwin this was the worst of the old wait4-model
// flakes; the Mach-exception model (per-thread signal fidelity + target-side
// I-cache flush on breakpoint writes) makes it deterministic (see the scoping
// note above).
func declareChurnSpec() {
	It("survives continue+step-over under continuous thread churn", Label("churn"), func() {
		line := markerLine(churnTargetSrc, "// BP")
		bin := buildTarget("churn_target", churnTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(20*time.Second, protocol.EventStepped)

		_, err := h.d.SetBreakpoint("churn_target.go", line)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint")

		iters := envInt("BINGO_E2E_CHURN_ITERS", 200)
		for i := 0; i < iters; i++ {
			Expect(h.d.Continue()).To(Succeed(), "Continue #%d", i)
			evt := h.waitFor(20*time.Second,
				protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"Continue #%d expected BreakpointHit, got %s: %s", i, evt.Kind, evt.Payload)

			Expect(h.d.StepOver()).To(Succeed(), "StepOver #%d", i)
			evt = h.waitFor(20*time.Second,
				protocol.EventStepped, protocol.EventBreakpointHit,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Or(Equal(protocol.EventStepped), Equal(protocol.EventBreakpointHit)),
				"StepOver #%d unexpected %s: %s", i, evt.Kind, evt.Payload)
		}
		AddReportEntry("churn-iterations", iters)
	})
}

// declarePauseSpec adds the async-interrupt (Pause) acceptance spec. It is the
// authoritative gate for Pause on the real backend: while the tracee runs, fire
// Pause, assert EventPaused surfaces, then resume and repeat. Running the
// Continue→Pause→Paused round-trip several times is the resume-after-pause
// proof — if the first resume hung or corrupted the tracee (e.g. a SIGSTOP that
// couldn't be cleanly discarded), the next Pause's signal would never surface
// and waitFor would time out.
func declarePauseSpec() {
	It("interrupts a running process on demand and resumes, repeatedly", Label("pause"), func() {
		bin := buildTarget("pause_target", basicTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped) // initial launch stop

		iters := envInt("BINGO_E2E_PAUSE_ITERS", 3)
		for i := 0; i < iters; i++ {
			Expect(h.d.Continue()).To(Succeed(), "Continue #%d", i)

			// Give the tracee a moment to actually be running before the async
			// interrupt, so Pause exercises the running→suspended path rather
			// than racing the resume.
			time.Sleep(20 * time.Millisecond)

			Expect(h.d.Pause()).To(Succeed(), "Pause #%d", i)
			evt := h.waitFor(15*time.Second,
				protocol.EventPaused, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventPaused),
				"Pause #%d expected Paused, got %s: %s", i, evt.Kind, evt.Payload)
		}
		AddReportEntry("pause-iterations", iters)
	})
}

// --- new operation specs (stepping / inspect / breakpoints / kill) ---
//
// LINUX and DARWIN both run the full set. The darwin backend used to run only
// specs that resume with a PLAIN continue INTO a trap, because single-stepping
// *off* an armed software breakpoint and killing a freely-running tracee were
// unreliable under the old wait4/ptrace model. The Mach-exception rearchitecture
// (#92) closed those gaps: per-thread exception delivery means a BSD signal can
// no longer divert a single-step into the runtime trampoline undetected, an
// explicit target-side I-cache flush on every breakpoint write makes a
// freshly-installed trap (e.g. <stepover-next>/<stepout-return>) visible the
// instant it is re-executed, and Kill resumes every thread before SIGKILL and
// reaps via wait4 without blocking the engine loop. So the step-off-an-armed-trap
// specs (basic's StepOver, stepping's StepInto/StepOut, breakpoints' continue off
// a parked BP, churn's hundreds of step-overs) and kill (kill-while-running) now
// run on darwin as well as linux. See the per-spec comments, the darwin
// container, and AGENTS.md -> Test layering.

// declareStepIntoSpec asserts StepInto crosses into a called function. It stops
// at the call to inner (CALLINNER) and single-steps (machine-instruction
// granularity) until the reported location is inside main.inner, proving the
// step descended into the callee rather than over it. The step count is bounded
// (a call site is only a couple of instructions from the CALL) so it stays
// deterministic without assuming an exact number of instructions. Runs on both
// linux and darwin: the repeated single-steps are reliable on darwin under the
// Mach-exception model (per-thread signal delivery keeps a mid-step BSD signal
// from diverting the step; see the scoping note above).
func declareStepIntoSpec() {
	It("steps into a called function", Label("stepping"), func() {
		callLine := markerLine(callTargetSrc, "// CALLINNER")
		bin := buildTarget("stepinto_target", callTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped) // initial launch stop

		_, err := h.d.SetBreakpoint("stepinto_target.go", callLine)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint at call site")

		Expect(h.d.Continue()).To(Succeed(), "Continue to call site")
		evt := h.waitFor(15*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit), "stopped at the call site")

		const maxSteps = 20
		reached := false
		for s := 0; s < maxSteps && !reached; s++ {
			Expect(h.d.StepInto()).To(Succeed(), "StepInto #%d", s)
			evt = h.waitFor(15*time.Second,
				protocol.EventStepped, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventStepped), "StepInto #%d emits Stepped", s)
			var st protocol.SteppedPayload
			Expect(json.Unmarshal(evt.Payload, &st)).To(Succeed(), "decode Stepped #%d", s)
			if st.Location.Function == "main.inner" {
				reached = true
			}
		}
		Expect(reached).To(BeTrue(),
			"StepInto reached main.inner within %d instruction steps", maxSteps)
	})
}

// stopInsideInner builds the shared call-chain target (callTargetSrc), launches
// it, sets a breakpoint inside main.inner (the BPINNER marker), and continues
// until the tracee is stopped there. It returns the harness parked inside the
// callee, ready for step-out or state inspection — the common preamble of the
// StepOut and inspect specs.
func stopInsideInner(targetName string) *e2eHarness {
	GinkgoHelper()
	innerLine := markerLine(callTargetSrc, "// BPINNER")
	bin := buildTarget(targetName, callTargetSrc)

	h := newE2EHarness(bin)
	h.waitFor(15*time.Second, protocol.EventStepped) // initial launch stop

	_, err := h.d.SetBreakpoint(targetName+".go", innerLine)
	Expect(err).NotTo(HaveOccurred(), "SetBreakpoint inside callee")

	Expect(h.d.Continue()).To(Succeed(), "Continue into callee")
	evt := h.waitFor(15*time.Second,
		protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
	Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit), "stopped inside main.inner")
	return h
}

// declareStepOutSpec asserts StepOut returns control to the caller. It stops
// inside inner (BPINNER) and StepOut, then asserts the resulting location is
// back in main.outer (the caller) — the return address the callee will unwind
// to. The return address is read from the saved frame pointer (BP+8), the same
// chain walkStack follows; reading *(SP) only works at a function's first
// instruction, before the prologue, and was the old "null return address" bug.
// Runs on both linux and darwin: the step off the armed breakpoint plus the
// <stepout-return> trap are reliable on darwin under the Mach-exception model
// (the target-side I-cache flush makes the freshly-written return trap visible
// the instant the callee returns; see the scoping note above).
func declareStepOutSpec() {
	It("steps out of a callee back to its caller", Label("stepping"), func() {
		h := stopInsideInner("stepout_target")

		Expect(h.d.StepOut()).To(Succeed(), "StepOut of callee")
		evt := h.waitFor(15*time.Second,
			protocol.EventStepped, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventStepped), "StepOut emits Stepped")

		var st protocol.SteppedPayload
		Expect(json.Unmarshal(evt.Payload, &st)).To(Succeed(), "decode Stepped")
		Expect(st.Location.Function).To(Equal("main.outer"),
			"StepOut returned to the caller main.outer, got %q", st.Location.Function)
	})
}

// declareInspectSpec asserts the state-inspection operations (StackFrames,
// Locals, Goroutines) report a coherent snapshot at a breakpoint inside a known
// call chain. It stops inside inner (main.inner <- main.outer <- main.main) and
// checks: the innermost frames name those functions in order; Locals for the
// innermost frame include the callee's declared local `q`; Goroutines returns a
// plausible, non-empty list with a resolved current location. File/line in
// resolved frames come back <autogenerated> under -N -l, so the assertions key
// off the reliable Function name, not file:line.
func declareInspectSpec() {
	It("reports stack frames, locals, and goroutines at a breakpoint", Label("inspect"), func() {
		h := stopInsideInner("inspect_target")

		frames, err := h.d.StackFrames()
		Expect(err).NotTo(HaveOccurred(), "StackFrames")
		Expect(len(frames)).To(BeNumerically(">=", 3),
			"expected at least inner<-outer<-main, got %d frames", len(frames))
		Expect(frames[0].Location.Function).To(Equal("main.inner"), "innermost frame")
		Expect(frames[1].Location.Function).To(Equal("main.outer"), "caller frame")
		Expect(frames[2].Location.Function).To(Equal("main.main"), "outermost user frame")

		locals, err := h.d.Locals(0)
		Expect(err).NotTo(HaveOccurred(), "Locals(0)")
		names := make([]string, 0, len(locals))
		for _, v := range locals {
			names = append(names, v.Name)
		}
		Expect(names).To(ContainElement("q"),
			"innermost frame locals should include the declared local q, got %v", names)

		grs, err := h.d.Goroutines()
		Expect(err).NotTo(HaveOccurred(), "Goroutines")
		Expect(len(grs)).To(BeNumerically(">=", 1), "at least one goroutine")
		Expect(grs[0].CurrentLoc.Function).NotTo(BeEmpty(),
			"goroutine current location should resolve to a function")
	})
}

// declareClearBreakpointSpec asserts a cleared breakpoint stops firing. It sets
// two breakpoints (A before B in the loop body), advances until it is stopped at
// B, clears A (the non-current one — clearing the breakpoint the process is
// currently parked on re-arms it through the step-off/reinstall path), then
// Continues several times and asserts every subsequent stop is at B and never at
// the cleared line A. Runs on both linux and darwin: after clearing A the process
// is parked on B, so each subsequent Continue single-steps *off* B's armed trap
// (the restore->single-step->reinstall dance), a path made reliable on darwin by
// the Mach-exception model (see the scoping note above).
func declareClearBreakpointSpec() {
	It("stops stopping at a cleared breakpoint", Label("breakpoints"), func() {
		lineA := markerLine(twoBPTargetSrc, "// BP_A")
		lineB := markerLine(twoBPTargetSrc, "// BP_B")
		bin := buildTarget("clearbp_target", twoBPTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped) // initial launch stop

		bpA, err := h.d.SetBreakpoint("clearbp_target.go", lineA)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A")
		_, err = h.d.SetBreakpoint("clearbp_target.go", lineB)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B")

		// Advance to A, then to B, so we are parked on B (not A) when we clear A.
		Expect(h.d.Continue()).To(Succeed(), "Continue to A")
		evt := h.waitFor(15*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit), "first stop")
		Expect(bpLine(evt)).To(Equal(lineA), "first stop is at A")

		Expect(h.d.Continue()).To(Succeed(), "Continue to B")
		evt = h.waitFor(15*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit), "second stop")
		Expect(bpLine(evt)).To(Equal(lineB), "second stop is at B")

		Expect(h.d.ClearBreakpoint(bpA.ID)).To(Succeed(), "ClearBreakpoint A")

		// With A cleared, every remaining stop in the loop must be B.
		const rounds = 4
		for i := 0; i < rounds; i++ {
			Expect(h.d.Continue()).To(Succeed(), "Continue #%d after clear", i)
			evt = h.waitFor(15*time.Second,
				protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"post-clear stop #%d is a breakpoint", i)
			Expect(bpLine(evt)).To(Equal(lineB),
				"post-clear stop #%d must be at B (%d), not the cleared A (%d)", i, lineB, lineA)
		}
	})
}

// declareKillRunningSpec asserts Kill terminates a RUNNING tracee, not just a
// suspended one. It Continues the process (so it is genuinely running, past the
// launch stop), then Kills and asserts the engine tears down — proving Kill
// reaps a process that is not sitting at a stop.
//
// Runs on both linux and darwin. Killing a freely-running tracee used to deadlock
// the old darwin backend (the SIGKILL landed the ptraced process in a
// signal-delivery stop that cmd.Wait() couldn't drain from the blocked engine
// loop). Under the Mach-exception model darwin killProcess resumes every thread,
// SIGKILLs, and reaps via wait4 without a cmd.Wait on the engine loop, so
// kill-while-running tears down cleanly. Kill of a *suspended* tracee also works
// and is covered by every spec's cleanup and by the Restart spec.
func declareKillRunningSpec() {
	It("kills a running process", Label("kill"), func() {
		bin := buildTarget("kill_target", basicTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped) // initial launch stop

		Expect(h.d.Continue()).To(Succeed(), "Continue so the tracee is running")
		// Let it actually be running before we kill, so this exercises the
		// running→exited path rather than racing the resume.
		time.Sleep(30 * time.Millisecond)

		Expect(h.d.Kill()).To(Succeed(), "Kill running process")
		// Kill tears the engine down; which signal surfaces depends on which
		// stop wins the race inside the loop. On linux the real wait4 exit
		// typically arrives as ErrProcessExited and the loop emits
		// EventProcessExited before closing; on darwin the synthetic StopExited
		// injected by Kill wins and the loop returns straight to its deferred
		// close(events) with no explicit exit event (see engine.loop's
		// stateExited guard). Both outcomes prove the running tracee was reaped,
		// so accept either. Only a timeout — neither event nor close — is a
		// real failure (a wedged Kill that never reaped the process).
		assertTerminated(h.d.Events(), 15*time.Second)
	})
}

// assertTerminated drains ch until the engine signals the tracee is gone —
// either an explicit EventProcessExited or the events channel closing (the
// engine's clean-teardown path after Kill pre-sets stateExited). Fails on a
// surfaced Error event or on timeout, the latter meaning Kill left the process
// or engine wedged.
func assertTerminated(ch <-chan protocol.Event, timeout time.Duration) {
	GinkgoHelper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return // channel closed: engine torn down, process reaped
			}
			if e2eDebug {
				GinkgoWriter.Printf("event: kind=%v seq=%d payload=%s\n", evt.Kind, evt.Seq, evt.Payload)
			}
			switch evt.Kind {
			case protocol.EventProcessExited:
				return
			case protocol.EventError:
				Fail(fmt.Sprintf("Kill surfaced an error event: %s", evt.Payload))
			}
		case <-deadline:
			Fail(fmt.Sprintf("TIMEOUT after %s: Kill did not terminate the running tracee", timeout))
		}
	}
}

// bpLine decodes a BreakpointHit event and returns its resolved line.
func bpLine(evt protocol.Event) int {
	GinkgoHelper()
	var hit protocol.BreakpointHitPayload
	Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode BreakpointHit")
	return hit.Breakpoint.Location.Line
}

// --- shared acceptance loop ---

// stepDriver is the subset of debugger.Debugger and client.Client that the
// continue+step-over acceptance loop needs. Sharing it lets the in-process
// (basic) spec and the full-stack (over-the-WebSocket) spec assert against one
// implementation — they differ only in whether commands cross the transport.
type stepDriver interface {
	Continue() error
	StepOver() error
	Events() <-chan protocol.Event
}

// assertContinueStepOver runs the core correctness loop `iters` times: Continue
// to the breakpoint on `line`, confirm the hit, StepOver the call on that line
// (the most fragile sequence — restore original byte -> single-step -> reinstall
// trap -> resume), and confirm the step advanced past `line`. Never hangs
// (waitFor fails on timeout), errors, or exits early.
func assertContinueStepOver(d stepDriver, line, iters int) {
	GinkgoHelper()
	for i := 0; i < iters; i++ {
		Expect(d.Continue()).To(Succeed(), "Continue #%d", i)
		evt := awaitEvent(d.Events(), 20*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
			"Continue #%d expected BreakpointHit, got %s: %s", i, evt.Kind, evt.Payload)

		var hit protocol.BreakpointHitPayload
		Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode BreakpointHit #%d", i)
		Expect(hit.Breakpoint.Location.Line).To(Equal(line), "BreakpointHit #%d at BP line", i)

		Expect(d.StepOver()).To(Succeed(), "StepOver #%d", i)
		evt = awaitEvent(d.Events(), 20*time.Second,
			protocol.EventStepped, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventStepped),
			"StepOver #%d expected Stepped, got %s: %s", i, evt.Kind, evt.Payload)

		var st protocol.SteppedPayload
		Expect(json.Unmarshal(evt.Payload, &st)).To(Succeed(), "decode Stepped #%d", i)
		Expect(st.Location.Line).NotTo(Equal(line), "StepOver #%d advanced past the BP line", i)
	}
}

// --- harness ---

type e2eHarness struct {
	d debugger.Debugger
}

// newE2EHarness launches bin under a fresh debugger and registers a bounded
// cleanup. The Kill timeout surfaces the "Kill doesn't reap after a hang"
// failure mode instead of wedging the whole suite.
func newE2EHarness(bin string) *e2eHarness {
	GinkgoHelper()
	d := debugger.New(nil)
	Expect(d.Launch(bin, nil, nil)).To(Succeed(), "Launch target")
	DeferCleanup(func() {
		done := make(chan struct{})
		go func() { _ = d.Kill(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			AddReportEntry("kill-timeout", "Kill did not return within 5s (backend may be wedged)")
		}
	})
	return &e2eHarness{d: d}
}

// waitFor drains the event stream until one of kinds arrives, failing the spec
// on timeout (a hang) or a closed channel.
func (h *e2eHarness) waitFor(timeout time.Duration, kinds ...protocol.EventKind) protocol.Event {
	GinkgoHelper()
	return awaitEvent(h.d.Events(), timeout, kinds...)
}

// awaitEvent drains ch until one of kinds arrives, failing the spec on timeout
// (a hang) or a closed channel. Shared by the direct-debugger harness and the
// full-stack (client-over-WebSocket) harness, which drain different channels.
func awaitEvent(ch <-chan protocol.Event, timeout time.Duration, kinds ...protocol.EventKind) protocol.Event {
	GinkgoHelper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				Fail(fmt.Sprintf("events channel closed while waiting for %v", kinds))
			}
			if e2eDebug {
				GinkgoWriter.Printf("event: kind=%v seq=%d payload=%s\n", evt.Kind, evt.Seq, evt.Payload)
			}
			for _, k := range kinds {
				if evt.Kind == k {
					return evt
				}
			}
			// Ignore unrelated events (Continued, BreakpointSet, Output,
			// SessionState transitions, ...).
		case <-deadline:
			Fail(fmt.Sprintf("TIMEOUT after %s waiting for %v (possible hang)", timeout, kinds))
		}
	}
}

// buildTarget compiles src into a throwaway binary with optimizations and
// inlining disabled (-N -l) so DWARF line info is exact and the BP-line call is
// a real call the step-over must step over.
func buildTarget(name, src string) string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	srcPath := filepath.Join(dir, name+".go")
	Expect(os.WriteFile(srcPath, []byte(src), 0o600)).To(Succeed(), "write target source")

	binPath := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", binPath, srcPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build target %s:\n%s", name, out)
	return binPath
}

// markerLine returns the 1-based line number of the first line containing marker.
func markerLine(src, marker string) int {
	GinkgoHelper()
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, marker) {
			return i + 1
		}
	}
	Fail(fmt.Sprintf("marker %q not found in target source", marker))
	return 0
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
