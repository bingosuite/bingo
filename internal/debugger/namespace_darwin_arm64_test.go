//go:build darwin && arm64 && bingonative

package debugger

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
)

const (
	testMachSend uint32 = iota
	testMachReceive
	testMachSendOnce
	testMachPortSet
	testMachDeadName
)

const testMachInvalidName = 15

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
	if len(f.rights[port]) == 0 {
		return 0, fmt.Errorf("invalid Mach name %#x", port)
	}
	return f.rights[port][right], nil
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

type machRefsRead struct {
	right uint32
	refs  uint32
	err   error
}

type machRefsMutation struct {
	right uint32
	delta int32
}

type machSendReleaseCalls struct {
	darwinMachCalls
	afterType func(uint32) error
	typeReads int
	refReads  []machRefsRead
	mutations []machRefsMutation
}

func (ops *machSendReleaseCalls) portType(port uint32) (uint32, error) {
	kind, err := ops.darwinMachCalls.portType(port)
	ops.typeReads++
	if err == nil && ops.afterType != nil {
		err = ops.afterType(port)
	}
	return kind, err
}

func (ops *machSendReleaseCalls) refs(port, right uint32) (uint32, error) {
	refs, err := ops.darwinMachCalls.refs(port, right)
	ops.refReads = append(ops.refReads, machRefsRead{right, refs, err})
	return refs, err
}

func (ops *machSendReleaseCalls) modRefs(port, right uint32, delta int32) error {
	ops.mutations = append(ops.mutations, machRefsMutation{right, delta})
	return ops.darwinMachCalls.modRefs(port, right, delta)
}

func TestDarwinSendReleaseHandlesDeathBetweenTypeAndRefs(t *testing.T) {
	for _, right := range []uint32{testMachSend, testMachSendOnce} {
		t.Run(fmt.Sprintf("right-%d", right), func(t *testing.T) {
			_, fake := newFakeMachNamespace()
			fake.rights[700] = map[uint32]uint32{right: 5}
			ops := &machSendReleaseCalls{darwinMachCalls: fake}
			ops.afterType = func(port uint32) error {
				if ops.typeReads == 1 {
					fake.rights[port] = map[uint32]uint32{testMachDeadName: 5}
				}
				return nil
			}
			if err := releaseMachSendRefs(ops, 700, 2, right == testMachSendOnce); err != nil {
				t.Fatal(err)
			}
			if ops.typeReads != 2 || !slices.Equal(ops.refReads, []machRefsRead{{right, 0, nil}, {testMachDeadName, 5, nil}}) {
				t.Fatalf("did not revalidate the zero-ref transition: types=%d refs=%v", ops.typeReads, ops.refReads)
			}
			if !slices.Equal(ops.mutations, []machRefsMutation{{testMachDeadName, -2}}) ||
				!maps.Equal(fake.rights[700], map[uint32]uint32{testMachDeadName: 3}) {
				t.Fatalf("release stole coalesced credits: mutations=%v remaining=%v", ops.mutations, fake.rights[700])
			}
		})
	}
}

func TestDarwinSendReleaseRejectsUnprovenZeroRefs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		right      uint32
		refs       uint32
		owned      uint32
		transition bool
		nextRight  uint32
		typeReads  int
		refReads   int
	}{
		{"stable-zero", testMachSend, 0, 1, false, 0, 2, 1},
		{"insufficient-send", testMachSend, 1, 2, false, 0, 1, 1},
		{"saturated-send", testMachSend, darwinMaxUserRefs, 1, false, 0, 1, 1},
		{"zero-dead-name", testMachDeadName, 0, 1, false, 0, 1, 1},
		{"unexpected-type", testMachReceive, 1, 1, false, 0, 1, 0},
		{"transition-insufficient", testMachSend, 1, 2, true, testMachDeadName, 2, 2},
		{"transition-saturated", testMachSend, darwinMaxUserRefs, 1, true, testMachDeadName, 2, 2},
		{"transition-zero-dead-name", testMachSend, 0, 1, true, testMachDeadName, 2, 2},
		{"transition-unexpected", testMachSend, 1, 1, true, testMachReceive, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fake := newFakeMachNamespace()
			fake.rights[700] = map[uint32]uint32{tc.right: tc.refs}
			ops := &machSendReleaseCalls{darwinMachCalls: fake}
			ops.afterType = func(port uint32) error {
				if tc.transition && ops.typeReads == 1 {
					fake.rights[port] = map[uint32]uint32{tc.nextRight: tc.refs}
				}
				if tc.name == "stable-zero" && ops.typeReads == 2 {
					fake.rights[port][testMachSend] = 5
				}
				return nil
			}
			if err := releaseMachSendRefs(ops, 700, tc.owned, false); err == nil {
				t.Fatal("unproven reference retirement succeeded")
			}
			if ops.typeReads != tc.typeReads || len(ops.refReads) != tc.refReads || len(ops.mutations) != 0 {
				t.Fatalf("release was not bounded and non-mutating: types=%d refs=%v mutations=%v",
					ops.typeReads, ops.refReads, ops.mutations)
			}
		})
	}
}

func TestDarwinNamespaceRetainsFailedTransitionRelease(t *testing.T) {
	b, fake := newFakeMachNamespace()
	b.threadPorts[701] = 2
	fake.rights[701][testMachSend] = 5
	fake.failRelease = 1
	ops := &machSendReleaseCalls{darwinMachCalls: fake}
	ops.afterType = func(port uint32) error {
		if port == 701 && ops.typeReads == 1 {
			fake.rights[port] = map[uint32]uint32{testMachDeadName: 5}
		}
		return nil
	}
	b.namespace.calls = ops
	markNamespaceComplete(b)
	if err := b.releaseMachNamespace(); err == nil {
		t.Fatal("cleanup did not reach the injected release failure")
	}
	if b.threadPorts[701] != 2 || !b.taskOK || b.namespace.released ||
		fake.rights[701][testMachDeadName] != 5 ||
		!slices.Equal(ops.mutations, []machRefsMutation{{testMachDeadName, -2}}) {
		t.Fatalf("failed transition release lost ownership: threads=%v rights=%v mutations=%v",
			b.threadPorts, fake.rights, ops.mutations)
	}
	fake.failRelease = 0
	if err := b.releaseMachNamespace(); err != nil {
		t.Fatal(err)
	}
	if len(b.threadPorts) != 0 || b.taskOK || !b.namespace.released ||
		len(fake.rights) != 1 || fake.rights[701][testMachDeadName] != 3 {
		t.Fatalf("retry did not preserve independent credits: threads=%v rights=%v", b.threadPorts, fake.rights)
	}
	before := fake.calls
	if err := b.releaseMachNamespace(); err != nil || fake.calls != before {
		t.Fatalf("completed release was not idempotent: err=%v", err)
	}
}

func TestDarwinSendReleaseNativeDeathBetweenTypeAndRefs(t *testing.T) {
	for _, owned := range []uint32{1, 2} {
		t.Run(fmt.Sprintf("owned-%d", owned), func(t *testing.T) {
			native := nativeDarwinMachCalls{}
			port, err := native.allocate(testMachReceive)
			if err != nil {
				t.Fatal(err)
			}
			receiveOwned := true
			var credits uint32
			t.Cleanup(func() {
				if receiveOwned {
					if err := native.modRefs(port, testMachReceive, -1); err != nil {
						t.Errorf("release test receiver: %v", err)
						return
					}
				}
				if credits != 0 {
					if err := native.modRefs(port, testMachDeadName, -int32(credits)); err != nil {
						t.Errorf("release remaining test credits: %v", err)
						return
					}
				}
				_, err := native.portType(port)
				var failure *darwinMachError
				if !errors.As(err, &failure) || failure.code != testMachInvalidName {
					t.Errorf("test-owned Mach name survived checked cleanup: %v", err)
				}
			})
			const independent = uint32(3)
			for range owned + independent {
				if err := native.makeSend(port); err != nil {
					t.Fatal(err)
				}
				credits++
			}
			ops := &machSendReleaseCalls{darwinMachCalls: native}
			ops.afterType = func(name uint32) error {
				if ops.typeReads != 1 {
					return nil
				}
				if err := native.modRefs(name, testMachReceive, -1); err != nil {
					return err
				}
				receiveOwned = false
				return nil
			}
			if err := releaseMachSendRefs(ops, port, owned, false); err != nil {
				t.Fatal(err)
			}
			credits -= owned
			if ops.typeReads != 2 ||
				!slices.Equal(ops.refReads, []machRefsRead{{testMachSend, 0, nil}, {testMachDeadName, owned + independent, nil}}) ||
				!slices.Equal(ops.mutations, []machRefsMutation{{testMachDeadName, -int32(owned)}}) {
				t.Fatalf("native zero-ref transition was not revalidated: types=%d refs=%v mutations=%v",
					ops.typeReads, ops.refReads, ops.mutations)
			}
			remaining, err := native.refs(port, testMachDeadName)
			if err != nil || remaining != independent {
				t.Fatalf("native release stole independent credits: remaining=%d err=%v", remaining, err)
			}
		})
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
