//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	linuxCloneRecoveryTimeout = 5 * time.Second
	linuxCloneCensusLimit     = 32768
)

type linuxCloneReadError struct {
	parentTID int
	cause     error
}

func (e *linuxCloneReadError) Error() string {
	return fmt.Sprintf("%v: read cloned tid from parent %d: %v",
		ErrSessionInvalidated, e.parentTID, e.cause)
}

func (e *linuxCloneReadError) Unwrap() []error {
	return []error{ErrSessionInvalidated, e.cause}
}

type linuxCloneRecovery struct {
	cause    error
	deadline time.Time
	known    map[int]uint64
}

// Fatal group teardown can invalidate an already-observed clone stop before
// GETEVENTMSG. ESRCH is not death evidence: recover ownership first, then keep
// the original error unless a real leader terminal arrives within the deadline.
func (b *linuxBackend) recoverCloneRead(ctx context.Context, cause error) error {
	var readErr *linuxCloneReadError
	if !errors.As(cause, &readErr) || !errors.Is(readErr.cause, syscall.ESRCH) ||
		b.attached() || b.tracer == nil || b.tracer.closed() {
		return cause
	}
	if b.cloneRecovery == nil {
		b.cloneRecovery = &linuxCloneRecovery{
			cause: cause, deadline: time.Now().Add(linuxCloneRecoveryTimeout),
			known: b.waits.knownTIDs(),
		}
	}
	recovery := b.cloneRecovery
	scanCtx, cancel := context.WithDeadline(ctx, recovery.deadline)
	defer cancel()
	for {
		err := b.discoverCloneTIDs(scanCtx)
		if !errors.Is(err, errLinuxDiscoveryRetired) {
			return err
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-scanCtx.Done():
			timer.Stop()
			return fmt.Errorf("%w: reconcile retired clone identity: %w", cause, scanCtx.Err())
		case <-timer.C:
		}
	}
}

func (b *linuxBackend) discoverCloneTIDs(ctx context.Context) error {
	tids, err := b.tracedTIDs(ctx)
	if err != nil {
		return fmt.Errorf("%w: discover clone ownership: %w", b.cloneRecovery.cause, err)
	}
	for _, tid := range tids {
		generation, err := b.waits.discover(tid, b.cloneRecovery.known[tid])
		if err != nil {
			return fmt.Errorf("%w: register discovered tid %d: %w", b.cloneRecovery.cause, tid, err)
		}
		b.cloneRecovery.known[tid] = generation
	}
	return nil
}

func (b *linuxBackend) registerRetiringThreadGroup() error {
	if b.cloneRecovery != nil {
		// The tracer census already admitted every unreported clone. An
		// ordinary group rescan could erase a terminal the broker just queued.
		return nil
	}
	return b.registerThreadGroup()
}

func (b *linuxBackend) waitWithCloneRecovery(ctx context.Context) (linuxWaitResult, error) {
	if b.cloneRecovery == nil {
		return b.waitAny(ctx)
	}
	bounded, cancel := context.WithDeadline(ctx, b.cloneRecovery.deadline)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return linuxWaitResult{}, fmt.Errorf("%w: await real leader terminal: %w", b.cloneRecovery.cause, err)
	}
	result, err := b.waitAny(bounded)
	if errors.Is(err, context.DeadlineExceeded) {
		return result, fmt.Errorf("%w: await real leader terminal: %w", b.cloneRecovery.cause, err)
	}
	return result, err
}

func (b *linuxBackend) tracedTIDs(ctx context.Context) ([]int, error) {
	if b.tracedTIDsFn != nil {
		return b.tracedTIDsFn(ctx)
	}
	return linuxTracerTIDs(ctx, "/proc", b.tracer.threadID())
}

// A clone need not share the parent's thread group. Enumerate the exact tracer's
// tasks rather than guessing the missing child from /proc/<leader>/task. No
// ptrace operation or wait syscall runs here; the existing broker owns reaping.
func linuxTracerTIDs(ctx context.Context, proc string, tracerTID int) ([]int, error) {
	if tracerTID <= 0 {
		return nil, fmt.Errorf("clone census requires a live tracer thread")
	}
	processes, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	var tids []int
	scanned := 0
	for _, process := range processes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := strconv.Atoi(process.Name()); err != nil {
			continue
		}
		taskDir := filepath.Join(proc, process.Name(), "task")
		tasks, err := linuxCloneProcessTasks(proc, process.Name())
		if err != nil {
			return nil, err
		}
		for _, task := range tasks {
			scanned++
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if scanned > linuxCloneCensusLimit {
				return nil, fmt.Errorf("clone census exceeds %d tasks", linuxCloneCensusLimit)
			}
			tid, err := strconv.Atoi(task.Name())
			if err != nil || tid <= 0 {
				return nil, fmt.Errorf("invalid task name %q", task.Name())
			}
			owner, err := linuxStatusTracerPID(filepath.Join(taskDir, task.Name(), "status"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if owner == tracerTID {
				tids = append(tids, tid)
			}
		}
	}
	return tids, nil
}

func linuxCloneProcessTasks(proc, pid string) ([]os.DirEntry, error) {
	taskDir := filepath.Join(proc, pid, "task")
	tasks, err := os.ReadDir(taskDir)
	if !errors.Is(err, os.ErrNotExist) {
		return tasks, err
	}
	// A retired group leader can hide /proc/<pid>/task while siblings live.
	// Only disappearance of the group itself permits skipping that entry.
	if _, err := os.Stat(filepath.Join(proc, pid)); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("clone census cannot enumerate %s", taskDir)
}
