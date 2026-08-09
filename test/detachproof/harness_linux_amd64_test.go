//go:build detachproof && linux && amd64

// Package detachproof holds PROOF-ONLY, TEST-ONLY acceptance evidence for
// hypothesis U4 on linux/amd64:
//
//	Debugger.Kill on an ATTACHED and RUNNING tracee performs breakpoint
//	restoration (PTRACE_POKEDATA) and PTRACE_DETACH while the tracee is not in
//	a ptrace-stop. Both operations fail with ESRCH, both failures are
//	discarded, Kill reports success, and the process is left (a) still traced
//	and (b) still carrying the INT3 trap byte.
//
// Nothing in this package is imported by production code. It carries its own
// `detachproof` build tag so it is invisible to `go test ./...`, to the `e2e`
// suite, and to every existing CI job. It makes ZERO production edits — every
// observation is taken from outside the debugger via /proc/<pid>/status,
// /proc/<pid>/mem, and the target's own filesystem markers.
//
// Read AGENTS.md → "Backend quirks / Linux" and "Engine concurrency model"
// for the invariants this exercises. PR #108 added the `attach` e2e spec but
// deliberately only ever detaches from a SUSPENDED tracee (its cleanup runs
// while parked at a breakpoint), so the attached+running detach path has no
// coverage at all — that gap is exactly what this package probes.
//
// Tuning env vars:
//
//	BINGO_PROOF_ITERS  (default 1)  repeat count for the main proof
package detachproof

import (
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// proofTargetSrc is the deterministic external target.
//
// Determinism comes from the GATE: main never executes the breakpointed line
// inside gated() until the gate file appears. So the debugger can attach, set a
// breakpoint on a FUTURE code path, Continue (making the tracee genuinely
// running, not parked at a stop), and Kill — with a hard guarantee that the
// trap was never reached before Kill. Releasing the gate afterwards is then a
// clean, single-variable test of whether Kill left the process usable.
//
// runtime.LockOSThread pins main to the thread linux PTRACE_ATTACH traces (the
// attach path never applies PTRACE_O_TRACECLONE — see PR #108), so a trap taken
// on that line is delivered to us and not to the Go runtime as "fatal: trace
// trap". Markers on disk give the harness an observable PID, readiness signal,
// liveness signal and completion signal without any stdio back-pressure.
const proofTargetSrc = `package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

//go:noinline
func gated(n int) int {
	s := 0
	s += n // BP
	return s
}

func main() {
	runtime.LockOSThread()
	gate, ready, done, beat := os.Args[1], os.Args[2], os.Args[3], os.Args[4]

	if err := os.WriteFile(ready, []byte(fmt.Sprint(syscall.Gettid())), 0o600); err != nil {
		os.Exit(3)
	}

	// Phase 1: spin without ever entering gated(), so the breakpoint address is
	// provably never executed, until the harness opens the gate.
	deadline := time.Now().Add(300 * time.Second)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(4)
		}
		// Heartbeat proves the traced main thread is still scheduling, which
		// distinguishes "process alive but its traced thread is frozen in a
		// ptrace-stop" from "process alive and running".
		_ = os.WriteFile(beat, []byte(fmt.Sprint(time.Now().UnixNano())), 0o600)
		time.Sleep(2 * time.Millisecond)
	}

	// Phase 2: execute the formerly-breakpointed site exactly once.
	v := gated(10)
	if err := os.WriteFile(done, []byte(fmt.Sprint(v)), 0o600); err != nil {
		os.Exit(5)
	}
	// Linger so the harness can still observe /proc state after completion.
	time.Sleep(60 * time.Second)
	os.Exit(0)
}
`

// gatedExpected is what gated(10) returns; the target writes it to the done
// marker so a truncated/garbled completion is distinguishable from a real one.
const gatedExpected = 10

// buildProofTarget compiles src with -N -l so DWARF line info is exact and the
// breakpointed line is a real instruction (mirrors the e2e suite's buildTarget).
func buildProofTarget(t *testing.T, name, src string) (binPath, srcName string) {
	t.Helper()
	dir := t.TempDir()
	srcName = name + ".go"
	srcPath := filepath.Join(dir, srcName)
	if err := os.WriteFile(srcPath, []byte(src), 0o600); err != nil {
		t.Fatalf("write target source: %v", err)
	}
	binPath = filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", binPath, srcPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build target %s: %v\n%s", name, err, out)
	}
	return binPath, srcName
}

// markerLine returns the 1-based line of the first line containing marker.
func markerLine(t *testing.T, src, marker string) int {
	t.Helper()
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, marker) {
			return i + 1
		}
	}
	t.Fatalf("marker %q not found in target source", marker)
	return 0
}

// target is a running proof target plus its marker paths.
type target struct {
	cmd   *exec.Cmd
	pid   int
	gate  string
	ready string
	done  string
	beat  string

	// waited closes when the single reaper goroutine has collected the child's
	// termination status; state then holds it (nil if a leaked engine waitLoop
	// blocked in Wait4(-1) won the race for the status first).
	waited chan struct{}
	state  atomic.Pointer[os.ProcessState]

	// errMu guards errBuf. The target's stderr is captured because a Go
	// process killed by an unabsorbed SIGTRAP prints a runtime fatal-error
	// banner naming the signal — direct evidence of the leftover INT3 that
	// does not depend on winning the wait-status race against the engine's
	// leaked waitLoop.
	errMu  sync.Mutex
	errBuf bytes.Buffer
}

// stderr returns whatever the target has written to stderr so far.
func (tg *target) stderr() string {
	tg.errMu.Lock()
	defer tg.errMu.Unlock()
	return strings.TrimSpace(tg.errBuf.String())
}

// startTarget launches bin as an INDEPENDENT OS process (never under the
// debugger) and waits for its readiness marker. One reaper goroutine owns
// Wait(), so the termination cause — crucially, death by SIGTRAP on a leftover
// INT3 — is captured rather than lost. Cleanup is strict: SIGKILL, then join
// the reaper.
func startTarget(t *testing.T, bin string) *target {
	t.Helper()
	dir := t.TempDir()
	tg := &target{
		gate:   filepath.Join(dir, "gate"),
		ready:  filepath.Join(dir, "ready"),
		done:   filepath.Join(dir, "done"),
		beat:   filepath.Join(dir, "beat"),
		waited: make(chan struct{}),
	}
	tg.cmd = exec.Command(bin, tg.gate, tg.ready, tg.done, tg.beat)
	tg.cmd.Stderr = &lockedWriter{mu: &tg.errMu, w: &tg.errBuf}
	if err := tg.cmd.Start(); err != nil {
		t.Fatalf("start target: %v", err)
	}
	tg.pid = tg.cmd.Process.Pid
	go func() {
		defer close(tg.waited)
		if st, err := tg.cmd.Process.Wait(); err == nil {
			tg.state.Store(st)
		}
	}()
	t.Cleanup(func() {
		_ = tg.cmd.Process.Signal(syscall.SIGKILL)
		select {
		case <-tg.waited:
		case <-time.After(3 * time.Second):
			t.Logf("cleanup: reap of pid %d did not complete in 3s (a leaked waitLoop may hold the status)", tg.pid)
		}
	})

	if !waitForFile(tg.ready, 15*time.Second) {
		t.Fatalf("target pid %d never wrote its readiness marker", tg.pid)
	}
	// PTRACE_ATTACH traces one THREAD, and the linux backend attaches to the
	// pid. Unless the target's locked main thread is the group leader
	// (tid == pid), the heartbeat would come from an untraced M and would not
	// prove the traced thread resumed.
	b, err := os.ReadFile(tg.ready)
	if err != nil {
		t.Fatalf("read readiness marker: %v", err)
	}
	tid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse readiness marker %q: %v", b, err)
	}
	if tid != tg.pid {
		t.Fatalf("target's locked main thread is tid %d but the process is pid %d; "+
			"PTRACE_ATTACH would trace a different thread than the one heartbeating", tid, tg.pid)
	}
	// Let the Go runtime reach steady state before attaching (mirrors the e2e
	// attach harness) so the attach stop is not racing process startup.
	time.Sleep(200 * time.Millisecond)
	return tg
}

// lockedWriter serialises writes from the exec copier goroutine against test
// goroutines reading the buffer.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// terminated reports whether the target died within timeout and, if so, how.
func (tg *target) terminated(timeout time.Duration) (dead bool, how string) {
	select {
	case <-tg.waited:
	case <-time.After(timeout):
		return false, ""
	}
	st := tg.state.Load()
	if st == nil {
		return true, "<status consumed by another waiter>"
	}
	desc := st.String()
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		desc = fmt.Sprintf("%s (signal %d = %v)", desc, ws.Signal(), ws.Signal())
	}
	return true, desc
}

func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// openGate releases the target into phase 2.
func (tg *target) openGate(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(tg.gate, []byte("go"), 0o600); err != nil {
		t.Fatalf("open gate: %v", err)
	}
}

// heartbeat returns the target's last heartbeat value, or "" if none yet.
func (tg *target) heartbeat() string {
	b, err := os.ReadFile(tg.beat)
	if err != nil {
		return ""
	}
	return string(b)
}

// beating reports whether the traced main thread produced a NEW heartbeat
// within window — i.e. it is genuinely scheduling rather than frozen in a
// ptrace-stop. A process can be alive (siblings running) while its traced
// thread is stopped, so plain liveness is not sufficient evidence.
func (tg *target) beating(window time.Duration) bool {
	before := tg.heartbeat()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if h := tg.heartbeat(); h != "" && h != before {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// tracerPID reads TracerPid from /proc/<pid>/status. Returns (0, nil) when the
// process is untraced, and a non-nil error only when status is unreadable
// (process gone).
func tracerPID(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "TracerPid:"); ok {
			return strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	return 0, fmt.Errorf("TracerPid not present in /proc/%d/status", pid)
}

func mustTracerPID(t *testing.T, pid int, when string) int {
	t.Helper()
	tp, err := tracerPID(pid)
	if err != nil {
		t.Fatalf("read TracerPid %s: %v (target pid %d gone?)", when, err, pid)
	}
	return tp
}

// tgidOf resolves a thread id to its owning process id via /proc/<tid>/status.
// TracerPid names the tracing THREAD (ptrace attaches per-thread and the linux
// backend attaches from its dedicated tracer thread), so identifying the tracer
// as "this process" requires this extra hop rather than comparing to os.Getpid.
func tgidOf(tid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", tid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "Tgid:"); ok {
			return strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	return 0, fmt.Errorf("Tgid not present in /proc/%d/status", tid)
}

// tracedByThisProcess reports whether tracerTid belongs to the test process.
func tracedByThisProcess(tracerTid int) bool {
	if tracerTid == 0 {
		return false
	}
	tgid, err := tgidOf(tracerTid)
	return err == nil && tgid == os.Getpid()
}

// procState returns the single-letter state from /proc/<pid>/stat ("t" is
// tracing-stop, "R"/"S" are runnable/sleeping, "Z" is zombie).
func procState(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "?"
	}
	// The comm field is parenthesised and may contain spaces; state follows it.
	if i := strings.LastIndex(string(b), ")"); i >= 0 && i+2 < len(b) {
		return string(b[i+2])
	}
	return "?"
}

// funcImage locates sym in the ELF and returns its runtime virtual address plus
// its pristine on-disk bytes. The linux backend has no TextSlide hook (see
// AGENTS.md — only darwin computes an ASLR slide), so `go build`'s default
// non-PIE ET_EXEC layout means vaddr IS the runtime address; the ET_EXEC assert
// makes that assumption fail loudly rather than silently.
func funcImage(t *testing.T, bin, sym string) (vaddr uint64, disk []byte) {
	t.Helper()
	f, err := elf.Open(bin)
	if err != nil {
		t.Fatalf("open ELF %s: %v", bin, err)
	}
	defer func() { _ = f.Close() }()

	if f.Type != elf.ET_EXEC {
		t.Fatalf("proof assumes a non-PIE ET_EXEC target (slide 0); got %v", f.Type)
	}
	syms, err := f.Symbols()
	if err != nil {
		t.Fatalf("read ELF symbols: %v", err)
	}
	for _, s := range syms {
		if s.Name != sym {
			continue
		}
		if int(s.Section) >= len(f.Sections) {
			t.Fatalf("symbol %s has out-of-range section index", sym)
		}
		sect := f.Sections[s.Section]
		data, err := sect.Data()
		if err != nil {
			t.Fatalf("read section %s: %v", sect.Name, err)
		}
		off := s.Value - sect.Addr
		if off+s.Size > uint64(len(data)) {
			t.Fatalf("symbol %s exceeds section %s bounds", sym, sect.Name)
		}
		return s.Value, data[off : off+s.Size]
	}
	t.Fatalf("symbol %s not found in %s", sym, bin)
	return 0, nil
}

// readTraceeMem reads n bytes at addr from /proc/<pid>/mem. The harness is the
// target's parent (and, in the bug case, still its tracer), so ptrace_may_access
// permits the read under the default Yama scope.
func readTraceeMem(t *testing.T, pid int, addr uint64, n int) ([]byte, error) {
	t.Helper()
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return nil, fmt.Errorf("open /proc/%d/mem: %w", pid, err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, int64(addr)); err != nil {
		return nil, fmt.Errorf("read /proc/%d/mem at 0x%x: %w", pid, addr, err)
	}
	return buf, nil
}

// diffBytes returns the indices where a and b differ, capped at max entries.
func diffBytes(a, b []byte, max int) []int {
	var out []int
	for i := range a {
		if i >= len(b) {
			break
		}
		if a[i] != b[i] {
			out = append(out, i)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

// int3 is the amd64 software-breakpoint trap byte the engine patches in.
const int3 = byte(0xCC)

// withTracerThread runs fn on a dedicated, OS-locked thread. ptrace control
// operations are thread-bound: every request for a tracee must be issued from
// the exact thread that attached. The goroutine never unlocks, so the thread
// dies with it and can never be recycled for another tracee.
func withTracerThread(fn func()) {
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer close(done)
		fn()
	}()
	<-done
}

func proofIters() int {
	if v := os.Getenv("BINGO_PROOF_ITERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}
