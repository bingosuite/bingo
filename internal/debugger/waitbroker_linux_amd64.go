//go:build linux && amd64

package debugger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const linuxWaitFallbackInterval = time.Second

type linuxWaitResult struct {
	tid        int
	generation uint64
	status     syscall.WaitStatus
	retired    bool
	err        error
}

type linuxWaitSource interface {
	register(tid int) (uint64, error)
	release(tid int, generation uint64) bool
	next(context.Context) (linuxWaitResult, error)
	tryNext() (linuxWaitResult, bool, error)
	close()
}

type linuxWait4Func func(pid int, status *syscall.WaitStatus, options int, rusage *syscall.Rusage) (int, error)

type linuxWaitRegistration struct {
	owner      *linuxWaitOwner
	generation uint64
}

type linuxWaitScan struct {
	tid int
	reg linuxWaitRegistration
}

// linuxWaitBroker is the process-wide owner of Linux wait syscalls. SIGCHLD
// triggers exact registered-TID scans, so one debugger can never consume
// another debugger's stop or the status of an unrelated child.
type linuxWaitBroker struct {
	mu             sync.Mutex
	registrations  map[int]linuxWaitRegistration
	nextGeneration atomic.Uint64
	wait4          linuxWait4Func
	wake           chan struct{}
	sigchld        chan os.Signal
}

type linuxWaitOwner struct {
	broker *linuxWaitBroker

	mu         sync.Mutex
	registered map[int]uint64
	queue      []linuxWaitResult
	closed     bool
	notify     chan struct{}
}

var processLinuxWaitBroker = newLinuxWaitBroker(syscall.Wait4)

func newLinuxWaitBroker(wait4 linuxWait4Func) *linuxWaitBroker {
	return newLinuxWaitBrokerWithRunner(wait4, true)
}

func newLinuxWaitBrokerWithRunner(wait4 linuxWait4Func, run bool) *linuxWaitBroker {
	b := &linuxWaitBroker{
		registrations: make(map[int]linuxWaitRegistration),
		wait4:         wait4,
		wake:          make(chan struct{}, 1),
		sigchld:       make(chan os.Signal, 1),
	}
	if run {
		signal.Notify(b.sigchld, syscall.SIGCHLD)
		go b.run()
	}
	return b
}

func (b *linuxWaitBroker) newOwner() *linuxWaitOwner {
	return &linuxWaitOwner{
		broker:     b,
		registered: make(map[int]uint64),
		notify:     make(chan struct{}, 1),
	}
}

func (o *linuxWaitOwner) register(tid int) (uint64, error) {
	if tid <= 0 {
		return 0, fmt.Errorf("invalid tid %d", tid)
	}

	b := o.broker
	b.mu.Lock()
	defer b.mu.Unlock()

	if current, ok := b.registrations[tid]; ok {
		if current.owner == o {
			return current.generation, nil
		}
		current.owner.mu.Lock()
		stale := current.owner.closed
		if stale {
			delete(current.owner.registered, tid)
			current.owner.signalLocked()
		}
		current.owner.mu.Unlock()
		if !stale {
			return 0, fmt.Errorf("tid %d is already owned by another debugger", tid)
		}
	}

	generation := b.nextGeneration.Add(1)
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return 0, fmt.Errorf("wait owner is closed")
	}
	o.removeQueuedTIDLocked(tid, 0)
	o.registered[tid] = generation
	o.signalLocked()
	o.mu.Unlock()
	b.registrations[tid] = linuxWaitRegistration{owner: o, generation: generation}
	b.signal()
	return generation, nil
}

func (o *linuxWaitOwner) next(ctx context.Context) (linuxWaitResult, error) {
	for {
		o.mu.Lock()
		switch {
		case len(o.queue) > 0:
			result := o.popLocked()
			o.mu.Unlock()
			if result.err != nil {
				return linuxWaitResult{}, result.err
			}
			return result, nil
		case o.closed:
			o.mu.Unlock()
			return linuxWaitResult{}, syscall.ECHILD
		case len(o.registered) == 0:
			o.mu.Unlock()
			return linuxWaitResult{}, syscall.ECHILD
		}
		o.mu.Unlock()

		select {
		case <-ctx.Done():
			return linuxWaitResult{}, ctx.Err()
		case <-o.notify:
		}
	}
}

func (o *linuxWaitOwner) tryNext() (linuxWaitResult, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case len(o.queue) > 0:
		result := o.popLocked()
		if result.err != nil {
			return linuxWaitResult{}, false, result.err
		}
		return result, true, nil
	case o.closed, len(o.registered) == 0:
		return linuxWaitResult{}, false, syscall.ECHILD
	default:
		return linuxWaitResult{}, false, nil
	}
}

func (o *linuxWaitOwner) release(tid int, generation uint64) bool {
	b := o.broker
	b.mu.Lock()
	defer b.mu.Unlock()

	current, ok := b.registrations[tid]
	if !ok || current.owner != o || current.generation != generation {
		return false
	}
	delete(b.registrations, tid)

	o.mu.Lock()
	if o.registered[tid] == generation {
		delete(o.registered, tid)
	}
	o.removeQueuedTIDLocked(tid, generation)
	o.signalLocked()
	o.mu.Unlock()
	b.signal()
	return true
}

func (o *linuxWaitOwner) popLocked() linuxWaitResult {
	result := o.queue[0]
	o.queue[0] = linuxWaitResult{}
	o.queue = o.queue[1:]
	return result
}

func (o *linuxWaitOwner) removeQueuedTIDLocked(tid int, generation uint64) {
	filtered := o.queue[:0]
	for _, result := range o.queue {
		if result.tid == tid && (generation == 0 || result.generation == generation) {
			continue
		}
		filtered = append(filtered, result)
	}
	clear(o.queue[len(filtered):])
	o.queue = filtered
}

func (o *linuxWaitOwner) close() {
	o.mu.Lock()
	o.closed = true
	o.queue = nil
	o.signalLocked()
	o.mu.Unlock()
	o.broker.signal()
}

func (o *linuxWaitOwner) signalLocked() {
	select {
	case o.notify <- struct{}{}:
	default:
	}
}

func (b *linuxWaitBroker) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *linuxWaitBroker) run() {
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		if b.scan() {
			continue
		}

		if b.registrationCount() == 0 {
			<-b.wake
			continue
		}

		timer.Reset(linuxWaitFallbackInterval)
		select {
		case <-b.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-b.sigchld:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (b *linuxWaitBroker) registrationCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.registrations)
}

func (b *linuxWaitBroker) snapshot() []linuxWaitScan {
	b.mu.Lock()
	defer b.mu.Unlock()

	snapshot := make([]linuxWaitScan, 0, len(b.registrations))
	for tid, reg := range b.registrations {
		snapshot = append(snapshot, linuxWaitScan{tid: tid, reg: reg})
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].tid < snapshot[j].tid })
	return snapshot
}

// scan is separated from run so deterministic tests can drive one broker pass
// without timing against its polling goroutine.
func (b *linuxWaitBroker) scan() bool {
	progressed := false
	for _, item := range b.snapshot() {
		progressed = b.scanRegistration(item) || progressed
	}
	return progressed
}

func (b *linuxWaitBroker) scanRegistration(item linuxWaitScan) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	current, ok := b.registrations[item.tid]
	if !ok || current != item.reg {
		return false
	}

	// The exact wait is nonblocking. Keeping registration validation and status
	// consumption in one critical section prevents detach recovery from replacing
	// this TID's generation after validation but before wait4 consumes its stop.
	var status syscall.WaitStatus
	tid, err := b.wait4(item.tid, &status, syscall.WNOHANG|syscall.WALL, nil)
	switch {
	case err == nil && tid == 0:
		return false
	case err == nil:
		b.deliverLocked(item.tid, current, linuxWaitResult{
			tid: tid, generation: current.generation, status: status,
		}, status.Exited() || status.Signaled())
	case errors.Is(err, syscall.EINTR):
	case errors.Is(err, syscall.ECHILD):
		b.retireLocked(item.tid, current)
	default:
		b.deliverLocked(item.tid, current, linuxWaitResult{
			tid:        item.tid,
			generation: current.generation,
			err:        fmt.Errorf("wait4 tid %d: %w", item.tid, err),
		}, true)
	}
	return true
}

func (b *linuxWaitBroker) deliverLocked(tid int, current linuxWaitRegistration, result linuxWaitResult, terminal bool) {
	if terminal {
		delete(b.registrations, tid)
	}

	owner := current.owner
	owner.mu.Lock()
	if terminal && owner.registered[tid] == current.generation {
		delete(owner.registered, tid)
	}
	if !owner.closed {
		owner.queue = append(owner.queue, result)
	}
	owner.signalLocked()
	owner.mu.Unlock()
}

func (b *linuxWaitBroker) retire(tid int, expected linuxWaitRegistration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current, ok := b.registrations[tid]
	if !ok || current != expected {
		return
	}
	b.retireLocked(tid, current)
}

func (b *linuxWaitBroker) retireLocked(tid int, current linuxWaitRegistration) {
	delete(b.registrations, tid)

	owner := current.owner
	owner.mu.Lock()
	if owner.registered[tid] == current.generation {
		delete(owner.registered, tid)
	}
	if !owner.closed {
		owner.queue = append(owner.queue, linuxWaitResult{
			tid:        tid,
			generation: current.generation,
			retired:    true,
		})
	}
	owner.signalLocked()
	owner.mu.Unlock()
}
