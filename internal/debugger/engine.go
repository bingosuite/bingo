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
			case bpResumeStep:
				e.setState(stateSuspended)
				e.emitStepped(stop)
			case bpResumeSourceStep:
				// Use sob.file/sob.line (the BP's known location) rather than
				// a DWARF lookup from stop.PC: stop.PC is one instruction past
				// the BP and can land on a DWARF entry with line==0.
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
	return <-ch
}
