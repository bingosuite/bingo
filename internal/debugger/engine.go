package debugger

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"

	"github.com/bingosuite/bingo/pkg/protocol"
)

const (
	eventBufSize  = 64
	maxStackDepth = 64

	stepOverNextFile  = "<stepover-next>"
	stepOutReturnFile = "<stepout-return>"
)

type engineState uint8

const (
	stateNoProcess engineState = iota
	stateRunning
	stateSuspended
	stateExited
)

// engine implements Debugger. See AGENTS.md → engine concurrency model and
// shutdown sequence for the loop / waitLoop / dispatch invariants.

// bpResumeAction is what to do after stepping past a software breakpoint
// (restore bytes → single-step → reinstall trap → action).
type bpResumeAction uint8

const (
	bpResumeContinue   bpResumeAction = iota // ContinueProcess and keep running
	bpResumeStep                             // emit EventStepped (machine-instruction)
	bpResumeSourceStep                       // set temp BP at next source line, then continue
	bpResumeStepOut                          // set return-addr BP, then continue
)

type engine struct {
	backend Backend
	proc    process
	bps     *breakpointTable
	dw      *dwarfReader

	events chan protocol.Event
	cmdCh  chan engineCmd
	stopCh chan stopResult

	// done is closed by the loop on exit; waitLoop selects on it to abandon
	// pending sends to stopCh.
	done chan struct{}

	seq   uint64
	state engineState
	mu    sync.Mutex

	// Software-breakpoint step-over state. lastBP is the BP the process
	// stopped at; on next resume we restore bytes, single-step, reinstall
	// the trap, then perform bpResumeAction. steppingOverBP is non-nil
	// during the in-flight single-step.
	lastBP         *breakpointEntry
	lastBPTID      int // thread that hit lastBP (Mach port on Darwin)
	steppingOverBP *breakpointEntry

	// curTID is the thread the user is currently stopped on — the one that hit
	// the last breakpoint or completed the last step. Updated on every
	// user-visible suspend. Step primitives must target this thread, never
	// threads[0]: darwin's task_threads returns creation order, so threads[0]
	// is frequently an idle runtime M, and single-stepping the wrong thread
	// leaves the intended thread parked while a sibling runs, corrupting the
	// step-over state machine (see #92).
	curTID int

	bpResume  bpResumeAction
	bpRetAddr uint64 // bpResumeStepOut only

	// Source-line target remembered from the previous step-over. More
	// reliable than re-querying locationForPC, which can land on a DWARF
	// boundary with line==0. Zeroed on each sourceStepOver and on user-BP hits.
	stepOverFile string
	stepOverLine int

	// manualStopPending records that a Pause request has fired the backend's
	// interrupt signal (PauseSignal — SIGSTOP on linux, SIGUSR2 on darwin) at
	// the tracee and we are awaiting the resulting signal-delivery stop, which
	// should be turned into EventPaused rather than auto-resumed. It needs no
	// synchronization: both Pause()'s dispatched closure and handleStop run on
	// the single engine loop thread. See AGENTS.md → Pause.
	manualStopPending bool

	// log is the single sink for all engine logging. Never call the
	// package-level slog functions directly — they bypass the per-session
	// logger the hub/server configure, producing duplicate, uncorrelated
	// log lines. See AGENTS.md.
	log *slog.Logger
}

type engineCmd struct {
	fn  func() error
	err chan error
}

// threadStepper is implemented by backends (currently darwin/arm64) that can
// single-step one specific thread over a disarmed breakpoint while holding
// every other thread, then tear that critical section down. On such backends
// the per-process single-step primitive alone cannot guarantee the breakpoint
// thread (rather than some other thread) is the one that steps, so the engine
// prefers this path. Backends that don't implement it fall back to SingleStep.
type threadStepper interface {
	singleStepThread(tid int, addr uint64) error
	endThreadStep()
}

// stepThreadOverBP single-steps tid over a just-disarmed breakpoint at addr. On
// darwin it holds all other threads and steps tid specifically; elsewhere it
// falls back to a plain per-process single-step (ptrace stops are per-thread
// there).
func (e *engine) stepThreadOverBP(tid int, addr uint64) error {
	if ts, ok := e.backend.(threadStepper); ok {
		return ts.singleStepThread(tid, addr)
	}
	return e.backend.SingleStep(tid)
}

// endThreadStep releases the threads held for an atomic step-over. No-op on
// backends without a threadStepper. Safe to call when no step is in flight.
func (e *engine) endThreadStep() {
	if ts, ok := e.backend.(threadStepper); ok {
		ts.endThreadStep()
	}
}

// activeTID resolves the thread the user is currently stopped on. It prefers
// curTID (set on every user-visible suspend) and falls back to the first task
// thread only before any stop has been recorded. Callers that single-step or
// read registers must use this, not threads[0]: on darwin threads[0] is often
// an idle runtime M, not the goroutine under inspection (see curTID).
func (e *engine) activeTID() (int, error) {
	if e.curTID != 0 {
		return e.curTID, nil
	}
	threads, err := e.backend.Threads()
	if err != nil || len(threads) == 0 {
		return 0, fmt.Errorf("no current thread")
	}
	return threads[0], nil
}

type stopResult struct {
	evt StopEvent
	err error
}

func newEngine(b Backend, log *slog.Logger) *engine {
	if log == nil {
		log = slog.Default()
	}
	e := &engine{
		backend: b,
		bps:     newBreakpointTable(),
		events:  make(chan protocol.Event, eventBufSize),
		cmdCh:   make(chan engineCmd, 8),
		stopCh:  make(chan stopResult, 1),
		done:    make(chan struct{}),
		state:   stateNoProcess,
		log:     log,
	}
	go e.loop()
	return e
}

func (e *engine) Events() <-chan protocol.Event { return e.events }

func (e *engine) Launch(binaryPath string, args []string, env []string) error {
	return e.dispatch(func() error {
		if err := e.proc.launch(e.backend, binaryPath, args, env); err != nil {
			return err
		}
		setPID(e.backend, e.proc.pid)
		e.loadDWARF(binaryPath)
		// startTracedProcess already consumed the initial SIGTRAP. The process
		// is stopped — no waitLoop needed.
		e.setState(stateSuspended)
		e.emitStoppedAtCurrentPC()
		return nil
	})
}

func (e *engine) Attach(pid int, binaryPath string) error {
	return e.dispatch(func() error {
		if err := e.proc.attach(e.backend, pid); err != nil {
			return err
		}
		setPID(e.backend, pid)
		if binaryPath != "" {
			e.loadDWARF(binaryPath)
		}
		e.setState(stateSuspended)
		e.emitStoppedAtCurrentPC()
		return nil
	})
}

// Kill terminates the tracee. Safe to call multiple times.
func (e *engine) Kill() error {
	select {
	case <-e.done:
		return nil
	default:
	}
	return e.dispatch(func() error {
		if e.getState() == stateExited {
			return nil
		}
		// Release any threads held for an in-flight atomic step-over first, so
		// a detach (attached-process Kill) never leaves them Mach-suspended.
		e.endThreadStep()
		e.bps.clearAll(e.backend)
		if killErr := e.proc.kill(e.backend); killErr != nil {
			return killErr
		}
		e.setState(stateExited)
		// Inject a synthetic StopExited so the loop sees stateExited and exits.
		select {
		case e.stopCh <- stopResult{evt: StopEvent{Reason: StopExited}}:
		default:
		}
		return nil
	})
}

func (e *engine) SetBreakpoint(file string, line int) (protocol.Breakpoint, error) {
	var bp protocol.Breakpoint
	err := e.dispatch(func() error {
		if e.dw == nil {
			return fmt.Errorf("SetBreakpoint: no DWARF info — was a binary path provided to Launch/Attach?")
		}
		addr, err := e.dw.PCForFileLine(file, line)
		if err != nil {
			return err
		}
		entry, err := e.bps.set(e.backend, file, line, addr)
		if err != nil {
			return err
		}
		bp = entry.toProtocol()
		return nil
	})
	return bp, err
}

func (e *engine) ClearBreakpoint(id int) error {
	return e.dispatch(func() error {
		return e.bps.clear(e.backend, id)
	})
}

func (e *engine) Continue() error {
	return e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		if e.lastBP != nil {
			return e.resumeFromBreakpoint(bpResumeContinue, 0)
		}
		if err := e.backend.ContinueProcess(); err != nil {
			return err
		}
		e.setState(stateRunning)
		go e.waitLoop()
		return nil
	})
}

func (e *engine) StepOver() error {
	return e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		return e.stepOver()
	})
}

func (e *engine) StepInto() error {
	return e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		if e.lastBP != nil {
			return e.resumeFromBreakpoint(bpResumeStep, 0)
		}
		tid, err := e.activeTID()
		if err != nil {
			return fmt.Errorf("StepInto: %w", err)
		}
		regs, err := e.backend.GetRegisters(tid)
		if err != nil {
			return fmt.Errorf("StepInto: get registers: %w", err)
		}
		// Step exactly one instruction on the user thread. On darwin this holds
		// every other thread Mach-suspended and hardware-single-steps tid
		// specifically: only the stepped thread runs during the step window, so
		// the runtime's sysmon can't observe it and inject a preemption, and any
		// Mach breakpoint exception seen mid-step is unambiguously this thread's
		// (#92); elsewhere it degrades to a plain per-thread single-step.
		if err := e.stepThreadOverBP(tid, regs.PC); err != nil {
			return err
		}
		e.setState(stateRunning)
		go e.waitLoop()
		return nil
	})
}

func (e *engine) StepOut() error {
	return e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		return e.stepOut()
	})
}

// Pause asynchronously interrupts a running tracee. It is the only resume-side
// operation issued while the process is RUNNING rather than suspended: it fires
// the backend's interrupt signal (PauseSignal) at the tracee via
// StopProcess and records manualStopPending so the resulting signal-delivery
// stop is turned into EventPaused instead of being auto-resumed (see
// handleStop's StopSignal branch). The suspend is reported asynchronously, so
// this returns as soon as the interrupt is armed.
func (e *engine) Pause() error {
	return e.dispatch(func() error {
		if e.getState() != stateRunning {
			return ErrNotRunning
		}
		e.manualStopPending = true
		if err := e.backend.StopProcess(); err != nil {
			e.manualStopPending = false
			return fmt.Errorf("Pause: %w", err)
		}
		return nil
	})
}

func (e *engine) Locals(frameIndex int) ([]protocol.Variable, error) {
	var vars []protocol.Variable
	err := e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		if e.dw == nil {
			return fmt.Errorf("Locals: no DWARF info")
		}
		// Inspect the thread the user is stopped on (curTID via activeTID), not
		// threads[0]: on Darwin threads[0] is frequently an idle runtime M, so a
		// breakpoint that fires on another thread would otherwise report an
		// unrelated frame's locals. See the activeTID/collectFrames invariant.
		tid, err := e.activeTID()
		if err != nil {
			return fmt.Errorf("Locals: %w", err)
		}
		regs, err := e.backend.GetRegisters(tid)
		if err != nil {
			return fmt.Errorf("Locals: get registers: %w", err)
		}
		framePCs := e.walkStack(regs)
		if frameIndex < 0 || frameIndex >= len(framePCs) {
			return fmt.Errorf("Locals: frame index %d out of range (have %d frames)",
				frameIndex, len(framePCs))
		}
		framePC := framePCs[frameIndex]
		frameBase := regs.BP
		if frameIndex > 0 {
			bp := regs.BP
			for i := 0; i < frameIndex && bp != 0; i++ {
				var buf [8]byte
				if err := e.backend.ReadMemory(bp, buf[:]); err != nil {
					break
				}
				bp = binary.LittleEndian.Uint64(buf[:])
			}
			frameBase = bp
		}
		vars, err = e.dw.LocalsForFrame(e.backend, framePC, frameBase)
		return err
	})
	return vars, err
}

func (e *engine) StackFrames() ([]protocol.Frame, error) {
	var frames []protocol.Frame
	err := e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		var err error
		// Walk the currently-stopped thread. lastBPTID is only valid immediately
		// after a breakpoint hit and is cleared once we single-step off it, so it
		// goes stale after a step; curTID always tracks the active stop.
		frames, err = e.collectFrames(e.curTID)
		return err
	})
	return frames, err
}

func (e *engine) Goroutines() ([]protocol.Goroutine, error) {
	var goroutines []protocol.Goroutine
	err := e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		var err error
		goroutines, err = e.readGoroutines()
		return err
	})
	return goroutines, err
}

func (e *engine) loop() {
	// Pin to one OS thread. On Darwin the backend issues ptrace/Mach calls
	// directly from these dispatch closures, so they must stay on one thread.
	// On Linux the backend owns a dedicated tracer thread (see tracerThread)
	// and this lock is merely belt-and-braces.
	runtime.LockOSThread()
	defer func() {
		close(e.done)
		close(e.events)
		// Release the linux tracer thread now that no more ptrace ops can be
		// issued (the loop has exited). No-op on backends without one.
		if c, ok := e.backend.(interface{ closeTracer() }); ok {
			c.closeTracer()
		}
	}()

	for {
		select {
		case cmd := <-e.cmdCh:
			cmd.err <- cmd.fn()

		case result := <-e.stopCh:
			if result.err != nil {
				if errors.Is(result.err, ErrProcessExited) {
					e.emitProcessExited(0)
				} else {
					e.emitError(protocol.CmdNone, result.err)
				}
				e.drainCmds()
				return
			}
			// Kill may have already moved us to stateExited while a real
			// (non-exit) stop was buffered in stopCh — its synthetic StopExited
			// is dropped when the channel is full. Do NOT let that stale stop
			// reach handleStop: StopBreakpoint/StopSingleStep/StopSignal call
			// setState(stateSuspended) unconditionally, which would resurrect
			// the engine out of stateExited and wedge the loop (done/events
			// never close, hub never sees the exit). Tear down cleanly instead.
			if e.getState() == stateExited {
				e.drainCmds()
				return
			}
			e.handleStop(result.evt)
			if e.getState() == stateExited {
				e.drainCmds()
				return
			}
		}
	}
}

func (e *engine) waitLoop() {
	// Lock to an OS thread: wait4 has per-thread semantics on some platforms
	// and we don't want a thread carrying unrelated ptrace state.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	evt, err := e.backend.Wait()
	select {
	case e.stopCh <- stopResult{evt: evt, err: err}:
	case <-e.done:
	}
}

//nolint:gocognit,gocyclo // Stop handling is a single serialized debugger state machine.
func (e *engine) handleStop(stop StopEvent) {
	switch stop.Reason {
	case StopExited:
		if e.getState() == stateExited {
			return
		}
		e.setState(stateExited)
		e.emitProcessExited(stop.ExitCode)

	case StopKilled:
		if e.getState() == stateExited {
			return
		}
		e.setState(stateExited)
		e.emitProcessExited(-1)

	case StopBreakpoint:
		e.setState(stateSuspended)
		var err error
		stop, err = e.populateBreakpointStop(stop)
		if err != nil {
			e.emitError(protocol.CmdNone, err)
			return
		}
		bp := e.bps.atAddr(stop.PC)
		e.log.Debug("StopBreakpoint", "pc", fmt.Sprintf("0x%x", stop.PC),
			"found", bp != nil,
			"steppingOverBP", e.steppingOverBP != nil)
		if bp == nil {
			// Spurious SIGTRAP — a BRK we did not install (Go runtime
			// internal trap or libc assertion). On ARM64 PC points AT the
			// BRK; ContinueProcess with signal=0 leaves PC unchanged and
			// re-executes the trap forever. Advance PC past the 4-byte BRK.
			e.log.Warn("spurious SIGTRAP — advancing PC past BRK and resuming",
				"pc", fmt.Sprintf("0x%x", stop.PC))
			if regs, err := e.backend.GetRegisters(stop.TID); err == nil {
				regs.PC = stop.PC + uint64(len(archTrapInstruction()))
				_ = e.backend.SetRegisters(stop.TID, regs)
			}
			_ = e.backend.ContinueProcess()
			e.setState(stateRunning)
			go e.waitLoop()
			return
		}
		e.log.Debug("StopBreakpoint matched", "file", bp.file, "line", bp.line,
			"addr", fmt.Sprintf("0x%x", bp.addr))
		e.rewindToBreakpoint(stop)
		if bp.file == stepOverNextFile {
			_ = e.bps.clear(e.backend, bp.id)
			e.lastBP = nil
			e.emitStepped(stop)
			return
		}
		if bp.file == stepOutReturnFile {
			_ = e.bps.clear(e.backend, bp.id)
			e.lastBP = nil
			e.emitStepped(stop)
			return
		}
		e.lastBP = bp
		e.lastBPTID = stop.TID
		e.stepOverFile = ""
		e.stepOverLine = 0
		e.emitBreakpointHit(bp, stop)

	case StopSingleStep:
		var err error
		stop, err = e.populateStopPC(stop, false)
		if err != nil {
			// Rearm the in-flight step-over BP and release held threads before
			// surfacing the error, so the process is left in a clean state.
			if sob := e.steppingOverBP; sob != nil {
				e.steppingOverBP = nil
				_ = e.bps.reinstall(e.backend, sob)
			}
			e.endThreadStep()
			e.setState(stateSuspended)
			e.emitError(protocol.CmdNone, err)
			return
		}
		e.log.Debug("StopSingleStep", "pc", fmt.Sprintf("0x%x", stop.PC),
			"steppingOverBP", e.steppingOverBP != nil)
		if sob := e.steppingOverBP; sob != nil {
			e.steppingOverBP = nil
			if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
				// Reinstall failed. Suspend instead of resuming — running
				// without the trap would let the process loose.
				e.endThreadStep()
				e.log.Error("breakpoint reinstall failed — suspending to prevent runaway process",
					"addr", fmt.Sprintf("0x%x", sob.addr), "err", rerr)
				e.setState(stateSuspended)
				e.emitError(protocol.CmdNone, fmt.Errorf("reinstall breakpoint 0x%x: %w", sob.addr, rerr))
				return
			}
			// The trap byte is back in place; only now is it safe to release
			// the threads we held for the atomic step-over.
			e.endThreadStep()
			e.log.Debug("breakpoint reinstalled", "addr", fmt.Sprintf("0x%x", sob.addr))
			switch e.bpResume {
			case bpResumeContinue:
				_ = e.backend.ContinueProcess()
				e.setState(stateRunning)
				go e.waitLoop()
			case bpResumeStep:
				e.setState(stateSuspended)
				e.emitStepped(stop)
			case bpResumeSourceStep:
				// Use sob.file/sob.line (the BP's known location) rather than
				// a DWARF lookup from stop.PC: stop.PC is one instruction past
				// the BP and can land on a DWARF entry with line==0.
				if e.dw != nil && sob.file != "" && sob.line > 0 {
					if nextPC, nextLine, ok := e.dw.NextLinePC(sob.file, sob.line); ok {
						e.log.Debug("sourceStepOver: setting "+stepOverNextFile,
							"from", fmt.Sprintf("%s:%d", sob.file, sob.line),
							"nextPC", fmt.Sprintf("0x%x", nextPC), "nextLine", nextLine)
						entry, setErr := e.bps.set(e.backend, stepOverNextFile, 0, nextPC)
						if setErr == nil || errors.Is(setErr, errBreakpointExists) {
							e.stepOverFile = sob.file
							e.stepOverLine = nextLine
							if cerr := e.backend.ContinueProcess(); cerr == nil {
								e.setState(stateRunning)
								go e.waitLoop()
								return
							} else if entry != nil {
								_ = e.bps.clear(e.backend, entry.id)
								e.stepOverFile = ""
								e.stepOverLine = 0
							}
						} else {
							e.log.Warn("sourceStepOver: set "+stepOverNextFile+" failed",
								"addr", fmt.Sprintf("0x%x", nextPC), "err", setErr)
						}
					} else {
						e.log.Warn("sourceStepOver: NextLinePC found no next line",
							"file", sob.file, "line", sob.line)
					}
				}
				e.log.Debug("sourceStepOver fallback: emitting Stepped")
				e.setState(stateSuspended)
				e.emitStepped(stop)
			case bpResumeStepOut:
				_, setErr := e.bps.set(e.backend, stepOutReturnFile, 0, e.bpRetAddr)
				if setErr != nil && !errors.Is(setErr, errBreakpointExists) {
					e.emitError(protocol.CmdStepOut, fmt.Errorf("StepOut: set return breakpoint: %w", setErr))
					return
				}
				_ = e.backend.ContinueProcess()
				e.setState(stateRunning)
				go e.waitLoop()
			}
			return
		}
		e.endThreadStep()
		e.setState(stateSuspended)
		e.emitStepped(stop)

	case StopSignal:
		// Reinstall any in-flight step-over BP before resuming or suspending.
		if sob := e.steppingOverBP; sob != nil {
			e.steppingOverBP = nil
			if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
				e.endThreadStep()
				e.setState(stateSuspended)
				e.emitError(protocol.CmdNone, fmt.Errorf("reinstall breakpoint 0x%x after signal: %w", sob.addr, rerr))
				return
			}
			e.endThreadStep()
		}
		if stop.Signal == e.backend.PauseSignal() {
			if e.manualStopPending {
				// A Pause request's interrupt signal has arrived. Suspend and
				// report EventPaused instead of auto-resuming — this is the one
				// signal stop we deliberately turn into a suspending event.
				e.manualStopPending = false
				var err error
				if stop, err = e.populateStopPC(stop, false); err != nil {
					e.setState(stateSuspended)
					e.emitError(protocol.CmdNone, err)
					return
				}
				e.setState(stateSuspended)
				e.emitPaused(stop)
				return
			}
			// The interrupt signal with no pending Pause is a leftover: a Pause
			// raced a self-stop (breakpoint/step won and cleared
			// manualStopPending), leaving the signal queued. Suppress it
			// silently — surfacing it as output or EventPaused would be bogus.
			// Continue discards it (ContinueProcess resumes with signal 0).
			_ = e.backend.ContinueProcess()
			e.setState(stateRunning)
			go e.waitLoop()
			return
		}
		e.emitOutput("stderr", fmt.Sprintf("signal %d", stop.Signal))
		_ = e.backend.ContinueProcess()
		e.setState(stateRunning)
		go e.waitLoop()
	}
}

func (e *engine) populateStopPC(stop StopEvent, rewind bool) (StopEvent, error) {
	if stop.PC != 0 {
		return stop, nil
	}
	if stop.TID == 0 {
		threads, err := e.backend.Threads()
		if err != nil {
			return stop, fmt.Errorf("get stop thread: %w", err)
		}
		if len(threads) == 0 {
			return stop, fmt.Errorf("get stop thread: no threads")
		}
		stop.TID = threads[0]
	}
	regs, err := e.backend.GetRegisters(stop.TID)
	if err != nil {
		return stop, fmt.Errorf("get stop PC for tid %d: %w", stop.TID, err)
	}
	if rewind {
		stop.PC = archRewindPC(regs.PC)
	} else {
		stop.PC = regs.PC
	}
	return stop, nil
}

// rewindToBreakpoint writes the breakpoint address back into the tracee's live
// PC register. On amd64 the CPU advances RIP past the INT3 before delivering
// the trap, so after a software-breakpoint stop the register points one byte
// past the patched instruction even though stop.PC has already been rewound
// for table lookup. Every resume path (plain continue after a sentinel step
// breakpoint, or the restore→single-step→reinstall step-over dance) would then
// execute starting one byte into the original instruction, corrupting the
// tracee and letting it run away — which manifests as a hung Continue/StepOver.
// Writing the rewound PC back makes every resume start at the real
// instruction. It is a no-op where the register already matches (e.g. arm64,
// whose BRK leaves PC in place, and Darwin).
func (e *engine) rewindToBreakpoint(stop StopEvent) {
	if stop.TID == 0 {
		return
	}
	regs, err := e.backend.GetRegisters(stop.TID)
	if err != nil {
		e.log.Warn("rewindToBreakpoint: get registers failed",
			"tid", stop.TID, "err", err)
		return
	}
	if regs.PC == stop.PC {
		return
	}
	regs.PC = stop.PC
	if err := e.backend.SetRegisters(stop.TID, regs); err != nil {
		e.log.Warn("rewindToBreakpoint: set registers failed",
			"tid", stop.TID, "pc", fmt.Sprintf("0x%x", stop.PC), "err", err)
	}
}

func (e *engine) populateBreakpointStop(stop StopEvent) (StopEvent, error) {
	if stop.PC != 0 {
		return stop, nil
	}
	if stop.TID != 0 {
		return e.populateStopPC(stop, true)
	}

	threads, err := e.backend.Threads()
	if err != nil {
		return stop, fmt.Errorf("find breakpoint thread: %w", err)
	}
	if len(threads) == 0 {
		return stop, fmt.Errorf("find breakpoint thread: no threads")
	}

	trap := archTrapInstruction()
	var firstTrap *StopEvent
	var fallback *StopEvent
	var firstErr error
	for _, tid := range threads {
		regs, err := e.backend.GetRegisters(tid)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		candidate := StopEvent{
			Reason: stop.Reason,
			TID:    tid,
			PC:     archRewindPC(regs.PC),
		}
		if fallback == nil {
			cp := candidate
			fallback = &cp
		}

		if !e.instructionAt(candidate.PC, trap) {
			continue
		}
		cp := candidate
		if e.bps.atAddr(candidate.PC) != nil {
			return cp, nil
		}
		if firstTrap == nil {
			firstTrap = &cp
		}
	}

	if firstTrap != nil {
		return *firstTrap, nil
	}
	if fallback != nil {
		return *fallback, nil
	}
	return stop, fmt.Errorf("find breakpoint thread: read registers: %w", firstErr)
}

func (e *engine) instructionAt(addr uint64, want []byte) bool {
	buf := make([]byte, len(want))
	if err := e.backend.ReadMemory(addr, buf); err != nil {
		return false
	}
	for i := range want {
		if buf[i] != want[i] {
			return false
		}
	}
	return true
}

// drainCmds answers queued commands with ErrProcessExited so blocked dispatchers
// unblock immediately.
func (e *engine) drainCmds() {
	for {
		select {
		case cmd := <-e.cmdCh:
			cmd.err <- ErrProcessExited
		default:
			return
		}
	}
}

func (e *engine) stepOver() error {
	if e.lastBP != nil {
		return e.resumeFromBreakpoint(bpResumeSourceStep, 0)
	}
	return e.sourceStepOver()
}

// sourceStepOver sets a temp BP at the next source line and resumes. Falls
// back to a single machine-instruction step when DWARF can't resolve a target.
//
//nolint:gocognit // Source stepping fallback logic stays together to preserve state transitions.
func (e *engine) sourceStepOver() error {
	if e.dw != nil {
		// Prefer the remembered destination from the previous step-over over
		// re-querying locationForPC, which can land on a DWARF boundary.
		file := e.stepOverFile
		line := e.stepOverLine
		e.stepOverFile = ""
		e.stepOverLine = 0

		if file == "" || line == 0 {
			if tid, err := e.activeTID(); err == nil {
				if regs, err := e.backend.GetRegisters(tid); err == nil {
					loc := e.dw.locationForPC(regs.PC)
					file = loc.File
					line = loc.Line
				}
			}
		}

		if file != "" && line > 0 {
			if nextPC, nextLine, ok := e.dw.NextLinePC(file, line); ok {
				entry, setErr := e.bps.set(e.backend, stepOverNextFile, 0, nextPC)
				if setErr == nil || errors.Is(setErr, errBreakpointExists) {
					e.stepOverFile = file
					e.stepOverLine = nextLine
					if cerr := e.backend.ContinueProcess(); cerr != nil {
						if entry != nil {
							_ = e.bps.clear(e.backend, entry.id)
						}
						e.stepOverFile = ""
						e.stepOverLine = 0
						return cerr
					}
					e.setState(stateRunning)
					go e.waitLoop()
					return nil
				}
			}
		}
	}
	tid, err := e.activeTID()
	if err != nil {
		return fmt.Errorf("StepOver: %w", err)
	}
	regs, err := e.backend.GetRegisters(tid)
	if err != nil {
		return fmt.Errorf("StepOver: get registers: %w", err)
	}
	// No DWARF next-line target (e.g. stopped outside known source): fall back
	// to a single machine-instruction step of the user thread via the atomic
	// path, same rationale as StepInto (#92).
	if err := e.stepThreadOverBP(tid, regs.PC); err != nil {
		return err
	}
	e.setState(stateRunning)
	go e.waitLoop()
	return nil
}

func (e *engine) stepOut() error {
	tid, err := e.activeTID()
	if err != nil {
		return fmt.Errorf("StepOut: %w", err)
	}
	regs, err := e.backend.GetRegisters(tid)
	if err != nil {
		return fmt.Errorf("StepOut: get registers: %w", err)
	}
	// The return address lives at BP+8 — just above the caller's saved frame
	// pointer at BP — the same frame-pointer chain walkStack follows. Reading
	// *(SP) only yields the return address at a function's first instruction,
	// before the prologue moves SP below the pushed return address; StepOut is
	// normally invoked at a mid-function breakpoint, where *(SP) is a local slot.
	// That mismatch was the "null return address" StepOut failure.
	if regs.BP == 0 {
		return fmt.Errorf("StepOut: null frame pointer — at outermost frame?")
	}
	var retBuf [8]byte
	if err := e.backend.ReadMemory(regs.BP+8, retBuf[:]); err != nil {
		return fmt.Errorf("StepOut: read return address: %w", err)
	}
	retAddr := binary.LittleEndian.Uint64(retBuf[:])
	if retAddr == 0 {
		return fmt.Errorf("StepOut: null return address — at outermost frame?")
	}
	if e.lastBP != nil {
		return e.resumeFromBreakpoint(bpResumeStepOut, retAddr)
	}
	_, setErr := e.bps.set(e.backend, stepOutReturnFile, 0, retAddr)
	if setErr != nil && !errors.Is(setErr, errBreakpointExists) {
		return fmt.Errorf("StepOut: set return breakpoint: %w", setErr)
	}
	if err := e.backend.ContinueProcess(); err != nil {
		return fmt.Errorf("StepOut: continue: %w", err)
	}
	e.setState(stateRunning)
	go e.waitLoop()
	return nil
}

// resumeFromBreakpoint runs the step-over-software-BP sequence:
// restore bytes → single-step → reinstall trap (in StopSingleStep handler)
// → perform action.
func (e *engine) resumeFromBreakpoint(action bpResumeAction, retAddr uint64) error {
	bp := e.lastBP
	e.lastBP = nil
	e.steppingOverBP = bp
	e.bpResume = action
	e.bpRetAddr = retAddr

	e.bps.removeFromTable(bp)
	if err := e.backend.WriteMemory(bp.addr, bp.originalBytes); err != nil {
		e.bps.addToTable(bp)
		e.steppingOverBP = nil
		return fmt.Errorf("resume BP: restore bytes: %w", err)
	}

	// Use the TID that hit the breakpoint. On Darwin task_threads returns
	// threads in creation order, so threads[0] is often an idle Go runtime M.
	tid := e.lastBPTID
	if tid == 0 {
		threads, err := e.backend.Threads()
		if err != nil || len(threads) == 0 {
			_ = e.backend.WriteMemory(bp.addr, archTrapInstruction())
			e.bps.addToTable(bp)
			e.steppingOverBP = nil
			return fmt.Errorf("resume BP: no threads")
		}
		tid = threads[0]
	}
	e.lastBPTID = 0
	if err := e.stepThreadOverBP(tid, bp.addr); err != nil {
		_ = e.backend.WriteMemory(bp.addr, archTrapInstruction())
		e.bps.addToTable(bp)
		e.steppingOverBP = nil
		return fmt.Errorf("resume BP: single step: %w", err)
	}
	e.setState(stateRunning)
	go e.waitLoop()
	return nil
}

// collectFrames walks the stack of the thread identified by tid (the thread
// that actually stopped) and resolves each PC to a source frame. When tid is 0
// it falls back to the first task thread. Walking the correct thread matters on
// Darwin: threads[0] is frequently an idle runtime M parked in libsystem, whose
// frame-pointer chain does not follow the Go ABI and can wander the full
// maxStackDepth, turning frame resolution into dozens of costly DWARF lookups.
func (e *engine) collectFrames(tid int) ([]protocol.Frame, error) {
	if e.dw == nil {
		return nil, nil
	}
	if tid == 0 {
		threads, err := e.backend.Threads()
		if err != nil || len(threads) == 0 {
			return nil, fmt.Errorf("StackFrames: no threads")
		}
		tid = threads[0]
	}
	regs, err := e.backend.GetRegisters(tid)
	if err != nil {
		return nil, fmt.Errorf("StackFrames: %w", err)
	}
	return e.dw.FramesForStack(e.walkStack(regs)), nil
}

func (e *engine) walkStack(regs Registers) []uint64 {
	pcs := []uint64{regs.PC}
	bp := regs.BP
	for i := 0; i < maxStackDepth && bp != 0; i++ {
		var frame [16]byte
		if err := e.backend.ReadMemory(bp, frame[:]); err != nil {
			break
		}
		retAddr := binary.LittleEndian.Uint64(frame[8:])
		if retAddr == 0 {
			break
		}
		pcs = append(pcs, retAddr)
		bp = binary.LittleEndian.Uint64(frame[:8])
	}
	return pcs
}

func (e *engine) readGoroutines() ([]protocol.Goroutine, error) {
	// Report the stopped thread's location (curTID via activeTID); threads[0] may
	// be an idle runtime M and would misreport where execution is paused.
	tid, err := e.activeTID()
	if err != nil {
		return nil, nil
	}
	regs, err := e.backend.GetRegisters(tid)
	if err != nil {
		return nil, fmt.Errorf("Goroutines: %w", err)
	}
	loc := protocol.Location{}
	if e.dw != nil {
		loc = e.dw.locationForPC(regs.PC)
	}
	return []protocol.Goroutine{{
		ID:         1,
		Status:     "waiting",
		CurrentLoc: loc,
	}}, nil
}

func (e *engine) loadDWARF(binaryPath string) {
	dr, err := openDWARF(binaryPath)
	if err != nil {
		e.dw = nil
		return
	}
	// On platforms that support ASLR (Darwin ARM64), ask the backend for the
	// slide so DWARF addresses match the actual load address.
	if sg, ok := e.backend.(interface{ TextSlide(string) int64 }); ok {
		dr.slide = sg.TextSlide(binaryPath)
	}
	e.dw = dr
}

func (e *engine) nextSeq() uint64 {
	e.mu.Lock()
	e.seq++
	s := e.seq
	e.mu.Unlock()
	return s
}

func (e *engine) emit(kind protocol.EventKind, payload any) {
	evt, err := protocol.NewEvent(kind, e.nextSeq(), payload)
	if err != nil {
		slog.Error("engine.emit: marshal event failed", "kind", kind, "err", err)
		return
	}
	// Non-blocking on purpose: this runs on the serialized loop, so blocking
	// while a reader is gone would deadlock the loop against its own teardown.
	// The buffer is sized so the continuously-draining hub never fills it, and
	// the exit path is backstopped by the events channel closing on loop return.
	select {
	case e.events <- evt:
	default:
		slog.Warn("engine.emit: events buffer full — dropping", "kind", kind)
	}
}

func (e *engine) emitBreakpointHit(bp *breakpointEntry, stop StopEvent) {
	if stop.TID != 0 {
		e.curTID = stop.TID
	}
	// Suspending for a self-stop cancels any pending Pause: a Pause interrupt
	// signal that raced this stop and lost is now leftover in the kernel queue,
	// to be suppressed (not reported as Paused) when it surfaces on the next
	// resume.
	e.manualStopPending = false
	frames, _ := e.collectFrames(stop.TID)
	goroutines, _ := e.readGoroutines()
	var g protocol.Goroutine
	if len(goroutines) > 0 {
		g = goroutines[0]
	}
	e.emit(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{
		Breakpoint: bp.toProtocol(),
		Goroutine:  g,
		Frames:     frames,
	})
}

// emitStoppedAtCurrentPC emits EventStepped at the current PC (used after
// Launch/Attach). Always emits even on register-read failure: the hub needs
// a suspending event or it loses track of state and drops resume commands.
func (e *engine) emitStoppedAtCurrentPC() {
	stop := StopEvent{}
	threads, err := e.backend.Threads()
	if err == nil && len(threads) > 0 {
		if regs, err := e.backend.GetRegisters(threads[0]); err == nil {
			stop.PC = regs.PC
		}
	}
	e.emitStepped(stop)
}

func (e *engine) emitStepped(stop StopEvent) {
	if stop.TID != 0 {
		e.curTID = stop.TID
	}
	// Completing a step suspends for a self-stop, which cancels any pending
	// Pause the same way a breakpoint hit does (see emitBreakpointHit).
	e.manualStopPending = false
	frames, _ := e.collectFrames(stop.TID)
	goroutines, _ := e.readGoroutines()
	var g protocol.Goroutine
	if len(goroutines) > 0 {
		g = goroutines[0]
	}
	loc := protocol.Location{}
	if e.dw != nil {
		loc = e.dw.locationForPC(stop.PC)
	}
	e.emit(protocol.EventStepped, protocol.SteppedPayload{
		Goroutine: g,
		Location:  loc,
		Frames:    frames,
	})
}

// emitPaused reports an asynchronous Pause halt. It mirrors emitStepped but
// carries EventPaused/PausedPayload: the location is wherever execution was
// interrupted, not a source-line boundary.
func (e *engine) emitPaused(stop StopEvent) {
	if stop.TID != 0 {
		e.curTID = stop.TID
	}
	frames, _ := e.collectFrames(stop.TID)
	goroutines, _ := e.readGoroutines()
	var g protocol.Goroutine
	if len(goroutines) > 0 {
		g = goroutines[0]
	}
	loc := protocol.Location{}
	if e.dw != nil {
		loc = e.dw.locationForPC(stop.PC)
	}
	e.emit(protocol.EventPaused, protocol.PausedPayload{
		Goroutine: g,
		Location:  loc,
		Frames:    frames,
	})
}

func (e *engine) emitProcessExited(code int) {
	e.emit(protocol.EventProcessExited, protocol.ProcessExitedPayload{ExitCode: code})
}

func (e *engine) emitOutput(stream, content string) {
	e.emit(protocol.EventOutput, protocol.OutputPayload{Stream: stream, Content: content})
}

func (e *engine) emitError(cmd protocol.CommandKind, err error) {
	e.emit(protocol.EventError, protocol.ErrorPayload{Command: cmd, Message: err.Error()})
}

func (e *engine) setState(s engineState) {
	e.mu.Lock()
	e.state = s
	e.mu.Unlock()
}

func (e *engine) getState() engineState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

func (e *engine) requireSuspended() error {
	if e.getState() != stateSuspended {
		return ErrNotSuspended
	}
	return nil
}

// dispatch sends fn to the loop and waits for its result. Returns
// ErrProcessExited if the loop has already exited.
func (e *engine) dispatch(fn func() error) error {
	ch := make(chan error, 1)
	select {
	case e.cmdCh <- engineCmd{fn: fn, err: ch}:
	case <-e.done:
		return ErrProcessExited
	}
	select {
	case err := <-ch:
		return err
	default:
	}
	select {
	case err := <-ch:
		return err
	case <-e.done:
		select {
		case err := <-ch:
			return err
		default:
			return ErrProcessExited
		}
	}
}
