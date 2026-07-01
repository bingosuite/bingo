//go:build linux && amd64

package debugger

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// E2EDBG temporary instrumentation (removed before final). Logs at Info so it
// surfaces in CI without BINGO_E2E_DEBUG. Capped to avoid flooding on livelock.
var dbgWaitN atomic.Int64

func dbgLog(msg string, args ...any) {
	if dbgWaitN.Add(1) <= 600 {
		slog.Info("E2EDBG "+msg, args...)
	}
}

func newBackend() Backend {
	return &linuxBackend{}
}

type linuxBackend struct {
	pid         int
	stepping    bool // true after SingleStep; classifies the next SIGTRAP
	lastStopTID int

	// delayedSignal holds a non-fault signal (e.g. SIGURG async preemption)
	// observed mid-single-step. It is deferred and injected on the next
	// ContinueProcess/SingleStep rather than dropped. Mirrors Delve's
	// nativeThread.delayedSignal / resumeWithSig so signals are forwarded to
	// the tracee instead of being swallowed.
	delayedSignal int
}

const linuxPtraceOptions = syscall.PTRACE_O_TRACEEXIT |
	syscall.PTRACE_O_TRACEEXEC

// startTracedProcess forks under ptrace. The child is stopped at its first
// instruction (execve SIGTRAP) ready for the engine to set breakpoints.
func startTracedProcess(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	// codeql-suppress[go/command-injection]: The debugger intentionally launches the local binary selected by the operator.
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("exec %q: %w", binaryPath, err)
	}

	pid := cmd.Process.Pid

	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("wait for execve stop: %w", err)
	}
	if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("unexpected initial stop: %v", ws)
	}

	if err := syscall.PtraceSetOptions(pid, linuxPtraceOptions); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("PTRACE_SETOPTIONS: %w", err)
	}

	return pid, cmd, nil
}

func attachToProcess(pid int) error {
	if err := syscall.PtraceAttach(pid); err != nil {
		return fmt.Errorf("PTRACE_ATTACH pid %d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return fmt.Errorf("wait after PTRACE_ATTACH: %w", err)
	}
	return nil
}

func killProcess(pid int, cmd *exec.Cmd) error {
	if cmd != nil {
		if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
			return err
		}
		_ = cmd.Wait()
		return nil
	}
	// Attached (not launched): detach, don't kill — we don't own the process.
	_ = syscall.PtraceDetach(pid)
	return nil
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

func (b *linuxBackend) ContinueProcess() error {
	b.stepping = false
	sig := b.delayedSignal
	b.delayedSignal = 0
	tid := b.traceTID()
	dbgLog("ContinueProcess", "tid", tid, "sig", sig)
	if err := syscall.PtraceCont(tid, sig); err != nil {
		return fmt.Errorf("PTRACE_CONT tid %d (sig %d): %w", tid, sig, err)
	}
	return nil
}

func (b *linuxBackend) SingleStep(tid int) error {
	b.stepping = true
	dbgLog("SingleStep", "tid", tid)
	if err := ptraceSingleStepSig(tid, 0); err != nil {
		return fmt.Errorf("PTRACE_SINGLESTEP tid %d: %w", tid, err)
	}
	return nil
}

func (b *linuxBackend) StopProcess() error {
	p, err := os.FindProcess(b.pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGSTOP)
}

func (b *linuxBackend) ReadMemory(addr uint64, dst []byte) error {
	tid := b.traceTID()
	n, err := syscall.PtracePeekData(tid, uintptr(addr), dst)
	if err != nil {
		return fmt.Errorf("PTRACE_PEEKDATA tid %d 0x%x: %w", tid, addr, err)
	}
	if n != len(dst) {
		return fmt.Errorf("PTRACE_PEEKDATA tid %d 0x%x: short read %d/%d", tid, addr, n, len(dst))
	}
	return nil
}

func (b *linuxBackend) WriteMemory(addr uint64, src []byte) error {
	tid := b.traceTID()
	n, err := syscall.PtracePokeData(tid, uintptr(addr), src)
	if err != nil {
		return fmt.Errorf("PTRACE_POKEDATA tid %d 0x%x: %w", tid, addr, err)
	}
	if n != len(src) {
		return fmt.Errorf("PTRACE_POKEDATA tid %d 0x%x: short write %d/%d", tid, addr, n, len(src))
	}
	return nil
}

// GetRegisters reads PTRACE_GETREGS. The Go runtime stores g at FS_BASE on amd64.
func (b *linuxBackend) GetRegisters(tid int) (Registers, error) {
	var r syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(tid, &r); err != nil {
		return Registers{}, fmt.Errorf("PTRACE_GETREGS tid %d: %w", tid, err)
	}
	return Registers{
		PC:  r.Rip,
		SP:  r.Rsp,
		BP:  r.Rbp,
		TLS: r.Fs_base,
	}, nil
}

// SetRegisters writes back the engine-owned fields, preserving everything else
// by reading the full register set first.
func (b *linuxBackend) SetRegisters(tid int, reg Registers) error {
	var r syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(tid, &r); err != nil {
		return fmt.Errorf("PTRACE_GETREGS (pre-set) tid %d: %w", tid, err)
	}
	r.Rip = reg.PC
	r.Rsp = reg.SP
	r.Rbp = reg.BP
	r.Fs_base = reg.TLS
	if err := syscall.PtraceSetRegs(tid, &r); err != nil {
		return fmt.Errorf("PTRACE_SETREGS tid %d: %w", tid, err)
	}
	return nil
}

func (b *linuxBackend) Threads() ([]int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", b.pid))
	if err != nil {
		return nil, fmt.Errorf("read /proc/%d/task: %w", b.pid, err)
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		var tid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &tid); err == nil {
			tids = append(tids, tid)
		}
	}
	if len(tids) == 0 {
		return nil, fmt.Errorf("no threads for pid %d", b.pid)
	}
	return tids, nil
}

// Wait blocks until the tracee produces a meaningful debug stop. Single-step
// vs breakpoint disambiguation uses b.stepping (reliable because ptrace is
// serialised). PTRACE_EVENT stops (clone/exec/exit) are handled internally
// and don't surface to the engine.
//
//nolint:gocognit,gocyclo // The wait loop is one serialized ptrace state machine.
func (b *linuxBackend) Wait() (StopEvent, error) {
	for {
		var ws syscall.WaitStatus
		// WALL includes clone()d threads.
		tid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		dbgLog("wait4 return", "tid", tid, "err", err, "raw", uint32(ws),
			"stopped", ws.Stopped(), "stopsig", int(ws.StopSignal()),
			"cause", ws.TrapCause(), "exited", ws.Exited(),
			"signaled", ws.Signaled(), "stepping", b.stepping, "pid", b.pid)
		if err != nil {
			if isNoChildProcess(err) {
				return StopEvent{Reason: StopExited, TID: b.pid}, nil
			}
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}

		if ws.Exited() {
			if tid == b.pid {
				b.recordStop(tid)
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, nil
			}
			continue
		}

		if ws.Signaled() {
			if tid != b.pid {
				continue
			}
			b.recordStop(tid)
			return StopEvent{Reason: StopKilled, TID: tid}, nil
		}

		if !ws.Stopped() {
			continue
		}

		sig := ws.StopSignal()

		// PTRACE_EVENT stops are encoded as SIGTRAP | (event << 8).
		if sig == syscall.SIGTRAP {
			cause := ws.TrapCause()

			switch cause {
			case syscall.PTRACE_EVENT_CLONE:
				if err := continueIfTraceeExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT clone parent tid %d: %w", tid, err)
				}
				continue

			case syscall.PTRACE_EVENT_EXIT:
				if tid != b.pid {
					if err := continueIfTraceeExists(tid, 0); err != nil {
						return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting thread tid %d: %w", tid, err)
					}
					continue
				}
				// Main thread is about to call exit_group — let it actually exit.
				if err := continueIfTraceeExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting process tid %d: %w", tid, err)
				}
				b.recordStop(tid)
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: 0}, nil

			case syscall.PTRACE_EVENT_EXEC:
				if err := syscall.PtraceCont(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exec tid %d: %w", tid, err)
				}
				continue

			case 0:
				b.recordStop(tid)

				if b.stepping {
					b.stepping = false
					dbgLog("-> StopSingleStep", "tid", tid)
					return StopEvent{Reason: StopSingleStep, TID: tid}, nil
				}

				dbgLog("-> StopBreakpoint", "tid", tid)
				return StopEvent{
					Reason: StopBreakpoint,
					TID:    tid,
				}, nil

			default:
				if err := continueIfTraceeExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT trap cause %d tid %d: %w", cause, tid, err)
				}
				continue
			}
		}

		if sig == syscall.SIGSTOP && tid != b.pid {
			if err := syscall.PtraceSetOptions(tid, linuxPtraceOptions); err != nil && !isNoSuchProcess(err) {
				return StopEvent{}, fmt.Errorf("PTRACE_SETOPTIONS clone child tid %d: %w", tid, err)
			}
			if err := syscall.Kill(b.pid, syscall.SIGCONT); err != nil && !isNoSuchProcess(err) {
				return StopEvent{}, fmt.Errorf("SIGCONT tracee pid %d: %w", b.pid, err)
			}
			if err := continueTraceeGroup(b.pid, 0); err != nil {
				return StopEvent{}, err
			}
			continue
		}

		// A real (non-SIGTRAP) signal was delivered to the tracee. On Linux
		// ptrace resume commands (PTRACE_CONT/SINGLESTEP) are restricted to the
		// tracer thread — the engine's event-loop goroutine — so this wait
		// goroutine must NOT resume here (doing so returns ESRCH and wedges the
		// tracee). Hand the signal to the engine, which re-steps or continues
		// from the tracer goroutine via ResumeSignal, forwarding a synchronous
		// fault or deferring an async signal (SIGURG preemption) — mirroring
		// Delve's singleStep() / resumeWithSig(). Report whether we were mid
		// single-step so the engine re-issues the right resume.
		stepping := b.stepping
		b.stepping = false
		b.recordStop(tid)
		dbgLog("-> StopSignal", "tid", tid, "sig", int(sig), "stepping", stepping)
		return StopEvent{Reason: StopSignal, TID: tid, Signal: int(sig), Stepping: stepping}, nil
	}
}

// ResumeSignal resumes the tracee after a non-trap signal. It runs on the
// engine goroutine (the ptrace tracer thread), which is why Wait defers signal
// resumes here instead of issuing them itself. Mirrors Delve: fault signals are
// re-delivered on the next single-step, asynchronous signals are deferred to the
// next continue, and a signal that interrupted a continue is forwarded.
func (b *linuxBackend) ResumeSignal(tid, signal int, stepping bool) error {
	if tid == 0 {
		tid = b.traceTID()
	}
	if stepping {
		b.stepping = true
		switch signal {
		case int(syscall.SIGILL), int(syscall.SIGBUS), int(syscall.SIGFPE),
			int(syscall.SIGSEGV), int(syscall.SIGSTKFLT):
			// Synchronous fault from the stepped instruction: re-issue the
			// step delivering the signal so the tracee's handler observes it.
			dbgLog("ResumeSignal re-step fault", "tid", tid, "sig", signal)
			return singleStepSig(tid, signal)
		case int(syscall.SIGSTOP):
			// Spurious/late SIGSTOP: swallow and keep stepping.
			dbgLog("ResumeSignal re-step drop-stop", "tid", tid)
			return singleStepSig(tid, 0)
		default:
			// Asynchronous signal (SIGURG preemption, SIGCHLD, ...): defer to
			// the next continue so entering a handler mid-step doesn't perturb
			// the step's trap PC; re-step suppressing it now.
			b.delayedSignal = signal
			dbgLog("ResumeSignal re-step defer", "tid", tid, "sig", signal)
			return singleStepSig(tid, 0)
		}
	}
	// The signal interrupted a continue: forward it transparently so the
	// tracee's runtime/handlers see it. Never re-deliver a bare SIGSTOP.
	forward := signal
	if signal == int(syscall.SIGSTOP) {
		forward = 0
	}
	b.stepping = false
	dbgLog("ResumeSignal continue-forward", "tid", tid, "sig", signal, "forward", forward)
	return continueIfTraceeExists(tid, forward)
}

var _ Backend = (*linuxBackend)(nil)
var _ signalResumer = (*linuxBackend)(nil)

func (b *linuxBackend) setPID(pid int) {
	b.pid = pid
	b.lastStopTID = pid
}

func (b *linuxBackend) traceTID() int {
	if b.lastStopTID != 0 {
		return b.lastStopTID
	}
	return b.pid
}

func (b *linuxBackend) recordStop(tid int) {
	if tid != 0 {
		b.lastStopTID = tid
	}
}

func isNoSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrNotExist)
}

func isNoChildProcess(err error) bool {
	return errors.Is(err, syscall.ECHILD)
}

func continueIfTraceeExists(tid int, signal int) error {
	if tid == 0 {
		return nil
	}
	if err := syscall.PtraceCont(tid, signal); err != nil && !isNoSuchProcess(err) {
		return err
	}
	return nil
}

func continueTraceeGroup(pid int, signal int) error {
	tids, err := taskTIDs(pid)
	if err != nil {
		if isNoSuchProcess(err) {
			return nil
		}
		return err
	}
	if len(tids) == 0 {
		return continueIfTraceeExists(pid, signal)
	}
	for _, tid := range tids {
		if err := continueIfTraceeExists(tid, signal); err != nil {
			return fmt.Errorf("PTRACE_CONT tid %d: %w", tid, err)
		}
	}
	return nil
}

func taskTIDs(pid int) ([]int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(entries))
	for _, entry := range entries {
		var tid int
		if _, err := fmt.Sscanf(entry.Name(), "%d", &tid); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids, nil
}

// ptraceSingleStepSig issues PTRACE_SINGLESTEP delivering signal sig to the
// tracee (the syscall.PtraceSingleStep wrapper always passes signal 0, which
// would drop a pending signal). Mirrors Delve's ptraceSingleStep(pid, sig).
func ptraceSingleStepSig(tid, sig int) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE,
		uintptr(syscall.PTRACE_SINGLESTEP), uintptr(tid), 0, uintptr(sig), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func singleStepSig(tid, sig int) error {
	if tid == 0 {
		return nil
	}
	if err := ptraceSingleStepSig(tid, sig); err != nil && !isNoSuchProcess(err) {
		return err
	}
	return nil
}
