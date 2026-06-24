//go:build linux && amd64

package debugger

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func newBackend() Backend {
	return &linuxBackend{}
}

type linuxBackend struct {
	pid         int
	stepping    bool // true after SingleStep; classifies the next SIGTRAP
	lastStopTID int
}

// startTracedProcess forks under ptrace. The child is stopped at its first
// instruction (execve SIGTRAP) ready for the engine to set breakpoints.
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

	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("wait for execve stop: %w", err)
	}
	if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
		_ = cmd.Process.Kill()
		return 0, nil, fmt.Errorf("unexpected initial stop: %v", ws)
	}

	// Track child threads (TRACECLONE) and get a stop just before the main
	// thread calls exit_group (TRACEEXIT).
	const opts = syscall.PTRACE_O_TRACECLONE |
		syscall.PTRACE_O_TRACEEXIT |
		syscall.PTRACE_O_TRACEEXEC
	if err := syscall.PtraceSetOptions(pid, opts); err != nil {
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
	tid := b.traceTID()
	if err := syscall.PtraceCont(tid, 0); err != nil {
		return fmt.Errorf("PTRACE_CONT tid %d: %w", tid, err)
	}
	return nil
}

func (b *linuxBackend) SingleStep(tid int) error {
	b.stepping = true
	if err := syscall.PtraceSingleStep(tid); err != nil {
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
		if err != nil {
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
				if err := syscall.PtraceCont(tid, 0); err != nil {
					if isNoSuchProcess(err) {
						continue
					}
					return StopEvent{}, fmt.Errorf("PTRACE_CONT clone parent tid %d: %w", tid, err)
				}
				continue

			case syscall.PTRACE_EVENT_EXIT:
				if tid != b.pid {
					if err := syscall.PtraceCont(tid, 0); err != nil {
						if isNoSuchProcess(err) {
							continue
						}
						return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting thread tid %d: %w", tid, err)
					}
					continue
				}
				// Main thread is about to call exit_group — let it actually exit.
				if err := syscall.PtraceCont(tid, 0); err != nil {
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
				regs, err := b.GetRegisters(tid)
				if err != nil {
					return StopEvent{}, err
				}
				b.recordStop(tid)

				if b.stepping {
					b.stepping = false
					return StopEvent{Reason: StopSingleStep, TID: tid, PC: regs.PC}, nil
				}

				return StopEvent{
					Reason: StopBreakpoint,
					TID:    tid,
					PC:     archRewindPC(regs.PC),
				}, nil

			default:
				if err := syscall.PtraceCont(tid, 0); err != nil {
					if isNoSuchProcess(err) {
						continue
					}
					return StopEvent{}, fmt.Errorf("PTRACE_CONT trap cause %d tid %d: %w", cause, tid, err)
				}
				continue
			}
		}

		if sig == syscall.SIGSTOP && tid != b.pid {
			if err := syscall.PtraceCont(tid, 0); err != nil {
				if isNoSuchProcess(err) {
					continue
				}
				return StopEvent{}, fmt.Errorf("PTRACE_CONT clone child tid %d: %w", tid, err)
			}
			continue
		}

		// SIGURG is Go's goroutine-preemption signal — must be re-delivered
		// transparently or scheduling breaks.
		if sig == syscall.SIGURG {
			if b.stepping {
				if err := syscall.PtraceSingleStep(tid); err != nil {
					if isNoSuchProcess(err) {
						continue
					}
					return StopEvent{}, fmt.Errorf("PTRACE_SINGLESTEP after SIGURG tid %d: %w", tid, err)
				}
			} else {
				if err := syscall.PtraceCont(tid, int(sig)); err != nil {
					if isNoSuchProcess(err) {
						continue
					}
					return StopEvent{}, fmt.Errorf("PTRACE_CONT SIGURG tid %d: %w", tid, err)
				}
			}
			continue
		}

		regs, err := b.GetRegisters(tid)
		if err != nil {
			return StopEvent{}, err
		}
		b.recordStop(tid)
		return StopEvent{
			Reason: StopSignal,
			TID:    tid,
			PC:     regs.PC,
			Signal: int(sig),
		}, nil
	}
}

var _ Backend = (*linuxBackend)(nil)

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
	return errors.Is(err, syscall.ESRCH)
}
