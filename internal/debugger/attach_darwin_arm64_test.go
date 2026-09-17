//go:build darwin && arm64 && bingonative

package debugger

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestDarwinExceptionRepliesDistinguishDeadFromUnknownRights(t *testing.T) {
	b := &darwinBackend{pendingReplies: map[int][]replyInfo{
		17: {{port: 0xffffffff, bits: 18, id: 2401}, {port: 0xffffffff, bits: 18, id: 2401}},
	}}
	if err := b.flushReply(17); err != nil {
		t.Fatalf("retire dead reply sentinels without a namespace deallocation: %v", err)
	}
	if len(b.pendingReplies) != 0 {
		t.Fatal("a received dead sentinel retained a nonexistent reply right")
	}
	b.pendingReplies[17] = []replyInfo{{port: 0, bits: 18, id: 2401}}
	if err := b.flushReply(17); err == nil {
		t.Fatal("an unknown null reply right was mistaken for confirmed retirement")
	}
	if len(b.pendingReplies[17]) != 1 {
		t.Fatal("failed reply lost its outstanding obligation")
	}
}

func TestDarwinNamespaceCompletionAfterAttachFailedBeforeTheSwap(t *testing.T) {
	b := &darwinBackend{pid: 17}
	if err := b.beginTeardown(); err != nil {
		t.Fatal(err)
	}
	if b.canReleaseMachNamespace() {
		t.Fatal("failed attach bypassed waiter acknowledgement")
	}
	if err := b.acknowledgeWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !b.canReleaseMachNamespace() {
		t.Fatal("a PID recorded before a failed swap was mistaken for victim ownership")
	}
}

func TestDarwinRestorationRequiresAnOwnedQuiescentTask(t *testing.T) {
	for _, missing := range []string{"acknowledgement", "hold", "quiescence"} {
		t.Run(missing, func(t *testing.T) {
			b := &darwinBackend{}
			b.teardown.Store(true)
			b.attachOwned.Store(true)
			b.waitAcknowledged.Store(missing != "acknowledgement")
			b.attachState.taskHeld = missing != "hold"
			b.attachState.quiesced = missing != "quiescence"
			err := b.WriteMemory(0x1000, []byte{0x1f, 0x20, 0x03, 0xd5})
			if !errors.Is(err, ErrAttachedDetachIncomplete) {
				t.Fatalf("restoration without %s: %v", missing, err)
			}
			if b.taskOK {
				t.Fatal("restoration acquired a task capability before proving its hold")
			}
			if err := b.prepareAttachedRendezvous(context.Background()); err == nil {
				t.Fatalf("rendezvous without %s was accepted", missing)
			}
			if b.taskOK {
				t.Fatal("rendezvous acquired a task capability before proving its hold")
			}
		})
	}
}

func TestDarwinRendezvousExcludesTheSavedEntryClass(t *testing.T) {
	for _, tc := range []struct {
		entry uint32
		owned bool
		want  uint32
	}{
		{darwinExecuteEC, false, darwinStepEC},
		{darwinExecuteEC, true, darwinStepEC},
		{darwinStepEC, false, darwinExecuteEC},
		{darwinBRKEC, false, darwinExecuteEC},
		{0x15, false, 0},
		{0x15, true, darwinExecuteEC},
		{0, false, 0},
		{0, true, darwinExecuteEC},
	} {
		if got := darwinRendezvousClass(tc.entry, tc.owned); got != tc.want {
			t.Errorf("entry=%#x owned=%v: got %#x, want %#x", tc.entry, tc.owned, got, tc.want)
		}
	}
}

func TestDarwinRendezvousRequiresAFreshProgrammedClassAndExactExecutePC(t *testing.T) {
	r := darwinRendezvous{expected: darwinExecuteEC, pc: 0x1000}
	if matches, err := r.matches(darwinExecuteEC, 0x1000); err != nil || matches {
		t.Fatalf("unprogrammed rendezvous matched: %v, %v", matches, err)
	}
	r.armed = true
	for _, old := range []uint32{darwinBRKEC, darwinStepEC} {
		if matches, err := r.matches(old, 0x1000); err != nil || matches {
			t.Fatalf("older class %#x matched: %v, %v", old, matches, err)
		}
	}
	if matches, err := r.matches(0x15, 0x1000); err == nil || matches {
		t.Fatalf("non-debug exception accepted: %v, %v", matches, err)
	}
	if matches, err := r.matches(darwinExecuteEC, 0x1004); err == nil || matches {
		t.Fatalf("wrong execution PC accepted: %v, %v", matches, err)
	}
	if matches, err := r.matches(darwinExecuteEC, 0x1000); err != nil || !matches {
		t.Fatalf("fresh exact-PC execution marker rejected: %v, %v", matches, err)
	}
	r.expected = darwinStepEC
	if matches, err := r.matches(darwinExecuteEC, 0x1000); err != nil || matches {
		t.Fatalf("older hardware class matched the step fallback: %v, %v", matches, err)
	}
	if matches, err := r.matches(darwinStepEC, 0x2000); err != nil || !matches {
		t.Fatalf("step landing at a branch destination rejected: %v, %v", matches, err)
	}
}

func TestDarwinRendezvousPreservesUnsupportedDebugState(t *testing.T) {
	var r darwinRendezvous
	if !darwinDebugStateEmpty(r.prior, false) {
		t.Fatal("empty debug state rejected")
	}
	r.prior.__mdscr_el1 = 1
	if darwinDebugStateEmpty(r.prior, false) || !darwinDebugStateEmpty(r.prior, true) {
		t.Fatal("only an owned hardware step may be replaced")
	}
	r.prior.__mdscr_el1 |= 2
	if darwinDebugStateEmpty(r.prior, true) {
		t.Fatal("foreign debug control accepted beside an owned step")
	}
	r = darwinRendezvous{}
	for slot := range r.prior.__bcr {
		r.prior.__bcr[slot] = 4
		r.prior.__wcr[slot] = 4
	}
	if !darwinDebugStateEmpty(r.prior, true) || darwinDebugStateEmpty(r.prior, false) {
		t.Fatal("XNU's disabled comparator normalization must be tied to the owned step")
	}
	for slot := 0; slot < len(r.prior.__bcr); slot++ {
		r = darwinRendezvous{}
		r.prior.__bcr[slot] = 1
		if darwinDebugStateEmpty(r.prior, true) {
			t.Fatalf("execution comparator %d ignored", slot)
		}
		r = darwinRendezvous{}
		r.prior.__bvr[slot] = 4
		if darwinDebugStateEmpty(r.prior, false) {
			t.Fatalf("foreign comparator address %d ignored", slot)
		}
		r = darwinRendezvous{}
		r.prior.__wcr[slot] = 1
		if darwinDebugStateEmpty(r.prior, false) {
			t.Fatalf("watchpoint %d ignored", slot)
		}
		r = darwinRendezvous{}
		r.prior.__wvr[slot] = 4
		if darwinDebugStateEmpty(r.prior, false) {
			t.Fatalf("foreign watchpoint address %d ignored", slot)
		}
	}
}

func TestDarwinRendezvousDoesNotSampleRegistersWithoutAnOutstandingRPC(t *testing.T) {
	b := &darwinBackend{}
	r := darwinRendezvous{armed: true, expected: darwinExecuteEC, pc: 0x1000}
	if acknowledged, err := b.acknowledgeAttachedRendezvous(context.Background(), 0, &r); err != nil || acknowledged {
		t.Fatalf("absent RPC attempted native inspection: %v, %v", acknowledged, err)
	}
	if r.acknowledged || r.complete {
		t.Fatal("absent RPC completed a rendezvous")
	}
	r.acknowledged = true
	b.threadHolds = map[int]int{0: 1}
	if acknowledged, err := b.acknowledgeAttachedRendezvous(context.Background(), 0, &r); err == nil || acknowledged {
		t.Fatalf("failed debug restoration reported completion: %v, %v", acknowledged, err)
	}
	if !r.acknowledged || r.complete {
		t.Fatal("failed restoration lost its existing acknowledgement")
	}
}

func TestDarwinExceptionRestorationPreservesEveryTuple(t *testing.T) {
	tuples := []darwinExceptionTuple{
		{mask: 64, port: 123, behavior: 1, flavor: 6},
		{mask: 128, port: 0, behavior: 0, flavor: 0},
		{mask: 256, port: 456, behavior: 3, flavor: 2},
	}
	var state darwinExceptionState
	for _, tuple := range tuples {
		state.saved = append(state.saved, darwinSavedException{tuple: tuple})
	}
	var restored []darwinExceptionTuple
	set := func(tuple darwinExceptionTuple) error {
		restored = append(restored, tuple)
		return nil
	}
	if err := state.restore(set); err != nil {
		t.Fatal(err)
	}
	if err := state.restore(set); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, tuples) {
		t.Fatalf("restored = %+v, want exact tuples once: %+v", restored, tuples)
	}
	var released []uint32
	release := func(port uint32) error {
		released = append(released, port)
		return nil
	}
	for i := 0; i < 2; i++ {
		if err := state.release(release); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(released, []uint32{123, 456}) {
		t.Fatalf("released = %v, want only non-null send rights exactly once", released)
	}
}

func TestDarwinExceptionRestorationExplicitlyClearsAnEmptySnapshot(t *testing.T) {
	var state darwinExceptionState
	var restored []darwinExceptionTuple
	if err := state.restore(func(tuple darwinExceptionTuple) error {
		restored = append(restored, tuple)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []darwinExceptionTuple{{mask: 64, port: 0, behavior: 1, flavor: 5}}
	if !reflect.DeepEqual(restored, want) {
		t.Fatalf("empty snapshot restore = %+v, want explicit ARM64 default/none clear %+v", restored, want)
	}
}

func TestDarwinExceptionRestorationRetainsFailedObligations(t *testing.T) {
	fault := errors.New("injected Mach failure")
	state := darwinExceptionState{saved: []darwinSavedException{
		{tuple: darwinExceptionTuple{mask: 64, port: 123}},
		{tuple: darwinExceptionTuple{mask: 128, port: 456}},
	}}
	var sets []uint32
	fail := true
	set := func(tuple darwinExceptionTuple) error {
		sets = append(sets, tuple.port)
		if fail && tuple.port == 456 {
			return fault
		}
		return nil
	}
	if err := state.restore(set); !errors.Is(err, fault) {
		t.Fatalf("restore = %v, want original error", err)
	}
	if state.restored || !state.saved[0].restored || state.saved[1].restored {
		t.Fatal("failed restore forgot or prematurely completed a tuple")
	}
	if err := state.release(func(uint32) error {
		t.Fatal("released a saved right before complete restoration")
		return nil
	}); err == nil {
		t.Fatal("incomplete restoration permitted saved-right release")
	}
	fail = false
	if err := state.restore(set); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sets, []uint32{123, 456, 456}) {
		t.Fatalf("restore retries = %v", sets)
	}
	fail = true
	var releases []uint32
	release := func(port uint32) error {
		releases = append(releases, port)
		if fail && port == 456 {
			return fault
		}
		return nil
	}
	if err := state.release(release); !errors.Is(err, fault) {
		t.Fatalf("release = %v, want original error", err)
	}
	fail = false
	if err := state.release(release); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(releases, []uint32{123, 456, 456}) {
		t.Fatalf("release retries = %v; a discharged right must not be released twice", releases)
	}
}

func TestDarwinNamespaceCompletionRequiresAcknowledgementAndNoRetainedDetach(t *testing.T) {
	b := &darwinBackend{}
	b.targetReleased.Store(true)
	if b.canReleaseMachNamespace() {
		t.Fatal("target release alone authorized namespace destruction")
	}
	b.teardown.Store(true)
	if b.canReleaseMachNamespace() {
		t.Fatal("wake latch alone authorized namespace destruction")
	}
	b.waitAcknowledged.Store(true)
	b.attachOwned.Store(true)
	if b.canReleaseMachNamespace() {
		t.Fatal("outstanding target ownership authorized namespace destruction")
	}
	b.attachOwned.Store(false)
	outstandingDarwinDetaches.Store(b, struct{}{})
	defer outstandingDarwinDetaches.Delete(b)
	if b.canReleaseMachNamespace() {
		t.Fatal("outstanding detach registry was ignored")
	}
	outstandingDarwinDetaches.Delete(b)
	if !b.canReleaseMachNamespace() {
		t.Fatal("acknowledged COMPLETE state was not recognized")
	}
}
