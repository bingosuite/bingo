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

// declareBasicStepOverSpec adds the continue+step-over acceptance spec to the
// enclosing Ginkgo container. It is the correctness gate: set a breakpoint on a
// line that calls a function, repeatedly Continue to it and StepOver the call,
// asserting the tracee reaches the breakpoint every time and the step advances
// past the BP line (never hangs, errors, or exits early).
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
		for i := 0; i < iters; i++ {
			Expect(h.d.Continue()).To(Succeed(), "Continue #%d", i)
			evt := h.waitFor(15*time.Second,
				protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"Continue #%d expected BreakpointHit, got %s: %s", i, evt.Kind, evt.Payload)

			var hit protocol.BreakpointHitPayload
			Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode BreakpointHit #%d", i)
			Expect(hit.Breakpoint.Location.Line).To(Equal(line), "BreakpointHit #%d at BP line", i)

			// Step over the call on the BP line — the most fragile sequence
			// (restore original byte -> single-step -> reinstall trap -> resume).
			Expect(h.d.StepOver()).To(Succeed(), "StepOver #%d", i)
			evt = h.waitFor(15*time.Second,
				protocol.EventStepped, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventStepped),
				"StepOver #%d expected Stepped, got %s: %s", i, evt.Kind, evt.Payload)

			var st protocol.SteppedPayload
			Expect(json.Unmarshal(evt.Payload, &st)).To(Succeed(), "decode Stepped #%d", i)
			Expect(st.Location.Line).NotTo(Equal(line), "StepOver #%d advanced past the BP line", i)
		}
		AddReportEntry("basic-iterations", iters)
	})
}

// declareChurnSpec adds the multi-thread robustness spec. Under continuous
// thread churn a StepOver may legitimately surface as either Stepped or a
// different thread's BreakpointHit; what must NOT happen is a hang (waitFor
// timeout), an error, or an unexpected process exit.
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

// --- harness ---

type e2eHarness struct {
	d debugger.Debugger
}

// newE2EHarness launches bin under a fresh debugger and registers a bounded
// cleanup. The Kill timeout surfaces the "Kill doesn't reap after a hang"
// failure mode instead of wedging the whole suite.
func newE2EHarness(bin string) *e2eHarness {
	GinkgoHelper()
	d := debugger.New()
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
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-h.d.Events():
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
			// Ignore unrelated events (Continued, BreakpointSet, Output, ...).
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
