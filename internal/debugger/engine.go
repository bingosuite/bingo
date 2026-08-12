package debugger

import (
	"bytes"
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

	// eventBufReserve is extra channel capacity on top of eventBufSize that
	// ORDINARY events may never occupy. It is held back so a suspending halt
	// event always has somewhere to go: emit is non-blocking and drops on a
	// full buffer, and dropping the halt's EventPaused is what strands a
	// session for good (issue #183). See haltOnError and emitReserved.
	eventBufReserve = 1

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

	// A sibling's already-queued hit can surface after the trap that produced it
	// has been restored and removed from bps. This includes one-shot internal
	// sentinels and user entries cleared during an in-flight step-off. A stale
	// stop is resumed only when current memory matches bytes this engine actually
	// restored and no live architecture trap spans that address. The histories
	// stay separate so the Linux internal-sentinel diagnostic remains precise.
	retiredInternalBreakpointBytes map[uint64][][]byte
	retiredClearedBreakpointBytes  map[uint64][][]byte
	retiredInternalBreakpointHits  int

	// manualStopPending records that a Pause request has fired the backend's
	// interrupt signal (PauseSignal — SIGSTOP on linux, SIGUSR2 on darwin) at
	// the tracee and we are awaiting the resulting signal-delivery stop, which
	// should be turned into EventPaused rather than auto-resumed. It needs no
	// synchronization: both Pause()'s dispatched closure and handleStop run on
	// the single engine loop thread. See AGENTS.md → Pause.
	manualStopPending bool

	// goLayout caches the DWARF-resolved runtime struct offsets used by the
	// goroutine/thread snapshot reader. Resolved lazily per loaded image and
	// reset in loadDWARF. nil until first use; see goroutines.go.
	goLayout *goLayout

	// prevGoids is the set of live goids from the previous snapshot, used to
	// compute created/exited lifecycle deltas. Loop-thread-only (like
	// manualStopPending); needs no synchronization. See goroutines.go.
	prevGoids map[int]struct{}

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

// stepThreadExitCompleter closes the linux parked-stop gate after the engine has
// reinstalled the breakpoint whose hardware step owner died, and releases the
// thread the backend held as the write anchor for that reinstall. It is separate
// from threadStepper because Kill may call endThreadStep concurrently with Wait,
// whereas this callback runs only in the serialized Wait→engine→Wait handoff.
//
// It returns an error because the release is a real ptrace op that can fail. A
// failure must halt the engine suspended with the gate still closed, not tear
// the session down: the tracee is intact and Kill/Restart remains the recovery.
type stepThreadExitCompleter interface {
	completeStepThreadExit() error
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

func (e *engine) completeStepThreadExit() error {
	if c, ok := e.backend.(stepThreadExitCompleter); ok {
		return c.completeStepThreadExit()
	}
	return nil
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
		events:  make(chan protocol.Event, eventBufSize+eventBufReserve),
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
		// A running tracee has an in-flight waitLoop blocked in Wait4(-1); on
		// linux that waitLoop — not killProcess — must reap the SIGKILL death,
		// since two concurrent wait4 callers race and wedge Kill (#111). Capture
		// whether we're running before endThreadStep/clearAll touch anything.
		running := e.getState() == stateRunning
		// Release any threads held for an in-flight atomic step-over first, so
		// a detach (attached-process Kill) never leaves them Mach-suspended.
		e.endThreadStep()
		e.clearAllBreakpoints()
		if killErr := e.proc.kill(e.backend, running); killErr != nil {
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
		return e.clearBreakpoint(id)
	})
}

func (e *engine) clearBreakpoint(id int) error {
	if e.steppingOverBP.matchesID(id) {
		if !e.steppingOverBP.enabled {
			return breakpointNotFound(id)
		}
		// The step-off path already restored the original instruction and
		// transiently removed this entry from the table. Keep its metadata for
		// the pending resume action, but make completion skip the reinstall.
		// Its restored bytes identify any same-address sibling hit the kernel
		// had already queued, so that stop can resume at the instruction rather
		// than being advanced into or past it as an unowned trap.
		e.retiredClearedBreakpointBytes = recordRetiredBreakpointBytes(
			e.retiredClearedBreakpointBytes, e.steppingOverBP.addr, e.steppingOverBP.originalBytes)
		e.steppingOverBP.enabled = false
		return nil
	}

	entry := e.bps.atID(id)
	if err := e.bps.clear(e.backend, id); err != nil {
		return err
	}
	e.invalidateClearedBreakpoint(entry)
	return nil
}

func (e *engine) invalidateClearedBreakpoint(entry *breakpointEntry) {
	if e.lastBP != entry {
		return
	}
	e.lastBP = nil
	e.lastBPTID = 0
}

func (e *engine) clearAllBreakpoints() {
	e.bps.clearAll(e.backend)
	if e.lastBP != nil && !e.lastBP.enabled {
		e.invalidateClearedBreakpoint(e.lastBP)
	}
	if e.steppingOverBP != nil {
		// Its bytes were restored before the single-step began, so no backend
		// write is needed to clear the transiently table-less entry.
		e.steppingOverBP.enabled = false
	}
}

func (e *engine) Continue() error {
	return e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		if e.lastBP != nil {
			if err := e.resumeFromBreakpoint(bpResumeContinue, 0); err != nil {
				return err
			}
			e.emitContinued()
			return nil
		}
		if err := e.backend.ContinueProcess(); err != nil {
			return err
		}
		e.setState(stateRunning)
		go e.waitLoop()
		e.emitContinued()
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
		framePC, frameBase, err := e.frameLocation(frameIndex)
		if err != nil {
			return fmt.Errorf("Locals: %w", err)
		}
		vars, err = e.dw.LocalsForFrame(e.backend, framePC, frameBase)
		return err
	})
	return vars, err
}

// Evaluate resolves a single variable NAME in the given frame (local/parameter
// first, then a package global) and returns its bounded typed tree. It is
// non-suspending and non-resuming — like Locals, it only reads a suspended
// tracee. Expression parsing (dotted paths, indexing, arithmetic) is a later PR.
func (e *engine) Evaluate(frameIndex int, name string) (protocol.Variable, error) {
	var result protocol.Variable
	err := e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		framePC, frameBase, err := e.frameLocation(frameIndex)
		if err != nil {
			return fmt.Errorf("Evaluate: %w", err)
		}
		result, err = e.dw.EvaluateName(e.backend, framePC, frameBase, name)
		return err
	})
	return result, err
}

// frameLocation computes the DWARF lookup PC and frame-base address for the
// given stack frame of the currently-stopped thread. Callers must already be on
// the engine loop (inside dispatch) and have verified suspension.
//
// It inspects the thread the user is stopped on (curTID via activeTID), not
// threads[0]: on Darwin threads[0] is frequently an idle runtime M, so a
// breakpoint that fires on another thread would otherwise report an unrelated
// frame. See the activeTID/collectFrames invariant.
func (e *engine) frameLocation(frameIndex int) (framePC, frameBase uint64, err error) {
	if e.dw == nil {
		return 0, 0, fmt.Errorf("no DWARF info")
	}
	tid, err := e.activeTID()
	if err != nil {
		return 0, 0, err
	}
	regs, err := e.backend.GetRegisters(tid)
	if err != nil {
		return 0, 0, fmt.Errorf("get registers: %w", err)
	}
	framePCs := e.walkStack(regs)
	if frameIndex < 0 || frameIndex >= len(framePCs) {
		return 0, 0, fmt.Errorf("frame index %d out of range (have %d frames)",
			frameIndex, len(framePCs))
	}
	// Resolve the CFA (Go's DW_AT_frame_base) for each frame from 0 up to the
	// requested one. Locals are DW_OP_fbreg offsets from the CFA, recovered
	// from .debug_frame CFI: the Go rule is SP-relative on arm64 (frame pointer
	// + framesize, NOT + 16, because x29 points at the saved FP/LR pair at the
	// bottom of the frame with locals above it) and frame-pointer-relative on
	// amd64. Frames chain by SP_{i+1} = CFA_i — a callee's CFA is its caller's
	// SP at the call, and Go passes arguments in registers so the call itself
	// leaves SP unmoved (arm64) / only pushes the return address (amd64, which
	// the FP-relative rule already accounts for).
	const cfaFallbackFromFP = 16
	sp, fp := regs.SP, regs.BP
	for i := 0; i < frameIndex; i++ {
		cfa, ok := e.dw.cfa(frameLookupPC(framePCs[i], i), sp, fp)
		if !ok {
			cfa = fp + cfaFallbackFromFP
		}
		var buf [8]byte
		if err := e.backend.ReadMemory(fp, buf[:]); err != nil {
			return 0, 0, fmt.Errorf("read frame pointer: %w", err)
		}
		sp = cfa
		fp = binary.LittleEndian.Uint64(buf[:])
	}
	framePC = frameLookupPC(framePCs[frameIndex], frameIndex)
	frameBase, ok := e.dw.cfa(framePC, sp, fp)
	if !ok {
		frameBase = fp + cfaFallbackFromFP
	}
	return framePC, frameBase, nil
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

// GoroutineSnapshot returns the full concurrency picture on demand: every
// goroutine (with parent linkage for a spawn tree), every OS thread, the
// current goroutine, and the created/exited lifecycle deltas since the previous
// snapshot. Only valid while suspended (the tracee must be stopped for the
// memory reads to be race-free). Like the auto-streamed snapshots it advances
// the lifecycle-delta baseline.
func (e *engine) GoroutineSnapshot() (protocol.GoroutineSnapshotPayload, error) {
	var snap protocol.GoroutineSnapshotPayload
	err := e.dispatch(func() error {
		if err := e.requireSuspended(); err != nil {
			return err
		}
		snap = e.goroutineSnapshot()
		return nil
	})
	return snap, err
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
					// A session-invalidating failure leaves threads stopped and
					// owned by us — the one that triggered it, plus anything the
					// park queue was holding. Exiting the loop closes the tracer
					// thread, and the kernel then detaches every tracee and
					// resumes it straight into breakpoints that are still
					// written into its text. Discharge them explicitly instead.
					if errors.Is(result.err, ErrSessionInvalidated) {
						// Restoring saved bytes into an image that has already
						// been replaced would poke old instructions at old
						// addresses into the NEW one. Nothing of ours is in it.
						e.discardTracee(!errors.Is(result.err, ErrImageReplaced))
					}
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

// discardTracee gives up the tracee after a session-invalidating backend
// failure, without going through Kill's state machine (the loop is already on
// its way out).
//
// restore says whether the original instruction bytes should be written back
// first. They must be for a tracee that still runs the image they came from —
// that restoration is what makes releasing its threads safe. They must NOT be
// when an execve already replaced the image: those bytes describe addresses in a
// program that no longer exists, so writing them would corrupt the new one.
//
// A launched process is then SIGKILLed and reaped, which resumes every thread
// still ptrace-stopped — including anything the park queue held and the thread
// whose stop failed the wait. An attached process is detached instead; the
// kernel releases its remaining threads when the tracer thread closes.
//
// running is false because the waitLoop that produced this failure has already
// delivered its result, so killProcess is the sole reaper (see #111).
func (e *engine) discardTracee(restore bool) {
	e.endThreadStep()
	if restore {
		e.bps.clearAll(e.backend)
	}
	if err := e.proc.kill(e.backend, false); err != nil {
		e.log.Warn("discarding the tracee after a session-invalidating stop failed",
			"err", err)
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
		// A breakpoint stop while a step-over is still in flight means that
		// step-over will never complete: its own StopSingleStep is the only
		// other way out of it, and both backends refuse to surface a foreign
		// breakpoint while the stepped thread can still produce one (linux
		// parks it, darwin re-faults it). So the stepped thread died, and the
		// retained entry must be resolved before the table lookup. A live entry
		// needs its lifted trap put back so a same-address sibling resolves to
		// the real breakpoint; a cleared entry already owns restored bytes and
		// must not be resurrected.
		if sob := e.steppingOverBP; sob != nil {
			e.steppingOverBP = nil
			if sob.enabled {
				if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
					e.endThreadStep()
					e.log.Error("breakpoint reinstall failed after the stepped thread died",
						"addr", fmt.Sprintf("0x%x", sob.addr), "err", rerr)
					e.haltOnError(protocol.CmdNone, fmt.Errorf(
						"reinstall breakpoint 0x%x after the stepped thread died "+
							"(it may no longer be armed or tracked): %w", sob.addr, rerr), stop)
					return
				}
				e.log.Debug("breakpoint reinstalled after stepped-thread death",
					"addr", fmt.Sprintf("0x%x", sob.addr))
			}
			e.endThreadStep()
		}
		var err error
		stop, err = e.populateBreakpointStop(stop)
		if err != nil {
			// Without a located stop the PC was never rewound off the trap.
			// On amd64 RIP is left one byte past the INT3, so a plain resume
			// would execute from mid-instruction; say so, because the client
			// has no other way to know that continuing is the unsafe option.
			e.haltOnError(protocol.CmdNone, fmt.Errorf(
				"%w — the breakpoint PC was not rewound, so resuming may execute "+
					"from mid-instruction; kill or restart to recover safely", err), stop)
			return
		}
		bp := e.bps.atAddr(stop.PC)
		e.log.Debug("StopBreakpoint", "pc", fmt.Sprintf("0x%x", stop.PC),
			"found", bp != nil,
			"steppingOverBP", e.steppingOverBP != nil)
		if bp == nil {
			if e.resumeRetiredBreakpoint(stop) {
				return
			}
			// Spurious SIGTRAP — a BRK we did not install (Go runtime
			// internal trap or libc assertion). On ARM64 PC points AT the
			// BRK; ContinueProcess with signal=0 leaves PC unchanged and
			// re-executes the trap forever. Advance PC past the 4-byte BRK.
			e.resumeUnownedBreakpoint(stop, stop.PC+uint64(len(archTrapInstruction())))
			return
		}
		e.log.Debug("StopBreakpoint matched", "file", bp.file, "line", bp.line,
			"addr", fmt.Sprintf("0x%x", bp.addr))
		e.rewindToBreakpoint(stop)
		if bp.file == stepOverNextFile || bp.file == stepOutReturnFile {
			if !e.retireInternalBreakpoint(bp, stop) {
				return
			}
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
			// Rearm a still-live in-flight BP and release held threads before
			// surfacing the error, so the process is left in a clean state.
			if sob := e.steppingOverBP; sob != nil {
				e.steppingOverBP = nil
				if sob.enabled {
					_ = e.bps.reinstall(e.backend, sob)
				}
			}
			e.endThreadStep()
			e.setState(stateSuspended)
			e.haltOnError(protocol.CmdNone, err, stop)
			return
		}
		e.log.Debug("StopSingleStep", "pc", fmt.Sprintf("0x%x", stop.PC),
			"steppingOverBP", e.steppingOverBP != nil)
		if sob := e.steppingOverBP; sob != nil {
			e.steppingOverBP = nil
			if sob.enabled {
				if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
					// Reinstall failed. Suspend instead of resuming — running
					// without the trap would let the process loose. The breakpoint
					// stays out of the table: its trap is not guaranteed to be
					// installed, so re-adding the entry would advertise an armed
					// breakpoint that can never fire.
					e.endThreadStep()
					e.log.Error("breakpoint reinstall failed — suspending to prevent runaway process",
						"addr", fmt.Sprintf("0x%x", sob.addr), "err", rerr)
					e.setState(stateSuspended)
					e.haltOnError(protocol.CmdNone, fmt.Errorf(
						"reinstall breakpoint 0x%x (it may no longer be armed or tracked): %w", sob.addr, rerr), stop)
					return
				}
				e.log.Debug("breakpoint reinstalled", "addr", fmt.Sprintf("0x%x", sob.addr))
			}
			// The original instruction is executable now: either the live trap
			// was reinstalled or a clear cancelled it while the step was in flight.
			e.endThreadStep()
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
					// The tracee is halted here — the step-over trap was
					// reinstalled and the stepping bookkeeping ended (which
					// disarms the hardware single-step but deliberately does
					// NOT resume the world; only ContinueProcess does, and it
					// is never reached on this path). resumeFromBreakpoint left
					// the engine running, so unlike the other failure paths
					// this one must correct the state itself or every later
					// command is rejected with ErrNotSuspended against a
					// process that cannot move.
					e.setState(stateSuspended)
					e.haltOnError(protocol.CmdStepOut,
						fmt.Errorf("StepOut: set return breakpoint: %w", setErr), stop)
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

	case StopStepThreadExited:
		// The backend is holding a genuinely ptrace-stopped thread solely as a
		// memory-write anchor — the dying step owner itself, or a sibling whose
		// real stop stays queued. Resolve the breakpoint transaction FIRST:
		// reinstall a live entry through the anchor, or preserve the restored
		// bytes of an entry cleared mid-step. Only the acknowledgement below
		// releases that anchor and lets held stops drain.
		if sob := e.steppingOverBP; sob != nil {
			if sob.enabled {
				if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
					// steppingOverBP is deliberately left set: the trap is still out
					// of the tracee and the engine still owns it, which is also what
					// keeps the backend's gate closed so Continue/Step are refused.
					e.setState(stateSuspended)
					e.haltOnError(protocol.CmdNone, fmt.Errorf(
						"reinstall breakpoint 0x%x after stepped thread exited; kill or restart to recover safely: %w",
						sob.addr, rerr), stop)
					return
				}
				e.log.Debug("breakpoint reinstalled after stepped thread exited",
					"addr", fmt.Sprintf("0x%x", sob.addr))
			}
			e.steppingOverBP = nil
		}
		if cerr := e.completeStepThreadExit(); cerr != nil {
			// The breakpoint transaction is resolved but the anchor could not be
			// released, so the gate stays closed and no waitLoop is started:
			// resuming would leave a thread stopped that nothing will ever deliver.
			e.setState(stateSuspended)
			e.haltOnError(protocol.CmdNone, fmt.Errorf(
				"reconcile the exited step owner; kill or restart to recover safely: %w",
				cerr), stop)
			return
		}
		e.setState(stateRunning)
		go e.waitLoop()

	case StopSignal:
		// Reinstall any still-live in-flight BP before resuming or suspending.
		if sob := e.steppingOverBP; sob != nil {
			e.steppingOverBP = nil
			if sob.enabled {
				if rerr := e.bps.reinstall(e.backend, sob); rerr != nil {
					e.endThreadStep()
					e.setState(stateSuspended)
					e.haltOnError(protocol.CmdNone, fmt.Errorf(
						"reinstall breakpoint 0x%x after signal (it may no longer be armed or tracked): %w",
						sob.addr, rerr), stop)
					return
				}
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
					e.haltOnError(protocol.CmdNone, err, stop)
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

func (e *engine) retireInternalBreakpoint(bp *breakpointEntry, stop StopEvent) bool {
	if err := e.bps.clear(e.backend, bp.id); err != nil {
		e.setState(stateSuspended)
		e.haltOnError(protocol.CmdNone,
			fmt.Errorf("clear internal breakpoint 0x%x: %w", bp.addr, err), stop)
		return false
	}
	e.retiredInternalBreakpointBytes = recordRetiredBreakpointBytes(
		e.retiredInternalBreakpointBytes, bp.addr, bp.originalBytes)
	return true
}

func recordRetiredBreakpointBytes(history map[uint64][][]byte, addr uint64, original []byte) map[uint64][][]byte {
	if history == nil {
		history = make(map[uint64][][]byte)
	}
	restored := bytes.Clone(original)
	priorBytes := history[addr]
	for _, prior := range priorBytes {
		if bytes.Equal(prior, restored) {
			return history
		}
	}
	history[addr] = append(priorBytes, restored)
	return history
}

func matchesRetiredBreakpointBytes(current []byte, history [][]byte) bool {
	for _, prior := range history {
		if bytes.Equal(current, prior) {
			return true
		}
	}
	return false
}

// resumeRetiredBreakpoint handles a sibling whose kernel stop arrives after
// another thread removed the trap that produced it. On amd64 the live RIP is
// then one byte into the restored instruction. An address match is insufficient:
// tracee code may later place a genuine trap there, including an x86 CD 03
// spanning the rewind boundary, and rewinding that live trap would re-execute it
// forever.
func (e *engine) resumeRetiredBreakpoint(stop StopEvent) bool {
	internalHistory := e.retiredInternalBreakpointBytes[stop.PC]
	clearedHistory := e.retiredClearedBreakpointBytes[stop.PC]
	if len(internalHistory) == 0 && len(clearedHistory) == 0 {
		return false
	}

	liveResumePC, liveTrap, current, err := archLiveTrapResumePC(e.backend, stop.PC)
	if err != nil {
		e.setState(stateSuspended)
		e.haltOnError(protocol.CmdNone,
			fmt.Errorf("inspect retired breakpoint at 0x%x: %w", stop.PC, err), stop)
		return true
	}
	if liveTrap {
		e.log.Debug("retired address now contains a live trap; advancing it",
			"tid", stop.TID, "pc", fmt.Sprintf("0x%x", stop.PC))
		e.resumeUnownedBreakpoint(stop, liveResumePC)
		return true
	}

	internalMatch := matchesRetiredBreakpointBytes(current, internalHistory)
	if !internalMatch && !matchesRetiredBreakpointBytes(current, clearedHistory) {
		e.log.Debug("retired address no longer matches restored breakpoint bytes",
			"tid", stop.TID, "pc", fmt.Sprintf("0x%x", stop.PC))
		e.resumeUnownedBreakpoint(stop, stop.PC+uint64(len(archTrapInstruction())))
		return true
	}

	regs, err := e.backend.GetRegisters(stop.TID)
	if err != nil {
		e.setState(stateSuspended)
		e.haltOnError(protocol.CmdNone,
			fmt.Errorf("read registers for retired breakpoint on thread %d: %w", stop.TID, err), stop)
		return true
	}
	if regs.PC != stop.PC {
		regs.PC = stop.PC
		if err := e.backend.SetRegisters(stop.TID, regs); err != nil {
			e.setState(stateSuspended)
			e.haltOnError(protocol.CmdNone,
				fmt.Errorf("rewind retired breakpoint on thread %d: %w", stop.TID, err), stop)
			return true
		}
	}

	e.log.Debug("resuming delayed sibling hit on retired breakpoint",
		"tid", stop.TID, "pc", fmt.Sprintf("0x%x", stop.PC))
	if err := e.backend.ContinueProcess(); err != nil {
		e.setState(stateSuspended)
		e.haltOnError(protocol.CmdNone,
			fmt.Errorf("continue after retired breakpoint on thread %d: %w", stop.TID, err), stop)
		return true
	}
	if internalMatch {
		e.retiredInternalBreakpointHits++
	}
	e.setState(stateRunning)
	go e.waitLoop()
	return true
}

func (e *engine) resumeUnownedBreakpoint(stop StopEvent, resumePC uint64) {
	e.log.Warn("spurious SIGTRAP — advancing past unowned trap and resuming",
		"pc", fmt.Sprintf("0x%x", stop.PC))
	if regs, err := e.backend.GetRegisters(stop.TID); err == nil {
		regs.PC = resumePC
		_ = e.backend.SetRegisters(stop.TID, regs)
	}
	_ = e.backend.ContinueProcess()
	e.setState(stateRunning)
	go e.waitLoop()
}

func (e *engine) populateStopPC(stop StopEvent, rewind bool) (StopEvent, error) {
	if stop.PC != 0 {
		return stop, nil
	}
	// Keep the guessed thread local until it is confirmed by a successful
	// register read. On darwin task_threads returns creation order, so
	// threads[0] is frequently an idle runtime M rather than the thread that
	// stopped; committing it to stop.TID on the error path would hand that
	// wrong thread to curTID via the halt event, and curTID is what every
	// later step primitive targets (see the curTID field comment). A real TID
	// supplied by the backend is preserved untouched either way.
	tid := stop.TID
	if tid == 0 {
		threads, err := e.backend.Threads()
		if err != nil {
			return stop, fmt.Errorf("get stop thread: %w", err)
		}
		if len(threads) == 0 {
			return stop, fmt.Errorf("get stop thread: no threads")
		}
		tid = threads[0]
	}
	regs, err := e.backend.GetRegisters(tid)
	if err != nil {
		return stop, fmt.Errorf("get stop PC for tid %d: %w", tid, err)
	}
	stop.TID = tid
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
// restore bytes → single-step → conditionally reinstall the still-enabled trap
// (in StopSingleStep handler) → perform action.
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

func (e *engine) loadDWARF(binaryPath string) {
	// A new image invalidates the cached runtime struct offsets and the
	// lifecycle delta baseline.
	e.goLayout = nil
	e.prevGoids = nil
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

// emit queues an ordinary event. It refuses to touch the reserved tail slot,
// which exists solely so a suspending halt event can always be delivered — see
// emitReserved.
func (e *engine) emit(kind protocol.EventKind, payload any) {
	e.emitWithin(kind, payload, eventBufSize)
}

// emitReserved queues an event that may use the reserved slot on top of
// eventBufSize. Only suspending halt events use it: they are the one kind whose
// loss is unrecoverable, because the hub gates on them and drains resumeCh only
// while gated.
//
// One reserved slot is enough because a second halt cannot be queued behind the
// first. Emitting a halt leaves the engine stateSuspended with the tracee
// stopped, and a stopped tracee cannot produce the next stop; the only way back
// to running is a resume, which the hub sends exclusively from inside the
// suspend wait loop it enters after RECEIVING the suspending event. So the
// reserved slot is always free again before another halt can be reported. The
// same argument covers the ordinary suspending events (BreakpointHit/Stepped/
// Paused/Panic): none can still be queued when a halt is handled.
func (e *engine) emitReserved(kind protocol.EventKind, payload any) {
	e.emitWithin(kind, payload, eventBufSize+eventBufReserve)
}

func (e *engine) emitWithin(kind protocol.EventKind, payload any, limit int) {
	evt, err := protocol.NewEvent(kind, e.nextSeq(), payload)
	if err != nil {
		slog.Error("engine.emit: marshal event failed", "kind", kind, "err", err)
		return
	}
	// Non-blocking on purpose: this runs on the serialized loop, so blocking
	// while a reader is gone would deadlock the loop against its own teardown.
	// The buffer is sized so the continuously-draining hub never fills it, and
	// the exit path is backstopped by the events channel closing on loop return.
	//
	// The length check is race-safe without a lock: the loop thread is the only
	// writer, so len can only FALL between the check and the send as the hub
	// drains. Observing room therefore guarantees the send succeeds.
	if len(e.events) >= limit {
		slog.Warn("engine.emit: events buffer full — dropping", "kind", kind, "limit", limit)
		return
	}
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
	// Build the concurrency snapshot once: the current goroutine is embedded in
	// the stop event, then the full snapshot is streamed as its own event. One
	// build avoids a double allgs scan and a double lifecycle-delta pass.
	snap := e.goroutineSnapshot()
	e.emit(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{
		Breakpoint: bp.toProtocol(),
		Goroutine:  currentGoroutineFrom(snap),
		Frames:     frames,
	})
	e.emit(protocol.EventGoroutineSnapshot, snap)
}

// emitGoroutineSnapshot builds and streams a standalone concurrency snapshot.
// Used at the entry stop and on the CmdGoroutineSnapshot on-demand path. It is
// a non-suspending event, so the hub forwards it without gating.
func (e *engine) emitGoroutineSnapshot() {
	e.emit(protocol.EventGoroutineSnapshot, e.goroutineSnapshot())
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
	// The entry stop is the one Stepped that carries a full snapshot: it seeds a
	// UI with the initial goroutine/thread picture and establishes the
	// lifecycle-delta baseline. Regular steps stay cheap (no snapshot). At the
	// very first entry the runtime may be pre-init, so the snapshot degrades to
	// the synthetic goroutine.
	e.emitGoroutineSnapshot()
}

func (e *engine) emitStepped(stop StopEvent) {
	if stop.TID != 0 {
		e.curTID = stop.TID
	}
	// Completing a step suspends for a self-stop, which cancels any pending
	// Pause the same way a breakpoint hit does (see emitBreakpointHit).
	e.manualStopPending = false
	frames, _ := e.collectFrames(stop.TID)
	loc := protocol.Location{}
	if e.dw != nil {
		loc = e.dw.locationForPC(stop.PC)
	}
	// Steps are high-frequency and must stay cheap: embed a synthetic goroutine
	// for the stopped thread rather than scanning runtime.allgs. The rich
	// snapshot is streamed only on breakpoint/pause/entry stops, protecting the
	// fragile single-step/step-over path from extra per-step memory reads.
	e.emit(protocol.EventStepped, protocol.SteppedPayload{
		Goroutine: e.syntheticGoroutine(stop.PC),
		Location:  loc,
		Frames:    frames,
	})
}

// emitPaused reports an asynchronous Pause halt. It mirrors emitStepped but
// carries EventPaused/PausedPayload: the location is wherever execution was
// interrupted, not a source-line boundary. Like a breakpoint hit it also
// streams a full concurrency snapshot.
func (e *engine) emitPaused(stop StopEvent) {
	if stop.TID != 0 {
		e.curTID = stop.TID
	}
	frames, _ := e.collectFrames(stop.TID)
	snap := e.goroutineSnapshot()
	loc := protocol.Location{}
	if e.dw != nil {
		loc = e.dw.locationForPC(stop.PC)
	}
	e.emit(protocol.EventPaused, protocol.PausedPayload{
		Goroutine: currentGoroutineFrom(snap),
		Location:  loc,
		Frames:    frames,
	})
	e.emit(protocol.EventGoroutineSnapshot, snap)
}

// haltOnError reports a FAILED asynchronous stop-handling operation: the
// detailed cause first, then the suspending EventPaused that puts the hub back
// into its suspend wait loop. Callers must already have ensured stateSuspended.
//
// The Paused is the load-bearing half, so the two are emitted with different
// guarantees. The cause is an ordinary event and takes an ordinary slot; when
// none is free it is logged instead of emitted, since losing detail is far
// cheaper than losing the suspend. The Paused then goes out through
// emitReserved, which can always fall back on the slot ordinary events are
// forbidden to use. That is what stops a saturated buffer from delivering a
// lone non-suspending EventError and recreating issue #183.
func (e *engine) haltOnError(cmd protocol.CommandKind, cause error, stop StopEvent) {
	if len(e.events) < eventBufSize {
		e.emitError(cmd, cause)
	} else {
		e.log.Error("halting: event buffer full — logging the cause instead of emitting it",
			"command", cmd, "err", cause)
	}
	e.emitHaltedOnError(stop)
}

// emitHaltedOnError emits the suspending event for an internal halt. It exists
// because these failures are not attributable to any command in flight: the
// resume that led here already returned nil and already emitted EventContinued,
// so the hub has transitioned to running and left its suspend wait loop.
// EventError is not a suspending event, so reporting the failure alone leaves
// the hub believing the tracee runs while it is in fact halted — and since
// resumeCh is drained only inside that wait loop, every later resume sits
// unread and the session is stranded for good.
//
// It issues NO backend calls. The caller is on a backend error path, so the
// backend is by definition already failing or gone: a goroutineSnapshot (as
// emitPaused does) or even a stack walk would push dozens of further reads
// through it and could delay or prevent the one event that restores liveness.
// A synthetic goroutine and a pure-DWARF location are all the hub needs to gate
// on and all DAP needs to map to `stopped` reason=pause; clients that want more
// can request frames or a snapshot now that the session is responsive again.
// Both go through locForPC so a PC of 0 short-circuits instead of scanning
// every compilation unit, and so the location agrees with the goroutine's.
func (e *engine) emitHaltedOnError(stop StopEvent) {
	if stop.TID != 0 {
		e.curTID = stop.TID
	}
	// This is a suspend like any other self-stop, so it cancels a racing Pause
	// whose interrupt is still queued in the tracee (see emitBreakpointHit).
	e.manualStopPending = false
	e.emitReserved(protocol.EventPaused, protocol.PausedPayload{
		Goroutine: e.syntheticGoroutine(stop.PC),
		Location:  e.locForPC(stop.PC),
	})
}

// emitContinued reports that the tracee has resumed free execution in response
// to a Continue. Unlike a step — which self-completes into EventStepped — a
// plain resume produces no later stop of its own, so without this event a
// client that did not issue the resume (another WebSocket observer, or a DAP
// adapter translating it into a `continued` event) has no way to learn the
// process is running again until it happens to hit the next breakpoint. Steps
// deliberately do not emit it.
func (e *engine) emitContinued() {
	e.emit(protocol.EventContinued, protocol.ContinuedPayload{})
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
