//go:build e2e && darwin && arm64 && bingonative

package debugger

/*
#include "mach_darwin_arm64.h"
*/
import "C"

import (
	"context"
	"encoding/binary"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

type darwinWaitHooks struct {
	gate               atomic.Pointer[DarwinWaitGate]
	exceptions         atomic.Uint64
	teardownExceptions atomic.Uint64
	executeRendezvous  atomic.Uint64
	stepRendezvous     atomic.Uint64
	failPatch          atomic.Bool
	rendezvousMu       sync.Mutex
	arms               map[int]uint64
	acks               map[int]uint64
}

type DarwinWaitGate struct {
	boundary string
	thread   atomic.Int64
	Entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (g *DarwinWaitGate) Release() { g.once.Do(func() { close(g.release) }) }

func (hooks *darwinWaitHooks) at(boundary string) error {
	return hooks.atContext(context.Background(), boundary)
}

func (hooks *darwinWaitHooks) afterReceive(tid int) error {
	gate := hooks.gate.Load()
	if gate != nil && gate.thread.Load() != 0 && gate.thread.Load() != int64(tid) {
		return nil
	}
	return hooks.at("after-receive")
}

func (hooks *darwinWaitHooks) atContext(ctx context.Context, boundary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gate := hooks.gate.Load()
	if gate == nil || gate.boundary != boundary || !hooks.gate.CompareAndSwap(gate, nil) {
		return nil
	}
	close(gate.Entered)
	select {
	case <-gate.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(8 * time.Second):
		return fmt.Errorf("native test gate %s exceeded its deadline", boundary)
	}
}

func (hooks *darwinWaitHooks) received(teardown bool) {
	hooks.exceptions.Add(1)
	if teardown {
		hooks.teardownExceptions.Add(1)
	}
}

func (hooks *darwinWaitHooks) rendezvousArmed(tid int) {
	hooks.rendezvousMu.Lock()
	defer hooks.rendezvousMu.Unlock()
	if hooks.arms == nil {
		hooks.arms = make(map[int]uint64)
	}
	hooks.arms[tid]++
}

func (hooks *darwinWaitHooks) rendezvoused(tid int, ec uint32) {
	if ec == darwinExecuteEC {
		hooks.executeRendezvous.Add(1)
	} else {
		hooks.stepRendezvous.Add(1)
	}
	hooks.rendezvousMu.Lock()
	defer hooks.rendezvousMu.Unlock()
	if hooks.acks == nil {
		hooks.acks = make(map[int]uint64)
	}
	hooks.acks[tid]++
}

func (hooks *darwinWaitHooks) patched() error {
	if hooks.failPatch.Swap(false) {
		return fmt.Errorf("native test failure after instruction write")
	}
	return nil
}

func DarwinFailNextPatchWrite(d Debugger) error {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return err
	}
	if !b.attachOwned.Load() || b.teardown.Load() {
		return fmt.Errorf("patch fault requires an active attached fixture")
	}
	b.waitHooks.failPatch.Store(true)
	return nil
}

func DarwinInspectUntrackedPatch(d Debugger, pid int) (uint32, error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return 0, err
	}
	e := d.(*engine)
	var instruction uint32
	err = e.dispatch(func() error {
		if pid <= 0 || b.pid != pid || !b.attachOwned.Load() || !b.teardown.Load() ||
			!b.attachState.taskHeld || len(b.attachState.patches) != 1 || len(e.bps.byAddr) != 0 {
			return fmt.Errorf("untracked-patch probe requires one retained, table-less fixture patch")
		}
		for addr := range b.attachState.patches {
			var data [4]byte
			if err := b.ReadMemory(addr, data[:]); err != nil {
				return err
			}
			instruction = binary.LittleEndian.Uint32(data[:])
		}
		return nil
	})
	return instruction, err
}

func darwinProbeBackend(d Debugger) (*darwinBackend, error) {
	e, ok := d.(*engine)
	if !ok {
		return nil, fmt.Errorf("native probe requires an engine")
	}
	b, ok := e.backend.(*darwinBackend)
	if !ok {
		return nil, fmt.Errorf("native probe requires the Darwin backend")
	}
	return b, nil
}

// Gates hold only the selected real waiter's boundary, never another session or
// an arbitrary process. The hard deadline prevents a failed spec retaining it.
func DarwinGateWait(d Debugger, boundary string) (*DarwinWaitGate, error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return nil, err
	}
	if boundary != "before-receive" && boundary != "after-receive" && boundary != "before-return" &&
		boundary != "before-rendezvous-ack" && boundary != "after-rendezvous-ack" {
		return nil, fmt.Errorf("unknown native wait boundary %q", boundary)
	}
	gate := &DarwinWaitGate{boundary: boundary, Entered: make(chan struct{}), release: make(chan struct{})}
	if !b.waitHooks.gate.CompareAndSwap(nil, gate) {
		return nil, fmt.Errorf("a native wait gate is already installed")
	}
	return gate, nil
}

type DarwinDetachProbe struct {
	Latched            bool
	WaitAcknowledged   bool
	Retained           bool
	Complete           bool
	QueuedExceptions   uint32
	Exceptions         uint64
	TeardownExceptions uint64
	ExecuteRendezvous  uint64
	StepRendezvous     uint64
	RendezvousArms     map[int]uint64
	RendezvousAcks     map[int]uint64
}

func DarwinInspectDetach(d Debugger) (DarwinDetachProbe, error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return DarwinDetachProbe{}, err
	}
	var status C.mach_port_status_t
	count := C.mach_msg_type_number_t(C.MACH_PORT_RECEIVE_STATUS_COUNT)
	if kr := C.mach_port_get_attributes(C.mach_task_self_, b.excPort,
		C.MACH_PORT_RECEIVE_STATUS, C.mach_port_info_t(unsafe.Pointer(&status)), &count); kr != C.KERN_SUCCESS {
		return DarwinDetachProbe{}, fmt.Errorf("inspect native exception queue: %s", machErrString(kr))
	}
	_, retained := outstandingDarwinDetaches.Load(b)
	b.waitHooks.rendezvousMu.Lock()
	arms, acks := maps.Clone(b.waitHooks.arms), maps.Clone(b.waitHooks.acks)
	b.waitHooks.rendezvousMu.Unlock()
	return DarwinDetachProbe{
		Latched: b.teardown.Load(), WaitAcknowledged: b.waitAcknowledged.Load(),
		Retained: retained, Complete: b.canReleaseMachNamespace(),
		QueuedExceptions: uint32(status.mps_msgcount),
		Exceptions:       b.waitHooks.exceptions.Load(), TeardownExceptions: b.waitHooks.teardownExceptions.Load(),
		ExecuteRendezvous: b.waitHooks.executeRendezvous.Load(), StepRendezvous: b.waitHooks.stepRendezvous.Load(),
		RendezvousArms: arms, RendezvousAcks: acks,
	}, nil
}

// Holding one extra send uref deliberately withholds the kernel's no-senders
// acknowledgement, exercising the real incomplete-detach retry rather than a
// mock error. The caller must release this exact extra reference.
func DarwinHoldExceptionSender(d Debugger) (func() error, error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return nil, err
	}
	port := b.excPort
	if kr := C.mach_port_mod_refs(C.mach_task_self_, port, C.MACH_PORT_RIGHT_SEND, 1); kr != C.KERN_SUCCESS {
		return nil, fmt.Errorf("hold test exception sender: %s", machErrString(kr))
	}
	var mu sync.Mutex
	released := false
	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if released {
			return nil
		}
		if kr := C.bingo_port_deallocate(port); kr != C.KERN_SUCCESS {
			return fmt.Errorf("release test exception sender: %s", machErrString(kr))
		}
		released = true
		return nil
	}, nil
}

// A failed native spec may leave its victim task-suspended, where SIGKILL alone
// cannot run the fatal AST. Only the fixture's captured task capability is used;
// this must never become a production foreign-process cleanup path.
func DarwinTerminateTestTarget(d Debugger, pid int) error {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return err
	}
	if pid <= 0 || b.pid != pid || b.launched {
		return fmt.Errorf("emergency cleanup requires the exact attached fixture PID")
	}
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	if !b.taskOK || b.targetGone.Load() {
		return nil
	}
	if kr := C.task_terminate(b.taskPort); kr != C.KERN_SUCCESS {
		return fmt.Errorf("terminate native fixture task %d: %s", pid, machErrString(kr))
	}
	return nil
}
