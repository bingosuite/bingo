//go:build darwin && arm64 && bingonative

package debugger

/*
#include "mach_darwin_arm64.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"syscall"
)

const (
	darwinExecuteEC = 0x30
	darwinStepEC    = 0x32
	darwinBRKEC     = 0x3c
)

type darwinRendezvous struct {
	prior        C.arm_debug_state64_t
	expected     uint32
	pc           uint64
	armed        bool
	acknowledged bool
	complete     bool
}

func (r *darwinRendezvous) matches(ec uint32, pc uint64) (bool, error) {
	if !r.armed {
		return false, nil
	}
	if ec != r.expected {
		if ec != darwinBRKEC && ec != darwinStepEC && ec != darwinExecuteEC {
			return false, fmt.Errorf("unexpected debug exception class %#x", ec)
		}
		return false, nil
	}
	if ec == darwinExecuteEC && pc != r.pc {
		return false, fmt.Errorf("rendezvous PC changed: got %#x, armed %#x", pc, r.pc)
	}
	return true, nil
}

// XNU saves EL0 ESR before the interruptible exception-handler selection path;
// nested EL1 entries do not overwrite the user PCB. A different debug class
// therefore proves a new user boundary, unlike a quiet exception queue.
func darwinRendezvousClass(entry uint32, stepOwned bool) uint32 {
	switch entry {
	case darwinExecuteEC:
		return darwinStepEC
	case darwinBRKEC, darwinStepEC:
		return darwinExecuteEC
	default:
		if stepOwned {
			return darwinExecuteEC
		}
		return 0
	}
}

func darwinDebugStateEmpty(state C.arm_debug_state64_t, ownedStep bool) bool {
	var canonical C.uint64_t
	if ownedStep {
		// XNU leaves its forced user-mode control bit in disabled BCR/WCR
		// slots after consuming SS. Those bits came from Bingo's own step.
		canonical = 4
	}
	for i := range state.__bcr {
		if state.__bcr[i]&^canonical != 0 || state.__bvr[i] != 0 ||
			state.__wcr[i]&^canonical != 0 || state.__wvr[i] != 0 {
			return false
		}
	}
	if ownedStep {
		state.__mdscr_el1 &^= 1
	}
	return state.__mdscr_el1 == 0
}

func darwinThreadDead(tid int) (bool, error) {
	var kind C.mach_port_type_t
	if kr := C.mach_port_type(C.mach_task_self_, C.mach_port_t(tid), &kind); kr != C.KERN_SUCCESS {
		return false, fmt.Errorf("inspect retained thread %d: %s", tid, machErrString(kr))
	}
	return kind&C.MACH_PORT_TYPE_DEAD_NAME != 0, nil
}

func darwinCheckThreadHandler(tid int) error {
	var ports C.bingo_exception_ports
	if kr := C.bingo_thread_exception_ports(C.mach_port_t(tid), &ports); kr != C.KERN_SUCCESS {
		return fmt.Errorf("inspect thread %d exception routing: %s", tid, machErrString(kr))
	}
	var failures []error
	for i := 0; i < int(ports.count); i++ {
		if ports.ports[i] == C.MACH_PORT_NULL {
			continue
		}
		failures = append(failures, fmt.Errorf("thread %d has a private breakpoint handler; cannot own its rendezvous", tid))
		if ports.ports[i] != C.MACH_PORT_DEAD {
			if kr := C.bingo_port_deallocate(ports.ports[i]); kr != C.KERN_SUCCESS {
				failures = append(failures, fmt.Errorf("release inspected thread handler: %s", machErrString(kr)))
			}
		}
	}
	return errors.Join(failures...)
}

func (b *darwinBackend) ensureAttachedHold(tid int) error {
	b.threadMu.Lock()
	held := b.threadHolds[tid] > 0
	b.threadMu.Unlock()
	if held {
		return nil
	}
	return b.holdThread(tid)
}

func (b *darwinBackend) prepareAttachedRendezvous(ctx context.Context) error {
	state := &b.attachState
	if !state.taskHeld || !state.quiesced || !b.waitAcknowledged.Load() {
		return fmt.Errorf("rendezvous requires a restored task under acknowledged, owned suspension")
	}
	task, err := b.task()
	if err != nil {
		return err
	}
	var defaults C.arm_debug_state64_t
	if kr := C.bingo_get_task_debug_state(task, &defaults); kr != C.KERN_SUCCESS {
		return fmt.Errorf("inspect inherited task debug state: %s", machErrString(kr))
	}
	if !darwinDebugStateEmpty(defaults, false) {
		return fmt.Errorf("task has foreign inherited debug state; cannot safely rendezvous new threads")
	}
	var slots C.int
	if rc := C.bingo_hardware_breakpoints(&slots); rc != 0 {
		return fmt.Errorf("discover hardware breakpoint slots: %w", syscall.Errno(rc))
	}
	if slots < 1 {
		return fmt.Errorf("attached restoration requires a hardware execution breakpoint slot")
	}
	if state.rendezvous == nil {
		state.rendezvous = make(map[int]*darwinRendezvous)
	}
	b.threadMu.Lock()
	for port := range b.threadPorts {
		if !slices.Contains(state.threads, int(port)) {
			state.threads = append(state.threads, int(port))
		}
	}
	b.threadMu.Unlock()
	for _, tid := range state.threads {
		if err := ctx.Err(); err != nil {
			return err
		}
		dead, err := darwinThreadDead(tid)
		if err != nil {
			return err
		}
		if dead {
			b.threadMu.Lock()
			delete(b.threadHolds, tid)
			b.threadMu.Unlock()
			state.rendezvous[tid] = &darwinRendezvous{complete: true}
			continue
		}
		if err := b.ensureAttachedHold(tid); err != nil {
			return err
		}
		if state.rendezvous[tid] != nil {
			continue
		}
		if err := darwinCheckThreadHandler(tid); err != nil {
			return err
		}
		var prior C.arm_debug_state64_t
		if kr := C.bingo_get_debug_state(C.mach_port_t(tid), &prior); kr != C.KERN_SUCCESS {
			return fmt.Errorf("read thread %d debug state: %s", tid, machErrString(kr))
		}
		stepOwned := tid == state.stepOwner
		if !darwinDebugStateEmpty(prior, stepOwned) {
			return fmt.Errorf("thread %d has foreign debug state; cannot safely replace it", tid)
		}
		var entry C.uint32_t
		if kr := C.bingo_exception_class(C.mach_port_t(tid), &entry); kr != C.KERN_SUCCESS {
			return fmt.Errorf("read thread %d entry ESR: %s", tid, machErrString(kr))
		}
		prior.__mdscr_el1 &^= 1
		expected := darwinRendezvousClass(uint32(entry), stepOwned)
		state.rendezvous[tid] = &darwinRendezvous{
			prior: prior, expected: expected, complete: expected == 0,
		}
	}
	return nil
}

func (b *darwinBackend) armAttachedRendezvous(tid int, rendezvous *darwinRendezvous) error {
	next := rendezvous.prior
	if rendezvous.expected == darwinExecuteEC {
		regs, err := b.GetRegisters(tid)
		if err != nil {
			return err
		}
		if regs.PC == 0 || regs.PC&3 != 0 {
			return fmt.Errorf("thread %d has invalid rendezvous PC %#x", tid, regs.PC)
		}
		rendezvous.pc = regs.PC
		next.__bvr[0] = C.uint64_t(regs.PC)
		next.__bcr[0] = 0x1e5
	} else {
		next.__mdscr_el1 |= 1
	}
	// Debug-state-only writes let XNU arm SS without replaying a potentially
	// stale GPR snapshot over a kernel-side signal/context transition.
	if kr := C.bingo_set_debug_state(C.mach_port_t(tid), &next); kr != C.KERN_SUCCESS {
		return fmt.Errorf("arm thread %d restoration rendezvous: %s", tid, machErrString(kr))
	}
	rendezvous.armed = true
	b.waitHooks.rendezvousArmed(tid)
	return nil
}

func (b *darwinBackend) acknowledgeAttachedRendezvous(ctx context.Context, tid int, rendezvous *darwinRendezvous) (bool, error) {
	if !rendezvous.acknowledged {
		b.replyMu.Lock()
		pending := len(b.pendingReplies[tid]) != 0
		b.replyMu.Unlock()
		if !rendezvous.armed || !pending {
			return false, nil
		}
		var ec C.uint32_t
		if kr := C.bingo_exception_class(C.mach_port_t(tid), &ec); kr != C.KERN_SUCCESS {
			return false, fmt.Errorf("read thread %d rendezvous ESR: %s", tid, machErrString(kr))
		}
		var pc uint64
		if uint32(ec) == darwinExecuteEC {
			regs, err := b.GetRegisters(tid)
			if err != nil {
				return false, err
			}
			pc = regs.PC
		}
		matches, err := rendezvous.matches(uint32(ec), pc)
		if err != nil {
			return false, fmt.Errorf("thread %d: %w", tid, err)
		}
		if !matches {
			return false, nil
		}
		if err := b.waitHooks.atContext(ctx, "before-rendezvous-ack"); err != nil {
			return false, err
		}
		rendezvous.acknowledged = true
		b.waitHooks.rendezvoused(tid, uint32(ec))
		if err := b.waitHooks.atContext(ctx, "after-rendezvous-ack"); err != nil {
			return false, err
		}
	}
	if err := b.ensureAttachedHold(tid); err != nil {
		return false, err
	}
	if kr := C.bingo_set_debug_state(C.mach_port_t(tid), &rendezvous.prior); kr != C.KERN_SUCCESS {
		return false, fmt.Errorf("restore thread %d debug state: %s", tid, machErrString(kr))
	}
	rendezvous.complete = true
	return true, nil
}

func (b *darwinBackend) awaitAttachedRendezvous(ctx context.Context, tid int, rendezvous *darwinRendezvous) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("thread %d did not acknowledge restoration rendezvous: %w", tid, err)
		}
		dead, err := darwinThreadDead(tid)
		if err != nil {
			return err
		}
		if dead {
			rendezvous.complete = true
			return nil
		}
		cls, stopped, err := b.receiveMachMessage(10)
		if err != nil {
			return err
		}
		if b.targetGone.Load() {
			return nil
		}
		if cls != C.BINGO_MSG_EXC || stopped != tid {
			continue
		}
		if acknowledged, err := b.acknowledgeAttachedRendezvous(ctx, tid, rendezvous); err != nil || acknowledged {
			return err
		}
		// An older step can clear SS, and a signal can change the saved return
		// PC. Re-arm at the now RPC-stable context without changing its GPRs.
		if err := b.armAttachedRendezvous(tid, rendezvous); err != nil {
			return err
		}
		if err := b.flushReply(tid); err != nil {
			return err
		}
	}
}

func (b *darwinBackend) holdAttachedTask() error {
	if b.attachState.taskHeld || b.targetGone.Load() {
		return nil
	}
	task, err := b.task()
	if err != nil {
		return err
	}
	if kr := C.task_suspend(task); kr != C.KERN_SUCCESS {
		return fmt.Errorf("reacquire restoration task hold: %s", machErrString(kr))
	}
	b.attachState.taskHeld = true
	b.attachState.quiesced = true
	return nil
}

func (b *darwinBackend) rendezvousAttached(ctx context.Context) error {
	state := &b.attachState
	if state.barrierDone {
		return nil
	}
	if err := b.prepareAttachedRendezvous(ctx); err != nil {
		return err
	}
	for _, tid := range state.threads {
		rendezvous := state.rendezvous[tid]
		if rendezvous.complete {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if acknowledged, err := b.acknowledgeAttachedRendezvous(ctx, tid, rendezvous); err != nil || acknowledged {
			if err != nil {
				return err
			}
			continue
		}
		if err := b.armAttachedRendezvous(tid, rendezvous); err != nil {
			return err
		}
		if err := b.flushReply(tid); err != nil {
			return err
		}
		if err := b.resumeThread(tid); err != nil {
			return err
		}
		task, err := b.task()
		if err != nil {
			return err
		}
		if kr := C.task_resume(task); kr != C.KERN_SUCCESS {
			return fmt.Errorf("run thread %d restoration rendezvous: %s", tid, machErrString(kr))
		}
		state.taskHeld = false
		state.quiesced = false
		err = b.awaitAttachedRendezvous(ctx, tid, rendezvous)
		if holdErr := b.holdAttachedTask(); err != nil || holdErr != nil {
			return errors.Join(err, holdErr)
		}
		if b.targetGone.Load() {
			return nil
		}
	}
	// A stepped syscall may create a thread, but temporary per-thread debug
	// state is not inherited. The task defaults were checked before any resume,
	// all software patches are gone, and the final task hold covers that child.
	threads, err := b.Threads()
	if err != nil {
		return err
	}
	for _, tid := range threads {
		if !slices.Contains(state.threads, tid) {
			state.threads = append(state.threads, tid)
		}
	}
	state.barrierDone = true
	return nil
}
