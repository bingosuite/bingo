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
)

type engineState uint8

const (
	stateNoProcess engineState = iota
	stateRunning
	stateSuspended
	stateExited
)

// engine implements Debugger.
//
// Concurrency model
// -----------------
// All state mutations happen inside the event loop goroutine (e.loop).
// Public methods send a closure to cmdCh and block until it executes.
//
// Wait() runs in a dedicated goroutine (waitLoop). Each invocation calls
// Backend.Wait() once and sends the result to stopCh. A new waitLoop is
// started each time the process is resumed.
//
// Shutdown sequence
// -----------------
// When the process exits (StopExited/StopKilled/ErrProcessExited), the loop
// sets stateExited, drains any pending commands with ErrProcessExited so
// their callers unblock, then closes stopCh to signal any in-flight waitLoop
// goroutine that nobody will read its result, and finally returns — which
// closes the events channel via defer.
//
// Kill() dispatches a synthetic StopExited result into stopCh, which causes
// the loop to set stateExited, call drainCmds, close done, and return.
// bpResumeAction describes what to do after we finish stepping over a software
// breakpoint (restore original bytes → single-step → reinstall trap).
type bpResumeAction uint8

const (
	bpResumeContinue    bpResumeAction = iota // call ContinueProcess and keep running
	bpResumeStep                              // stay suspended and emit EventStepped (machine-instruction level)
	bpResumeSourceStep                        // set temp BP at next source line then ContinueProcess
	bpResumeStepOut                           // set return-addr BP then ContinueProcess
)

type engine struct {
	backend Backend
	proc    process
	bps     *breakpointTable
	dw      *dwarfReader

	events chan protocol.Event
	cmdCh  chan engineCmd
	stopCh chan stopResult

	// done is closed by the loop when it exits. waitLoop goroutines select
	// on done so they can abandon the send to stopCh and exit cleanly.
	done chan struct{}

	seq   uint64
	state engineState
	mu    sync.Mutex

	// Software-breakpoint step-over state.
	//
	// When the process stops at a user breakpoint, lastBP is set to that
	// entry. On the next resume command (Continue/StepOver/StepInto/StepOut)
	// we restore the original bytes, single-step past the instruction,
	// reinstall the trap, and then perform bpResumeAction.
	// steppingOverBP is non-nil while that single-step is in flight.
	lastBP         *breakpointEntry
	lastBPTID      int // TID (Mach thread port on Darwin) of the thread that hit lastBP
	steppingOverBP *breakpointEntry
	bpResume       bpResumeAction
	bpRetAddr      uint64 // return address; only used for bpResumeStepOut

	// stepOverFile and stepOverLine record the source line that the most
	// recent <stepover-next> breakpoint was aimed at. sourceStepOver reads
	// these instead of re-querying DWARF via locationForPC, which can be
	// unreliable when PC lands exactly at a DWARF line-entry boundary.
	// Consumed (zeroed) on each sourceStepOver call and on user-BP hits.
	stepOverFile string
	stepOverLine int
}

type engineCmd struct {
	fn  func() error
	err chan error
}

type stopResult struct {
	evt StopEvent
	err error
}

func newEngine(b Backend) *engine {
	e := &engine{
		backend: b,
		bps:     newBreakpointTable(),
		events:  make(chan protocol.Event, eventBufSize),
		cmdCh:   make(chan engineCmd, 8),
		stopCh:  make(chan stopResult, 1),
		done:    make(chan struct{}),
		state:   stateNoProcess,
	}
	go e.loop()
	return e
}

// ── Debugger interface ────────────────────────────────────────────────────────

func (e *engine) Events() <-chan protocol.Event { return e.events }

func (e *engine) Launch(binaryPath string, args []string, env []string) error {
	return e.dispatch(func() error {
		if err := e.proc.launch(binaryPath, args, env); err != nil {
			return err
		}
		setPID(e.backend, e.proc.pid)
		e.loadDWARF(binaryPath)
		// startTracedProcess already consumed the initial SIGTRAP — the
		// process is stopped at its first instruction. Set suspended (not
		// running) and emit a stopped event. No waitLoop: the process is
		// not running, so there is nothing for Wait4 to report.
		e.setState(stateSuspended)
		e.emitStoppedAtCurrentPC()
		return nil
	})
}

func (e *engine) Attach(pid int, binaryPath string) error {
	return e.dispatch(func() error {
		if err := e.proc.attach(pid); err != nil {
			return err
		}
		setPID(e.backend, pid)
		if binaryPath != "" {
			e.loadDWARF(binaryPath)
		}
		// attachToProcess already consumed the post-attach stop — the
		// process is stopped. No waitLoop needed (same rationale as Launch).
		e.setState(stateSuspended)
		e.emitStoppedAtCurrentPC()
		return nil
	})
}

// Kill terminates the tracee. Safe to call multiple times.
func (e *engine) Kill() error {
	// Fast path: if the loop has already exited, nothing to do.
	select {
	case <-e.done:
		return nil
	default:
	}
	return e.dispatch(func() error {
		if e.getState() == stateExited {
			return nil
		}
		e.bps.clearAll(e.backend)
		if killErr := e.proc.kill(e.backend); killErr != nil {
			return killErr
		}
		e.setState(stateExited)
		// Inject a synthetic StopExited into stopCh so the loop's next
		// iteration sees stateExited and exits cleanly.
		select {
		case e.stopCh <- stopResult{evt: StopEvent{Reason: StopExited}}:
		default:
			// stopCh already has a result; the loop will process it and
			// see stateExited regardless.
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
		threads, err := e.backend.Threads()
		if err != nil || len(threads) == 0 {
			return fmt.Errorf("StepInto: no threads")
		}
		if err := e.backend.SingleStep(threads[0]); err != nil {
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

func (e *engine) Locals(frameIndex int) ([]protocol.Variable, error) {
	var vars []protocol.Variable
	err := e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		if e.dw == nil {
			return fmt.Errorf("Locals: no DWARF info")
		}
		threads, err := e.backend.Threads()
		if err != nil || len(threads) == 0 {
			return fmt.Errorf("Locals: no threads")
		}
		regs, err := e.backend.GetRegisters(threads[0])
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
		frames, err = e.collectFrames()
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

// ── Event loop ────────────────────────────────────────────────────────────────

func (e *engine) loop() {
	// All ptrace calls (Continue, SingleStep, ReadMemory, WriteMemory, …) are
	// made from dispatch closures that execute on this goroutine. Pin it to one
	// OS thread so the kernel always sees the same tracer thread — required on
	// Linux (ptrace is thread-specific) and avoids scheduler surprises on macOS.
	runtime.LockOSThread()
	defer func() {
		close(e.done)   // signal waitLoop goroutines to abandon pending sends
		close(e.events) // signal hub that no more events are coming
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
			e.handleStop(result.evt)
			if e.getState() == stateExited {
				e.drainCmds()
				return
			}
		}
	}
}

// waitLoop calls Backend.Wait() once and sends the result to stopCh.
// It selects on e.done so that if the engine loop has already exited
// (e.g. Kill was called), the goroutine exits cleanly without blocking.
func (e *engine) waitLoop() {
	// Lock to an OS thread for the duration of the wait4 call. On some
	// platforms wait4 has per-thread semantics; locking also ensures the
	// goroutine is not rescheduled onto a thread that has unrelated ptrace
	// state while the syscall is in progress.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	evt, err := e.backend.Wait()
	select {
	case e.stopCh <- stopResult{evt: evt, err: err}:
	case <-e.done:
		// Engine loop has exited — discard the result and exit cleanly.
	}
}

func (e *engine) handleStop(stop StopEvent) {
	switch stop.Reason {
	case StopExited:
		if e.getState() == stateExited {
			return // already exited (e.g. from Kill's synthetic stop)
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
		bp := e.bps.atAddr(stop.PC)
		slog.Debug("StopBreakpoint", "pc", fmt.Sprintf("0x%x", stop.PC),
			"found", bp != nil,
			"steppingOverBP", e.steppingOverBP != nil)
		if bp == nil {
			// Spurious SIGTRAP — a BRK we did not install (e.g. Go runtime
			// internal breakpoint or system library assertion).
			// On ARM64, BRK traps with PC pointing AT the BRK instruction.
			// Calling ContinueProcess with signal=0 leaves PC unchanged and the
			// CPU re-executes the BRK immediately — infinite trap loop.
			// Advance PC past the 4-byte BRK before resuming.
			slog.Warn("spurious SIGTRAP — advancing PC past BRK and resuming",
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
		slog.Debug("StopBreakpoint matched", "file", bp.file, "line", bp.line,
			"addr", fmt.Sprintf("0x%x", bp.addr))
		if bp.file == "<stepover-next>" {
			_ = e.bps.clear(e.backend, bp.id)
			e.lastBP = nil
			e.emitStepped(stop)
			return
		}
		if bp.file == "<stepout-return>" {
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
		slog.Debug("StopSingleStep", "pc", fmt.Sprintf("0x%x", stop.PC),
			"steppingOverBP", e.steppingOverBP != nil)
		if sob := e.steppingOverBP; sob != nil {
			// Finished stepping over the breakpoint instruction — reinstall it.
			e.steppingOverBP = nil
			if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
				// BP could not be reinstalled (WriteMemory failed). Do NOT resume —
				// that would run the process indefinitely with no trap to stop it.
				// Suspend and report the error so the client knows what happened.
				slog.Error("breakpoint reinstall failed — suspending to prevent runaway process",
					"addr", fmt.Sprintf("0x%x", sob.addr), "err", rerr)
				e.setState(stateSuspended)
				e.emitError(protocol.CmdNone, fmt.Errorf("reinstall breakpoint 0x%x: %w", sob.addr, rerr))
				return
			}
			slog.Debug("breakpoint reinstalled", "addr", fmt.Sprintf("0x%x", sob.addr))
			switch e.bpResume {
			case bpResumeContinue:
				_ = e.backend.ContinueProcess()
				e.setState(stateRunning)
				go e.waitLoop()
			case bpResumeStep:
				e.setState(stateSuspended)
				e.emitStepped(stop)
			case bpResumeSourceStep:
				// BP step-over complete. Set a temp BP at the next source line and continue.
				// Use sob.file/sob.line (the BP's known location) rather than a DWARF
				// lookup from stop.PC: stop.PC is one instruction past the BP and may
				// land on a DWARF entry with line == 0, causing NextLinePC to pick the
				// wrong address.
				if e.dw != nil && sob.file != "" && sob.line > 0 {
					if nextPC, nextLine, ok := e.dw.NextLinePC(sob.file, sob.line); ok {
						slog.Debug("sourceStepOver: setting <stepover-next>",
							"from", fmt.Sprintf("%s:%d", sob.file, sob.line),
							"nextPC", fmt.Sprintf("0x%x", nextPC), "nextLine", nextLine)
						entry, setErr := e.bps.set(e.backend, "<stepover-next>", 0, nextPC)
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
							slog.Warn("sourceStepOver: set <stepover-next> failed",
								"addr", fmt.Sprintf("0x%x", nextPC), "err", setErr)
						}
					} else {
						slog.Warn("sourceStepOver: NextLinePC found no next line",
							"file", sob.file, "line", sob.line)
					}
				}
				// Fallback: emit Stepped at current position.
				slog.Debug("sourceStepOver fallback: emitting Stepped")
				e.setState(stateSuspended)
				e.emitStepped(stop)
			case bpResumeStepOut:
				_, setErr := e.bps.set(e.backend, "<stepout-return>", 0, e.bpRetAddr)
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
		e.setState(stateSuspended)
		e.emitStepped(stop)

	case StopSignal:
		// If a step-over was in progress, its breakpoint has been removed from
		// the table. Reinstall it before resuming so we don't lose the BP.
		if sob := e.steppingOverBP; sob != nil {
			e.steppingOverBP = nil
			if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
				e.setState(stateSuspended)
				e.emitError(protocol.CmdNone, fmt.Errorf("reinstall breakpoint 0x%x after signal: %w", sob.addr, rerr))
				return
			}
		}
		e.emitOutput("stderr", fmt.Sprintf("signal %d", stop.Signal))
		_ = e.backend.ContinueProcess()
		e.setState(stateRunning)
		go e.waitLoop()
	}
}

// drainCmds answers any queued commands with ErrProcessExited so that
// any goroutine blocked in dispatch() unblocks immediately.
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

// ── Stepping ──────────────────────────────────────────────────────────────────

func (e *engine) stepOver() error {
	if e.lastBP != nil {
		return e.resumeFromBreakpoint(bpResumeSourceStep, 0)
	}
	return e.sourceStepOver()
}

// sourceStepOver sets a temporary breakpoint at the first instruction of the
// next source line and resumes. Falls back to a machine-instruction single-step
// if the next line cannot be determined from DWARF.
func (e *engine) sourceStepOver() error {
	if e.dw != nil {
		// Prefer the remembered destination from the previous step-over; it is
		// more reliable than re-querying locationForPC at the current PC, which
		// may land on a DWARF boundary and return the wrong line.
		file := e.stepOverFile
		line := e.stepOverLine
		e.stepOverFile = ""
		e.stepOverLine = 0

		if file == "" || line == 0 {
			// No remembered target — look up from current PC.
			threads, err := e.backend.Threads()
			if err == nil && len(threads) > 0 {
				if regs, err := e.backend.GetRegisters(threads[0]); err == nil {
					loc := e.dw.locationForPC(regs.PC)
					file = loc.File
					line = loc.Line
				}
			}
		}

		if file != "" && line > 0 {
			if nextPC, nextLine, ok := e.dw.NextLinePC(file, line); ok {
				entry, setErr := e.bps.set(e.backend, "<stepover-next>", 0, nextPC)
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
	// Fallback: machine-instruction single-step.
	threads, err := e.backend.Threads()
	if err != nil || len(threads) == 0 {
		return fmt.Errorf("StepOver: no threads")
	}
	if err := e.backend.SingleStep(threads[0]); err != nil {
		return err
	}
	e.setState(stateRunning)
	go e.waitLoop()
	return nil
}

func (e *engine) stepOut() error {
	threads, err := e.backend.Threads()
	if err != nil || len(threads) == 0 {
		return fmt.Errorf("StepOut: no threads")
	}
	regs, err := e.backend.GetRegisters(threads[0])
	if err != nil {
		return fmt.Errorf("StepOut: get registers: %w", err)
	}
	var retBuf [8]byte
	if err := e.backend.ReadMemory(regs.SP, retBuf[:]); err != nil {
		return fmt.Errorf("StepOut: read return address: %w", err)
	}
	retAddr := binary.LittleEndian.Uint64(retBuf[:])
	if retAddr == 0 {
		return fmt.Errorf("StepOut: null return address — at outermost frame?")
	}
	if e.lastBP != nil {
		return e.resumeFromBreakpoint(bpResumeStepOut, retAddr)
	}
	_, setErr := e.bps.set(e.backend, "<stepout-return>", 0, retAddr)
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

// resumeFromBreakpoint handles the step-over sequence for software breakpoints:
// restore original bytes → single-step past the instruction → reinstall trap
// (done in handleStop's StopSingleStep case) → then perform action.
func (e *engine) resumeFromBreakpoint(action bpResumeAction, retAddr uint64) error {
	bp := e.lastBP
	e.lastBP = nil
	e.steppingOverBP = bp
	e.bpResume = action
	e.bpRetAddr = retAddr

	// Temporarily remove from the table and restore original bytes so the
	// single-step executes the real instruction, not the trap.
	e.bps.removeFromTable(bp)
	if err := e.backend.WriteMemory(bp.addr, bp.originalBytes); err != nil {
		e.bps.addToTable(bp) // roll back
		e.steppingOverBP = nil
		return fmt.Errorf("resume BP: restore bytes: %w", err)
	}

	// Use the TID of the thread that hit the breakpoint. On Darwin, task_threads
	// returns threads in creation order; blindly using threads[0] often picks an
	// idle Go runtime M instead of the goroutine running user code.
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
	if err := e.backend.SingleStep(tid); err != nil {
		_ = e.backend.WriteMemory(bp.addr, archTrapInstruction())
		e.bps.addToTable(bp)
		e.steppingOverBP = nil
		return fmt.Errorf("resume BP: single step: %w", err)
	}
	e.setState(stateRunning)
	go e.waitLoop()
	return nil
}

// ── Stack walking ─────────────────────────────────────────────────────────────

func (e *engine) collectFrames() ([]protocol.Frame, error) {
	if e.dw == nil {
		return nil, nil
	}
	threads, err := e.backend.Threads()
	if err != nil || len(threads) == 0 {
		return nil, fmt.Errorf("StackFrames: no threads")
	}
	regs, err := e.backend.GetRegisters(threads[0])
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

// ── Goroutines ────────────────────────────────────────────────────────────────

func (e *engine) readGoroutines() ([]protocol.Goroutine, error) {
	threads, err := e.backend.Threads()
	if err != nil || len(threads) == 0 {
		return nil, nil
	}
	regs, err := e.backend.GetRegisters(threads[0])
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

// ── DWARF loading ─────────────────────────────────────────────────────────────

func (e *engine) loadDWARF(binaryPath string) {
	dr, err := openDWARF(binaryPath)
	if err != nil {
		e.dw = nil
		return
	}
	// On platforms that support ASLR (Darwin ARM64), ask the backend for the
	// slide so DWARF addresses can be adjusted to match the actual load address.
	if sg, ok := e.backend.(interface{ TextSlide(string) int64 }); ok {
		dr.slide = sg.TextSlide(binaryPath)
	}
	e.dw = dr
}

// ── Emission ──────────────────────────────────────────────────────────────────

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
		return
	}
	select {
	case e.events <- evt:
	default:
	}
}

func (e *engine) emitBreakpointHit(bp *breakpointEntry, stop StopEvent) {
	frames, _ := e.collectFrames()
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

// emitStoppedAtCurrentPC reads the current PC and emits an EventStepped so
// clients know where the process is stopped (used after Launch/Attach).
// Always emits even if registers cannot be read — the hub must receive a
// suspending event or it loses track of state and drops resume commands.
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
	frames, _ := e.collectFrames()
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

func (e *engine) emitProcessExited(code int) {
	e.emit(protocol.EventProcessExited, protocol.ProcessExitedPayload{ExitCode: code})
}

func (e *engine) emitOutput(stream, content string) {
	e.emit(protocol.EventOutput, protocol.OutputPayload{Stream: stream, Content: content})
}

func (e *engine) emitError(cmd protocol.CommandKind, err error) {
	e.emit(protocol.EventError, protocol.ErrorPayload{Command: cmd, Message: err.Error()})
}

// ── State ─────────────────────────────────────────────────────────────────────

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

// ── Dispatch ──────────────────────────────────────────────────────────────────

func (e *engine) dispatch(fn func() error) error {
	ch := make(chan error, 1)
	// Select on both cmdCh and done. If the engine loop has already exited
	// (done is closed), return ErrProcessExited immediately rather than
	// blocking forever or panicking on a send to a closed channel.
	select {
	case e.cmdCh <- engineCmd{fn: fn, err: ch}:
	case <-e.done:
		return ErrProcessExited
	}
	return <-ch
}
