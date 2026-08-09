//go:build e2e && darwin && arm64 && bingonative

// Darwin/arm64 half of the U1 fatal-signal proof — the cross-platform control.
//
// The darwin backend masks ONLY EXC_MASK_BREAKPOINT and leaves BSD signals to
// native, thread-directed delivery (see AGENTS.md → Backend quirks → Darwin), so
// a synchronous fatal fault is never intercepted and never reaches the engine's
// StopSignal branch at all: the tracee dies and surfaces as a process exit. This
// container asserts that, which establishes two things the report needs — that
// the target itself is not the reason linux wedges, and that any fix must be
// confined to the linux backend because darwin is already correct.
//
// No raw-ptrace ablation here: darwin has no ptrace path to ablate.

package integration

import . "github.com/onsi/ginkgo/v2"

var _ = Describe("PROOF U1: darwin fatal signal handling (control)", Label("darwin"), func() {
	declareUndebuggedControlSpec()
	declareFatalSignalTerminatesSpec()
})
