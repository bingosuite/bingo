//go:build e2e && linux && amd64

// Regression spec for #198: a sibling thread's software-breakpoint stop must
// never land in the middle of another thread's step-over.
//
// Linux ptrace stops are PER-THREAD and bingo does not stop the world, so
// sibling threads keep running while the engine is suspended and can be sitting
// in their own unreaped INT3 stops. engine.resumeFromBreakpoint then begins a
// step-over that is only safe if it completes atomically:
//
//	bp := e.lastBP; e.lastBP = nil
//	e.steppingOverBP = bp          <- ONE SLOT
//	e.bps.removeFromTable(bp)      <- the entry leaves byID/byAddr
//	WriteMemory(bp.addr, original) <- the INT3 is DISARMED in tracee memory
//	SingleStep(tid)                <- only the resulting StopSingleStep rearms it
//
// Before the fix, a process-wide Wait4(-1, WALL) could CONSUME a sibling's
// already-pending INT3 status inside that window. handleStop's StopBreakpoint
// branch then overwrote lastBP/lastBPTID while steppingOverBP still held the
// half-stepped entry, and the next Continue overwrote that last reference: the
// breakpoint was disarmed, absent from the table, and could never fire or be
// cleared again. When the sibling had parked on the SAME address being stepped,
// the table lookup missed, the "spurious SIGTRAP" path skipped
// rewindToBreakpoint, and the thread was resumed one byte into the restored
// multi-byte instruction.
//
// WHAT THIS SPEC ASSERTS
//
// After EVERY stop, both breakpoints must still be installed. The probe
// re-issues SetBreakpoint: breakpointTable.set consults byAddr BEFORE it reads
// or writes tracee memory, so success means the entry is gone — it cannot be an
// artifact of the probe itself. A detection is corroborated read-only by
// ClearBreakpoint on the ORIGINAL id (clear consults byID before any write).
// The session must also stay live: no error, no hang, and the tracee runs to a
// clean exit with every marker thread having completed every iteration.
//
// WHAT IT DELIBERATELY DOES NOT ASSERT
//
// Not the number of hits. While a trap is lifted for a step-over, another
// thread can execute that very address and sail through it. That is an inherent
// property of software breakpoints without a stop-the-world, it predates this
// spec, and it is out of scope here — so the spec counts stops for
// non-vacuity but never requires a particular total.
//
// NON-VACUITY
//
// One pinned thread hammers MARK_A while N pinned siblings hammer MARK_B, so
// siblings also cover the same-address case. At every stop the spec snapshots
// /proc/<pid>/task/*/status plus each thread's stopped PC from
// /proc/<pid>/task/<tid>/syscall, and counts threads that are in ptrace
// tracing-stop AND parked ON a marker instruction — addresses resolved from the
// target's own DWARF, matched at addr or addr+1 since an amd64 INT3 stop leaves
// RIP one byte past the trap. A run in which that never reached two never set up
// the race and is rejected: it would prove nothing.
//
// TARGET SEQUENCING
//
// Every marker thread must exist and be parked BEFORE the first suspend: a
// thread that clones while the engine is suspended stops at PTRACE_EVENT_CLONE
// with no waitLoop to reap it and freezes for the whole suspend. The target
// therefore parks every thread on a single `start` gate and publishes
// ready.<pid> only once they are all up; the harness opens the gate while the
// tracee is still running.
//
// Tuning:
//
//	BINGO_E2E_OVERLAP_ITERS    (default 30) marker calls per thread
//	BINGO_E2E_OVERLAP_SIBLINGS (default 4)  sibling threads on MARK_B

package integration

import (
	"debug/dwarf"
	"debug/elf"
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
// thread. All of them are released together and then hammer their markers in a
// tight loop, so at any moment several are parked in unreaped INT3 stops while
// the engine steps another one off its trap.
//
// Both markers are //go:noinline and place the marked statement SECOND in the
// function: Go 1.25.5 folds a function's first statement into the prologue's
// func-decl line, leaving it is_stmt=false and unresolvable by
// dwarfReader.PCForFileLine.
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

	start := filepath.Join(dir, "start")
	var wg sync.WaitGroup
	var up int32

	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		atomic.AddInt32(&up, 1)
		waitFile(start)
		for i := 0; i < iters; i++ {
			markerA(i)
		}
	}()

	for k := 0; k < nb; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			atomic.AddInt32(&up, 1)
			waitFile(start)
			for i := 0; i < iters; i++ {
				markerB(i)
			}
		}()
	}

	// Publish readiness only once every marker thread exists and is parked on
	// the gate. A clone that happens while the debugger is suspended stops the
	// cloning thread at PTRACE_EVENT_CLONE with no waitLoop to reap it,
	// freezing the target for the whole suspend.
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

var _ = Describe("Linux amd64 sibling-breakpoint / step-over overlap", Label("linux"), func() {
	It("keeps both breakpoints armed when a sibling breakpoint lands mid-step-over",
		Label("overlap"), func() {
			const targetName = "overlap_target"
			lineA := markerLine(overlapTargetSrc, "// MARK_A")
			lineB := markerLine(overlapTargetSrc, "// MARK_B")
			srcFile := targetName + ".go"

			iters := envInt("BINGO_E2E_OVERLAP_ITERS", 30)
			siblings := envInt("BINGO_E2E_OVERLAP_SIBLINGS", 4)
			markerThreads := siblings + 1
			maxStops := markerThreads*iters + 50

			coord := GinkgoT().TempDir()
			bin := buildTarget(targetName, overlapTargetSrc)

			j := &journal{}
			j.logf("target=%s lineA=%d lineB=%d iters=%d siblings=%d coord=%s",
				bin, lineA, lineB, iters, siblings, coord)

			d := newOverlapDebugger(bin, []string{coord, strconv.Itoa(iters), strconv.Itoa(siblings)})
			ev := awaitEvent(d.Events(), 20*time.Second, protocol.EventStepped)
			j.logf("launch entry stop: %s", ev.Kind)

			// Resolve the marker addresses from the target's own DWARF so the
			// non-vacuity check can assert the parked threads really are sitting
			// on the breakpoint instructions, not merely in some tracing-stop.
			addrA := elfAddrForLine(bin, srcFile, lineA)
			addrB := elfAddrForLine(bin, srcFile, lineB)
			trapPCs := map[uint64]string{
				addrA: "MARK_A", addrA + 1: "MARK_A+1(post-INT3)",
				addrB: "MARK_B", addrB + 1: "MARK_B+1(post-INT3)",
			}
			j.logf("marker addresses from DWARF: MARK_A=0x%x MARK_B=0x%x", addrA, addrB)

			bpA, err := d.SetBreakpoint(srcFile, lineA)
			Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A (MARK_A)")
			bpB, err := d.SetBreakpoint(srcFile, lineB)
			Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B (MARK_B)")
			j.logf("armed bpA id=%d line=%d, bpB id=%d line=%d",
				bpA.ID, bpA.Location.Line, bpB.ID, bpB.Location.Line)

			// Resume so the runtime can bring every marker thread up: clone
			// events are only absorbed by Backend.Wait, which only runs while a
			// waitLoop is in flight.
			mustContinue(d, "Continue to spin up marker threads")
			pid := waitForReady(coord, 40*time.Second)
			j.logf("target ready, pid=%d, %d marker threads parked on the gate", pid, markerThreads)

			// Open the gate while the tracee is running: every marker thread is
			// released at once and they then hammer the two breakpoints, so
			// several sit in unreaped INT3 stops whenever one is being stepped.
			touch(filepath.Join(coord, "start"))

			var (
				stops        int
				hitsA, hitsB int
				maxAtTrap    int
				overlapReady int
			)

			for {
				ev = awaitEvent(d.Events(), 30*time.Second,
					protocol.EventBreakpointHit, protocol.EventProcessExited,
					protocol.EventError)
				if ev.Kind == protocol.EventProcessExited {
					j.logf("tracee exited after %d stops", stops)
					break
				}
				Expect(ev.Kind).To(Equal(protocol.EventBreakpointHit),
					"stop %d: expected BreakpointHit, got %s: %s\n%s",
					stops, ev.Kind, ev.Payload, j.dump())

				stops++
				Expect(stops).To(BeNumerically("<=", maxStops),
					"runaway stop count — the tracee should have exited by now\n%s", j.dump())

				line := hitLine(ev)
				switch line {
				case lineA:
					hitsA++
				case lineB:
					hitsB++
				default:
					Fail(fmt.Sprintf("stop %d: stopped at unexpected line %d\n%s",
						stops, line, j.dump()))
				}

				// Non-vacuity: snapshot who else is parked ON a marker right
				// now, i.e. whose INT3 stop is pending while the resume below
				// steps this thread off its trap.
				states, stopped := procThreadStates(pid)
				atTrap := countAtTrap(pid, states, trapPCs)
				if atTrap > maxAtTrap {
					maxAtTrap = atTrap
				}
				if atTrap >= 2 {
					overlapReady++
					if overlapReady <= 3 || overlapReady%25 == 0 {
						j.logf("stop %d at line %d: tracing-stopped=%d atMarkerPC=%d (overlap #%d)\n%s",
							stops, line, stopped, atTrap, overlapReady,
							renderStates(pid, states, trapPCs))
					}
				}

				// THE INVARIANT. Every stop is a resting point at which BOTH
				// breakpoints must still be installed. The resume that produced
				// this stop stepped one of them over; if a sibling stop was
				// delivered during that step-over, the stepped-over entry was
				// dropped from the table and never reinstalled.
				armedA := probeArmed(d, srcFile, lineA)
				armedB := probeArmed(d, srcFile, lineB)
				if !armedA || !armedB {
					lost, lostLine, lostID := "A (MARK_A)", lineA, bpA.ID
					if armedA {
						lost, lostLine, lostID = "B (MARK_B)", lineB, bpB.ID
					}
					// Independent, strictly READ-ONLY corroboration:
					// breakpointTable.clear looks the id up in byID before
					// touching memory, so "not found" confirms the ORIGINAL
					// entry is gone — not an artifact of the probe above.
					clearErr := d.ClearBreakpoint(lostID)
					post, _ := procThreadStates(pid)
					Fail(fmt.Sprintf(
						"breakpoint %s was silently orphaned by a sibling stop during a step-over "+
							"(stop %d, at line %d)\n\n"+
							"OBSERVED\n"+
							"  The resume that led to this stop began a step-over:\n"+
							"  engine.resumeFromBreakpoint set steppingOverBP, called\n"+
							"  bps.removeFromTable(bp), restored the original bytes and\n"+
							"  PTRACE_SINGLESTEPped that thread. A DIFFERENT thread's pending INT3\n"+
							"  stop was then reported (Wait4 classifies it StopBreakpoint because\n"+
							"  tid != stepTID), and handleStop's StopBreakpoint branch overwrote\n"+
							"  lastBP/lastBPTID while steppingOverBP still held the half-stepped\n"+
							"  breakpoint. Only StopSingleStep reinstalls it, and it never ran.\n\n"+
							"  Probe: SetBreakpoint(%s:%d) SUCCEEDED. It must return\n"+
							"  errBreakpointExists while the entry is in breakpointTable.byAddr, so\n"+
							"  breakpoint %s is disarmed in tracee memory and gone from the table.\n"+
							"  Read-only corroboration: ClearBreakpoint(id=%d) on the ORIGINAL entry\n"+
							"  returned %v.\n"+
							"  The next Continue assigns steppingOverBP to this stop's breakpoint,\n"+
							"  destroying the last reference to it: it can never fire or be cleared\n"+
							"  again.\n\n"+
							"EXPECTED\n"+
							"  Both breakpoints remain installed across a step-over, regardless of\n"+
							"  which thread Wait4 reports next.\n\n"+
							"STATE\n"+
							"  armedA=%v armedB=%v  stops=%d hitsA=%d hitsB=%d\n"+
							"  atMarkerPC(max)=%d overlapReady=%d\n"+
							"  MARK_A=0x%x MARK_B=0x%x\n"+
							"  /proc thread states:\n%s\n"+
							"HARNESS JOURNAL\n%s",
						lost, stops, line,
						srcFile, lostLine, lost, lostID, clearErr,
						armedA, armedB, stops, hitsA, hitsB,
						maxAtTrap, overlapReady, addrA, addrB,
						renderStates(pid, post, trapPCs), j.dump()))
				}

				mustContinue(d, "Continue after stop %d", stops)
			}

			// NON-VACUITY GATE: the spec only means something if the scenario it
			// claims to test actually occurred — sibling threads parked in their
			// own INT3 stops while a step-over ran.
			Expect(maxAtTrap).To(BeNumerically(">=", 2),
				"NON-VACUITY FAILED — across %d stops no two threads were ever parked ON a marker "+
					"instruction (MARK_A=0x%x MARK_B=0x%x), so no step-over could overlap a pending "+
					"sibling breakpoint stop and this run proves nothing\n%s",
				stops, addrA, addrB, j.dump())
			Expect(overlapReady).To(BeNumerically(">=", 1),
				"NON-VACUITY FAILED — not one of %d stops had the overlap precondition met\n%s",
				stops, j.dump())
			Expect(hitsA).To(BeNumerically(">=", 1), "MARK_A was hit at least once\n%s", j.dump())
			Expect(hitsB).To(BeNumerically(">=", 1), "MARK_B was hit at least once\n%s", j.dump())

			// Liveness: the tracee ran to completion with every marker thread
			// finishing every iteration, and both breakpoints survived.
			Expect(fileExists(filepath.Join(coord, "finished"))).To(BeTrue(),
				"every marker thread completed all %d iterations\n%s", iters, j.dump())

			AddReportEntry("overlap-stops", stops)
			AddReportEntry("overlap-hits", fmt.Sprintf("A=%d B=%d", hitsA, hitsB))
			AddReportEntry("overlap-at-marker-pc-max", maxAtTrap)
			AddReportEntry("overlap-ready-stops", fmt.Sprintf("%d/%d", overlapReady, stops))
			GinkgoWriter.Printf(
				"overlap summary: stops=%d hitsA=%d hitsB=%d atMarkerPC(max)=%d overlapReady=%d/%d\n",
				stops, hitsA, hitsB, maxAtTrap, overlapReady, stops)
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
// The membership check in breakpointTable.set happens BEFORE it reads or writes
// tracee memory, so the observation itself cannot be an artifact of the probe.
// Any other error fails the spec: the probe must be trustworthy.
//
// Bounded: a wedged engine must fail the spec, not hang it.
func probeArmed(d debugger.Debugger, file string, line int) bool {
	GinkgoHelper()
	type result struct {
		bp  protocol.Breakpoint
		err error
	}
	ch := make(chan result, 1)
	go func() {
		bp, err := d.SetBreakpoint(file, line)
		ch <- result{bp, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			Expect(r.err.Error()).To(ContainSubstring("already installed"),
				"probe SetBreakpoint(%s:%d) returned an unexpected error", file, line)
			return true
		}
		_ = d.ClearBreakpoint(r.bp.ID)
		return false
	case <-time.After(20 * time.Second):
		Fail(fmt.Sprintf("TIMEOUT after 20s in probe SetBreakpoint(%s:%d) — engine wedged", file, line))
		return false
	}
}

// mustContinue issues Continue with a watchdog. awaitEvent bounds the wait for
// the resulting event, but a wedged engine loop would otherwise hang inside the
// synchronous dispatch itself, with no timeout at all.
func mustContinue(d debugger.Debugger, format string, args ...any) {
	GinkgoHelper()
	ch := make(chan error, 1)
	go func() { ch <- d.Continue() }()
	select {
	case err := <-ch:
		Expect(err).NotTo(HaveOccurred(), fmt.Sprintf(format, args...))
	case <-time.After(20 * time.Second):
		Fail(fmt.Sprintf("TIMEOUT after 20s in Continue (%s) — engine wedged",
			fmt.Sprintf(format, args...)))
	}
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
// does once every marker thread is created, pinned and parked on the gate.
// Returns the tracee pid.
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

// procPC reads the stopped thread's instruction pointer from
// /proc/<pid>/task/<tid>/syscall, whose last field is the PC. It is only
// meaningful for a stopped task; "running" or an unreadable file yields 0.
func procPC(pid int, tid string) uint64 {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/syscall", pid, tid))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 2 {
		return 0
	}
	pc, err := strconv.ParseUint(strings.TrimPrefix(fields[len(fields)-1], "0x"), 16, 64)
	if err != nil {
		return 0
	}
	return pc
}

// countAtTrap counts threads that are BOTH in tracing-stop AND parked on a
// marker instruction. This is what makes the non-vacuity guard meaningful: an
// unrelated SIGURG / clone-event / group stop lands on some other PC and is not
// counted.
func countAtTrap(pid int, states []threadState, trapPCs map[uint64]string) int {
	n := 0
	for _, s := range states {
		if !s.stopped() {
			continue
		}
		if _, ok := trapPCs[procPC(pid, s.TID)]; ok {
			n++
		}
	}
	return n
}

func renderStates(pid int, states []threadState, trapPCs map[uint64]string) string {
	var b strings.Builder
	for _, s := range states {
		pc := procPC(pid, s.TID)
		at := trapPCs[pc]
		if at == "" {
			at = "-"
		}
		fmt.Fprintf(&b, "  tid=%-8s state=%-18s pc=0x%-14x at=%-20s name=%s\n",
			s.TID, s.State, pc, at, s.Name)
	}
	if b.Len() == 0 {
		return "  <no /proc data>\n"
	}
	return b.String()
}

// elfAddrForLine resolves file:line to a text address by reading the target
// binary's own DWARF line table, mirroring dwarfReader.PCForFileLine (first
// is-stmt row whose file name has the given suffix). Go targets are non-PIE, so
// the DWARF address is also the runtime address, which is what /proc reports.
// Test-side only — it exists so the non-vacuity check can name the exact
// instruction the parked threads must be sitting on.
func elfAddrForLine(bin, file string, line int) uint64 {
	GinkgoHelper()
	f, err := elf.Open(bin)
	Expect(err).NotTo(HaveOccurred(), "open target ELF")
	defer func() { _ = f.Close() }()

	data, err := f.DWARF()
	Expect(err).NotTo(HaveOccurred(), "read target DWARF")

	rd := data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagCompileUnit {
			continue
		}
		lr, err := data.LineReader(entry)
		if err != nil || lr == nil {
			continue
		}
		var le dwarf.LineEntry
		for {
			if err := lr.Next(&le); err != nil {
				break
			}
			if le.Line != line || !le.IsStmt || le.File == nil {
				continue
			}
			if strings.HasSuffix(le.File.Name, file) {
				return le.Address
			}
		}
	}
	Fail(fmt.Sprintf("no DWARF is-stmt address for %s:%d in %s", file, line, bin))
	return 0
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
