//go:build e2e && linux && amd64

// PROOF-ONLY SUITE — hypothesis U1 (linux/amd64 fatal-signal suppression).
//
// This file adds NO production code and changes no existing spec. It exists to
// prove or disprove one claim about the linux backend:
//
//	engine.handleStop's StopSignal branch resumes with
//	backend.ContinueProcess(), and linuxBackend.ContinueProcess() always issues
//	PTRACE_CONT with signal 0. A signal-delivery-stop for a SYNCHRONOUS fatal
//	fault (SIGSEGV / SIGBUS / SIGILL / SIGFPE) is therefore swallowed: the
//	faulting instruction is re-executed with the signal discarded, refaults
//	immediately, and the tracee spins forever instead of dying.
//
// Four specs, all Label("signalproof"), all bounded:
//
//	1. control-undebugged  — the same binary, run with NO debugger, must reach a
//	                         terminal fatal outcome quickly. Establishes the
//	                         EXPECTED behaviour.
//	2. repro-bingo         — the same binary under the REAL debugger.Debugger
//	                         (real ptrace backend). Observes the storm of
//	                         "signal 11" EventOutputs and the absence of
//	                         EventProcessExited.
//	3. ablation-suppressed — a minimal raw-ptrace tracer written here, resuming
//	                         with signal 0 exactly like ContinueProcess does.
//	4. ablation-forwarded  — byte-identical to (3) except PTRACE_CONT carries the
//	                         signal. The ONLY difference between 3 and 4 is the
//	                         signal argument, which isolates the root cause to
//	                         that one value.
//
// Everything is bounded: the target self-exits after segvTargetWatchdog as a
// CI orphan guard, every observation window is a wall-clock deadline, and each
// spec SIGKILLs and reaps whatever it started.
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
	"runtime"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"golang.org/x/sys/unix"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// segvTargetWatchdog bounds the tracee's lifetime no matter what the tracer
// does, so a reproduced wedge can never leave an orphan spinning on a CI runner.
// It is far longer than any observation window, so it cannot be mistaken for the
// fatal outcome the specs are looking for.
const segvTargetWatchdog = 120 * time.Second

// segvTargetSrc dereferences a nil pointer on a known line. The deref is the
// minimal deterministic synchronous fault: the kernel raises SIGSEGV against the
// faulting thread, and — critically — re-raises it every time the same
// instruction is re-executed, which is what turns a swallowed signal into an
// unbounded refault loop rather than a one-off missed notification.
//
// runtime.LockOSThread pins main to the main OS thread so the fault is
// guaranteed to arrive on the thread the raw-ptrace ablations trace (they
// deliberately do not enable PTRACE_O_TRACECLONE — see runPtraceAblation).
//
// The watchdog goroutine is the orphan guard described above. `p` is a package
// var so -N -l cannot constant-fold the deref away.
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

var _ = Describe("PROOF U1: linux fatal signal suppression", Label("linux", "signalproof"), func() {

	// Control. Without a tracer in the loop the kernel delivers SIGSEGV to the Go
	// runtime's handler, which panics and terminates the process. This is the
	// EXPECTED outcome that the debugged runs are measured against; if this spec
	// ever fails, the target is wrong and the other three prove nothing.
	It("control: the target reaches a terminal fatal outcome with no debugger", func() {
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

	// Reproduction against the real thing: real debugger.Debugger, real ptrace
	// backend, real tracee. Asserts the two halves of the hypothesis at once —
	// a storm of repeated signal-delivery notifications (no progress) AND no
	// terminal event within a window the control clears in well under a second.
	It("repro: the debugger swallows SIGSEGV and the tracee refaults forever", func() {
		bin := buildTarget("segv_target", segvTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(15*time.Second, protocol.EventStepped) // launch entry stop

		Expect(h.d.Continue()).To(Succeed(), "resume into the faulting instruction")

		window := proofWindow()
		signals, terminal := drainSignalStorm(h.d.Events(), window)

		AddReportEntry("repro-signal-11-events", signals)
		AddReportEntry("repro-terminal-event", fmt.Sprintf("%v", terminal))

		Expect(terminal).To(BeEmpty(),
			"BUG CONFIRMED CHECK: the tracee must have reached a terminal outcome "+
				"like the undebugged control, but the debugger reported %q", terminal)
		Expect(signals).To(BeNumerically(">", 1),
			"a single swallowed signal would be a lost notification; repeated ones "+
				"prove the faulting instruction is being re-executed forever")
	})

	// Ablation A — the production primitive, in isolation. PTRACE_CONT with
	// signal 0 is exactly what linuxBackend.ContinueProcess() issues.
	It("ablation: raw ptrace resuming with signal 0 wedges the tracee", func() {
		bin := buildTarget("segv_target", segvTargetSrc)

		res := runPtraceAblation(bin, false, proofWindow())

		AddReportEntry("ablation-suppressed", res.String())
		Expect(res.err).NotTo(HaveOccurred())
		Expect(res.terminated).To(BeFalse(),
			"resuming with signal 0 must leave the tracee alive and refaulting")
		Expect(res.faultStops).To(BeNumerically(">", 1),
			"the same fault must be reported over and over")
	})

	// Ablation B — the controlled counterfactual. Identical tracer, identical
	// target; the ONLY change is that PTRACE_CONT carries the signal. If this
	// terminates while A wedges, the signal argument is the whole root cause.
	It("ablation: raw ptrace forwarding the signal terminates the tracee", func() {
		bin := buildTarget("segv_target", segvTargetSrc)

		res := runPtraceAblation(bin, true, proofExitBudget())

		AddReportEntry("ablation-forwarded", res.String())
		Expect(res.err).NotTo(HaveOccurred())
		Expect(res.terminated).To(BeTrue(),
			"forwarding the signal must let the fault reach the tracee and kill it")
		Expect(res.faultStops).To(BeNumerically("<=", 2),
			"a forwarded fault is delivered once, not stormed")
	})
})

// --- observation helpers ---

// drainSignalStorm consumes the debugger's event stream for the whole window
// and returns how many "signal N" outputs arrived plus the first terminal
// condition seen (empty if the tracee never terminated). It deliberately drains
// the entire window rather than returning on the first signal: one signal proves
// only that the stop surfaced, whereas an unbounded stream proves no progress.
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
// budget (which it must not — the control is supposed to die fast).
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
// classes the backend does (SIGTRAP absorbed, SIGURG re-delivered so the Go
// runtime keeps scheduling) and differs only in what it passes to PTRACE_CONT
// for the fatal fault: signal 0 when forward is false — byte-for-byte the
// production behaviour — and the real signal when forward is true.
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

		var res ablationResult
		start := time.Now()

		cmd := exec.Command(bin)
		cmd.Stderr = GinkgoWriter
		cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
		if err := cmd.Start(); err != nil {
			res.err = fmt.Errorf("start tracee: %w", err)
			out <- res
			return
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
			out <- res
			return
		}
		if err := syscall.PtraceSetOptions(pid, unix.PTRACE_O_EXITKILL); err != nil {
			res.err = fmt.Errorf("PTRACE_O_EXITKILL: %w", err)
			out <- res
			return
		}
		if err := syscall.PtraceCont(pid, 0); err != nil {
			res.err = fmt.Errorf("initial PTRACE_CONT: %w", err)
			out <- res
			return
		}

		deadline := time.Now().Add(window)
		for time.Now().Before(deadline) {
			tid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
			if err != nil {
				if err == syscall.ECHILD {
					res.terminated = true
				} else {
					res.err = fmt.Errorf("wait4: %w", err)
				}
				break
			}
			if ws.Exited() || ws.Signaled() {
				res.terminated = true
				res.status = ws
				break
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
				// SIGURG and friends: re-delivered, as the backend does.
				err = syscall.PtraceCont(tid, int(sig))
			}
			if err != nil && err != syscall.ESRCH {
				res.err = fmt.Errorf("PTRACE_CONT sig %d tid %d: %w", sig, tid, err)
				break
			}
		}

		res.elapsed = time.Since(start)
		out <- res
	}()

	select {
	case res := <-out:
		return res
	case <-time.After(window + 30*time.Second):
		Fail("ptrace ablation goroutine did not finish within its own window")
		return ablationResult{}
	}
}

func proofWindow() time.Duration {
	return time.Duration(envInt("BINGO_PROOF_WINDOW_MS", 5000)) * time.Millisecond
}

func proofExitBudget() time.Duration {
	return time.Duration(envInt("BINGO_PROOF_EXIT_MS", 20000)) * time.Millisecond
}
