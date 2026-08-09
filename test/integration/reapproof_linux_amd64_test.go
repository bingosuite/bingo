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
//	reap control      a debugger-launched TRACEE, killed while suspended so
//	                  Kill runs production's existing reapAfterKill path,
//	                  MUST disappear from /proc (traced children of this
//	                  process are reapable, and the detector reports "gone"
//	                  for a tracee too — so a persistent subject zombie can
//	                  only be a property of the natural-exit lifetime)
//	subject           the debugger-launched tracee after EventProcessExited,
//	                  followed by one targeted nonblocking wait4(pid, WNOHANG)
//	                  to establish that its own final status was still pending
//
// All four run the same binary in the same process, back to back, so a
// difference between them can only come from who reaps.
//
// Run: go test -tags e2e -count=1 -v -timeout 600s ./test/integration \
//	-ginkgo.label-filter=reapproof
//
// Tuning:
//
//	BINGO_PROOF_SETTLE_MS   (default 5000)  observation window per subject
//	BINGO_PROOF_CYCLES      (default 25)    launch→exit cycles in the accumulation spec
//
// SCOPE — what this file does and does not measure. Every claim below is backed
// by an assertion here; nothing else may be inferred from these results.
//
//	MEASURED    the natural-exit (os.Exit) path of a debugger-LAUNCHED tracee,
//	            on linux/amd64, in one no-race run and one race run per
//	            experiment; the reported exit code; whether the owned final
//	            status is still pending afterwards; and that the same codebase
//	            DOES reap a traced child through reapAfterKill.
//	NOT MEASURED the attach path (nothing here attaches to a pre-existing
//	            process, so no claim is made about it); retention of sibling
//	            thread tasks, file descriptors, memory or any other resource
//	            beyond the leader task's /proc entry; and any non-linux platform.

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

// reapProofDelayedTargetSrc is the multi-session variant: argv[1] pid file,
// argv[2] milliseconds to live after being resumed, argv[3] exit code. The
// delay lets several sessions be simultaneously resumed — and therefore several
// waitLoops simultaneously blocked in Wait4(-1, WALL) — when each tracee dies.
const reapProofDelayedTargetSrc = `package main

import (
	"os"
	"strconv"
	"time"
)

func main() {
	_ = os.WriteFile(os.Args[1], []byte(strconv.Itoa(os.Getpid())), 0o600)
	ms, _ := strconv.Atoi(os.Args[2])
	code, _ := strconv.Atoi(os.Args[3])
	time.Sleep(time.Duration(ms) * time.Millisecond)
	os.Exit(code)
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

// reapOwned performs exactly ONE precisely-targeted, nonblocking wait for pid.
//
// This is the load-bearing measurement for the final-reaping claim. Observing
// "state Z for the whole window" alone leaves two readings open: the status was
// still pending and nobody asked for it, or the task was for some reason not
// reapable at all. Wait4(pid, WNOHANG) settles it. WNOHANG means the call can
// never block, so if it returns pid the status was already sitting there,
// unconsumed, waiting for an owner that never came — a missing reap, not an
// unreapable child. Returning 0 would mean somebody else got there first.
func reapOwned(pid int) (int, syscall.WaitStatus, time.Duration, error) {
	var ws syscall.WaitStatus
	start := time.Now()
	got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG|syscall.WALL, nil)
	return got, ws, time.Since(start), err
}

// childrenOfSelf lists every task currently parented to this process. The
// reapAfterKill control kills its session at the entry stop, before the target's
// main() has run and published a PID file, so the tracee has to be identified by
// diffing this set across the launch. Deliberately black-box: no engine
// internals are reached into.
func childrenOfSelf() []int {
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
		if _, ppid, present := readTaskState(pid); present && ppid == self {
			out = append(out, pid)
		}
	}
	sort.Ints(out)
	return out
}

func addedChildren(before, after []int) []int {
	was := make(map[int]bool, len(before))
	for _, p := range before {
		was[p] = true
	}
	var out []int
	for _, p := range after {
		if !was[p] {
			out = append(out, p)
		}
	}
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

// awaitExitSoft drains ch for a terminal event WITHOUT failing the spec on
// timeout — the multi-session experiment needs to record "this session never
// heard about its own exit" as data rather than aborting at the first victim.
func awaitExitSoft(ch <-chan protocol.Event, timeout time.Duration) (string, int) {
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				return "channel-closed", 0
			}
			switch evt.Kind {
			case protocol.EventProcessExited:
				var p protocol.ProcessExitedPayload
				_ = json.Unmarshal(evt.Payload, &p)
				return string(evt.Kind), p.ExitCode
			case protocol.EventError:
				return string(evt.Kind) + ":" + string(evt.Payload), 0
			}
		case <-deadline:
			return "TIMEOUT", 0
		}
	}
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

			// Now claim the status explicitly, targeted at exactly this PID and
			// nonblocking. Done AFTER the observation window and BEFORE the
			// hypothesis assertion, so the evidence is recorded even though the
			// assertion below is expected to fail while the defect is present.
			zombiesBefore := zombieChildren()
			gotPID, ws, took, werr := reapOwned(pid)
			_, _, presentAfterReap := readTaskState(pid)
			AddReportEntry("subject-owned-reap", fmt.Sprintf(
				"wait4(pid=%d, WNOHANG|WALL) -> returned=%d err=%v took=%s "+
					"exited=%v exitStatus=%d signaled=%v procEntryAfterReap=%v "+
					"zombieChildrenBeforeReap=%v",
				pid, gotPID, werr, took.Round(time.Microsecond),
				ws.Exited(), ws.ExitStatus(), ws.Signaled(), presentAfterReap,
				zombiesBefore))

			// Self-diagnosis. A targeted wait on a thread-group leader returns 0
			// while any sibling task in the group is still unreaped
			// (delay_group_leader), which would be a different — and materially
			// weaker — finding than "the leader's own status was pending". If
			// that happens, record exactly what was drained so the run is
			// conclusive either way rather than merely red.
			if gotPID == 0 && werr == nil {
				drained := reapEverything()
				retryPID, retryWS, retryTook, retryErr := reapOwned(pid)
				_, _, presentAfterRetry := readTaskState(pid)
				AddReportEntry("subject-owned-reap-retry", fmt.Sprintf(
					"targeted wait returned 0; drainedSiblings=%d then wait4(pid=%d) -> "+
						"returned=%d err=%v took=%s exitStatus=%d procEntryAfterRetry=%v",
					drained, pid, retryPID, retryErr, retryTook.Round(time.Microsecond),
					retryWS.ExitStatus(), presentAfterRetry))
			}

			if v.persistentZombie {
				// These four assertions are the difference between "a zombie was
				// seen" and "the owned final status was still pending". They must
				// hold whenever the leak is real.
				Expect(werr).NotTo(HaveOccurred(),
					"wait4(%d, WNOHANG) must not error while the task is a zombie", pid)
				Expect(gotPID).To(Equal(pid),
					"exactly the subject PID must be immediately reapable: a single "+
						"nonblocking wait4(%d, WNOHANG|WALL) returned %d. The final wait "+
						"status was still pending %s after EventProcessExited, so nothing "+
						"in the engine, the backend or the Go runtime ever consumed it. "+
						"(A 0 here would instead mean the leader was still held by an "+
						"unreaped sibling task — see the subject-owned-reap-retry entry.)",
					pid, gotPID, window)
				Expect(ws.Exited()).To(BeTrue(),
					"the pending status must be a normal exit, not a signal death")
				Expect(ws.ExitStatus()).To(Equal(reapProofExitCode),
					"the pending status must carry the tracee's real exit code")
				Expect(presentAfterReap).To(BeFalse(),
					"/proc/%d must be gone once the pending status is claimed — proving the "+
						"task was reapable all along and only the reap was missing", pid)
			}

			Expect(v.persistentZombie).To(BeFalse(),
				"HYPOTHESIS U2 CONFIRMED: tracee pid=%d is still a zombie (state=%q, ppid=%d) "+
					"%s after EventProcessExited(code=%d) — the engine tore down on the main "+
					"thread's PTRACE_EVENT_EXIT and nothing ever reaped the final wait status. "+
					"A single nonblocking wait4(%d, WNOHANG|WALL) then returned %d in %s with "+
					"exit status %d, proving the owned status was pending the entire time. "+
					"timeline=[%s]",
				pid, v.finalState, v.finalPPid, window, code,
				pid, gotPID, took.Round(time.Microsecond), ws.ExitStatus(), v.timeline)
		})

		// The correctly-reaping control. It is not enough to show that a plain
		// exec'd child can be reaped (the negative control already does that):
		// the subject is a PTRACE tracee, so the question "are traced children
		// reapable by this codebase at all?" has to be answered with a traced
		// child, through a path production code already owns.
		//
		// Kill on a SUSPENDED session is exactly that path. engine.Kill captures
		// running=false (state is stateSuspended at the entry stop), which routes
		// process.kill → killProcess → reapAfterKill, and reapAfterKill drains
		// until ECHILD. If /proc/<pid> is gone afterwards, then a traced child of
		// this process IS reapable, the /proc detector does report "gone" for a
		// tracee, and the persistent zombie seen in the subject spec can only be
		// a property of the NATURAL-EXIT lifetime — not of ptrace, not of the
		// harness, not of the detector.
		It("reaps a suspended session's tracee when Kill runs the reapAfterKill path", func() {
			bin := buildTarget("reapproof_killctl", reapProofTargetSrc)
			pidFile := filepath.Join(GinkgoT().TempDir(), "killctl.pid")

			before := childrenOfSelf()
			d := debugger.New(nil)
			Expect(d.Launch(bin, []string{pidFile}, nil)).To(Succeed(), "Launch kill-control target")
			awaitEvent(d.Events(), 20*time.Second, protocol.EventStepped) // entry stop ⇒ suspended

			// The target is stopped at the execve trap, so main() has not run and
			// no PID file exists yet; identify the tracee by diffing children.
			added := addedChildren(before, childrenOfSelf())
			Expect(added).To(HaveLen(1),
				"expected exactly one new child after Launch, got %v", added)
			pid := added[0]

			stopState, stopPPid, present := readTaskState(pid)
			Expect(present).To(BeTrue(), "tracee /proc entry must exist while suspended")
			AddReportEntry("kill-control(at entry stop)", fmt.Sprintf(
				"pid=%d state=%q ppid=%d tracerPID=%d", pid, stopState, stopPPid, os.Getpid()))

			// Kill while suspended ⇒ killProcess(running=false) ⇒ reapAfterKill.
			killStart := time.Now()
			Expect(d.Kill()).To(Succeed(), "Kill a suspended session")
			killTook := time.Since(killStart)

			v := classify(observe(pid, window))
			AddReportEntry("kill-control(after Kill)", fmt.Sprintf(
				"pid=%d killReturnedIn=%s reaped=%v everZombie=%v persistentZombie=%v "+
					"finalState=%q timeline=[%s]",
				pid, killTook.Round(time.Millisecond), v.reaped, v.everZombie,
				v.persistentZombie, v.finalState, v.timeline))

			Expect(v.reaped).To(BeTrue(),
				"REAP CONTROL FAILED: a suspended session's tracee (pid=%d) killed through "+
					"reapAfterKill is still present after %s (state=%q). If traced children "+
					"were not reapable at all, the subject result would say nothing about the "+
					"natural-exit path. timeline=[%s]",
				pid, window, v.finalState, v.timeline)
			Expect(v.persistentZombie).To(BeFalse(),
				"a tracee reaped via reapAfterKill must not remain a zombie")
		})

		// NOTE ON READING THIS SPEC'S NUMBERS. The residual count here is NOT the
		// per-session leak rate, and must not be quoted as one. Because the linux
		// waitLoop waits on Wait4(-1, WALL) — process-wide, not scoped to its own
		// tracee — each new session incidentally reaps and discards the PREVIOUS
		// session's pending status (the `ws.Exited() && tid != b.pid` → continue
		// branch). So N sequential sessions in one process leave exactly ONE
		// zombie: the last one, which has no successor to clear it. That is the
		// present-day shape of the leak — one leaked zombie per idle server, not
		// one per session.
		//
		// The warning that matters for triage: that masking is a side effect of
		// the cross-session wait scope tracked in #205. If #205 is fixed by
		// isolating each session's wait namespace WITHOUT also adding a final
		// reap on the natural-exit path, the incidental cleanup disappears and
		// this leak becomes one zombie per session — unbounded in a long-lived
		// server. Final reaping must be preserved by, and verified alongside, any
		// #205 isolation fix.
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
			var zombieCycleIdx []int
			for i, pid := range pids {
				state, _, present := readTaskState(pid)
				if present && strings.HasPrefix(state, "Z") {
					stillZombie = append(stillZombie, pid)
					zombieCycleIdx = append(zombieCycleIdx, i)
				}
			}
			zc := zombieChildren()

			// Whether the survivor is precisely the LAST cycle is the signature
			// of incidental cross-session reaping: every earlier zombie was
			// cleared by the next session's process-wide Wait4, and only the
			// final session had no successor.
			onlyLastCycle := len(zombieCycleIdx) == 1 && zombieCycleIdx[0] == cycles-1

			AddReportEntry("cycles", fmt.Sprintf(
				"launched=%d exitCodes=%v zombieTracees=%d zombieCycleIndexes=%v "+
					"onlyLastCycleLeaked=%v zombieChildrenOfTestProcess=%d",
				cycles, codes, len(stillZombie), zombieCycleIdx, onlyLastCycle, len(zc)))

			Expect(codes).To(Equal(map[int]int{reapProofExitCode: cycles}),
				"every cycle must report the real exit code")
			Expect(stillZombie).To(BeEmpty(),
				"HYPOTHESIS U2 CONFIRMED (accumulation): %d of %d tracees are still zombies "+
					"%s after their EventProcessExited (cycle indexes %v, onlyLastCycle=%v). "+
					"READ THIS AS: exactly one leaked zombie per idle server today — each "+
					"earlier zombie was incidentally reaped and discarded by the NEXT "+
					"session's process-wide Wait4(-1, WALL), and only the final session had "+
					"no successor to clear it. This is NOT a per-session rate. WARNING: that "+
					"incidental cleanup is a side effect of the cross-session wait scope in "+
					"#205; isolating sessions there without also adding a final reap on the "+
					"natural-exit path turns this into one zombie per session, unbounded. "+
					"zombie pids=%v",
				len(stillZombie), cycles, window, zombieCycleIdx, onlyLastCycle, stillZombie)
		})

		// The accumulation result above is only interpretable once you know who
		// reaps the ones that DO disappear. This spec isolates that: it proves
		// that a LATER, unrelated session's waitLoop is what clears an EARLIER
		// session's zombie, because the linux backend waits on Wait4(-1, WALL)
		// — every child of the whole process, not just its own tracee — and
		// silently swallows a foreign child's exit (`ws.Exited() && tid !=
		// b.pid` → continue). That incidental cross-session reaping is what
		// masks the leak down to "one zombie per idle server" instead of "one
		// per session".
		It("shows an earlier session's zombie is cleared only by a later session's waitLoop", func() {
			bin := buildTarget("reapproof_attrib", reapProofTargetSrc)
			dir := GinkgoT().TempDir()

			dA, pidA, codeA := launchAndRunToExit(bin, filepath.Join(dir, "a.pid"))
			defer func() { _ = dA.Kill() }()
			Expect(codeA).To(Equal(reapProofExitCode))

			// Session A alone: the zombie must persist for the whole window.
			vA := classify(observe(pidA, window))
			AddReportEntry("sessionA-alone", fmt.Sprintf(
				"pid=%d persistentZombie=%v finalState=%q timeline=[%s]",
				pidA, vA.persistentZombie, vA.finalState, vA.timeline))
			Expect(vA.persistentZombie).To(BeTrue(),
				"precondition: session A's tracee must still be an unreaped zombie")

			// Now run a completely unrelated session B. Its waitLoop calls
			// Wait4(-1, WALL) and can therefore consume A's pending status.
			dB, pidB, codeB := launchAndRunToExit(bin, filepath.Join(dir, "b.pid"))
			defer func() { _ = dB.Kill() }()
			Expect(codeB).To(Equal(reapProofExitCode))

			_, _, aStillPresent := readTaskState(pidA)
			AddReportEntry("after-sessionB", fmt.Sprintf(
				"sessionA pid=%d stillPresent=%v | sessionB pid=%d", pidA, aStillPresent, pidB))

			Expect(aStillPresent).To(BeTrue(),
				"CROSS-SESSION REAP CONFIRMED: session A's zombie (pid=%d) vanished only after "+
					"an unrelated session B (pid=%d) ran. Session B's waitLoop is blocked in "+
					"Wait4(-1, WALL), so it reaps and discards foreign children — masking the "+
					"per-session leak and, more seriously, consuming wait statuses that do not "+
					"belong to it.", pidA, pidB)
		})

		// Direct consequence of the same Wait4(-1, WALL) scope: with several
		// live sessions, one session's waitLoop can absorb ANOTHER session's
		// tracee exit (the `tid != b.pid` branches continue the foreign thread
		// and loop), so the owning session never learns its process died. Each
		// session here must receive its own EventProcessExited with its own
		// exit code.
		It("delivers each concurrent session its own process-exit event", func() {
			bin := buildTarget("reapproof_multi", reapProofDelayedTargetSrc)
			dir := GinkgoT().TempDir()

			const sessions = 4
			type live struct {
				d    debugger.Debugger
				want int
			}
			running := make([]live, 0, sessions)
			defer func() {
				for _, l := range running {
					_ = l.d.Kill()
				}
			}()

			// Resume every session first, so all waitLoops are simultaneously
			// blocked in Wait4(-1, WALL) when the staggered exits land.
			for i := 0; i < sessions; i++ {
				code := 50 + i
				delay := strconv.Itoa(400 + i*300)
				d := debugger.New(nil)
				args := []string{filepath.Join(dir, fmt.Sprintf("m%d.pid", i)), delay, strconv.Itoa(code)}
				Expect(d.Launch(bin, args, nil)).To(Succeed(), "Launch session %d", i)
				awaitEvent(d.Events(), 20*time.Second, protocol.EventStepped)
				Expect(d.Continue()).To(Succeed(), "Continue session %d", i)
				running = append(running, live{d: d, want: code})
			}

			type outcome struct {
				index int
				got   string
				code  int
			}
			results := make([]outcome, 0, sessions)
			for i, l := range running {
				kind, code := awaitExitSoft(l.d.Events(), 25*time.Second)
				results = append(results, outcome{index: i, got: kind, code: code})
			}

			lost := []string{}
			for i, r := range results {
				AddReportEntry(fmt.Sprintf("session%d", i), fmt.Sprintf(
					"want=ProcessExited(code=%d) got=%s(code=%d)", running[i].want, r.got, r.code))
				if r.got != string(protocol.EventProcessExited) || r.code != running[i].want {
					lost = append(lost, fmt.Sprintf("session%d want=%d got=%s(%d)",
						i, running[i].want, r.got, r.code))
				}
			}

			Expect(lost).To(BeEmpty(),
				"CROSS-SESSION WAIT THEFT CONFIRMED: %d of %d concurrent sessions did not receive "+
					"their own EventProcessExited. Wait4(-1, WALL) is process-wide, so a session's "+
					"waitLoop can absorb another session's tracee status and the owner never learns "+
					"its process exited. detail=%v", len(lost), sessions, lost)
		})
	})
