//go:build e2e && linux && amd64

package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// Linux/amd64 debugger acceptance suite. Drives the real ptrace backend
// (single tracer thread, PC rewind, clone tracing, stepTID disambiguation).
// Runs on native ubuntu runners; see .github/workflows/debugger-e2e.yml.
var _ = Describe("Linux amd64 debugger backend (ptrace) E2E", Label("linux"), func() {
	declareBasicStepOverSpec()
	declareChurnSpec()
	declarePauseSpec()
	declareStepIntoSpec()
	declareStepOutSpec()
	declareInspectSpec()
	declareTypedLocalsSpec()
	declareClearBreakpointSpec()
	declareKillRunningSpec()
	declareExitCodeSpec()
	declareAttachSpec()
	declareConcurrencySpec()
	declareBaselineOwnershipSpec()
	declareCurrentGoroutineScanSpec()
	declareFullStackSpec()
	declareRestartSpec()
	declareDAPSpec()
	declareDAPEvaluateSpec()
	declareDAPExitSpec()
	declareDAPMultiClientSpec()
	declareDAPJoinSpec()
	declareSignalForwardingSpec()
	declareStepOverlapSpec()
	declareStepOverlapStepIntoSpec()
	declareStepOverlapSignalSpec()
	declareStepOverlapPauseSpec()
	declareStepOverlapPauseDeliverySpec()
	declareStepOverlapKillSpec()
	declareStepOwnerExitSpec()
	declareStepOwnerHoldSpec()
	declareLeaderRetirementSpec()
	declareLinuxWaitOwnershipSpecs()
})

const signalTargetExitOK = 43

const waitOwnershipExitTargetSrc = `package main

import (
	"os"
	"strconv"
	"time"
)

func main() {
	_ = os.WriteFile(os.Args[1], []byte(strconv.Itoa(os.Getpid())), 0o600)
	delay, _ := strconv.Atoi(os.Args[2])
	code, _ := strconv.Atoi(os.Args[3])
	time.Sleep(time.Duration(delay) * time.Millisecond)
	os.Exit(code)
}
`

func declareLinuxWaitOwnershipSpecs() {
	It("kills a suspended tracee without waiting on an unrelated live child",
		Label("kill"), func() {
			bin := buildTarget("wait_owner_unrelated", basicTargetSrc)
			unrelated := exec.Command(bin)
			Expect(unrelated.Start()).To(Succeed(), "start unrelated child")
			DeferCleanup(func() {
				_ = unrelated.Process.Kill()
				_ = unrelated.Wait()
			})

			before := directChildPIDs()
			d := debugger.New(nil)
			DeferCleanup(func() {
				done := make(chan struct{})
				go func() {
					_ = d.Kill()
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
			})
			Expect(d.Launch(bin, nil, nil)).To(Succeed(), "launch suspended tracee")
			awaitEvent(d.Events(), 15*time.Second, protocol.EventStepped)

			added := addedPIDs(before, directChildPIDs())
			Expect(added).To(HaveLen(1), "identify the debugger-owned child")
			traceePID := added[0]

			killDone := make(chan error, 1)
			go func() { killDone <- d.Kill() }()
			select {
			case err := <-killDone:
				Expect(err).NotTo(HaveOccurred(), "kill suspended tracee")
			case <-time.After(15 * time.Second):
				Fail("Kill waited on an unrelated live child")
			}

			Eventually(func() bool { return !processExists(traceePID) }, 10*time.Second, 20*time.Millisecond).
				Should(BeTrue(), "the debugger-owned tracee must be reaped")
			Expect(syscall.Kill(unrelated.Process.Pid, 0)).To(Succeed(),
				"killing one debugger session must not consume or terminate an unrelated child")
		})

	It("routes concurrent initial stops and natural exits to their owning sessions",
		Label("exit"), func() {
			bin := buildTarget("wait_owner_exit", waitOwnershipExitTargetSrc)
			dir := GinkgoT().TempDir()

			const sessions = 4
			type liveSession struct {
				debugger debugger.Debugger
				pid      int
				exitCode int
			}
			live := make([]liveSession, 0, sessions)
			DeferCleanup(func() {
				for _, session := range live {
					_ = session.debugger.Kill()
				}
			})

			for i := 0; i < sessions; i++ {
				pidFile := filepath.Join(dir, fmt.Sprintf("session-%d.pid", i))
				exitCode := 50 + i
				d := debugger.New(nil)
				args := []string{pidFile, strconv.Itoa(1500 + i*250), strconv.Itoa(exitCode)}
				Expect(d.Launch(bin, args, nil)).To(Succeed(),
					"session %d initial stop must not be stolen by an earlier running session", i)
				awaitEvent(d.Events(), 15*time.Second, protocol.EventStepped)
				Expect(d.Continue()).To(Succeed(), "continue session %d", i)
				Eventually(func() bool { return pathExists(pidFile) }, 5*time.Second, 20*time.Millisecond).
					Should(BeTrue(), "session %d target must publish its PID", i)
				raw, err := os.ReadFile(pidFile)
				Expect(err).NotTo(HaveOccurred(), "read session %d PID", i)
				pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
				Expect(err).NotTo(HaveOccurred(), "parse session %d PID", i)
				live = append(live, liveSession{debugger: d, pid: pid, exitCode: exitCode})
			}

			for i, session := range live {
				evt := awaitEvent(session.debugger.Events(), 20*time.Second,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Equal(protocol.EventProcessExited),
					"session %d must receive its own natural exit, got %s: %s", i, evt.Kind, evt.Payload)
				var payload protocol.ProcessExitedPayload
				Expect(json.Unmarshal(evt.Payload, &payload)).To(Succeed(), "decode session %d exit", i)
				Expect(payload.ExitCode).To(Equal(session.exitCode),
					"session %d received another tracee's status", i)
				Eventually(func() bool { return !processExists(session.pid) }, 10*time.Second, 20*time.Millisecond).
					Should(BeTrue(), "session %d natural exit must be reaped", i)
			}
		})
}

func directChildPIDs() []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var children []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, "PPid:") {
				continue
			}
			ppid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
			if ppid == os.Getpid() {
				children = append(children, pid)
			}
			break
		}
	}
	return children
}

func addedPIDs(before, after []int) []int {
	known := make(map[int]struct{}, len(before))
	for _, pid := range before {
		known[pid] = struct{}{}
	}
	var added []int
	for _, pid := range after {
		if _, ok := known[pid]; !ok {
			added = append(added, pid)
		}
	}
	return added
}

func processExists(pid int) bool {
	return pathExists(filepath.Join("/proc", strconv.Itoa(pid)))
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

const segvSignalTargetSrc = `package main

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var sinkByte byte

type kernelSigaction struct {
	handler  uintptr
	flags    uintptr
	restorer uintptr
	mask     uint64
}

func resetToDefault(sig syscall.Signal) {
	// os/signal.Reset still lets Go translate synchronous faults into exit(2);
	// this target needs kernel signal death to exercise StopKilled.
	action := kernelSigaction{}
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_RT_SIGACTION,
		uintptr(sig),
		uintptr(unsafe.Pointer(&action)),
		0,
		unsafe.Sizeof(action.mask),
		0,
		0,
	)
	runtime.KeepAlive(&action)
	if errno != 0 {
		os.Exit(89)
	}
}

func main() {
	resetToDefault(syscall.SIGSEGV)
	var p *byte
	sinkByte = *p
}
`

const abortSignalTargetSrc = `package main

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

type kernelSigaction struct {
	handler  uintptr
	flags    uintptr
	restorer uintptr
	mask     uint64
}

func resetToDefault(sig syscall.Signal) {
	// os/signal.Reset still lets Go translate fatal signals into exit(2); this
	// target needs kernel signal death to exercise StopKilled.
	action := kernelSigaction{}
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_RT_SIGACTION,
		uintptr(sig),
		uintptr(unsafe.Pointer(&action)),
		0,
		unsafe.Sizeof(action.mask),
		0,
		0,
	)
	runtime.KeepAlive(&action)
	if errno != 0 {
		os.Exit(89)
	}
}

func main() {
	resetToDefault(syscall.SIGABRT)
	if err := syscall.Tgkill(os.Getpid(), syscall.Gettid(), syscall.SIGABRT); err != nil {
		os.Exit(90)
	}
	for {
		runtime.Gosched()
	}
}
`

// ordinarySignalTargetSrc directs one handled signal at a known locked thread
// while a sibling thread keeps making progress. Exit 43 proves all three
// outcomes: SIGUSR1 reached the tracee, the signalled thread was the one resumed,
// and another thread was not stranded by the resume.
const ordinarySignalTargetSrc = `package main

import (
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	signalledProgress int64
	siblingProgress   int64
)

func runCounter(counter *int64, tid chan<- int) {
	runtime.LockOSThread()
	if tid != nil {
		tid <- syscall.Gettid()
	}
	for {
		atomic.AddInt64(counter, 1)
		time.Sleep(100 * time.Microsecond)
	}
}

func main() {
	runtime.GOMAXPROCS(2)
	received := make(chan os.Signal, 1)
	signal.Notify(received, syscall.SIGUSR1)

	tid := make(chan int, 1)
	go runCounter(&signalledProgress, tid)
	go runCounter(&siblingProgress, nil)
	signalledTID := <-tid

	for atomic.LoadInt64(&signalledProgress) == 0 || atomic.LoadInt64(&siblingProgress) == 0 {
		runtime.Gosched()
	}
	if err := syscall.Tgkill(os.Getpid(), signalledTID, syscall.SIGUSR1); err != nil {
		os.Exit(91)
	}
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		os.Exit(92)
	}
	signalledBefore := atomic.LoadInt64(&signalledProgress)
	siblingBefore := atomic.LoadInt64(&siblingProgress)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&signalledProgress) > signalledBefore &&
			atomic.LoadInt64(&siblingProgress) > siblingBefore {
			os.Exit(43)
		}
		runtime.Gosched()
	}
	os.Exit(93)
}
`

// declareSignalForwardingSpec is the native regression for issue #206. A
// signal-delivery stop must produce one Output event, then the signal must be
// injected into the exact stopped TID so the target either dies from its fatal
// signal or observes its handled signal and exits normally.
func declareSignalForwardingSpec() {
	It("forwards fatal signals once and reaches signal death", Label("signals"), func() {
		cases := []struct {
			name   string
			src    string
			signal syscall.Signal
		}{
			{name: "SIGSEGV", src: segvSignalTargetSrc, signal: syscall.SIGSEGV},
			{name: "SIGABRT", src: abortSignalTargetSrc, signal: syscall.SIGABRT},
		}

		for _, tc := range cases {
			By(tc.name)
			bin := buildTarget("signal_"+strings.ToLower(tc.name)+"_target", tc.src)
			assertSignalDeath(bin, tc.signal)
			h := newE2EHarness(bin)
			h.waitFor(15*time.Second, protocol.EventStepped)

			Expect(h.d.Continue()).To(Succeed(), "Continue into %s", tc.name)
			awaitSignalExit(h.d.Events(), tc.signal, -1)
		}
	})

	It("delivers an ordinary signal to its stopped thread without stranding its sibling",
		Label("signals"), func() {
			bin := buildTarget("signal_usr1_target", ordinarySignalTargetSrc)
			h := newE2EHarness(bin)
			h.waitFor(15*time.Second, protocol.EventStepped)

			Expect(h.d.Continue()).To(Succeed(), "Continue into SIGUSR1 target")
			awaitSignalExit(h.d.Events(), syscall.SIGUSR1, signalTargetExitOK)
		})
}

func assertSignalDeath(bin string, signal syscall.Signal) {
	GinkgoHelper()
	err := exec.Command(bin).Run()
	var exitErr *exec.ExitError
	Expect(errors.As(err, &exitErr)).To(BeTrue(), "%s should terminate from %s, got %v", bin, signal, err)
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	Expect(ok).To(BeTrue(), "%s returned a non-wait exit status", bin)
	Expect(status.Signaled()).To(BeTrue(), "%s exited normally with status %d", bin, status.ExitStatus())
	Expect(status.Signal()).To(Equal(signal), "%s terminated from the wrong signal", bin)
}

func awaitSignalExit(events <-chan protocol.Event, signal syscall.Signal, wantExit int) {
	GinkgoHelper()
	wantOutput := fmt.Sprintf("signal %d", signal)
	outputs := 0
	deadline := time.After(20 * time.Second)

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				Fail(fmt.Sprintf("events closed before ProcessExited after %s", signal))
			}
			switch evt.Kind {
			case protocol.EventOutput:
				var out protocol.OutputPayload
				Expect(json.Unmarshal(evt.Payload, &out)).To(Succeed(), "decode Output after %s", signal)
				if strings.HasPrefix(out.Content, "signal ") {
					Expect(out.Stream).To(Equal("stderr"), "signal output stream")
					Expect(out.Content).To(Equal(wantOutput), "reported signal")
					outputs++
				}
			case protocol.EventProcessExited:
				var exited protocol.ProcessExitedPayload
				Expect(json.Unmarshal(evt.Payload, &exited)).To(Succeed(), "decode ProcessExited after %s", signal)
				Expect(outputs).To(Equal(1),
					"%s must be reported exactly once before exit; a larger count is the refault loop", signal)
				Expect(exited.ExitCode).To(Equal(wantExit), "ProcessExited after %s", signal)
				return
			case protocol.EventError:
				Fail(fmt.Sprintf("debugger error after %s: %s", signal, evt.Payload))
			}
		case <-deadline:
			Fail(fmt.Sprintf("TIMEOUT after %s: no ProcessExited after %s (possible suppressed-signal refault loop)",
				20*time.Second, signal))
		}
	}
}

// overlapTargetSrc pounds two breakpoint-able lines from several
// LockOSThread'd goroutines at once, so a sibling thread's SIGTRAP reliably
// surfaces from Wait4(-1, …, WALL) while another thread is single-stepping off
// one of those very traps. Every spinner owns its own M and runs the SAME hot
// loop, which produces both #199 variants:
//
//   - a DISTINCT sibling (a thread trapping on OVERLAP_B while another steps
//     off OVERLAP_A), and
//   - a SAME-ADDRESS sibling (a thread trapping on OVERLAP_A in the window
//     where that trap has been restored to its original bytes for the step).
//
// OVERLAP_A contains a call so a source-level StepOver over it really has to
// step off the armed trap; OVERLAP_B is a plain statement. The per-iteration
// sleep keeps the trap rate high but bounded — a thread that has trapped stays
// ptrace-stopped until the debugger resumes it, so the number of stops in
// flight can never exceed the spinner count.
const overlapTargetSrc = `package main

import (
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

var sink int64

func work(n int64) int64 {
	s := int64(0)
	for i := int64(0); i < n; i++ {
		s += i
	}
	return s
}

func spin(id int64) {
	runtime.LockOSThread()
	x := id
	for {
		x += work(x % 5) // OVERLAP_A
		x++              // OVERLAP_B
		atomic.AddInt64(&sink, x)
		time.Sleep(300 * time.Microsecond)
	}
}

func main() {
	// Safety net: self-exit if the debugger abandons us while running.
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	runtime.GOMAXPROCS(4)
	for i := int64(0); i < 6; i++ {
		go spin(i)
	}
	for {
		time.Sleep(10 * time.Millisecond)
	}
}
`

const stepOwnerExitTargetSrc = `#include <pthread.h>
#include <stdatomic.h>
#include <stdint.h>
#include <unistd.h>

extern void die_now(void);

static _Atomic uint64_t sink;

static void *spin(void *unused) {
	(void)unused;
	for (;;) {
		atomic_fetch_add_explicit(&sink, 1, memory_order_relaxed); // STEP_EXIT_SIBLING
	}
}

static void *die(void *unused) {
	(void)unused;
	usleep(200000);
	die_now();
	return NULL;
}

int main(void) {
	pthread_t workers[8];
	pthread_t doomed;
	alarm(180);
	for (int i = 0; i < 8; i++) {
		pthread_create(&workers[i], NULL, spin, NULL);
	}
	pthread_create(&doomed, NULL, die, NULL);
	for (;;) {
		pause();
	}
}
`

const stepOwnerExitTargetAsm = `.file 1 "step_owner_exit_target.S"
.text
.globl die_now
.type die_now,@function
die_now:
	movq $60, %rax
	xorq %rdi, %rdi
.loc 1 9 0
	syscall # STEP_OWNER_EXIT
	ud2
.size die_now, .-die_now
.section .note.GNU-stack,"",@progbits
`

func buildStepOwnerExitTarget() string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	cPath := filepath.Join(dir, "step_owner_exit_target.c")
	asmPath := filepath.Join(dir, "step_owner_exit_target.S")
	Expect(os.WriteFile(cPath, []byte(stepOwnerExitTargetSrc), 0o600)).To(Succeed())
	Expect(os.WriteFile(asmPath, []byte(stepOwnerExitTargetAsm), 0o600)).To(Succeed())

	binPath := filepath.Join(dir, "step_owner_exit_target")
	cmd := exec.Command("gcc", "-g", "-O0", "-fno-omit-frame-pointer", "-no-pie",
		"-pthread", "-std=gnu11", "-o", binPath, cPath, asmPath)
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build stepped-thread-exit target:\n%s", out)
	return binPath
}

// stepOwnerHoldTargetSrc is the same raw-SYS_exit hazard as
// stepOwnerExitTargetSrc, with the manufactured sibling anchor deliberately
// removed.
//
// A spawner produces one doomed thread at a time, each of which walks into the
// same raw SYS_exit breakpoint. Nothing else in this target is breakpointed, so
// when the step owner dies there is normally NOTHING parked — which is exactly
// the case the held-owner anchor exists for. The later doomed threads are what
// prove the reinstall really happened: they hit the SAME logical breakpoint
// after the reconciliation, which is impossible if the trap was left out of the
// tracee or the entry was orphaned.
//
// The spawner is a loop rather than a fixed set of staggered threads so the next
// hit is always ~one interval away from whenever the debugger becomes ready,
// instead of being pinned to wall-clock offsets the debugger's own DWARF load
// and breakpoint round-trips can overrun.
//
// It `pthread_join`s each doomed thread before creating the next, which is what
// makes the spec deterministic rather than merely likely:
//
//   - Only ONE thread can ever be at or near the breakpoint, so no second trap
//     can be pending when the step begins — a pending trap would park and supply
//     exactly the sibling anchor this spec exists to do without.
//   - The join blocks for as long as the dying thread is held at its
//     PTRACE_EVENT_EXIT stop (the kernel clears and wakes the join futex in
//     mm_release, which is past that stop), so the NEXT doomed thread cannot
//     even be created until the engine has reinstalled and released the anchor.
//     Its later hit therefore proves the trap was physically written back.
//
// A raw SYS_exit thread is joinable: exit(2) still performs the
// CLONE_CHILD_CLEARTID futex wake that pthread_join waits on. Verified against
// glibc before this spec was written.
const stepOwnerHoldTargetSrc = `#include <pthread.h>
#include <unistd.h>

extern void die_now(void);

static void *doom(void *unused) {
	(void)unused;
	die_now();
	return NULL;
}

static void *spawner(void *unused) {
	(void)unused;
	for (;;) {
		pthread_t t;
		if (pthread_create(&t, NULL, doom, NULL) == 0) {
			pthread_join(t, NULL);
		}
		usleep(200000);
	}
}

int main(void) {
	pthread_t s;
	alarm(180);
	pthread_create(&s, NULL, spawner, NULL);
	for (;;) {
		pause();
	}
}
`

const stepOwnerHoldTargetAsm = `.file 1 "step_owner_hold_target.S"
.text
.globl die_now
.type die_now,@function
die_now:
	movq $60, %rax
	xorq %rdi, %rdi
.loc 1 9 0
	syscall # STEP_OWNER_HOLD
	ud2
.size die_now, .-die_now
.section .note.GNU-stack,"",@progbits
`

func buildStepOwnerHoldTarget() string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	cPath := filepath.Join(dir, "step_owner_hold_target.c")
	asmPath := filepath.Join(dir, "step_owner_hold_target.S")
	Expect(os.WriteFile(cPath, []byte(stepOwnerHoldTargetSrc), 0o600)).To(Succeed())
	Expect(os.WriteFile(asmPath, []byte(stepOwnerHoldTargetAsm), 0o600)).To(Succeed())

	binPath := filepath.Join(dir, "step_owner_hold_target")
	cmd := exec.Command("gcc", "-g", "-O0", "-fno-omit-frame-pointer", "-no-pie",
		"-pthread", "-std=gnu11", "-o", binPath, cPath, asmPath)
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build held-step-owner target:\n%s", out)
	return binPath
}

// declareStepOverlapSpec is the permanent regression gate for issue #199: a
// foreign thread's breakpoint stop surfacing while another thread single-steps
// off a software breakpoint.
//
// Linux-only by construction. It is the ptrace Wait4(-1, …, WALL) semantics
// that let an arbitrary thread's stop arrive mid-step; the darwin backend's
// receive loop already parks a straggler and only lets the stepping thread's
// trap complete a step, so the linux park queue has no darwin counterpart to
// exercise and the spec would be vacuous there.
//
// What it asserts is deliberately NOT the per-cycle outcome. Whether a given
// StepOver completes as Stepped or as a sibling's BreakpointHit — and how many
// siblings trap in any window — is inherently racy, and the trap really is
// temporarily absent from the tracee's memory during a step. Pinning hit counts
// would make this flaky without testing the fix. Instead it pins the invariants
// the park queue guarantees:
//
//  1. Both logical breakpoints survive every stop. Re-setting either line must
//     keep reporting "already installed": under the bug the stepped-over entry
//     was dropped from byID/byAddr with its trap disarmed, so a re-set would
//     succeed and the original id became unclearable.
//  2. Every reported BreakpointHit belongs to one of the two ids we set — a
//     lost/overwritten entry surfaces as an unknown id.
//  3. No EventError and no unexpected process exit across the whole run.
//  4. Both ids are still clearable at the end (the direct user-visible symptom
//     in #199).
//
// Non-vacuity is asserted separately: hits must be observed on BOTH lines and
// from more than one goroutine, otherwise the run never created the overlap the
// spec exists to cover.
func declareStepOverlapSpec() {
	It("keeps both breakpoints armed when a sibling thread traps mid-step", Label("overlap"), func() {
		lineA := markerLine(overlapTargetSrc, "// OVERLAP_A")
		lineB := markerLine(overlapTargetSrc, "// OVERLAP_B")
		bin := buildTarget("overlap_target", overlapTargetSrc)

		h := newE2EHarness(bin)
		h.waitFor(20*time.Second, protocol.EventStepped) // initial launch stop

		bpA, err := h.d.SetBreakpoint("overlap_target.go", lineA)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A")
		bpB, err := h.d.SetBreakpoint("overlap_target.go", lineB)
		Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B")
		known := map[int]bool{bpA.ID: true, bpB.ID: true}

		hits := map[int]int{}
		goroutines := map[int]bool{}
		stepOverDeliveredForeign := 0

		// assertBothStillArmed is the live probe for invariant 1. Re-setting an
		// installed address is rejected without touching tracee memory, so it
		// is a safe, side-effect-free way to ask the engine whether it still
		// tracks each breakpoint at this stop.
		assertBothStillArmed := func(where string) {
			GinkgoHelper()
			for _, ln := range []int{lineA, lineB} {
				_, setErr := h.d.SetBreakpoint("overlap_target.go", ln)
				Expect(setErr).To(HaveOccurred(),
					"%s: breakpoint at line %d vanished from the table", where, ln)
				Expect(setErr.Error()).To(ContainSubstring("already installed"),
					"%s: unexpected error re-setting line %d", where, ln)
			}
		}

		// record asserts the shared per-stop invariants and tallies coverage.
		record := func(evt protocol.Event, where string) {
			GinkgoHelper()
			if evt.Kind != protocol.EventBreakpointHit {
				return
			}
			var hit protocol.BreakpointHitPayload
			Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode BreakpointHit")
			Expect(known).To(HaveKey(hit.Breakpoint.ID),
				"%s: hit reports breakpoint id %d, which is neither %d nor %d "+
					"(an entry was overwritten or lost)", where, hit.Breakpoint.ID, bpA.ID, bpB.ID)
			hits[hit.Breakpoint.ID]++
			goroutines[hit.Goroutine.ID] = true
		}

		// Sized against the target's own 180s watchdog, not against wall-clock
		// preference: a cycle costs ~1.05s under -race on a fast hosted runner
		// and ~1.26s on a slow one (runners vary by ~20%; see run 31330091994,
		// where every spec was that much slower). 150 cycles fits the fast case
		// at ~158s and blows the watchdog at ~190s on the slow one, which would
		// surface as a spurious ProcessExited rather than a real regression. 90
		// keeps ~37% headroom at the slow rate while still resolving dozens of
		// step-overs as a parked sibling's breakpoint — the path under test.
		iters := envInt("BINGO_E2E_OVERLAP_ITERS", 90)
		for i := 0; i < iters; i++ {
			Expect(h.d.Continue()).To(Succeed(), "Continue #%d", i)
			evt := h.waitFor(30*time.Second,
				protocol.EventBreakpointHit, protocol.EventStepped,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Or(Equal(protocol.EventBreakpointHit), Equal(protocol.EventStepped)),
				"Continue #%d unexpected %s: %s", i, evt.Kind, evt.Payload)
			record(evt, fmt.Sprintf("after Continue #%d", i))
			assertBothStillArmed(fmt.Sprintf("after Continue #%d", i))

			// StepOver single-steps off whichever trap we are parked on; this
			// is the window in which a sibling stop must be held back.
			Expect(h.d.StepOver()).To(Succeed(), "StepOver #%d", i)
			evt = h.waitFor(30*time.Second,
				protocol.EventStepped, protocol.EventBreakpointHit,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Or(Equal(protocol.EventStepped), Equal(protocol.EventBreakpointHit)),
				"StepOver #%d unexpected %s: %s", i, evt.Kind, evt.Payload)
			if evt.Kind == protocol.EventBreakpointHit {
				stepOverDeliveredForeign++
			}
			record(evt, fmt.Sprintf("after StepOver #%d", i))
			assertBothStillArmed(fmt.Sprintf("after StepOver #%d", i))
		}

		// Invariant 4: the user-visible symptom in #199 was that the original
		// id could no longer be cleared.
		Expect(h.d.ClearBreakpoint(bpA.ID)).To(Succeed(), "ClearBreakpoint A after the run")
		Expect(h.d.ClearBreakpoint(bpB.ID)).To(Succeed(), "ClearBreakpoint B after the run")

		// Non-vacuity: without hits on both lines from several threads the run
		// never produced the concurrent stops this spec exists to cover.
		Expect(hits[bpA.ID]).To(BeNumerically(">", 0), "no hits on OVERLAP_A")
		Expect(hits[bpB.ID]).To(BeNumerically(">", 0), "no hits on OVERLAP_B")
		Expect(len(goroutines)).To(BeNumerically(">=", 2),
			"all hits came from one goroutine — no cross-thread overlap was exercised")

		// The decisive one: assert the parking path actually ran. Everything
		// above stays green if foreign stops are never held back at all, so
		// without this the spec could pass on a run that never reproduced the
		// overlap — or against a build with the rule removed.
		parked, ok := debugger.LinuxParkedStopCount(h.d)
		Expect(ok).To(BeTrue(), "parked-stop hook unavailable")
		Expect(parked).To(BeNumerically(">", 0),
			"no foreign stop was ever parked across %d cycles: this run never "+
				"exercised the rule under test", iters)
		// A sibling must also reach an auto-cleared sentinel before its delayed
		// stop is handled; otherwise the retired-byte correction is not exercised
		// against the native backend at all.
		retired, ok := debugger.LinuxRetiredInternalBreakpointCount(h.d)
		Expect(ok).To(BeTrue(), "retired internal-breakpoint hook unavailable")
		Expect(retired).To(BeNumerically(">", 0),
			"no delayed retired-sentinel hit was recovered across %d cycles", iters)

		AddReportEntry("overlap-iterations", iters)
		AddReportEntry("overlap-hits-A", hits[bpA.ID])
		AddReportEntry("overlap-hits-B", hits[bpB.ID])
		AddReportEntry("overlap-goroutines", len(goroutines))
		AddReportEntry("overlap-stepover-resolved-as-breakpoint", stepOverDeliveredForeign)
		AddReportEntry("overlap-parked-stops", parked)
		AddReportEntry("overlap-retired-internal-breakpoint-hits", retired)
	})
}

// overlapSignalTargetSrc adds a dedicated, locked signal thread and a raw
// nanosleep syscall to the overlap workload. The harness directs SIGUSR1 to the
// known signal TID after single-stepping the syscall has begun. The step thread
// blocks runtime preemption's SIGURG, and the SIGCONT storm starts only after
// this gate, so the syscall keeps the step open until the foreign signal stop is
// provably parked instead of relying on a process-directed storm to win a
// microsecond-scale race.
//
// SIGUSR1 is handled explicitly so forwarding it changes only an atomic counter.
// Its exact number is asserted by the spec, so do not swap it for another
// signal.
//
// The storm also sends SIGCONT, which the wait loop absorbs rather than
// reporting. That is the reachable trigger for the absorb-resume rule: when it
// lands on the thread being single-stepped, the absorbed stop has consumed that
// step and the wait loop must re-arm it. Continuing instead cancels the step
// with the gate still latched and the tracee freezes. SIGCONT is safe to storm
// here because nothing in the target is ever group-stopped, so it has no effect
// on a thread that receives it.
const overlapSignalTargetSrc = `package main

import (
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	sink          int64
	signalHandled int64
)

func slowStep()

func work(n int64) int64 {
	s := int64(0)
	for i := int64(0); i < n; i++ {
		s += i
	}
	return s
}

func spin(id int64) {
	runtime.LockOSThread()
	x := id
	for {
		x += work(x % 5) // OVERLAP_A
		x++              // OVERLAP_B
		atomic.AddInt64(&sink, x)
		time.Sleep(300 * time.Microsecond)
	}
}

func signalThread(ready chan<- int) {
	runtime.LockOSThread()
	ready <- syscall.Gettid()
	for atomic.LoadInt64(&signalHandled) == 0 {
		atomic.AddInt64(&sink, 1)
	}
	for {
		atomic.AddInt64(&sink, 1) // SIGNAL_THREAD_RESUMED
	}
}

func stepThread() {
	runtime.LockOSThread()
	mask := uint64(1) << (uint(syscall.SIGURG) - 1)
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_RT_SIGPROCMASK,
		0,
		uintptr(unsafe.Pointer(&mask)),
		0,
		unsafe.Sizeof(mask),
		0,
		0,
	)
	runtime.KeepAlive(&mask)
	if errno != 0 {
		os.Exit(96)
	}
	slowStep()
	for {
		time.Sleep(time.Second)
	}
}

func main() {
	go func() { time.Sleep(180 * time.Second); os.Exit(0) }()
	runtime.GOMAXPROCS(6)
	signals := make(chan os.Signal, 64)
	signal.Notify(signals, syscall.SIGUSR1)
	go func() {
		for range signals {
			atomic.StoreInt64(&signalHandled, 1)
			atomic.AddInt64(&sink, 1)
		}
	}()
	if len(os.Args) != 3 {
		os.Exit(94)
	}
	ready := make(chan int, 1)
	go signalThread(ready)
	signalTID := <-ready
	if err := os.WriteFile(os.Args[1],
		[]byte(fmt.Sprintf("%d %d\n", os.Getpid(), signalTID)), 0600); err != nil {
		os.Exit(95)
	}
	go stepThread()
	pid := os.Getpid()
	go func() {
		for {
			if _, err := os.Stat(os.Args[2]); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		for i := int64(0); i < 6; i++ {
			go spin(i)
		}
		for {
			_ = syscall.Kill(pid, syscall.SIGCONT)
			time.Sleep(1 * time.Millisecond)
		}
	}()
	for {
		time.Sleep(10 * time.Millisecond)
	}
}
`

const overlapSignalTargetAsm = `#include "textflag.h"

TEXT ·slowStep(SB), NOSPLIT, $16-0
	MOVQ $5, 0(SP)
	MOVQ $0, 8(SP)
	MOVQ $35, AX
	LEAQ 0(SP), DI
	XORQ SI, SI
	SYSCALL // OVERLAP_SIGNAL_STEP
	RET
`

func buildOverlapSignalTarget() string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	goPath := filepath.Join(dir, "overlap_signal_target.go")
	asmPath := filepath.Join(dir, "overlap_signal_target.s")
	modPath := filepath.Join(dir, "go.mod")
	Expect(os.WriteFile(goPath, []byte(overlapSignalTargetSrc), 0o600)).To(Succeed())
	Expect(os.WriteFile(asmPath, []byte(overlapSignalTargetAsm), 0o600)).To(Succeed())
	Expect(os.WriteFile(modPath, []byte("module overlap_signal_target\n\ngo 1.25\n"), 0o600)).To(Succeed())

	binPath := filepath.Join(dir, "overlap_signal_target")
	cmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", binPath, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build deterministic overlap-signal target:\n%s", out)
	return binPath
}

// overlapProbe bundles the per-stop bookkeeping the overlap specs share: the
// liveness probe that both logical breakpoints are still tracked, and the tally
// that proves the run was not vacuous.
type overlapProbe struct {
	h          *e2eHarness
	file       string
	lines      []int
	known      map[int]bool
	hits       map[int]int
	goroutines map[int]bool
	// lateGoroutines counts distinct goroutines still hitting breakpoints in
	// the final stretch of a run. A thread that was resumed incorrectly stays
	// ptrace-stopped forever, so continued breadth here is the observable proof
	// that no thread was stranded.
	lateGoroutines map[int]bool
	signalOutputs  int
	// signalValues counts each distinct signal number the engine reported.
	// The value matters, not just the count: the wait loop copies the signal
	// out of the wait status when it holds a stop back, and a delivered stop
	// that lost its number surfaces as "signal 0". Counting the numbers is what
	// makes dropping that copy a test failure rather than an invisible change.
	signalValues map[int]int
}

func newOverlapProbe(h *e2eHarness, file string, ids []int, lines []int) *overlapProbe {
	known := map[int]bool{}
	for _, id := range ids {
		known[id] = true
	}
	return &overlapProbe{
		h: h, file: file, lines: lines, known: known,
		hits: map[int]int{}, goroutines: map[int]bool{},
		lateGoroutines: map[int]bool{},
		signalValues:   map[int]int{},
	}
}

// assertArmed re-sets each breakpoint line and requires the engine to reject it
// as already installed. Re-setting an installed address never touches tracee
// memory, so this is a side-effect-free way to ask whether the entry survived
// the last stop. Under #199 the stepped-over entry was dropped from byID/byAddr
// with its trap disarmed, and this probe would succeed instead of failing.
func (p *overlapProbe) assertArmed(where string) {
	GinkgoHelper()
	for _, ln := range p.lines {
		_, err := p.h.d.SetBreakpoint(p.file, ln)
		Expect(err).To(HaveOccurred(), "%s: breakpoint at line %d vanished from the table", where, ln)
		Expect(err.Error()).To(ContainSubstring("already installed"),
			"%s: unexpected error re-setting line %d", where, ln)
	}
}

func (p *overlapProbe) record(evt protocol.Event, where string, late bool) {
	GinkgoHelper()
	if evt.Kind != protocol.EventBreakpointHit {
		return
	}
	var hit protocol.BreakpointHitPayload
	Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed(), "decode BreakpointHit")
	Expect(p.known).To(HaveKey(hit.Breakpoint.ID),
		"%s: hit reports unknown breakpoint id %d (an entry was overwritten or lost)", where, hit.Breakpoint.ID)
	p.hits[hit.Breakpoint.ID]++
	p.goroutines[hit.Goroutine.ID] = true
	if late {
		p.lateGoroutines[hit.Goroutine.ID] = true
	}
}

// awaitFrom drains a harness's event channel until one of kinds arrives. It
// takes the harness rather than a probe so a spec can wait on a tracee it holds
// no breakpoint bookkeeping for.
func awaitFrom(h *e2eHarness, timeout time.Duration, kinds ...protocol.EventKind) protocol.Event {
	GinkgoHelper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-h.d.Events():
			if !ok {
				Fail(fmt.Sprintf("events channel closed while waiting for %v", kinds))
			}
			for _, k := range kinds {
				if evt.Kind == k {
					return evt
				}
			}
		case <-deadline:
			Fail(fmt.Sprintf("TIMEOUT after %s waiting for %v (possible hang)", timeout, kinds))
		}
	}
}

// await drains until one of kinds arrives, tallying the engine's signal reports
// so a spec can prove the foreign-signal path was actually exercised.
func (p *overlapProbe) await(timeout time.Duration, kinds ...protocol.EventKind) protocol.Event {
	GinkgoHelper()
	deadline := time.After(timeout)
	for {
		select {
		case evt, ok := <-p.h.d.Events():
			if !ok {
				Fail(fmt.Sprintf("events channel closed while waiting for %v", kinds))
			}
			if evt.Kind == protocol.EventOutput {
				var out protocol.OutputPayload
				if json.Unmarshal(evt.Payload, &out) == nil && strings.HasPrefix(out.Content, "signal ") {
					p.signalOutputs++
					if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(out.Content, "signal "))); err == nil {
						p.signalValues[n]++
					}
				}
			}
			for _, k := range kinds {
				if evt.Kind == k {
					return evt
				}
			}
		case <-deadline:
			Fail(fmt.Sprintf("TIMEOUT after %s waiting for %v (possible hang)", timeout, kinds))
		}
	}
}

// setupOverlap launches src, sets breakpoints on both markers and returns a
// probe over them, already parked at the launch stop.
func setupOverlap(name, src string) (*overlapProbe, int, int) {
	GinkgoHelper()
	lineA := markerLine(src, "// OVERLAP_A")
	lineB := markerLine(src, "// OVERLAP_B")
	bin := buildTarget(name, src)

	h := newE2EHarness(bin)
	h.waitFor(20*time.Second, protocol.EventStepped)

	bpA, err := h.d.SetBreakpoint(name+".go", lineA)
	Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A")
	bpB, err := h.d.SetBreakpoint(name+".go", lineB)
	Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B")

	return newOverlapProbe(h, name+".go", []int{bpA.ID, bpB.ID}, []int{lineA, lineB}), bpA.ID, bpB.ID
}

func setupOverlapSignal() (*overlapProbe, int, int, int) {
	GinkgoHelper()
	syncDir := GinkgoT().TempDir()
	syncPath := filepath.Join(syncDir, "signal-thread")
	stormPath := filepath.Join(syncDir, "start-sigcont-storm")
	h := newE2EHarnessArgs(buildOverlapSignalTarget(), []string{syncPath, stormPath})
	h.waitFor(20*time.Second, protocol.EventStepped)

	stepLine := markerLine(overlapSignalTargetAsm, "// OVERLAP_SIGNAL_STEP")
	stepBP, err := h.d.SetBreakpoint("overlap_signal_target.s", stepLine)
	Expect(err).NotTo(HaveOccurred(), "set blocking single-step breakpoint")
	resumedLine := markerLine(overlapSignalTargetSrc, "// SIGNAL_THREAD_RESUMED")
	resumedBP, err := h.d.SetBreakpoint("overlap_signal_target.go", resumedLine)
	Expect(err).NotTo(HaveOccurred(), "set signal-thread liveness breakpoint")
	parkedSignalsBefore, ok := debugger.LinuxParkedSignalCount(h.d)
	Expect(ok).To(BeTrue(), "parked-signal hook unavailable")

	Expect(h.d.Continue()).To(Succeed(), "continue to blocking single-step breakpoint")
	evt := awaitFrom(h, 30*time.Second,
		protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
	Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
		"blocking single-step breakpoint did not stop: %s", evt.Payload)
	var hit protocol.BreakpointHitPayload
	Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed())
	Expect(hit.Breakpoint.ID).To(Equal(stepBP.ID))

	var pid, signalTID int
	syncData, err := os.ReadFile(syncPath)
	Expect(err).NotTo(HaveOccurred(), "read signal-thread identity")
	_, err = fmt.Sscanf(string(syncData), "%d %d", &pid, &signalTID)
	Expect(err).NotTo(HaveOccurred(), "decode signal-thread identity %q", syncData)
	Expect(pid).To(BeNumerically(">", 0))
	Expect(signalTID).To(BeNumerically(">", 0))
	Expect(hit.Goroutine.ThreadID).To(BeNumerically(">", 0),
		"blocking step did not report its OS thread")
	Expect(hit.Goroutine.Current).To(BeTrue(),
		"blocking step fell back to a non-current goroutine")
	Expect(hit.Goroutine.ThreadID).NotTo(Equal(signalTID),
		"the directed signal thread must be foreign to the single-step owner")

	Expect(h.d.StepInto()).To(Succeed(), "single-step blocking nanosleep syscall")
	Expect(syscall.Tgkill(pid, signalTID, syscall.SIGUSR1)).To(Succeed(),
		"direct SIGUSR1 to the known foreign thread")
	evt = awaitFrom(h, 30*time.Second,
		protocol.EventStepped, protocol.EventBreakpointHit,
		protocol.EventProcessExited, protocol.EventError)
	Expect(evt.Kind).To(Equal(protocol.EventStepped),
		"the blocking syscall step did not complete after holding the foreign signal: %s", evt.Payload)
	Expect(h.d.ClearBreakpoint(stepBP.ID)).To(Succeed(),
		"clear blocking single-step breakpoint")

	lineA := markerLine(overlapSignalTargetSrc, "// OVERLAP_A")
	lineB := markerLine(overlapSignalTargetSrc, "// OVERLAP_B")
	bpA, err := h.d.SetBreakpoint("overlap_signal_target.go", lineA)
	Expect(err).NotTo(HaveOccurred(), "SetBreakpoint A")
	bpB, err := h.d.SetBreakpoint("overlap_signal_target.go", lineB)
	Expect(err).NotTo(HaveOccurred(), "SetBreakpoint B")
	p := newOverlapProbe(h, "overlap_signal_target.go",
		[]int{bpA.ID, bpB.ID}, []int{lineA, lineB})

	Expect(h.d.Continue()).To(Succeed(), "release the held foreign signal")
	evt = p.await(30*time.Second,
		protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
	Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
		"the directed signal thread did not resume after SIGUSR1 forwarding: %s", evt.Payload)
	Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed())
	Expect(hit.Breakpoint.ID).To(Equal(resumedBP.ID),
		"the first post-signal stop must prove the exact signaled thread resumed")
	Expect(hit.Goroutine.Current).To(BeTrue(),
		"signal-thread liveness stop fell back to a non-current goroutine")
	Expect(hit.Goroutine.ThreadID).To(Equal(signalTID),
		"the liveness breakpoint ran on TID %d, want directed signal TID %d",
		hit.Goroutine.ThreadID, signalTID)
	Expect(h.d.ClearBreakpoint(resumedBP.ID)).To(Succeed(),
		"clear signal-thread liveness breakpoint")
	Expect(os.WriteFile(stormPath, nil, 0o600)).To(Succeed(),
		"start the hot-loop and stepped-thread SIGCONT workloads")
	Expect(h.d.Continue()).To(Succeed(), "continue into the hot-loop overlap workload")
	evt = p.await(30*time.Second,
		protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
	Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
		"hot-loop overlap workload did not stop: %s", evt.Payload)
	p.record(evt, "after deterministic foreign signal", false)
	return p, bpA.ID, bpB.ID, parkedSignalsBefore
}

// declareStepOverlapStepIntoSpec covers the machine-granularity step
// (bpResumeStep) rather than the source-level step-over: StepInto off an armed
// trap issues the same restore → single-step → reinstall dance, but suspends on
// completion instead of resuming, so any sibling stop held back during the step
// stays held until the NEXT resume. That is the path where a premature release
// would surface a breakpoint hit the user never continued into.
func declareStepOverlapStepIntoSpec() {
	It("holds a sibling trap across a machine step and never doubles the step completion",
		Label("overlap"), func() {
			p, _, _ := setupOverlap("overlap_stepinto_target", overlapTargetSrc)

			iters := envInt("BINGO_E2E_OVERLAP_STEPINTO_ITERS", 100)
			for i := 0; i < iters; i++ {
				where := fmt.Sprintf("cycle #%d", i)

				Expect(p.h.d.Continue()).To(Succeed(), "Continue %s", where)
				evt := p.await(30*time.Second,
					protocol.EventBreakpointHit, protocol.EventStepped,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Or(Equal(protocol.EventBreakpointHit), Equal(protocol.EventStepped)),
					"Continue %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, i >= iters*3/4)
				p.assertArmed(where)

				// StepInto off the armed trap. Exactly one stop must answer it:
				// a released sibling arriving as a second stop here would mean
				// the queue drained before the step completed.
				Expect(p.h.d.StepInto()).To(Succeed(), "StepInto %s", where)
				evt = p.await(30*time.Second,
					protocol.EventStepped, protocol.EventBreakpointHit,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Or(Equal(protocol.EventStepped), Equal(protocol.EventBreakpointHit)),
					"StepInto %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, i >= iters*3/4)
				p.assertArmed(where)
			}

			Expect(len(p.goroutines)).To(BeNumerically(">=", 2),
				"all hits came from one goroutine — no cross-thread overlap was exercised")
			AddReportEntry("overlap-stepinto-iterations", iters)
			AddReportEntry("overlap-stepinto-goroutines", len(p.goroutines))
			if parked, ok := debugger.LinuxParkedStopCount(p.h.d); ok {
				// Reported, not asserted: a plain machine step is a much
				// narrower window than the step-over spec's, so a run with no
				// park here is plausible rather than a signal that the rule
				// stopped working. The two specs that gate on it are enough.
				AddReportEntry("overlap-stepinto-parked-stops", parked)
			}
		})
}

// declareStepOverlapSignalSpec drives the foreign StopSignal case: an ordinary
// (non-trap) signal landing on a thread that is not the one being stepped.
//
// The invariant under test is that the thread the engine resumes is the thread
// that actually stopped. A held-back signal must not move the backend's resume
// target while the step is outstanding, and must move it when it is finally
// delivered. Getting that wrong resumes a thread that was never stopped and
// strands the one that was — which is why the assertion is breadth of progress
// in the final quarter of the run, not a per-stop equality.
func declareStepOverlapSignalSpec() {
	It("resumes the thread that stopped when a foreign signal lands mid-step",
		Label("overlap"), func() {
			p, bpA, bpB, parkedSignalsBefore := setupOverlapSignal()

			iters := envInt("BINGO_E2E_OVERLAP_SIGNAL_ITERS", 40)
			// Keep the machine-step burst and its existing watchdog envelope:
			// besides the directed SIGUSR1 gate above, SIGCONT still exercises
			// the stepped-thread re-arm path opportunistically across the run.
			steps := envInt("BINGO_E2E_OVERLAP_SIGNAL_STEPS", 12)
			for i := 0; i < iters; i++ {
				where := fmt.Sprintf("cycle #%d", i)
				late := i >= iters*3/4

				Expect(p.h.d.Continue()).To(Succeed(), "Continue %s", where)
				evt := p.await(30*time.Second,
					protocol.EventBreakpointHit, protocol.EventStepped,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Or(Equal(protocol.EventBreakpointHit), Equal(protocol.EventStepped)),
					"Continue %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, late)
				p.assertArmed(where)

				for s := 0; s < steps; s++ {
					stepWhere := fmt.Sprintf("%s step #%d", where, s)
					Expect(p.h.d.StepInto()).To(Succeed(), "StepInto %s", stepWhere)
					evt = p.await(30*time.Second,
						protocol.EventStepped, protocol.EventBreakpointHit,
						protocol.EventProcessExited, protocol.EventError)
					Expect(evt.Kind).To(Or(Equal(protocol.EventStepped), Equal(protocol.EventBreakpointHit)),
						"StepInto %s unexpected %s: %s", stepWhere, evt.Kind, evt.Payload)
					p.record(evt, stepWhere, late)
					p.assertArmed(stepWhere)
				}

				Expect(p.h.d.StepOver()).To(Succeed(), "StepOver %s", where)
				evt = p.await(30*time.Second,
					protocol.EventStepped, protocol.EventBreakpointHit,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).To(Or(Equal(protocol.EventStepped), Equal(protocol.EventBreakpointHit)),
					"StepOver %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, late)
				p.assertArmed(where)
			}

			Expect(p.h.d.ClearBreakpoint(bpA)).To(Succeed(), "ClearBreakpoint A after the run")
			Expect(p.h.d.ClearBreakpoint(bpB)).To(Succeed(), "ClearBreakpoint B after the run")

			// Non-vacuity: the directed setup gate must have reached the
			// debugger, otherwise this ran as a plain overlap spec.
			Expect(p.signalOutputs).To(BeNumerically(">", 0),
				"no signal stops were reported — the foreign-signal path was never exercised")

			// Integrity of the reported signal number, not just its arrival.
			// A held stop carries its signal through the queue in the StopEvent
			// the wait loop builds; if that copy is dropped the stop still
			// arrives and still counts above, but the engine reports "signal 0".
			// SIGUSR1 is the only signal that can surface here — SIGURG, SIGCONT
			// and a new thread's SIGSTOP are absorbed before classification — so
			// SIGUSR1 must be present and a zero-valued signal must not be. An
			// exact HaveLen(1) is deliberately avoided: it would flake on any
			// incidental signal without catching anything these two miss.
			Expect(p.signalValues).To(HaveKeyWithValue(int(syscall.SIGUSR1), BeNumerically(">", 0)),
				"no SIGUSR1 was reported by number; reported signals were %v", p.signalValues)
			Expect(p.signalValues).NotTo(HaveKey(0),
				"a stop was reported as \"signal 0\": the signal number was lost on its way through the wait loop (reported %v)", p.signalValues)

			// And the parking rule itself must have run. Asserted here as well
			// as in the step-over spec because this is the variant that carries
			// foreign non-suspending signal stops through the queue, not just
			// breakpoint stops.
			parked, ok := debugger.LinuxParkedStopCount(p.h.d)
			Expect(ok).To(BeTrue(), "parked-stop hook unavailable")
			Expect(parked).To(BeNumerically(">", 0),
				"no foreign stop was ever parked across %d cycles: this run never "+
					"exercised the rule under test", iters)
			// Specifically a *signal* must have gone through the queue, not just
			// a sibling trap. That is the precondition for the signal-number
			// assertions above to mean anything: it is the queued path that
			// rebuilds the StopEvent, so unless a signal was actually held back
			// a lost signal number could not have shown up.
			parkedSignals, ok := debugger.LinuxParkedSignalCount(p.h.d)
			Expect(ok).To(BeTrue(), "parked-signal hook unavailable")
			Expect(parkedSignals).To(BeNumerically(">", parkedSignalsBefore),
				"the deterministic setup gate did not hold a signal stop behind "+
					"its in-flight single-step")
			// Liveness: threads are still making progress at the end of the
			// run. A thread resumed while another was left stopped would drop
			// out permanently, since every spinner is LockOSThread'd.
			Expect(len(p.lateGoroutines)).To(BeNumerically(">=", 2),
				"fewer than two goroutines still hitting breakpoints in the final quarter — a thread was stranded")

			AddReportEntry("overlap-signal-iterations", iters)
			AddReportEntry("overlap-signal-steps-per-cycle", steps)
			AddReportEntry("overlap-signal-stops", p.signalOutputs)
			AddReportEntry("overlap-signal-goroutines", len(p.goroutines))
			AddReportEntry("overlap-signal-late-goroutines", len(p.lateGoroutines))
			// Evidence for the absorb-resume rule: the count of times an
			// absorbed stop landed on the thread being single-stepped and its
			// step had to be re-armed. Reported rather than asserted because
			// which thread a process-directed signal lands on is racy — the
			// storm makes it likely over a full run, not guaranteed. The
			// failure this rule prevents is a freeze, which a spec can only
			// observe as a timeout, so this counter is the only direct
			// evidence available that the rule fired against a real kernel.
			rearms, ok := debugger.LinuxStepRearmCount(p.h.d)
			Expect(ok).To(BeTrue(), "step-rearm hook unavailable")

			AddReportEntry("overlap-signal-parked-stops", parked)
			AddReportEntry("overlap-signal-parked-signal-stops", parkedSignals-parkedSignalsBefore)
			AddReportEntry("overlap-signal-values", fmt.Sprintf("%v", p.signalValues))
			AddReportEntry("overlap-signal-step-rearms", rearms)
		})
}

// declareStepOverlapPauseSpec races Pause against an in-flight step. Pause is a
// process-directed-by-way-of-main-thread SIGSTOP, so when the stepped thread is
// a spinner the interrupt arrives on a foreign thread and is held back until the
// step completes. The session must still end up suspended and resumable — a
// dropped or prematurely released interrupt shows up here as a hang.
//
// Non-vacuity has three layers, because every racing assertion here also holds
// on a run that never raced anything:
//
//   - Pause must be accepted at least once (the engine was still running, so a
//     step really was in flight).
//   - The interrupt must be HELD by the park queue at least once. Pause targets
//     the main thread, which never traps in this target, and SIGURG/SIGCONT/a
//     new thread's initial SIGSTOP are absorbed before classification, so a
//     parked signal stop can only be this interrupt arriving mid-step.
//   - The interrupt must SURFACE as EventPaused at least once during the race,
//     proving it is delivered rather than swallowed.
//   - Delivery with nothing competing is pinned separately, by
//     declareStepOverlapPauseDeliverySpec, which needs a tracee of its own.
//
// The EventPaused count is asserted only across the whole run, never per cycle:
// a held interrupt surfaces when it drains while manualStopPending is still set
// (the usual case for a source-level step-over, which releases the queue and
// then runs on to the next line), and is legitimately suppressed when a
// self-stop completes the step first. That is the documented pending-interrupt
// race and it must stay tolerated.
func declareStepOverlapPauseSpec() {
	It("stays resumable when a Pause interrupt races an in-flight step",
		Label("overlap"), func() {
			p, idA, idB := setupOverlap("overlap_pause_target", overlapTargetSrc)

			// Sized for statistical power against the target's own 180s
			// watchdog, not for speed. Only ~10-22% of cycles land the
			// interrupt inside the machine-step window (measured: 9/40, 5/40,
			// 4/40 across three native runs), so at 40 cycles the held-interrupt
			// assertion below would itself flake at roughly 0.9^40 ~ 1.5%. 70
			// cycles takes that to ~0.06% while costing ~88s at the slowest
			// per-cycle rate yet observed (1.25s under -race) — still ~2x inside
			// the watchdog, so this must not be raised much further.
			iters := envInt("BINGO_E2E_OVERLAP_PAUSE_ITERS", 70)
			paused := 0
			pausedEvents := 0
			heldInterrupts := 0
			for i := 0; i < iters; i++ {
				where := fmt.Sprintf("cycle #%d", i)

				Expect(p.h.d.Continue()).To(Succeed(), "Continue %s", where)
				evt := p.await(30*time.Second,
					protocol.EventBreakpointHit, protocol.EventStepped,
					protocol.EventPaused, protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).NotTo(Or(Equal(protocol.EventProcessExited), Equal(protocol.EventError)),
					"Continue %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				p.record(evt, where, false)
				p.assertArmed(where)

				Expect(p.h.d.StepOver()).To(Succeed(), "StepOver %s", where)
				// Pause immediately: the step may already have completed and
				// suspended, in which case Pause is legitimately rejected.
				heldBefore, _ := debugger.LinuxParkedSignalCount(p.h.d)
				accepted := false
				if err := p.h.d.Pause(); err != nil {
					Expect(err).To(MatchError(debugger.ErrNotRunning),
						"Pause %s failed for an unexpected reason", where)
				} else {
					accepted = true
					paused++
				}

				evt = p.await(30*time.Second,
					protocol.EventStepped, protocol.EventBreakpointHit, protocol.EventPaused,
					protocol.EventProcessExited, protocol.EventError)
				Expect(evt.Kind).NotTo(Or(Equal(protocol.EventProcessExited), Equal(protocol.EventError)),
					"StepOver+Pause %s unexpected %s: %s", where, evt.Kind, evt.Payload)
				if evt.Kind == protocol.EventPaused {
					pausedEvents++
				}
				p.record(evt, where, false)
				p.assertArmed(where)

				// The interrupt lands on the main thread, which is never the
				// thread being stepped here (only the spinners trap), so a
				// signal stop appearing in the queue across this window is the
				// Pause interrupt being received and held BY the park queue
				// while the step was still in flight. Nothing else can produce
				// one: SIGURG, SIGCONT and a new thread's initial SIGSTOP are
				// all absorbed before classification.
				if heldAfter, ok := debugger.LinuxParkedSignalCount(p.h.d); ok && accepted && heldAfter > heldBefore {
					heldInterrupts++
				}
			}

			// Still fully resumable after all that racing.
			Expect(p.h.d.Continue()).To(Succeed(), "final Continue")
			evt := p.await(30*time.Second,
				protocol.EventBreakpointHit, protocol.EventStepped, protocol.EventPaused,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).NotTo(Or(Equal(protocol.EventProcessExited), Equal(protocol.EventError)),
				"final Continue unexpected %s: %s", evt.Kind, evt.Payload)

			// Both logical breakpoints survived the whole race and are still
			// clearable by their original id — the integrity invariant issue
			// #199 broke, checked here as everywhere else in this label.
			for _, id := range []int{idA, idB} {
				Expect(p.h.d.ClearBreakpoint(id)).To(Succeed(), "clear bp %d after the race", id)
			}

			// The deterministic end-to-end delivery check lives elsewhere: the
			// racing loop's EventPaused count is reliably non-zero but is not
			// guaranteed per cycle (see below), so it cannot stand alone.
			// It is a spec of its own — declareStepOverlapPauseDeliverySpec —
			// because it needs a tracee that never had a breakpoint, and two
			// live tracees cannot share this process (see that spec).

			AddReportEntry("overlap-pause-iterations", iters)
			AddReportEntry("overlap-pause-accepted", paused)
			AddReportEntry("overlap-pause-events-during-race", pausedEvents)
			AddReportEntry("overlap-pause-interrupts-held-mid-step", heldInterrupts)
			if parked, ok := debugger.LinuxParkedStopCount(p.h.d); ok {
				AddReportEntry("overlap-pause-parked-stops", parked)
			}
			if held, ok := debugger.LinuxParkedSignalCount(p.h.d); ok {
				AddReportEntry("overlap-pause-parked-signal-stops", held)
			}

			// Non-vacuity, in three independent parts. Every one of them is
			// COUNTED above and ASSERTED here — reporting a count without
			// asserting it pins nothing about future runs, which is exactly
			// how this spec was vacuous before.
			//
			// 1. Pause was accepted at all — proof the engine was still running
			//    when the interrupt was issued, i.e. a step really was in
			//    flight. Without this the loop degenerates into a plain step
			//    loop and every other assertion still passes.
			Expect(paused).To(BeNumerically(">", 0),
				"no Pause was accepted across %d cycles: every one raced a step that had already "+
					"suspended, so this spec degenerated into a plain step loop and proved nothing "+
					"about Pause racing an in-flight step", iters)

			// 2. The interrupt actually reached the backend DURING a step and
			//    was held by the park queue. This is the strong observable:
			//    Pause targets the main thread, which is never the stepped
			//    thread here (only the spinners trap), and SIGURG, SIGCONT and
			//    a new thread's initial SIGSTOP are absorbed before
			//    classification — so a signal stop entering the queue can only
			//    be this interrupt. An accepted Pause whose signal never
			//    reached the queue would mean the overlap never actually
			//    happened.
			Expect(heldInterrupts).To(BeNumerically(">", 0),
				"%d Pause calls were accepted but no interrupt was ever held back mid-step: the "+
					"signal never raced an in-flight single-step, so the park path this spec "+
					"covers was never exercised", paused)

			// 3. The interrupt was actually SURFACED as EventPaused during the
			//    race, not merely accepted and swallowed. A held interrupt
			//    surfaces when it drains while manualStopPending is still set —
			//    which is the common case for a source-level step-over, because
			//    the queue is released once the machine step is done and the
			//    engine is running on to the next line, well before the
			//    EventStepped that would clear the flag. It is suppressed when
			//    a self-stop completes the step first, which is the documented
			//    pending-interrupt race and legitimate per cycle. So this is
			//    asserted only across the whole run, never per cycle.
			Expect(pausedEvents).To(BeNumerically(">", 0),
				"%d Pause calls were accepted and %d interrupts were held mid-step, but not one "+
					"ever surfaced as EventPaused across %d cycles: the interrupt is being "+
					"dropped rather than delivered", paused, heldInterrupts, iters)
		})
}

// declareStepOverlapPauseDeliverySpec is the deterministic backstop for the
// racing pause spec above: it proves an accepted Pause really does surface as
// EventPaused, with nothing else able to stop the tracee.
//
// It needs a tracee that never had a breakpoint, and it gets one by being a
// separate spec rather than a phase of the racing one. Two earlier shapes both
// failed natively and are worth recording, since both look obviously fine:
//
//  1. Clear this spec's two breakpoints and pause the same tracee. Clearing is
//     not enough — the engine is parked on one of them, and a Continue from a
//     breakpoint goes through the step-off path, whose reinstall re-arms the
//     trap and re-adds it to the table under the same id (the documented
//     clear-while-parked behaviour). A spinner hits it and the pause loses to a
//     BreakpointHit.
//  2. Launch a second tracee while the first is still alive. Before the
//     process-global exact-TID wait broker, per-session Wait4(-1, …, WALL)
//     callers stole each other's stops. Dedicated wait-ownership specs now
//     exercise concurrent sessions; this focused control still uses one tracee
//     so only Pause delivery can stop it.
//
// As its own It, Ginkgo has already run the previous spec's DeferCleanup Kill,
// so this tracee is alone. It never sets a breakpoint, so the SIGSTOP that
// Pause directs at the main thread is the only stop that can occur and
// EventPaused is strictly required.
func declareStepOverlapPauseDeliverySpec() {
	It("delivers EventPaused when nothing competes with the interrupt",
		Label("linux", "overlap"), func() {
			h := newE2EHarness(buildTarget("overlap_pause_quiet", overlapTargetSrc))
			h.waitFor(20*time.Second, protocol.EventStepped)
			Expect(h.d.Continue()).To(Succeed(), "Continue a breakpoint-free tracee")

			Eventually(func() error { return h.d.Pause() }, 30*time.Second, 50*time.Millisecond).
				Should(Succeed(), "Pause was never accepted against a freely running tracee")

			evt := awaitFrom(h, 30*time.Second,
				protocol.EventPaused, protocol.EventBreakpointHit, protocol.EventStepped,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventPaused),
				"pausing a freely running tracee with no breakpoints must surface EventPaused, got %s: %s",
				evt.Kind, evt.Payload)
		})
}

// declareStepOverlapKillSpec tears the session down at the worst moment: right
// after a step was armed, while sibling stops are most likely being held. Kill
// must return promptly and the held stops must be discarded rather than acted
// on — every parked thread is dead by then, so any post-exit work against one
// would be a ptrace call into a dead TID.
func declareStepOverlapKillSpec() {
	It("tears down cleanly when killed with sibling stops held back",
		Label("overlap"), func() {
			p, _, _ := setupOverlap("overlap_kill_target", overlapTargetSrc)

			// Warm up so several threads are cycling through both traps and the
			// queue is genuinely in play by the time we kill.
			warmup := envInt("BINGO_E2E_OVERLAP_KILL_WARMUP", 15)
			for i := 0; i < warmup; i++ {
				where := fmt.Sprintf("warmup #%d", i)
				Expect(p.h.d.Continue()).To(Succeed(), "Continue %s", where)
				evt := p.await(30*time.Second,
					protocol.EventBreakpointHit, protocol.EventStepped,
					protocol.EventProcessExited, protocol.EventError)
				p.record(evt, where, false)
				Expect(p.h.d.StepOver()).To(Succeed(), "StepOver %s", where)
				evt = p.await(30*time.Second,
					protocol.EventStepped, protocol.EventBreakpointHit,
					protocol.EventProcessExited, protocol.EventError)
				p.record(evt, where, false)
			}

			// Arm one more step and kill without waiting for it to land.
			Expect(p.h.d.StepOver()).To(Succeed(), "StepOver before kill")

			done := make(chan error, 1)
			go func() { done <- p.h.d.Kill() }()
			select {
			case err := <-done:
				Expect(err).NotTo(HaveOccurred(), "Kill with held stops")
			case <-time.After(15 * time.Second):
				Fail("Kill did not return within 15s — teardown wedged with stops held back")
			}

			// The engine closes its event stream once teardown completes.
			deadline := time.After(15 * time.Second)
			for closed := false; !closed; {
				select {
				case _, ok := <-p.h.d.Events():
					if !ok {
						closed = true
					}
				case <-deadline:
					Fail("event stream still open 15s after Kill — engine did not shut down")
				}
			}

			Expect(len(p.goroutines)).To(BeNumerically(">=", 2),
				"all hits came from one goroutine — no cross-thread overlap was exercised before the kill")
			AddReportEntry("overlap-kill-warmup", warmup)
			AddReportEntry("overlap-kill-goroutines", len(p.goroutines))
		})
}

// declareStepOwnerExitSpec drives the abnormal end of the step-off transaction:
// the breakpoint instruction is a raw SYS_exit, so the exact thread executing
// the restored instruction dies instead of producing StopSingleStep. Hot sibling
// threads trap while that transaction is open. Their stop must remain queued
// until the engine has reinstalled the original logical breakpoint.
//
// Which thread anchors that reconciliation is racy and both answers are correct:
// a sibling that has already parked lends its TID, and otherwise the dying owner
// is held as its own anchor. The non-vacuity gate is therefore that an anchor of
// SOME kind was established, not specifically a parked one — the owner-held path
// resolves the whole transaction in microseconds, so on a fast runner no sibling
// ever needs to park here. declareStepOwnerHoldSpec pins the owner-held path
// exclusively; this spec's own content is that the stop which follows the death
// still carries the SIBLING breakpoint's identity.
func declareStepOwnerExitSpec() {
	It("reconciles the removed breakpoint before surfacing a stop held behind a dead step owner",
		Label("overlap"), func() {
			deathLine := markerLine(stepOwnerExitTargetAsm, "# STEP_OWNER_EXIT")
			siblingLine := markerLine(stepOwnerExitTargetSrc, "// STEP_EXIT_SIBLING")
			h := newE2EHarness(buildStepOwnerExitTarget())
			h.waitFor(20*time.Second, protocol.EventStepped)

			deathBP, err := h.d.SetBreakpoint("step_owner_exit_target.S", deathLine)
			Expect(err).NotTo(HaveOccurred(), "set raw thread-exit breakpoint")
			Expect(h.d.Continue()).To(Succeed())
			evt := awaitFrom(h, 30*time.Second,
				protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"raw thread-exit breakpoint did not stop: %s", evt.Payload)
			var hit protocol.BreakpointHitPayload
			Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed())
			Expect(hit.Breakpoint.ID).To(Equal(deathBP.ID))

			siblingBP, err := h.d.SetBreakpoint("step_owner_exit_target.c", siblingLine)
			Expect(err).NotTo(HaveOccurred(), "set sibling breakpoint")
			Expect(h.d.Continue()).To(Succeed(),
				"single-step the raw SYS_exit instruction")
			evt = awaitFrom(h, 30*time.Second,
				protocol.EventBreakpointHit, protocol.EventStepped,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"held sibling stop did not surface after reconciliation: %s", evt.Payload)
			Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed())
			Expect(hit.Breakpoint.ID).To(Equal(siblingBP.ID),
				"the stop following thread death must retain the sibling breakpoint identity")

			_, err = h.d.SetBreakpoint("step_owner_exit_target.S", deathLine)
			Expect(err).To(MatchError(ContainSubstring("already installed")),
				"the dead owner must not orphan the original logical breakpoint")

			exits, ok := debugger.LinuxStepThreadExitCount(h.d)
			Expect(ok).To(BeTrue(), "stepped-thread-exit hook unavailable")
			Expect(exits).To(BeNumerically(">", 0),
				"the target never killed the exact thread under single-step")
			parked, ok := debugger.LinuxParkedStopCount(h.d)
			Expect(ok).To(BeTrue(), "parked-stop hook unavailable")
			held, ok := debugger.LinuxHeldStepOwnerCount(h.d)
			Expect(ok).To(BeTrue(), "held-step-owner hook unavailable")
			Expect(parked+held).To(BeNumerically(">", 0),
				"the dead step owner's transaction was never anchored on a stopped "+
					"thread, so nothing proves the reinstall had a legal write target")

			Expect(h.d.ClearBreakpoint(deathBP.ID)).To(Succeed(),
				"clear reconciled thread-exit breakpoint")
			Expect(h.d.ClearBreakpoint(siblingBP.ID)).To(Succeed(),
				"clear sibling breakpoint")
			AddReportEntry("overlap-step-owner-exits", exits)
			AddReportEntry("overlap-step-owner-exit-parked-stops", parked)
			AddReportEntry("overlap-step-owner-exit-held-owners", held)
		})
}

// declareStepOwnerHoldSpec is the empty-queue half of the same abnormal
// transaction, and the only spec that exercises the held-owner anchor against a
// real kernel.
//
// declareStepOwnerExitSpec manufactures the anchor: it arms a hot sibling line
// before resuming, so by the time the step owner dies a sibling stop is already
// parked and lends its TID. That is a legitimate path, but it means that spec
// passes identically whether or not the held-owner rule exists. Here the ONLY
// armed breakpoint is the raw SYS_exit itself, so when its thread dies mid-step
// the queue is empty and the dying owner has to anchor its own reconciliation —
// held at its PTRACE_EVENT_EXIT stop, written through, then released.
//
// What makes it mutation-sensitive rather than merely green:
//
//   - A LATER doomed thread must hit the SAME logical breakpoint id. Its thread
//     cannot even be created until the held owner is released (the spawner joins
//     each doomed thread, and the join futex is only woken past the exit stop),
//     so that hit is proof the trap was written back into the tracee through the
//     held owner and the entry stayed in the table. Drop the hold and the
//     reinstall has no anchor at all; skip the reinstall and the second hit
//     never comes.
//   - LinuxHeldStepOwnerCount must have advanced, or the run reconciled through
//     some parked sibling and proves nothing about this path. The parked-sibling
//     path cannot increment it.
//   - LinuxStepThreadExitCount must have advanced, or no step owner ever died.
//
// The parked count is deliberately NOT asserted: an empty queue is the premise.
func declareStepOwnerHoldSpec() {
	It("anchors reconciliation on the dying step owner when no sibling stop is held",
		Label("overlap"), func() {
			deathLine := markerLine(stepOwnerHoldTargetAsm, "# STEP_OWNER_HOLD")
			h := newE2EHarness(buildStepOwnerHoldTarget())
			h.waitFor(20*time.Second, protocol.EventStepped)

			deathBP, err := h.d.SetBreakpoint("step_owner_hold_target.S", deathLine)
			Expect(err).NotTo(HaveOccurred(), "set raw thread-exit breakpoint")

			Expect(h.d.Continue()).To(Succeed())
			evt := awaitFrom(h, 30*time.Second,
				protocol.EventBreakpointHit, protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"raw thread-exit breakpoint did not stop: %s", evt.Payload)
			var hit protocol.BreakpointHitPayload
			Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed())
			Expect(hit.Breakpoint.ID).To(Equal(deathBP.ID))

			// Resuming single-steps the restored SYS_exit, so this exact thread
			// dies instead of completing its step — with nothing parked behind
			// it. The next stop can only be a LATER doomed thread arriving at
			// the same breakpoint, which requires the reinstall to have gone
			// through the held owner.
			Expect(h.d.Continue()).To(Succeed(),
				"single-step the raw SYS_exit instruction")
			evt = awaitFrom(h, 60*time.Second,
				protocol.EventBreakpointHit, protocol.EventStepped,
				protocol.EventProcessExited, protocol.EventError)
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointHit),
				"no later thread reached the breakpoint after the step owner died: %s", evt.Payload)
			Expect(json.Unmarshal(evt.Payload, &hit)).To(Succeed())
			Expect(hit.Breakpoint.ID).To(Equal(deathBP.ID),
				"the reinstalled breakpoint must keep its original identity")

			_, err = h.d.SetBreakpoint("step_owner_hold_target.S", deathLine)
			Expect(err).To(MatchError(ContainSubstring("already installed")),
				"the dead owner must not orphan the original logical breakpoint")

			held, ok := debugger.LinuxHeldStepOwnerCount(h.d)
			Expect(ok).To(BeTrue(), "held-step-owner hook unavailable")
			Expect(held).To(BeNumerically(">", 0),
				"the run never held a dying step owner as its own anchor, so it says "+
					"nothing about the empty-queue reconciliation path")
			exits, ok := debugger.LinuxStepThreadExitCount(h.d)
			Expect(ok).To(BeTrue(), "stepped-thread-exit hook unavailable")
			Expect(exits).To(BeNumerically(">", 0),
				"the target never killed the exact thread under single-step")

			Expect(h.d.ClearBreakpoint(deathBP.ID)).To(Succeed(),
				"the reconciled breakpoint must still be clearable by its id")
			AddReportEntry("overlap-held-step-owners", held)
			AddReportEntry("overlap-held-step-owner-exits", exits)
		})
}

// dethreadTargetSrc makes a NON-LEADER thread call execve().
//
// The kernel's de_thread() then retires the old leader: the execing thread takes
// over the leader's pid, and the retired task's exit is reported to the tracer as
// PTRACE_EVENT_EXIT under tid == pid — while the process is very much alive and
// about to run a new image. Its GETEVENTMSG status decodes to an ordinary
// "exited, code 0", so nothing at that stop distinguishes it from a real exit.
//
// The idle threads exist so the execve genuinely goes through de_thread rather
// than the single-threaded fast path.
const dethreadTargetSrc = `#include <pthread.h>
#include <unistd.h>

static void *idle(void *unused) {
	(void)unused;
	for (;;) { usleep(20000); }
	return NULL;
}

static void *execer(void *unused) {
	(void)unused;
	usleep(400000);
	char *argv[] = {"/bin/true", NULL};
	char *envp[] = {NULL};
	execve("/bin/true", argv, envp);
	_exit(9);
	return NULL;
}

int main(void) {
	pthread_t i1, i2, e;
	alarm(180);
	pthread_create(&i1, NULL, idle, NULL);
	pthread_create(&i2, NULL, idle, NULL);
	pthread_create(&e, NULL, execer, NULL);
	for (;;) { pause(); }
}
`

func buildDethreadTarget() string {
	GinkgoHelper()
	dir := GinkgoT().TempDir()
	src := filepath.Join(dir, "dethread_target.c")
	Expect(os.WriteFile(src, []byte(dethreadTargetSrc), 0o600)).To(Succeed())
	bin := filepath.Join(dir, "dethread_target")
	cmd := exec.Command("gcc", "-g", "-O0", "-no-pie", "-pthread", "-std=gnu11", "-o", bin, src)
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "build de_thread target:\n%s", out)
	return bin
}

// declareLeaderRetirementSpec is the native proof for the leader-retirement
// rule, and the only place the real de_thread() sequence is driven through the
// whole engine.
//
// Before the fix, the retired leader's PTRACE_EVENT_EXIT was taken as proof of
// whole-process death: the backend purged, read the status, and returned a
// terminal, so the engine reported EventProcessExited with exit code 0 for a
// process that was alive and had just replaced its image. The exec that follows
// was never even seen.
//
// The assertion is deliberately about WHICH terminal is reported, not merely
// that the session ends. Both the buggy and the fixed build end the session, so
// "it stopped" proves nothing; only the kind distinguishes them. A false
// ProcessExited(0) is the bug, an EventError naming the image replacement is the
// fix.
func declareLeaderRetirementSpec() {
	It("reports an image replacement, not a process exit, when a non-leader execs",
		Label("overlap"), func() {
			h := newE2EHarness(buildDethreadTarget())
			h.waitFor(20*time.Second, protocol.EventStepped)

			Expect(h.d.Continue()).To(Succeed())
			evt := awaitFrom(h, 60*time.Second,
				protocol.EventError, protocol.EventProcessExited,
				protocol.EventBreakpointHit, protocol.EventStepped)

			Expect(evt.Kind).NotTo(Equal(protocol.EventProcessExited),
				"the retired leader's PTRACE_EVENT_EXIT was reported as the process "+
					"exiting, but de_thread() only retired it — the process is alive "+
					"under the same pid running a new image: %s", evt.Payload)
			Expect(evt.Kind).To(Equal(protocol.EventError),
				"a post-startup exec must invalidate the session explicitly: %s", evt.Payload)

			var errPayload protocol.ErrorPayload
			Expect(json.Unmarshal(evt.Payload, &errPayload)).To(Succeed())
			Expect(errPayload.Message).To(ContainSubstring("process image"),
				"the failure must name the image replacement rather than a bare wait error")
			AddReportEntry("overlap-leader-retirement-message", errPayload.Message)
		})
}
