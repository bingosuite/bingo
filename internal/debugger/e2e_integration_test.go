//go:build e2e && ((linux && amd64) || (darwin && arm64 && bingonative))

// Package debugger_test — real end-to-end acceptance tests that exercise the
// native OS backend (ptrace on linux/amd64, ptrace+Mach on darwin/arm64)
// against a freshly-built target process. Unlike the unit tests (which use the
// in-process fakeBackend) these drive the ACTUAL low-level debugging path and
// are the acceptance gate for the debugger backend fixes.
//
// Build tag `e2e` keeps them out of the default `go test ./...` run.
//
// Run on linux/amd64:
//
//	go test -tags e2e -race -run TestE2E ./internal/debugger -v
//
// Run on darwin/arm64 (needs the debugger entitlement, so codesign the test
// binary before running it):
//
//	go test -tags 'e2e bingonative' -c -o /tmp/e2e.test ./internal/debugger
//	codesign --sign - --entitlements entitlements.plist --force /tmp/e2e.test
//	/tmp/e2e.test -test.v -test.run TestE2E
//
// Tuning env vars:
//
//	BINGO_E2E_ITERS        (default 25)   basic continue+stepover iterations
//	BINGO_E2E_CHURN_ITERS  (default 200)  iterations under thread churn
package debugger_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// Set BINGO_E2E_DEBUG=1 to route engine debug logs to stderr and log every
// received event — invaluable when diagnosing a hang or a wrong-thread step.
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

func TestE2EBasic(t *testing.T) {
	src := basicTargetSrc
	line := markerLine(t, src, "// BP")
	bin := buildTarget(t, "basic_target", src)

	h := newHarness(t, bin)

	// Launch stops at the entry point and emits an initial Stepped.
	h.waitFor(15*time.Second, protocol.EventStepped)

	bp, err := h.d.SetBreakpoint("basic_target.go", line)
	if err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}
	if bp.Location.Line != line {
		t.Fatalf("breakpoint resolved to line %d, want %d", bp.Location.Line, line)
	}

	iters := envInt("BINGO_E2E_ITERS", 25)
	for i := 0; i < iters; i++ {
		if err := h.d.Continue(); err != nil {
			t.Fatalf("Continue #%d: %v", i, err)
		}
		evt := h.waitFor(15*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		if evt.Kind != protocol.EventBreakpointHit {
			t.Fatalf("Continue #%d: expected BreakpointHit, got %v: %s", i, evt.Kind, evt.Payload)
		}
		var hit protocol.BreakpointHitPayload
		if err := json.Unmarshal(evt.Payload, &hit); err != nil {
			t.Fatalf("Continue #%d: decode BreakpointHit: %v", i, err)
		}
		if hit.Breakpoint.Location.Line != line {
			t.Fatalf("Continue #%d: BreakpointHit at line %d, want %d", i, hit.Breakpoint.Location.Line, line)
		}

		// Step over the call on the BP line — the most fragile sequence
		// (restore original byte -> single-step -> reinstall trap -> resume).
		if err := h.d.StepOver(); err != nil {
			t.Fatalf("StepOver #%d: %v", i, err)
		}
		evt = h.waitFor(15*time.Second,
			protocol.EventStepped, protocol.EventProcessExited, protocol.EventError)
		if evt.Kind != protocol.EventStepped {
			t.Fatalf("StepOver #%d: expected Stepped, got %v: %s", i, evt.Kind, evt.Payload)
		}
		var st protocol.SteppedPayload
		if err := json.Unmarshal(evt.Payload, &st); err != nil {
			t.Fatalf("StepOver #%d: decode Stepped: %v", i, err)
		}
		if st.Location.Line == line {
			t.Fatalf("StepOver #%d: did not advance past BP line %d", i, line)
		}
	}
	t.Logf("E2E BASIC PASS: %d continue+stepover iterations at line %d", iters, line)
}

func TestE2EChurn(t *testing.T) {
	src := churnTargetSrc
	line := markerLine(t, src, "// BP")
	bin := buildTarget(t, "churn_target", src)

	h := newHarness(t, bin)

	h.waitFor(20*time.Second, protocol.EventStepped)

	if _, err := h.d.SetBreakpoint("churn_target.go", line); err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}

	iters := envInt("BINGO_E2E_CHURN_ITERS", 200)
	for i := 0; i < iters; i++ {
		if err := h.d.Continue(); err != nil {
			t.Fatalf("Continue #%d: %v", i, err)
		}
		evt := h.waitFor(20*time.Second,
			protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
		if evt.Kind != protocol.EventBreakpointHit {
			t.Fatalf("Continue #%d: expected BreakpointHit, got %v: %s", i, evt.Kind, evt.Payload)
		}
		if err := h.d.StepOver(); err != nil {
			t.Fatalf("StepOver #%d: %v", i, err)
		}
		// Under churn a different thread can legitimately reach the breakpoint
		// during the step, so accept Stepped OR BreakpointHit. What must NOT
		// happen: a hang (waitFor timeout), an error, or an unexpected exit.
		evt = h.waitFor(20*time.Second,
			protocol.EventStepped, protocol.EventBreakpointHit,
			protocol.EventProcessExited, protocol.EventError)
		if evt.Kind == protocol.EventProcessExited || evt.Kind == protocol.EventError {
			t.Fatalf("StepOver #%d: unexpected %v: %s", i, evt.Kind, evt.Payload)
		}
	}
	t.Logf("E2E CHURN PASS: %d continue+stepover iterations under thread churn at line %d", iters, line)
}

// --- harness helpers ---

type harness struct {
	t *testing.T
	d debugger.Debugger
}

func newHarness(t *testing.T, bin string) *harness {
	t.Helper()
	d := debugger.New()
	if err := d.Launch(bin, nil, nil); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { _ = d.Kill(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			// Surfaces the "Kill doesn't reap after a hang" failure mode.
			t.Logf("WARNING: Kill did not return within 5s (backend may be wedged)")
		}
	})
	return &harness{t: t, d: d}
}

// waitFor drains the event stream until one of kinds arrives, failing on
// timeout (a hang) or a closed channel.
func (h *harness) waitFor(timeout time.Duration, kinds ...protocol.EventKind) protocol.Event {
	h.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-h.d.Events():
			if !ok {
				h.t.Fatalf("events channel closed while waiting for %v", kinds)
			}
			if e2eDebug {
				h.t.Logf("event: kind=%v seq=%d payload=%s", evt.Kind, evt.Seq, evt.Payload)
			}
			for _, k := range kinds {
				if evt.Kind == k {
					return evt
				}
			}
			// Ignore unrelated events (Continued, BreakpointSet, Output, ...).
		case <-deadline:
			h.t.Fatalf("TIMEOUT after %s waiting for %v (possible hang)", timeout, kinds)
		}
	}
}

func buildTarget(t *testing.T, name, src string) string {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, name+".go")
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		t.Fatalf("write target source: %v", err)
	}
	binPath := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build target %s: %v\n%s", name, err, out)
	}
	return binPath
}

func markerLine(t *testing.T, src, marker string) int {
	t.Helper()
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, marker) {
			return i + 1
		}
	}
	t.Fatalf("marker %q not found in target source", marker)
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
