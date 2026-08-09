//go:build proofu3 && linux && amd64

// PROOF-ONLY HARNESS — hypothesis U3 (linux/amd64 cross-session wait4 isolation).
//
// NOT part of any product suite. Behind the `proofu3` build tag so it is
// invisible to `go build ./...`, `go vet ./...` and `go test ./...`. It changes
// no production code; it only drives the existing public debugger.Debugger API
// and raw wait4(2) to characterise kernel/process-wide wait semantics.
//
// Hypothesis under test
// ---------------------
// bingo runs every session in ONE server process. On linux each session owns a
// debugger engine whose backend blocks in Wait4(-1, WALL) (backend_linux_amd64.go
// Wait), and the suspended-kill path loops Wait4(-1, WALL) until ECHILD
// (reapAfterKill). wait4(2) with pid==-1 is scoped to the CALLING PROCESS (every
// thread of the thread group, since __WNOTHREAD is not passed), not to the
// caller's own tracee. Therefore:
//
//	U3-a (liveness):  reapAfterKill can never observe ECHILD while ANY other
//	                  session's tracee is still a live child, so a suspended-kill
//	                  blocks the engine loop forever.
//	U3-b (isolation): a session's Wait()/reapAfterKill can consume ANOTHER
//	                  session's ptrace stop or child death, so the owning session
//	                  never sees it.
//
// Method
// ------
// Every scenario runs in a FRESH CHILD PROCESS of the test binary (TestMain
// re-entry via BINGO_PROOF_U3_SCENARIO). A wedged engine leaks an unkillable
// goroutine blocked in wait4 and un-reaped tracees, which would poison any later
// case run in the same process; process-per-scenario gives exact cleanup (the
// parent kills the child's whole process group) and a single unambiguous exit
// code per case. Every scenario is internally deadline-bounded so the child
// always terminates on its own; the parent deadline is only a backstop.
//
// Exit-code contract (child → parent)
//
//	0  scenario completed with NO bug observed
//	1  setup failure
//	20 wedge/deadlock observed (U3-a)
//	30 stop/exit theft observed (U3-b)
//
// Tuning:
//
//	BINGO_PROOF_U3_ITERS      (default 15) race-scenario repetitions
//	BINGO_PROOF_U3_KILL_WAIT  (default 20) seconds before a Kill is declared wedged
package proofu3

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

const (
	envScenario    = "BINGO_PROOF_U3_SCENARIO"
	envLongBin     = "BINGO_PROOF_U3_LONG_BIN"
	envQuickBin    = "BINGO_PROOF_U3_QUICK_BIN"
	envChildBudget = "BINGO_PROOF_U3_CHILD_BUDGET"

	exitOK     = 0
	exitSetup  = 1
	exitWedged = 20
	exitStolen = 30
	// exitWatchdog means the scenario blocked past its whole budget in a wait
	// that nothing in-process can cancel. It is a hang, not a harness timeout.
	exitWatchdog = 40
	// exitLaunchWedged is a hang inside Launch itself (a peer consumed the new
	// tracee's execve stop). Kept distinct so a launch hang can never be
	// miscounted as evidence of exit theft.
	exitLaunchWedged = 50
)

// longTargetSrc stays alive far longer than any scenario. Used both as a
// peer tracee that is launched-but-never-resumed (a live child that will never
// produce a waitable event) and as a freely-running tracee that never exits.
const longTargetSrc = `package main

import (
	"os"
	"time"
)

func main() {
	time.Sleep(300 * time.Second)
	os.Exit(0)
}
`

// quickTargetSrc exits on its own a controllable number of milliseconds after it
// is resumed, producing exactly one process-death event for its owning session.
const quickTargetSrc = `package main

import (
	"os"
	"strconv"
	"time"
)

func main() {
	ms := 1000
	if len(os.Args) > 1 {
		if n, err := strconv.Atoi(os.Args[1]); err == nil {
			ms = n
		}
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	os.Exit(7)
}
`

var (
	longBin  string
	quickBin string
	start    = time.Now()
)

func TestMain(m *testing.M) {
	// Child (scenario) mode: run exactly one scenario and exit with its verdict.
	if s := os.Getenv(envScenario); s != "" {
		longBin = os.Getenv(envLongBin)
		quickBin = os.Getenv(envQuickBin)
		startChildWatchdog()
		os.Exit(runScenario(s))
	}

	dir, err := os.MkdirTemp("", "proofu3")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(exitSetup)
	}
	if longBin, err = buildTarget(dir, "long_target", longTargetSrc); err != nil {
		fmt.Fprintln(os.Stderr, "build long target:", err)
		os.Exit(exitSetup)
	}
	if quickBin, err = buildTarget(dir, "quick_target", quickTargetSrc); err != nil {
		fmt.Fprintln(os.Stderr, "build quick target:", err)
		os.Exit(exitSetup)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func buildTarget(dir, name, src string) (string, error) {
	srcPath := filepath.Join(dir, name+".go")
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		return "", err
	}
	binPath := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w\n%s", err, out)
	}
	return binPath, nil
}

// ---------------------------------------------------------------------------
// Parent side: spawn one child per scenario, enforce a backstop deadline.
// ---------------------------------------------------------------------------

type childResult struct {
	code   int
	output string
}

// runChild executes one scenario in a fresh child process. The child is put in
// its own process group so that on a backstop timeout the parent can SIGKILL the
// child AND every tracee it forked, leaving no strays behind.
func runChild(t *testing.T, scenario string, backstop time.Duration) childResult {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(),
		envScenario+"="+scenario,
		envLongBin+"="+longBin,
		envQuickBin+"="+quickBin,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Capture through a real file, never an io.Writer. An io.Writer makes
	// os/exec build a pipe whose write end is inherited by the child AND by
	// every tracee it forks; cmd.Wait then blocks until all of them close it,
	// so a surviving tracee would hide the child's own exit code behind the
	// parent backstop and turn a successful proof into a harness timeout.
	logFile, err := os.CreateTemp(t.TempDir(), "scenario-*.log")
	if err != nil {
		t.Fatalf("create scenario log: %v", err)
	}
	defer logFile.Close()
	readOut := func() string {
		b, _ := os.ReadFile(logFile.Name())
		return string(b)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start scenario %q: %v", scenario, err)
	}
	pgid := cmd.Process.Pid

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(backstop):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
		t.Fatalf("scenario %q exceeded the parent backstop of %s — the child failed to "+
			"self-terminate, which its internal deadlines should always guarantee.\n%s",
			scenario, backstop, readOut())
	}

	code := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("scenario %q wait: %v\n%s", scenario, waitErr, readOut())
		}
	}
	// Reap any tracee the child left behind (a wedged engine's tracees are
	// re-parented to init after the child dies, but its own group members are
	// not; kill the group unconditionally — the child itself is already gone).
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	out := readOut()
	t.Logf("scenario %q exit=%d\n%s", scenario, code, out)
	return childResult{code: code, output: out}
}

func requireCode(t *testing.T, r childResult, scenario string, want int) {
	t.Helper()
	if r.code == exitSetup {
		t.Fatalf("scenario %q failed during setup", scenario)
	}
	if r.code != want {
		t.Errorf("scenario %q: got exit %d, want %d (%s)", scenario, r.code, want, codeName(want))
	}
}

func codeName(c int) string {
	switch c {
	case exitOK:
		return "no bug observed"
	case exitWedged:
		return "wedge/deadlock observed"
	case exitStolen:
		return "theft observed"
	case exitWatchdog:
		return "child watchdog: blocked in an uncancellable wait"
	case exitLaunchWedged:
		return "Launch itself never returned"
	default:
		return "unknown"
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// --- Controls ---------------------------------------------------------------

// TestControl_SingleSession_SuspendedKill is the baseline: with exactly one
// session in the process, a suspended-kill must return promptly. It isolates the
// suspended-kill path itself from the multi-session variable.
func TestControl_SingleSession_SuspendedKill(t *testing.T) {
	r := runChild(t, "control-single-suspended-kill", 90*time.Second)
	requireCode(t, r, "control-single-suspended-kill", exitOK)
}

// TestControl_SequentialSessions_SuspendedKill proves the failure needs
// OVERLAPPING lifetimes, not merely two sessions: create+kill B, then create+kill
// A. Both must return promptly.
func TestControl_SequentialSessions_SuspendedKill(t *testing.T) {
	r := runChild(t, "control-sequential-suspended-kill", 90*time.Second)
	requireCode(t, r, "control-sequential-suspended-kill", exitOK)
}

// TestControl_SingleSession_ProcessExit is the control for the theft scenario:
// with one session, the tracee's death must always reach its owner.
func TestControl_SingleSession_ProcessExit(t *testing.T) {
	iters := envInt("BINGO_PROOF_U3_ITERS", 15)
	lost := 0
	for i := 0; i < iters; i++ {
		r := runChild(t, "control-single-process-exit", 90*time.Second)
		switch r.code {
		case exitOK:
		case exitStolen:
			lost++
		default:
			// A control that HANGS must never be laundered into a pass: that is
			// exactly the outcome this control exists to rule out.
			t.Fatalf("iteration %d: control exited %d (%s); a single-session run must "+
				"either deliver the exit or lose it, never hang or fail setup",
				i, r.code, codeName(r.code))
		}
	}
	t.Logf("CONTROL exit-delivery loss: %d/%d", lost, iters)
	if lost != 0 {
		t.Errorf("control lost %d/%d process-exit events with a single session; "+
			"the theft scenario's signal is not attributable to multi-session waiting", lost, iters)
	}
}

// --- Kernel-semantics floor -------------------------------------------------

// TestFloor_Wait4AnyChildIsProcessWide pins the kernel property the whole
// hypothesis rests on: wait4(-1) returns ECHILD only when the CALLING PROCESS
// has no children at all. Any unrelated live child — including another session's
// tracee — makes the ECHILD terminator of reapAfterKill unreachable.
func TestFloor_Wait4AnyChildIsProcessWide(t *testing.T) {
	r := runChild(t, "floor-wait4-scope", 60*time.Second)
	requireCode(t, r, "floor-wait4-scope", exitOK)
	for _, want := range []string{
		"RESULT no_children_result=ECHILD",
		"RESULT with_unrelated_child_result=BLOCKED",
	} {
		if !strings.Contains(r.output, want) {
			t.Errorf("floor scenario did not report %q", want)
		}
	}
}

// --- U3-a: suspended-kill liveness -----------------------------------------

// TestProof_SuspendedKillWedgedByLivePeerSession is the headline case. Two
// independent debugger.New sessions in one process; peer B is launched and left
// suspended (a live child that will never produce a waitable event); session A
// is launched, left suspended, and killed. A's reapAfterKill reaps A's own
// tracee and then loops on Wait4(-1) forever, because B is still a child, so
// A.Kill() never returns and A's engine loop is permanently blocked.
func TestProof_SuspendedKillWedgedByLivePeerSession(t *testing.T) {
	scenario := "proof-suspended-kill-live-peer"
	r := runChild(t, scenario, 180*time.Second)
	requireCode(t, r, scenario, exitWedged)
	if !strings.Contains(r.output, "RESULT a_tracee_reaped=true") {
		t.Errorf("expected A's own tracee to be fully reaped before the wedge "+
			"(otherwise the hang is not attributable to the peer):\n%s", r.output)
	}
	requireWedgeStack(t, r)
	if !strings.Contains(r.output, "RESULT b_still_child=true") {
		t.Errorf("expected the peer tracee to still be a child at the wedge, "+
			"which is what makes process-wide ECHILD unreachable:\n%s", r.output)
	}
}

// requireWedgeStack pins the wedge to the exact production frames under test.
// Without it any future change that blocks Kill elsewhere would still produce
// the timeout verdict and be misreported as this defect.
func requireWedgeStack(t *testing.T, r childResult) {
	t.Helper()
	for _, frame := range []string{"syscall.Wait4", "reapAfterKill"} {
		if !strings.Contains(r.output, frame) {
			t.Errorf("wedge verdict is not attributable: goroutine dump lacks %q, so the "+
				"hang was not shown to be reapAfterKill's Wait4:\n%s", frame, r.output)
		}
	}
}

// TestProof_SuspendedKillWedgedByPlainChild is the scope proof. It replaces the
// peer SESSION with an ordinary untraced child process, showing that
// reapAfterKill's ECHILD terminator is process-wide: ANY live child of the
// server wedges a suspended Kill, so the exposure is wider than "two debugger
// sessions". Same polarity as the headline proof — passing means reproduced.
func TestProof_SuspendedKillWedgedByPlainChild(t *testing.T) {
	r := runChild(t, "proof-suspended-kill-plain-child", 120*time.Second)
	requireCode(t, r, "proof-suspended-kill-plain-child", exitWedged)
	if !strings.Contains(r.output, "RESULT kill_returned=false") {
		t.Errorf("expected Kill to be wedged by an ordinary untraced child:\n%s", r.output)
	}
	if !strings.Contains(r.output, "RESULT a_tracee_reaped=true") {
		t.Errorf("expected A's own tracee reaped before the wedge:\n%s", r.output)
	}
	if !strings.Contains(r.output, "RESULT plain_child_still_child=true") {
		t.Errorf("expected the untraced peer to still be a live child at the wedge:\n%s",
			r.output)
	}
	requireWedgeStack(t, r)
}

// --- Ownership attribution (deterministic; no wait4 wake race) --------------

// TestProof_OwnershipPlainChildren removes the race from the argument. At the
// moment of measurement session A's reapAfterKill is the process's ONLY waiter,
// so the pids it consumes are named rather than inferred. It asserts three
// things a correct implementation would each falsify: Kill blocks while only
// UNOWNED children live, it survives every intermediate death and ends on the
// last (i.e. its terminator is process-wide ECHILD), and the peers' exit
// statuses were consumed by A rather than by the harness.
func TestProof_OwnershipPlainChildren(t *testing.T) {
	r := runChild(t, "proof-ownership-plain-children", 120*time.Second)
	requireCode(t, r, "proof-ownership-plain-children", exitStolen)
	mustResult(t, r, "kill_blocked_while_only_unowned_children_live=true",
		"Kill must block while the only live children belong to no session")
	mustResult(t, r, "kill_returned_after_last_peer_death=true",
		"Kill must end on the LAST unowned death, proving a process-wide ECHILD terminator")
	mustResult(t, r, "peer_statuses_consumed_by_session_a=3",
		"every unowned exit status must have been consumed by session A's wait")
	if strings.Contains(r.output, "reaped by the HARNESS just now") {
		t.Errorf("attribution broken: the harness reaped a peer itself:\n%s", r.output)
	}
}

// TestProof_OwnershipForeignTracee is the same deterministic attribution, but
// the victim is a REAL peer debugger session's tracee, with every owned tid
// recorded. Session B is suspended at entry so it has no waitLoop; session A is
// therefore the only waiter, and B's death is provably delivered to A.
// TestProof_PeerPtraceStopMakesWedgeUnrecoverable proves the wedge has no
// external escape hatch. A peer session's tracee parked in ptrace-stop cannot
// be killed by anyone but its own tracer, so it remains a live child forever
// and the process-wide ECHILD that reapAfterKill waits for can never arrive.
//
// This also demonstrates facet 4 of U3: whoever wins the wait is not
// necessarily able to act on what it wins.
func TestProof_PeerPtraceStopMakesWedgeUnrecoverable(t *testing.T) {
	r := runChild(t, "proof-peer-ptrace-stop-unrecoverable", 120*time.Second)
	requireCode(t, r, "proof-peer-ptrace-stop-unrecoverable", exitWedged)
	mustResult(t, r, "b_waitloop_armed=false",
		"the peer session must have no waitLoop, so session A is the sole waiter")
	mustResult(t, r, "a_tracee_reaped=true",
		"session A's own tracee must already be gone before we attribute the wait")
	mustResult(t, r, "kill_blocked_while_peer_session_tracee_lives=true",
		"Kill must block while another SESSION's tracee is alive")
	mustResult(t, r, "b_tracee_state_after_external_sigkill=t",
		"the peer tracee must survive an external SIGKILL, frozen in ptrace-stop")
	mustResult(t, r, "b_tracee_sigkill_pending_undelivered=true",
		"SIGKILL must be queued-but-unacted-on, which is why the tracee cannot die")
	mustResult(t, r, "b_tracee_still_child=true",
		"the peer tracee must remain a child, keeping process-wide ECHILD unreachable")
	mustResult(t, r, "kill_returned_after_external_sigkill=false",
		"the wedge must be unrecoverable by any action outside the owning session")
	mustResult(t, r, "harness_consumed_a_status=false",
		"the harness must not have consumed a status itself")
	requireWedgeStack(t, r)
}

// mustResult asserts an exact RESULT line, so a proof cannot pass on a value it
// never actually observed.
func mustResult(t *testing.T, r childResult, want, why string) {
	t.Helper()
	if !strings.Contains(r.output, "RESULT "+want) {
		t.Errorf("missing RESULT %s — %s\n%s", want, why, r.output)
	}
}

// --- U3-b: cross-session stop/exit theft ------------------------------------

// TestProof_PeerWaitLoopStealsProcessExit runs two sessions with concurrently
// armed waitLoops: A runs a tracee that never exits, B runs one that exits
// promptly. Both waitLoops block in Wait4(-1), so B's death can be delivered to
// A, which silently swallows it (tid != b.pid → continue) and leaves B waiting
// forever. This is an inherent race, so it is repeated and reported as a rate.
func TestProof_PeerWaitLoopStealsProcessExit(t *testing.T) {
	iters := envInt("BINGO_PROOF_U3_ITERS", 15)
	stolen, launchWedged, otherHang := 0, 0, 0
	for i := 0; i < iters; i++ {
		r := runChild(t, "proof-peer-waitloop-steals-exit", 120*time.Second)
		switch r.code {
		case exitSetup:
			t.Fatalf("iteration %d: setup failure", i)
		case exitOK:
		case exitStolen:
			stolen++
		case exitLaunchWedged:
			// B never got to run: that is launch theft (U3-c), a DIFFERENT
			// facet. Counting it here would let this test claim exit theft it
			// never observed, so it is only reported.
			launchWedged++
		case exitWatchdog:
			otherHang++
		default:
			t.Fatalf("iteration %d: unexpected exit %d (%s)", i, r.code, codeName(r.code))
		}
	}
	t.Logf("PROOF cross-session exit theft: %d/%d iterations where B launched, ran and "+
		"then never received its own EventProcessExited "+
		"(separately: %d iterations wedged in Launch (U3-c), %d other hangs)",
		stolen, iters, launchWedged, otherHang)
	if stolen == 0 {
		t.Errorf("no EXIT theft reproduced in %d iterations (%d launch wedges, %d other "+
			"hangs do not count) — this is a wait4 wake race; re-run or raise "+
			"BINGO_PROOF_U3_ITERS before concluding the path is safe",
			iters, launchWedged, otherHang)
	}
}

// --- U3-c: cross-session launch-stop theft ----------------------------------

// TestControl_SingleSession_Launch is the control for launch theft: one session,
// no peer waitLoop, so Launch must always complete.
func TestControl_SingleSession_Launch(t *testing.T) {
	iters := envInt("BINGO_PROOF_U3_ITERS", 15)
	for i := 0; i < iters; i++ {
		r := runChild(t, "control-single-launch", 60*time.Second)
		requireCode(t, r, "control-single-launch", exitOK)
	}
}

// TestProof_PeerWaitLoopStealsLaunchStop shows the defect can wedge a session
// before it executes anything: a peer's armed waitLoop consumes the new tracee's
// post-execve SIGTRAP, so startTracedProcess's pid-specific Wait4 — and with it
// Launch itself — never returns.
func TestProof_PeerWaitLoopStealsLaunchStop(t *testing.T) {
	iters := envInt("BINGO_PROOF_U3_ITERS", 15)
	wedged := 0
	for i := 0; i < iters; i++ {
		r := runChild(t, "proof-peer-waitloop-steals-launch", 120*time.Second)
		switch r.code {
		case exitSetup:
			t.Fatalf("iteration %d: setup failure", i)
		case exitOK:
		case exitWedged, exitLaunchWedged, exitWatchdog:
			wedged++
		default:
			t.Fatalf("iteration %d: unexpected exit %d (%s)", i, r.code, codeName(r.code))
		}
	}
	t.Logf("PROOF cross-session launch-stop theft: %d/%d launches never returned", wedged, iters)
	if wedged == 0 {
		t.Errorf("no launch wedge reproduced in %d iterations — this is a wait4 wake race; "+
			"re-run or raise BINGO_PROOF_U3_ITERS before concluding the path is safe", iters)
	}
}

// TestProof_ReapAfterKillVersusPeerDeath combines both halves: A is
// suspended-killed (its reapAfterKill is parked in Wait4(-1)) while peer session
// B runs a tracee that is about to exit. Either A's Kill stays wedged (U3-a) or
// it terminates only by consuming B's death, which B then never sees (U3-b).
// Both outcomes are defects; the scenario reports which one occurred.
func TestProof_ReapAfterKillVersusPeerDeath(t *testing.T) {
	iters := envInt("BINGO_PROOF_U3_ITERS", 15)
	wedged, stolen, clean, launchWedged := 0, 0, 0, 0
	for i := 0; i < iters; i++ {
		r := runChild(t, "proof-reapafterkill-vs-peer-death", 120*time.Second)
		switch r.code {
		case exitSetup:
			t.Fatalf("iteration %d: setup failure", i)
		case exitWedged, exitWatchdog:
			wedged++
		case exitStolen:
			stolen++
		case exitLaunchWedged:
			launchWedged++
		case exitOK:
			clean++
		default:
			t.Fatalf("iteration %d: unexpected exit %d (%s)", i, r.code, codeName(r.code))
		}
	}
	t.Logf("PROOF reapAfterKill vs peer death: wedged=%d stolen=%d clean=%d "+
		"(separately: %d wedged in Launch) (of %d)",
		wedged, stolen, clean, launchWedged, iters)
	if wedged+stolen == 0 {
		t.Errorf("neither wedge nor theft reproduced in %d iterations "+
			"(%d launch wedges do not count)", iters, launchWedged)
	}
}

// ---------------------------------------------------------------------------
// Child side: the scenarios.
// ---------------------------------------------------------------------------

func runScenario(name string) int {
	switch name {
	case "control-single-suspended-kill":
		return scenarioSingleSuspendedKill()
	case "control-sequential-suspended-kill":
		return scenarioSequentialSuspendedKill()
	case "control-single-process-exit":
		return scenarioSingleProcessExit()
	case "floor-wait4-scope":
		return scenarioWait4Scope()
	case "proof-suspended-kill-live-peer":
		return scenarioSuspendedKillLivePeer()
	case "proof-suspended-kill-plain-child":
		return scenarioSuspendedKillPlainChild()
	case "proof-ownership-plain-children":
		return scenarioOwnershipPlainChildren()
	case "proof-peer-ptrace-stop-unrecoverable":
		return scenarioPeerPtraceStopMakesWedgeUnrecoverable()
	case "proof-peer-waitloop-steals-exit":
		return scenarioPeerWaitLoopStealsExit()
	case "proof-reapafterkill-vs-peer-death":
		return scenarioReapAfterKillVsPeerDeath()
	case "proof-peer-waitloop-steals-launch":
		return scenarioPeerWaitLoopStealsLaunch()
	case "control-single-launch":
		return scenarioSingleLaunch()
	default:
		tracef("unknown scenario %q", name)
		return exitSetup
	}
}

func killWait() time.Duration {
	return time.Duration(envInt("BINGO_PROOF_U3_KILL_WAIT", 20)) * time.Second
}

func tracef(format string, args ...any) {
	fmt.Printf("[%7.3fs] %s\n", time.Since(start).Seconds(), fmt.Sprintf(format, args...))
}

func result(k string, v any) { fmt.Printf("RESULT %s=%v\n", k, v) }

// launchSuspended creates a session, launches bin, and returns the session plus
// the PID of the tracee it forked. The tracee is left at its entry stop: a live
// child of this process with NO pending waitable event and NO waitLoop.
func launchSuspended(bin string, args ...string) (debugger.Debugger, int, error) {
	before := childPIDs()
	d := debugger.New(nil)
	if err := d.Launch(bin, args, nil); err != nil {
		return nil, 0, err
	}
	pid := 0
	for _, p := range diffPIDs(before, childPIDs()) {
		pid = p
	}
	if pid == 0 {
		return nil, 0, errors.New("could not identify the launched tracee PID")
	}
	return d, pid, nil
}

// launchResult carries a bounded launch outcome. Bounding matters because a
// peer session's waitLoop can consume the new tracee's execve SIGTRAP, leaving
// startTracedProcess's pid-specific Wait4 blocked forever — Launch itself hangs.
type launchResult struct {
	dbg debugger.Debugger
	pid int
	err error
}

// launchSuspendedTimed bounds launchSuspended so a stolen execve stop is
// reported as a verdict rather than hanging the scenario. The launch goroutine
// is deliberately abandoned on timeout: it is blocked in a kernel wait that
// nothing in-process can cancel, which is precisely the defect being measured.
func launchSuspendedTimed(bin string, d time.Duration, args ...string) (launchResult, bool) {
	ch := make(chan launchResult, 1)
	go func() {
		dbg, pid, err := launchSuspended(bin, args...)
		ch <- launchResult{dbg: dbg, pid: pid, err: err}
	}()
	select {
	case r := <-ch:
		return r, true
	case <-time.After(d):
		return launchResult{}, false
	}
}

// startChildWatchdog guarantees the child self-terminates. Every scenario is
// individually deadline-bounded, but the defect under test can block a call in
// an uncancellable kernel wait, so this is the last-resort backstop that turns
// "parent had to SIGKILL me" into a reportable verdict with a goroutine dump.
func startChildWatchdog() {
	budget := time.Duration(envInt(envChildBudget, 75)) * time.Second
	go func() {
		time.Sleep(budget)
		tracef("child watchdog fired after %s — the scenario blocked in an "+
			"uncancellable wait", budget)
		dumpBlockedGoroutines()
		os.Exit(exitWatchdog)
	}()
}

// childPIDs returns every process whose real parent is this process — i.e. every
// tracee any session in this process forked.
func childPIDs() map[int]bool {
	out := map[int]bool{}
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppidOf(pid) == self {
			out[pid] = true
		}
	}
	return out
}

func diffPIDs(before, after map[int]bool) []int {
	var added []int
	for pid := range after {
		if !before[pid] {
			added = append(added, pid)
		}
	}
	return added
}

func ppidOf(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PPid:"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return -1
			}
			return n
		}
	}
	return -1
}

// procState returns the single-letter scheduler state of pid ("Z" zombie,
// "t" tracing-stop, ...) or "gone" once the entry has been reaped away.
func procState(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		// Only a genuinely absent /proc entry means reaped. Any other error
		// (EACCES, EIO) must not be laundered into "gone", or a_tracee_reaped
		// would assert something the harness never observed.
		if errors.Is(err, fs.ErrNotExist) {
			return "gone"
		}
		return "unreadable:" + err.Error()
	}
	// Fields after the (comm) parenthesis: state is the first.
	if i := strings.LastIndex(string(b), ") "); i >= 0 {
		f := strings.Fields(string(b)[i+2:])
		if len(f) > 0 {
			return f[0]
		}
	}
	return "?"
}

func threadCount(pid int) int {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return 0
	}
	return len(entries)
}

// awaitReaped polls until pid has left the process table entirely (reaped, not
// merely a zombie). Used to establish that a kill's OWN tracee is gone before we
// attribute any remaining hang to a peer session.
func awaitReaped(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if procState(pid) == "gone" {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// threadTIDs enumerates every kernel task of pid. Recording this set BEFORE a
// process dies is what lets the harness name the exact tids a foreign wait must
// have consumed.
func threadTIDs(pid int) []int {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, n)
		}
	}
	sort.Ints(tids)
	return tids
}

// reapedBySomeoneElse reports whether pid's exit status has already been
// consumed by another waiter in this process. A non-blocking wait4 on the exact
// pid is the discriminator: ECHILD means the status is gone (somebody reaped
// it), a returned pid means WE just reaped it — which would invalidate any
// claim that the session under test did.
func reapedBySomeoneElse(pid int) (string, bool) {
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG|syscall.WALL, nil)
	switch {
	case isNoChild(err):
		return "ECHILD (status already consumed by another waiter)", true
	case err != nil:
		return "wait4 error: " + err.Error(), false
	case wpid == 0:
		return "still live (no status available)", false
	default:
		return fmt.Sprintf("reaped by the HARNESS just now (pid=%d) — not attributable", wpid), false
	}
}

func isNoChild(err error) bool { return errors.Is(err, syscall.ECHILD) }

// sigkillPending reports whether SIGKILL is queued-but-undelivered for pid.
// A task sitting in ptrace-stop accepts the signal into its pending mask but
// does not act on it until its TRACER restarts it, which is what makes such a
// tracee unkillable from outside its owning session.
func sigkillPending(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "ShdPnd:") && !strings.HasPrefix(line, "SigPnd:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		mask, err := strconv.ParseUint(f[1], 16, 64)
		if err != nil {
			continue
		}
		if mask&(1<<(uint(syscall.SIGKILL)-1)) != 0 {
			return true
		}
	}
	return false
}

// hasPendingStatus reports whether pid still has an unconsumed wait status.
// Used to tell "another waiter already took it" from "there was never one".
func hasPendingStatus(pid int) (string, bool) {
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG|syscall.WALL|syscall.WUNTRACED, nil)
	switch {
	case isNoChild(err):
		return "ECHILD (not a child any more)", false
	case err != nil:
		return "wait4 error: " + err.Error(), false
	case wpid == 0:
		return "live child, no wait status available", false
	default:
		return fmt.Sprintf("status available and consumed by the HARNESS (pid=%d)", wpid), true
	}
}

// startUntracedChild spawns a plain child this process never waits on, so its
// only possible reaper is whatever else calls wait4 in this process.
func startUntracedChild() (int, error) {
	c := exec.Command(longBin)
	if err := c.Start(); err != nil {
		return 0, err
	}
	return c.Process.Pid, nil
}

// killAsync runs d.Kill() on its own goroutine so a wedged kill can be observed
// rather than hanging the scenario.
func killAsync(d debugger.Debugger) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- d.Kill() }()
	return ch
}

// drain consumes a session's event channel, recording whether a process-exit was
// ever delivered. Sessions must be drained or the engine's bounded event buffer
// backs up and changes the timing under test.
// drain consumes a session's event stream, reporting whether it ever saw its
// tracee exit and counting any stop event it received. The stop counter exists
// because a foreign tid reaching Wait's cause==0 branch is reported as THIS
// session's breakpoint hit; a session whose own tracee is untouched must never
// see one.
func drain(d debugger.Debugger, sawExit chan<- struct{}, foreignStops *atomic.Int64) {
	go func() {
		notified := false
		for evt := range d.Events() {
			switch evt.Kind {
			case protocol.EventProcessExited:
				if !notified {
					notified = true
					select {
					case sawExit <- struct{}{}:
					default:
					}
				}
			case protocol.EventBreakpointHit, protocol.EventStepped, protocol.EventPaused:
				if foreignStops != nil {
					foreignStops.Add(1)
				}
			}
		}
	}()
}

func dumpBlockedGoroutines() {
	fmt.Println("--- goroutine dump (expect a wait4 frame under reapAfterKill) ---")
	_ = pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
	fmt.Println("--- end goroutine dump ---")
}

// --- scenario bodies --------------------------------------------------------

func scenarioSingleSuspendedKill() int {
	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
	tracef("A launched, tracee pid=%d state=%s threads=%d", aPID, procState(aPID), threadCount(aPID))
	result("a_pid", aPID)
	result("peer_sessions", 0)

	t0 := time.Now()
	select {
	case err := <-killAsync(a):
		tracef("A.Kill returned after %s (err=%v)", time.Since(t0), err)
		result("kill_returned", true)
		result("kill_ms", time.Since(t0).Milliseconds())
		result("a_tracee_reaped", awaitReaped(aPID, 5*time.Second))
		return exitOK
	case <-time.After(killWait()):
		tracef("A.Kill DID NOT return within %s", killWait())
		result("kill_returned", false)
		dumpBlockedGoroutines()
		return exitWedged
	}
}

func scenarioSequentialSuspendedKill() int {
	for i, label := range []string{"B", "A"} {
		d, pid, err := launchSuspended(longBin)
		if err != nil {
			tracef("launch %s: %v", label, err)
			return exitSetup
		}
		tracef("%s launched, tracee pid=%d", label, pid)
		t0 := time.Now()
		select {
		case err := <-killAsync(d):
			tracef("%s.Kill returned after %s (err=%v)", label, time.Since(t0), err)
			result(fmt.Sprintf("kill_%d_ms", i), time.Since(t0).Milliseconds())
		case <-time.After(killWait()):
			tracef("%s.Kill DID NOT return within %s", label, killWait())
			result("kill_returned", false)
			dumpBlockedGoroutines()
			return exitWedged
		}
	}
	result("kill_returned", true)
	return exitOK
}

func scenarioSingleProcessExit() int {
	sawExit := make(chan struct{}, 1)
	b, bPID, err := launchSuspended(quickBin, "600")
	if err != nil {
		tracef("launch B: %v", err)
		return exitSetup
	}
	drain(b, sawExit, nil)
	tracef("B launched, tracee pid=%d", bPID)
	if err := b.Continue(); err != nil {
		tracef("B.Continue: %v", err)
		return exitSetup
	}
	select {
	case <-sawExit:
		tracef("B observed its tracee's exit")
		result("b_exit_seen", true)
		return exitOK
	case <-time.After(20 * time.Second):
		tracef("B never observed its tracee's exit (state=%s)", procState(bPID))
		result("b_exit_seen", false)
		dumpBlockedGoroutines()
		return exitStolen
	}
}

// scenarioWait4Scope characterises wait4(-1) scope with no debugger involved:
// ECHILD is a property of the CALLING PROCESS having no children at all.
func scenarioWait4Scope() int {
	// Phase 1: no children whatsoever → ECHILD must be immediate.
	res := make(chan error, 1)
	go func() {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		res <- err
	}()
	select {
	case err := <-res:
		if !errors.Is(err, syscall.ECHILD) {
			tracef("phase 1: unexpected wait4 error %v", err)
			result("no_children_result", "UNEXPECTED")
			return exitSetup
		}
		tracef("phase 1: wait4(-1) with no children → ECHILD")
		result("no_children_result", "ECHILD")
	case <-time.After(5 * time.Second):
		tracef("phase 1: wait4(-1) blocked with no children (unexpected)")
		result("no_children_result", "BLOCKED")
		return exitSetup
	}

	// Phase 2: one unrelated, non-traced child → wait4(-1) must block, never
	// ECHILD. This is exactly the state reapAfterKill is left in once its own
	// tracee is reaped but another session's tracee is still alive.
	cmd := exec.Command(longBin)
	if err := cmd.Start(); err != nil {
		tracef("phase 2: start unrelated child: %v", err)
		return exitSetup
	}
	peer := cmd.Process.Pid
	tracef("phase 2: unrelated child pid=%d started (never waited on)", peer)

	res2 := make(chan error, 1)
	go func() {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		res2 <- err
	}()
	select {
	case err := <-res2:
		tracef("phase 2: wait4(-1) returned early: %v", err)
		result("with_unrelated_child_result", "RETURNED")
		_ = cmd.Process.Kill()
		return exitSetup
	case <-time.After(5 * time.Second):
		tracef("phase 2: wait4(-1) still blocked after 5s with one live child → ECHILD unreachable")
		result("with_unrelated_child_result", "BLOCKED")
	}

	// Phase 3: the blocked waiter is genuinely watching that child — killing it
	// releases the wait. This proves phase 2 was a real block, not a lost wakeup.
	_ = cmd.Process.Kill()
	select {
	case err := <-res2:
		tracef("phase 3: wait4(-1) released by the peer's death (err=%v)", err)
		result("released_by_peer_death", true)
	case <-time.After(10 * time.Second):
		tracef("phase 3: wait4(-1) never released")
		result("released_by_peer_death", false)
		return exitSetup
	}
	return exitOK
}

// scenarioSuspendedKillLivePeer — U3-a headline proof.
// scenarioSingleLaunch is the control for launch theft: with one session in the
// process there is no peer waitLoop, so Launch must always complete promptly.
func scenarioSingleLaunch() int {
	r, ok := launchSuspendedTimed(longBin, 20*time.Second)
	result("launch_completed", ok)
	if !ok {
		tracef("single-session Launch did not complete within 20s")
		return exitWedged
	}
	if r.err != nil {
		tracef("launch: %v", r.err)
		return exitSetup
	}
	tracef("single-session Launch completed, tracee pid=%d", r.pid)
	_ = syscall.Kill(r.pid, syscall.SIGKILL)
	return exitOK
}

// scenarioPeerWaitLoopStealsLaunch — U3-c: a peer's waitLoop consumes a NEW
// session's execve stop, so Launch itself never returns.
//
// A is launched and continued, so its waitLoop is parked in Wait4(-1, WALL)
// with nothing of its own to receive. B is then launched: the kernel delivers
// B's post-execve SIGTRAP to whichever waiter it wakes, and A's process-wide
// wait is eligible. When A wins, startTracedProcess's pid-specific Wait4 for B
// blocks forever and B.Launch never returns — a brand new session is wedged by
// an unrelated one before it ever runs a line of user code.
func scenarioPeerWaitLoopStealsLaunch() int {
	var aStops atomic.Int64
	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
	aExit := make(chan struct{}, 1)
	drain(a, aExit, &aStops)
	if err := a.Continue(); err != nil {
		tracef("A.Continue: %v", err)
		return exitSetup
	}
	// Let A's runtime finish spawning threads so its own clone traffic is over
	// and the only contested event is B's execve stop.
	time.Sleep(700 * time.Millisecond)
	tracef("peer A running, tracee pid=%d threads=%d", aPID, threadCount(aPID))
	result("a_pid", aPID)

	t0 := time.Now()
	r, ok := launchSuspendedTimed(longBin, killWait())
	result("launch_completed", ok)
	if !ok {
		tracef("B.Launch DID NOT return within %s while peer A's waitLoop is armed",
			killWait())
		result("a_saw_a_process_exit", len(aExit) > 0)
		// A's own tracee never stops or exits, so any stop A reports is an
		// event that belonged to B.
		result("a_foreign_stops", aStops.Load())
		dumpBlockedGoroutines()
		return exitWedged
	}
	if r.err != nil {
		tracef("B.Launch failed: %v", r.err)
		return exitSetup
	}
	tracef("B.Launch completed in %s, tracee pid=%d — no theft this iteration",
		time.Since(t0), r.pid)
	result("b_pid", r.pid)
	_ = syscall.Kill(r.pid, syscall.SIGKILL)
	_ = syscall.Kill(aPID, syscall.SIGKILL)
	return exitOK
}

// scenarioOwnershipPlainChildren is the DETERMINISTIC ownership proof. It
// removes the wait4 wake race entirely: at the moment of measurement, session
// A's reapAfterKill is the ONLY waiter in the process (A's own tracee is
// already reaped; the peers are untraced children nobody waits on). Therefore
// every status the process receives is necessarily returned to A, and the
// harness can name the exact pids A consumed instead of inferring theft from a
// timeout.
//
// It also shows the terminator is process-wide ECHILD rather than "my tracee is
// gone": Kill stays blocked as each peer dies and returns only after the LAST
// one is reaped.
func scenarioOwnershipPlainChildren() int {
	const peers = 3
	pids := make([]int, 0, peers)
	for i := 0; i < peers; i++ {
		pid, err := startUntracedChild()
		if err != nil {
			tracef("start untraced peer %d: %v", i, err)
			return exitSetup
		}
		pids = append(pids, pid)
	}
	result("peer_pids", fmt.Sprint(pids))
	result("peer_pids_traced_by_any_session", false)

	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
	aTIDs := threadTIDs(aPID)
	result("a_tracee_pid", aPID)
	result("a_owned_tids", fmt.Sprint(aTIDs))

	killed := killAsync(a)
	if !awaitReaped(aPID, 15*time.Second) {
		tracef("A's own tracee never reaped; cannot attribute the wait")
		return exitSetup
	}
	result("a_tracee_reaped", true)

	// A owns nothing that is still alive, yet Kill must not return: the only
	// thing left for its Wait4(-1, WALL) to find are pids owned by nobody.
	select {
	case err := <-killed:
		tracef("A.Kill returned early (err=%v) — no wedge, defect not reproduced", err)
		result("kill_returned_before_peers_died", true)
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		return exitOK
	case <-time.After(3 * time.Second):
		result("kill_blocked_while_only_unowned_children_live", true)
	}

	// Release the peers one at a time. Each death is a status only A can
	// receive. Kill must survive every intermediate death and end on the last.
	for i, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		last := i == len(pids)-1
		select {
		case err := <-killed:
			if !last {
				tracef("A.Kill returned after peer %d/%d died — expected it to keep waiting",
					i+1, len(pids))
				result("kill_returned_after_peer_index", i)
				return exitSetup
			}
			tracef("A.Kill returned only after the LAST unowned child died (err=%v)", err)
			result("kill_returned_after_last_peer_death", true)
		case <-time.After(4 * time.Second):
			if last {
				tracef("A.Kill still blocked after every peer died — different failure mode")
				result("kill_returned_after_last_peer_death", false)
				dumpBlockedGoroutines()
				return exitWedged
			}
			result(fmt.Sprintf("kill_still_blocked_after_peer_%d_death", i), true)
		}
	}

	// Attribution: every peer status was consumed by A's loop, not by us.
	consumed := 0
	for _, pid := range pids {
		why, ok := reapedBySomeoneElse(pid)
		result(fmt.Sprintf("peer_%d_status", pid), why)
		if ok {
			consumed++
		}
	}
	result("peer_statuses_consumed_by_session_a", consumed)
	result("peer_count", len(pids))
	tracef("session A consumed %d/%d statuses for pids it does not own: %v",
		consumed, len(pids), pids)
	return exitStolen
}

// scenarioPeerPtraceStopMakesWedgeUnrecoverable is the strongest liveness
// result in this harness, and it involves NO race at all.
//
// Session B is launched and left suspended at its entry stop. Its tracee is
// therefore parked in ptrace-stop, and B's entry status was already consumed by
// Launch's pid-specific wait — so the tracee has no pending wait status and can
// generate no new one. Session A is then suspended-killed, parking reapAfterKill
// in Wait4(-1, WALL) as the process's only waiter.
//
// The operator's natural rescue — SIGKILL the offending peer tracee from
// outside — provably CANNOT work: a task in ptrace-stop takes SIGKILL into its
// pending mask but does not act on it until its own TRACER restarts it, and its
// tracer is session B, which is not going to. So B's tracee stays a live child
// forever, process-wide ECHILD is unreachable forever, and A's Kill (running on
// A's engine loop goroutine) is permanently unrecoverable.
func scenarioPeerPtraceStopMakesWedgeUnrecoverable() int {
	bExit := make(chan struct{}, 1)
	b, bPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch B: %v", err)
		return exitSetup
	}
	drain(b, bExit, nil)
	result("b_session_tracee_pid", bPID)
	result("b_session_owned_tids", fmt.Sprint(threadTIDs(bPID)))
	result("b_waitloop_armed", false) // suspended at entry: no waitLoop exists
	result("b_tracee_state_initial", procState(bPID))

	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
	result("a_session_tracee_pid", aPID)

	killed := killAsync(a)
	if !awaitReaped(aPID, 15*time.Second) {
		tracef("A's own tracee never reaped; cannot attribute the wait")
		return exitSetup
	}
	result("a_tracee_reaped", true)

	select {
	case err := <-killed:
		tracef("A.Kill returned early (err=%v) — no wedge this run", err)
		result("kill_returned_before_rescue_attempt", true)
		_ = syscall.Kill(bPID, syscall.SIGKILL)
		return exitOK
	case <-time.After(3 * time.Second):
		result("kill_blocked_while_peer_session_tracee_lives", true)
	}

	// The rescue attempt: kill the obstacle from outside its owning session.
	tracef("attempting external rescue: SIGKILL peer session B's tracee pid=%d (state=%s)",
		bPID, procState(bPID))
	_ = syscall.Kill(bPID, syscall.SIGKILL)
	time.Sleep(2 * time.Second)

	result("b_tracee_state_after_external_sigkill", procState(bPID))
	result("b_tracee_sigkill_pending_undelivered", sigkillPending(bPID))
	result("b_tracee_still_child", ppidOf(bPID) == os.Getpid())
	why, harnessTook := hasPendingStatus(bPID)
	result("b_tracee_wait_status", why)
	result("harness_consumed_a_status", harnessTook)

	select {
	case err := <-killed:
		tracef("A.Kill returned after the external SIGKILL (err=%v) — recoverable", err)
		result("kill_returned_after_external_sigkill", true)
		return exitOK
	case <-time.After(10 * time.Second):
		tracef("A.Kill STILL blocked: peer tracee is frozen in ptrace-stop with SIGKILL " +
			"pending, so process-wide ECHILD is unreachable by any external action")
		result("kill_returned_after_external_sigkill", false)
		result("b_session_saw_its_own_process_exit", len(bExit) > 0)
		dumpBlockedGoroutines()
		return exitWedged
	}
}

// scenarioSuspendedKillPlainChild shows the wedge is not specific to a second
// DEBUGGER session. The obstacle here is an ordinary, never-traced child of the
// same process — the kind any server might spawn (a helper, a build step). It
// proves reapAfterKill's terminator is process-wide, so its scope is "any live
// child", not merely "another session's tracee".
func scenarioSuspendedKillPlainChild() int {
	peer := exec.Command(longBin)
	peer.SysProcAttr = &syscall.SysProcAttr{Setpgid: false}
	if err := peer.Start(); err != nil {
		tracef("start plain child: %v", err)
		return exitSetup
	}
	// Deliberately never Wait for it: it stays an unreaped, live child.
	bPID := peer.Process.Pid
	tracef("plain (untraced) child started pid=%d state=%s", bPID, procState(bPID))
	result("plain_child_pid", bPID)
	result("plain_child_is_traced", false)

	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
	tracef("session A launched, tracee pid=%d state=%s", aPID, procState(aPID))
	result("a_pid", aPID)

	t0 := time.Now()
	killed := killAsync(a)
	reaped := awaitReaped(aPID, 15*time.Second)
	tracef("A's own tracee reaped=%v after %s", reaped, time.Since(t0))
	result("a_tracee_reaped", reaped)

	select {
	case err := <-killed:
		tracef("A.Kill returned after %s (err=%v)", time.Since(t0), err)
		result("kill_returned", true)
		result("kill_ms", time.Since(t0).Milliseconds())
		_ = syscall.Kill(bPID, syscall.SIGKILL)
		return exitOK
	case <-time.After(killWait()):
		tracef("A.Kill DID NOT return within %s while an ordinary untraced child lives",
			killWait())
		result("kill_returned", false)
		result("plain_child_state_at_wedge", procState(bPID))
		result("plain_child_still_child", ppidOf(bPID) == os.Getpid())
		dumpBlockedGoroutines()
		return exitWedged
	}
}

func scenarioSuspendedKillLivePeer() int {
	// Peer session B: launched, never resumed. Its tracee is a live child of
	// this process, parked at its entry stop, with its execve SIGTRAP already
	// consumed by startTracedProcess — so it will never produce a waitable
	// event on its own. It is nothing but an obstacle to a process-wide ECHILD.
	_, bPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch B: %v", err)
		return exitSetup
	}
	tracef("peer B launched, tracee pid=%d state=%s threads=%d", bPID, procState(bPID), threadCount(bPID))
	result("b_pid", bPID)

	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
	tracef("session A launched, tracee pid=%d state=%s threads=%d", aPID, procState(aPID), threadCount(aPID))
	result("a_pid", aPID)

	// A is suspended, so engine.Kill passes running=false and killProcess calls
	// reapAfterKill — the Wait4(-1, WALL)-until-ECHILD loop under test.
	t0 := time.Now()
	killed := killAsync(a)

	reaped := awaitReaped(aPID, 15*time.Second)
	tracef("A's own tracee reaped=%v (state=%s) after %s", reaped, procState(aPID), time.Since(t0))
	result("a_tracee_reaped", reaped)

	select {
	case err := <-killed:
		tracef("A.Kill returned after %s (err=%v)", time.Since(t0), err)
		result("kill_returned", true)
		result("kill_ms", time.Since(t0).Milliseconds())
		return exitOK
	case <-time.After(killWait()):
		tracef("A.Kill DID NOT return within %s while peer B's tracee is still a live child",
			killWait())
		result("kill_returned", false)
		result("b_state_at_wedge", procState(bPID))
		result("b_still_child", ppidOf(bPID) == os.Getpid())
		dumpBlockedGoroutines()
		// Do not attempt B.Kill() here: B's own reapAfterKill would block on the
		// same process-wide wait and muddy the verdict. The parent SIGKILLs the
		// whole process group, which reaps B's tracee too.
		return exitWedged
	}
}

// scenarioPeerWaitLoopStealsExit — U3-b: two armed waitLoops, one death.
func scenarioPeerWaitLoopStealsExit() int {
	// A: a freely-running tracee that will not exit for the whole scenario. Its
	// waitLoop is therefore parked in Wait4(-1, WALL) with nothing of its own to
	// receive — a pure thief.
	aExit := make(chan struct{}, 1)
	var aStops atomic.Int64
	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
	drain(a, aExit, &aStops)
	if err := a.Continue(); err != nil {
		tracef("A.Continue: %v", err)
		return exitSetup
	}
	// Let A's Go runtime finish spinning up its threads so its clone-event
	// traffic is over before B exists; otherwise the scenario measures clone
	// theft instead of exit theft.
	time.Sleep(700 * time.Millisecond)
	tracef("A running, tracee pid=%d threads=%d", aPID, threadCount(aPID))
	result("a_pid", aPID)

	bExit := make(chan struct{}, 1)
	bRes, bOK := launchSuspendedTimed(quickBin, killWait(), "600")
	if !bOK {
		// B's execve stop was consumed by A's waitLoop; Launch never returns.
		// Report this separately: it is the launch-theft facet, and must not be
		// allowed to satisfy an assertion about EXIT theft.
		tracef("B.Launch wedged by peer A's waitLoop (launch-stop theft)")
		result("launch_wedged", true)
		dumpBlockedGoroutines()
		return exitLaunchWedged
	}
	b, bPID, err := bRes.dbg, bRes.pid, bRes.err
	if err != nil {
		tracef("launch B: %v", err)
		return exitSetup
	}
	drain(b, bExit, nil)
	if err := b.Continue(); err != nil {
		tracef("B.Continue: %v", err)
		return exitSetup
	}
	tracef("B running, tracee pid=%d — its death is the single contested event", bPID)
	result("b_pid", bPID)

	select {
	case <-bExit:
		tracef("B received its own tracee's exit — no theft this iteration")
		result("b_exit_seen", true)
		return exitOK
	case <-time.After(20 * time.Second):
		tracef("B NEVER received its tracee's exit; b state=%s a_saw_exit=%v",
			procState(bPID), len(aExit) > 0)
		result("b_exit_seen", false)
		result("b_tracee_state", procState(bPID))
		result("a_saw_a_process_exit", len(aExit) > 0)
		result("a_foreign_stops", aStops.Load())
		dumpBlockedGoroutines()
		return exitStolen
	}
}

// scenarioReapAfterKillVsPeerDeath — A's reapAfterKill and B's waitLoop contend
// for B's death.
func scenarioReapAfterKillVsPeerDeath() int {
	bExit := make(chan struct{}, 1)
	b, bPID, err := launchSuspended(quickBin, "2500")
	if err != nil {
		tracef("launch B: %v", err)
		return exitSetup
	}
	drain(b, bExit, nil)
	if err := b.Continue(); err != nil {
		tracef("B.Continue: %v", err)
		return exitSetup
	}
	tracef("peer B running, tracee pid=%d, will exit in ~2.5s", bPID)
	result("b_pid", bPID)

	a, aRes := (debugger.Debugger)(nil), launchResult{}
	{
		r, ok := launchSuspendedTimed(longBin, killWait())
		if !ok {
			tracef("A.Launch wedged by peer B's waitLoop (launch-stop theft)")
			result("launch_wedged", true)
			dumpBlockedGoroutines()
			return exitLaunchWedged
		}
		aRes = r
	}
	if aRes.err != nil {
		tracef("launch A: %v", aRes.err)
		return exitSetup
	}
	a, aPID := aRes.dbg, aRes.pid
	tracef("session A launched suspended, tracee pid=%d", aPID)
	result("a_pid", aPID)

	t0 := time.Now()
	killed := killAsync(a)
	result("a_tracee_reaped", awaitReaped(aPID, 10*time.Second))

	killReturned := false
	exitSeen := false
	deadline := time.After(killWait())
	for !(killReturned && exitSeen) {
		select {
		case err := <-killed:
			killReturned = true
			tracef("A.Kill returned after %s (err=%v)", time.Since(t0), err)
			result("kill_ms", time.Since(t0).Milliseconds())
		case <-bExit:
			exitSeen = true
			tracef("B received its tracee's exit after %s", time.Since(t0))
		case <-deadline:
			result("kill_returned", killReturned)
			result("b_exit_seen", exitSeen)
			tracef("deadline: kill_returned=%v b_exit_seen=%v", killReturned, exitSeen)
			dumpBlockedGoroutines()
			if killReturned && !exitSeen {
				// A's reapAfterKill reached ECHILD, which is only possible once
				// B's tracee was fully reaped — and B never saw that death.
				tracef("verdict: A's Kill terminated by consuming peer B's death")
				return exitStolen
			}
			tracef("verdict: A's Kill is wedged behind the peer session")
			return exitWedged
		}
	}
	result("kill_returned", true)
	result("b_exit_seen", true)
	tracef("both A.Kill and B's exit completed — benign interleaving this iteration")
	return exitOK
}
