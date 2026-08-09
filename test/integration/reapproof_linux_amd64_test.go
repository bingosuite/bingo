//go:build e2e && linux && amd64

// PROOF-ONLY, TEST-ONLY investigation of Linux/amd64 hypothesis U2:
//
//	"The main thread's PTRACE_EVENT_EXIT reports the logical exit and the engine
//	 closes down before the later *final* wait status is reaped, leaving zombie
//	 tracees behind in a long-lived server process."
//
// Nothing in this file is production code and nothing here changes production
// behaviour. It exists to answer one question empirically on a real
// linux/amd64 kernel: after EventProcessExited, does the tracee's task remain
// in state Z (defunct) for as long as the tracer process lives?
//
// Why this is not answerable by reading alone: Go's os/exec may (in principle)
// reap a never-Waited child via a finalizer, the kernel may auto-reap a traced
// task on detach, and there is always a brief legitimate window between "the
// leader was continued out of its PTRACE_EVENT_EXIT stop" and "the leader has
// actually become a zombie". The measurement therefore has to distinguish a
// TRANSIENT pre-reap window from a PERSISTENT zombie, and it has to prove the
// detector itself is sound. Both are done with explicit controls:
//
//	positive control  a plain child that is deliberately never waited for
//	                  MUST be observed as a persistent zombie (detector
//	                  can see a real leak)
//	negative control  a plain child that IS waited for MUST disappear
//	                  (detector does not report false positives, and the
//	                  observation window is long enough for a real reap)
//	subject           the debugger-launched tracee after EventProcessExited
//
// All three run the same binary in the same process, back to back, so a
// difference between them can only come from who reaps.
//
// Run: go test -tags e2e -count=1 -v -timeout 600s ./test/integration \
//	-ginkgo.label-filter=reapproof
//
// Tuning:
//
//	BINGO_PROOF_SETTLE_MS   (default 5000)  observation window per subject
//	BINGO_PROOF_CYCLES      (default 25)    launch→exit cycles in the accumulation spec

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// reapProofExitCode is the distinctive status the proof target exits with. It
// doubles as the control that the *event* still carries the real exit code —
// any candidate fix must keep this true (issue #94's regression).
const reapProofExitCode = 42

// reapProofTargetSrc publishes its own PID to the file named by argv[1] and
// then exits with reapProofExitCode. Publishing the PID from inside the target
// (rather than reaching into engine internals) keeps this test entirely
// black-box against production code.
const reapProofTargetSrc = `package main

import (
	"os"
	"strconv"
)

func main() {
	if len(os.Args) > 1 {
		_ = os.WriteFile(os.Args[1], []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	os.Exit(42)
}
`

// --- /proc observation primitives ---

// taskState is one sample of a task's kernel state.
type taskState struct {
	at      time.Duration
	present bool   // /proc/<pid>/status readable
	state   string // "Z (zombie)", "R (running)", ...
	ppid    int
}

func (s taskState) String() string {
	if !s.present {
		return fmt.Sprintf("%v=gone", s.at)
	}
	return fmt.Sprintf("%v=%s", s.at, strings.Fields(s.state)[0])
}

// readTaskState reads /proc/<pid>/status. present=false means the task is fully
// gone (reaped), which on Linux is exactly the absence of the proc entry.
func readTaskState(pid int) (state string, ppid int, present bool) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return "", 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "State:"):
			state = strings.TrimSpace(strings.TrimPrefix(line, "State:"))
		case strings.HasPrefix(line, "PPid:"):
			ppid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
		}
	}
	return state, ppid, true
}

// observe samples pid every 50ms for window and returns the full timeline. The
// timeline is what separates a transient pre-reap window (a few Z samples then
// gone) from a persistent zombie (Z for the entire window).
func observe(pid int, window time.Duration) []taskState {
	const interval = 50 * time.Millisecond
	start := time.Now()
	var samples []taskState
	for {
		elapsed := time.Since(start).Round(interval)
		state, ppid, present := readTaskState(pid)
		samples = append(samples, taskState{at: elapsed, present: present, state: state, ppid: ppid})
		if time.Since(start) >= window {
			return samples
		}
		time.Sleep(interval)
	}
}

// verdict classifies a timeline. persistentZombie means the task was in state Z
// on the FINAL sample — i.e. it survived the whole observation window without
// anyone reaping it. reaped means the proc entry was gone by the end.
type verdict struct {
	persistentZombie bool
	reaped           bool
	everZombie       bool
	finalState       string
	finalPPid        int
	timeline         string
}

func classify(samples []taskState) verdict {
	v := verdict{}
	parts := make([]string, 0, len(samples))
	for _, s := range samples {
		parts = append(parts, s.String())
		if s.present && strings.HasPrefix(s.state, "Z") {
			v.everZombie = true
		}
	}
	last := samples[len(samples)-1]
	v.reaped = !last.present
	v.persistentZombie = last.present && strings.HasPrefix(last.state, "Z")
	v.finalState = last.state
	v.finalPPid = last.ppid
	// Keep the report compact: first three, last three, and any transition.
	v.timeline = compactTimeline(parts)
	return v
}

func compactTimeline(parts []string) string {
	if len(parts) <= 8 {
		return strings.Join(parts, " ")
	}
	head := parts[:3]
	tail := parts[len(parts)-3:]
	return strings.Join(head, " ") + " … " + strings.Join(tail, " ")
}

// zombieChildren returns the PIDs of every direct child of THIS process that is
// currently a zombie. Used by the accumulation spec, where per-PID tracking is
// less interesting than "does the count grow without bound".
func zombieChildren() []int {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		state, ppid, present := readTaskState(pid)
		if present && ppid == self && strings.HasPrefix(state, "Z") {
			out = append(out, pid)
		}
	}
	sort.Ints(out)
	return out
}

// reapEverything drains every reapable child so the runner is left clean no
// matter what the spec observed. WNOHANG keeps it from ever blocking.
func reapEverything() int {
	reaped := 0
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG|syscall.WALL, nil)
		if err != nil || pid <= 0 {
			return reaped
		}
		reaped++
	}
}

func proofSettleWindow() time.Duration {
	return time.Duration(envInt("BINGO_PROOF_SETTLE_MS", 5000)) * time.Millisecond
}

// launchAndRunToExit launches bin under a real debugger, drives it to a clean
// exit, and returns the tracee's PID plus the exit code the engine reported.
// It deliberately keeps the Debugger (and therefore the engine's *exec.Cmd)
// referenced by the caller: a long-lived bingo server holds its session for far
// longer than one exit, so letting GC collect it here would test the wrong
// thing.
func launchAndRunToExit(bin, pidFile string) (debugger.Debugger, int, int) {
	GinkgoHelper()
	d := debugger.New(nil)
	Expect(d.Launch(bin, []string{pidFile}, nil)).To(Succeed(), "Launch proof target")

	awaitEvent(d.Events(), 20*time.Second, protocol.EventStepped) // entry stop
	Expect(d.Continue()).To(Succeed(), "Continue to os.Exit")

	evt := awaitEvent(d.Events(), 20*time.Second, protocol.EventProcessExited, protocol.EventError)
	Expect(evt.Kind).To(Equal(protocol.EventProcessExited),
		"expected ProcessExited, got %s: %s", evt.Kind, evt.Payload)
	var payload protocol.ProcessExitedPayload
	Expect(json.Unmarshal(evt.Payload, &payload)).To(Succeed(), "decode ProcessExited")

	raw, err := os.ReadFile(pidFile)
	Expect(err).NotTo(HaveOccurred(), "target must have published its PID")
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	Expect(err).NotTo(HaveOccurred(), "parse published PID %q", raw)

	return d, pid, payload.ExitCode
}

var _ = Describe("PROOF U2: linux/amd64 tracee reaping after PTRACE_EVENT_EXIT",
	Label("linux", "reapproof"), func() {

		var window time.Duration

		BeforeEach(func() {
			window = proofSettleWindow()
			// Start from a clean slate so a leftover from a previous spec can
			// never be mistaken for this spec's subject.
			reapEverything()
		})

		AfterEach(func() {
			AddReportEntry("cleanup-reaped", reapEverything())
		})

		It("establishes that the /proc detector can tell reaped from unreaped", func() {
			bin := buildTarget("reapproof_control", reapProofTargetSrc)
			dir := GinkgoT().TempDir()

			// NEGATIVE CONTROL: a child that IS waited for must vanish well
			// inside the observation window. If this fails, the window is too
			// short and every other result in this file is meaningless.
			reaped := exec.Command(bin, filepath.Join(dir, "neg.pid"))
			Expect(reaped.Start()).To(Succeed(), "start negative-control child")
			negPID := reaped.Process.Pid
			_ = reaped.Wait()
			negV := classify(observe(negPID, window))
			AddReportEntry("negative-control(waited)", fmt.Sprintf(
				"pid=%d reaped=%v persistentZombie=%v finalState=%q timeline=[%s]",
				negPID, negV.reaped, negV.persistentZombie, negV.finalState, negV.timeline))
			Expect(negV.reaped).To(BeTrue(),
				"NEGATIVE CONTROL FAILED: a child that was Wait()ed for is still present "+
					"after %s (state=%q). The observation window or the detector is wrong, "+
					"so no conclusion can be drawn from the subject.", window, negV.finalState)
			Expect(negV.persistentZombie).To(BeFalse(),
				"NEGATIVE CONTROL FAILED: a reaped child must not look like a zombie")

			// POSITIVE CONTROL: a child that is deliberately never waited for
			// must be observed as a persistent zombie. If this fails, the
			// detector cannot see a leak at all and a clean subject result
			// would be a false negative.
			leaked := exec.Command(bin, filepath.Join(dir, "pos.pid"))
			Expect(leaked.Start()).To(Succeed(), "start positive-control child")
			posPID := leaked.Process.Pid
			posV := classify(observe(posPID, window))
			AddReportEntry("positive-control(never waited)", fmt.Sprintf(
				"pid=%d reaped=%v persistentZombie=%v finalState=%q ppid=%d timeline=[%s]",
				posPID, posV.reaped, posV.persistentZombie, posV.finalState, posV.finalPPid, posV.timeline))
			Expect(posV.persistentZombie).To(BeTrue(),
				"POSITIVE CONTROL FAILED: a child that was never Wait()ed for did not remain "+
					"a zombie for %s (final state %q). Something else in this process is "+
					"reaping children, so a clean subject result would be a false negative.",
				window, posV.finalState)
			Expect(posV.finalPPid).To(Equal(os.Getpid()),
				"positive control zombie must still be parented to the test process")

			_ = leaked.Wait() // clean up the deliberate leak
		})

		It("does not leave the tracee as a persistent zombie after EventProcessExited", func() {
			bin := buildTarget("reapproof_target", reapProofTargetSrc)
			pidFile := filepath.Join(GinkgoT().TempDir(), "subject.pid")

			d, pid, code := launchAndRunToExit(bin, pidFile)
			defer func() { _ = d.Kill() }()

			// CONTROL: the exit code must still be correct. Any fix for the
			// reaping behaviour has to preserve this (issue #94).
			Expect(code).To(Equal(reapProofExitCode),
				"EventProcessExited must still carry the tracee's real exit code")

			// The tracer (this process) deliberately stays alive here — that is
			// the whole hypothesis: a long-lived bingo server keeps the session
			// and never reaps.
			v := classify(observe(pid, window))
			AddReportEntry("subject(debugger-launched)", fmt.Sprintf(
				"pid=%d exitCode=%d reaped=%v everZombie=%v persistentZombie=%v "+
					"finalState=%q ppid=%d tracerPID=%d timeline=[%s]",
				pid, code, v.reaped, v.everZombie, v.persistentZombie,
				v.finalState, v.finalPPid, os.Getpid(), v.timeline))

			Expect(v.persistentZombie).To(BeFalse(),
				"HYPOTHESIS U2 CONFIRMED: tracee pid=%d is still a zombie (state=%q, ppid=%d) "+
					"%s after EventProcessExited(code=%d) — the engine tore down on the main "+
					"thread's PTRACE_EVENT_EXIT and nothing ever reaped the final wait status. "+
					"timeline=[%s]",
				pid, v.finalState, v.finalPPid, window, code, v.timeline)
		})

		It("does not accumulate zombie tracees across repeated launch→exit cycles", func() {
			bin := buildTarget("reapproof_cycles", reapProofTargetSrc)
			dir := GinkgoT().TempDir()
			cycles := envInt("BINGO_PROOF_CYCLES", 25)

			// Hold every Debugger (and thus every *exec.Cmd) for the whole spec:
			// a long-lived server keeps its sessions, so nothing may be reclaimed
			// by GC mid-run.
			held := make([]debugger.Debugger, 0, cycles)
			pids := make([]int, 0, cycles)
			codes := map[int]int{}
			defer func() {
				for _, d := range held {
					_ = d.Kill()
				}
			}()

			for i := 0; i < cycles; i++ {
				pidFile := filepath.Join(dir, fmt.Sprintf("cycle%d.pid", i))
				d, pid, code := launchAndRunToExit(bin, pidFile)
				held = append(held, d)
				pids = append(pids, pid)
				codes[code]++
			}

			// One settle window for the whole batch: any legitimate transient
			// pre-reap window has long since closed for every cycle.
			time.Sleep(window)

			var stillZombie []int
			for _, pid := range pids {
				state, _, present := readTaskState(pid)
				if present && strings.HasPrefix(state, "Z") {
					stillZombie = append(stillZombie, pid)
				}
			}
			zc := zombieChildren()

			AddReportEntry("cycles", fmt.Sprintf(
				"launched=%d exitCodes=%v zombieTracees=%d zombieChildrenOfTestProcess=%d",
				cycles, codes, len(stillZombie), len(zc)))

			Expect(codes).To(Equal(map[int]int{reapProofExitCode: cycles}),
				"every cycle must report the real exit code")
			Expect(stillZombie).To(BeEmpty(),
				"HYPOTHESIS U2 CONFIRMED (accumulation): %d of %d tracees are still zombies "+
					"%s after their EventProcessExited. A long-lived server leaks one defunct "+
					"process per debug session. zombie pids=%v",
				len(stillZombie), cycles, window, stillZombie)
		})
	})
