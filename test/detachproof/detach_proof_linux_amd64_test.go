//go:build detachproof && linux && amd64

package detachproof

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// awaitEvent drains ch until one of kinds arrives. Fails the test on timeout or
// on a closed channel (an unexpected engine teardown).
func awaitEvent(t *testing.T, ch <-chan protocol.Event, timeout time.Duration, kinds ...protocol.EventKind) protocol.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatalf("events channel closed while waiting for %v", kinds)
			}
			for _, k := range kinds {
				if evt.Kind == k {
					return evt
				}
			}
		case <-deadline:
			t.Fatalf("TIMEOUT after %s waiting for %v", timeout, kinds)
		}
	}
}

// Note on the event channel: engine.emit is deliberately non-blocking (it runs
// on the serialized loop), so an unread Events() channel can never wedge the
// engine. The proof therefore reads the channel only where it needs to, and
// never runs a background drainer that would race awaitEvent for the same
// events.

// boundedKill calls Kill with a watchdog so a wedged backend fails loudly.
// It returns Kill's error and whether it returned in time.
func boundedKill(d debugger.Debugger, timeout time.Duration) (error, bool) {
	type res struct{ err error }
	ch := make(chan res, 1)
	go func() { ch <- res{d.Kill()} }()
	select {
	case r := <-ch:
		return r.err, true
	case <-time.After(timeout):
		return nil, false
	}
}

// attachAndArm starts a gated target, attaches the real ptrace backend to it,
// and sets a breakpoint on the gated (not-yet-executed) line. It returns the
// target, the debugger, the breakpointed function's runtime address and its
// pristine on-disk bytes.
//
// When setBP is false the breakpoint step is skipped (used by the no-breakpoint
// control, which isolates the detach failure from the trap-restoration failure).
func attachAndArm(t *testing.T, setBP bool) (*target, debugger.Debugger, uint64, []byte, int) {
	t.Helper()
	bin, srcName := buildProofTarget(t, "detach_proof_target", proofTargetSrc)
	bpLine := markerLine(t, proofTargetSrc, "// BP")
	vaddr, disk := funcImage(t, bin, "main.gated")

	tg := startTarget(t, bin)

	// Baseline: untraced, and the gated function image matches the ELF exactly.
	if tp := mustTracerPID(t, tg.pid, "before attach"); tp != 0 {
		t.Fatalf("target pid %d was already traced (TracerPid=%d) before attach", tg.pid, tp)
	}
	if got, err := readTraceeMem(t, tg.pid, vaddr, len(disk)); err != nil {
		t.Fatalf("baseline memory read: %v", err)
	} else if d := diffBytes(disk, got, 4); len(d) != 0 {
		t.Fatalf("baseline mismatch: in-memory main.gated already differs from the ELF at %v", d)
	}

	d := debugger.New(nil)
	t.Cleanup(func() {
		if _, ok := boundedKill(d, 5*time.Second); !ok {
			t.Logf("cleanup: Kill did not return within 5s (backend may be wedged)")
		}
	})
	if err := d.Attach(tg.pid, bin); err != nil {
		t.Fatalf("Attach to pid %d: %v", tg.pid, err)
	}
	// Attach stops the tracee; the engine reports the stop as EventStepped.
	awaitEvent(t, d.Events(), 15*time.Second, protocol.EventStepped)

	if tp := mustTracerPID(t, tg.pid, "after attach"); !tracedByThisProcess(tp) {
		t.Fatalf("after Attach: TracerPid=%d does not belong to this test process (%d)", tp, os.Getpid())
	}

	bpOff := -1
	if setBP {
		bp, err := d.SetBreakpoint(srcName, bpLine)
		if err != nil {
			t.Fatalf("SetBreakpoint %s:%d: %v", srcName, bpLine, err)
		}
		if bp.Location.Line != bpLine {
			t.Fatalf("breakpoint resolved to line %d, want %d", bp.Location.Line, bpLine)
		}
		// The trap must actually be in the tracee's text now — otherwise the
		// later "trap left behind" assertion would be vacuous.
		got, err := readTraceeMem(t, tg.pid, vaddr, len(disk))
		if err != nil {
			t.Fatalf("post-SetBreakpoint memory read: %v", err)
		}
		idx := diffBytes(disk, got, 8)
		if len(idx) != 1 || got[idx[0]] != int3 {
			t.Fatalf("expected exactly one INT3 patched into main.gated, got diffs %v", idx)
		}
		bpOff = idx[0]
		t.Logf("armed: INT3 installed at main.gated+0x%x (runtime 0x%x)", idx[0], vaddr+uint64(idx[0]))
	}

	return tg, d, vaddr, disk, bpOff
}

// resumeAndConfirmRunning continues the tracee and proves it is genuinely
// running (not parked at a stop) before Kill, via a fresh heartbeat from the
// traced main thread.
func resumeAndConfirmRunning(t *testing.T, tg *target, d debugger.Debugger) {
	t.Helper()
	if err := d.Continue(); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if !tg.beating(5 * time.Second) {
		t.Fatalf("after Continue the traced thread never produced a fresh heartbeat (state=%q); "+
			"the tracee is not running, so this run cannot prove anything about the running path",
			procState(tg.pid))
	}
	if _, err := os.Stat(tg.done); err == nil {
		t.Fatalf("target reached phase 2 before Kill — the gate did not hold")
	}
}

// TestAttachedRunningKillDetaches is the U4 proof.
//
// It asserts the CORRECT, expected behaviour: after Debugger.Kill reports
// success on an attached+running tracee, the process must be fully released —
// untraced, with its original instruction bytes restored — and must then run
// through the formerly-breakpointed line to completion.
//
// A FAILURE of this test on origin/main is the confirmation of hypothesis U4.
func TestAttachedRunningKillDetaches(t *testing.T) {
	iters := proofIters()
	// Tallies make the deterministic part of the failure distinguishable from
	// the racy part: the trap leak reproduces every time, while the detach
	// outcome depends on whether the tracee happens to be in a transient
	// ptrace-stop at the instant PTRACE_DETACH is issued.
	var trapLeaks, detachLeaks, deaths, freezes, completions int
	t.Cleanup(func() {
		t.Logf("U4 TALLY over %d iteration(s): trap-left-behind=%d, still-traced=%d, target-died=%d, "+
			"target-frozen=%d, target-completed=%d", iters, trapLeaks, detachLeaks, deaths, freezes, completions)
	})
	for i := 0; i < iters; i++ {
		t.Run(fmt.Sprintf("iter%d", i), func(t *testing.T) {
			tg, d, vaddr, disk, bpOff := attachAndArm(t, true)
			resumeAndConfirmRunning(t, tg, d)

			killErr, returned := boundedKill(d, 10*time.Second)
			if !returned {
				t.Fatalf("Kill did not return within 10s on an attached+running tracee")
			}
			// The reported outcome. U4's premise is that this is a SUCCESS
			// despite both underlying operations having failed.
			if killErr != nil {
				t.Logf("Kill reported an error (this alone would already be an improvement): %v", killErr)
			} else {
				t.Logf("Kill reported SUCCESS (nil error)")
			}

			// Evidence 1 — the process must no longer be traced.
			tp := mustTracerPID(t, tg.pid, "after Kill")
			if tp != 0 {
				detachLeaks++
				t.Errorf("U4 CONFIRMED (detach leak): after a successful Kill, target pid %d is STILL TRACED "+
					"(TracerPid=%d, this process is %d, proc state=%q). PTRACE_DETACH requires the tracee to be "+
					"in a ptrace-stop; on a running tracee it fails with ESRCH and killProcess discards the error.",
					tg.pid, tp, os.Getpid(), procState(tg.pid))
			}

			// Evidence 2 — the original instruction bytes must be restored.
			got, err := readTraceeMem(t, tg.pid, vaddr, len(disk))
			if err != nil {
				t.Fatalf("post-Kill memory read: %v", err)
			}
			if idx := diffBytes(disk, got, 8); len(idx) != 0 {
				trapLeaks++
				if len(idx) != 1 || idx[0] != bpOff || got[idx[0]] != int3 {
					t.Errorf("unexpected post-Kill divergence: diffs %v (armed offset was 0x%x); "+
						"this is NOT the clean leftover-INT3 signature and must be investigated "+
						"before the result is trusted", idx, bpOff)
				}
				t.Errorf("U4 CONFIRMED (trap leak): after a successful Kill, main.gated still differs from the ELF "+
					"at offsets %v (first byte in memory = 0x%02x, want 0x%02x). bps.clearAll writes via "+
					"PTRACE_POKEDATA, which needs a ptrace-stop; on a running tracee it fails with ESRCH and "+
					"clearAll discards per-entry failures.", idx, got[idx[0]], disk[idx[0]])
			}

			// Evidence 3 — the released process must execute the former
			// breakpoint site normally: no SIGTRAP, no freeze, no crash.
			tg.openGate(t)
			if !waitForFile(tg.done, 20*time.Second) {
				if dead, how := tg.terminated(2 * time.Second); dead {
					deaths++
					t.Errorf("U4 CONFIRMED (tracee destroyed): after a successful Kill the target DIED on reaching "+
						"the former breakpoint instead of completing phase 2 — %s.\n--- target stderr ---\n%s\n---------------------\n"+
						"The leftover INT3 raised a SIGTRAP that no debugger was left to absorb, so the Go runtime "+
						"took it as a fatal trap. Killing an ATTACHED process is exactly what the attached branch "+
						"of killProcess promises not to do.", how, tg.stderr())
				} else {
					freezes++
					t.Errorf("U4 CONFIRMED (tracee frozen): after a successful Kill the target never completed "+
						"phase 2 within 20s and is still alive. proc state=%q, TracerPid=%d. The leftover INT3 "+
						"raised SIGTRAP into a still-attached tracer whose engine had already torn down, leaving "+
						"the traced thread parked in a ptrace-stop with nobody to resume it.",
						procState(tg.pid), mustTracerPID(t, tg.pid, "after gate release"))
				}
			} else {
				completions++
				b, _ := os.ReadFile(tg.done)
				if string(b) != fmt.Sprint(gatedExpected) {
					t.Errorf("target completed phase 2 but computed %q, want %q", b, fmt.Sprint(gatedExpected))
				}
			}
		})
	}
}

// TestControlSuspendedDetach is the positive control: the SAME attach + same
// breakpoint + same Kill, but issued while the tracee is SUSPENDED at the attach
// stop instead of running. Here PTRACE_POKEDATA and PTRACE_DETACH are both
// legal, so this must pass on origin/main. If it fails, the harness — not the
// production path — is at fault, and the main proof means nothing.
func TestControlSuspendedDetach(t *testing.T) {
	tg, d, vaddr, disk, _ := attachAndArm(t, true)

	// No Continue: stay parked at the attach stop.
	if st := procState(tg.pid); st != "t" && st != "T" {
		t.Logf("note: proc state before Kill is %q (expected a tracing-stop)", st)
	}

	killErr, returned := boundedKill(d, 10*time.Second)
	if !returned {
		t.Fatalf("Kill did not return within 10s on an attached+suspended tracee")
	}
	if killErr != nil {
		t.Fatalf("control: Kill on a suspended attached tracee failed: %v", killErr)
	}

	if tp := mustTracerPID(t, tg.pid, "after suspended Kill"); tp != 0 {
		t.Errorf("control FAILED: suspended detach left TracerPid=%d, want 0", tp)
	}
	// Distinguish "detach did not resume the tracee" (a different, SIGSTOP-
	// pending problem) from the U4 trap-freeze the main proof looks for.
	if !tg.beating(5 * time.Second) {
		t.Errorf("control FAILED: after a suspended detach the traced thread is not scheduling (state=%q); "+
			"this is a resume problem, not the U4 trap freeze", procState(tg.pid))
	}
	got, err := readTraceeMem(t, tg.pid, vaddr, len(disk))
	if err != nil {
		t.Fatalf("post-Kill memory read: %v", err)
	}
	if idx := diffBytes(disk, got, 8); len(idx) != 0 {
		t.Errorf("control FAILED: suspended detach left %d modified byte(s) in main.gated at %v", len(idx), idx)
	}
	tg.openGate(t)
	if !waitForFile(tg.done, 20*time.Second) {
		t.Errorf("control FAILED: after a suspended detach the target did not complete phase 2 (state=%q)",
			procState(tg.pid))
	}
}

// TestControlRunningNoBreakpoint is the discriminating control: attached and
// running exactly as in the main proof, but with NO breakpoint installed. It
// separates the two independent failures:
//
//	detach failure  → the process stays traced (still observable here),
//	restore failure → the process is left trapped (NOT observable here).
//
// So a target that stays traced yet still completes phase 2 pins the freeze in
// the main proof on the leftover INT3 rather than on tracing per se.
func TestControlRunningNoBreakpoint(t *testing.T) {
	tg, d, vaddr, disk, _ := attachAndArm(t, false)
	resumeAndConfirmRunning(t, tg, d)

	killErr, returned := boundedKill(d, 10*time.Second)
	if !returned {
		t.Fatalf("Kill did not return within 10s")
	}
	t.Logf("Kill (running, no breakpoint) returned: %v", killErr)

	tp := mustTracerPID(t, tg.pid, "after Kill")
	t.Logf("control observation: TracerPid after running-detach with no breakpoint = %d (0 means detach succeeded)", tp)

	got, err := readTraceeMem(t, tg.pid, vaddr, len(disk))
	if err != nil {
		t.Fatalf("post-Kill memory read: %v", err)
	}
	if idx := diffBytes(disk, got, 8); len(idx) != 0 {
		t.Fatalf("control invariant broken: no breakpoint was set, yet main.gated differs at %v", idx)
	}

	// With no trap in the text the target must complete regardless of whether
	// the detach succeeded.
	tg.openGate(t)
	if !waitForFile(tg.done, 20*time.Second) {
		t.Errorf("control: target with no breakpoint failed to complete phase 2 (state=%q, TracerPid=%d)",
			procState(tg.pid), tp)
	} else if tp != 0 {
		t.Errorf("U4 CONFIRMED (detach leak, isolated): with NO breakpoint the target ran to completion, "+
			"but Kill still left it traced (TracerPid=%d). This isolates the ignored PTRACE_DETACH failure "+
			"from the ignored breakpoint-restore failure.", tp)
	}
}

// eventKindsSeen is a debug aid: it records which event kinds the engine emitted
// around a Kill, so a report can show that no error event surfaced to clients.
func eventKindsSeen(ch <-chan protocol.Event, window time.Duration) []string {
	var kinds []string
	deadline := time.After(window)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return append(kinds, "<closed>")
			}
			kinds = append(kinds, string(evt.Kind))
		case <-deadline:
			return kinds
		}
	}
}

// TestAttachedRunningKillEmitsNoError records the CLIENT-visible outcome: an
// attached+running Kill must not silently look clean if it left the tracee
// traced and trapped. It captures the event stream instead of draining it, so
// the report can state exactly what a WebSocket/DAP client would have seen.
func TestAttachedRunningKillEmitsNoError(t *testing.T) {
	bin, srcName := buildProofTarget(t, "detach_proof_target", proofTargetSrc)
	bpLine := markerLine(t, proofTargetSrc, "// BP")
	tg := startTarget(t, bin)

	d := debugger.New(nil)
	t.Cleanup(func() {
		if _, ok := boundedKill(d, 5*time.Second); !ok {
			t.Logf("cleanup: Kill did not return within 5s")
		}
	})
	if err := d.Attach(tg.pid, bin); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	awaitEvent(t, d.Events(), 15*time.Second, protocol.EventStepped)
	if _, err := d.SetBreakpoint(srcName, bpLine); err != nil {
		t.Fatalf("SetBreakpoint: %v", err)
	}
	if err := d.Continue(); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if !tg.beating(5 * time.Second) {
		t.Fatalf("tracee not running after Continue")
	}

	killErr, returned := boundedKill(d, 10*time.Second)
	if !returned {
		t.Fatalf("Kill did not return within 10s")
	}
	kinds := eventKindsSeen(d.Events(), 2*time.Second)
	payload, _ := json.Marshal(kinds)
	t.Logf("Kill error = %v; events observed after Kill = %s", killErr, payload)

	tp := mustTracerPID(t, tg.pid, "after Kill")
	if killErr == nil && tp != 0 {
		t.Errorf("U4 CONFIRMED (silent failure): Kill returned nil and emitted %s, yet the tracee is still "+
			"traced (TracerPid=%d). No client — WebSocket, DAP or CLI — can learn that the detach failed.",
			payload, tp)
	}
}
