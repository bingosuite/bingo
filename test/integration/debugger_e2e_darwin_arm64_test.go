//go:build e2e && darwin && arm64 && bingonative

package integration

import . "github.com/onsi/ginkgo/v2"

// Darwin/arm64 debugger acceptance suite. Drives the real ptrace+Mach backend
// (atomic single-thread step-over, asyncpreemptoff-by-default so a thread-
// directed SIGURG can't be misdirected under wait4 — see backend_darwin_arm64.go
// and #92). Needs the bingonative tag and a debugger-entitled (codesigned) test
// binary; see .github/workflows/debugger-e2e.yml and the justfile e2e-darwin
// recipe.
//
// This container lists ONLY the specs that are DETERMINISTICALLY green on the
// current darwin backend, so `just e2e-darwin` and the darwin CI job never flake.
// The common denominator: every spec here resumes the tracee with a PLAIN
// continue INTO a trap (or from a launch/paused stop) and only ever kills a
// SUSPENDED tracee — it never single-steps *off* an armed software breakpoint.
//   - pause: Continue -> Pause -> Paused round-trips (async interrupt).
//   - inspect: continue INTO a breakpoint, then StackFrames/Locals/Goroutines.
//   - fullstack: the whole client -> WebSocket -> hub -> debugger transport on
//     plain-resume paths plus one breakpoint hit entered from a Paused stop.
//   - restart: hub kill + relaunch + reinstall, reaching the breakpoint again by
//     a plain continue into the freshly-armed trap.
//
// Everything that single-steps *off* an armed software breakpoint (basic's
// StepOver, stepping's StepInto/StepOut, breakpoints' continue off a parked BP,
// and churn's hundreds of step-overs) is deliberately LINUX-ONLY, as is kill
// (kill-while-running). On darwin a BSD signal delivered *during* the
// restore->single-step-over-the-trap->reinstall dance can divert PC into the Go
// runtime signal trampoline, and the step-retire logic can't tell "stepped past
// the trap" from "diverted into the handler", so those paths hang a low but
// nonzero fraction of the time; kill-while-running deadlocks outright. All are
// wait4-model gaps that the Mach-exception rearchitecture in #92 closes, which
// restores basic, stepping, breakpoints, churn, and kill to this container. See
// AGENTS.md -> Test layering.
var _ = Describe("Darwin arm64 debugger backend (ptrace+Mach) E2E", Label("darwin"), func() {
	declarePauseSpec()
	declareInspectSpec()
	declareFullStackSpec()
	declareRestartSpec()
})
