//go:build darwin && arm64 && bingonative

package debugger

// Darwin/arm64 (Apple Silicon) Backend. Self-contained: process lifecycle via
// ptrace, registers via Mach thread_get_state, memory via mach_vm_read/write,
// and breakpoints via the ARM64 hardware debug registers (see WriteMemory).
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
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func newBackend() Backend {
	return &darwinBackend{}
}

// hwBreakpointSlots is how many hardware breakpoints we use. Apple Silicon
// exposes at least 6 instruction breakpoint registers; the engine needs at
// most two at a time (one user breakpoint + one transient step-over
// breakpoint), so 4 is comfortably sufficient.
const hwBreakpointSlots = 4

// rearmInterval is how often Wait re-applies the hardware-breakpoint set to
// every tracee thread while the process is running. See rearmWhileRunning: it
// bounds how long a freshly-created Go M can run unarmed (and thus miss the
// breakpoint) before it is caught, which is what keeps an allocation-free,
// cooperatively-yielding target from hanging wait4 forever.
const rearmInterval = 10 * time.Millisecond

type darwinBackend struct {
	pid      int
	stepMu   sync.Mutex
	stepping bool
	stepTID  int // Mach thread port saved by SingleStep, reused on the step trap

	// suspended tracks whether SingleStep Mach-suspended the sibling threads to
	// isolate a single-step. resumeSuspended re-enumerates the task's threads and
	// drains their suspend counts rather than resuming stored thread ports: a
	// thread's suspend count is kernel state that survives us dropping the port
	// send right, so we needn't (and must not) hold stale thread ports across the
	// step. Guarded by suspMu. Drained before any whole-process resume.
	suspMu    sync.Mutex
	suspended bool

	// hwBP holds the addresses of the hardware breakpoints currently armed on
	// the tracee, in slot order (max hwBreakpointSlots). Breakpoints are
	// installed in the ARM64 debug registers rather than by patching code, so
	// the tracee's ad-hoc code signature is never invalidated (no AMFI SIGKILL)
	// and there is no i-cache coherency problem. Guarded by hwMu. Re-applied to
	// every tracee thread on each resume — see applyDebugState.
	hwMu sync.Mutex
	hwBP []uint64

	// taskPort caches the Mach task port for the tracee. task_for_pid returns a
	// fresh send right on every call and never coalesces them, so calling it per
	// memory/thread/register operation (as the hot arm/disarm/re-arm paths do)
	// leaks thousands of port rights and eventually wedges the Mach call itself.
	// Fetch it once and reuse. Guarded by taskMu; invalidated by setPID.
	taskMu   sync.Mutex
	taskPort C.mach_port_t
	taskSet  bool

	// prevThreadsFree releases the thread-port send rights returned by the
	// previous engine-facing Threads() call. task_threads hands back a fresh send
	// right per thread; the engine borrows the returned tids transiently, so we
	// cannot free them before it returns. Instead we free the previous batch at
	// the start of the next Threads() call (the engine has finished with it by
	// then), keeping at most one batch outstanding. Guarded by threadsMu.
	threadsMu       sync.Mutex
	prevThreadsFree func()
}

// Darwin ptrace request codes from <sys/ptrace.h>. PT_ATTACH (10) makes the
// stop wait4-able by PID. Do NOT use PT_ATTACHEXC (14) — Mach exceptions are
// incompatible with our wait4-based Wait loop.
const (
	ptDarwinContinue = uintptr(7)
	ptDarwinKill     = uintptr(8)
	ptDarwinStep     = uintptr(9)
	ptDarwinAttach   = uintptr(10)
	ptDarwinDetach   = uintptr(11)
)

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

func startTracedProcess(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	// Do NOT use SysProcAttr{Ptrace:true} (PT_TRACE_ME). On Apple Silicon a
	// PT_TRACE_ME'd child does not get CS_DEBUGGED after exec, so its ad-hoc /
	// linker-signed code pages keep CS_KILL set. PT_ATTACH, by contrast, runs
	// cs_allow_invalid on the target: it sets CS_DEBUGGED and clears CS_KILL. So
	// we start the child normally and immediately PT_ATTACH by pid — attach
	// stops are wait4-visible just like PT_TRACE_ME, keeping the wait loop
	// unchanged. (Breakpoints are hardware, so we never patch code pages, but
	// PT_ATTACH's CS_DEBUGGED also keeps mach_vm_write data pokes from tripping
	// AMFI.)
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("exec %q: %w", binaryPath, err)
	}
	pid := cmd.Process.Pid
	if err := ptrace(ptDarwinAttach, uintptr(pid), 0, 0); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("PT_ATTACH pid %d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("wait for attach stop: %w", err)
	}
	// The PT_ATTACH stop is delivered as SIGSTOP. Darwin's WaitStatus.Stopped()
	// deliberately excludes SIGSTOP (it means job-control stop, not trace stop),
	// so check for the absence of exit/termination instead of ws.Stopped().
	if ws.Exited() || ws.Signaled() {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("target did not stop on attach: %v", ws)
	}
	return pid, cmd, nil
}

func attachToProcess(pid int) error {
	if err := ptrace(ptDarwinAttach, uintptr(pid), 0, 0); err != nil {
		return fmt.Errorf("PT_ATTACH pid %d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return fmt.Errorf("wait after PT_ATTACH: %w", err)
	}
	return nil
}

func killProcess(pid int, cmd *exec.Cmd) error {
	// A ptrace-stopped Darwin task ignores SIGKILL until it is resumed (a
	// leftover tracee "survives kill -9" until a SIGCONT), and a Mach-suspended
	// thread (left over from an in-flight single-step) blocks termination too.
	// Drain every thread's suspend count first.
	_ = C.bingo_resume_all_threads(C.int(pid))

	if cmd == nil {
		// Attached (not launched): detach, don't kill — we don't own the process.
		_ = ptrace(ptDarwinDetach, uintptr(pid), 1, 0)
		return nil
	}

	// Force the tracee to die by every avenue so the signal lands regardless of
	// its state — this is what makes Kill deterministic instead of wedging:
	//   PT_CONTINUE(SIGKILL): resume a ptrace-stopped tracee delivering SIGKILL
	//     (the common case: the process is stopped at a breakpoint).
	//   out-of-band SIGKILL: kills a tracee that is running (not ptrace-stopped),
	//     e.g. after a missed-breakpoint hang left it free-running.
	//   PT_KILL / PT_DETACH(SIGKILL): last resort if PT_CONTINUE failed while the
	//     tracee was stopped in an odd state (group-stop, pending Mach exception,
	//     mid-teardown after an AMFI kill). Detaching fully resumes it so a queued
	//     SIGKILL is finally delivered.
	if err := ptrace(ptDarwinContinue, uintptr(pid), 1, uintptr(syscall.SIGKILL)); err != nil {
		if p, e := os.FindProcess(pid); e == nil {
			_ = p.Signal(syscall.SIGKILL)
		}
		if err := ptrace(ptDarwinKill, uintptr(pid), 0, 0); err != nil {
			_ = ptrace(ptDarwinDetach, uintptr(pid), 1, uintptr(syscall.SIGKILL))
		}
	}

	// Reap, but never block indefinitely. cmd.Wait drives wait4(pid); it returns
	// promptly once the killed process dies (or ECHILD if the engine's waitLoop
	// reaped it first). Bound the wait so that even a tracee wedged in a state
	// where our SIGKILL somehow does not land cannot hang the engine loop (and
	// hence Kill) forever — the caller gets a clean return either way.
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	return nil
}

func (b *darwinBackend) ContinueProcess() error {
	b.resumeSuspended()
	b.clearStep()
	// Re-arm hardware breakpoints on every current thread. Go migrates
	// goroutines across OS threads (and creates new M's under load), so the
	// thread that next reaches a breakpoint may not be the one that hit the
	// last one; arming them all here keeps the breakpoint effective across
	// migration/churn.
	b.applyDebugState()
	// Resume with signal 0. We never re-inject asynchronous signals through the
	// ptrace signal argument (see resumeAfterSignal for the full rationale and
	// the one documented limitation). Re-posting a SIGURG via PT_CONTINUE's
	// signal argument delivers a spurious PROCESS-directed copy that wedges
	// cooperative Go code ~15% of the time (a lost-wakeup in __psynch_cvwait);
	// dropping it keeps the common path — all cooperative code, the whole E2E
	// suite — reliable.
	err := ptrace(ptDarwinContinue, uintptr(b.pid), 1, 0)
	if err != nil {
		return fmt.Errorf("PT_CONTINUE: %w", err)
	}
	return nil
}

func (b *darwinBackend) SingleStep(tid int) error {
	// Single-step exactly `tid` on Darwin/arm64.
	//
	// Darwin's ptrace PT_STEP resumes the whole task at the BSD level and steps
	// the runnable thread(s). To make ONLY `tid` advance one instruction we
	// Mach-suspend every other thread first, then PT_STEP. ContinueProcess (and
	// the next SingleStep / kill) resumes the siblings.
	//
	// NOTE: the engine's darwin step-over does NOT go through here — it uses a
	// transient hardware breakpoint on the next source line instead (see
	// needsTempBPStepOver and stepOverBreakpointViaTempBP). This path remains
	// for StepInto / StepOut.
	b.resumeSuspended()
	b.suspendSiblings(tid)
	b.setStep(tid)
	if err := ptrace(ptDarwinStep, uintptr(b.pid), 1, 0); err != nil {
		b.clearStep()
		b.resumeSuspended()
		return fmt.Errorf("PT_STEP: %w", err)
	}
	return nil
}

// suspendSiblings Mach-suspends every tracee thread except target, so a
// subsequent PT_STEP advances only target. Best-effort: threads that fail to
// suspend (e.g. just exited) are skipped. The thread ports are released
// immediately — a thread's suspend count is kernel state that persists after we
// drop the send right, and resumeSuspended re-enumerates to drain it.
func (b *darwinBackend) suspendSiblings(target int) {
	tids, free, err := b.threadsWithFree()
	if err != nil {
		return
	}
	defer free()
	b.suspMu.Lock()
	defer b.suspMu.Unlock()
	for _, tid := range tids {
		if tid == target {
			continue
		}
		C.bingo_thread_suspend(C.mach_port_t(tid))
	}
	b.suspended = true
}

// resumeSuspended drains the Mach suspend count of every current tracee thread
// if a prior suspendSiblings left siblings suspended. Idempotent: a no-op when
// nothing was suspended. Re-enumerates rather than resuming stored ports (which
// would be stale send rights by now — see the suspended field).
func (b *darwinBackend) resumeSuspended() {
	b.suspMu.Lock()
	defer b.suspMu.Unlock()
	if !b.suspended {
		return
	}
	if tids, free, err := b.threadsWithFree(); err == nil {
		defer free()
		for _, tid := range tids {
			C.bingo_thread_resume(C.mach_port_t(tid))
		}
	}
	b.suspended = false
}

func (b *darwinBackend) StopProcess() error {
	p, err := os.FindProcess(b.pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGSTOP)
}

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

// WriteMemory installs/removes breakpoints via the ARM64 hardware debug
// registers instead of patching the tracee's code.
//
// The engine plants a breakpoint by writing the architecture trap instruction
// (archTrapInstruction) at an address, and removes it by writing back the
// original bytes. Rather than modify executable memory — which on Apple Silicon
// invalidates the ad-hoc code signature and gets the tracee AMFI-SIGKILLed on
// the next page-in — we translate those writes into hardware-breakpoint arm /
// disarm operations and leave memory untouched:
//
//   - write == the trap instruction  => arm a hardware breakpoint at addr.
//   - write of anything else at an armed hardware-breakpoint addr (the engine
//     restoring the saved original bytes) => disarm it.
//
// ReadMemory always returns the real (unpatched) instruction, so the original
// bytes the engine saves are correct. Any other write (never emitted by the
// current engine) falls through to a genuine memory write.
func (b *darwinBackend) WriteMemory(addr uint64, src []byte) error {
	if len(src) == 0 {
		return nil
	}
	trap := archTrapInstruction()
	if len(src) == len(trap) && string(src) == string(trap) {
		return b.armHWBreakpoint(addr)
	}
	if b.isHWBreakpoint(addr) {
		return b.disarmHWBreakpoint(addr)
	}
	return b.rawWriteMemory(addr, src)
}

// rawWriteMemory performs a genuine write into tracee memory. Only used as a
// fallback for writes the engine never actually emits for breakpoints (kept for
// Backend-contract completeness / data pokes).
func (b *darwinBackend) rawWriteMemory(addr uint64, src []byte) error {
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

// armHWBreakpoint records addr as an active hardware breakpoint and applies the
// debug registers to every tracee thread. Idempotent for an already-armed addr.
func (b *darwinBackend) armHWBreakpoint(addr uint64) error {
	b.hwMu.Lock()
	for _, a := range b.hwBP {
		if a == addr {
			b.hwMu.Unlock()
			b.applyDebugState()
			return nil
		}
	}
	if len(b.hwBP) >= hwBreakpointSlots {
		b.hwMu.Unlock()
		return fmt.Errorf("no free hardware breakpoint slot for 0x%x (max %d)", addr, hwBreakpointSlots)
	}
	b.hwBP = append(b.hwBP, addr)
	b.hwMu.Unlock()
	b.applyDebugState()
	return nil
}

// disarmHWBreakpoint removes addr from the active set and re-applies the debug
// registers to every tracee thread.
func (b *darwinBackend) disarmHWBreakpoint(addr uint64) error {
	b.hwMu.Lock()
	for i, a := range b.hwBP {
		if a == addr {
			b.hwBP = append(b.hwBP[:i], b.hwBP[i+1:]...)
			break
		}
	}
	b.hwMu.Unlock()
	b.applyDebugState()
	return nil
}

func (b *darwinBackend) isHWBreakpoint(addr uint64) bool {
	b.hwMu.Lock()
	defer b.hwMu.Unlock()
	for _, a := range b.hwBP {
		if a == addr {
			return true
		}
	}
	return false
}

// applyDebugState writes the current hardware-breakpoint set into every tracee
// thread's debug registers. Best-effort per thread (a thread that just exited is
// skipped). Called whenever the set changes and before every resume, so threads
// created since the last resume also get the breakpoints.
func (b *darwinBackend) applyDebugState() {
	b.hwMu.Lock()
	addrs := append([]uint64(nil), b.hwBP...)
	b.hwMu.Unlock()

	tids, free, err := b.threadsWithFree()
	if err != nil {
		return
	}
	defer free()
	var cAddrs *C.uint64_t
	if len(addrs) > 0 {
		cAddrs = (*C.uint64_t)(unsafe.Pointer(&addrs[0]))
	}
	for _, tid := range tids {
		C.bingo_set_thread_hw_breakpoints(C.mach_port_t(tid), cAddrs, C.int(len(addrs)))
	}
}

// rearmWhileRunning starts a ticker that re-applies the hardware-breakpoint set
// to every current tracee thread every rearmInterval while the process runs,
// and returns a stop function that halts the ticker and waits for any in-flight
// re-arm to finish (so no re-arm races the stop handling that follows wait4).
//
// Why this is necessary. Hardware breakpoints live in each thread's debug
// registers, so a thread created after the last resume is NOT armed until we
// write its registers. applyDebugState re-arms on every ContinueProcess and
// every signal-driven resume, but those only fire when the tracee produces a
// stop. Our E2E targets allocate nothing in the hot loop (so the GC never runs
// and never sends SIGURG) and cooperatively yield every iteration (so async
// preemption never targets them either). If the main goroutine migrates onto a
// Go M created since the last resume — which the churn target does constantly
// via runtime.LockOSThread — that M is unarmed, misses the breakpoint, and
// produces no stop of its own; wait4 then blocks until the harness's timeout.
// Re-arming on a timer bounds that window to rearmInterval: the migrated M is
// armed within one tick and traps on its next loop iteration.
//
// The re-arm is NON-SUSPENDING: it writes ARM_DEBUG_STATE64 to each thread via
// applyDebugState without Mach-suspending anything. An earlier version suspended
// the whole task on every tick to force threads off-core (thread_set_state only
// updates a thread's saved context; the CPU reloads the debug registers when the
// thread is next scheduled on-core). That was reliable for arming, but freezing
// the entire Go runtime — sysmon, the netpoller, a goroutine holding the
// scheduler or timer lock — at a 10ms cadence perturbed the runtime's timer
// wakeups enough to occasionally lose the main goroutine's time.Sleep wakeup and
// deadlock the tracee (every M ends up parked, correctly armed, with no thread
// ever reaching the breakpoint). Because the reliable arming path is
// applyDebugState on resume (the tracee is ptrace-stopped, so every existing
// thread is already off-core and is armed for certain), the timer only needs to
// catch threads created mid-run, which context-switch within microseconds on a
// yielding target and so pick up the debug registers from a plain non-suspending
// write. Dropping the whole-task suspend removes the deadlock without
// reintroducing missed breakpoints. Skipped while single-stepping, since
// re-writing ARM_DEBUG_STATE64 would perturb an in-flight PT_STEP.
func (b *darwinBackend) rearmWhileRunning() (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(rearmInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if !b.isStepping() {
					b.applyDebugState()
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// threadAtHWBreakpoint finds a tracee thread whose PC is exactly on one of our
// armed hardware breakpoints (a hardware breakpoint traps with PC AT the
// matched instruction). Returns that thread and address so Wait can hand the
// engine an already-resolved breakpoint stop.
func (b *darwinBackend) threadAtHWBreakpoint() (tid int, pc uint64, ok bool) {
	b.hwMu.Lock()
	armed := append([]uint64(nil), b.hwBP...)
	b.hwMu.Unlock()
	if len(armed) == 0 {
		return 0, 0, false
	}
	tids, free, err := b.threadsWithFree()
	if err != nil {
		return 0, 0, false
	}
	defer free()
	for _, t := range tids {
		regs, err := b.GetRegisters(t)
		if err != nil {
			continue
		}
		for _, a := range armed {
			if regs.PC == a {
				return t, a, true
			}
		}
	}
	return 0, 0, false
}

// threadsWithFree enumerates the tracee's threads and returns their Mach thread
// ports as tids plus a free closure that releases every one of those port send
// rights (and the array). task_threads inserts a fresh send right per thread on
// every call, so callers MUST invoke free once they are done using the tids —
// otherwise the port-right count grows without bound at re-arm frequency and
// task_threads / thread_set_state eventually fail, silently disarming the
// hardware breakpoints and hanging the tracee. On error the returned free is a
// no-op.
func (b *darwinBackend) threadsWithFree() (tids []int, free func(), err error) {
	noop := func() {}
	task, err := b.task()
	if err != nil {
		return nil, noop, err
	}
	var threads C.thread_act_port_array_t
	var count C.mach_msg_type_number_t
	kr := C.bingo_thread_list(task, &threads, &count)
	if kr != C.KERN_SUCCESS {
		return nil, noop, fmt.Errorf("task_threads pid %d: %s", b.pid, machErrString(kr))
	}
	ports := unsafe.Slice((*C.mach_port_t)(unsafe.Pointer(threads)), int(count))
	tids = make([]int, len(ports))
	for i, p := range ports {
		tids[i] = int(p)
	}
	return tids, func() { C.bingo_free_thread_list(threads, count) }, nil
}

// Threads enumerates the tracee's threads for the engine. Because the engine
// borrows the returned tids (Mach thread ports) transiently and cannot release
// them itself, we defer freeing each batch until the next call — see
// prevThreadsFree. At most one batch is ever outstanding.
func (b *darwinBackend) Threads() ([]int, error) {
	b.threadsMu.Lock()
	defer b.threadsMu.Unlock()
	if b.prevThreadsFree != nil {
		b.prevThreadsFree()
		b.prevThreadsFree = nil
	}
	tids, free, err := b.threadsWithFree()
	if err != nil {
		return nil, err
	}
	b.prevThreadsFree = free
	return tids, nil
}

//nolint:gocognit // The wait loop is one serialized ptrace state machine.
func (b *darwinBackend) Wait() (StopEvent, error) {
	for {
		stopRearm := b.rearmWhileRunning()
		var ws syscall.WaitStatus
		tid, err := syscall.Wait4(b.pid, &ws, 0, nil)
		stopRearm()
		if err != nil {
			if isNoChildProcess(err) {
				return StopEvent{Reason: StopExited, TID: b.pid}, nil
			}
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}
		if ws.Exited() {
			return StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, nil
		}
		if ws.Signaled() {
			return StopEvent{Reason: StopKilled, TID: tid}, nil
		}
		if !ws.Stopped() {
			continue
		}

		sig := ws.StopSignal()

		if sig == syscall.SIGTRAP {
			if stepping, thread := b.consumeStep(); stepping {
				return StopEvent{Reason: StopSingleStep, TID: thread}, nil
			}
			// A hardware-breakpoint trap: hand the engine the exact thread and
			// PC. Nothing is patched in memory, so the engine's fallback scan
			// for a trap instruction would not find it.
			if t, pc, ok := b.threadAtHWBreakpoint(); ok {
				return StopEvent{Reason: StopBreakpoint, TID: t, PC: pc}, nil
			}
			return StopEvent{Reason: StopBreakpoint}, nil
		}

		// SIGURG (Go async preemption) and SIGWINCH (terminal resize) are
		// asynchronous, runtime-handled signals; resume transparently without
		// re-injecting them (see resumeAfterSignal). A breakpoint trap can
		// coalesce with one of these: Go fires SIGURG every few milliseconds, so
		// when a thread hits a hardware breakpoint at nearly the same instant
		// wait4 can surface the async signal and hide the trap. If any thread is
		// parked on an armed hardware breakpoint, report the breakpoint now and
		// leave the process stopped; the coalesced async signal is dropped on the
		// next resume (see resumeAfterSignal for why dropping is the correct
		// wait4 policy). Prioritising the breakpoint is what matters here.
		if sig == syscall.SIGURG || sig == syscall.SIGWINCH {
			if !b.isStepping() {
				if t, pc, ok := b.threadAtHWBreakpoint(); ok {
					return StopEvent{Reason: StopBreakpoint, TID: t, PC: pc}, nil
				}
			}
			if err := b.resumeAfterSignal(sig); err != nil {
				if isNoSuchProcess(err) {
					return StopEvent{Reason: StopKilled, TID: tid}, nil
				}
				return StopEvent{}, err
			}
			continue
		}

		// Other signals arriving mid-step: re-deliver via PT_STEP so the step
		// still completes (the target consumes the signal, then retires one
		// instruction and traps). PT_CONTINUE here would run the target free.
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

func (b *darwinBackend) setPID(pid int) {
	b.taskMu.Lock()
	if b.pid != pid {
		b.taskPort = 0
		b.taskSet = false
	}
	b.pid = pid
	b.taskMu.Unlock()
}

// needsTempBPStepOver reports that this backend cannot single-step an arbitrary
// thread, so the engine must step over breakpoints with a transient next-line
// breakpoint. See tempBPStepper and SingleStep for why (PT_STEP arms the task's
// first thread, not the tid we pass). With hardware breakpoints the transient
// next-line breakpoint is likewise a debug-register breakpoint, so this remains
// the correct, memory-safe step-over strategy on darwin.
func (b *darwinBackend) needsTempBPStepOver() bool { return true }

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

// resumeAfterSignal resumes the tracee after wait4 surfaced an asynchronous,
// runtime-handled signal (SIGURG async preemption, SIGWINCH resize). It resumes
// WITHOUT re-injecting the signal via the ptrace signal argument, which on the
// wait4/BSD-stop model DROPS the intercepted signal: XNU already cleared the
// thread's pending bit to take the trace-stop, and PT_CONTINUE with signal 0
// does not re-post it.
//
// Dropping is deliberate — it is the better of two imperfect wait4 options, and
// the choice is proven empirically by asyncpreempt_e2e_test.go (env-guarded, not
// part of the acceptance gate):
//
//   - Re-inject (PT_CONTINUE with the signal): XNU's ptrace re-adds the signal to
//     the stopped (sigwait) thread AND calls psignal(), which is PROCESS-directed.
//     The intended M does get preempted, so a purely async-preemptible goroutine
//     works — but the extra process-directed copy lands on an arbitrary thread and
//     ~15% of the time interrupts an M parked in __psynch_cvwait, causing a
//     lost-wakeup wedge. That is the original churn hang; it fails the E2E gate.
//   - Drop (signal 0, this code): no spurious copy, so cooperative Go code — which
//     is every realistic program and the entire E2E suite — is 100% reliable. The
//     cost: a goroutine that is ONLY async-preemptible (a tight loop with no calls,
//     allocations, or channel ops, hence no cooperative safe point) is not
//     preempted for stop-the-world while traced, so runtime.GC() in such a target
//     can hang under the debugger. This is a corner case for a concurrency
//     debugger; correctness of the common path is worth it.
//
// Thread-directed re-delivery (reach exactly the intended M with no spurious copy)
// is not achievable under wait4: PT_THUPDATE sets a thread's siglist bit but has
// no wakeup(&t->sigwait)/task_resume, so it cannot drive the stop's resume, and
// PT_CONTINUE(sig) is process-directed. The only complete fix is to move the stop
// source to Mach exception ports (where SIGURG is never intercepted), which
// AGENTS.md places out of scope. Delve takes the same "resume with 0" stance on
// its wait4 path (pkg/proc/native/threads_darwin.go: ptraceCont always passes 0).
//
// While a single-step is in flight we must resume with PT_STEP so the step still
// retires; otherwise PT_CONTINUE, re-arming the hardware breakpoints on all
// current threads first so threads created since the last resume also trap.
func (b *darwinBackend) resumeAfterSignal(sig syscall.Signal) error {
	request := ptDarwinContinue
	if b.isStepping() {
		request = ptDarwinStep
	} else {
		b.applyDebugState()
	}
	if err := ptrace(request, uintptr(b.pid), 1, 0); err != nil {
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

// task returns the Mach task port for the tracee. Cached after the first
// successful lookup: task_for_pid hands back a new send right each call and
// leaks otherwise, and it can wedge under a flood of outstanding rights.
// Requires the com.apple.security.cs.debugger entitlement or disabled SIP.
func (b *darwinBackend) task() (C.mach_port_t, error) {
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	if b.taskSet && b.taskPort != 0 {
		return b.taskPort, nil
	}
	var task C.mach_port_t
	kr := C.bingo_task_for_pid(C.int(b.pid), &task)
	if kr != C.KERN_SUCCESS {
		return 0, fmt.Errorf("task_for_pid(%d): %s — debugger entitlement required",
			b.pid, machErrString(kr))
	}
	b.taskPort = task
	b.taskSet = true
	return task, nil
}

func machErrString(kr C.kern_return_t) string {
	return C.GoString(C.mach_error_string(kr))
}
