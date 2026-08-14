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
	tid    int
	status syscall.WaitStatus
	err    error
}

type linuxWaitSource interface {
	register(tid int) error
	next(context.Context) (linuxWaitResult, error)
	close()
}

type linuxWait4Func func(pid int, status *syscall.WaitStatus, options int, rusage *syscall.Rusage) (int, error)

type linuxWaitRegistration struct {
	owner      *linuxWaitOwner
	generation uint64
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

func (o *linuxWaitOwner) register(tid int) error {
	if tid <= 0 {
		return fmt.Errorf("invalid tid %d", tid)
	}

	b := o.broker
	b.mu.Lock()
	defer b.mu.Unlock()

	if current, ok := b.registrations[tid]; ok {
		if current.owner == o {
			return nil
		}
		current.owner.mu.Lock()
		stale := current.owner.closed
		if stale {
			delete(current.owner.registered, tid)
			current.owner.signalLocked()
		}
		current.owner.mu.Unlock()
		if !stale {
			return fmt.Errorf("tid %d is already owned by another debugger", tid)
		}
	}

	generation := b.nextGeneration.Add(1)
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return fmt.Errorf("wait owner is closed")
	}
	o.registered[tid] = generation
	o.signalLocked()
	o.mu.Unlock()
	b.registrations[tid] = linuxWaitRegistration{owner: o, generation: generation}
	b.signal()
	return nil
}

func (o *linuxWaitOwner) next(ctx context.Context) (linuxWaitResult, error) {
	for {
		o.mu.Lock()
		switch {
		case len(o.queue) > 0:
			result := o.queue[0]
			o.queue[0] = linuxWaitResult{}
			o.queue = o.queue[1:]
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

func (b *linuxWaitBroker) snapshot() []struct {
	tid int
	reg linuxWaitRegistration
} {
	b.mu.Lock()
	defer b.mu.Unlock()

	snapshot := make([]struct {
		tid int
		reg linuxWaitRegistration
	}, 0, len(b.registrations))
	for tid, reg := range b.registrations {
		snapshot = append(snapshot, struct {
			tid int
			reg linuxWaitRegistration
		}{tid: tid, reg: reg})
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].tid < snapshot[j].tid })
	return snapshot
}

// scan is separated from run so deterministic tests can drive one broker pass
// without timing against its polling goroutine.
func (b *linuxWaitBroker) scan() bool {
	progressed := false
	for _, item := range b.snapshot() {
		var status syscall.WaitStatus
		tid, err := b.wait4(item.tid, &status, syscall.WNOHANG|syscall.WALL, nil)
		switch {
		case err == nil && tid == 0:
			continue
		case err == nil:
			progressed = true
			b.deliver(item.tid, item.reg, linuxWaitResult{tid: tid, status: status},
				status.Exited() || status.Signaled())
		case errors.Is(err, syscall.EINTR):
			progressed = true
		case errors.Is(err, syscall.ECHILD):
			progressed = true
			b.retire(item.tid, item.reg)
		default:
			progressed = true
			b.deliver(item.tid, item.reg, linuxWaitResult{
				tid: item.tid,
				err: fmt.Errorf("wait4 tid %d: %w", item.tid, err),
			}, true)
		}
	}
	return progressed
}

func (b *linuxWaitBroker) deliver(tid int, expected linuxWaitRegistration, result linuxWaitResult, terminal bool) {
	b.mu.Lock()
	current, ok := b.registrations[tid]
	if !ok || current != expected {
		b.mu.Unlock()
		return
	}
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
	b.mu.Unlock()
}

func (b *linuxWaitBroker) retire(tid int, expected linuxWaitRegistration) {
	b.mu.Lock()
	current, ok := b.registrations[tid]
	if !ok || current != expected {
		b.mu.Unlock()
		return
	}
	delete(b.registrations, tid)

	owner := current.owner
	owner.mu.Lock()
	if owner.registered[tid] == current.generation {
		delete(owner.registered, tid)
	}
	owner.signalLocked()
	owner.mu.Unlock()
	b.mu.Unlock()
}
