//go:build darwin && arm64 && bingonative

package debugger

// Darwin/arm64 (Apple Silicon) Backend. Pure Mach: process lifecycle via
// posix_spawn + a task-level Mach exception port, registers via
// thread_get_state, memory via mach_vm_read/write. Stops are detected by a
// mach_msg receive loop on a port set (issue #92), NOT wait4/ptrace.
//
// Why no ptrace/PT_SIGEXC: bingo masks ONLY EXC_MASK_BREAKPOINT and leaves BSD
// signals to native, thread-directed delivery. Go's async-preemption SIGURG is
// thread-directed (pthread_kill to a specific M); under the old ptrace/wait4
// model the only resume primitive was process-directed (XNU psignal), which
// misdirected SIGURG to the wrong M and deadlocked the runtime — which is why
// that model had to disable async preemption (asyncpreemptoff). Leaving signals
// native re-delivers SIGURG to exactly the M the runtime targeted, so async
// preemption can stay ON. The tradeoff: the debugger no longer surfaces Unix
// signals as stops on darwin (a crash becomes a process exit, not a signal
// stop); bingo's engine only needs breakpoint/step/exit, and no e2e asserts on
// signal reporting. See AGENTS.md → Backend quirks / Pause.
//
// Requires com.apple.security.cs.debugger entitlement (or SIP disabled).
// Cannot be cross-compiled from non-macOS — cgo needs the macOS SDK.

/*
#cgo LDFLAGS: -framework CoreFoundation

#include <stdlib.h>
#include <string.h>
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
	pid int

	// launched distinguishes a process we spawned (we own it → SIGKILL on Kill)
	// from one we attached to (we don't own it → detach, don't kill). posix_spawn
	// yields no *exec.Cmd, so this flag replaces the old cmd!=nil check.
	launched bool

	// Mach exception-port machinery (issue #92). portSet is the receive set Wait
	// blocks on; excPort receives EXC_BREAKPOINT (software BRK and hardware
	// single-step both raise it); notePort receives the dead-name notification
	// when the tracee exits; ctrlPort is a private port Pause posts to so it can
	// wake a blocked mach_msg (task/thread_suspend cannot wake a receive).
	portSet  C.mach_port_t
	excPort  C.mach_port_t
	notePort C.mach_port_t
	ctrlPort C.mach_port_t
	portsOK  bool

	// taskPort caches the Mach task port obtained from task_for_pid. It is
	// acquired once at launch/attach (past the fork/exec race) and reused for the
	// process's lifetime. Re-calling task_for_pid on every memory/thread op — as
	// the code originally did — intermittently wedges IN THE KERNEL on a busy
	// tracer: the send right is never released and repeated port insertions
	// eventually block task_for_pid indefinitely. Delve and LLDB both acquire the
	// task port exactly once for this reason.
	taskMu   sync.Mutex
	taskPort C.mach_port_t
	taskOK   bool

	// One retained send-right per live tracee thread, keyed by Mach port name.
	// task_threads hands out a fresh send-right per thread on every call; we keep
	// exactly one and deallocate the rest (and the rights of threads that have
	// exited) so the debugger doesn't leak ports under thread churn and so a
	// given thread keeps a stable port name across enumerations.
	threadMu    sync.Mutex
	threadPorts map[C.mach_port_t]struct{}

	// Step bookkeeping. stepping/stepTID drive the ablation-only plain
	// SingleStep; the sob* fields drive the atomic step-over-breakpoint retire
	// loop. advanceStepOver runs on the waitLoop goroutine while endThreadStep
	// may run on the engine loop goroutine (Kill mid-step), so the shared fields
	// are guarded by stepMu. sobActive gates the step-over path in Wait; sobAddr
	// is the disarmed breakpoint address the stepped thread must retire past;
	// sobBranch records whether the instruction there alters control flow (so we
	// can't assume it retires to sobAddr+4); sobSteps bounds the retry loop that
	// steps through signal-handler excursions; stepThreadPort is the thread armed
	// with hardware single-step that must be disarmed afterwards.
	stepMu         sync.Mutex
	stepping       bool
	stepTID        int
	stepThreadPort int
	sobActive      bool
	sobAddr        uint64
	sobBranch      bool
	sobSteps       int

	// Deferred exception replies. bingo leaves BSD signals native (only
	// EXC_MASK_BREAKPOINT is masked), so replying KERN_SUCCESS to a breakpoint
	// exception the instant it arrives lets the kernel build a pending signal's
	// handler frame on the still-suspended faulting thread — diverting its PC into
	// _sigtramp before the engine reads it (observed with Go async-preemption
	// SIGURG). Instead the reply header captured at receive time is stashed here,
	// keyed by faulting thread port, and sent by bingo_reply_exception only when
	// that thread is about to be resumed. An un-acknowledged Mach exception keeps
	// the thread frozen at the faulting instruction with a stable PC. At most one
	// reply is pending per thread (a suspended thread cannot fault again).
	replyMu        sync.Mutex
	pendingReplies map[int]replyInfo

	// Diagnostic-only (BINGO_DARWIN_SUSPEND_PROBE): waitStartNanos records when
	// Wait() blocked in mach_msg (0 = not blocked). A watchdog goroutine, started
	// once, reads it to detect a wedge and dump the task/thread suspend state.
	// suspendProbeOn caches the env gate so a disabled probe adds no per-wait work
	// beyond one bool check.
	waitStartNanos int64
	probeOnce      sync.Once
	suspendProbeOn bool
}

// darwinPauseSignal is the sentinel PauseSignal the engine matches a manual-stop
// StopSignal against. Under the Mach model no real signal is sent for Pause —
// StopProcess posts to ctrlPort and Wait synthesises StopSignal{darwinPauseSignal}
// after stopping the world — but the engine still needs a stable "which signal
// means Pause" value to key emitPaused off (see handleStop's StopSignal branch).
// SIGUSR2 is kept for continuity; the tracee never actually receives it.
const darwinPauseSignal = syscall.SIGUSR2

// The two darwin/arm64 step-over correctness knobs are independently toggleable
// so an operator can ablate either in isolation.
//
//	BINGO_DARWIN_ATOMIC_STEPOVER     default ON  — "0" disables the retire loop
//	                                 (falls back to a single naive per-thread
//	                                 step; ablation only).
//	BINGO_DARWIN_ASYNC_PREEMPT_OFF   default OFF — "1" forces
//	                                 GODEBUG=asyncpreemptoff=1 in the tracee.
//
// asyncpreemptoff defaults OFF under the Mach exception model: BSD signals are
// delivered natively and thread-directed, so Go's async-preemption SIGURG
// reaches the M the runtime targeted (the #92 fix). The old wait4/ptrace model
// had to force it ON to dodge SIGURG misdirection; that stopgap is gone. The
// toggle remains as an ablation lever.
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
// run_state/suspend/PC. It never changes control flow and only does work after a
// multi-second Wait stall, so it does not perturb the pre-wedge timing.
func darwinSuspendProbeEnabled() bool {
	return os.Getenv("BINGO_DARWIN_SUSPEND_PROBE") == "1"
}

// startTracedProcess launches binaryPath suspended at its entry point via
// posix_spawn(POSIX_SPAWN_START_SUSPENDED), acquires the Mach task port and
// exception ports (racing the spawn), then freezes the process into bingo's
// resting state (every thread individually Mach-suspended, task resumed). It
// returns no *exec.Cmd — posix_spawn owns no exec.Cmd — so the caller relies on
// the pid and the backend's launched flag.
func startTracedProcess(b Backend, binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	db, _ := b.(*darwinBackend)
	if db == nil {
		return 0, nil, fmt.Errorf("darwin startTracedProcess: nil backend")
	}

	cpath := C.CString(binaryPath)
	defer C.free(unsafe.Pointer(cpath))

	argv := make([]*C.char, 0, len(args)+2)
	argv = append(argv, cpath)
	for _, a := range args {
		ca := C.CString(a)
		defer C.free(unsafe.Pointer(ca))
		argv = append(argv, ca)
	}
	argv = append(argv, nil)

	fullEnv := append(os.Environ(), env...)
	if darwinAsyncPreemptOffEnabled() {
		fullEnv = withDarwinAsyncPreemptOff(fullEnv)
	}
	envp := make([]*C.char, 0, len(fullEnv)+1)
	for _, e := range fullEnv {
		ce := C.CString(e)
		defer C.free(unsafe.Pointer(ce))
		envp = append(envp, ce)
	}
	envp = append(envp, nil)

	var cpid C.int
	rc := C.bingo_posix_spawn(cpath, &argv[0], &envp[0], &cpid)
	if rc != 0 {
		return 0, nil, fmt.Errorf("posix_spawn %q: %s", binaryPath, C.GoString(C.strerror(rc)))
	}
	pid := int(cpid)
	db.launched = true

	if err := db.acquirePorts(pid); err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return 0, nil, err
	}

	task, err := db.task()
	if err != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return 0, nil, err
	}
	if kr := C.bingo_freeze_at_launch(task); kr != C.KERN_SUCCESS {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return 0, nil, fmt.Errorf("freeze at launch: %s", machErrString(kr))
	}
	return pid, nil, nil
}

// acquirePorts records the pid, obtains the Mach task port (retrying the brief
// spawn/task race) and installs the exception/notification/control port set.
func (b *darwinBackend) acquirePorts(pid int) error {
	b.pid = pid

	var task C.mach_port_t
	var kr C.kern_return_t
	// posix_spawn returns after the child task exists, so task_for_pid normally
	// succeeds on the first try; a short bounded retry covers the rare race where
	// the task port is not yet resolvable.
	for i := 0; i < 200; i++ {
		kr = C.bingo_task_for_pid(C.int(pid), &task)
		if kr == C.KERN_SUCCESS {
			break
		}
		time.Sleep(1 * time.Millisecond)
	}
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("task_for_pid(%d): %s — debugger entitlement required",
			pid, machErrString(kr))
	}
	b.taskMu.Lock()
	b.taskPort = task
	b.taskOK = true
	b.taskMu.Unlock()

	if kr := C.bingo_setup_exception_ports(task, &b.portSet, &b.excPort, &b.notePort, &b.ctrlPort); kr != C.KERN_SUCCESS {
		return fmt.Errorf("setup exception ports: %s", machErrString(kr))
	}
	b.portsOK = true
	return nil
}

// withDarwinAsyncPreemptOff merges GODEBUG=asyncpreemptoff=1 into the tracee's
// environment. Under the Mach exception model this is applied ONLY when the
// BINGO_DARWIN_ASYNC_PREEMPT_OFF=1 ablation toggle is set (default OFF): native
// thread-directed signal delivery re-delivers SIGURG correctly, so async
// preemption no longer needs disabling. It respects an explicit asyncpreemptoff
// the caller already set.
func withDarwinAsyncPreemptOff(env []string) []string {
	const key = "asyncpreemptoff"
	out := make([]string, 0, len(env)+1)
	godebug := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "GODEBUG=") {
			godebug = strings.TrimPrefix(kv, "GODEBUG=")
			continue
		}
		out = append(out, kv)
	}
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

func attachToProcess(b Backend, pid int) error {
	db, _ := b.(*darwinBackend)
	if db == nil {
		return fmt.Errorf("darwin attach: nil backend")
	}
	db.launched = false
	if err := db.acquirePorts(pid); err != nil {
		return err
	}
	// The attached process is running; bring it to resting state (all threads
	// Mach-suspended) so the engine can inspect and set breakpoints.
	task, err := db.task()
	if err != nil {
		return err
	}
	if cls := C.bingo_stop_the_world(task, db.portSet); cls == C.BINGO_MSG_ERROR {
		return fmt.Errorf("attach: stop the world failed")
	}
	return nil
}

// running is unused on Darwin: the waitLoop blocks on a Mach exception port,
// not wait4, so this Wait4(pid) reap is always the sole reaper — no race with
// the waitLoop the way the linux backend has (see backend_linux_amd64.go).
func killProcess(b Backend, pid int, _ *exec.Cmd, _ bool) error {
	db, _ := b.(*darwinBackend)

	// Attached (not launched): we don't own the process. Resume its threads so it
	// keeps running after we stop intercepting, but never kill it.
	if db != nil && !db.launched {
		db.flushAllReplies()
		if task, err := db.task(); err == nil {
			C.bingo_resume_all_threads(task)
		}
		return nil
	}

	// Launched: a Mach-suspended thread never runs the SIGKILL AST, so resume
	// every thread first, then SIGKILL, then reap. By the time the engine calls
	// this it has already cleared breakpoints and torn down any in-flight
	// single-step (Kill → endThreadStep → bps.clearAll → proc.kill), so resuming
	// runs clean code straight into the fatal signal.
	if db != nil {
		db.flushAllReplies()
		if task, err := db.task(); err == nil {
			C.bingo_resume_all_threads(task)
		}
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill: SIGKILL: %w", err)
	}
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pid, &ws, 0, nil)
		if err == nil || isNoChildProcess(err) {
			return nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return fmt.Errorf("kill: reap: %w", err)
	}
}

func (b *darwinBackend) ContinueProcess() error {
	b.clearStep()
	b.flushAllReplies()
	task, err := b.task()
	if err != nil {
		return err
	}
	if kr := C.bingo_resume_all_threads(task); kr != C.KERN_SUCCESS {
		return fmt.Errorf("resume all threads: %s", machErrString(kr))
	}
	return nil
}

// SingleStep is the ablation-only naive step (BINGO_DARWIN_ATOMIC_STEPOVER=0):
// arm hardware single-step on tid and resume just that thread, with no retire
// loop. singleStepThread is the production path.
func (b *darwinBackend) SingleStep(tid int) error {
	b.stepMu.Lock()
	b.stepping = true
	b.stepTID = tid
	b.stepThreadPort = tid
	b.stepMu.Unlock()
	if err := b.setSingleStep(tid, true); err != nil {
		b.clearStep()
		return fmt.Errorf("SingleStep arm tid %d: %w", tid, err)
	}
	b.flushReply(tid)
	if kr := C.bingo_resume_one_thread(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
		_ = b.setSingleStep(tid, false)
		b.clearStep()
		return fmt.Errorf("SingleStep resume tid %d: %s", tid, machErrString(kr))
	}
	return nil
}

// singleStepThread single-steps exactly one thread over a disarmed software
// breakpoint (or, for a plain StepInto, over one instruction from addr) and
// guarantees the instruction retires even if the kernel diverts the thread into
// a signal handler.
//
// Under the Mach model the resting state already has every thread
// Mach-suspended, so this does NOT suspend others: it arms ARMv8 hardware
// single-step (MDSCR_EL1.SS + PSTATE.SS) on tid, resumes ONLY tid, and records
// the sob* state so Wait's advanceStepOver drives it to completion. With only
// tid runnable and Go's sysmon (another thread) frozen, no fresh preemption
// SIGURG can be generated in the window; a SIGURG already pending on tid is
// stepped through by the retire loop. endThreadStep disarms single-step once the
// step trap is reported.
func (b *darwinBackend) singleStepThread(tid int, addr uint64) error {
	if !darwinAtomicStepOverEnabled() {
		return b.SingleStep(tid)
	}
	branch := b.instrAltersPC(addr)
	if err := b.setSingleStep(tid, true); err != nil {
		return fmt.Errorf("single step thread: arm single-step tid %d: %w", tid, err)
	}
	b.stepMu.Lock()
	b.stepThreadPort = tid
	b.sobActive = true
	b.sobAddr = addr
	b.sobBranch = branch
	b.sobSteps = 0
	b.stepMu.Unlock()
	b.flushReply(tid)
	if kr := C.bingo_resume_one_thread(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
		_ = b.setSingleStep(tid, false)
		b.stepMu.Lock()
		b.stepThreadPort = 0
		b.sobActive = false
		b.stepMu.Unlock()
		return fmt.Errorf("single step thread: resume tid %d: %s", tid, machErrString(kr))
	}
	return nil
}

// sobStepCap bounds the per-step-over retry loop. A single non-safepoint SIGURG
// handler is only a few hundred instructions; this leaves ample headroom while
// still guaranteeing Wait can never spin forever on a pathological target.
const sobStepCap = 200000

// instrAltersPC reports whether the 4-byte AArch64 instruction at addr can set
// PC to something other than addr+4 (any branch, branch-register, compare/test
// branch, conditional branch, or exception-generating instruction). When it can,
// we cannot assume the instruction retires to sobAddr+4 and fall back to trusting
// a single step. ADR/ADRP are excluded: they only compute an address and fall
// through. On any read error we conservatively report true (trust one step).
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
// exit/kill). It does not disarm single-step: on exit the thread is gone, and
// endThreadStep handles the live-teardown case.
func (b *darwinBackend) clearStepOver() {
	b.stepMu.Lock()
	b.sobActive = false
	b.stepMu.Unlock()
}

// advanceStepOver is called from Wait for every EXC_BREAKPOINT seen while a
// software-BP step-over is in flight. The stepped thread has just re-suspended
// on its single-step trap (mach_recv suspended it). advanceStepOver returns
// (event, true) once the disarmed instruction has retired past sobAddr — handing
// one StopSingleStep back to the engine — or (zero, false) after re-arming
// single-step and resuming the stepped thread again to work through a
// signal-handler excursion. Only the stepped thread ever runs, so no fresh
// preemption signals are generated. sobActive may be cleared concurrently by
// endThreadStep (Kill); we re-check it under stepMu and treat teardown as
// completion.
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
		// PC != sobAddr means it left the breakpoint. PC == sobAddr means a
		// handler just returned without executing it yet; step again.
		retired = regs.PC != addr
	} else {
		// Straight-line instruction: retired iff PC advanced to the next
		// instruction. A signal diversion lands PC in the runtime trampoline
		// (never sobAddr+4), cleanly distinguishing "executed" from "diverted".
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

	// Not yet past sobAddr: re-arm single-step (PSTATE.SS self-clears after each
	// trap) and resume the same (only-runnable) thread for another step.
	if err := b.setSingleStep(tid, true); err != nil {
		b.stepMu.Lock()
		b.sobActive = false
		b.stepMu.Unlock()
		return StopEvent{Reason: StopSingleStep, TID: tid}, true
	}
	b.flushReply(tid)
	if kr := C.bingo_resume_one_thread(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
		b.stepMu.Lock()
		b.sobActive = false
		b.stepMu.Unlock()
		return StopEvent{Reason: StopSingleStep, TID: tid}, true
	}
	return StopEvent{}, false
}

// endThreadStep tears down the critical section opened by singleStepThread:
// disarm hardware single-step on the stepped thread. It does NOT resume other
// threads — the resting state keeps every thread Mach-suspended until the engine
// issues ContinueProcess. Idempotent and safe to call when no step is in flight.
// May run on the engine loop goroutine (Kill) while advanceStepOver runs on the
// waitLoop goroutine, so the shared fields are touched under stepMu.
func (b *darwinBackend) endThreadStep() {
	b.stepMu.Lock()
	b.sobActive = false
	port := b.stepThreadPort
	b.stepThreadPort = 0
	b.stepMu.Unlock()
	if port != 0 {
		_ = b.setSingleStep(port, false)
	}
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

// StopProcess asynchronously interrupts the running tracee for Pause by posting
// a sentinel message to ctrlPort. Wait, blocked in mach_msg on the port set,
// wakes, stops the world and synthesises StopSignal{PauseSignal()} which the
// engine turns into EventPaused. Unlike the old model this sends no real signal:
// task/thread_suspend cannot wake a blocked receive, and Delve's alternative
// (a synchronous exception_raise RPC) can block the caller if the receiver is
// mid-handling, whereas a one-way port send never blocks. MACH_SEND_TIMED_OUT
// means a prior wake is still queued — one pending Pause is enough, so that is a
// no-op success.
func (b *darwinBackend) StopProcess() error {
	if !b.portsOK {
		return fmt.Errorf("StopProcess: no exception ports")
	}
	kr := C.bingo_send_ctrl(b.ctrlPort)
	if kr != C.KERN_SUCCESS && kr != C.MACH_SEND_TIMED_OUT {
		return fmt.Errorf("StopProcess: send ctrl: %s", machErrString(kr))
	}
	return nil
}

// PauseSignal is the sentinel the engine matches a manual-stop StopSignal
// against. See darwinPauseSignal and StopProcess.
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

// adoptExcThreadPort reconciles the send right the kernel inserted for the
// faulting thread of an exception message into the retained per-thread set,
// keeping exactly one right per thread name. If we already track the thread the
// message's extra right is released; otherwise it is adopted as the retained
// one. This runs on the waitLoop goroutine and shares threadPorts with Threads()
// (engine goroutine), so it takes threadMu. The name always ends with uref >= 1,
// so the tid stays valid for the subsequent GetRegisters/suspend/step this stop
// performs. Mirrors the adopt/dedup accounting in Threads().
func (b *darwinBackend) adoptExcThreadPort(thread C.mach_port_t) {
	b.threadMu.Lock()
	defer b.threadMu.Unlock()
	if b.threadPorts == nil {
		b.threadPorts = make(map[C.mach_port_t]struct{})
	}
	if _, ok := b.threadPorts[thread]; ok {
		C.bingo_port_deallocate(thread)
	} else {
		b.threadPorts[thread] = struct{}{}
	}
}

// TaskPortSendRefs reports the Mach send-right user-reference count on the cached
// tracee task port and whether the task port has been acquired yet. It exists
// for the darwin port-hygiene regression test: the exception path must not leak
// a task send right per stop (which would grow this count unboundedly toward
// KERN_UREFS_OVERFLOW and wedge Wait). Returns (0, false) before launch/attach.
func (b *darwinBackend) TaskPortSendRefs() (int, bool) {
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	if !b.taskOK {
		return 0, false
	}
	return int(C.bingo_port_send_refs(b.taskPort)), true
}

// Wait blocks on the Mach port set and turns each message into a StopEvent. It
// is the pure-Mach replacement for the old wait4 loop: EXC_BREAKPOINT messages
// (software BRK and hardware single-step) drive breakpoint/step detection, the
// dead-name notification signals process exit, and the control port carries
// Pause. The faulting thread is suspended and the exception acknowledged inside
// bingo_mach_recv before this returns, so a returned stop already holds that
// thread; a real breakpoint additionally stops the rest of the world here.
//
//nolint:gocognit // The receive loop is one serialized Mach stop state machine.
func (b *darwinBackend) Wait() (StopEvent, error) {
	b.probeOnce.Do(func() {
		if darwinSuspendProbeEnabled() {
			b.suspendProbeOn = true
			go b.suspendProbeWatchdog()
		}
	})
	for {
		var thread C.mach_port_t
		var exc C.int
		var code0 C.int64_t
		var id C.int
		var replyPort C.mach_port_t
		var replyBits C.uint
		var replyID C.int
		if b.suspendProbeOn {
			atomic.StoreInt64(&b.waitStartNanos, time.Now().UnixNano())
		}
		cls := C.bingo_mach_recv(b.portSet, -1, &thread, &exc, &code0, &id, 1, 0,
			&replyPort, &replyBits, &replyID)
		if b.suspendProbeOn {
			atomic.StoreInt64(&b.waitStartNanos, 0)
		}

		switch cls {
		case C.BINGO_MSG_NONE:
			continue

		case C.BINGO_MSG_ERROR:
			return StopEvent{}, fmt.Errorf("mach_msg recv failed")

		case C.BINGO_MSG_DEATH:
			b.clearStepOver()
			return b.reap()

		case C.BINGO_MSG_PAUSE:
			// A step-over never overlaps a running-state Pause; a control wake
			// seen mid-step is a stale leftover — ignore and keep receiving.
			if b.stepOverActive() {
				continue
			}
			if died, err := b.stopTheWorld(); err != nil {
				return StopEvent{}, err
			} else if died {
				b.clearStepOver()
				return b.reap()
			}
			return StopEvent{Reason: StopSignal, Signal: b.PauseSignal()}, nil

		case C.BINGO_MSG_EXC:
			tid := int(thread)
			// The exception message carried a fresh send right to the faulting
			// thread (see bingo_mach_recv). Fold it into the retained per-thread
			// set: release the redundant right if we already track this thread,
			// otherwise adopt it. Skipping this leaks one thread send right per
			// stop (unbounded on a long-lived thread hitting a hot breakpoint,
			// and one leaked dead name per exited thread under churn).
			b.adoptExcThreadPort(thread)
			// Defer the reply until this thread is resumed; see pendingReplies.
			b.stashReply(tid, replyInfo{port: replyPort, bits: replyBits, id: replyID})
			// Step-over in flight: ONLY the stepped thread's own single-step trap
			// advances it. A sibling/newly-created thread whose real-breakpoint
			// exception was still in flight when stop-the-world ran can surface
			// here mid-step — the non-blocking drain in bingo_stop_the_world
			// cannot catch an exception the kernel has not finished delivering
			// yet. Misreading such a straggler as the step's completion returns a
			// bogus StopSingleStep and leaves the real single-step armed, which
			// wedges the next resume (the residual churn hang). So only the
			// stepping thread's trap drives advanceStepOver; a straggler stays
			// parked (already suspended, reply stashed) and re-faults on the next
			// resume. Mirrors Delve's "loop until the trap belongs to the stepping
			// thread" guard in singleStep.
			if b.stepOverActive() {
				if b.isStepThread(tid) {
					if ev, done := b.advanceStepOver(); done {
						return ev, nil
					}
				}
				continue
			}
			// Ablation single-step (BINGO_DARWIN_ATOMIC_STEPOVER=0). Same guard:
			// only the thread the step was issued against completes it; a straggler
			// sibling breakpoint stays parked for the next resume.
			if b.consumeStep(tid) {
				return StopEvent{Reason: StopSingleStep, TID: tid}, nil
			}
			// Real breakpoint hit: stop the rest of the world into resting state,
			// then report the faulting thread.
			if died, err := b.stopTheWorld(); err != nil {
				return StopEvent{}, err
			} else if died {
				b.clearStepOver()
				return b.reap()
			}
			return StopEvent{Reason: StopBreakpoint, TID: tid}, nil

		default:
			return StopEvent{}, fmt.Errorf("mach_msg recv: unknown class %d", int(cls))
		}
	}
}

// stopTheWorld suspends every running thread and drains any queued breakpoint
// exceptions so the process reaches bingo's resting state with nothing pending.
// Returns died=true if the tracee exited during the stop.
func (b *darwinBackend) stopTheWorld() (died bool, err error) {
	task, err := b.task()
	if err != nil {
		return false, err
	}
	switch C.bingo_stop_the_world(task, b.portSet) {
	case C.BINGO_MSG_NONE:
		return false, nil
	case C.BINGO_MSG_DEATH:
		return true, nil
	default:
		return false, fmt.Errorf("stop the world failed")
	}
}

// reap collects the tracee's exit status after a dead-name notification. A
// concurrent kill path may have already reaped it (ECHILD), which is treated as
// a normal exit.
func (b *darwinBackend) reap() (StopEvent, error) {
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(b.pid, &ws, 0, nil)
		if err == nil {
			break
		}
		if isNoChildProcess(err) {
			return StopEvent{Reason: StopExited, TID: b.pid}, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		return StopEvent{}, fmt.Errorf("wait4 reap: %w", err)
	}
	if ws.Signaled() {
		return StopEvent{Reason: StopKilled, TID: b.pid}, nil
	}
	return StopEvent{Reason: StopExited, TID: b.pid, ExitCode: ws.ExitStatus()}, nil
}

// suspendProbeWatchdog (diagnostic only) polls waitStartNanos and, when Wait()
// has been blocked for longer than probeWedgeThreshold, dumps the tracee's
// TASK-level suspend_count and per-thread state once per stall episode.
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
	fmt.Fprintf(os.Stderr, "[suspendprobe] pid=%d waited=%s TASK_SUSPEND_COUNT=%d\n", b.pid, waited, int(sc))

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
	case stopped0 > 0 || stopped1 > 0:
		verdict = "threads STOPPED (task-suspend freeze)"
	case running1 > 0:
		verdict = "a thread is RUNNING and cannot be stopped (STW stall)"
	case waiting1 > 0 && static:
		verdict = "threads WAITING in kernel, PCs static (possible lost-wakeup)"
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

// replyInfo captures the fields bingo_reply_exception needs to acknowledge a
// deferred exception: the kernel's reply port, the remote bits, and the original
// request id. See the pendingReplies field and bingo_reply_exception.
type replyInfo struct {
	port C.mach_port_t
	bits C.uint
	id   C.int
}

// stashReply records the deferred reply for a faulting thread. If one is somehow
// already pending for that thread (should be impossible — a suspended thread
// can't fault again), the stale reply is flushed first so its exception isn't
// left un-acknowledged forever.
func (b *darwinBackend) stashReply(tid int, r replyInfo) {
	b.replyMu.Lock()
	if b.pendingReplies == nil {
		b.pendingReplies = make(map[int]replyInfo)
	}
	old, ok := b.pendingReplies[tid]
	b.pendingReplies[tid] = r
	b.replyMu.Unlock()
	if ok {
		C.bingo_reply_exception(old.port, old.bits, old.id)
	}
}

// flushReply acknowledges and clears any deferred exception for tid. It MUST be
// called before resuming tid: until the exception is replied the thread stays
// blocked on it and thread_resume alone will not run it.
func (b *darwinBackend) flushReply(tid int) {
	b.replyMu.Lock()
	r, ok := b.pendingReplies[tid]
	if ok {
		delete(b.pendingReplies, tid)
	}
	b.replyMu.Unlock()
	if ok {
		C.bingo_reply_exception(r.port, r.bits, r.id)
	}
}

// flushAllReplies acknowledges and clears every deferred exception. Called
// before a resume-all (ContinueProcess / kill) so no thread is left blocked on
// an un-acknowledged exception.
func (b *darwinBackend) flushAllReplies() {
	b.replyMu.Lock()
	pending := b.pendingReplies
	b.pendingReplies = nil
	b.replyMu.Unlock()
	for _, r := range pending {
		C.bingo_reply_exception(r.port, r.bits, r.id)
	}
}

func (b *darwinBackend) setPID(pid int) { b.pid = pid }

func (b *darwinBackend) clearStep() {
	b.stepMu.Lock()
	b.stepping = false
	b.stepTID = 0
	b.stepMu.Unlock()
}

func (b *darwinBackend) consumeStep(tid int) bool {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	// Only the thread the ablation step was issued against retires it; a trap on
	// any other thread is a concurrent breakpoint, not this step.
	if !b.stepping || b.stepTID != tid {
		return false
	}
	b.stepping = false
	b.stepTID = 0
	return true
}

// isStepThread reports whether an in-flight atomic step-over belongs to tid, so
// Wait only lets the stepping thread's own trap drive advanceStepOver.
func (b *darwinBackend) isStepThread(tid int) bool {
	b.stepMu.Lock()
	defer b.stepMu.Unlock()
	return b.sobActive && b.stepThreadPort == tid
}

func isNoChildProcess(err error) bool {
	return errors.Is(err, syscall.ECHILD)
}

// TextSlide returns the ASLR slide for the main executable: actual load address
// minus preferred __TEXT vmaddr. Returns 0 on any error.
//
// Scans the task's VM map for the first executable region with the 64-bit Mach-O
// magic. Works even at the very first suspended stop (before dyld runs), unlike
// TASK_DYLD_INFO whose image array is unpopulated at that point.
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

// task returns the tracee's Mach task port, acquired once via task_for_pid and
// cached thereafter. See the taskPort field comment for why re-acquiring per
// call is unsafe (it intermittently hangs task_for_pid in the kernel).
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
