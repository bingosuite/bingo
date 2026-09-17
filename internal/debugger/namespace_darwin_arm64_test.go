//go:build darwin && arm64 && bingonative

package debugger

import (
	"errors"
	"fmt"
	"maps"
	"testing"
)

const (
	testMachSend uint32 = iota
	testMachReceive
	testMachSendOnce
	testMachPortSet
	testMachDeadName
)

type fakeMachNamespace struct {
	rights          map[uint32]map[uint32]uint32
	next            uint32
	watched         uint32
	previous        uint32
	calls           int
	failCall        int
	releases        int
	failRelease     int
	replyErr        error
	dieOnCancel     bool
	dieOnSendChange bool
}

func newFakeMachNamespace() (*darwinBackend, *fakeMachNamespace) {
	ops := &fakeMachNamespace{
		rights: map[uint32]map[uint32]uint32{
			700: {testMachSend: 1},
			701: {testMachSend: 1},
		},
		next: 100,
	}
	b := &darwinBackend{pid: 17, taskPort: 700, taskOK: true, threadPorts: map[uint32]uint32{701: 1}}
	b.namespace.calls = ops
	return b, ops
}

func (f *fakeMachNamespace) call() error {
	f.calls++
	if f.failCall == f.calls {
		return errors.New("injected Mach acquisition failure")
	}
	return nil
}

func (f *fakeMachNamespace) allocate(right uint32) (uint32, error) {
	if err := f.call(); err != nil {
		return 0, err
	}
	f.next++
	f.rights[f.next] = map[uint32]uint32{right: 1}
	return f.next, nil
}

func (f *fakeMachNamespace) makeSend(port uint32) error {
	if err := f.call(); err != nil {
		return err
	}
	f.rights[port][testMachSend]++
	return nil
}

func (f *fakeMachNamespace) moveMember(_, _ uint32) error { return f.call() }

func (f *fakeMachNamespace) fireNotification(task uint32) {
	f.rights[task][testMachDeadName] += f.rights[task][testMachSend] + 1
	delete(f.rights[task], testMachSend)
	f.watched = 0
}

func (f *fakeMachNamespace) notification(task, notify uint32) (uint32, error) {
	if err := f.call(); err != nil {
		return 0, err
	}
	if notify != 0 {
		f.watched = task
		return f.previous, nil
	}
	if f.dieOnCancel {
		f.dieOnCancel = false
		f.fireNotification(task)
		return 0, &darwinMachError{code: 4}
	}
	if f.watched == 0 {
		return 0, nil
	}
	f.watched = 0
	return f.allocate(testMachSendOnce)
}

func (f *fakeMachNamespace) portType(port uint32) (uint32, error) {
	if err := f.call(); err != nil {
		return 0, err
	}
	if len(f.rights[port]) == 0 {
		return 0, fmt.Errorf("invalid Mach name %#x", port)
	}
	var kind uint32
	for right := range f.rights[port] {
		kind |= 1 << (right + 16)
	}
	return kind, nil
}

func (f *fakeMachNamespace) refs(port, right uint32) (uint32, error) {
	if err := f.call(); err != nil {
		return 0, err
	}
	if refs := f.rights[port][right]; refs != 0 {
		return refs, nil
	}
	return 0, &darwinMachError{code: 17}
}

func (f *fakeMachNamespace) modRefs(port, right uint32, delta int32) error {
	if err := f.call(); err != nil {
		return err
	}
	f.releases++
	if f.failRelease == f.releases {
		return errors.New("injected Mach release failure")
	}
	if f.dieOnSendChange && right == testMachSend {
		f.dieOnSendChange = false
		f.rights[port][testMachDeadName] = f.rights[port][testMachSend]
		delete(f.rights[port], testMachSend)
		return &darwinMachError{code: 17}
	}
	before := f.rights[port][right]
	if delta >= 0 || int64(before)+int64(delta) < 0 || before == 0 {
		return fmt.Errorf("invalid reference decrement port=%#x right=%d delta=%d refs=%d", port, right, delta, before)
	}
	after := uint32(int64(before) + int64(delta))
	if after == 0 {
		delete(f.rights[port], right)
	} else {
		f.rights[port][right] = after
	}
	if right == testMachReceive && f.rights[port][testMachSend] != 0 {
		f.rights[port][testMachDeadName] = f.rights[port][testMachSend]
		delete(f.rights[port], testMachSend)
	}
	if len(f.rights[port]) == 0 {
		delete(f.rights, port)
	}
	return nil
}

func (f *fakeMachNamespace) retireReplies(*darwinBackend) error {
	if err := f.call(); err != nil {
		return err
	}
	return f.replyErr
}

func markNamespaceComplete(b *darwinBackend) {
	b.teardown.Store(true)
	b.waitAcknowledged.Store(true)
	b.targetReleased.Store(true)
}

func expectNamespaceReleased(t *testing.T, b *darwinBackend, f *fakeMachNamespace) {
	t.Helper()
	if len(f.rights) != 0 || b.portSet != 0 || b.excPort != 0 || b.notePort != 0 ||
		b.ctrlPort != 0 || b.taskOK || b.taskPort != 0 || len(b.threadPorts) != 0 ||
		!b.namespace.released || b.portsOK {
		t.Fatalf("namespace not fully retired: remaining=%v released=%v", f.rights, b.namespace.released)
	}
	before := f.calls
	if err := b.releaseMachNamespace(); err != nil {
		t.Fatal(err)
	}
	if f.calls != before {
		t.Fatal("idempotent release touched a retired Mach name")
	}
}

func TestDarwinNamespaceUnwindsEveryPartialAcquisition(t *testing.T) {
	control, calls := newFakeMachNamespace()
	if err := control.setupMachNamespace(control.taskPort); err != nil {
		t.Fatal(err)
	}
	setupCalls := calls.calls
	if setupCalls != 11 {
		t.Fatalf("setup made %d ownership calls, update the partial-acquisition coverage", setupCalls)
	}
	for failed := 1; failed <= setupCalls; failed++ {
		t.Run(fmt.Sprintf("call-%d", failed), func(t *testing.T) {
			b, ops := newFakeMachNamespace()
			ops.failCall = failed
			if err := b.setupMachNamespace(b.taskPort); err == nil {
				t.Fatal("setup did not reach the injected failure")
			}
			if b.portsOK {
				t.Fatal("partial setup published a complete receive path")
			}
			if err := b.unwindMachSetup(); err != nil {
				t.Fatalf("unwind partial setup: %v", err)
			}
			expectNamespaceReleased(t, b, ops)
		})
	}
}

func TestDarwinNamespaceRetriesEveryPartialRelease(t *testing.T) {
	control, calls := newFakeMachNamespace()
	if err := control.setupMachNamespace(control.taskPort); err != nil {
		t.Fatal(err)
	}
	markNamespaceComplete(control)
	if err := control.releaseMachNamespace(); err != nil {
		t.Fatal(err)
	}
	if calls.releases != 10 {
		t.Fatalf("release made %d mutations, update failure coverage", calls.releases)
	}
	for failed := 1; failed <= calls.releases; failed++ {
		t.Run(fmt.Sprintf("release-%d", failed), func(t *testing.T) {
			b, ops := newFakeMachNamespace()
			if err := b.setupMachNamespace(b.taskPort); err != nil {
				t.Fatal(err)
			}
			markNamespaceComplete(b)
			ops.failRelease = failed
			if err := b.releaseMachNamespace(); err == nil {
				t.Fatal("cleanup did not reach the injected failure")
			}
			if b.namespace.released {
				t.Fatal("failed release was reported as complete")
			}
			ops.failRelease = 0
			if err := b.releaseMachNamespace(); err != nil {
				t.Fatalf("retry lost a partially released name/right: %v", err)
			}
			expectNamespaceReleased(t, b, ops)
		})
	}
}

func TestDarwinNamespaceGuardPrecedesEveryOwnershipCall(t *testing.T) {
	for _, missing := range []string{"latch", "waiter", "target", "attached", "registry"} {
		t.Run(missing, func(t *testing.T) {
			b, ops := newFakeMachNamespace()
			markNamespaceComplete(b)
			switch missing {
			case "latch":
				b.teardown.Store(false)
			case "waiter":
				b.waitAcknowledged.Store(false)
			case "target":
				b.targetReleased.Store(false)
			case "attached":
				b.attachOwned.Store(true)
			case "registry":
				outstandingDarwinDetaches.Store(b, struct{}{})
				defer outstandingDarwinDetaches.Delete(b)
			}
			if err := b.releaseMachNamespace(); err == nil || ops.calls != 0 {
				t.Fatalf("missing %s allowed namespace access: calls=%d err=%v", missing, ops.calls, err)
			}
		})
	}
}

func TestDarwinNamespaceRetiresNotificationCreditEvenAfterDelivery(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("death-during-cancel=%v", race), func(t *testing.T) {
			b, ops := newFakeMachNamespace()
			if err := b.setupMachNamespace(b.taskPort); err != nil {
				t.Fatal(err)
			}
			if race {
				ops.dieOnCancel = true
			} else {
				ops.fireNotification(700)
			}
			markNamespaceComplete(b)
			if err := b.releaseMachNamespace(); err != nil {
				t.Fatal(err)
			}
			expectNamespaceReleased(t, b, ops)
		})
	}
}

func TestDarwinNamespaceDoesNotStealCoalescedUserReferences(t *testing.T) {
	b, ops := newFakeMachNamespace()
	ops.rights[700][testMachSend]++
	ops.rights[701][testMachSend] = 5
	b.threadPorts[701] = 2
	prior, err := ops.allocate(testMachSendOnce)
	if err != nil {
		t.Fatal(err)
	}
	ops.previous = prior
	if err := b.setupMachNamespace(b.taskPort); err != nil {
		t.Fatal(err)
	}
	exc := uint32(b.excPort)
	ops.rights[exc][testMachSend]++
	ops.fireNotification(700)
	markNamespaceComplete(b)
	if err := b.releaseMachNamespace(); err != nil {
		t.Fatal(err)
	}
	for port, want := range map[uint32]map[uint32]uint32{
		700: {testMachDeadName: 1},
		701: {testMachSend: 3},
		exc: {testMachDeadName: 1},
	} {
		if !maps.Equal(ops.rights[port], want) {
			t.Errorf("port %#x: remaining %v, want other owner's %v", port, ops.rights[port], want)
		}
	}
	if len(ops.rights) != 3 {
		t.Fatalf("leaked namespace or notification send-once right: %v", ops.rights)
	}
}

func TestDarwinNamespaceHonorsTheAttachedRPCSendDrop(t *testing.T) {
	b, ops := newFakeMachNamespace()
	if err := b.setupMachNamespace(b.taskPort); err != nil {
		t.Fatal(err)
	}
	if err := ops.modRefs(uint32(b.excPort), testMachSend, -1); err != nil {
		t.Fatal(err)
	}
	b.attachState.sendDropped = true
	b.namespace.excSend = false
	markNamespaceComplete(b)
	if err := b.releaseMachNamespace(); err != nil {
		t.Fatal(err)
	}
	expectNamespaceReleased(t, b, ops)
}

func TestDarwinNamespaceRetainsReceiversUntilRepliesRetire(t *testing.T) {
	b, ops := newFakeMachNamespace()
	if err := b.setupMachNamespace(b.taskPort); err != nil {
		t.Fatal(err)
	}
	markNamespaceComplete(b)
	ops.replyErr = errors.New("pending exception reply")
	if err := b.releaseMachNamespace(); !errors.Is(err, ops.replyErr) {
		t.Fatalf("pending reply = %v", err)
	}
	for _, port := range []uint32{uint32(b.excPort), uint32(b.notePort), uint32(b.ctrlPort)} {
		if port == 0 || ops.rights[port][testMachReceive] != 1 {
			t.Fatal("a pending reply lost a receiver")
		}
	}
	ops.replyErr = nil
	if err := b.releaseMachNamespace(); err != nil {
		t.Fatal(err)
	}
	expectNamespaceReleased(t, b, ops)
}

func TestDarwinSendReleaseHandlesDeathBetweenTypeAndMutation(t *testing.T) {
	_, ops := newFakeMachNamespace()
	ops.rights[700][testMachSend] = 2
	ops.dieOnSendChange = true
	if err := releaseMachSendRefs(ops, 700, 1, false); err != nil {
		t.Fatal(err)
	}
	if ops.rights[700][testMachDeadName] != 1 {
		t.Fatalf("send-to-dead transition lost another owner's ref: %v", ops.rights[700])
	}
}

func TestDarwinNamespaceRejectsMissingOrSaturatedOwnedReferences(t *testing.T) {
	for _, refs := range []uint32{0, darwinMaxUserRefs} {
		t.Run(fmt.Sprintf("refs-%d", refs), func(t *testing.T) {
			b, ops := newFakeMachNamespace()
			if refs == 0 {
				delete(ops.rights, 700)
			} else {
				ops.rights[700][testMachSend] = refs
			}
			markNamespaceComplete(b)
			if err := b.releaseMachNamespace(); err == nil {
				t.Fatal("unprovable reference retirement succeeded")
			}
			if !b.taskOK || b.taskPort != 700 || b.namespace.released {
				t.Fatal("failed task release erased its ownership")
			}
		})
	}
}
