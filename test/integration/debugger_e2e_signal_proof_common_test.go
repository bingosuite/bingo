//go:build e2e && ((linux && amd64) || (darwin && arm64 && bingonative))

// PROOF-ONLY SUITE — hypothesis U1 (fatal-signal suppression).
//
// Adds NO production code and changes no existing spec. It exists to settle one
// claim about the linux/amd64 backend on a real kernel rather than by reading
// code:
//
//	engine.handleStop's StopSignal branch resumes via backend.ContinueProcess(),
//	and linuxBackend.ContinueProcess() always issues PTRACE_CONT with signal 0.
//	A signal-delivery-stop for a SYNCHRONOUS fatal fault (SIGSEGV / SIGBUS /
//	SIGILL / SIGFPE) is therefore discarded: the faulting instruction is
//	re-executed without the signal, refaults immediately, and the tracee spins
//	forever instead of dying.
//
// This file holds the cross-platform pieces — the target, the observation
// helpers, and the two specs whose meaning is identical on both backends. The
// platform files wire them into per-OS containers, and the linux file adds the
// raw-ptrace ablations that isolate the root cause to the PTRACE_CONT signal
// argument.
//
// Two labels, deliberately separated:
//
//	signalproof      — DIAGNOSTIC. Documents observed behaviour. Passes today on
//	                   both platforms; the linux storm spec passing is the bug.
//	signalregression — the CORRECT behaviour. Expected to FAIL on linux today
//	                   (that failure is the reproduction) and PASS on darwin
//	                   (whose Mach model leaves BSD signals native, so fatal
//	                   faults never reach this code path at all). This is the
//	                   shape a real regression test would take.
//
// Everything is bounded: the target self-exits after segvTargetWatchdog as a CI
// orphan guard, every observation window is a wall-clock deadline, and each spec
// reaps whatever it started.
//
// Tuning env vars:
//
//	BINGO_PROOF_WINDOW_MS  (default 5000)  observation window for the wedge specs
//	BINGO_PROOF_EXIT_MS    (default 20000) budget for a terminal outcome

package integration

import (
	"bytes"
	"fmt"
	"os/exec"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// segvTargetWatchdog bounds the tracee's lifetime no matter what the tracer
// does, so a reproduced wedge can never leave an orphan spinning on a CI runner.
// It is far longer than any observation window, so it can never be mistaken for
// the fatal outcome the specs are looking for.
const segvTargetWatchdog = 120 * time.Second

// segvTargetSrc dereferences a nil pointer on a known line. That is the minimal
// deterministic synchronous fault: the kernel raises SIGSEGV against the
// faulting thread and — critically — re-raises it every time the same
// instruction is re-executed, which is what turns a discarded signal into an
// unbounded refault loop rather than a one-off missed notification.
//
// runtime.LockOSThread pins main to the main OS thread so the fault is
// guaranteed to arrive on the thread the raw-ptrace ablations trace (they
// deliberately do not enable PTRACE_O_TRACECLONE — see runPtraceAblation).
//
// `p` is a package var so -N -l cannot constant-fold the deref away. The
// watchdog goroutine is the orphan guard described above; its distinctive exit
// code makes a watchdog exit impossible to confuse with a fault exit.
const segvTargetSrc = `package main

import (
	"os"
	"runtime"
	"time"
)

var p *int

func main() {
	go func() { time.Sleep(120 * time.Second); os.Exit(7) }()
	runtime.LockOSThread()
	*p = 1 // SEGV
	os.Exit(0)
}
`

// declareUndebuggedControlSpec is the baseline every other spec is measured
// against: with no tracer in the loop the kernel delivers SIGSEGV to the Go
// runtime's handler, which panics and terminates the process in milliseconds.
// If this spec ever fails, the target is wrong and the rest prove nothing.
func declareUndebuggedControlSpec() {
	It("control: the target reaches a terminal fatal outcome with no debugger", Label("signalproof"), func() {
		bin := buildTarget("segv_target", segvTargetSrc)

		cmd := exec.Command(bin)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		start := time.Now()
		err := runBounded(cmd, proofExitBudget())
		elapsed := time.Since(start)

		Expect(err).To(HaveOccurred(), "undebugged target must NOT exit cleanly")
		Expect(elapsed).To(BeNumerically("<", segvTargetWatchdog),
			"terminated by the fault, not by the watchdog")
		Expect(stderr.String()).To(ContainSubstring("SIGSEGV"),
			"undebugged target dies via the runtime's SIGSEGV handler")

		AddReportEntry("control-undebugged", fmt.Sprintf(
			"terminated after %s with %v", elapsed.Round(time.Millisecond), err))
	})
}

// declareFatalSignalTerminatesSpec asserts the behaviour a debugger MUST have:
// attaching a debugger may not make a fatally-faulting program immortal. The
// tracee is expected to reach the same terminal outcome the undebugged control
// reaches, and the debugger is expected to report it.
//
// This is the reproduction on linux (where it fails) and the
// darwin-is-unaffected proof (where it passes), which is exactly why it is
// written as a correctness assertion rather than a "count the storm" assertion.
func declareFatalSignalTerminatesSpec() {
	It("a fatal fault under the debugger terminates the tracee", Label("signalregression"), func() {
		bin := buildTarget("segv_target", segvTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped) // launch entry stop

		Expect(h.d.Continue()).To(Succeed(), "resume into the faulting instruction")

		budget := proofExitBudget()
		signals, terminal := drainSignalStorm(h.d.Events(), budget)

		AddReportEntry("signal-events-observed", signals)
		AddReportEntry("terminal-outcome", fmt.Sprintf("%q", terminal))

		Expect(terminal).NotTo(BeEmpty(), "no terminal outcome within %s: the debugger "+
			"swallowed the fault and the tracee is refaulting forever "+
			"(%d signal notifications observed in the window)", budget, signals)
	})
}

// --- observation helpers ---

// drainSignalStorm consumes the debugger's event stream for the whole window and
// returns how many "signal N" outputs arrived plus the first terminal condition
// seen (empty if the tracee never terminated). It deliberately keeps draining
// after the first signal: one signal proves only that a stop surfaced, whereas
// an unbounded stream with no progress is the wedge itself.
func drainSignalStorm(ch <-chan protocol.Event, window time.Duration) (int, string) {
	GinkgoHelper()
	deadline := time.After(window)
	signals := 0
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return signals, "events channel closed (engine torn down)"
			}
			switch evt.Kind {
			case protocol.EventOutput:
				if bytes.Contains(evt.Payload, []byte("signal ")) {
					signals++
				}
			case protocol.EventProcessExited:
				return signals, "EventProcessExited: " + string(evt.Payload)
			}
		case <-deadline:
			return signals, ""
		}
	}
}

// runBounded runs cmd and returns its error, failing the spec if it outlives
// budget — the control is supposed to die fast, so outliving the budget is
// itself a failure rather than something to tolerate.
func runBounded(cmd *exec.Cmd, budget time.Duration) error {
	GinkgoHelper()
	Expect(cmd.Start()).To(Succeed(), "start control target")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(budget):
		_ = cmd.Process.Kill()
		<-done
		Fail(fmt.Sprintf("control target did not terminate within %s", budget))
		return nil
	}
}

func proofWindow() time.Duration {
	return time.Duration(envInt("BINGO_PROOF_WINDOW_MS", 5000)) * time.Millisecond
}

func proofExitBudget() time.Duration {
	return time.Duration(envInt("BINGO_PROOF_EXIT_MS", 20000)) * time.Millisecond
}
