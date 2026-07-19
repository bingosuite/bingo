//go:build e2e && linux && amd64

package integration

import . "github.com/onsi/ginkgo/v2"

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
	declareClearBreakpointSpec()
	declareKillRunningSpec()
	declareFullStackSpec()
	declareRestartSpec()
})
