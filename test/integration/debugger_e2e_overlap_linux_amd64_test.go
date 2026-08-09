//go:build e2e && linux && amd64

// TEMPORARY PROOF SPEC — do not merge.
//
// Question: on linux/amd64, can a SIBLING thread's software-breakpoint stop,
// delivered by Wait4(-1, WALL) while the engine is mid-step-over of ANOTHER
// thread's breakpoint, corrupt the engine's one-slot step-over state and orphan
// the breakpoint being stepped over?
//
// Mechanism under test (internal/debugger @ 0509b53):
//
//	engine.resumeFromBreakpoint (engine.go:1031) begins the step-over of BP-A:
//	    bp := e.lastBP; e.lastBP = nil
//	    e.steppingOverBP = bp          <- ONE SLOT
//	    e.bps.removeFromTable(bp)      <- A leaves byID/byAddr
//	    WriteMemory(bp.addr, original) <- A's INT3 is DISARMED in tracee memory
//	    SingleStep(tidA)               <- backend: stepping=true, stepTID=tidA
//
//	Only handleStop's StopSingleStep branch (engine.go:638-727) ever reinstalls
//	steppingOverBP. But linux ptrace stops are PER-THREAD and bingo does not stop
//	the world, so sibling threads keep running while the engine is "suspended"
//	and can be sitting in their own unreaped INT3 stops. The next
//	Wait4(-1, WALL) may therefore return a sibling's StopBreakpoint BEFORE A's
//	single-step trap; backend_linux_amd64.go:523 correctly classifies it as a
//	breakpoint (tid != stepTID).
//
//	handleStop's StopBreakpoint branch (engine.go:589-636) has NO
//	`steppingOverBP != nil` guard. It unconditionally overwrites lastBP/lastBPTID
//	and emits BreakpointHit, leaving steppingOverBP == bpA. BP-A is now disarmed
//	in memory AND absent from the table, reachable only via steppingOverBP —
//	which the very next Continue (resumeFromBreakpoint for the sibling)
//	overwrites, losing BP-A permanently.
//
// DETECTOR (no production hooks): after every stop, re-issue SetBreakpoint on
// BOTH marker lines. breakpointTable.set returns errBreakpointExists iff the
// address is still in byAddr.
//
//	error "already installed" => still armed  => healthy
//	nil error                 => ORPHANED     => bug reproduced
//
// The probe undoes itself (ClearBreakpoint) in the orphaned case so the report
// describes the engine's state, not the probe's.
//
// CORROBORATION: the engine's own debug log prints
// `StopBreakpoint ... steppingOverBP=<bool>`; the spec runs with a debug logger
// wired to GinkgoWriter, so an overlapping stop is visible as
// steppingOverBP=true directly in the CI log.
//
// NON-VACUITY: (a) after the first stop of each cycle the spec polls
// /proc/<pid>/task/*/status and requires at least two threads in ptrace
// tracing-stop, logging every thread's state and stopped PC
// (/proc/<pid>/task/<tid>/syscall); (b) every cycle must yield exactly one
// MARK_A hit and exactly `siblings` MARK_B hits, which can only happen if all
// of those threads genuinely reached their traps.
//
// TARGET SEQUENCING (learned empirically, run 31323125619/31323315712): every
// marker thread must exist and be parked BEFORE the first suspend. A thread that
// clones while the engine is suspended stops at PTRACE_EVENT_CLONE with no
// waitLoop to reap it and freezes for the whole suspend, so the target publishes
// `ready.<pid>` only once every marker thread is up, and the harness releases
// A and all siblings together while the tracee is running.
//
// Tuning:
//
//	BINGO_E2E_OVERLAP_ITERS    (default 24) cycles
//	BINGO_E2E_OVERLAP_SIBLINGS (default 4)  sibling threads parked on MARK_B

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

// overlapTargetSrc runs one "A" thread and NB "B" sibling threads, each pinned
// with runtime.LockOSThread so a marker's INT3 always faults on the same OS
// thread. Both markers are //go:noinline and place the marked statement SECOND
// in the function: Go 1.25.5 folds a function's first statement into the
// prologue's func-decl line, leaving it is_stmt=false and unresolvable by
// dwarfReader.PCForFileLine.
//
// The harness owns all sequencing through files in a shared directory:
//
//	ready.<pid>  written by the target once every marker thread is parked
//	goA.<i>      released by the harness -> thread A runs markerA
//	gateB.<i>    released by the harness -> every sibling runs markerB
//	finished     written by the target after all cycles complete
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

	var wg sync.WaitGroup
	var up int32

	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		atomic.AddInt32(&up, 1)
		for i := 0; i < iters; i++ {
			waitFile(filepath.Join(dir, "goA."+strconv.Itoa(i)))
			markerA(i)
		}
	}()

	for k := 0; k < nb; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			atomic.AddInt32(&up, 1)
			for i := 0; i < iters; i++ {
				waitFile(filepath.Join(dir, "gateB."+strconv.Itoa(i)))
				markerB(i)
			}
		}()
	}

	// Publish readiness only once every marker thread exists and is parked on
	// its first gate. A clone that happens while the debugger is suspended
	// stops the cloning thread at PTRACE_EVENT_CLONE with no waitLoop to reap
	// it, freezing the target for the whole suspend.
	for atomic.LoadInt32(&up) < int32(nb+1) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	touch(filepath.Join(dir, "ready."+strconv.Itoa(os.Getpid())))

	wg.Wait()
	touch(filepath.Join(dir, "finished"))
	os.Exit(0)
}
`

var _ = Describe("Linux amd64 sibling-breakpoint / step-over overlap (PROOF)", Label("linux"), func() {
	It("keeps both breakpoints armed when a sibling breakpoint lands mid-step-over",
		Label("overlap"), func() {
			const targetName = "overlap_target"
			lineA := markerLine(overlapTargetSrc, "// MARK_A")
			lineB := markerLine(overlapTargetSrc, "// MARK_B")
			srcFile := targetName + ".go"

			iters := envInt("BINGO_E2E_OVERLAP_ITERS", 24)
			siblings := envInt("BINGO_E2E_OVERLAP_SIBLINGS", 4)
			perCycle := siblings + 1

			coord := GinkgoT().TempDir()
			bin := buildTarget(targetName, overlapTargetSrc)

			j := &journal{}
			j.logf("target=%s lineA=%d lineB=%d iters=%d siblings=%d coord=%s",
				bin, lineA, lineB, iters, siblings, coord)

			d := newOverlapDebugger(bin, []string{coord, strconv.Itoa(iters), strconv.Itoa(siblings)})
			ev := awaitEvent(d.Events(), 20*time.Second, protocol.EventStepped)
			j.logf("launch entry stop: %s", ev.Kind)

			bpA, err := d.SetBreakpoint(srcFile, lineA)
			Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A (MARK_A)")
			bpB, err := d.SetBreakpoint(srcFile, lineB)
			Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B (MARK_B)")
			j.logf("armed bpA id=%d line=%d, bpB id=%d line=%d",
				bpA.ID, bpA.Location.Line, bpB.ID, bpB.Location.Line)

			// Resume and let the runtime bring every marker thread up. The
			// tracee must be RUNNING for this: clone events are only absorbed
			// by Backend.Wait, which only runs while a waitLoop is in flight.
			Expect(d.Continue()).To(Succeed(), "Continue to spin up marker threads")
			pid := waitForReady(coord, 40*time.Second)
			j.logf("target ready, pid=%d, all %d marker threads parked", pid, perCycle)

			var (
				orphanCycles int
				minStopped   = 1 << 30
				maxStopped   int
			)

			for i := 0; i < iters; i++ {
				// Release thread A and every sibling together, so they race to
				// their traps inside one running window. Whichever Wait4 reaps
				// first is reported; the rest sit in unreaped INT3 stops — the
				// precondition for the overlap.
				touch(filepath.Join(coord, "goA."+strconv.Itoa(i)))
				touch(filepath.Join(coord, "gateB."+strconv.Itoa(i)))

				hitsA, hitsB := 0, 0
				for k := 0; k < perCycle; k++ {
					// Cycle 0 stop 0 needs no resume: the tracee is already
					// running from the readiness Continue above.
					if i > 0 || k > 0 {
						Expect(d.Continue()).To(Succeed(),
							"Continue cycle %d stop %d", i, k)
					}

					ev = awaitEvent(d.Events(), 30*time.Second,
						protocol.EventBreakpointHit, protocol.EventProcessExited,
						protocol.EventError)
					Expect(ev.Kind).To(Equal(protocol.EventBreakpointHit),
						"cycle %d stop %d: expected BreakpointHit, got %s: %s\n%s",
						i, k, ev.Kind, ev.Payload, j.dump())

					line := hitLine(ev)
					switch line {
					case lineA:
						hitsA++
					case lineB:
						hitsB++
					default:
						Fail(fmt.Sprintf("cycle %d stop %d: stopped at unexpected line %d\n%s",
							i, k, line, j.dump()))
					}

					if k == 0 {
						// LOAD-BEARING NON-VACUITY CHECK: at least one sibling
						// must already be parked in its own tracing-stop before
						// the step-over that follows.
						states, stopped := waitForTracingStops(pid, perCycle, 10*time.Second)
						if stopped < minStopped {
							minStopped = stopped
						}
						if stopped > maxStopped {
							maxStopped = stopped
						}
						j.logf("cycle %d: first stop at line %d; tracing-stopped=%d of %d marker threads\n%s",
							i, line, stopped, perCycle, renderStates(pid, states))
						Expect(stopped).To(BeNumerically(">=", 2),
							"cycle %d: NON-VACUITY FAILED — no sibling was parked in a tracing-stop, "+
								"so the step-over that follows cannot overlap anything\n%s\n%s",
							i, renderStates(pid, states), j.dump())
					}

					// THE PROBE. Every stop is a resting point at which BOTH
					// breakpoints must still be installed. The previous resume
					// stepped one of them over; if a sibling stop was delivered
					// during that step-over, the stepped-over entry was dropped
					// from the table and never reinstalled.
					armedA := probeArmed(d, srcFile, lineA)
					armedB := probeArmed(d, srcFile, lineB)
					if !armedA || !armedB {
						orphanCycles++
						lost, lostLine := "A (MARK_A)", lineA
						if armedA {
							lost, lostLine = "B (MARK_B)", lineB
						}
						states, _ := procThreadStates(pid)
						Fail(fmt.Sprintf(
							"BUG REPRODUCED — breakpoint %s silently orphaned (cycle %d, stop %d of %d)\n\n"+
								"OBSERVED\n"+
								"  The resume that led to this stop began a step-over of a software\n"+
								"  breakpoint: engine.resumeFromBreakpoint (internal/debugger/engine.go:1031)\n"+
								"  set steppingOverBP, called bps.removeFromTable(bp) and restored the\n"+
								"  original bytes, then PTRACE_SINGLESTEPped that thread.\n"+
								"  Wait4(-1, WALL) then returned a DIFFERENT thread's pending INT3 stop\n"+
								"  (backend_linux_amd64.go:523 classifies it StopBreakpoint because\n"+
								"  tid != stepTID). handleStop's StopBreakpoint branch\n"+
								"  (engine.go:589-636) has no steppingOverBP guard, so it overwrote\n"+
								"  lastBP/lastBPTID and emitted this BreakpointHit while steppingOverBP\n"+
								"  still held the half-stepped breakpoint. Only the StopSingleStep\n"+
								"  branch reinstalls it, and it never ran.\n\n"+
								"  Probe: SetBreakpoint(%s:%d) SUCCEEDED. It must return\n"+
								"  errBreakpointExists while the entry is in breakpointTable.byAddr, so\n"+
								"  breakpoint %s is disarmed in tracee memory and gone from the table.\n"+
								"  The next Continue assigns steppingOverBP to this stop's breakpoint,\n"+
								"  destroying the last reference to it: it can never fire or be cleared\n"+
								"  again.\n\n"+
								"EXPECTED\n"+
								"  Both breakpoints remain installed across a step-over, regardless of\n"+
								"  which thread Wait4 reports next.\n\n"+
								"STATE\n"+
								"  stop line=%d  armedA=%v armedB=%v  tracing-stopped=%d\n"+
								"  cycle=%d/%d stop=%d/%d hitsA=%d hitsB=%d\n"+
								"  /proc thread states:\n%s\n"+
								"HARNESS JOURNAL\n%s",
							lost, i, k, perCycle,
							srcFile, lostLine, lost,
							line, armedA, armedB, countStopped(states),
							i, iters, k, perCycle, hitsA, hitsB,
							renderStates(pid, states), j.dump()))
					}
				}

				Expect(hitsA).To(Equal(1),
					"cycle %d: exactly one MARK_A hit (got %d)\n%s", i, hitsA, j.dump())
				Expect(hitsB).To(Equal(siblings),
					"cycle %d: exactly %d MARK_B hits (got %d)\n%s",
					i, siblings, hitsB, j.dump())
			}

			// Every cycle survived: both breakpoints still armed, target runs
			// to a clean exit, and every marker thread completed every cycle.
			Expect(probeArmed(d, srcFile, lineA)).To(BeTrue(), "BP A armed at end\n%s", j.dump())
			Expect(probeArmed(d, srcFile, lineB)).To(BeTrue(), "BP B armed at end\n%s", j.dump())

			Expect(d.Continue()).To(Succeed(), "final Continue")
			ev = awaitEvent(d.Events(), 40*time.Second,
				protocol.EventProcessExited, protocol.EventBreakpointHit, protocol.EventError)
			Expect(ev.Kind).To(Equal(protocol.EventProcessExited),
				"target runs to completion after the last cycle, got %s: %s\n%s",
				ev.Kind, ev.Payload, j.dump())
			Expect(fileExists(filepath.Join(coord, "finished"))).To(BeTrue(),
				"every marker thread completed all %d cycles", iters)

			AddReportEntry("overlap-cycles", iters)
			AddReportEntry("overlap-siblings", siblings)
			AddReportEntry("overlap-orphans", orphanCycles)
			AddReportEntry("overlap-tracing-stopped-min", minStopped)
			AddReportEntry("overlap-tracing-stopped-max", maxStopped)
			GinkgoWriter.Printf(
				"OVERLAP PROOF SUMMARY: cycles=%d siblings=%d orphans=%d stopped(min/max)=%d/%d\n",
				iters, siblings, orphanCycles, minStopped, maxStopped)
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

// probeArmed reports whether a breakpoint is still installed at file:line by
// re-issuing SetBreakpoint:
//
//	errBreakpointExists -> still in breakpointTable.byAddr -> true
//	nil error           -> the address was free, so the entry was orphaned; the
//	                       accidental new entry is cleared again so the failure
//	                       report describes the engine's state, not the probe's
//
// Any other error fails the spec: the probe itself must be trustworthy.
func probeArmed(d debugger.Debugger, file string, line int) bool {
	GinkgoHelper()
	bp, err := d.SetBreakpoint(file, line)
	if err != nil {
		Expect(err.Error()).To(ContainSubstring("already installed"),
			"probe SetBreakpoint(%s:%d) returned an unexpected error", file, line)
		return true
	}
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

// waitForReady blocks until the target publishes ready.<pid>, which it only
// does once every marker thread is created, pinned and parked on its first
// gate. Returns the tracee pid.
func waitForReady(dir string, timeout time.Duration) int {
	GinkgoHelper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dir, "ready.*"))
		if len(matches) == 1 {
			n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(matches[0]), "ready."))
			if err == nil && n > 0 {
				return n
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	Fail(fmt.Sprintf("TIMEOUT after %s waiting for the target to park every marker thread", timeout))
	return 0
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

func procThreadStates(pid int) ([]threadState, int) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, 0
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
	return out, countStopped(out)
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
	for {
		states, stopped := procThreadStates(pid)
		if stopped >= want || time.Now().After(deadline) {
			return states, stopped
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// procPC reads the stopped thread's instruction pointer from
// /proc/<pid>/task/<tid>/syscall, whose last field is the PC. It is only
// meaningful for a stopped task; "running" or an unreadable file yields "".
// Purely diagnostic: it lets a reader confirm that the parked siblings all sit
// at the same marker address.
func procPC(pid int, tid string) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/syscall", pid, tid))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return ""
	}
	return fields[len(fields)-1]
}

func renderStates(pid int, states []threadState) string {
	var b strings.Builder
	for _, s := range states {
		fmt.Fprintf(&b, "  tid=%-8s state=%-18s pc=%-20s name=%s\n",
			s.TID, s.State, procPC(pid, s.TID), s.Name)
	}
	if b.Len() == 0 {
		return "  <no /proc data>\n"
	}
	return b.String()
}

// --- harness journal ---

// journal records the harness's own actions so a failure report shows what the
// test did, in order, alongside the engine's debug log.
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
