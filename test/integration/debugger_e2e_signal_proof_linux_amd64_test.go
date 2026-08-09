//go:build e2e && linux && amd64

// Linux/amd64 half of the U1 fatal-signal proof. Adds the two pieces that only
// make sense against ptrace: a diagnostic that measures the refault storm the
// engine produces today, and a pair of raw-ptrace ablations whose ONLY
// difference is the signal argument to PTRACE_CONT — which is what pins the
// root cause to that single value rather than to anything else in the backend.

package integration

import (
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sys/unix"

	"github.com/bingosuite/bingo/pkg/protocol"
)

var _ = Describe("PROOF U1: linux fatal signal suppression", Label("linux"), func() {
	declareUndebuggedControlSpec()
	declareFatalSignalTerminatesSpec()
	declareSignalStormSpec()
	declarePtraceAblationSpecs()
})

// declareSignalStormSpec measures the bug instead of asserting the fix. It
// passes while the bug exists, so it is the artifact that PROVES the wedge
// (repeated notifications, no progress) rather than merely failing when
// something is wrong. Paired with the undebugged control — which dies in
// milliseconds — an unbounded stream inside the same window is conclusive.
func declareSignalStormSpec() {
	It("diagnostic: the engine reports the same fault repeatedly and never progresses",
		Label("signalproof"), func() {
			bin := buildTarget("segv_target", segvTargetSrc)

			h := newE2EHarness(bin)
			h.waitFor(15*time.Second, protocol.EventStepped)

			Expect(h.d.Continue()).To(Succeed(), "resume into the faulting instruction")

			window := proofWindow()
			signals, terminal := drainSignalStorm(h.d.Events(), window)

			AddReportEntry("storm-signal-events", signals)
			AddReportEntry("storm-terminal-outcome", fmt.Sprintf("%q", terminal))

			Expect(terminal).To(BeEmpty(),
				"observed a terminal outcome %q — the wedge did not reproduce", terminal)
			Expect(signals).To(BeNumerically(">", 1),
				"a single swallowed signal would only be a lost notification; repeated "+
					"notifications for the same instruction are the refault loop")
		})
}

// declarePtraceAblationSpecs is the controlled experiment. Both specs run the
// same target under the same minimal tracer; only the PTRACE_CONT signal
// argument differs. Suppressed wedges, forwarded terminates.
func declarePtraceAblationSpecs() {
	It("ablation: raw ptrace resuming with signal 0 wedges the tracee",
		Label("signalproof"), func() {
			bin := buildTarget("segv_target", segvTargetSrc)

			res := runPtraceAblation(bin, false, proofWindow())

			AddReportEntry("ablation-suppressed", res.String())
			Expect(res.err).NotTo(HaveOccurred())
			Expect(res.terminated).To(BeFalse(),
				"resuming with signal 0 — what ContinueProcess does — must leave the "+
					"tracee alive and refaulting")
			Expect(res.faultStops).To(BeNumerically(">", 1),
				"the same fault must be reported over and over")
		})

	It("ablation: raw ptrace forwarding the signal terminates the tracee",
		Label("signalproof"), func() {
			bin := buildTarget("segv_target", segvTargetSrc)

			res := runPtraceAblation(bin, true, proofExitBudget())

			AddReportEntry("ablation-forwarded", res.String())
			Expect(res.err).NotTo(HaveOccurred())
			Expect(res.terminated).To(BeTrue(),
				"forwarding the signal lets the fault reach the tracee and kill it — "+
					"the only change from the wedging ablation is this argument")
			Expect(res.status.ExitStatus()).To(Equal(2),
				"Go's SIGSEGV handler panics and exits 2, exactly like the undebugged control")
			Expect(res.faultStops).To(BeNumerically("<=", 2),
				"a forwarded fault is delivered once, not stormed")
		})
}

// --- raw ptrace ablation ---

type ablationResult struct {
	faultStops int
	terminated bool
	status     syscall.WaitStatus
	elapsed    time.Duration
	err        error
}

func (r ablationResult) String() string {
	return fmt.Sprintf("faultStops=%d terminated=%t status=0x%x elapsed=%s err=%v",
		r.faultStops, r.terminated, uint32(r.status), r.elapsed.Round(time.Millisecond), r.err)
}

// runPtraceAblation is a minimal tracer that mirrors the linux backend's wait
// loop closely enough to isolate one variable. It handles the same signal
// classes the backend does (SIGTRAP absorbed, everything else re-delivered so
// the Go runtime keeps scheduling) and differs only in what it passes to
// PTRACE_CONT for a fatal fault: signal 0 when forward is false — the production
// behaviour — and the real signal when forward is true.
//
// PTRACE_O_TRACECLONE is deliberately NOT set: this tracer only needs the main
// thread, which the target pins with runtime.LockOSThread. PTRACE_O_EXITKILL IS
// set, so the tracee dies with the test process even if the spec aborts.
//
// Every ptrace request must issue from the thread that became the tracer, so the
// whole sequence — fork/exec included — runs on one locked OS thread.
func runPtraceAblation(bin string, forward bool, window time.Duration) ablationResult {
	GinkgoHelper()
	out := make(chan ablationResult, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		out <- tracePtraceAblation(bin, forward, window)
	}()

	select {
	case res := <-out:
		return res
	case <-time.After(window + 30*time.Second):
		Fail("ptrace ablation goroutine did not finish within its own window")
		return ablationResult{}
	}
}

//nolint:gocognit,gocyclo // One serialized wait/resume state machine, mirroring the backend.
func tracePtraceAblation(bin string, forward bool, window time.Duration) ablationResult {
	var res ablationResult
	start := time.Now()
	defer func() { res.elapsed = time.Since(start) }()

	cmd := exec.Command(bin)
	cmd.Stderr = GinkgoWriter
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		res.err = fmt.Errorf("start tracee: %w", err)
		return res
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_, _ = syscall.Wait4(pid, nil, 0, nil)
	}()

	// cmd.Start returns with the tracee stopped at its post-execve SIGTRAP.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		res.err = fmt.Errorf("initial wait4: %w", err)
		return res
	}
	if err := syscall.PtraceSetOptions(pid, unix.PTRACE_O_EXITKILL); err != nil {
		res.err = fmt.Errorf("PTRACE_O_EXITKILL: %w", err)
		return res
	}
	if err := syscall.PtraceCont(pid, 0); err != nil {
		res.err = fmt.Errorf("initial PTRACE_CONT: %w", err)
		return res
	}

	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		// Wait on our own pid, never -1: the test process hosts other tracees
		// (engine waitLoops from neighbouring specs), and a wait4(-1) here would
		// reap one of theirs and misreport it as this ablation's outcome.
		tid, err := syscall.Wait4(pid, &ws, 0, nil)
		if err != nil {
			res.err = fmt.Errorf("wait4: %w", err)
			return res
		}
		if ws.Exited() || ws.Signaled() {
			res.terminated = true
			res.status = ws
			return res
		}
		if !ws.Stopped() {
			continue
		}

		sig := ws.StopSignal()
		switch sig {
		case syscall.SIGTRAP:
			err = syscall.PtraceCont(tid, 0)
		case syscall.SIGSEGV, syscall.SIGBUS, syscall.SIGILL, syscall.SIGFPE:
			res.faultStops++
			if forward {
				err = syscall.PtraceCont(tid, int(sig))
			} else {
				// The production line under test: the fault is discarded.
				err = syscall.PtraceCont(tid, 0)
			}
		default:
			err = syscall.PtraceCont(tid, int(sig))
		}
		if err != nil && err != syscall.ESRCH {
			res.err = fmt.Errorf("PTRACE_CONT sig %d tid %d: %w", sig, tid, err)
			return res
		}
	}
	return res
}
