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
	"sync"
)

type darwinNamespace struct {
	mu                    sync.Mutex
	calls                 darwinMachCalls
	excSend               bool
	noteSend              bool
	ctrlSend              bool
	notificationPending   bool
	notificationDeadRef   uint32
	previousNotification  uint32
	cancelledNotification uint32
	released              bool
}

// The seam covers ownership-changing calls, including failures after only some
// rights exist. A name is never cleared until its last owned right is released.
type darwinMachCalls interface {
	allocate(uint32) (uint32, error)
	makeSend(uint32) error
	moveMember(uint32, uint32) error
	notification(uint32, uint32) (uint32, error)
	portType(uint32) (uint32, error)
	refs(uint32, uint32) (uint32, error)
	modRefs(uint32, uint32, int32) error
	retireReplies(*darwinBackend) error
}

type nativeDarwinMachCalls struct{}

// XNU pegs its 16-bit uref counter on overflow; a decrement then need not
// reclaim anything. Such a name cannot be reported as successfully released.
const darwinMaxUserRefs = 0xffff

type darwinMachError struct {
	op   string
	port uint32
	code C.kern_return_t
}

func (err *darwinMachError) Error() string {
	return fmt.Sprintf("%s(%#x): %s", err.op, err.port, machErrString(err.code))
}

func machRightChanged(err error) bool {
	var failure *darwinMachError
	return errors.As(err, &failure) &&
		(failure.code == C.KERN_INVALID_RIGHT || failure.code == C.KERN_INVALID_ARGUMENT)
}

func (nativeDarwinMachCalls) allocate(right uint32) (uint32, error) {
	var port C.mach_port_t
	if kr := C.mach_port_allocate(C.mach_task_self_, C.mach_port_right_t(right), &port); kr != C.KERN_SUCCESS {
		return 0, &darwinMachError{op: "mach_port_allocate", code: kr}
	}
	return uint32(port), nil
}

func (nativeDarwinMachCalls) makeSend(port uint32) error {
	if kr := C.mach_port_insert_right(C.mach_task_self_, C.mach_port_t(port),
		C.mach_port_t(port), C.MACH_MSG_TYPE_MAKE_SEND); kr != C.KERN_SUCCESS {
		return &darwinMachError{op: "mach_port_insert_right", port: port, code: kr}
	}
	return nil
}

func (nativeDarwinMachCalls) moveMember(port, set uint32) error {
	if kr := C.mach_port_move_member(C.mach_task_self_, C.mach_port_t(port),
		C.mach_port_t(set)); kr != C.KERN_SUCCESS {
		return &darwinMachError{op: "mach_port_move_member", port: port, code: kr}
	}
	return nil
}

func (nativeDarwinMachCalls) notification(task, notify uint32) (uint32, error) {
	var previous C.mach_port_t
	if kr := C.mach_port_request_notification(C.mach_task_self_, C.mach_port_t(task),
		C.MACH_NOTIFY_DEAD_NAME, 0, C.mach_port_t(notify), C.MACH_MSG_TYPE_MAKE_SEND_ONCE,
		&previous); kr != C.KERN_SUCCESS {
		return 0, &darwinMachError{op: "mach_port_request_notification", port: task, code: kr}
	}
	return uint32(previous), nil
}

func (nativeDarwinMachCalls) portType(port uint32) (uint32, error) {
	var kind C.mach_port_type_t
	if kr := C.mach_port_type(C.mach_task_self_, C.mach_port_t(port), &kind); kr != C.KERN_SUCCESS {
		return 0, &darwinMachError{op: "mach_port_type", port: port, code: kr}
	}
	return uint32(kind), nil
}

func (nativeDarwinMachCalls) refs(port, right uint32) (uint32, error) {
	var refs C.mach_port_urefs_t
	if kr := C.mach_port_get_refs(C.mach_task_self_, C.mach_port_t(port),
		C.mach_port_right_t(right), &refs); kr != C.KERN_SUCCESS {
		return 0, &darwinMachError{op: "mach_port_get_refs", port: port, code: kr}
	}
	return uint32(refs), nil
}

func (nativeDarwinMachCalls) modRefs(port, right uint32, delta int32) error {
	if kr := C.mach_port_mod_refs(C.mach_task_self_, C.mach_port_t(port),
		C.mach_port_right_t(right), C.mach_port_delta_t(delta)); kr != C.KERN_SUCCESS {
		return &darwinMachError{op: "mach_port_mod_refs", port: port, code: kr}
	}
	return nil
}

func (nativeDarwinMachCalls) retireReplies(b *darwinBackend) error {
	ctx, cancel := context.WithTimeout(context.Background(), attachedDetachTimeout)
	defer cancel()
	if b.excPort != C.MACH_PORT_NULL {
		for {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("drain retired exception queue: %w", err)
			}
			cls, _, err := b.receiveMachMessageFrom(b.excPort, 0)
			if err != nil {
				return err
			}
			if cls == C.BINGO_MSG_NONE {
				break
			}
		}
	}
	return b.flushAllReplies()
}

func (ns *darwinNamespace) operations() darwinMachCalls {
	if ns.calls != nil {
		return ns.calls
	}
	return nativeDarwinMachCalls{}
}

// The victim's handler is installed only after every receiver is in the set;
// partial setup must never redirect exceptions into an unserviceable path.
func (b *darwinBackend) setupMachNamespace(task C.mach_port_t) error {
	ns := &b.namespace
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ops := ns.operations()
	for _, entry := range []struct {
		port *C.mach_port_t
		send *bool
	}{
		{&b.excPort, &ns.excSend},
		{&b.notePort, &ns.noteSend},
		{&b.ctrlPort, &ns.ctrlSend},
	} {
		port, err := ops.allocate(C.MACH_PORT_RIGHT_RECEIVE)
		if err != nil {
			return err
		}
		*entry.port = C.mach_port_t(port)
		if err := ops.makeSend(port); err != nil {
			return err
		}
		*entry.send = true
	}
	previous, err := ops.notification(uint32(task), uint32(b.notePort))
	if err != nil {
		return err
	}
	ns.previousNotification = previous
	ns.notificationPending = true
	set, err := ops.allocate(C.MACH_PORT_RIGHT_PORT_SET)
	if err != nil {
		return err
	}
	b.portSet = C.mach_port_t(set)
	for _, port := range []C.mach_port_t{b.excPort, b.notePort, b.ctrlPort} {
		if err := ops.moveMember(uint32(port), set); err != nil {
			return err
		}
	}
	b.portsOK = true
	return nil
}

func (b *darwinBackend) cancelMachNotification(ops darwinMachCalls) error {
	ns := &b.namespace
	if ns.notificationPending {
		task := uint32(b.taskPort)
		previous, err := ops.notification(task, 0)
		if machRightChanged(err) {
			kind, typeErr := ops.portType(task)
			if typeErr != nil {
				return errors.Join(err, typeErr)
			}
			if kind&C.MACH_PORT_TYPE_DEAD_NAME != 0 {
				// ipc_right_check may convert SEND to DEAD_NAME inside the
				// first cancellation. The now-empty request can be canceled.
				previous, err = ops.notification(task, 0)
			}
		}
		if err != nil {
			return err
		}
		if previous == 0 {
			kind, err := ops.portType(task)
			if err != nil {
				return err
			}
			if kind&C.MACH_PORT_TYPE_DEAD_NAME == 0 {
				return fmt.Errorf("owned dead-name notification disappeared from live task %#x", task)
			}
			// XNU adds this uref when it converts the watched entry, not when
			// userspace receives the notification. Wait may already have
			// consumed the message; either way exactly this one ref is ours.
			ns.notificationDeadRef = 1
		}
		ns.cancelledNotification = previous
		ns.notificationPending = false
	}
	for _, port := range []*uint32{&ns.previousNotification, &ns.cancelledNotification} {
		if *port == 0 || *port == uint32(C.MACH_PORT_DEAD) {
			*port = 0
			continue
		}
		if err := releaseMachSendRefs(ops, *port, 1, true); err != nil {
			return fmt.Errorf("release notification send-once right: %w", err)
		}
		*port = 0
	}
	return nil
}

// Only the recorded urefs belong to this backend. task_for_pid, task_threads
// and exception descriptors can coalesce with rights held by another owner.
func releaseMachSendRefs(ops darwinMachCalls, port, owned uint32, once bool) error {
	for attempt := 0; attempt < 2; attempt++ {
		kind, err := ops.portType(port)
		if err != nil {
			return err
		}
		var right uint32
		switch {
		case kind&C.MACH_PORT_TYPE_DEAD_NAME != 0:
			right = C.MACH_PORT_RIGHT_DEAD_NAME
		case once && kind&C.MACH_PORT_TYPE_SEND_ONCE != 0:
			right = C.MACH_PORT_RIGHT_SEND_ONCE
		case !once && kind&C.MACH_PORT_TYPE_SEND != 0:
			right = C.MACH_PORT_RIGHT_SEND
		default:
			return fmt.Errorf("owned send right %#x has unexpected type %#x", port, kind)
		}
		refs, err := ops.refs(port, right)
		if err == nil {
			if refs < owned || refs == darwinMaxUserRefs {
				return fmt.Errorf("port %#x has %d urefs, cannot release %d owned urefs", port, refs, owned)
			}
			err = ops.modRefs(port, right, -int32(owned))
		}
		if err == nil || !machRightChanged(err) || attempt != 0 {
			return err
		}
	}
	return fmt.Errorf("port %#x changed right type during release", port)
}

func releaseMachReceiver(ops darwinMachCalls, port *C.mach_port_t, send *bool) error {
	if *port == C.MACH_PORT_NULL {
		return nil
	}
	name := uint32(*port)
	kind, err := ops.portType(name)
	if err != nil {
		return err
	}
	if kind&C.MACH_PORT_TYPE_RECEIVE == 0 {
		return fmt.Errorf("owned receive right %#x has unexpected type %#x", name, kind)
	}
	if *send {
		if err := releaseMachSendRefs(ops, name, 1, false); err != nil {
			return err
		}
		*send = false
	}
	if err := ops.modRefs(name, C.MACH_PORT_RIGHT_RECEIVE, -1); err != nil {
		return err
	}
	*port = C.MACH_PORT_NULL
	return nil
}

func (b *darwinBackend) releaseMachNamespace() error {
	ns := &b.namespace
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if !b.canReleaseMachNamespace() {
		return fmt.Errorf("Mach namespace release requires completed target and waiter teardown")
	}
	if ns.released {
		return nil
	}
	ops := ns.operations()
	if err := b.cancelMachNotification(ops); err != nil {
		return err
	}
	if err := ops.retireReplies(b); err != nil {
		return err
	}
	for _, entry := range []struct {
		port *C.mach_port_t
		send *bool
	}{
		{&b.excPort, &ns.excSend},
		{&b.ctrlPort, &ns.ctrlSend},
		{&b.notePort, &ns.noteSend},
	} {
		if err := releaseMachReceiver(ops, entry.port, entry.send); err != nil {
			return err
		}
	}
	if b.portSet != C.MACH_PORT_NULL {
		if err := ops.modRefs(uint32(b.portSet), C.MACH_PORT_RIGHT_PORT_SET, -1); err != nil {
			return err
		}
		b.portSet = C.MACH_PORT_NULL
	}
	b.portsOK = false

	b.threadMu.Lock()
	defer b.threadMu.Unlock()
	ports := make([]uint32, 0, len(b.threadPorts))
	for port := range b.threadPorts {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	for _, port := range ports {
		if err := releaseMachSendRefs(ops, port, b.threadPorts[port], false); err != nil {
			return fmt.Errorf("release retained thread: %w", err)
		}
		delete(b.threadPorts, port)
		delete(b.threadHolds, int(port))
	}
	b.taskMu.Lock()
	defer b.taskMu.Unlock()
	if b.taskOK {
		if err := releaseMachSendRefs(ops, uint32(b.taskPort), 1+ns.notificationDeadRef, false); err != nil {
			return fmt.Errorf("release cached task: %w", err)
		}
		b.taskPort = C.MACH_PORT_NULL
		b.taskOK = false
		ns.notificationDeadRef = 0
	}
	ns.released = true
	return nil
}

func (b *darwinBackend) releaseBackendResources() error {
	if !b.teardown.Load() || !b.waitAcknowledged.Load() {
		return fmt.Errorf("backend retirement requires acknowledged waiter teardown")
	}
	// Failed launch setup has a spawned PID but no process.live handle yet.
	// The backend keeps that obligation until the exact child is reaped.
	if b.launched && !b.targetReleased.Load() {
		if err := killProcess(b, b.pid, nil, false); err != nil {
			return err
		}
	}
	return b.releaseMachNamespace()
}

func (b *darwinBackend) unwindMachSetup() (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: unwind Mach setup: %w", ErrBackendCleanupIncomplete, err)
		}
	}()
	// Startup has not created a waiter or diagnostic reader. It can acknowledge
	// that absence itself, but still goes through the same durable COMPLETE guard.
	if err := b.beginTeardown(); err != nil {
		return err
	}
	if err := b.acknowledgeWait(context.Background()); err != nil {
		return err
	}
	return b.releaseBackendResources()
}
