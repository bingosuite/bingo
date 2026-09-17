//go:build e2e && darwin && arm64 && bingonative

package debugger

/*
#include "mach_darwin_arm64.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"
)

type DarwinNamespaceCensus struct {
	Names     int
	PortSets  int
	DeadNames int
	Receivers int
	Senders   int
	SendOnce  int
}

func DarwinMachCensus() (census DarwinNamespaceCensus, err error) {
	var names C.mach_port_name_array_t
	var kinds C.mach_port_type_array_t
	var namesCount, kindsCount C.mach_msg_type_number_t
	if kr := C.mach_port_names(C.mach_task_self_, &names, &namesCount, &kinds, &kindsCount); kr != C.KERN_SUCCESS {
		return census, fmt.Errorf("Mach namespace census: %s", machErrString(kr))
	}
	defer func() {
		for _, allocation := range []struct {
			address uintptr
			bytes   uintptr
		}{
			{uintptr(unsafe.Pointer(names)), uintptr(namesCount) * unsafe.Sizeof(C.mach_port_t(0))},
			{uintptr(unsafe.Pointer(kinds)), uintptr(kindsCount) * unsafe.Sizeof(C.mach_port_type_t(0))},
		} {
			if kr := C.vm_deallocate(C.mach_task_self_, C.vm_address_t(allocation.address),
				C.vm_size_t(allocation.bytes)); kr != C.KERN_SUCCESS {
				err = errors.Join(err, fmt.Errorf("release census buffer: %s", machErrString(kr)))
			}
		}
	}()
	if namesCount != kindsCount {
		return census, fmt.Errorf("Mach census returned %d names and %d types", namesCount, kindsCount)
	}
	ports := unsafe.Slice((*C.mach_port_t)(unsafe.Pointer(names)), int(namesCount))
	for _, port := range ports {
		var kind C.mach_port_type_t
		kr := C.mach_port_type(C.mach_task_self_, port, &kind)
		if kr == C.KERN_INVALID_NAME {
			// Go's unrelated runtime threads can retire during this census.
			// Positive absence contributes no port set or dead name.
			continue
		}
		if kr != C.KERN_SUCCESS {
			return census, fmt.Errorf("census port %#x: %s", uint32(port), machErrString(kr))
		}
		census.Names++
		if kind&C.MACH_PORT_TYPE_PORT_SET != 0 {
			census.PortSets++
		}
		if kind&C.MACH_PORT_TYPE_DEAD_NAME != 0 {
			census.DeadNames++
		}
		if kind&C.MACH_PORT_TYPE_RECEIVE != 0 {
			census.Receivers++
		}
		if kind&C.MACH_PORT_TYPE_SEND != 0 {
			census.Senders++
		}
		if kind&C.MACH_PORT_TYPE_SEND_ONCE != 0 {
			census.SendOnce++
		}
	}
	return census, nil
}

type DarwinNamespaceProbe struct {
	Names            []uint32
	Task             uint32
	Threads          []uint32
	PortSet          uint32
	ExceptionPort    uint32
	NotificationPort uint32
	ControlPort      uint32
	Eligible         bool
	Released         bool
}

func DarwinInspectNamespace(d Debugger) (DarwinNamespaceProbe, error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return DarwinNamespaceProbe{}, err
	}
	b.namespace.mu.Lock()
	defer b.namespace.mu.Unlock()
	b.threadMu.Lock()
	defer b.threadMu.Unlock()
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	p := DarwinNamespaceProbe{
		Task: uint32(b.taskPort), PortSet: uint32(b.portSet),
		ExceptionPort: uint32(b.excPort), NotificationPort: uint32(b.notePort), ControlPort: uint32(b.ctrlPort),
		Eligible: b.canReleaseMachNamespace(), Released: b.namespace.released,
	}
	for port := range b.threadPorts {
		p.Threads = append(p.Threads, port)
	}
	slices.Sort(p.Threads)
	p.Names = append(p.Names, p.Threads...)
	for _, name := range []uint32{p.Task, p.PortSet, p.ExceptionPort, p.NotificationPort, p.ControlPort,
		b.namespace.previousNotification, b.namespace.cancelledNotification} {
		if name != 0 && name != uint32(C.MACH_PORT_DEAD) {
			p.Names = append(p.Names, name)
		}
	}
	slices.Sort(p.Names)
	return p, nil
}

func DarwinRemainingMachNames(names []uint32) ([]uint32, error) {
	var remaining []uint32
	for _, name := range names {
		var kind C.mach_port_type_t
		kr := C.mach_port_type(C.mach_task_self_, C.mach_port_t(name), &kind)
		if kr == C.KERN_INVALID_NAME {
			continue
		}
		if kr != C.KERN_SUCCESS {
			return nil, fmt.Errorf("inspect former owned Mach name %#x: %s", name, machErrString(kr))
		}
		remaining = append(remaining, name)
	}
	return remaining, nil
}

func DarwinTryNamespaceRelease(d Debugger) error {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return err
	}
	return b.releaseMachNamespace()
}

type darwinNamespaceFault struct {
	nativeDarwinMachCalls
	setupCalls   int
	failSetup    int
	releaseCalls int
	failRelease  int
	held         atomic.Bool
}

func (f *darwinNamespaceFault) setupCall() error {
	f.setupCalls++
	if f.setupCalls == f.failSetup {
		return fmt.Errorf("native test namespace setup failure at call %d", f.setupCalls)
	}
	return nil
}

func (f *darwinNamespaceFault) allocate(right uint32) (uint32, error) {
	if err := f.setupCall(); err != nil {
		return 0, err
	}
	return f.nativeDarwinMachCalls.allocate(right)
}

func (f *darwinNamespaceFault) makeSend(port uint32) error {
	if err := f.setupCall(); err != nil {
		return err
	}
	return f.nativeDarwinMachCalls.makeSend(port)
}

func (f *darwinNamespaceFault) notification(task, notify uint32) (uint32, error) {
	if notify != 0 {
		if err := f.setupCall(); err != nil {
			return 0, err
		}
	}
	return f.nativeDarwinMachCalls.notification(task, notify)
}

func (f *darwinNamespaceFault) moveMember(port, set uint32) error {
	if err := f.setupCall(); err != nil {
		return err
	}
	return f.nativeDarwinMachCalls.moveMember(port, set)
}

func (f *darwinNamespaceFault) modRefs(port, right uint32, delta int32) error {
	f.releaseCalls++
	if f.held.Load() && f.releaseCalls >= f.failRelease {
		return fmt.Errorf("native test namespace release failure for %#x", port)
	}
	return f.nativeDarwinMachCalls.modRefs(port, right, delta)
}

// Faults intercept real ownership calls, not their effects: successfully
// acquired/released rights are still kernel-owned facts on either side.
func DarwinFaultNamespace(d Debugger, setupCall, releaseCall int) (func(), error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return nil, err
	}
	if setupCall < 0 || setupCall > 11 || releaseCall < 0 {
		return nil, fmt.Errorf("invalid namespace fault boundary")
	}
	fault := &darwinNamespaceFault{failSetup: setupCall, failRelease: releaseCall}
	fault.held.Store(releaseCall != 0)
	err = d.(*engine).dispatch(func() error {
		b.namespace.mu.Lock()
		defer b.namespace.mu.Unlock()
		if b.teardown.Load() || b.namespace.calls != nil || (setupCall != 0 && b.taskOK) {
			return fmt.Errorf("namespace fault must precede its acquisition or release")
		}
		b.namespace.calls = fault
		return nil
	})
	return func() { fault.held.Store(false) }, err
}

type DarwinNamespaceLeak struct {
	PortSet  uint32
	DeadName uint32
	mu       sync.Mutex
	receive  bool
	send     bool
}

func DarwinAllocateNamespaceLeak() (leak *DarwinNamespaceLeak, err error) {
	ops := nativeDarwinMachCalls{}
	leak = &DarwinNamespaceLeak{}
	leak.PortSet, err = ops.allocate(C.MACH_PORT_RIGHT_PORT_SET)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, leak.Close())
		}
	}()
	port, err := ops.allocate(C.MACH_PORT_RIGHT_RECEIVE)
	if err != nil {
		return leak, err
	}
	leak.DeadName = port
	leak.receive = true
	if err = ops.makeSend(port); err != nil {
		return leak, err
	}
	leak.send = true
	if err = ops.modRefs(port, C.MACH_PORT_RIGHT_RECEIVE, -1); err != nil {
		return leak, err
	}
	leak.receive = false
	return leak, nil
}

func (leak *DarwinNamespaceLeak) Close() error {
	leak.mu.Lock()
	defer leak.mu.Unlock()
	ops := nativeDarwinMachCalls{}
	if leak.receive {
		port := C.mach_port_t(leak.DeadName)
		if err := releaseMachReceiver(ops, &port, &leak.send); err != nil {
			return err
		}
		leak.DeadName = 0
		leak.receive = false
	} else if leak.DeadName != 0 {
		if err := releaseMachSendRefs(ops, leak.DeadName, 1, false); err != nil {
			return err
		}
		leak.DeadName = 0
		leak.send = false
	}
	if leak.PortSet != 0 {
		if err := ops.modRefs(leak.PortSet, C.MACH_PORT_RIGHT_PORT_SET, -1); err != nil {
			return err
		}
		leak.PortSet = 0
	}
	return nil
}

func DarwinHoldNamespaceReferences(d Debugger) ([]uint32, func() error, error) {
	b, err := darwinProbeBackend(d)
	if err != nil {
		return nil, nil, err
	}
	var names []uint32
	var mu sync.Mutex
	release := func() error {
		mu.Lock()
		defer mu.Unlock()
		for len(names) != 0 {
			if err := releaseMachSendRefs(nativeDarwinMachCalls{}, names[0], 1, false); err != nil {
				return err
			}
			names = names[1:]
		}
		return nil
	}
	err = d.(*engine).dispatch(func() error {
		if b.teardown.Load() {
			return fmt.Errorf("test references require an active native session")
		}
		probe, err := DarwinInspectNamespace(d)
		if err != nil {
			return err
		}
		for _, name := range append(probe.Threads, probe.Task) {
			if err := (nativeDarwinMachCalls{}).modRefs(name, C.MACH_PORT_RIGHT_SEND, 1); err != nil {
				return err
			}
			names = append(names, name)
		}
		return nil
	})
	if err != nil {
		cleanupErr := release()
		return slices.Clone(names), release, errors.Join(err, cleanupErr)
	}
	return slices.Clone(names), release, nil
}
