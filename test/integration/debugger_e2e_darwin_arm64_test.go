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
	declareFullStackSpec()
})
