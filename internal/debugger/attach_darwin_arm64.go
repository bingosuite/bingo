//go:build darwin && arm64 && bingonative

package debugger

/*
#include "mach_darwin_arm64.h"
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
	"unsafe"
)

type darwinExceptionTuple struct {
	mask     uint32
	port     uint32
	behavior int32
	flavor   int32
}

type darwinSavedException struct {
	tuple    darwinExceptionTuple
	restored bool
	released bool
}

type darwinExceptionState struct {
	saved    []darwinSavedException
	restored bool
}

type darwinAttachedState struct {
	exceptions  darwinExceptionState
	taskHeld    bool
	quiesced    bool
	sendDropped bool
	rpcsRetired bool
	threads     []int
	stepOwner   int
	rendezvous  map[int]*darwinRendezvous
	barrierDone bool
	patches     map[uint64][]byte
}

// An incomplete detach must remain discoverably owned even if its caller drops
// the debugger. Namespace retirement must consult canReleaseMachNamespace.
var outstandingDarwinDetaches sync.Map

func (s *darwinExceptionState) restore(set func(darwinExceptionTuple) error) error {
	if s.restored {
		return nil
	}
	if len(s.saved) == 0 {
		if err := set(darwinExceptionTuple{
			mask: uint32(C.EXC_MASK_BREAKPOINT), behavior: int32(C.EXCEPTION_DEFAULT),
			flavor: int32(C.THREAD_STATE_NONE),
		}); err != nil {
			return fmt.Errorf("clear empty prior breakpoint handler: %w", err)
		}
	}
	for i := range s.saved {
		entry := &s.saved[i]
		if entry.restored {
			continue
		}
		if err := set(entry.tuple); err != nil {
			return fmt.Errorf("restore breakpoint exception tuple %d (mask %#x, port %#x): %w",
				i, entry.tuple.mask, entry.tuple.port, err)
		}
		entry.restored = true
	}
	s.restored = true
	return nil
}

func (s *darwinExceptionState) release(deallocate func(uint32) error) error {
	if !s.restored {
		return fmt.Errorf("saved exception rights still needed for restoration")
	}
	for i := range s.saved {
		entry := &s.saved[i]
		if entry.released || entry.tuple.port == 0 {
			continue
		}
		if err := deallocate(entry.tuple.port); err != nil {
			return fmt.Errorf("release saved exception tuple %d: %w", i, err)
		}
		entry.released = true
	}
	return nil
}

func (b *darwinBackend) installExceptionHandler(task C.mach_port_t) error {
	if b.launched {
		if kr := C.task_set_exception_ports(task, C.EXC_MASK_BREAKPOINT, b.excPort,
			C.EXCEPTION_DEFAULT, C.THREAD_STATE_NONE); kr != C.KERN_SUCCESS {
			return fmt.Errorf("install breakpoint exception handler: %s", machErrString(kr))
		}
		return nil
	}
	var saved C.bingo_exception_ports
	if kr := C.bingo_swap_exception_ports(task, b.excPort, &saved); kr != C.KERN_SUCCESS {
		return fmt.Errorf("swap attached breakpoint exception handler: %s", machErrString(kr))
	}
	b.attachOwned.Store(true)
	outstandingDarwinDetaches.Store(b, struct{}{})
	for i := 0; i < int(saved.count); i++ {
		b.attachState.exceptions.saved = append(b.attachState.exceptions.saved, darwinSavedException{
			tuple: darwinExceptionTuple{
				mask: uint32(saved.masks[i]), port: uint32(saved.ports[i]),
				behavior: int32(saved.behaviors[i]), flavor: int32(saved.flavors[i]),
			},
		})
	}
	return nil
}

func (b *darwinBackend) requireActive() error {
	if b.teardown.Load() {
		return fmt.Errorf("Mach backend teardown is latched")
	}
	return nil
}

func (b *darwinBackend) teardownLatched() bool { return b.teardown.Load() }

func (b *darwinBackend) beginTeardown() error {
	b.teardown.Store(true)
	b.probeMu.Lock()
	if b.probeStop != nil && !b.probeStopped {
		close(b.probeStop)
		b.probeStopped = true
	}
	b.probeMu.Unlock()
	if b.pid == 0 || (!b.launched && !b.attachOwned.Load()) {
		b.targetReleased.Store(true)
	}
	if !b.portsOK || b.waitAcknowledged.Load() {
		return nil
	}
	kr := C.bingo_send_shutdown(b.ctrlPort)
	if kr != C.KERN_SUCCESS && kr != C.MACH_SEND_TIMED_OUT {
		return fmt.Errorf("wake Mach waiter for teardown: %s", machErrString(kr))
	}
	return nil
}

// Only the engine may acknowledge, after the exact one-shot wait goroutine has
// returned and its result send can no longer be blocked behind stopCh.
func (b *darwinBackend) acknowledgeWait(ctx context.Context) error {
	if !b.teardown.Load() {
		return fmt.Errorf("cannot acknowledge an active Mach backend")
	}
	b.probeMu.Lock()
	done := b.probeDone
	b.probeMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return fmt.Errorf("join Mach diagnostic reader: %w", ctx.Err())
		}
	}
	b.waitAcknowledged.Store(true)
	return nil
}

func (b *darwinBackend) canReleaseMachNamespace() bool {
	_, retained := outstandingDarwinDetaches.Load(b)
	return b.teardown.Load() && b.waitAcknowledged.Load() &&
		b.targetReleased.Load() && !b.attachOwned.Load() && !retained
}

func (b *darwinBackend) retainsAttachedOwnership() bool { return b.attachOwned.Load() }
func (b *darwinBackend) attachedQuiesced() bool         { return b.attachState.quiesced }
func (b *darwinBackend) attachedImageReplaced() bool    { return false }

func (b *darwinBackend) receiveMachMessage(timeout C.int) (C.int, int, error) {
	var thread C.mach_port_t
	var exc C.int
	var code C.int64_t
	var id C.int
	var reply replyInfo
	cls := C.bingo_mach_recv(b.portSet, timeout, &thread, &exc, &code, &id, 0, 0,
		&reply.port, &reply.bits, &reply.id)
	if cls == C.BINGO_MSG_EXC {
		b.adoptExcThreadPort(thread)
		if err := b.stashReply(int(thread), reply); err != nil {
			return cls, int(thread), err
		}
		b.waitHooks.received(b.teardown.Load())
	}
	if cls == C.BINGO_MSG_DEATH {
		b.targetGone.Store(true)
	}
	if cls == C.BINGO_MSG_ERROR {
		return cls, int(thread), fmt.Errorf("receive Mach exception/control message failed")
	}
	return cls, int(thread), nil
}

func (b *darwinBackend) drainAttachedMessages(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cls, _, err := b.receiveMachMessage(0)
		if err != nil {
			return err
		}
		if cls == C.BINGO_MSG_NONE {
			return nil
		}
	}
}

func (b *darwinBackend) flushReplyLocked(tid int) error {
	for len(b.pendingReplies[tid]) != 0 {
		reply := &b.pendingReplies[tid][0]
		// Copyout can return the dead sentinel rather than an allocated
		// dead-name right. It proves retirement but has no uref to release.
		if reply.port == C.MACH_PORT_DEAD {
			b.pendingReplies[tid] = b.pendingReplies[tid][1:]
			continue
		}
		kr := C.bingo_reply_exception_checked(&reply.port, &reply.bits, reply.id)
		if kr != C.KERN_SUCCESS {
			if kr != C.MACH_SEND_INVALID_DEST {
				return fmt.Errorf("reply to exception on thread %d: %s", tid, machErrString(kr))
			}
			if reply.port != C.MACH_PORT_DEAD {
				var kind C.mach_port_type_t
				if k := C.mach_port_type(C.mach_task_self_, reply.port, &kind); k != C.KERN_SUCCESS {
					return fmt.Errorf("inspect failed exception reply right on thread %d: %s", tid, machErrString(k))
				}
				if kind&C.MACH_PORT_TYPE_DEAD_NAME == 0 {
					return fmt.Errorf("exception reply destination on thread %d rejected send but is not dead", tid)
				}
				if k := C.bingo_port_deallocate(reply.port); k != C.KERN_SUCCESS {
					return fmt.Errorf("release dead exception reply on thread %d: %s", tid, machErrString(k))
				}
			}
		}
		b.pendingReplies[tid] = b.pendingReplies[tid][1:]
	}
	delete(b.pendingReplies, tid)
	return nil
}

func (b *darwinBackend) stopAttachedWorld() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), attachedDetachTimeout)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("stop attached threads: %w", err)
		}
		if err := b.requireActive(); err != nil {
			return false, context.Canceled
		}
		threads, err := b.Threads()
		if err != nil {
			return false, err
		}
		suspended := false
		for _, tid := range threads {
			var count C.int
			if kr := C.bingo_thread_suspend_count(C.mach_port_t(tid), &count); kr != C.KERN_SUCCESS {
				return false, fmt.Errorf("inspect attached thread %d: %s", tid, machErrString(kr))
			}
			if count == 0 {
				if err := b.holdThread(tid); err != nil {
					return false, err
				}
				suspended = true
			}
		}
		if err := b.drainAttachedMessages(ctx); err != nil {
			return false, err
		}
		if b.targetGone.Load() {
			return true, nil
		}
		if !suspended {
			return false, nil
		}
	}
}

func (b *darwinBackend) quiesceAttached(ctx context.Context) (bool, error) {
	if !b.teardown.Load() || !b.waitAcknowledged.Load() {
		return false, fmt.Errorf("attached quiescence requires acknowledged waiter teardown")
	}
	if err := b.drainAttachedMessages(ctx); err != nil {
		return false, err
	}
	if b.targetGone.Load() {
		b.attachState.quiesced = true
		return true, nil
	}
	task, err := b.task()
	if err != nil {
		return false, err
	}
	if !b.attachState.taskHeld {
		if kr := C.task_suspend(task); kr != C.KERN_SUCCESS {
			return false, fmt.Errorf("hold attached task for restoration: %s", machErrString(kr))
		}
		b.attachState.taskHeld = true
	}
	threads, err := b.Threads()
	if err != nil {
		return false, err
	}
	b.attachState.threads = threads
	if err := b.drainAttachedMessages(ctx); err != nil {
		return false, err
	}
	b.attachState.quiesced = true
	return b.targetGone.Load(), nil
}

func (b *darwinBackend) attachedDetachStops() []StopEvent {
	// ARM64 leaves PC on BRK, so no rewind is necessary. In particular, do not
	// rewrite a sampled user context before the rendezvous proves RPC ownership.
	return nil
}

func (b *darwinBackend) selectAttachedWriteTID() (int, error) {
	if !b.attachState.quiesced || len(b.attachState.threads) == 0 {
		return 0, fmt.Errorf("attached task has no verified restoration thread")
	}
	return b.attachState.threads[0], nil
}

func (b *darwinBackend) restoreAttachedMemory(addr uint64, src []byte) error {
	if !b.waitAcknowledged.Load() || !b.attachState.taskHeld || !b.attachState.quiesced {
		return fmt.Errorf("%w: instruction restoration requires an acknowledged waiter and owned task hold",
			ErrAttachedDetachIncomplete)
	}
	task, err := b.task()
	if err != nil {
		return err
	}
	var flush C.kern_return_t
	kr := C.bingo_write_memory_held(task, C.mach_vm_address_t(addr),
		unsafe.Pointer(&src[0]), C.mach_vm_size_t(len(src)), &flush)
	var failures []error
	if kr != C.KERN_SUCCESS {
		failures = append(failures, fmt.Errorf("restore instruction at 0x%x: %s", addr, machErrString(kr)))
	}
	if flush != C.KERN_SUCCESS {
		failures = append(failures, fmt.Errorf("flush restored instruction at 0x%x: %s", addr, machErrString(flush)))
	}
	return errors.Join(failures...)
}

func (b *darwinBackend) writeAttachedMemory(addr uint64, src []byte) error {
	state := &b.attachState
	if state.taskHeld {
		return fmt.Errorf("%w: a failed patch still owns a task suspension; retry Kill", ErrAttachedDetachIncomplete)
	}
	if prior, ok := state.patches[addr]; ok {
		if len(prior) != len(src) {
			return fmt.Errorf("attached patch at %#x changed width", addr)
		}
	} else {
		prior := make([]byte, len(src))
		if err := b.ReadMemory(addr, prior); err != nil {
			return err
		}
		if state.patches == nil {
			state.patches = make(map[uint64][]byte)
		}
		// A write can patch text and then fail protection/cache restoration,
		// before the engine installs a table entry. Detach still owns the bytes.
		state.patches[addr] = prior
	}
	task, err := b.task()
	if err != nil {
		return err
	}
	if kr := C.task_suspend(task); kr != C.KERN_SUCCESS {
		return fmt.Errorf("hold attached task for patch: %s", machErrString(kr))
	}
	state.taskHeld = true
	var flush C.kern_return_t
	kr := C.bingo_write_memory_held(task, C.mach_vm_address_t(addr),
		unsafe.Pointer(&src[0]), C.mach_vm_size_t(len(src)), &flush)
	var failures []error
	if kr != C.KERN_SUCCESS {
		failures = append(failures, fmt.Errorf("patch attached instruction: %s", machErrString(kr)))
	}
	if flush != C.KERN_SUCCESS {
		failures = append(failures, fmt.Errorf("flush attached instruction: %s", machErrString(flush)))
	}
	if err := b.waitHooks.patched(); err != nil {
		failures = append(failures, err)
	}
	if len(failures) != 0 {
		return fmt.Errorf("%w: %w", ErrAttachedDetachIncomplete,
			errors.Join(append(failures, b.beginTeardown())...))
	}
	if bytes.Equal(state.patches[addr], src) {
		delete(state.patches, addr)
	}
	if kr := C.task_resume(task); kr != C.KERN_SUCCESS {
		return fmt.Errorf("%w: %w", ErrAttachedDetachIncomplete, errors.Join(
			fmt.Errorf("release attached patch suspension: %s", machErrString(kr)), b.beginTeardown()))
	}
	state.taskHeld = false
	return nil
}

func (b *darwinBackend) restoreAttachedPatches() error {
	addresses := make([]uint64, 0, len(b.attachState.patches))
	for addr := range b.attachState.patches {
		addresses = append(addresses, addr)
	}
	slices.Sort(addresses)
	for _, addr := range addresses {
		if err := b.restoreAttachedMemory(addr, b.attachState.patches[addr]); err != nil {
			return err
		}
		delete(b.attachState.patches, addr)
	}
	return nil
}

func (b *darwinBackend) detachAttached() error {
	if !b.attachOwned.Load() {
		return nil
	}
	if !b.waitAcknowledged.Load() || !b.attachState.quiesced {
		return fmt.Errorf("%w: Mach detach has not quiesced its waiter and task", ErrAttachedDetachIncomplete)
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachedDetachTimeout)
	defer cancel()
	state := &b.attachState
	if !b.targetGone.Load() {
		if err := b.restoreAttachedPatches(); err != nil {
			return err
		}
		if err := b.rendezvousAttached(ctx); err != nil {
			return err
		}
	}
	if b.targetGone.Load() {
		state.exceptions.restored = true
	} else {
		task, err := b.task()
		if err != nil {
			return err
		}
		if err := state.exceptions.restore(func(tuple darwinExceptionTuple) error {
			kr := C.task_set_exception_ports(task, C.exception_mask_t(tuple.mask),
				C.mach_port_t(tuple.port), C.exception_behavior_t(tuple.behavior),
				C.thread_state_flavor_t(tuple.flavor))
			if kr != C.KERN_SUCCESS {
				return fmt.Errorf("task_set_exception_ports: %s", machErrString(kr))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := state.exceptions.release(func(port uint32) error {
		if kr := C.bingo_port_deallocate(C.mach_port_t(port)); kr != C.KERN_SUCCESS {
			return fmt.Errorf("mach_port_deallocate(%#x): %s", port, machErrString(kr))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := b.retireAttachedRPCs(ctx); err != nil {
		return err
	}
	if !b.targetGone.Load() {
		for _, tid := range state.threads {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := b.resumeThread(tid); err != nil {
				return fmt.Errorf("resume detached thread %d: %w", tid, err)
			}
		}
		task, err := b.task()
		if err != nil {
			return err
		}
		if state.taskHeld {
			if kr := C.task_resume(task); kr != C.KERN_SUCCESS {
				return fmt.Errorf("release restored attached task: %s", machErrString(kr))
			}
			state.taskHeld = false
		}
	}
	b.attachOwned.Store(false)
	b.targetReleased.Store(true)
	outstandingDarwinDetaches.Delete(b)
	return nil
}

func (b *darwinBackend) retireAttachedRPCs(ctx context.Context) error {
	state := &b.attachState
	if !state.sendDropped {
		// An RPC holds its own send right through exception_raise's return.
		// Dropping our send reference lets zero senders acknowledge all old
		// RPCs without destroying the receive right (owned by the next layer).
		if kr := C.bingo_port_deallocate(b.excPort); kr != C.KERN_SUCCESS {
			return fmt.Errorf("release old exception-port send reference: %s", machErrString(kr))
		}
		state.sendDropped = true
	}
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("retire old exception RPCs: %w", err)
		}
		if err := b.drainAttachedMessages(ctx); err != nil {
			return err
		}
		if err := b.flushAllReplies(); err != nil {
			return err
		}
		var status C.mach_port_status_t
		count := C.mach_msg_type_number_t(C.MACH_PORT_RECEIVE_STATUS_COUNT)
		if kr := C.mach_port_get_attributes(C.mach_task_self_, b.excPort,
			C.MACH_PORT_RECEIVE_STATUS, C.mach_port_info_t(unsafe.Pointer(&status)), &count); kr != C.KERN_SUCCESS {
			return fmt.Errorf("inspect outstanding exception senders: %s", machErrString(kr))
		}

		if status.mps_srights == 0 && status.mps_msgcount == 0 {
			state.rpcsRetired = true
			return nil
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (b *darwinBackend) holdThread(tid int) error {
	b.threadMu.Lock()
	defer b.threadMu.Unlock()
	if kr := C.thread_suspend(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
		return fmt.Errorf("suspend thread %d: %s", tid, machErrString(kr))
	}
	if b.attachOwned.Load() {
		if b.threadHolds == nil {
			b.threadHolds = make(map[int]int)
		}
		b.threadHolds[tid]++
	}
	return nil
}

func (b *darwinBackend) resumeThread(tid int) error {
	if !b.attachOwned.Load() {
		if kr := C.bingo_resume_one_thread(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
			return fmt.Errorf("resume thread %d: %s", tid, machErrString(kr))
		}
		return nil
	}
	b.threadMu.Lock()
	defer b.threadMu.Unlock()
	for b.threadHolds[tid] > 0 {
		if kr := C.thread_resume(C.mach_port_t(tid)); kr != C.KERN_SUCCESS {
			if b.teardown.Load() {
				dead, err := darwinThreadDead(tid)
				if err != nil {
					return err
				}
				if dead {
					delete(b.threadHolds, tid)
					return nil
				}
			}
			return fmt.Errorf("release owned suspension on thread %d: %s", tid, machErrString(kr))
		}
		b.threadHolds[tid]--
	}
	delete(b.threadHolds, tid)
	return nil
}
