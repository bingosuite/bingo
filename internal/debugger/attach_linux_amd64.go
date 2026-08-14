//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const linuxAttachTimeout = 5 * time.Second

const stopAttachedInternal StopReason = 255

type linuxTracee struct {
	generation         uint64
	stopped            bool
	resumeAllowed      bool
	interruptPending   bool
	initialStopPending bool
	stop               StopEvent
	signalDelivery     bool
	groupStopSignal    int
}

func attachToProcess(backend Backend, pid int) (retErr error) {
	b, ok := backend.(*linuxBackend)
	if !ok || b.tracer == nil || b.waits == nil {
		return fmt.Errorf("attachToProcess: backend does not support Linux wait ownership")
	}

	b.setPID(pid)
	b.attachedTracees = make(map[int]*linuxTracee)
	b.attachCleanup = true
	b.attachGone = false
	b.attachImageGone = false
	ctx, cancel := context.WithTimeout(context.Background(), linuxAttachTimeout)
	defer cancel()

	defer func() {
		if retErr == nil {
			b.attachCleanup = false
			return
		}
		_, quiesceErr := b.quiesceAttached(ctx)
		detachErr := b.detachAttachedWithContext(ctx)
		if cleanupErr := errors.Join(quiesceErr, detachErr); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("discard failed attach: %w", cleanupErr))
		}
		if len(b.attachedTracees) == 0 {
			b.attachedTracees = nil
			b.setPID(0)
		}
	}()

	if _, err := b.quiesceAttached(ctx); err != nil {
		return fmt.Errorf("attach pid %d: %w", pid, err)
	}
	if b.attachGone {
		return fmt.Errorf("attach pid %d: process exited while attaching", pid)
	}
	return nil
}

func (b *linuxBackend) attached() bool {
	return b.attachedTracees != nil
}

func (b *linuxBackend) retainsAttachedOwnership() bool {
	return b.attached()
}

func (b *linuxBackend) registerAttachedClone(tid int, generation uint64) {
	if !b.attached() {
		return
	}
	if state := b.attachedTracees[tid]; state != nil {
		state.generation = generation
		return
	}
	b.attachedTracees[tid] = &linuxTracee{
		generation:         generation,
		initialStopPending: true,
	}
}

func (b *linuxBackend) reconcileAttachedTracees() (int, error) {
	if !b.attached() {
		return 0, nil
	}
	if b.tracer == nil || b.tracer.closed() {
		return 0, fmt.Errorf("attach tracer thread is closed")
	}
	tids, err := b.Threads()
	if err != nil {
		if isNoSuchProcess(err) {
			if len(b.attachedTracees) == 0 {
				b.attachGone = true
			}
			return 0, nil
		}
		return 0, err
	}
	sort.Ints(tids)

	added := 0
	for _, tid := range tids {
		if _, ok := b.attachedTracees[tid]; ok {
			continue
		}
		var seizeErr error
		b.execPtrace(func() {
			seizeErr = b.ptraceControl(
				unix.PTRACE_SEIZE,
				tid,
				0,
				uintptr(linuxPtraceOptions),
			)
		})
		if errors.Is(seizeErr, syscall.EPERM) {
			tracerPID, err := b.attachedTracerPID(tid)
			if err != nil {
				return added, fmt.Errorf("verify PTRACE_SEIZE owner for tid %d: %w", tid, err)
			}
			if tracerPID != b.tracer.threadID() {
				return added, fmt.Errorf("PTRACE_SEIZE tid %d: already traced by pid %d", tid, tracerPID)
			}
			generation, err := b.waits.register(tid)
			if err != nil {
				return added, fmt.Errorf("register auto-attached tid %d: %w", tid, err)
			}
			b.attachedTracees[tid] = &linuxTracee{
				generation:         generation,
				initialStopPending: true,
			}
			added++
			continue
		}
		if isNoSuchProcess(seizeErr) {
			continue
		}
		if seizeErr != nil {
			return added, fmt.Errorf("PTRACE_SEIZE tid %d: %w", tid, seizeErr)
		}
		generation, err := b.waits.register(tid)
		if err != nil {
			return added, fmt.Errorf("register seized tid %d: %w", tid, err)
		}
		b.attachedTracees[tid] = &linuxTracee{generation: generation}
		added++
	}
	return added, nil
}

func (b *linuxBackend) attachedTracerPID(tid int) (int, error) {
	if b.tracerPIDFn != nil {
		return b.tracerPIDFn(tid)
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/status", b.pid, tid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "TracerPid:") {
			continue
		}
		return strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "TracerPid:")))
	}
	return 0, fmt.Errorf("TracerPid missing")
}

func (b *linuxBackend) interruptAttachedTracees() error {
	if b.tracer == nil || b.tracer.closed() {
		return fmt.Errorf("attach tracer thread is closed")
	}
	for _, tid := range b.attachedTIDs() {
		state := b.attachedTracees[tid]
		if state.stopped || state.interruptPending {
			continue
		}
		var interruptErr error
		b.execPtrace(func() {
			interruptErr = b.ptraceControl(unix.PTRACE_INTERRUPT, tid, 0, 0)
		})
		switch {
		case interruptErr == nil:
			state.interruptPending = true
		case errors.Is(interruptErr, syscall.EIO):
			// A stop already queued with the broker needs no second interrupt.
		case isNoSuchProcess(interruptErr):
			// The wait owner still owns the terminal status or exact ECHILD.
		default:
			return fmt.Errorf("PTRACE_INTERRUPT tid %d: %w", tid, interruptErr)
		}
	}
	return nil
}

func (b *linuxBackend) quiesceAttached(ctx context.Context) (bool, error) {
	if !b.attached() {
		return b.attachGone, nil
	}
	b.attachCleanup = true
	b.collectStepQueueForDetach()
	stablePasses := 0

	for {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("quiesce attached pid %d: %w", b.pid, err)
		}
		added, err := b.reconcileAttachedTracees()
		if err != nil {
			return false, fmt.Errorf("discover attached threads: %w", err)
		}
		if err := b.interruptAttachedTracees(); err != nil {
			return false, err
		}

		drained := false
		for {
			result, ok, err := b.waits.tryNext()
			if err != nil {
				if isNoChildProcess(err) && len(b.attachedTracees) == 0 {
					break
				}
				return false, fmt.Errorf("drain attached stops: %w", err)
			}
			if !ok {
				break
			}
			drained = true
			if err := b.recordAttachedQuiesceResult(result); err != nil {
				return false, err
			}
		}
		if drained {
			stablePasses = 0
			continue
		}
		if b.attachGone && len(b.attachedTracees) == 0 {
			return true, nil
		}
		if added == 0 && b.allAttachedStopped() {
			stablePasses++
			if stablePasses >= 2 {
				b.collectStepQueueForDetach()
				return false, nil
			}
			continue
		} else {
			stablePasses = 0
		}

		result, err := b.waits.next(ctx)
		if err != nil {
			if isNoChildProcess(err) && len(b.attachedTracees) == 0 {
				if !b.attachedProcessExists() {
					b.attachGone = true
					return true, nil
				}
			}
			return false, fmt.Errorf("wait for attached quiesce: %w", err)
		}
		if err := b.recordAttachedQuiesceResult(result); err != nil {
			return false, err
		}
	}
}

func (b *linuxBackend) recordAttachedQuiesceResult(result linuxWaitResult) error {
	if result.retired {
		return b.recordAttachedRetirement(result.tid, result.generation)
	}
	tid := result.tid
	ws := result.status
	if ws.Exited() || ws.Signaled() {
		b.removeAttachedTracee(tid)
		if tid == b.pid {
			b.attachGone = true
		}
		return nil
	}
	if !ws.Stopped() {
		return nil
	}

	sig := ws.StopSignal()
	if sig == syscall.SIGTRAP {
		switch cause := ws.TrapCause(); cause {
		case syscall.PTRACE_EVENT_CLONE:
			child, err := b.eventMsg(tid)
			if err != nil {
				return fmt.Errorf("read cloned tid from quiesced parent %d: %w", tid, err)
			}
			generation, err := b.waits.register(int(child))
			if err != nil {
				return fmt.Errorf("register quiesced clone tid %d: %w", child, err)
			}
			b.registerAttachedClone(int(child), generation)
			b.markAttachedStopped(tid, StopEvent{Reason: stopAttachedInternal, TID: tid}, false, 0, true)
			return nil
		case syscall.PTRACE_EVENT_EXEC:
			b.attachImageGone = true
			b.retractLeaderExit()
			b.markAttachedStopped(tid, StopEvent{Reason: stopAttachedInternal, TID: tid}, false, 0, true)
			return nil
		case syscall.PTRACE_EVENT_EXIT:
			b.markAttachedStopped(tid, StopEvent{Reason: stopAttachedInternal, TID: tid}, false, 0, true)
			return nil
		case unix.PTRACE_EVENT_STOP:
			state := b.attachedTracees[tid]
			if state != nil {
				state.initialStopPending = false
			}
			b.markAttachedStopped(tid, StopEvent{Reason: stopAttachedInternal, TID: tid}, false, 0, true)
			return nil
		case 0:
			reason, _ := classifyUserStop(true, b.stepping, b.stepTID, tid)
			b.markAttachedStopped(tid, StopEvent{Reason: reason, TID: tid}, false, 0, true)
			return nil
		default:
			b.markAttachedStopped(tid, StopEvent{Reason: stopAttachedInternal, TID: tid}, false, 0, true)
			return nil
		}
	}

	if int(uint32(ws)>>16) == unix.PTRACE_EVENT_STOP {
		b.markAttachedStopped(tid, StopEvent{Reason: StopSignal, TID: tid, Signal: int(sig)}, false, int(sig), true)
		return nil
	}
	event := StopEvent{Reason: StopSignal, TID: tid, Signal: int(sig)}
	b.markAttachedStopped(tid, event, true, 0, true)
	if event.Signal != b.PauseSignal() {
		b.pendingSignals.set(tid, event.Signal)
	}
	return nil
}

func (b *linuxBackend) recordAttachedRetirement(tid int, generation uint64) error {
	state := b.attachedTracees[tid]
	if state == nil || state.generation != generation {
		return nil
	}
	if b.attachedTIDExists(tid) {
		return fmt.Errorf("wait ownership retired for live attached tid %d generation %d", tid, generation)
	}
	delete(b.attachedTracees, tid)
	if tid == b.pid && !b.attachedProcessExists() {
		b.attachGone = true
	}
	return nil
}

func (b *linuxBackend) collectStepQueueForDetach() {
	if !b.attached() {
		return
	}
	for _, event := range b.parked {
		b.markAttachedStopped(event.TID, event, event.Reason == StopSignal, 0, false)
	}
	b.parked = nil
	if tid, ok := b.heldStepOwner(); ok {
		b.markAttachedStopped(tid, StopEvent{Reason: StopStepThreadExited, TID: tid}, false, 0, false)
		b.clearHeldStepOwner()
	}
	b.endStep()
	b.stepQueue.completeStepThreadExit()
}

func (b *linuxBackend) markAttachedStopped(
	tid int,
	event StopEvent,
	signalDelivery bool,
	groupStopSignal int,
	resumeAllowed bool,
) {
	if !b.attached() || tid == 0 {
		return
	}
	state := b.attachedTracees[tid]
	if state == nil {
		return
	}
	state.stopped = true
	state.resumeAllowed = resumeAllowed
	state.interruptPending = false
	state.stop = event
	state.signalDelivery = signalDelivery
	state.groupStopSignal = groupStopSignal
	if signalDelivery && event.Reason == StopSignal && event.Signal != b.PauseSignal() {
		b.pendingSignals.set(tid, event.Signal)
	}
	b.recordStop(tid)
}

func (b *linuxBackend) markAttachedRunning(tid int) {
	if !b.attached() {
		return
	}
	state := b.attachedTracees[tid]
	if state == nil {
		return
	}
	state.stopped = false
	state.resumeAllowed = false
	state.interruptPending = false
	state.stop = StopEvent{}
	state.signalDelivery = false
	state.groupStopSignal = 0
}

func (b *linuxBackend) removeAttachedTracee(tid int) {
	if !b.attached() {
		return
	}
	delete(b.attachedTracees, tid)
	b.pendingSignals.clear(tid)
}

func (b *linuxBackend) allAttachedStopped() bool {
	if len(b.attachedTracees) == 0 {
		return false
	}
	for _, state := range b.attachedTracees {
		if !state.stopped {
			return false
		}
	}
	return true
}

func (b *linuxBackend) attachedTIDs() []int {
	tids := make([]int, 0, len(b.attachedTracees))
	for tid := range b.attachedTracees {
		tids = append(tids, tid)
	}
	sort.Ints(tids)
	return tids
}

func (b *linuxBackend) attachedDetachStops() []StopEvent {
	stops := make([]StopEvent, 0, len(b.attachedTracees))
	for _, tid := range b.attachedTIDs() {
		if stop := b.attachedTracees[tid].stop; stop.Reason == StopBreakpoint {
			stops = append(stops, stop)
		}
	}
	return stops
}

func (b *linuxBackend) attachedQuiesced() bool {
	return (b.attachGone && len(b.attachedTracees) == 0) || b.allAttachedStopped()
}

func (b *linuxBackend) attachedImageReplaced() bool {
	return b.attachImageGone
}

func (b *linuxBackend) selectAttachedWriteTID() (int, error) {
	if state := b.attachedTracees[b.pid]; state != nil && state.stopped {
		b.recordStop(b.pid)
		return b.pid, nil
	}
	for _, tid := range b.attachedTIDs() {
		if b.attachedTracees[tid].stopped {
			b.recordStop(tid)
			return tid, nil
		}
	}
	return 0, fmt.Errorf("attached pid %d has no stopped memory-write anchor", b.pid)
}

func (b *linuxBackend) attachedProcessExists() bool {
	if b.processExistsFn != nil {
		return b.processExistsFn()
	}
	if b.pid == 0 {
		return false
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d/task", b.pid))
	return err == nil
}

func (b *linuxBackend) attachedTIDExists(tid int) bool {
	if b.tidExistsFn != nil {
		return b.tidExistsFn(tid)
	}
	_, err := os.Stat(fmt.Sprintf("/proc/%d/task/%d", b.pid, tid))
	return err == nil
}

func (b *linuxBackend) continueAttached() error {
	if b.attachCleanup {
		return fmt.Errorf("attached detach cleanup is pending; retry Kill")
	}
	resumed := false
	for _, tid := range b.attachedTIDs() {
		state := b.attachedTracees[tid]
		if !state.stopped || !state.resumeAllowed {
			continue
		}
		if err := b.continueTID(tid); err != nil {
			return err
		}
		b.markAttachedRunning(tid)
		resumed = true
	}
	if !resumed {
		return fmt.Errorf("PTRACE_CONT: no resumable attached thread")
	}
	return nil
}

func (b *linuxBackend) detachAttached() error {
	ctx, cancel := context.WithTimeout(context.Background(), linuxAttachTimeout)
	defer cancel()
	return b.detachAttachedWithContext(ctx)
}

func (b *linuxBackend) detachAttachedWithContext(ctx context.Context) error {
	if !b.attached() {
		return nil
	}
	if b.attachGone && len(b.attachedTracees) == 0 {
		b.attachedTracees = nil
		b.attachCleanup = false
		b.pendingSignals.purge()
		b.setPID(0)
		return nil
	}
	if !b.allAttachedStopped() {
		return fmt.Errorf("detach attached pid %d: not every owned TID is stopped", b.pid)
	}

	tids := detachOrder(b.attachedTIDs(), b.pid)
	var detachErr error
	for _, tid := range tids {
		state := b.attachedTracees[tid]
		if state == nil {
			continue
		}
		if err := b.detachAttachedTID(tid, state); err != nil {
			detachErr = errors.Join(detachErr, err)
			continue
		}
		b.waits.release(tid, state.generation)
		delete(b.attachedTracees, tid)
	}
	if detachErr != nil {
		_, recoveryErr := b.quiesceAttached(ctx)
		return errors.Join(detachErr, recoveryErr)
	}

	b.attachedTracees = nil
	b.attachCleanup = false
	b.pendingSignals.purge()
	b.setPID(0)
	return nil
}

func (b *linuxBackend) detachAttachedTID(tid int, state *linuxTracee) error {
	if b.tracer == nil || b.tracer.closed() {
		return fmt.Errorf("PTRACE_DETACH tid %d: tracer thread is closed", tid)
	}
	batch := b.pendingSignals.takeForContinue(tid)
	deferred := append([]int(nil), batch.deferred...)
	detachSignal := 0
	if state.signalDelivery && batch.current != 0 && state.stop.Signal == batch.current {
		detachSignal = batch.current
	} else if batch.current != 0 {
		deferred = append([]int{batch.current}, deferred...)
	}
	if state.groupStopSignal != 0 && !containsSignal(deferred, state.groupStopSignal) {
		deferred = append(deferred, state.groupStopSignal)
	}

	requeued, err := b.requeueSignals(tid, deferred)
	if err != nil {
		restore := pendingSignalBatch{deferred: append([]int(nil), deferred[requeued:]...)}
		if detachSignal != 0 {
			restore.current = detachSignal
		}
		b.pendingSignals.restore(tid, restore)
		return err
	}
	state.groupStopSignal = 0

	var detachErr error
	b.execPtrace(func() {
		detachErr = b.ptraceControl(syscall.PTRACE_DETACH, tid, 0, uintptr(detachSignal))
	})
	if detachErr != nil {
		b.pendingSignals.restore(tid, pendingSignalBatch{current: detachSignal})
		return fmt.Errorf("PTRACE_DETACH tid %d signal %d: %w", tid, detachSignal, detachErr)
	}
	return nil
}

func detachOrder(tids []int, leader int) []int {
	ordered := append([]int(nil), tids...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i] == leader {
			return false
		}
		if ordered[j] == leader {
			return true
		}
		return ordered[i] < ordered[j]
	})
	return ordered
}

func (b *linuxBackend) ptraceControl(request int, tid int, addr, data uintptr) error {
	call := syscall.Syscall6
	if b.ptraceSyscall6Fn != nil {
		call = b.ptraceSyscall6Fn
	}
	_, _, errno := call(
		syscall.SYS_PTRACE,
		uintptr(request),
		uintptr(tid),
		addr,
		data,
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
