//go:build e2e && darwin && arm64 && bingonative

// Diagnostic probe (NOT part of the acceptance gate) for the load-bearing
// assumption behind the darwin "resume with signal 0" fix: does dropping an
// intercepted SIGURG still leave Go's *thread-directed* async preemption
// working under the debugger?
//
// The E2E churn target cooperatively yields every iteration, so it reaches GC
// safe points on its own and passes whether or not SIGURG is delivered — it
// cannot distinguish "signal pending" from "signal dropped". This probe closes
// that coverage gap with a goroutine in a genuinely NON-cooperative tight loop
// (no calls, no allocations, no channel ops => only async-preemptible via
// SIGURG) plus a main loop that runs a stop-the-world runtime.GC() every
// iteration. STW must preempt the tight looper; if the debugger's signal-0
// resume DROPS the thread-directed SIGURG, GC can never stop the world and the
// whole process (including main) freezes, so the debugger stops seeing
// breakpoint hits and waitFor times out.
//
// Guarded behind BINGO_ASYNCPREEMPT_TEST so it never runs in the acceptance
// gate (it is an instrument, and on a wait4 backend it is expected to reveal a
// fundamental limitation rather than a regression).
//
//	go test -tags 'e2e bingonative' -race -c -o /tmp/ap.test ./internal/debugger
//	codesign --sign - --entitlements entitlements.plist --force /tmp/ap.test
//	BINGO_ASYNCPREEMPT_TEST=1 /tmp/ap.test -test.v -test.run TestDarwinAsyncPreemptGC -test.timeout 120s

package debugger_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// tightGCTargetSrc pairs a non-cooperative tight loop with a per-iteration
// stop-the-world GC. `tight` has no function calls / allocations / channel ops
// and never grows its stack, so the compiler inserts no cooperative safe point
// in its back-edge (Go >=1.14 relies on async preemption for such loops). The
// only way to preempt it for STW is a delivered SIGURG.
const tightGCTargetSrc = `package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

var sink uint64

func tight() {
	var x uint64 = 1
	for i := uint64(0); ; i++ {
		x = x*3 ^ i
		sink = x
	}
}

func compute(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += i
	}
	return s
}

func main() {
	go func() { time.Sleep(120 * time.Second); os.Exit(3) }()
	runtime.GOMAXPROCS(2)
	go tight()
	time.Sleep(20 * time.Millisecond) // let tight() occupy a P
	for i := 0; i < 1000000; i++ {
		_ = compute(i % 10) // BP
		runtime.GC()        // STW: must preempt the tight looper
		if i%20 == 0 {
			fmt.Fprintf(os.Stderr, "GC-PROGRESS iter=%d\n", i)
		}
		time.Sleep(time.Millisecond)
	}
}
`

// coopGCTargetSrc is the control: identical to tightGCTargetSrc except the
// looping goroutine calls a //go:noinline function every iteration, which
// inserts a cooperative safe point at the call prologue. Such a goroutine can
// be preempted for STW WITHOUT a delivered SIGURG, so GC must keep returning
// under the debugger even though signal-0 resume drops SIGURG. Pass here + hang
// in the non-cooperative case isolates the cause to dropped async preemption
// (not the harness, the GC, or the step-over path).
const coopGCTargetSrc = `package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

var sink uint64

//go:noinline
func spLeaf() { sink++ }

//go:noinline
func safepoint() { spLeaf() } // non-leaf => real morestack prologue => safe point

func coop() {
	var x uint64 = 1
	for i := uint64(0); ; i++ {
		x = x*3 ^ i
		sink = x
		safepoint() // cooperative safe point at the call prologue
	}
}

func compute(n int) int {
	s := 0
	for i := 0; i < n; i++ {
		s += i
	}
	return s
}

func main() {
	go func() { time.Sleep(120 * time.Second); os.Exit(3) }()
	runtime.GOMAXPROCS(2)
	go coop()
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 1000000; i++ {
		_ = compute(i % 10) // BP
		runtime.GC()
		if i%20 == 0 {
			fmt.Fprintf(os.Stderr, "GC-PROGRESS iter=%d\n", i)
		}
		time.Sleep(time.Millisecond)
	}
}
`

// TestDarwinAsyncPreemptGC drives the tight-loop+GC target under the real
// debugger. If runtime.GC() keeps returning (breakpoint keeps being hit),
// signal-0 resume preserves async preemption. If it hangs, the intercepted
// SIGURG is being dropped.
func TestDarwinAsyncPreemptGC(t *testing.T) {
	if os.Getenv("BINGO_ASYNCPREEMPT_TEST") == "" {
		t.Skip("set BINGO_ASYNCPREEMPT_TEST=1 to run the async-preemption GC probe")
	}
	src := tightGCTargetSrc
	line := markerLine(t, src, "// BP")
	bin := buildTarget(t, "tightgc_target", src)

	h := newHarness(t, bin)
	h.waitFor(15*time.Second, protocol.EventStepped)

	if _, err := h.d.SetBreakpoint("tightgc_target.go", line); err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}

	iters := envInt("BINGO_ASYNCPREEMPT_ITERS", 20)
	for i := 0; i < iters; i++ {
		if err := h.d.Continue(); err != nil {
			t.Fatalf("Continue #%d: %v", i, err)
		}
		// The tell: after a stepover, the next Continue runs main's
		// runtime.GC(). If STW hangs because the tight looper's preemption
		// SIGURG was dropped, main never reaches the BP again and this times
		// out — that timeout IS the "signal dropped" result.
		evt := h.waitFor(15*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		if evt.Kind != protocol.EventBreakpointHit {
			t.Fatalf("iter #%d: expected BreakpointHit, got %v: %s", i, evt.Kind, evt.Payload)
		}
		var hit protocol.BreakpointHitPayload
		_ = json.Unmarshal(evt.Payload, &hit)
		t.Logf("iter #%d: runtime.GC() returned under the debugger; BP hit at line %d", i, hit.Breakpoint.Location.Line)

		if err := h.d.StepOver(); err != nil {
			t.Fatalf("StepOver #%d: %v", i, err)
		}
		h.waitFor(15*time.Second,
			protocol.EventStepped, protocol.EventBreakpointHit,
			protocol.EventProcessExited, protocol.EventError)
	}
	t.Logf("ASYNC-PREEMPT GC PROBE PASS: %d iterations, runtime.GC() returned under the debugger every time", iters)
}

// TestDarwinAsyncPreemptGCControl is the control: same driver, same GC, but the
// looping goroutine is cooperatively preemptible. It must PASS under the same
// signal-0 debugger — proving the non-cooperative hang is specifically dropped
// async preemption, not a harness/GC/step-over defect.
func TestDarwinAsyncPreemptGCControl(t *testing.T) {
	if os.Getenv("BINGO_ASYNCPREEMPT_TEST") == "" {
		t.Skip("set BINGO_ASYNCPREEMPT_TEST=1 to run the async-preemption GC control")
	}
	src := coopGCTargetSrc
	line := markerLine(t, src, "// BP")
	bin := buildTarget(t, "coopgc_target", src)

	h := newHarness(t, bin)
	h.waitFor(15*time.Second, protocol.EventStepped)

	if _, err := h.d.SetBreakpoint("coopgc_target.go", line); err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}

	iters := envInt("BINGO_ASYNCPREEMPT_ITERS", 20)
	for i := 0; i < iters; i++ {
		if err := h.d.Continue(); err != nil {
			t.Fatalf("Continue #%d: %v", i, err)
		}
		evt := h.waitFor(15*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		if evt.Kind != protocol.EventBreakpointHit {
			t.Fatalf("iter #%d: expected BreakpointHit, got %v: %s", i, evt.Kind, evt.Payload)
		}
		if err := h.d.StepOver(); err != nil {
			t.Fatalf("StepOver #%d: %v", i, err)
		}
		h.waitFor(15*time.Second,
			protocol.EventStepped, protocol.EventBreakpointHit,
			protocol.EventProcessExited, protocol.EventError)
	}
	t.Logf("ASYNC-PREEMPT GC CONTROL PASS: %d iterations, cooperative looper preempted for GC every time", iters)
}

// mainOnlyGCTargetSrc is the most fundamental baseline: main does compute (BP) +
// runtime.GC() + sleep, with NO extra busy goroutine. Establishes whether STW
// runtime.GC() works under the debugger at all (it must — nothing here is
// non-cooperative).
const mainOnlyGCTargetSrc = `package main

import (
	"fmt"
	"os"
	"runtime"
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
	go func() { time.Sleep(120 * time.Second); os.Exit(3) }()
	runtime.GOMAXPROCS(2)
	for i := 0; i < 1000000; i++ {
		_ = compute(i % 10) // BP
		runtime.GC()
		if i%20 == 0 {
			fmt.Fprintf(os.Stderr, "GC-PROGRESS iter=%d\n", i)
		}
		time.Sleep(time.Millisecond)
	}
}
`

// TestDarwinGCMainOnly is the fundamental control: STW runtime.GC() under the
// debugger with no non-cooperative goroutine. Must PASS.
func TestDarwinGCMainOnly(t *testing.T) {
	if os.Getenv("BINGO_ASYNCPREEMPT_TEST") == "" {
		t.Skip("set BINGO_ASYNCPREEMPT_TEST=1 to run the main-only GC baseline")
	}
	src := mainOnlyGCTargetSrc
	line := markerLine(t, src, "// BP")
	bin := buildTarget(t, "mainonlygc_target", src)

	h := newHarness(t, bin)
	h.waitFor(15*time.Second, protocol.EventStepped)

	if _, err := h.d.SetBreakpoint("mainonlygc_target.go", line); err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}

	iters := envInt("BINGO_ASYNCPREEMPT_ITERS", 20)
	for i := 0; i < iters; i++ {
		if err := h.d.Continue(); err != nil {
			t.Fatalf("Continue #%d: %v", i, err)
		}
		evt := h.waitFor(15*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		if evt.Kind != protocol.EventBreakpointHit {
			t.Fatalf("iter #%d: expected BreakpointHit, got %v: %s", i, evt.Kind, evt.Payload)
		}
		if err := h.d.StepOver(); err != nil {
			t.Fatalf("StepOver #%d: %v", i, err)
		}
		h.waitFor(15*time.Second,
			protocol.EventStepped, protocol.EventBreakpointHit,
			protocol.EventProcessExited, protocol.EventError)
	}
	t.Logf("MAIN-ONLY GC BASELINE PASS: %d iterations, runtime.GC() returned under the debugger every time", iters)
}
