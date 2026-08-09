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
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

const (
	envScenario = "BINGO_PROOF_U3_SCENARIO"
	envLongBin  = "BINGO_PROOF_U3_LONG_BIN"
	envQuickBin = "BINGO_PROOF_U3_QUICK_BIN"

	exitOK     = 0
	exitSetup  = 1
	exitWedged = 20
	exitStolen = 30
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

	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
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
			scenario, backstop, out.String())
	}

	code := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("scenario %q wait: %v\n%s", scenario, waitErr, out.String())
		}
	}
	// Reap any tracee the child left behind (a wedged engine's tracees are
	// re-parented to init after the child dies, but its own group members are
	// not; kill the group unconditionally — the child itself is already gone).
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	t.Logf("scenario %q exit=%d\n%s", scenario, code, out.String())
	return childResult{code: code, output: out.String()}
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
		if r.code == exitSetup {
			t.Fatalf("iteration %d: setup failure", i)
		}
		if r.code == exitStolen {
			lost++
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
}

// --- U3-b: cross-session stop/exit theft ------------------------------------

// TestProof_PeerWaitLoopStealsProcessExit runs two sessions with concurrently
// armed waitLoops: A runs a tracee that never exits, B runs one that exits
// promptly. Both waitLoops block in Wait4(-1), so B's death can be delivered to
// A, which silently swallows it (tid != b.pid → continue) and leaves B waiting
// forever. This is an inherent race, so it is repeated and reported as a rate.
func TestProof_PeerWaitLoopStealsProcessExit(t *testing.T) {
	iters := envInt("BINGO_PROOF_U3_ITERS", 15)
	stolen := 0
	for i := 0; i < iters; i++ {
		r := runChild(t, "proof-peer-waitloop-steals-exit", 90*time.Second)
		if r.code == exitSetup {
			t.Fatalf("iteration %d: setup failure", i)
		}
		if r.code == exitStolen {
			stolen++
		}
	}
	t.Logf("PROOF cross-session exit theft: %d/%d iterations lost B's EventProcessExited", stolen, iters)
	if stolen == 0 {
		t.Errorf("no theft reproduced in %d iterations — this is a wait4 wake race; "+
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
	wedged, stolen, clean := 0, 0, 0
	for i := 0; i < iters; i++ {
		r := runChild(t, "proof-reapafterkill-vs-peer-death", 120*time.Second)
		switch r.code {
		case exitSetup:
			t.Fatalf("iteration %d: setup failure", i)
		case exitWedged:
			wedged++
		case exitStolen:
			stolen++
		default:
			clean++
		}
	}
	t.Logf("PROOF reapAfterKill vs peer death: wedged=%d stolen=%d clean=%d (of %d)",
		wedged, stolen, clean, iters)
	if wedged+stolen == 0 {
		t.Errorf("neither wedge nor theft reproduced in %d iterations", iters)
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
	case "proof-peer-waitloop-steals-exit":
		return scenarioPeerWaitLoopStealsExit()
	case "proof-reapafterkill-vs-peer-death":
		return scenarioReapAfterKillVsPeerDeath()
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
		return "gone"
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
func drain(d debugger.Debugger, sawExit chan<- struct{}) {
	go func() {
		notified := false
		for evt := range d.Events() {
			if evt.Kind == protocol.EventProcessExited && !notified {
				notified = true
				select {
				case sawExit <- struct{}{}:
				default:
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
	drain(b, sawExit)
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
	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
	drain(a, aExit)
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
	b, bPID, err := launchSuspended(quickBin, "600")
	if err != nil {
		tracef("launch B: %v", err)
		return exitSetup
	}
	drain(b, bExit)
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
	drain(b, bExit)
	if err := b.Continue(); err != nil {
		tracef("B.Continue: %v", err)
		return exitSetup
	}
	tracef("peer B running, tracee pid=%d, will exit in ~2.5s", bPID)
	result("b_pid", bPID)

	a, aPID, err := launchSuspended(longBin)
	if err != nil {
		tracef("launch A: %v", err)
		return exitSetup
	}
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
