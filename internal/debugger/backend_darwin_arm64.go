//go:build darwin && arm64 && bingonative

package debugger

// Darwin/arm64 (Apple Silicon) Backend. Self-contained: process lifecycle via
// ptrace, registers via Mach thread_get_state, memory via mach_vm_read/write.
//
// Requires com.apple.security.cs.debugger entitlement (or SIP disabled).
// Cannot be cross-compiled from non-macOS — cgo needs the macOS SDK.

/*
#cgo LDFLAGS: -framework CoreFoundation

#include "mach_darwin_arm64.h"
*/
import "C"

import (
	"debug/macho"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

func newBackend() Backend {
	return &darwinBackend{}
}

type darwinBackend struct {
	pid      int
	stepMu   sync.Mutex
	stepping bool
	stepTID  int // Mach thread port saved by SingleStep, reused on the step trap

	// taskPort caches the Mach task port obtained from task_for_pid. It is
	// acquired lazily on first use (after exec, so past the fork/exec race) and
	// reused for the process's lifetime. Re-calling task_for_pid on every memory
	// read/write/thread enumeration — as the code originally did — intermittently
	// wedges IN THE KERNEL on a busy tracer: the send right is never released and
	// the repeated port insertions eventually block task_for_pid indefinitely,
	// hanging the debugger even though the tracee is healthy. Delve and LLDB both
	// acquire the task port exactly once and cache it for this reason.
	taskMu   sync.Mutex
	taskPort C.mach_port_t
	taskOK   bool

	// One retained send-right per live tracee thread, keyed by Mach port name.
	// task_threads hands out a fresh send-right per thread on every call; we
	// keep exactly one and deallocate the rest (and the rights of threads that
	// have exited) so the debugger doesn't leak ports under thread churn and so
	// a given thread keeps a stable port name across enumerations.
	threadMu    sync.Mutex
	threadPorts map[C.mach_port_t]struct{}

	// Atomic step-over-breakpoint bookkeeping.
	//
	// suspended and the suspend/resume of held threads are touched ONLY from the
	// engine loop goroutine (singleStepThread before waitLoop starts, and
	// endThreadStep after the stopCh hand-back), so they need no lock.
	//
	// The remaining fields are shared: advanceStepOver drives the step on the
	// waitLoop goroutine while endThreadStep may run concurrently on the engine
	// loop goroutine if a Kill lands mid-step. They are guarded by stepMu.
	// sobActive gates the step-over path in Wait; sobAddr is the disarmed
	// breakpoint address the stepped thread must retire past; sobBranch records
	// whether the instruction there alters control flow (so we can't assume it
	// retires to sobAddr+4); sobSteps bounds the retry loop that steps through
	// signal-handler excursions; stepThreadPort is the thread armed with
	// hardware single-step that must be disarmed afterwards.
	suspended      []int
	stepThreadPort int
	sobActive      bool
	sobAddr        uint64
	sobBranch      bool
	sobSteps       int

	// Diagnostic-only (BINGO_DARWIN_SUSPEND_PROBE): waitStartNanos records when
	// Wait() blocked in wait4 (0 = not blocked). A watchdog goroutine, started
	// once, reads it to detect a wedge and dump the task/thread suspend state.
	// suspendProbeOn caches the env gate so a disabled probe adds no per-wait
	// work beyond one bool check.
	waitStartNanos int64
	probeOnce      sync.Once
	suspendProbeOn bool
}

// Darwin ptrace request codes from <sys/ptrace.h>. PT_ATTACH (10) makes the
// stop wait4-able by PID. Do NOT use PT_ATTACHEXC (14) — Mach exceptions are
// incompatible with our wait4-based Wait loop.
const (
	ptDarwinContinue = uintptr(7)
	ptDarwinStep     = uintptr(9)
	ptDarwinAttach   = uintptr(10)
	ptDarwinDetach   = uintptr(11)
)

// darwinPauseSignal is the signal StopProcess sends to interrupt a running
// tracee for Pause. It must be a CATCHABLE signal (so it surfaces through the
// wait4 loop as a signal-delivery stop; SIGSTOP does not — see StopProcess) and
// one Wait does not special-case (SIGTRAP is breakpoint/step; SIGURG/SIGWINCH
// are re-delivered), leaving the StopSignal fall-through. SIGUSR2 fits: it is
// never terminal-generated (unlike SIGINT/SIGTSTP), the Go runtime does not use
// it for itself, and the engine suppresses it on resume so the tracee never
// actually receives it. A genuine SIGUSR2 sent to the tracee while no Pause is
// pending is silently discarded (like a stray SIGSTOP on linux); the tradeoff
// is documented in AGENTS.md → Pause.
const darwinPauseSignal = syscall.SIGUSR2

// The two darwin/arm64 step-over correctness fixes are independently toggleable
// so an operator can run the behavior-preserving atomic step-over (#1) without
// the async-preemption workaround (#2), which alters the tracee's goroutine
// scheduling. See singleStepThread (#1) and withDarwinAsyncPreemptOff (#2).
//
//	BINGO_DARWIN_ATOMIC_STEPOVER     default ON  — "0" disables (falls back to a
//	                                 plain per-process single-step; reproduces
//	                                 the original wrong-thread hang; ablation
//	                                 only).
//	BINGO_DARWIN_ASYNC_PREEMPT_OFF   default OFF — "1" injects
//	                                 GODEBUG=asyncpreemptoff=1 into the tracee.
//
// Fix #2 defaults OFF because bingo is a visual concurrency debugger: disabling
// async preemption changes the very scheduling behavior a user may be trying to
// observe. It is offered as an opt-in reliability knob for the residual
// SIGURG-misdirection wedge under heavy step-over churn.
const asyncPreemptOffDefault = false

func darwinAtomicStepOverEnabled() bool {
	return os.Getenv("BINGO_DARWIN_ATOMIC_STEPOVER") != "0"
}

func darwinAsyncPreemptOffEnabled() bool {
	switch os.Getenv("BINGO_DARWIN_ASYNC_PREEMPT_OFF") {
	case "1":
		return true
	case "0":
		return false
	default:
		return asyncPreemptOffDefault
	}
}

// darwinSuspendProbeEnabled gates a diagnostic-only watchdog that, on a wedge,
// reads and logs the tracee's TASK-level Mach suspend_count plus per-thread
// run_state/suspend/PC. It never changes control flow and only does work after
// a multi-second wait4 stall, so it does not perturb the pre-wedge timing that
// the race depends on.
func darwinSuspendProbeEnabled() bool {
	return os.Getenv("BINGO_DARWIN_SUSPEND_PROBE") == "1"
}

func ptrace(request, pid, addr, data uintptr) error {
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PTRACE,
		request, pid, addr, data,
		0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

func startTracedProcess(_ Backend, binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	cmd.Env = append(os.Environ(), env...)
	if darwinAsyncPreemptOffEnabled() {
		cmd.Env = withDarwinAsyncPreemptOff(cmd.Env)
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("exec %q: %w", binaryPath, err)
	}
	pid := cmd.Process.Pid
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("wait for initial stop: %w", err)
	}
	if !ws.Stopped() {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("expected initial SIGTRAP, got: %v", ws)
	}
	return pid, cmd, nil
}

// withDarwinAsyncPreemptOff merges GODEBUG=asyncpreemptoff=1 into the tracee's
// environment. It is applied only when fix #2 is enabled
// (darwinAsyncPreemptOffEnabled); see the toggle block above for why it is
// opt-in. This is a darwin-specific correctness workaround, NOT a perf tweak.
//
// Go async preemption sends a THREAD-directed SIGURG (pthread_kill to a
// specific M) at a runtime-chosen moment. In our wait4/BSD-stop model that
// SIGURG surfaces as a ptrace stop and we must re-inject it to keep the runtime
// scheduling — but the only tool wait4 gives us is ptrace(PT_CONTINUE/PT_STEP,
// pid, …, SIGURG), which XNU implements as psignal(): a PROCESS-directed signal
// delivered to the first unblocked thread of the task (mach_process.c). That is
// almost never the M the runtime intended. The misdirected SIGURG (a) spuriously
// interrupts an unrelated M parked in __psynch_cvwait, racing the pthread
// condvar's __psynch_cvsignal into a rare lost-wakeup deadlock, and (b) leaves
// the intended M's runtime signalPending=1 forever (doSigPreempt clears the
// counter on the wrong M), desyncing the process-global pendingPreemptSignals.
// The result is an intermittent hang (~1 in 30 step-overs at iters=100).
//
// The wait4 model fundamentally cannot re-deliver a thread-directed signal:
// PT_THUPDATE can set a per-thread siglist bit but cannot resume a wait4 stop
// (no wakeup(&t->sigwait)), and the Mach-exception model real darwin debuggers
// use (LLDB debugserver, Delve's macnative PT_SIGEXC path) is explicitly out of
// scope here (AGENTS.md: PT_ATTACHEXC is incompatible with our wait4 loop).
// Disabling async preemption in the tracee removes the thread-directed SIGURG
// entirely (runtime.preemptone skips preemptM when asyncpreemptoff==1), so no
// signal is ever misdirected and the runtime's preempt bookkeeping stays
// consistent. The tracee falls back to cooperative preemption (function-
// prologue and loop safepoints), which is sufficient for debugging; the only
// loss is async-preemption of pathological safepoint-free tight loops.
//
// Linux does not need this: ptrace signal delivery there is per-thread, so the
// SIGURG is re-delivered to the correct thread.
func withDarwinAsyncPreemptOff(env []string) []string {
	const key = "asyncpreemptoff"
	out := make([]string, 0, len(env)+1)
	godebug := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "GODEBUG=") {
			// Last GODEBUG wins; collapse to a single merged entry below.
			godebug = strings.TrimPrefix(kv, "GODEBUG=")
			continue
		}
		out = append(out, kv)
	}
	// Respect an explicit asyncpreemptoff the caller already set (either value);
	// otherwise force it on.
	if !godebugHasKey(godebug, key) {
		if godebug == "" {
			godebug = key + "=1"
		} else {
			godebug += "," + key + "=1"
		}
	}
	return append(out, "GODEBUG="+godebug)
}

// godebugHasKey reports whether a comma-separated GODEBUG value already sets key.
func godebugHasKey(godebug, key string) bool {
	for _, f := range strings.Split(godebug, ",") {
		if k, _, ok := strings.Cut(f, "="); ok && k == key {
			return true
		}
	}
	return false
}

func attachToProcess(_ Backend, pid int) error {
	if err := ptrace(ptDarwinAttach, uintptr(pid), 0, 0); err != nil {
		return fmt.Errorf("PT_ATTACH pid %d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return fmt.Errorf("wait after PT_ATTACH: %w", err)
	}
	return nil
}

func killProcess(_ Backend, pid int, cmd *exec.Cmd) error {
	if cmd != nil {
		if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
			return err
		}
		// The tracee is ptrace-stopped, so the pending SIGKILL is not delivered
		// until it is resumed. Without this PT_CONTINUE, cmd.Wait blocks until
		// this process itself exits (the "Kill did not reap" wedge). Resuming
		// lets the tracee run straight into the fatal signal; ignore the error
		// since the process may already be gone. Breakpoints were restored by
		// the engine's bps.clearAll before this call, so the text is clean.
		_ = ptrace(ptDarwinContinue, uintptr(pid), 1, 0)
		_ = cmd.Wait()
		return nil
	}
	// Attached (not launched): detach, don't kill — we don't own the process.
	_ = ptrace(ptDarwinDetach, uintptr(pid), 1, 0)
	return nil
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

func (b *darwinBackend) ContinueProcess() error {
	b.clearStep()
	if err := ptrace(ptDarwinContinue, uintptr(b.pid), 1, 0); err != nil {
		return fmt.Errorf("PT_CONTINUE: %w", err)
	}
	return nil
}

func (b *darwinBackend) SingleStep(tid int) error {
	b.setStep(tid)
	// Darwin PT_STEP is per-process (by PID), not per-thread. The tid argument
	// is a Mach thread port, not a valid ptrace identifier — use b.pid.
	if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0); err != nil {
		b.clearStep()
		return fmt.Errorf("PT_STEP: %w", err)
	}
	return nil
}

// singleStepThread single-steps exactly one thread over a disarmed software
// breakpoint, atomically with respect to every other thread in the task, and
// guarantees the disarmed instruction actually retires even if the kernel tries
// to divert the thread into a signal handler.
//
// Two Darwin-specific hazards make the naive "PT_STEP once" wrong:
//
//  1. PT_STEP is per-process: it applies the hardware single-step to the
//     kernel's first task thread and task_release's the WHOLE task. If the
//     thread that hit the breakpoint is not that first thread, a plain PT_STEP
//     lets the breakpoint thread run FREELY past the (temporarily removed) trap
//     byte while a different thread absorbs the single-step.
//
//  2. ptrace resume delivers pending BSD signals. Go's scheduler preempts
//     goroutines with SIGURG (~100 Hz); a SIGURG left pending on the breakpoint
//     thread is delivered by PT_STEP, so the kernel enters the Go signal
//     trampoline INSTEAD of executing the instruction at bp.addr. On sigreturn
//     the thread lands back on bp.addr, the engine has meanwhile re-armed the
//     trap, and the target re-hits the breakpoint — the step is silently lost
//     and the client's StepOver never completes. Delve dodges this by
//     Mach-thread_resume'ing a single thread (which does not deliver BSD
//     signals), but under our wait4/BSD-stop model the breakpoint thread has
//     Mach suspend count 0, so only a ptrace resume can run it.
//
// To make the disarm→step→rearm section atomic AND land past bp.addr, we:
//  1. Mach-suspend every other thread (so only tid can run and no other thread
//     can generate a fresh SIGURG while the trap byte is absent),
//  2. arm ARMv8 hardware single-step on tid specifically (MDSCR_EL1.SS +
//     PSTATE.SS), so PT_STEP steps precisely tid,
//  3. issue the first PT_STEP with signal 0 (never re-injecting a signal), then
//  4. let Wait/advanceStepOver keep stepping tid — with every signal suppressed
//     — until PC has genuinely retired past bp.addr, transparently stepping
//     through any signal-handler excursion.
//
// endThreadStep MUST be called once the step trap is reported (after the engine
// re-arms the breakpoint) to disarm single-step on tid and resume the suspended
// threads. On any error here we roll back fully so no thread is left suspended.
func (b *darwinBackend) singleStepThread(tid int, addr uint64) error {
	if !darwinAtomicStepOverEnabled() {
		// Ablation path (BINGO_DARWIN_ATOMIC_STEPOVER=0): fall back to the plain
		// per-process single-step. This reproduces the original wrong-thread
		// hang and is intended only for measuring fix #1 in isolation.
		return b.SingleStep(tid)
	}
	if err := b.suspendOthers(tid); err != nil {
		return fmt.Errorf("single step thread: suspend others: %w", err)
	}
	if err := b.setSingleStep(tid, true); err != nil {
		b.resumeSuspended()
		return fmt.Errorf("single step thread: arm single-step tid %d: %w", tid, err)
	}
	branch := b.instrAltersPC(addr)
	b.stepMu.Lock()
	b.stepThreadPort = tid
	b.sobActive = true
	b.sobAddr = addr
	b.sobBranch = branch
	b.sobSteps = 0
	b.stepMu.Unlock()
	if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0); err != nil {
		_ = b.setSingleStep(tid, false)
		b.stepMu.Lock()
		b.stepThreadPort = 0
		b.sobActive = false
		b.stepMu.Unlock()
		b.resumeSuspended()
		return fmt.Errorf("single step thread: PT_STEP: %w", err)
	}
	return nil
}

// sobStepCap bounds the per-step-over retry loop. A single non-safepoint SIGURG
// handler is only a few hundred instructions; this leaves ample headroom while
// still guaranteeing Wait can never spin forever on a pathological target.
const sobStepCap = 200000

// instrAltersPC reports whether the 4-byte AArch64 instruction at addr can set
// PC to something other than addr+4 (any branch, branch-register, compare/test
// branch, conditional branch, or exception-generating instruction). When it
// can, we cannot assume the instruction retires to sobAddr+4 and fall back to
// trusting a single step. ADR/ADRP are deliberately excluded: they only compute
// an address into a register and fall through. On any read error we
// conservatively report true (trust one step).
func (b *darwinBackend) instrAltersPC(addr uint64) bool {
	var buf [4]byte
	if err := b.ReadMemory(addr, buf[:]); err != nil {
		return true
	}
	insn := uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24
	switch {
	case insn&0x7C000000 == 0x14000000: // B / BL (unconditional immediate)
		return true
	case insn&0xFE000000 == 0x54000000: // B.cond / BC.cond
		return true
	case insn&0x7E000000 == 0x34000000: // CBZ / CBNZ
		return true
	case insn&0x7E000000 == 0x36000000: // TBZ / TBNZ
		return true
	case insn&0xFE000000 == 0xD6000000: // BR / BLR / RET / ERET (branch register)
		return true
	case insn&0xFF000000 == 0xD4000000: // SVC / BRK / HLT / HVC / SMC (exception)
		return true
	default:
		return false
	}
}

// stepOverActive reports whether a software-BP step-over is in flight.
func (b *darwinBackend) stepOverActive() bool {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	return b.sobActive
}

// clearStepOver marks any in-flight step-over as finished (used on process
// exit/kill). It does not resume suspended threads: on exit they are gone, and
// endThreadStep handles the live-teardown case.
func (b *darwinBackend) clearStepOver() {
	b.stepMu.Lock()
	b.sobActive = false
	b.stepMu.Unlock()
}

// stepOverBumpCapped increments the step-over retry counter, clearing the
// step-over if the cap is exceeded, and reports the stepped thread port plus
// whether the loop must terminate now (cap hit or already torn down).
func (b *darwinBackend) stepOverBumpCapped() (tid int, done bool) {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	tid = b.stepThreadPort
	if !b.sobActive {
		return tid, true
	}
	b.sobSteps++
	if b.sobSteps > sobStepCap {
		b.sobActive = false
		return tid, true
	}
	return tid, false
}

// advanceStepOver is called from Wait for every SIGTRAP seen while a software-BP
// step-over is in flight. It returns (event, true) once the disarmed instruction
// has retired past bp.addr — handing a single StopSingleStep back to the engine —
// or (zero, false) after issuing another suppressed single-step to work through a
// signal-handler excursion. All other threads remain Mach-suspended throughout,
// so only the stepped thread runs and no fresh preemption signals are generated.
// sobActive may be cleared concurrently by endThreadStep (Kill); we re-check it
// under stepMu after reading registers and treat teardown as completion.
func (b *darwinBackend) advanceStepOver() (StopEvent, bool) {
	b.stepMu.Lock()
	active := b.sobActive
	tid := b.stepThreadPort
	addr := b.sobAddr
	branch := b.sobBranch
	b.stepMu.Unlock()
	if !active {
		return StopEvent{Reason: StopSingleStep, TID: tid}, true
	}

	retired := false
	if regs, err := b.GetRegisters(tid); err != nil {
		// Can't read PC (thread vanished mid-step); stop retrying and let the
		// engine resolve the stop rather than spin.
		retired = true
	} else if branch {
		// Control-flow instruction: its successor isn't statically sobAddr+4.
		// PC != sobAddr means it left the breakpoint (either retired or was
		// diverted). We can't cheaply tell these apart for a branch, so trust
		// the step once it moves — same best effort as before, but now atomic
		// w.r.t. other threads. PC == sobAddr means a handler just returned
		// without executing it yet; step again.
		retired = regs.PC != addr
	} else {
		// Straight-line instruction: it has retired iff PC advanced to the next
		// instruction. A signal diversion lands PC in the runtime trampoline
		// (never sobAddr+4), so this cleanly distinguishes "instruction
		// executed" from "diverted into a handler".
		retired = regs.PC == addr+4
	}

	b.stepMu.Lock()
	if !b.sobActive {
		b.stepMu.Unlock()
		return StopEvent{Reason: StopSingleStep, TID: tid}, true
	}
	if retired {
		b.sobActive = false
		b.stepMu.Unlock()
		return StopEvent{Reason: StopSingleStep, TID: tid}, true
	}
	b.sobSteps++
	if b.sobSteps > sobStepCap {
		b.sobActive = false
		b.stepMu.Unlock()
		return StopEvent{Reason: StopSingleStep, TID: tid}, true
	}
	b.stepMu.Unlock()

	// Not yet past bp.addr: keep stepping the same (only-runnable) thread with
	// the signal suppressed so the disarmed instruction eventually executes.
	if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0); err != nil {
		b.stepMu.Lock()
		b.sobActive = false
		b.stepMu.Unlock()
		return StopEvent{Reason: StopSingleStep, TID: tid}, true
	}
	return StopEvent{}, false
}

// endThreadStep tears down the critical section opened by singleStepThread:
// disarm hardware single-step on the stepped thread and resume every thread we
// suspended. Idempotent and safe to call when no atomic step is in flight. May
// run on the engine loop goroutine (Kill) while advanceStepOver runs on the
// waitLoop goroutine, so the shared fields are touched under stepMu; the
// suspend list itself is engine-loop-only.
func (b *darwinBackend) endThreadStep() {
	b.stepMu.Lock()
	b.sobActive = false
	port := b.stepThreadPort
	b.stepThreadPort = 0
	b.stepMu.Unlock()
	if port != 0 {
		// Best-effort: clear the hardware single-step bit on the stepped thread.
		_ = b.setSingleStep(port, false)
	}
	b.resumeSuspended()
}

// suspendOthers Mach-suspends every task thread except keep, recording the
// exact ports suspended so endThreadStep resumes precisely those (threads that
// spawn during the window are left alone; threads that exit are ignored).
func (b *darwinBackend) suspendOthers(keep int) error {
	tids, err := b.Threads()
	if err != nil {
		return err
	}
	b.suspended = b.suspended[:0]
	for _, tid := range tids {
		if tid == keep {
			continue
		}
		if kr := C.bingo_thread_suspend(C.mach_port_t(tid)); kr == C.KERN_SUCCESS {
			b.suspended = append(b.suspended, tid)
		}
	}
	return nil
}

func (b *darwinBackend) resumeSuspended() {
	for _, tid := range b.suspended {
		C.bingo_thread_resume(C.mach_port_t(tid))
	}
	b.suspended = b.suspended[:0]
}

func (b *darwinBackend) setSingleStep(tid int, on bool) error {
	v := C.int(0)
	if on {
		v = 1
	}
	if kr := C.bingo_set_single_step(C.mach_port_t(tid), v); kr != C.KERN_SUCCESS {
		return fmt.Errorf("set_single_step tid %d: %s", tid, machErrString(kr))
	}
	return nil
}

// StopProcess asynchronously interrupts the running tracee for Pause by sending
// it darwinPauseSignal. Unlike Delve's Darwin backend — which halts via Mach
// thread_suspend on every thread because it detects stops through a Mach
// exception port — bingo detects stops through wait4, so Pause must produce a
// wait4-visible stop. SIGSTOP cannot: XNU delivers it to a ptraced tracee as a
// job-control stop that Go's syscall.WaitStatus reports as neither Stopped nor
// Exited (StopSignal()==-1), so Wait's `if !ws.Stopped()` skips it and re-blocks
// forever. A catchable signal instead surfaces as an ordinary ptrace
// signal-delivery stop — Wait returns it as StopEvent{StopSignal, sig} and the
// engine's manual-stop detection turns it into EventPaused. The engine never
// re-injects it (Continue resumes with signal 0), so the tracee never actually
// receives the signal and resume is a plain ContinueProcess. ESRCH (process
// already gone) is an idempotent no-op, matching process.kill.
func (b *darwinBackend) StopProcess() error {
	return stopProcessSignal(b.pid, darwinPauseSignal)
}

// PauseSignal is the catchable signal StopProcess delivers and that the engine
// turns into EventPaused. See Backend.PauseSignal and StopProcess for why a
// catchable signal (not SIGSTOP) is required on darwin.
func (b *darwinBackend) PauseSignal() int { return int(darwinPauseSignal) }

func (b *darwinBackend) GetRegisters(tid int) (Registers, error) {
	thread := C.mach_port_t(tid)
	if thread == 0 {
		return Registers{}, fmt.Errorf("GetRegisters: invalid tid 0")
	}
	var pc, sp, fp, g C.uint64_t
	kr := C.bingo_get_registers(thread, &pc, &sp, &fp, &g)
	if kr != C.KERN_SUCCESS {
		return Registers{}, fmt.Errorf("thread_get_state tid %d: %s", tid, machErrString(kr))
	}
	return Registers{
		PC:  uint64(pc),
		SP:  uint64(sp),
		BP:  uint64(fp),
		TLS: uint64(g),
	}, nil
}

func (b *darwinBackend) SetRegisters(tid int, reg Registers) error {
	thread := C.mach_port_t(tid)
	if thread == 0 {
		return fmt.Errorf("SetRegisters: invalid tid 0")
	}
	kr := C.bingo_set_registers(thread,
		C.uint64_t(reg.PC),
		C.uint64_t(reg.SP),
		C.uint64_t(reg.BP),
		C.uint64_t(reg.TLS),
	)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("thread_set_state tid %d: %s", tid, machErrString(kr))
	}
	return nil
}

func (b *darwinBackend) ReadMemory(addr uint64, dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	task, err := b.task()
	if err != nil {
		return err
	}
	kr := C.bingo_read_memory(task,
		C.mach_vm_address_t(addr),
		unsafe.Pointer(&dst[0]),
		C.mach_vm_size_t(len(dst)),
	)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("mach_vm_read_overwrite 0x%x: %s", addr, machErrString(kr))
	}
	return nil
}

func (b *darwinBackend) WriteMemory(addr uint64, src []byte) error {
	if len(src) == 0 {
		return nil
	}
	task, err := b.task()
	if err != nil {
		return err
	}
	kr := C.bingo_write_memory(task,
		C.mach_vm_address_t(addr),
		unsafe.Pointer(&src[0]),
		C.mach_vm_size_t(len(src)),
	)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("mach_vm_write 0x%x: %s", addr, machErrString(kr))
	}
	return nil
}

func (b *darwinBackend) Threads() ([]int, error) {
	task, err := b.task()
	if err != nil {
		return nil, err
	}
	var threads C.thread_act_port_array_t
	var count C.mach_msg_type_number_t
	kr := C.bingo_thread_list(task, &threads, &count)
	if kr != C.KERN_SUCCESS {
		return nil, fmt.Errorf("task_threads pid %d: %s", b.pid, machErrString(kr))
	}
	ports := unsafe.Slice((*C.mach_port_t)(unsafe.Pointer(threads)), int(count))

	// task_threads inserts a fresh send-right per thread on every call. Retain
	// exactly one right per live thread; deallocate the duplicate rights this
	// call produced and the rights of threads that have since exited. Without
	// this the debugger leaks one send-right per thread per enumeration, and
	// Threads() runs on nearly every step/suspend/register operation.
	b.threadMu.Lock()
	if b.threadPorts == nil {
		b.threadPorts = make(map[C.mach_port_t]struct{})
	}
	seen := make(map[C.mach_port_t]struct{}, len(ports))
	tids := make([]int, len(ports))
	for i, p := range ports {
		seen[p] = struct{}{}
		if _, ok := b.threadPorts[p]; ok {
			C.bingo_port_deallocate(p)
		} else {
			b.threadPorts[p] = struct{}{}
		}
		tids[i] = int(p)
	}
	for p := range b.threadPorts {
		if _, ok := seen[p]; !ok {
			C.bingo_port_deallocate(p)
			delete(b.threadPorts, p)
		}
	}
	b.threadMu.Unlock()

	C.vm_deallocate(
		C.mach_task_self_,
		C.vm_address_t(uintptr(unsafe.Pointer(threads))),
		C.vm_size_t(uintptr(count)*unsafe.Sizeof(C.mach_port_t(0))),
	)
	return tids, nil
}

//nolint:gocognit // The wait loop is one serialized ptrace state machine.
func (b *darwinBackend) Wait() (StopEvent, error) {
	b.probeOnce.Do(func() {
		if darwinSuspendProbeEnabled() {
			b.suspendProbeOn = true
			go b.suspendProbeWatchdog()
		}
	})
	for {
		var ws syscall.WaitStatus
		if b.suspendProbeOn {
			atomic.StoreInt64(&b.waitStartNanos, time.Now().UnixNano())
		}
		tid, err := syscall.Wait4(b.pid, &ws, 0, nil)
		if b.suspendProbeOn {
			atomic.StoreInt64(&b.waitStartNanos, 0)
		}
		if err != nil {
			if isNoChildProcess(err) {
				return StopEvent{Reason: StopExited, TID: b.pid}, nil
			}
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}
		if ws.Exited() {
			b.clearStepOver()
			return StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, nil
		}
		if ws.Signaled() {
			b.clearStepOver()
			return StopEvent{Reason: StopKilled, TID: tid}, nil
		}
		if !ws.Stopped() {
			continue
		}

		sig := ws.StopSignal()

		// Software-BP step-over in flight: every other thread is Mach-suspended
		// and only the stepped thread runs. Drive it to completion here (the
		// sole wait4 owner), suppressing any signal so it can never be injected
		// into the atomic disarm→step→rearm window (a delivered SIGURG would
		// divert the thread into the Go signal trampoline instead of executing
		// the disarmed instruction). advanceStepOver returns done=true with a
		// single StopSingleStep once the instruction has retired past bp.addr.
		if b.stepOverActive() {
			if sig == syscall.SIGTRAP {
				if ev, done := b.advanceStepOver(); done {
					return ev, nil
				}
				continue
			}
			stepTID, done := b.stepOverBumpCapped()
			if done {
				return StopEvent{Reason: StopSingleStep, TID: stepTID}, nil
			}
			if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0); err != nil {
				b.endThreadStep()
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}

		if sig == syscall.SIGTRAP {
			if stepping, thread := b.consumeStep(); stepping {
				return StopEvent{Reason: StopSingleStep, TID: thread}, nil
			}
			return StopEvent{Reason: StopBreakpoint}, nil
		}

		// SIGURG is Go's goroutine-preemption signal. Re-deliver transparently.
		if sig == syscall.SIGURG {
			if err := b.resumeAfterSignal(sig); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}

		// SIGWINCH (terminal resize): handled by the Go runtime, re-deliver.
		if sig == syscall.SIGWINCH {
			if err := b.resumeAfterSignal(sig); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}

		// Other signals during a plain machine single-step (StepInto): re-deliver
		// via PT_STEP so the step completes. If PT_STEP fails, fall through to
		// StopSignal so the engine can recover. Don't fall back to
		// PT_CONTINUE+continue: that loops on SIGURG every ~10ms forever.
		if b.isStepping() {
			if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, uintptr(sig)); err == nil {
				continue
			} else if isNoSuchProcess(err) {
				return StopEvent{Reason: StopKilled, TID: tid}, nil
			}
			b.clearStep()
		}
		return StopEvent{Reason: StopSignal, Signal: int(sig)}, nil
	}
}

// suspendProbeWatchdog (diagnostic only) polls waitStartNanos and, when Wait()
// has been blocked in wait4 for longer than probeWedgeThreshold, dumps the
// tracee's TASK-level suspend_count and per-thread state once per stall episode.
//
// Interpreting the numbers (validated by a task_suspend positive control):
//   - A ptrace-STOPPED darwin task already reads task suspend_count == 1 (BSD
//     SSTOP shares the single Mach task suspend_count). So suspend_count == 1
//     is the ORDINARY stop baseline, NOT evidence of a leak on its own.
//   - task_suspend/task_resume increment/decrement that SAME counter, so a
//     LEAKED task_suspend (e.g. an unbalanced bingo_write_memory) stacked on a
//     ptrace stop would read suspend_count >= 2.
//   - A resumed/running task reads 0.
//
// Therefore the discriminators for the residual wedge are:
//
//	(a) suspend_count >= 2                         -> a held/leaked task suspend
//	(b) threads run_state==STOPPED at user PCs      -> task-suspend freeze
//	(c) a thread run_state==RUNNING, task not        -> a non-preemptible run the
//	    suspended                                       runtime can't stop (STW
//	                                                    stall; e.g. asyncpreemptoff
//	                                                    disabling async preemption)
//	(d) threads run_state==WAITING, static kernel PCs (__psynch_cvwait, …),
//	    suspend ∈ {0,1}                             -> pthread-condvar lost-wakeup
func (b *darwinBackend) suspendProbeWatchdog() {
	const probeWedgeThreshold = 9 * time.Second
	var lastDumped int64
	for {
		time.Sleep(1 * time.Second)
		start := atomic.LoadInt64(&b.waitStartNanos)
		if start == 0 || start == lastDumped {
			continue
		}
		if time.Duration(time.Now().UnixNano()-start) < probeWedgeThreshold {
			continue
		}
		lastDumped = start
		b.dumpSuspendState(start)
	}
}

func (b *darwinBackend) dumpSuspendState(waitStart int64) {
	task, err := b.task()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[suspendprobe] pid=%d task() err: %v\n", b.pid, err)
		return
	}
	waited := time.Duration(time.Now().UnixNano() - waitStart).Round(time.Millisecond)
	var sc C.uint32_t
	if kr := C.bingo_task_suspend_count(task, &sc); kr != C.KERN_SUCCESS {
		fmt.Fprintf(os.Stderr, "[suspendprobe] pid=%d waited=%s task_suspend_count err: %s\n",
			b.pid, waited, machErrString(kr))
		return
	}
	scStr := "0 (task resumed/running)"
	switch {
	case int(sc) == 1:
		scStr = "1 (== ordinary ptrace-stop baseline; NOT a leak by itself)"
	case int(sc) >= 2:
		scStr = fmt.Sprintf("%d (>=2: hold BEYOND ptrace baseline -> candidate task-suspend LEAK)", int(sc))
	}
	fmt.Fprintf(os.Stderr, "[suspendprobe] pid=%d waited=%s TASK_SUSPEND_COUNT=%s\n", b.pid, waited, scStr)

	pcs0, _, stopped0, _ := b.dumpThreads(task, "s0")
	time.Sleep(300 * time.Millisecond)
	pcs1, waiting1, stopped1, running1 := b.dumpThreads(task, "s1")

	static := len(pcs0) > 0
	for port, pc := range pcs0 {
		if pcs1[port] != pc {
			static = false
			break
		}
	}
	verdict := "INCONCLUSIVE"
	switch {
	case int(sc) >= 2:
		verdict = "task-suspend LEAK (suspend_count exceeds ptrace baseline)"
	case stopped0 > 0 || stopped1 > 0:
		verdict = "task-suspend FREEZE (threads STOPPED, not voluntarily waiting)"
	case running1 > 0:
		verdict = "non-preemptible RUN (a thread is RUNNING and cannot be stopped -> STW stall, e.g. asyncpreemptoff); NOT a suspend leak"
	case waiting1 > 0 && static:
		verdict = "pthread-condvar LOST-WAKEUP (threads WAITING in kernel, PCs static, suspend<=1)"
	}
	fmt.Fprintf(os.Stderr, "[suspendprobe]   VERDICT: %s (waiting=%d running=%d stopped=%d staticPCs=%v)\n",
		verdict, waiting1, running1, stopped1, static)
}

// dumpThreads logs each thread and returns its PC map plus WAITING/STOPPED/RUNNING tallies.
func (b *darwinBackend) dumpThreads(task C.mach_port_t, label string) (map[int]uint64, int, int, int) {
	pcs := map[int]uint64{}
	waiting, stopped, running := 0, 0, 0
	var threads C.thread_act_port_array_t
	var count C.mach_msg_type_number_t
	if kr := C.bingo_thread_list(task, &threads, &count); kr != C.KERN_SUCCESS {
		fmt.Fprintf(os.Stderr, "[suspendprobe]   %s task_threads err: %s\n", label, machErrString(kr))
		return pcs, waiting, stopped, running
	}
	defer C.vm_deallocate(
		C.mach_task_self_,
		C.vm_address_t(uintptr(unsafe.Pointer(threads))),
		C.vm_size_t(uintptr(count)*unsafe.Sizeof(C.mach_port_t(0))),
	)
	ports := unsafe.Slice((*C.mach_port_t)(unsafe.Pointer(threads)), int(count))
	for i, p := range ports {
		var rs, tsc C.int
		var pc C.uint64_t
		kr := C.bingo_thread_probe(p, &rs, &tsc, &pc)
		C.bingo_port_deallocate(p) // release this call's send-right; retained set lives in threadPorts
		if kr != C.KERN_SUCCESS {
			fmt.Fprintf(os.Stderr, "[suspendprobe]   %s th[%d] port=%d probe err: %s\n",
				label, i, int(p), machErrString(kr))
			continue
		}
		switch int(rs) {
		case 3: // TH_STATE_WAITING
			waiting++
		case 2: // TH_STATE_STOPPED
			stopped++
		case 1: // TH_STATE_RUNNING
			running++
		}
		pcs[int(p)] = uint64(pc)
		fmt.Fprintf(os.Stderr, "[suspendprobe]   %s th[%d] port=%d run_state=%d suspend=%d pc=%#x\n",
			label, i, int(p), int(rs), int(tsc), uint64(pc))
	}
	return pcs, waiting, stopped, running
}

func (b *darwinBackend) setPID(pid int) { b.pid = pid }

func (b *darwinBackend) setStep(tid int) {
	b.stepMu.Lock()
	b.stepping = true
	b.stepTID = tid
	b.stepMu.Unlock()
}

func (b *darwinBackend) clearStep() {
	b.stepMu.Lock()
	b.stepping = false
	b.stepTID = 0
	b.stepMu.Unlock()
}

func (b *darwinBackend) isStepping() bool {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	return b.stepping
}

func (b *darwinBackend) consumeStep() (bool, int) {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	if !b.stepping {
		return false, 0
	}
	tid := b.stepTID
	b.stepping = false
	b.stepTID = 0
	return true, tid
}

func (b *darwinBackend) resumeAfterSignal(sig syscall.Signal) error {
	request := ptDarwinContinue
	if b.isStepping() {
		request = ptDarwinStep
	}
	if err := ptrace(request, uintptr(b.pid), 1, uintptr(sig)); err != nil {
		return fmt.Errorf("ptrace resume after %s: %w", sig, err)
	}
	return nil
}

func isNoSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

func isNoChildProcess(err error) bool {
	return errors.Is(err, syscall.ECHILD)
}

// TextSlide returns the ASLR slide for the main executable: actual load
// address minus preferred __TEXT vmaddr. Returns 0 on any error.
//
// Scans the task's VM map for the first executable region with the 64-bit
// Mach-O magic. Works even at the very first ptrace stop (before dyld runs),
// unlike TASK_DYLD_INFO whose image array is unpopulated at that point.
func (b *darwinBackend) TextSlide(binaryPath string) int64 {
	task, err := b.task()
	if err != nil {
		return 0
	}

	var loadAddr C.mach_vm_address_t
	if kr := C.bingo_find_macho_load_addr(task, &loadAddr); kr != C.KERN_SUCCESS {
		return 0
	}

	preferredVmaddr, err := machoTextVmaddr(binaryPath)
	if err != nil {
		return 0
	}

	return int64(loadAddr) - int64(preferredVmaddr)
}

func machoTextVmaddr(binaryPath string) (uint64, error) {
	f, err := macho.Open(binaryPath)
	if err != nil {
		return 0, fmt.Errorf("macho.Open %s: %w", binaryPath, err)
	}
	defer func() { _ = f.Close() }()
	seg := f.Segment("__TEXT")
	if seg == nil {
		return 0, fmt.Errorf("no __TEXT segment in %s", binaryPath)
	}
	return seg.Addr, nil
}

var _ Backend = (*darwinBackend)(nil)

// task returns the Mach task port for the tracee. Requires the
// com.apple.security.cs.debugger entitlement or disabled SIP.
// task returns the tracee's Mach task port, acquiring it once via task_for_pid
// and caching it thereafter. See the taskPort field comment for why re-acquiring
// per call is unsafe (it intermittently hangs task_for_pid in the kernel).
func (b *darwinBackend) task() (C.mach_port_t, error) {
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	if b.taskOK {
		return b.taskPort, nil
	}
	var task C.mach_port_t
	kr := C.bingo_task_for_pid(C.int(b.pid), &task)
	if kr != C.KERN_SUCCESS {
		return 0, fmt.Errorf("task_for_pid(%d): %s — debugger entitlement required",
			b.pid, machErrString(kr))
	}
	b.taskPort = task
	b.taskOK = true
	return task, nil
}

func machErrString(kr C.kern_return_t) string {
	return C.GoString(C.mach_error_string(kr))
}
