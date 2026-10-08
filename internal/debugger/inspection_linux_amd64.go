//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const linuxInspectionTimeout = 5 * time.Second

type linuxInspectionHold struct {
	stopped     map[int]bool
	interrupt   map[int]bool
	synthetic   map[int]bool
	priorResume map[int]bool
	releasing   bool
}

func (b *linuxBackend) inspectionStop(tid int) (StopEvent, bool) {
	for _, stop := range b.parked {
		if stop.TID == tid {
			return stop, true
		}
	}
	for _, result := range b.inspectionReplay {
		if result.tid == tid && result.status.Stopped() &&
			result.status.StopSignal() == syscall.SIGTRAP && result.status.TrapCause() == 0 {
			return StopEvent{Reason: StopBreakpoint, TID: tid}, true
		}
	}
	return StopEvent{}, false
}

// A selected-context hold lasts until a successful primary resume. Real stops
// stay wait-owned and replay through Wait; only EVENT_STOP/SIGTRAP holds are
// released here. No injected signal can survive this transaction into detach.
func (b *linuxBackend) inspectionThreads() ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), linuxInspectionTimeout)
	defer cancel()
	return b.holdInspectionThreads(ctx)
}

func (b *linuxBackend) holdInspectionThreads(ctx context.Context) ([]int, error) {
	if !b.seized && !b.attached() {
		return nil, fmt.Errorf("inspection requires seized Linux tracees")
	}
	if b.stepping || b.stepExitPending {
		return nil, fmt.Errorf("inspection cannot overlap an unresolved hardware step")
	}
	if b.inspection == nil {
		b.inspection = &linuxInspectionHold{
			stopped:     map[int]bool{b.traceTID(): true},
			interrupt:   make(map[int]bool),
			synthetic:   make(map[int]bool),
			priorResume: make(map[int]bool),
		}
		for _, stop := range b.parked {
			b.inspection.stopped[stop.TID] = true
		}
		for tid, state := range b.attachedTracees {
			if state.stopped {
				b.inspection.stopped[tid] = true
				if state.resumeAllowed && tid != b.traceTID() && state.stop.Reason == stopAttachedInternal {
					b.inspection.priorResume[tid] = true
				}
			}
		}
		for _, result := range b.inspectionReplay {
			if result.status.Stopped() {
				b.inspection.stopped[result.tid] = true
			}
		}
	}
	origin := b.traceTID()
	defer b.recordStop(origin)
	stable := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("inspection hold for pid %d: %w; retained holds can be resumed or killed", b.pid, err)
		}
		tids, err := b.Threads()
		if err != nil {
			return nil, fmt.Errorf("inspection enumerate threads: %w", err)
		}
		if len(tids) == 0 || len(tids) > maxThreadScan {
			return nil, fmt.Errorf("inspection thread count %d outside 1..%d", len(tids), maxThreadScan)
		}
		sort.Ints(tids)
		allStopped := true
		for _, tid := range tids {
			if b.inspection.stopped[tid] {
				continue
			}
			allStopped = false
			if b.inspection.interrupt[tid] {
				continue
			}
			tracer, err := b.attachedTracerPID(tid)
			if err != nil {
				return nil, fmt.Errorf("inspection verify tid %d ownership: %w", tid, err)
			}
			if tracer != b.tracer.threadID() {
				return nil, fmt.Errorf("inspection tid %d is not owned by this tracer", tid)
			}
			generation, err := b.waits.register(tid)
			if err != nil {
				return nil, fmt.Errorf("inspection register tid %d: %w", tid, err)
			}
			b.registerAttachedClone(tid, generation)
			var interruptErr error
			b.execPtrace(func() { interruptErr = b.ptraceControl(unix.PTRACE_INTERRUPT, tid, 0, 0) })
			if interruptErr != nil && !errors.Is(interruptErr, syscall.EIO) && !isNoSuchProcess(interruptErr) {
				return nil, fmt.Errorf("inspection interrupt tid %d: %w", tid, interruptErr)
			}
			b.inspection.interrupt[tid] = true
		}
		drained := false
		for {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("inspection drain deadline: %w", err)
			}
			result, ok, err := b.waits.tryNext()
			if !ok {
				if err != nil {
					return nil, fmt.Errorf("%w: inspection drain: %v", ErrSessionInvalidated, err)
				}
				break
			}
			drained = true
			if err := b.recordInspectionResult(result); err != nil {
				return nil, err
			}
		}
		if drained {
			stable = 0
			continue
		}
		if allStopped {
			stable++
			if stable == 2 {
				return tids, nil
			}
			continue
		}
		stable = 0
		result, err := b.waits.next(ctx)
		if err != nil {
			if result.tid != 0 {
				if resultErr := b.recordInspectionResult(result); resultErr != nil {
					return nil, resultErr
				}
				continue
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("inspection wait: %w", err)
			}
			return nil, fmt.Errorf("%w: inspection wait: %v", ErrSessionInvalidated, err)
		}
		if err := b.recordInspectionResult(result); err != nil {
			return nil, err
		}
	}
}

func (b *linuxBackend) recordInspectionResult(result linuxWaitResult) error {
	hold := b.inspection
	if result.err != nil && !result.retired {
		b.inspectionReplay = append(b.inspectionReplay, result)
		return fmt.Errorf("%w: inspection wait result: %v", ErrSessionInvalidated, result.err)
	}
	if result.retired || result.status.Exited() || result.status.Signaled() {
		if result.retired {
			if err := b.recordAttachedRetirement(result.tid, result.generation); err != nil {
				return fmt.Errorf("%w: inspection retirement: %v", ErrSessionInvalidated, err)
			}
		}
		delete(hold.stopped, result.tid)
		delete(hold.synthetic, result.tid)
		delete(hold.interrupt, result.tid)
		delete(hold.priorResume, result.tid)
		if result.tid == b.pid {
			b.inspectionReplay = append(b.inspectionReplay, result)
			if !result.retired && result.status.Exited() {
				return &inspectionEndedError{stop: StopEvent{Reason: StopExited, TID: result.tid, ExitCode: result.status.ExitStatus()}}
			}
			if !result.retired && result.status.Signaled() {
				return &inspectionEndedError{stop: StopEvent{Reason: StopKilled, TID: result.tid}}
			}
			return fmt.Errorf("%w: reporting process exited during inspection", ErrSessionInvalidated)
		}
		if result.retired {
			return nil
		}
	}
	if result.status.Stopped() {
		hold.stopped[result.tid] = true
		delete(hold.interrupt, result.tid)
		b.markAttachedStopped(result.tid, StopEvent{Reason: stopAttachedInternal, TID: result.tid}, false, 0, false)
		if result.status.StopSignal() == syscall.SIGTRAP && result.status.TrapCause() == unix.PTRACE_EVENT_STOP {
			hold.synthetic[result.tid] = true
			if state := b.attachedTracees[result.tid]; state != nil {
				state.initialStopPending = false
			}
			return nil
		}
		if result.status.StopSignal() == syscall.SIGTRAP && result.status.TrapCause() == syscall.PTRACE_EVENT_CLONE {
			if err := b.registerClone(result.tid); err != nil {
				return fmt.Errorf("%w: inspection clone: %v", ErrSessionInvalidated, err)
			}
			result.cloneRegistered = true
		}
	}
	if len(b.inspectionReplay) >= 4*maxThreadScan {
		return fmt.Errorf("%w: inspection stop replay exceeds its bounded capacity", ErrSessionInvalidated)
	}
	b.inspectionReplay = append(b.inspectionReplay, result)
	if result.status.Stopped() && result.status.StopSignal() == syscall.SIGTRAP &&
		result.status.TrapCause() == syscall.PTRACE_EVENT_EXEC {
		b.attachImageGone = true
		return fmt.Errorf("%w: process image replaced while acquiring inspection hold", ErrImageReplaced)
	}
	return nil
}

func (b *linuxBackend) releaseInspectionThreads() error {
	if b.inspection == nil || !b.inspection.releasing {
		return nil
	}
	tids := make([]int, 0, len(b.inspection.synthetic))
	for tid := range b.inspection.synthetic {
		tids = append(tids, tid)
	}
	for tid := range b.inspection.priorResume {
		if !b.inspection.synthetic[tid] {
			tids = append(tids, tid)
		}
	}
	sort.Ints(tids)
	for _, tid := range tids {
		var err error
		if b.inspection.priorResume[tid] {
			err = b.continueTID(tid)
		} else {
			err = b.continueWithoutPendingSignals(tid)
		}
		if err != nil {
			return fmt.Errorf("%w: release inspection hold tid %d: %v", ErrSessionInvalidated, tid, err)
		}
		delete(b.inspection.synthetic, tid)
		delete(b.inspection.priorResume, tid)
		b.markAttachedRunning(tid)
	}
	b.inspection = nil
	return nil
}

func (b *linuxBackend) foldInspectionForDetach() error {
	if b.inspection == nil && len(b.inspectionReplay) == 0 {
		return nil
	}
	for _, result := range b.inspectionReplay {
		if err := b.recordAttachedQuiesceResult(result); err != nil {
			return err
		}
	}
	b.inspectionReplay = nil
	b.inspection = nil
	return nil
}

// TRACEME has no PTRACE_INTERRUPT. Convert only at the launch exec stop, under
// an uncatchable SIGSTOP hold, then verify no target instruction ran. Mid-session
// inspection thereafter uses the same signal-free SEIZE primitive as attach.
func (b *linuxBackend) seizeLaunchedProcess(pid int) error {
	code, sender, err := b.startupSignalInfo(pid)
	if err != nil {
		return err
	}
	if code != 0 || int(sender) != pid {
		return fmt.Errorf("launch exec stop lacks the legacy SI_USER provenance required for stopped seize conversion")
	}
	before, err := b.startupRegisters(pid)
	if err != nil {
		return fmt.Errorf("launch before seize: %w", err)
	}
	var controlErr error
	b.execPtrace(func() {
		controlErr = b.ptraceControl(syscall.PTRACE_DETACH, pid, 0, uintptr(syscall.SIGSTOP))
		if controlErr == nil {
			controlErr = b.ptraceControl(unix.PTRACE_SEIZE, pid, 0, uintptr(linuxPtraceOptions))
		}
		if controlErr == nil {
			controlErr = b.ptraceControl(unix.PTRACE_INTERRUPT, pid, 0, 0)
		}
	})
	if controlErr != nil {
		return fmt.Errorf("launch transfer to PTRACE_SEIZE: %w", controlErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), linuxInspectionTimeout)
	defer cancel()
	for {
		result, err := b.waitAny(ctx)
		if err != nil {
			return fmt.Errorf("launch seize rendezvous: %w", err)
		}
		ws := result.status
		if result.tid != pid || !ws.Stopped() {
			return fmt.Errorf("launch seize unexpected tid %d status %v", result.tid, ws)
		}
		if ws.StopSignal() == syscall.SIGSTOP && int(uint32(ws)>>16) == unix.PTRACE_EVENT_STOP {
			if err := b.requeueSignal(pid, int(syscall.SIGCONT)); err != nil {
				return fmt.Errorf("launch clear owned group stop: %w", err)
			}
			after, err := b.startupRegisters(pid)
			if err != nil {
				return fmt.Errorf("launch after seize: %w", err)
			}
			if after.PC != before.PC || after.SP != before.SP {
				return fmt.Errorf("launch seize changed entry context: before pc=%#x sp=%#x, after pc=%#x sp=%#x",
					before.PC, before.SP, after.PC, after.SP)
			}
			b.seized = true
			b.recordStop(pid)
			return nil
		}
		signal := 0
		if ws.StopSignal() == syscall.SIGSTOP && ws.TrapCause() == -1 {
			signal = int(syscall.SIGSTOP)
		} else if ws.StopSignal() != syscall.SIGTRAP || ws.TrapCause() != unix.PTRACE_EVENT_STOP {
			return fmt.Errorf("launch seize unexpected rendezvous status %v", ws)
		}
		b.execPtrace(func() { controlErr = b.ptraceCont(pid, signal) })
		if controlErr != nil {
			return fmt.Errorf("launch seize rendezvous resume: %w", controlErr)
		}
	}
}

func (b *linuxBackend) startupSignalInfo(pid int) (int32, int32, error) {
	if b.startupSignalInfoFn != nil {
		return b.startupSignalInfoFn(pid)
	}
	info := struct {
		Signo, Errno, Code, Padding int32
		PID                         int32
		Rest                        [108]byte
	}{}
	var err error
	b.execPtrace(func() {
		err = b.ptraceControl(syscall.PTRACE_GETSIGINFO, pid, 0, uintptr(unsafe.Pointer(&info)))
	})
	if err != nil {
		return 0, 0, fmt.Errorf("launch exec stop provenance: %w", err)
	}
	if info.Signo != int32(syscall.SIGTRAP) {
		return 0, 0, fmt.Errorf("launch exec stop is not a SIGTRAP signal-delivery stop")
	}
	return info.Code, info.PID, nil
}

func (b *linuxBackend) startupRegisters(pid int) (Registers, error) {
	if b.startupRegistersFn != nil {
		return b.startupRegistersFn(pid)
	}
	return b.GetRegisters(pid)
}
