//go:build e2e && darwin && arm64 && bingonative

// Diagnostic (NOT part of the acceptance gate; skipped unless
// BINGO_GC_PREEMPT_PROBE=1). Deterministically reproduces the pure Layer-2
// wedge so task suspend_count can be sampled at a fresh #2-OFF hang. It is
// Continue-only (no step-over), so it isolates the SIGURG/async-preemption
// residual from the Layer-1 atomic step-over fix. Pair with
// BINGO_DARWIN_SUSPEND_PROBE=1 to dump suspend_count + per-thread state at the
// wedge. Reuses the harness helpers from e2e_integration_test.go.
package debugger_test

import (
	"os"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// gcPreemptTargetSrc: a persistently-runnable, NON-cooperative goroutine (a
// tight integer loop with a back-edge and no calls/allocs -> NOT cooperatively
// preemptible; it can only be stopped by ASYNC preemption via SIGURG) plus a
// stop-the-world runtime.GC() that must preempt it every iteration. postGC is
// the breakpoint. If the debugger's SIGURG re-injection is misdirected (root
// cause #2), async preemption breaks, the STW GC can never stop the spinner,
// postGC is never reached, and the Continue wait wedges deterministically AT
// the GC. That is the pure Layer-2 scenario.
const gcPreemptTargetSrc = `package main

import (
	"os"
	"runtime"
	"time"
)

var sink uint64

func spin() {
	for {
		sink++
	}
}

//go:noinline
func postGC(i int) {
	sink += uint64(i) // BP
}

func main() {
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	go spin()
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 100000; i++ {
		runtime.GC()
		postGC(i)
	}
}
`

func TestE2EGCPreemptWedge(t *testing.T) {
	if os.Getenv("BINGO_GC_PREEMPT_PROBE") == "" {
		t.Skip("diagnostic; set BINGO_GC_PREEMPT_PROBE=1 (and BINGO_DARWIN_SUSPEND_PROBE=1) to run")
	}
	src := gcPreemptTargetSrc
	line := markerLine(t, src, "// BP")
	bin := buildTarget(t, "gcpreempt_target", src)

	h := newHarness(t, bin)
	h.waitFor(15*time.Second, protocol.EventStepped)

	if _, err := h.d.SetBreakpoint("gcpreempt_target.go", line); err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}

	iters := envInt("BINGO_E2E_ITERS", 25)
	for i := 0; i < iters; i++ {
		if err := h.d.Continue(); err != nil {
			t.Fatalf("Continue #%d: %v", i, err)
		}
		// 20s > the probe's 9s watchdog threshold, so a wedge dumps
		// suspend_count before this waitFor fatals.
		evt := h.waitFor(20*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		if evt.Kind != protocol.EventBreakpointHit {
			t.Fatalf("Continue #%d: expected BreakpointHit, got %v: %s", i, evt.Kind, evt.Payload)
		}
		t.Logf("GC-preempt continue #%d OK (STW GC completed)", i)
	}
	t.Logf("GC-PREEMPT: %d continues completed without a wedge", iters)
}
