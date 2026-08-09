//go:build e2e && linux && amd64

// TEMPORARY PROOF SPEC — do not merge.
//
// Goal: prove (or refute) on a real ptrace backend that a SIBLING thread's
// software-breakpoint stop, delivered by Wait4(-1, WALL) while the engine is
// mid-step-over of ANOTHER thread's breakpoint, corrupts the engine's one-slot
// step-over state and orphans the breakpoint being stepped over.
//
// Mechanism under test (internal/debugger, read at 0509b53):
//
//	engine.resumeFromBreakpoint (engine.go:1031) starts the step-over of BP-A:
//	    bp := e.lastBP; e.lastBP = nil
//	    e.steppingOverBP = bp          <- one slot
//	    e.bps.removeFromTable(bp)      <- A leaves byID/byAddr
//	    WriteMemory(bp.addr, original) <- A's INT3 is DISARMED in memory
//	    SingleStep(tidA)               <- backend: stepping=true, stepTID=tidA
//
//	Linux ptrace stops are PER-THREAD and bingo does not stop the world, so a
//	sibling thread B can hit its own armed INT3 while the engine is suspended at
//	A. That stop sits pending (no waitLoop is running while suspended). The very
//	next Wait4(-1, WALL) can therefore return B's stop BEFORE A's single-step
//	trap. backend_linux_amd64.go:523 correctly classifies it (tid != stepTID) as
//	StopBreakpoint.
//
//	engine.handleStop's StopBreakpoint branch (engine.go:589-636) has NO
//	`steppingOverBP != nil` guard. It unconditionally does:
//	    e.lastBP = bpB; e.lastBPTID = tidB; emitBreakpointHit(bpB)
//	leaving e.steppingOverBP == bpA. bpA is now: disarmed in tracee memory,
//	absent from the breakpoint table, and reachable only through steppingOverBP
//	— which the next Continue (resumeFromBreakpoint for bpB) overwrites,
//	permanently losing it.
//
// Detection without touching production code: while suspended at B, re-issue
// SetBreakpoint(fileA, lineA). breakpointTable.set returns errBreakpointExists
// iff bpA is still in byAddr.
//
//	err != nil ("already installed")  => bpA survived  => HEALTHY ordering
//	err == nil                        => bpA orphaned  => BUG REPRODUCED
//
// Corroborating evidence: the engine's own debug log prints
// `StopBreakpoint ... steppingOverBP=true` on exactly the overlapping stop, so
// the spec runs with a debug-level logger wired to GinkgoWriter.
//
// Non-vacuity: before the load-bearing Continue, the spec waits for every B
// thread's ack file AND polls /proc/<pid>/task/*/status until at least
// (1 + NB) threads report ptrace tracing-stop ('t'/'T'). That proves A and all
// B siblings are genuinely stopped at their traps before the step-over starts.
//
// Tuning:
//
//	BINGO_E2E_OVERLAP_ITERS    (default 24) cycles
//	BINGO_E2E_OVERLAP_SIBLINGS (default 4)  sibling threads parked on BP-B

package integration

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// overlapTargetSrc drives one "A" thread and NB "B" sibling threads, every one
// of them pinned with runtime.LockOSThread so a given marker's INT3 always
// faults on the same OS thread. Both markers are //go:noinline leaves with a
// single statement line, so the breakpoint address is unambiguous.
//
// The harness owns all sequencing through files in a shared directory:
//
//	goA.<i>      released by the harness  -> thread A runs markerA and traps
//	gateB.<i>    released by the harness  -> every B thread wakes
//	ackB.<i>.<k> written by B thread k    -> proves B is past the gate and is
//	                                         about to execute markerB
//
// Thread A is started and pinned FIRST, before any B thread exists. Linux
// __ptrace_link head-inserts each auto-attached clone into the tracer's
// ->ptraced list, so wait4(-1) scans the NEWEST tracee first; making A's M the
// oldest of the marker threads biases wait4 towards reporting a pending sibling
// B stop ahead of A's single-step trap, which is the ordering under test.
const overlapTargetSrc = `package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var sink int64

//go:noinline
func markerA(i int) int {
	v := i * 2
	atomic.AddInt64(&sink, int64(v)) // MARK_A
	return v
}

//go:noinline
func markerB(i int) int {
	v := i * 3
	atomic.AddInt64(&sink, int64(v)) // MARK_B
	return v
}

func waitFile(p string) {
	for {
		if _, err := os.Stat(p); err == nil {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func touch(p string) { _ = os.WriteFile(p, []byte("x"), 0o644) }

func main() {
	dir := os.Args[1]
	iters, _ := strconv.Atoi(os.Args[2])
	nb, _ := strconv.Atoi(os.Args[3])

	// Safety net: never outlive the harness if it abandons us mid-run.
	go func() { time.Sleep(240 * time.Second); os.Exit(0) }()

	touch(filepath.Join(dir, "pid."+strconv.Itoa(os.Getpid())))

	var wg sync.WaitGroup
	ready := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		close(ready)
		for i := 0; i < iters; i++ {
			waitFile(filepath.Join(dir, "goA."+strconv.Itoa(i)))
			markerA(i)
		}
	}()
	<-ready
	time.Sleep(100 * time.Millisecond)

	for k := 0; k < nb; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			runtime.LockOSThread()
			for i := 0; i < iters; i++ {
				waitFile(filepath.Join(dir, "gateB."+strconv.Itoa(i)))
				touch(filepath.Join(dir, "ackB."+strconv.Itoa(i)+"."+strconv.Itoa(k)))
				markerB(i)
			}
		}(k)
		time.Sleep(30 * time.Millisecond)
	}

	wg.Wait()
	touch(filepath.Join(dir, "finished"))
	os.Exit(0)
}
`

var _ = Describe("Linux amd64 sibling-breakpoint / step-over overlap (PROOF)", Label("linux"), func() {
	It("keeps the stepped-over breakpoint armed when a sibling breakpoint lands mid-step-over",
		Label("overlap"), func() {
			const targetName = "overlap_target"
			lineA := markerLine(overlapTargetSrc, "// MARK_A")
			lineB := markerLine(overlapTargetSrc, "// MARK_B")
			srcFile := targetName + ".go"

			iters := envInt("BINGO_E2E_OVERLAP_ITERS", 24)
			siblings := envInt("BINGO_E2E_OVERLAP_SIBLINGS", 4)

			coord := GinkgoT().TempDir()
			bin := buildTarget(targetName, overlapTargetSrc)

			j := &journal{}
			j.logf("target=%s lineA=%d lineB=%d iters=%d siblings=%d coord=%s",
				bin, lineA, lineB, iters, siblings, coord)

			d := newOverlapDebugger(bin, []string{coord, strconv.Itoa(iters), strconv.Itoa(siblings)})
			ev := awaitEvent(d.Events(), 20*time.Second, protocol.EventStepped)
			j.logf("launch entry stop: %s", ev.Kind)

			bpA, err := d.SetBreakpoint(srcFile, lineA)
			Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A")
			Expect(bpA.Location.Line).To(Equal(lineA), "BP A resolved to MARK_A")
			bpB, err := d.SetBreakpoint(srcFile, lineB)
			Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B")
			Expect(bpB.Location.Line).To(Equal(lineB), "BP B resolved to MARK_B")
			j.logf("armed bpA id=%d line=%d, bpB id=%d line=%d",
				bpA.ID, bpA.Location.Line, bpB.ID, bpB.Location.Line)

			// The target writes pid.<pid> as its very first action, but it only
			// runs once the entry stop is released, so resolve the pid lazily
			// after the first Continue.
			pid := 0

			var (
				overlapCycles int // cycles where a sibling stop landed mid-step-over
				healthyCycles int
			)

			for i := 0; i < iters; i++ {
				// --- release thread A and stop it at MARK_A -------------------
				touch(filepath.Join(coord, "goA."+strconv.Itoa(i)))
				j.logf("cycle %d: released goA.%d, Continue -> expect BP A", i, i)
				Expect(d.Continue()).To(Succeed(), "Continue to A #%d", i)

				ev = awaitEvent(d.Events(), 25*time.Second,
					protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
				Expect(ev.Kind).To(Equal(protocol.EventBreakpointHit),
					"cycle %d: expected BreakpointHit at A, got %s: %s\n%s",
					i, ev.Kind, ev.Payload, j.dump())
				Expect(hitLine(ev)).To(Equal(lineA),
					"cycle %d: stopped at MARK_A\n%s", i, j.dump())

				if pid == 0 {
					pid = waitForPID(coord, 20*time.Second)
					j.logf("tracee pid=%d", pid)
				}

				// Same invariant, mirrored: reaching A can itself overlap the
				// step-over of the PREVIOUS cycle's sibling stop (A is running
				// while the engine single-steps a B thread off MARK_B), which
				// would orphan BP B by the identical mechanism.
				if !probeArmed(d, srcFile, lineB) {
					overlapCycles++
					post, _ := procThreadStates(pid)
					Fail(fmt.Sprintf(
						"BUG REPRODUCED (cycle-entry variant) on cycle %d/%d\n\n"+
							"Thread A's breakpoint stop was reported while the engine was still\n"+
							"mid-step-over of the previous cycle's sibling BP B. handleStop's\n"+
							"StopBreakpoint branch (internal/debugger/engine.go:589-636) overwrote\n"+
							"lastBP with BP A while steppingOverBP still held BP B, so BP B was left\n"+
							"disarmed and removed from the breakpoint table: SetBreakpoint(%s:%d)\n"+
							"succeeded where it must have returned errBreakpointExists.\n\n"+
							"cycles: overlap=%d healthy=%d\n"+
							"/proc thread states at detection:\n%s\n"+
							"harness journal:\n%s",
						i, iters, srcFile, lineB, overlapCycles, healthyCycles,
						renderStates(post), j.dump()))
				}

				// --- park every sibling on MARK_B ----------------------------
				touch(filepath.Join(coord, "gateB."+strconv.Itoa(i)))
				acks := waitForAcks(coord, i, siblings, 25*time.Second)
				Expect(acks).To(Equal(siblings),
					"cycle %d: all %d sibling threads must pass the gate (got %d acks)\n%s",
					i, siblings, acks, j.dump())

				// LOAD-BEARING SYNCHRONISATION: do not resume A until A and
				// every sibling are genuinely in ptrace tracing-stop.
				states, stopped := waitForTracingStops(pid, siblings+1, 25*time.Second)
				j.logf("cycle %d: /proc tracing-stopped=%d (want >=%d)\n%s",
					i, stopped, siblings+1, renderStates(states))
				Expect(stopped).To(BeNumerically(">=", siblings+1),
					"cycle %d: NON-VACUITY FAILED — need A + %d siblings in tracing-stop before Continue, saw %d\n%s\n%s",
					i, siblings, stopped, renderStates(states), j.dump())

				// --- the load-bearing Continue -------------------------------
				// This disarms A, PTRACE_SINGLESTEPs A, then Wait4(-1, WALL)
				// races A's step trap against the already-pending sibling stops.
				j.logf("cycle %d: Continue from A (step-over of bpA begins, %d sibling stops pending)",
					i, siblings)
				Expect(d.Continue()).To(Succeed(), "Continue from A #%d", i)

				ev = awaitEvent(d.Events(), 25*time.Second,
					protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
				Expect(ev.Kind).To(Equal(protocol.EventBreakpointHit),
					"cycle %d: expected a sibling BreakpointHit after resuming A, got %s: %s\n%s",
					i, ev.Kind, ev.Payload, j.dump())
				Expect(hitLine(ev)).To(Equal(lineB),
					"cycle %d: the post-resume stop is a sibling at MARK_B\n%s", i, j.dump())

				// --- THE PROBE ------------------------------------------------
				armed := probeArmed(d, srcFile, lineA)
				if armed {
					healthyCycles++
					j.logf("cycle %d: bpA still armed after the sibling stop (A's step trap won the wait4 race)", i)
				} else {
					overlapCycles++
					post, _ := procThreadStates(pid)
					Fail(fmt.Sprintf(
						"BUG REPRODUCED on cycle %d/%d\n\n"+
							"Observed: while the engine was mid-step-over of BP A (steppingOverBP=bpA,\n"+
							"A disarmed in memory and removed from the breakpoint table), Wait4(-1, WALL)\n"+
							"returned a SIBLING thread's pending StopBreakpoint at MARK_B. handleStop's\n"+
							"StopBreakpoint branch (internal/debugger/engine.go:589-636) has no\n"+
							"steppingOverBP guard, so it overwrote lastBP/lastBPTID with the sibling's and\n"+
							"emitted BreakpointHit. Re-issuing SetBreakpoint(%s:%d) then SUCCEEDED, which is\n"+
							"only possible if BP A is no longer in breakpointTable.byAddr — i.e. BP A has\n"+
							"been silently disarmed and orphaned. The next Continue overwrites\n"+
							"steppingOverBP with the sibling's entry, losing the only remaining reference\n"+
							"to BP A permanently.\n\n"+
							"Expected: BP A stays armed (SetBreakpoint returns errBreakpointExists).\n\n"+
							"cycles: overlap=%d healthy=%d\n"+
							"/proc thread states at detection:\n%s\n"+
							"harness journal:\n%s",
						i, iters, srcFile, lineA, overlapCycles, healthyCycles,
						renderStates(post), j.dump()))
				}

				// --- drain the remaining sibling stops -----------------------
				for k := 1; k < siblings; k++ {
					Expect(d.Continue()).To(Succeed(), "drain sibling %d cycle %d", k, i)
					ev = awaitEvent(d.Events(), 25*time.Second,
						protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
					Expect(ev.Kind).To(Equal(protocol.EventBreakpointHit),
						"cycle %d drain %d: expected BreakpointHit, got %s: %s\n%s",
						i, k, ev.Kind, ev.Payload, j.dump())
					Expect(hitLine(ev)).To(Equal(lineB),
						"cycle %d drain %d: sibling stop at MARK_B\n%s", i, k, j.dump())

					if !probeArmed(d, srcFile, lineB) {
						overlapCycles++
						post, _ := procThreadStates(pid)
						Fail(fmt.Sprintf(
							"BUG REPRODUCED (sibling-drain variant) on cycle %d drain %d\n\n"+
								"BP B was orphaned by the same one-slot steppingOverBP overwrite while\n"+
								"stepping one sibling off MARK_B with other siblings' stops still pending.\n"+
								"SetBreakpoint(%s:%d) succeeded, so BP B is gone from byAddr.\n\n"+
								"cycles: overlap=%d healthy=%d\n"+
								"/proc thread states at detection:\n%s\n"+
								"harness journal:\n%s",
							i, k, srcFile, lineB, overlapCycles, healthyCycles,
							renderStates(post), j.dump()))
					}
				}

				j.logf("cycle %d complete (overlap=%d healthy=%d)", i, overlapCycles, healthyCycles)
			}

			// Every cycle survived: both breakpoints must still be armed and the
			// target must run to a clean exit.
			Expect(probeArmed(d, srcFile, lineA)).To(BeTrue(), "BP A armed at end\n%s", j.dump())
			Expect(probeArmed(d, srcFile, lineB)).To(BeTrue(), "BP B armed at end\n%s", j.dump())

			Expect(d.Continue()).To(Succeed(), "final Continue")
			ev = awaitEvent(d.Events(), 30*time.Second,
				protocol.EventProcessExited, protocol.EventBreakpointHit, protocol.EventError)
			Expect(ev.Kind).To(Equal(protocol.EventProcessExited),
				"target runs to completion after the last cycle, got %s: %s\n%s",
				ev.Kind, ev.Payload, j.dump())
			Expect(fileExists(filepath.Join(coord, "finished"))).To(BeTrue(),
				"both marker threads completed all %d cycles", iters)

			AddReportEntry("overlap-cycles", iters)
			AddReportEntry("overlap-siblings", siblings)
			AddReportEntry("overlap-detected", overlapCycles)
			AddReportEntry("overlap-healthy-ordering", healthyCycles)
			GinkgoWriter.Printf(
				"OVERLAP PROOF SUMMARY: cycles=%d siblings=%d detected=%d healthy=%d\n",
				iters, siblings, overlapCycles, healthyCycles)
		})
})

// newOverlapDebugger launches bin with args under the real native backend and a
// debug-level logger wired to GinkgoWriter, so the engine's own
// `StopBreakpoint ... steppingOverBP=<bool>` trace lands in the CI log next to
// the harness journal. Registers a bounded Kill cleanup.
func newOverlapDebugger(bin string, args []string) debugger.Debugger {
	GinkgoHelper()
	log := slog.New(slog.NewTextHandler(GinkgoWriter, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := debugger.New(log)
	Expect(d.Launch(bin, args, nil)).To(Succeed(), "Launch overlap target")
	DeferCleanup(func() {
		done := make(chan struct{})
		go func() { _ = d.Kill(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			AddReportEntry("kill-timeout", "Kill did not return within 10s (backend may be wedged)")
		}
	})
	return d
}

// probeArmed reports whether a breakpoint is still installed at file:line, by
// re-issuing SetBreakpoint and reading the outcome:
//
//	errBreakpointExists -> still in breakpointTable.byAddr (armed)  -> true
//	nil error           -> the address was free, so it has been orphaned; the
//	                       freshly-created entry is cleared again so the probe
//	                       leaves the tracee exactly as it found it -> false
//
// Any other error fails the spec: it means the probe itself is unreliable.
func probeArmed(d debugger.Debugger, file string, line int) bool {
	GinkgoHelper()
	bp, err := d.SetBreakpoint(file, line)
	if err != nil {
		Expect(err.Error()).To(ContainSubstring("already installed"),
			"probe SetBreakpoint(%s:%d) returned an unexpected error", file, line)
		return true
	}
	// Orphaned: undo the probe's side effect so the failure report describes the
	// engine's own state, not the probe's.
	_ = d.ClearBreakpoint(bp.ID)
	return false
}

func hitLine(ev protocol.Event) int {
	GinkgoHelper()
	var hit protocol.BreakpointHitPayload
	Expect(json.Unmarshal(ev.Payload, &hit)).To(Succeed(), "decode BreakpointHit payload")
	return hit.Breakpoint.Location.Line
}

// --- coordination files ---

func touch(p string) {
	GinkgoHelper()
	Expect(os.WriteFile(p, []byte("x"), 0o600)).To(Succeed(), "touch %s", p)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// waitForPID resolves the tracee's pid from the pid.<pid> file the target
// writes as its first action.
func waitForPID(dir string, timeout time.Duration) int {
	GinkgoHelper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dir, "pid.*"))
		if len(matches) == 1 {
			n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(matches[0]), "pid."))
			if err == nil && n > 0 {
				return n
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	Fail(fmt.Sprintf("TIMEOUT after %s waiting for the target to publish its pid", timeout))
	return 0
}

// waitForAcks blocks until every sibling has written its ack for cycle i,
// returning how many were seen. An ack is written immediately before markerB,
// so it proves the sibling is past the gate and about to execute the trap.
func waitForAcks(dir string, cycle, want int, timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		got := 0
		for k := 0; k < want; k++ {
			if fileExists(filepath.Join(dir,
				fmt.Sprintf("ackB.%d.%d", cycle, k))) {
				got++
			}
		}
		if got >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// --- /proc thread-state inspection ---

type threadState struct {
	TID   string
	Name  string
	State string
}

// stopped reports whether the thread is in ptrace tracing-stop. Linux renders
// this as 't (tracing stop)' on modern kernels and 'T (tracing stop)' on older
// ones; accept both.
func (t threadState) stopped() bool {
	return strings.HasPrefix(t.State, "t") || strings.HasPrefix(t.State, "T")
}

func procThreadStates(pid int) ([]threadState, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, err
	}
	out := make([]threadState, 0, len(entries))
	for _, e := range entries {
		tid := e.Name()
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/status", pid, tid))
		if err != nil {
			continue
		}
		ts := threadState{TID: tid}
		for _, ln := range strings.Split(string(raw), "\n") {
			switch {
			case strings.HasPrefix(ln, "Name:"):
				ts.Name = strings.TrimSpace(strings.TrimPrefix(ln, "Name:"))
			case strings.HasPrefix(ln, "State:"):
				ts.State = strings.TrimSpace(strings.TrimPrefix(ln, "State:"))
			}
		}
		out = append(out, ts)
	}
	return out, nil
}

func countStopped(states []threadState) int {
	n := 0
	for _, s := range states {
		if s.stopped() {
			n++
		}
	}
	return n
}

// waitForTracingStops polls /proc until at least want threads report
// tracing-stop, returning the final snapshot and count. It never fails the
// spec — the caller asserts, so the snapshot can be included in the report.
func waitForTracingStops(pid, want int, timeout time.Duration) ([]threadState, int) {
	deadline := time.Now().Add(timeout)
	var states []threadState
	for {
		s, err := procThreadStates(pid)
		if err == nil {
			states = s
			if countStopped(states) >= want {
				return states, countStopped(states)
			}
		}
		if time.Now().After(deadline) {
			return states, countStopped(states)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func renderStates(states []threadState) string {
	var b strings.Builder
	for _, s := range states {
		fmt.Fprintf(&b, "  tid=%-8s state=%-20s name=%s\n", s.TID, s.State, s.Name)
	}
	if b.Len() == 0 {
		return "  <no /proc data>\n"
	}
	return b.String()
}

// --- harness journal ---

// journal records the harness's own actions so a failure report can show what
// the test did, in order, alongside the engine's debug log.
type journal struct {
	start time.Time
	lines []string
}

func (j *journal) logf(format string, args ...any) {
	if j.start.IsZero() {
		j.start = time.Now()
	}
	line := fmt.Sprintf("[%8.3fs] %s", time.Since(j.start).Seconds(),
		fmt.Sprintf(format, args...))
	j.lines = append(j.lines, line)
	GinkgoWriter.Println(line)
}

func (j *journal) dump() string {
	return strings.Join(j.lines, "\n")
}
