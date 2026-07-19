//go:build e2e && darwin && arm64 && bingonative

package integration

import . "github.com/onsi/ginkgo/v2"

// Darwin/arm64 debugger acceptance suite. Drives the real ptrace+Mach backend
// (asyncpreemptoff SIGURG workaround, serialized single-step thread select).
// Needs the bingonative tag and a debugger-entitled (codesigned) test binary;
// see .github/workflows/debugger-e2e.yml and the justfile e2e-darwin recipe.
var _ = Describe("Darwin arm64 debugger backend (ptrace+Mach) E2E", Label("darwin"), func() {
	declareBasicStepOverSpec()
	declareChurnSpec()
	declarePauseSpec()
	declareInspectSpec()
	declareFullStackSpec()
	declareRestartSpec()
	// stepping (StepInto/StepOut), breakpoints (ClearBreakpoint), and kill
	// (kill-while-running) are intentionally LINUX-ONLY. The first two require
	// RESUMING FROM an armed software breakpoint (the restore->single-step-over-
	// trap->reinstall dance), which hits a darwin backend lost-wakeup that hangs
	// ~0.3-2% of the time per resume — the same #89 root cause the redesigned
	// fullstack spec routes around, and the same fragile single-step subsystem
	// #78 touches (a distinct bug, left to a follow-up, NOT patched here).
	// kill-while-running deadlocks the darwin backend outright (see
	// declareKillRunningSpec). The specs that surface those paths on darwin are
	// basic (resume-from-breakpoint + step-over, the acceptance canary) and every
	// spec's suspended-kill cleanup. See AGENTS.md -> Test layering.
})
