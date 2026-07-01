//go:build linux && amd64

package debugger

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

func newBackend() Backend {
	return &linuxBackend{pt: ptraceThread()}
}

type linuxBackend struct {
	pt          *ptraceExec
	pid         int
	stepping    bool // true after SingleStep; classifies the next SIGTRAP
	stepTID     int  // the exact thread SingleStep was issued against
	lastStopTID int
}

// ptraceExec serializes every ptrace(2) command onto one dedicated OS thread.
//
// Linux ties a tracee to the specific "tracer" thread that established the
// trace relationship — here the thread that forks the child under
// PTRACE_TRACEME. Every later ptrace request (PTRACE_CONT, PTRACE_SINGLESTEP,
// register/memory access, PTRACE_SETOPTIONS, PTRACE_DETACH) must be issued
// from that same thread; from any other thread the kernel rejects it with
// ESRCH. The engine's wait loop runs on a *different* OS thread than the one
// that forked the tracee, so ptrace commands re-issued from inside Wait()
// (the transparent SIGURG / SIGCONT / clone re-steps) used to fail with ESRCH
// — an error the helpers silently swallowed — leaving the tracee parked and
// wedging step-over forever. Funnelling every command through this single
// locked thread guarantees they all originate from the tracer.
//
// wait4(2) is deliberately NOT routed here: any thread in the tracer's thread
// group may reap the tracee, and blocking this thread inside wait4 would stop
// it from servicing the very commands the wait loop needs. This mirrors
// Delve's model (dedicated ptrace thread for commands; wait4 on regular
// goroutines — see pkg/proc/native/proc.go execPtraceFunc and
// proc_linux.go trapWait).
type ptraceExec struct {
	mu    sync.Mutex
	reqCh chan func()
	ackCh chan struct{}
}

var (
	ptraceOnce sync.Once
	ptraceInst *ptraceExec
)

// ptraceThread returns the process-wide ptrace command thread, creating it on
// first use. Exactly one debug target is traced at a time in this codebase, so
// a single shared tracer thread keeps the fork (in startTracedProcess) and all
// of the backend's subsequent commands on the same thread.
func ptraceThread() *ptraceExec {
	ptraceOnce.Do(func() { ptraceInst = newPtraceExec() })
	return ptraceInst
}

func newPtraceExec() *ptraceExec {
	p := &ptraceExec{
		reqCh: make(chan func()),
		ackCh: make(chan struct{}),
	}
	started := make(chan struct{})
	go func() {
		// Never unlocked: this thread must remain the tracer for the lifetime
		// of the process so every ptrace command originates from it.
		runtime.LockOSThread()
		close(started)
		for fn := range p.reqCh {
			fn()
			p.ackCh <- struct{}{}
		}
	}()
	<-started
	return p
}

// run executes fn on the ptrace thread and blocks until it completes. The
// mutex makes each request/ack pair atomic so concurrent callers can't steal
// one another's acknowledgement.
func (p *ptraceExec) run(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqCh <- fn
	<-p.ackCh
}

// PTRACE_O_TRACECLONE auto-traces every thread the tracee spawns, so a
// breakpoint hit on ANY OS thread is caught by us. Without it the Go
// scheduler can migrate the goroutine that owns a breakpoint onto an untraced
// runtime thread; when that thread executes the INT3 the SIGTRAP is delivered
// to the tracee's own runtime ("SIGTRAP: trace trap") and it dies. The clone
// bookkeeping (parent PTRACE_EVENT_CLONE + child SIGSTOP) is handled in Wait.
const linuxPtraceOptions = syscall.PTRACE_O_TRACECLONE |
	syscall.PTRACE_O_TRACEEXIT |
	syscall.PTRACE_O_TRACEEXEC

// startTracedProcess forks under ptrace. The child is stopped at its first
// instruction (execve SIGTRAP) ready for the engine to set breakpoints. The
// fork and initial setup run on the ptrace thread so that thread becomes the
// tracer for every subsequent command.
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

	var (
		pid int
		err error
	)
	ptraceThread().run(func() {
		if serr := cmd.Start(); serr != nil {
			err = fmt.Errorf("exec %q: %w", binaryPath, serr)
			return
		}
		pid = cmd.Process.Pid

		var ws syscall.WaitStatus
		if _, werr := syscall.Wait4(pid, &ws, 0, nil); werr != nil {
			_ = cmd.Process.Kill()
			err = fmt.Errorf("wait for execve stop: %w", werr)
			return
		}
		if !ws.Stopped() || ws.StopSignal() != syscall.SIGTRAP {
			_ = cmd.Process.Kill()
			err = fmt.Errorf("unexpected initial stop: %v", ws)
			return
		}
		if serr := syscall.PtraceSetOptions(pid, linuxPtraceOptions); serr != nil {
			_ = cmd.Process.Kill()
			err = fmt.Errorf("PTRACE_SETOPTIONS: %w", serr)
			return
		}
	})
	if err != nil {
		return 0, nil, err
	}
	return pid, cmd, nil
}

func attachToProcess(pid int) error {
	var err error
	ptraceThread().run(func() {
		if aerr := syscall.PtraceAttach(pid); aerr != nil {
			err = fmt.Errorf("PTRACE_ATTACH pid %d: %w", pid, aerr)
			return
		}
		var ws syscall.WaitStatus
		if _, werr := syscall.Wait4(pid, &ws, 0, nil); werr != nil {
			err = fmt.Errorf("wait after PTRACE_ATTACH: %w", werr)
		}
	})
	return err
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
	ptraceThread().run(func() { _ = syscall.PtraceDetach(pid) })
	return nil
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}

func (b *linuxBackend) ContinueProcess() error {
	b.stepping = false
	b.stepTID = 0
	tid := b.traceTID()
	var err error
	b.pt.run(func() { err = syscall.PtraceCont(tid, 0) })
	if err != nil {
		return fmt.Errorf("PTRACE_CONT tid %d: %w", tid, err)
	}
	return nil
}

func (b *linuxBackend) SingleStep(tid int) error {
	b.stepping = true
	b.stepTID = tid
	var err error
	b.pt.run(func() { err = syscall.PtraceSingleStep(tid) })
	if err != nil {
		return fmt.Errorf("PTRACE_SINGLESTEP tid %d: %w", tid, err)
	}
	return nil
}

func (b *linuxBackend) StopProcess() error {
	// SIGSTOP is delivered via kill(2), not ptrace, so it is safe from any
	// thread.
	p, err := os.FindProcess(b.pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGSTOP)
}

func (b *linuxBackend) ReadMemory(addr uint64, dst []byte) error {
	tid := b.traceTID()
	var (
		n   int
		err error
	)
	b.pt.run(func() { n, err = syscall.PtracePeekData(tid, uintptr(addr), dst) })
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
	var (
		n   int
		err error
	)
	b.pt.run(func() { n, err = syscall.PtracePokeData(tid, uintptr(addr), src) })
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
	var (
		r   syscall.PtraceRegs
		err error
	)
	b.pt.run(func() { err = syscall.PtraceGetRegs(tid, &r) })
	if err != nil {
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
// by reading the full register set first. Both the read and the write run on
// the ptrace thread as one unit.
func (b *linuxBackend) SetRegisters(tid int, reg Registers) error {
	var err error
	b.pt.run(func() {
		var r syscall.PtraceRegs
		if err = syscall.PtraceGetRegs(tid, &r); err != nil {
			err = fmt.Errorf("PTRACE_GETREGS (pre-set) tid %d: %w", tid, err)
			return
		}
		r.Rip = reg.PC
		r.Rsp = reg.SP
		r.Rbp = reg.BP
		r.Fs_base = reg.TLS
		if err = syscall.PtraceSetRegs(tid, &r); err != nil {
			err = fmt.Errorf("PTRACE_SETREGS tid %d: %w", tid, err)
		}
	})
	return err
}

func (b *linuxBackend) Threads() ([]int, error) {
	tids, err := taskTIDs(b.pid)
	if err != nil {
		return nil, fmt.Errorf("read /proc/%d/task: %w", b.pid, err)
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
// wait4 runs directly on the caller's (wait-loop) thread — that is legal from
// any thread in the tracer's group — but every ptrace command re-issued in
// response goes through b.pt so it executes on the tracer thread.
//
//nolint:gocognit,gocyclo // The wait loop is one serialized ptrace state machine.
func (b *linuxBackend) Wait() (StopEvent, error) {
	for {
		var ws syscall.WaitStatus
		// WALL includes clone()d threads.
		tid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
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
				if err := b.contIfExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT clone parent tid %d: %w", tid, err)
				}
				continue

			case syscall.PTRACE_EVENT_EXIT:
				if tid != b.pid {
					if err := b.contIfExists(tid, 0); err != nil {
						return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting thread tid %d: %w", tid, err)
					}
					continue
				}
				// Main thread is about to call exit_group — let it actually exit.
				if err := b.contIfExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exiting process tid %d: %w", tid, err)
				}
				b.recordStop(tid)
				return StopEvent{Reason: StopExited, TID: tid, ExitCode: 0}, nil

			case syscall.PTRACE_EVENT_EXEC:
				if err := b.contIfExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT exec tid %d: %w", tid, err)
				}
				continue

			case 0:
				b.recordStop(tid)

				// A single-step completes only on the exact thread we issued
				// the step against. Any other cause==0 SIGTRAP is a software
				// breakpoint hit — possibly on a different OS thread, since the
				// goroutine that owns the breakpoint can migrate between the Go
				// runtime's threads.
				if b.stepping && tid == b.stepTID {
					b.stepping = false
					b.stepTID = 0
					return StopEvent{Reason: StopSingleStep, TID: tid}, nil
				}

				return StopEvent{
					Reason: StopBreakpoint,
					TID:    tid,
				}, nil

			default:
				if err := b.contIfExists(tid, 0); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT trap cause %d tid %d: %w", cause, tid, err)
				}
				continue
			}
		}

		// A newly cloned thread reports an initial SIGSTOP signal-delivery-stop
		// (its parent separately reports PTRACE_EVENT_CLONE above). Propagate
		// our trace options to it — so its own future clones are traced too —
		// then release it with signal 0 (swallow the synthetic SIGSTOP). This
		// mirrors Delve's addThread; resuming the child independently (rather
		// than stopping the whole group) avoids the historical clone deadlock.
		if sig == syscall.SIGSTOP && tid != b.pid {
			if err := b.setOptionsIfExists(tid); err != nil {
				return StopEvent{}, fmt.Errorf("PTRACE_SETOPTIONS clone child tid %d: %w", tid, err)
			}
			if err := b.contIfExists(tid, 0); err != nil {
				return StopEvent{}, fmt.Errorf("PTRACE_CONT clone child tid %d: %w", tid, err)
			}
			continue
		}

		// SIGURG is Go's goroutine-preemption signal; keep it transparent.
		// While single-stepping we must re-issue on the SAME thread we stepped
		// (swallowing the signal); every other thread — including a preempted
		// sibling during a step-over — is simply continued with the signal
		// re-injected so the runtime scheduler keeps working.
		if sig == syscall.SIGURG {
			if b.stepping && tid == b.stepTID {
				if err := b.stepIfExists(tid); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_SINGLESTEP after SIGURG tid %d: %w", tid, err)
				}
			} else {
				if err := b.contIfExists(tid, int(sig)); err != nil {
					return StopEvent{}, fmt.Errorf("PTRACE_CONT SIGURG tid %d: %w", tid, err)
				}
			}
			continue
		}

		if sig == syscall.SIGCONT {
			if err := b.contIfExists(tid, 0); err != nil {
				return StopEvent{}, fmt.Errorf("PTRACE_CONT SIGCONT tid %d: %w", tid, err)
			}
			continue
		}

		b.recordStop(tid)
		return StopEvent{
			Reason: StopSignal,
			TID:    tid,
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
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrNotExist)
}

func isNoChildProcess(err error) bool {
	return errors.Is(err, syscall.ECHILD)
}

// contIfExists issues PTRACE_CONT on the ptrace thread, tolerating a vanished
// thread (ESRCH) — a clone child can exit before we act on its stop.
func (b *linuxBackend) contIfExists(tid int, signal int) error {
	if tid == 0 {
		return nil
	}
	var err error
	b.pt.run(func() { err = syscall.PtraceCont(tid, signal) })
	if err != nil && !isNoSuchProcess(err) {
		return err
	}
	return nil
}

// stepIfExists issues PTRACE_SINGLESTEP on the ptrace thread, tolerating a
// vanished thread.
func (b *linuxBackend) stepIfExists(tid int) error {
	if tid == 0 {
		return nil
	}
	var err error
	b.pt.run(func() { err = syscall.PtraceSingleStep(tid) })
	if err != nil && !isNoSuchProcess(err) {
		return err
	}
	return nil
}

// setOptionsIfExists issues PTRACE_SETOPTIONS on the ptrace thread.
func (b *linuxBackend) setOptionsIfExists(tid int) error {
	var err error
	b.pt.run(func() { err = syscall.PtraceSetOptions(tid, linuxPtraceOptions) })
	if err != nil && !isNoSuchProcess(err) {
		return err
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
