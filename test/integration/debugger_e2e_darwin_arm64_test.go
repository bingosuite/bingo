//go:build e2e && darwin && arm64 && bingonative

package integration

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// Darwin/arm64 debugger acceptance suite. Drives the real pure-Mach backend
// (task-level EXC_MASK_BREAKPOINT exception port + mach_msg receive loop,
// posix_spawn START_SUSPENDED launch, per-thread hardware single-step, Mach
// stop-the-world), with Go async preemption ENABLED in the tracee — the #92
// rearchitecture that replaced the wait4/ptrace model. Needs the bingonative tag
// and a debugger-entitled (codesigned) test binary; see
// .github/workflows/debugger-e2e.yml and the justfile e2e-darwin recipe.
//
// The Mach exception model gives per-thread signal fidelity: a thread-directed
// SIGURG is delivered natively to the exact M the runtime targeted (breakpoints
// are masked as Mach exceptions; BSD signals are left native), so the old
// wait4-era misdirection that forced asyncpreemptoff — and made single-stepping
// off an armed breakpoint flaky — is gone by construction. Stop-the-world drains
// any exception queued before a sibling thread was suspended, closing the
// concurrent-fault race. Kill resumes every thread before SIGKILL and reaps via
// wait4, so kill-while-running no longer deadlocks. That is why the whole suite
// now runs on darwin, matching linux (minus linux-only backend mechanics):
//   - basic: Continue into a breakpoint then repeated StepOver.
//   - stepping: StepInto crosses into a callee; StepOut returns to the caller.
//   - breakpoints: a cleared breakpoint stops firing.
//   - churn: hundreds of step-overs under continuous thread creation.
//   - kill: Kill terminates a freely-running tracee.
//   - exit: EventProcessExited reports the tracee's real exit code.
//   - attach: attach by PID to an already-running tracee, then breakpoint it.
//   - pause: Continue -> Pause -> Paused round-trips (async interrupt).
//   - inspect: continue into a breakpoint, then StackFrames/Locals/Goroutines.
//   - restart: hub kill + relaunch + reinstall, reaching the breakpoint again.
//   - fullstack: the whole client -> WebSocket -> hub -> debugger transport.
//   - dap: a real Debug Adapter Protocol client drives a session over TCP, and
//     N WebSocket observers share it (DAP + WebSocket coexisting on one session).
//
// See AGENTS.md -> Test layering and Backend quirks -> Darwin / arm64.
var _ = Describe("Darwin arm64 debugger backend (Mach exceptions) E2E", Label("darwin"), func() {
	declareBasicStepOverSpec()
	declareChurnSpec()
	declarePauseSpec()
	declareStepIntoSpec()
	declareStepOutSpec()
	declareInspectSpec()
	declareClearBreakpointSpec()
	declareKillRunningSpec()
	declareExitCodeSpec()
	declareAttachSpec()
	declareFullStackSpec()
	declareRestartSpec()
	declareDAPSpec()
	declareDAPExitSpec()
	declareDAPMultiClientSpec()
	declarePortHygieneSpec()
})

// declarePortHygieneSpec is the regression gate for the Mach exception-message
// port-right leak (issue: exception path leaked task/thread send rights). Every
// exception_raise message the kernel delivers carries a send right to BOTH the
// faulting thread and the task; Mach coalesces each onto our existing port name
// and increments that name's user-reference count. The task name is a single
// cached port, so a per-stop leak grows one name's uref without bound until it
// hits KERN_UREFS_OVERFLOW — after which the kernel can no longer copy out the
// exception message and Wait wedges (a hard hang reachable in any long session or
// hot-loop breakpoint).
//
// The spec drives dozens of breakpoint stops and asserts the tracee task port's
// send-ref count stays bounded. Before the fix the count grew ~linearly with the
// number of stops (each stop's resume single-steps off the armed trap and then
// re-hits it — two exceptions, two leaked task rights); after the fix each
// exception's task right is released as it is received, so the count is flat.
// Darwin-only: it reads a darwin backend introspection hook.
func declarePortHygieneSpec() {
	It("does not leak task send rights across many breakpoint stops", Label("hygiene"), func() {
		line := markerLine(basicTargetSrc, "// BP")
		bin := buildTarget("hygiene_target", basicTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped) // initial launch stop

		_, err := h.d.SetBreakpoint("hygiene_target.go", line)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint")

		// Reach the breakpoint once so the task port is acquired and the send-ref
		// count is at steady state before the baseline reading.
		Expect(h.d.Continue()).To(Succeed(), "initial Continue")
		evt := h.waitFor(15*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit), "initial stop: %s", evt.Payload)

		base, ok := debugger.DarwinTaskPortSendRefs(h.d)
		Expect(ok).To(BeTrue(), "task port should be acquired after the first stop")

		iters := envInt("BINGO_E2E_HYGIENE_ITERS", 40)
		for i := 0; i < iters; i++ {
			Expect(h.d.Continue()).To(Succeed(), "Continue #%d", i)
			evt := h.waitFor(15*time.Second,
				protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"Continue #%d expected BreakpointHit, got %s: %s", i, evt.Kind, evt.Payload)
		}

		after, ok := debugger.DarwinTaskPortSendRefs(h.d)
		Expect(ok).To(BeTrue())
		// Post-fix the count is flat; allow a tiny constant slack for any
		// transient. Pre-fix it grew by ~2 per stop (dozens over the loop), so a
		// small bound is a wide fail-before / pass-after margin.
		Expect(after-base).To(BeNumerically("<=", 3),
			"task port send-ref leak: base=%d after=%d over %d stops (grows ~linearly when the exception path leaks)",
			base, after, iters)
		AddReportEntry("hygiene-iterations", iters)
		AddReportEntry("hygiene-task-send-refs", map[string]int{"base": base, "after": after})
	})
}
