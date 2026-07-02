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
	bpResume       bpResumeAction
	bpRetAddr      uint64 // bpResumeStepOut only

	// Source-line target remembered from the previous step-over. More
	// reliable than re-querying locationForPC, which can land on a DWARF
	// boundary with line==0. Zeroed on each sourceStepOver and on user-BP hits.
	stepOverFile string
	stepOverLine int

	// Destination address of an in-flight continue-based step-over/step-out.
	// A one-shot sentinel trap is planted here and the origin trap removed, so
	// the thread runs to the destination with a plain continue (no hardware
	// single-step, which is unreliable on Darwin/arm64). Zero when no
	// step-over is in flight. See resumeFromBreakpoint / finishStepOverContinue.
	stepDestAddr uint64
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

func (e *engine) Events() <-chan protocol.Event { return e.events }

func (e *engine) Launch(binaryPath string, args []string, env []string) error {
	return e.dispatch(func() error {
		if err := e.proc.launch(binaryPath, args, env); err != nil {
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
		if err := e.proc.attach(pid); err != nil {
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
		tid := e.inspectionTID(0)
		if tid == 0 {
			return fmt.Errorf("Locals: no threads")
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
		frames, err = e.collectFrames(0)
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
		goroutines, err = e.readGoroutines(0)
		return err
	})
	return goroutines, err
}

func (e *engine) loop() {
	// All ptrace calls are made from dispatch closures running on this
	// goroutine. Pin to one OS thread: ptrace is thread-specific on Linux.
	runtime.LockOSThread()
	defer func() {
		close(e.done)
		close(e.events)
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
		slog.Debug("StopBreakpoint", "pc", fmt.Sprintf("0x%x", stop.PC),
			"found", bp != nil,
			"steppingOverBP", e.steppingOverBP != nil)
		if bp == nil {
			// Spurious SIGTRAP — a BRK we did not install (Go runtime
			// internal trap or libc assertion). On ARM64 PC points AT the
			// BRK; ContinueProcess with signal=0 leaves PC unchanged and
			// re-executes the trap forever. Advance PC past the 4-byte BRK.
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
		if bp.file == stepOverNextFile {
			_ = e.bps.clear(e.backend, bp.id)
			e.finishStepOverContinue(stop)
			return
		}
		if bp.file == stepOutReturnFile {
			_ = e.bps.clear(e.backend, bp.id)
			e.finishStepOverContinue(stop)
			return
		}
		e.lastBP = bp
		e.lastBPTID = stop.TID
		// A genuine user breakpoint. If a step-over/step-out was in flight
		// (its origin trap removed, destination sentinel armed), tear that
		// state down — reinstalling the origin and clearing the sentinel —
		// before reporting the hit.
		e.abandonStepOver()
		e.emitBreakpointHit(bp, stop)

	case StopSingleStep:
		var err error
		stop, err = e.populateStopPC(stop, false)
		if err != nil {
			e.setState(stateSuspended)
			e.emitError(protocol.CmdNone, err)
			return
		}
		slog.Debug("StopSingleStep", "pc", fmt.Sprintf("0x%x", stop.PC),
			"steppingOverBP", e.steppingOverBP != nil)
		if sob := e.steppingOverBP; sob != nil {
			// The single-step moved the thread off sob.addr (whose original
			// bytes were restored for the step); reinstall the trap before
			// resuming so the breakpoint stays active.
			e.steppingOverBP = nil
			if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
				// Reinstall failed. Suspend instead of resuming — running
				// without the trap would let the process loose.
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
			default:
				// bpResumeStep, and the rare bpResumeSourceStep/bpResumeStepOut
				// fallback where no destination address was available (missing
				// DWARF / return addr) so resumeFromBreakpoint single-stepped
				// instead of continuing to a sentinel. We've advanced one
				// instruction off the origin and reinstalled the trap; report
				// that as the step result rather than risk a hang.
				e.setState(stateSuspended)
				e.emitStepped(stop)
			}
			return
		}
		e.setState(stateSuspended)
		e.emitStepped(stop)

	case StopSignal:
		// Reinstall any in-flight step-over BP before resuming.
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

// resumeFromBreakpoint advances past the software breakpoint the process is
// stopped on (e.lastBP) and then performs the requested action.
//
// For source-level step-over and step-out we know the destination address (the
// next source line, or the return address), so we plant a one-shot sentinel trap
// there, remove the origin trap, single-step the trapped thread one instruction
// off the just-fired origin BRK PC (see stepOffClearedBP — a plain continue from
// a just-fired-BRK PC intermittently wedges on Darwin/arm64), then let it run to
// the sentinel with a plain continue. The destination trap is guaranteed to be on
// the execution path. The origin trap is reinstalled when the sentinel is hit
// (finishStepOverContinue), which also steps the thread off the sentinel PC.
//
// For plain continue and machine-instruction step we have no destination, so we
// fall back to the classic restore → single-step → reinstall sequence (the
// StopSingleStep handler reinstalls the trap and performs the action). That path
// single-steps off the origin directly, so it also never plain-continues from a
// just-fired-BRK PC.
func (e *engine) resumeFromBreakpoint(action bpResumeAction, retAddr uint64) error {
	bp := e.lastBP
	e.lastBP = nil
	originTID := e.lastBPTID
	e.steppingOverBP = bp
	e.bpResume = action
	e.bpRetAddr = retAddr

	// Determine a known destination for step-over / step-out and plant the
	// sentinel there before touching the origin.
	e.stepDestAddr = 0
	dest, haveDest := uint64(0), false
	switch action {
	case bpResumeSourceStep:
		if e.dw != nil && bp.file != "" && bp.line > 0 {
			if nextPC, nextLine, ok := e.dw.NextLinePC(bp.file, bp.line); ok && nextPC != bp.addr {
				if _, serr := e.bps.set(e.backend, stepOverNextFile, 0, nextPC); serr == nil || errors.Is(serr, errBreakpointExists) {
					e.stepOverFile = bp.file
					e.stepOverLine = nextLine
					dest, haveDest = nextPC, true
				}
			}
		}
	case bpResumeStepOut:
		if retAddr != 0 && retAddr != bp.addr {
			if _, serr := e.bps.set(e.backend, stepOutReturnFile, 0, retAddr); serr == nil || errors.Is(serr, errBreakpointExists) {
				dest, haveDest = retAddr, true
			}
		}
	}

	// Restore the original instruction at the origin so the thread can execute
	// it. For the continue path the trap stays off until the sentinel is hit;
	// for the single-step path it is reinstalled in the StopSingleStep handler.
	e.bps.removeFromTable(bp)
	if err := e.backend.WriteMemory(bp.addr, bp.originalBytes); err != nil {
		e.bps.addToTable(bp)
		e.steppingOverBP = nil
		if haveDest {
			if entry := e.bps.atAddr(dest); entry != nil {
				_ = e.bps.clear(e.backend, entry.id)
			}
		}
		return fmt.Errorf("resume BP: restore bytes: %w", err)
	}

	if haveDest {
		e.stepDestAddr = dest
		e.stepOffClearedBP(originTID)
		if err := e.backend.ContinueProcess(); err != nil {
			_ = e.backend.WriteMemory(bp.addr, archTrapInstruction())
			e.bps.addToTable(bp)
			e.steppingOverBP = nil
			if entry := e.bps.atAddr(dest); entry != nil {
				_ = e.bps.clear(e.backend, entry.id)
			}
			e.stepDestAddr = 0
			return fmt.Errorf("resume BP: continue to destination: %w", err)
		}
		e.setState(stateRunning)
		go e.waitLoop()
		return nil
	}

	// Fallback single-step path (plain continue / instruction step).
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

// finishStepOverContinue completes a continue-based step-over/step-out: the
// origin trap was removed and a one-shot sentinel planted at the destination,
// which has just been hit (and cleared by the caller, restoring the original
// bytes there). It reinstalls the origin trap, steps the trapped thread off the
// just-cleared destination PC, and emits Stepped.
//
// The step-off is essential: the thread is parked on the PC where the sentinel
// BRK just fired, and on Darwin/arm64 a plain task continue from a just-fired-BRK
// PC intermittently wedges (the thread stays in the kernel exception-return path
// and the next wait4 blocks forever). Single-stepping one instruction off it now
// — while the process is stopped and no wait loop is active — leaves the thread
// on a clean PC so the following user Continue/Step resumes reliably. On backends
// that resume an ex-breakpoint PC fine (Linux) the step-off is a no-op.
//
// stepOverFile/stepOverLine are left intact so the next step-over can reuse the
// remembered destination line (the reported Stepped PC is still the destination;
// the extra instruction retired by the step-off stays within that source line).
func (e *engine) finishStepOverContinue(stop StopEvent) {
	if sob := e.steppingOverBP; sob != nil {
		e.steppingOverBP = nil
		if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
			// Reinstall failed. Suspend instead of resuming — running without
			// the trap would let the process loose.
			slog.Error("breakpoint reinstall failed — suspending to prevent runaway process",
				"addr", fmt.Sprintf("0x%x", sob.addr), "err", rerr)
			e.stepDestAddr = 0
			e.lastBP = nil
			e.setState(stateSuspended)
			e.emitError(protocol.CmdNone, fmt.Errorf("reinstall breakpoint 0x%x: %w", sob.addr, rerr))
			return
		}
	}
	e.stepOffClearedBP(stop.TID)
	e.stepDestAddr = 0
	e.lastBP = nil
	e.setState(stateSuspended)
	e.emitStepped(stop)
}

// abandonStepOver tears down an in-flight continue-based step-over when a
// *different* genuine breakpoint is hit before the destination sentinel. It
// reinstalls the origin trap and clears the leftover sentinel so the reported
// breakpoint hit leaves consistent state.
func (e *engine) abandonStepOver() {
	if sob := e.steppingOverBP; sob != nil {
		e.steppingOverBP = nil
		_ = e.bps.reinstall(e.backend, sob)
	}
	if e.stepDestAddr != 0 {
		if entry := e.bps.atAddr(e.stepDestAddr); entry != nil &&
			(entry.file == stepOverNextFile || entry.file == stepOutReturnFile) {
			_ = e.bps.clear(e.backend, entry.id)
		}
		e.stepDestAddr = 0
	}
	e.stepOverFile = ""
	e.stepOverLine = 0
}

// stepOffClearedBP advances tid one instruction off the PC where a software
// breakpoint just fired, before the next resume. It is used both for the origin
// user breakpoint (in resumeFromBreakpoint, before continuing to the destination
// sentinel) and for a just-cleared internal sentinel (in finishStepOverContinue,
// before handing control back for the next user resume). On Darwin/arm64 a thread
// parked on a just-fired-BRK PC cannot be reliably resumed by a plain task
// continue — it must be single-stepped off first. Backends that don't need it —
// Linux, where PTRACE_CONT resumes an ex-breakpoint PC fine — don't implement
// StepOffBreakpoint and this is a no-op. Runs synchronously while the process is
// stopped and no wait loop is active.
func (e *engine) stepOffClearedBP(tid int) {
	if tid == 0 {
		return
	}
	if so, ok := e.backend.(interface{ StepOffBreakpoint(tid int) error }); ok {
		if err := so.StepOffBreakpoint(tid); err != nil {
			slog.Warn("step off cleared breakpoint failed", "tid", tid, "err", err)
		}
	}
}

// inspectionTID resolves the thread whose state should be inspected while the
// process is suspended. Prefer the explicitly-provided trapped thread (tid);
// fall back to the last thread known to have stopped (lastBPTID), then to
// threads[0].
//
// Using the trapped thread is essential on Darwin/arm64: task_threads returns
// threads in creation order, so threads[0] is frequently an idle Go runtime M
// (or, under churn, a thread mid-create/teardown) whose transient stack yields a
// garbage frame-pointer chain. Walking that garbage produces dozens of bogus
// return addresses, and resolving each one drives DWARF into a full linear scan
// of .debug_info — up to maxStackDepth such scans back-to-back, which wedges the
// engine loop for many seconds and makes the caller's waitFor time out. The
// thread that actually hit the breakpoint/step has a valid chain that terminates
// at the top of its goroutine stack, so its walk is short and every PC resolves.
func (e *engine) inspectionTID(tid int) int {
	if tid != 0 {
		return tid
	}
	if e.lastBPTID != 0 {
		return e.lastBPTID
	}
	threads, err := e.backend.Threads()
	if err != nil || len(threads) == 0 {
		return 0
	}
	return threads[0]
}

func (e *engine) collectFrames(tid int) ([]protocol.Frame, error) {
	if e.dw == nil {
		return nil, nil
	}
	tid = e.inspectionTID(tid)
	if tid == 0 {
		return nil, fmt.Errorf("StackFrames: no threads")
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
		nextBP := binary.LittleEndian.Uint64(frame[:8])
		pcs = append(pcs, retAddr)
		// Saved frame pointers must climb monotonically toward the top of the
		// stack (higher addresses). If the chain stalls or moves backward we've
		// wandered off real frames onto a transient/garbage stack — stop rather
		// than emit up to maxStackDepth bogus PCs, each of which would trigger a
		// full-DWARF scan and stall the engine loop.
		if nextBP <= bp {
			break
		}
		bp = nextBP
	}
	return pcs
}

func (e *engine) readGoroutines(tid int) ([]protocol.Goroutine, error) {
	tid = e.inspectionTID(tid)
	if tid == 0 {
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
		return
	}
	select {
	case e.events <- evt:
	default:
	}
}

func (e *engine) emitBreakpointHit(bp *breakpointEntry, stop StopEvent) {
	frames, _ := e.collectFrames(stop.TID)
	goroutines, _ := e.readGoroutines(stop.TID)
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
	frames, _ := e.collectFrames(stop.TID)
	goroutines, _ := e.readGoroutines(stop.TID)
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
