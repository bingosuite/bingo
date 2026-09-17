//go:build e2e && darwin && arm64 && bingonative

package debugger

/*
#include "mach_darwin_arm64.h"
*/
import "C"

import (
	"fmt"
	"sync"
)

// The extra send reference pins this exact test-owned thread even if enumeration
// later retires it. Release balances only the control's own state and reference.
type DarwinThreadControl struct {
	TID int
	PC  uint64

	mu      sync.Mutex
	backend *darwinBackend
	prior   C.arm_debug_state64_t
	debug   bool
	held    bool
	right   bool
}

func (control *DarwinThreadControl) Release() error {
	control.mu.Lock()
	defer control.mu.Unlock()
	if !control.right {
		return nil
	}
	dead, err := darwinThreadDead(control.TID)
	if err != nil {
		return err
	}
	thread := C.mach_port_t(control.TID)
	if !dead {
		if control.debug {
			if kr := C.bingo_set_debug_state(thread, &control.prior); kr != C.KERN_SUCCESS {
				return fmt.Errorf("restore test thread debug state: %s", machErrString(kr))
			}
			control.debug = false
		}
		if control.held {
			if kr := C.thread_resume(thread); kr != C.KERN_SUCCESS {
				return fmt.Errorf("release test-owned thread suspension: %s", machErrString(kr))
			}
			control.held = false
		}
	}
	if kr := C.bingo_port_deallocate(thread); kr != C.KERN_SUCCESS {
		return fmt.Errorf("release test thread send reference: %s", machErrString(kr))
	}
	control.right = false
	return nil
}

func (control *DarwinThreadControl) VerifyExecuteStop() error {
	control.mu.Lock()
	defer control.mu.Unlock()
	if !control.right || !control.debug {
		return fmt.Errorf("execution-stop proof requires the test's live comparator")
	}
	b := control.backend
	b.replyMu.Lock()
	pending := len(b.pendingReplies[control.TID]) != 0
	b.replyMu.Unlock()
	if !pending {
		return fmt.Errorf("test execution breakpoint has no received exception RPC")
	}
	var ec C.uint32_t
	if kr := C.bingo_exception_class(C.mach_port_t(control.TID), &ec); kr != C.KERN_SUCCESS {
		return fmt.Errorf("inspect test execution ESR: %s", machErrString(kr))
	}
	regs, err := b.GetRegisters(control.TID)
	if err != nil {
		return err
	}
	if uint32(ec) != darwinExecuteEC || regs.PC != control.PC {
		return fmt.Errorf("native execution stop = EC %#x PC %#x, want EC %#x PC %#x",
			uint32(ec), regs.PC, darwinExecuteEC, control.PC)
	}
	return nil
}

func DarwinHoldStoppedThread(d Debugger, pid int) (*DarwinThreadControl, error) {
	return darwinControlStoppedThread(d, pid, false)
}

// Prime a genuine EC30 RPC, not a synthetic ESR. The caller gates the real waiter
// after receive, verifies provenance, and removes this comparator before detach.
func DarwinPrimeExecuteStop(d Debugger, pid int) (*DarwinThreadControl, error) {
	return darwinControlStoppedThread(d, pid, true)
}

func DarwinGateStepStop(d Debugger, pid int) (*DarwinWaitGate, int, error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return nil, 0, err
	}
	e := d.(*engine)
	var gate *DarwinWaitGate
	var tid int
	err = e.dispatch(func() error {
		if pid <= 0 || b.pid != pid || !b.attachOwned.Load() || b.teardown.Load() ||
			e.getState() != stateSuspended || e.curTID == 0 {
			return fmt.Errorf("step gate requires the exact stopped fixture")
		}
		tid = e.curTID
		var err error
		gate, err = DarwinGateWait(d, "after-receive")
		if err == nil {
			gate.thread.Store(int64(tid))
		}
		return err
	})
	return gate, tid, err
}

func DarwinVerifyStepStop(d Debugger, pid, tid int) error {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return err
	}
	if pid <= 0 || b.pid != pid || !b.attachOwned.Load() {
		return fmt.Errorf("step proof requires the exact attached fixture")
	}
	b.replyMu.Lock()
	pending := len(b.pendingReplies[tid]) != 0
	b.replyMu.Unlock()
	b.stepMu.Lock()
	owner := b.stepThreadPort
	b.stepMu.Unlock()
	if !pending || owner != tid {
		return fmt.Errorf("step proof requires the active owner's received RPC")
	}
	var ec C.uint32_t
	if kr := C.bingo_exception_class(C.mach_port_t(tid), &ec); kr != C.KERN_SUCCESS {
		return fmt.Errorf("inspect test step ESR: %s", machErrString(kr))
	}
	if uint32(ec) != darwinStepEC {
		return fmt.Errorf("native step ESR EC = %#x, want %#x", uint32(ec), darwinStepEC)
	}
	return nil
}

func darwinControlStoppedThread(d Debugger, pid int, execute bool) (*DarwinThreadControl, error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return nil, err
	}
	e := d.(*engine)
	var control *DarwinThreadControl
	err = e.dispatch(func() error {
		if pid <= 0 || b.pid != pid || b.launched || !b.attachOwned.Load() || b.teardown.Load() ||
			e.getState() != stateSuspended || e.curTID == 0 || e.steppingOverBP != nil {
			return fmt.Errorf("thread control requires the exact stopped attached fixture")
		}
		if execute && e.lastBP != nil {
			return fmt.Errorf("clear the fixture's software breakpoint before priming execution")
		}
		if execute {
			gate := b.waitHooks.gate.Load()
			if gate == nil || gate.boundary != "after-receive" {
				return fmt.Errorf("prime requires a gated receive so the engine cannot advance its EC30")
			}
			gate.thread.Store(int64(e.curTID))
		}
		control = &DarwinThreadControl{TID: e.curTID, backend: b}
		thread := C.mach_port_t(control.TID)
		if kr := C.mach_port_mod_refs(C.mach_task_self_, thread, C.MACH_PORT_RIGHT_SEND, 1); kr != C.KERN_SUCCESS {
			return fmt.Errorf("retain test thread capability: %s", machErrString(kr))
		}
		control.right = true
		if !execute {
			if kr := C.thread_suspend(thread); kr != C.KERN_SUCCESS {
				return fmt.Errorf("hold test thread: %s", machErrString(kr))
			}
			control.held = true
			return nil
		}
		if kr := C.bingo_get_debug_state(thread, &control.prior); kr != C.KERN_SUCCESS {
			return fmt.Errorf("read test thread debug state: %s", machErrString(kr))
		}
		if !darwinDebugStateEmpty(control.prior, false) {
			return fmt.Errorf("test thread already has a debug configuration")
		}
		regs, err := b.GetRegisters(control.TID)
		if err != nil {
			return err
		}
		if regs.PC == 0 || regs.PC&3 != 0 {
			return fmt.Errorf("test execution breakpoint has invalid PC %#x", regs.PC)
		}
		control.PC = regs.PC
		next := control.prior
		next.__bvr[0] = C.uint64_t(regs.PC)
		next.__bcr[0] = 0x1e5
		control.debug = true
		if kr := C.bingo_set_debug_state(thread, &next); kr != C.KERN_SUCCESS {
			return fmt.Errorf("arm test execution breakpoint: %s", machErrString(kr))
		}
		if err := b.flushReply(control.TID); err != nil {
			return err
		}
		if err := b.resumeThread(control.TID); err != nil {
			return err
		}
		e.setState(stateRunning)
		e.startWait()
		return nil
	})
	return control, err
}
