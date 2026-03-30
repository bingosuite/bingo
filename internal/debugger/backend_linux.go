//go:build linux

package debugger

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func newBackend() Backend {
	return &linuxBackend{}
}

// linuxBackend implements Backend using Linux ptrace.
type linuxBackend struct {
	pid      int
	stepping bool // true if the last command was SingleStep; used to classify SIGTRAP
}

// ── Process lifecycle (called by process.go helpers) ─────────────────────────

// startTracedProcess forks binaryPath under ptrace and returns its PID.
// The child is stopped at its first instruction (execve SIGTRAP) ready for
// the engine to set breakpoints before calling ContinueProcess.
func startTracedProcess(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
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

	// Wait for the execve-SIGTRAP that ptrace delivers after the child's
	// first exec when PTRACE_TRACEME is active.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("wait for execve stop: %w", err)
	}
	if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("unexpected initial stop: %v", ws)
	}

	// Enable follow-fork and exit notification so we track child threads and
	// get a stop just before the main thread calls exit_group.
	const opts = syscall.PTRACE_O_TRACECLONE |
		syscall.PTRACE_O_TRACEEXIT |
		syscall.PTRACE_O_TRACEEXEC
	if err := syscall.PtraceSetOptions(pid, opts); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("PTRACE_SETOPTIONS: %w", err)
	}

	return pid, cmd, nil
}

// attachToProcess attaches to a running process. The process is stopped on
// return and the engine can begin inspecting it.
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

// killProcess terminates the tracee and reaps it.
func killProcess(pid int, cmd *exec.Cmd) error {
	if cmd != nil {
		// Launched process: kill and wait to avoid a zombie.
		if err := cmd.Process.Kill(); err != nil && !isAlreadyExited(err) {
			return err
		}
		_ = cmd.Wait()
		return nil
	}
	// Attached process: detach first so the target can continue after us.
	_ = syscall.PtraceDetach(pid)
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil // already gone
	}
	return p.Kill()
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

// ── Backend implementation ────────────────────────────────────────────────────

func (b *linuxBackend) ContinueProcess() error {
	b.stepping = false
	return syscall.PtraceCont(b.pid, 0)
}

func (b *linuxBackend) SingleStep(tid int) error {
	b.stepping = true
	return syscall.PtraceSingleStep(tid)
}

func (b *linuxBackend) StopProcess() error {
	p, err := os.FindProcess(b.pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGSTOP)
}

func (b *linuxBackend) ReadMemory(addr uint64, dst []byte) error {
	n, err := syscall.PtracePeekData(b.pid, uintptr(addr), dst)
	if err != nil {
		return fmt.Errorf("PTRACE_PEEKDATA 0x%x: %w", addr, err)
	}
	if n != len(dst) {
		return fmt.Errorf("PTRACE_PEEKDATA 0x%x: short read %d/%d", addr, n, len(dst))
	}
	return nil
}

func (b *linuxBackend) WriteMemory(addr uint64, src []byte) error {
	n, err := syscall.PtracePokeData(b.pid, uintptr(addr), src)
	if err != nil {
		return fmt.Errorf("PTRACE_POKEDATA 0x%x: %w", addr, err)
	}
	if n != len(src) {
		return fmt.Errorf("PTRACE_POKEDATA 0x%x: short write %d/%d", addr, n, len(src))
	}
	return nil
}

func (b *linuxBackend) GetRegisters(tid int) (Registers, error) {
	return archGetRegisters(tid)
}

func (b *linuxBackend) SetRegisters(tid int, reg Registers) error {
	return archSetRegisters(tid, reg)
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

// Wait blocks until the tracee produces a meaningful debug stop.
//
// Single-step vs breakpoint disambiguation:
//
//	We track b.stepping. If b.stepping is true when a SIGTRAP arrives, it is
//	a single-step completion; otherwise it is a software breakpoint.
//	This is reliable because ptrace is serialised: after SingleStep returns, the
//	next event from Wait4 will be the step trap for that exact request.
//
// PTRACE_EVENT stops (clone, exec, exit) are handled internally: we resume the
// tracee with PtraceCont and loop back to Wait4, so these never surface to the
// engine.
func (b *linuxBackend) Wait() (StopEvent, error) {
	for {
		var ws syscall.WaitStatus
		// Wait for any child thread (WALL includes clone()d threads).
		tid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		if err != nil {
			return StopEvent{}, fmt.Errorf("wait4: %w", err)
		}

		if ws.Exited() {
			if tid == b.pid {
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: ws.ExitStatus()}, nil
			}
			// Non-main thread exit — ignore and keep waiting.
			continue
		}

		if ws.Signaled() {
			// The process was killed by an unhandled signal.
			return StopEvent{Reason: StopKilled, TID: tid}, nil
		}

		if !ws.Stopped() {
			// Continued or other synthetic status — keep waiting.
			continue
		}

		sig := ws.StopSignal()

		// PTRACE_EVENT stops are encoded as SIGTRAP | (event << 8).
		// TrapCause() returns the event code (0 for a plain SIGTRAP).
		if sig == syscall.SIGTRAP {
			cause := ws.TrapCause()

			switch cause {
			case syscall.PTRACE_EVENT_CLONE:
				// New thread created — resume it and continue waiting.
				newTID, _ := syscall.PtraceGetEventMsg(tid)
				_ = syscall.PtraceCont(int(newTID), 0)
				_ = syscall.PtraceCont(tid, 0)
				continue

			case syscall.PTRACE_EVENT_EXIT:
				// The main thread is about to call exit_group.
				// Deliver this as StopExited so the engine shuts down.
				_ = syscall.PtraceCont(tid, 0) // let the process actually exit
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: 0}, nil

			case syscall.PTRACE_EVENT_EXEC:
				// A new program was exec'd — resume.
				_ = syscall.PtraceCont(tid, 0)
				continue

			case 0:
				// Plain SIGTRAP: either a breakpoint or a single-step.
				regs, err := b.GetRegisters(tid)
				if err != nil {
					return StopEvent{}, err
				}

				if b.stepping {
					b.stepping = false
					return StopEvent{Reason: StopSingleStep, TID: tid, PC: regs.PC}, nil
				}

				// Software breakpoint: rewind PC to the trap address.
				return StopEvent{
					Reason: StopBreakpoint,
					TID:    tid,
					PC:     archRewindPC(regs.PC),
				}, nil

			default:
				// Unknown ptrace event — resume and keep waiting.
				_ = syscall.PtraceCont(tid, 0)
				continue
			}
		}

		// Non-SIGTRAP signal (SIGSEGV, SIGBUS, etc.) — deliver to engine.
		regs, err := b.GetRegisters(tid)
		if err != nil {
			return StopEvent{}, err
		}
		return StopEvent{
			Reason: StopSignal,
			TID:    tid,
			PC:     regs.PC,
			Signal: int(sig),
		}, nil
	}
}

var _ Backend = (*linuxBackend)(nil)

func (b *linuxBackend) setPID(pid int) { b.pid = pid }
