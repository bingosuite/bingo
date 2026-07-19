//go:build e2e && darwin && arm64 && bingonative

package integration

import . "github.com/onsi/ginkgo/v2"

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
//   - pause: Continue -> Pause -> Paused round-trips (async interrupt).
//   - inspect: continue into a breakpoint, then StackFrames/Locals/Goroutines.
//   - restart: hub kill + relaunch + reinstall, reaching the breakpoint again.
//   - fullstack: the whole client -> WebSocket -> hub -> debugger transport.
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
	declareFullStackSpec()
	declareRestartSpec()
})
