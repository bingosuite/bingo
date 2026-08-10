package debugger

import "testing"

func TestPendingSignalsStayWithTheirTID(t *testing.T) {
	var pending pendingSignals
	const (
		firstTID     = 1001
		secondTID    = 1002
		firstSignal  = 11
		secondSignal = 6
	)

	pending.set(firstTID, firstSignal)
	pending.set(secondTID, secondSignal)

	if got := pending.take(9999); got != 0 {
		t.Fatalf("take(unrelated tid) = %d, want 0", got)
	}
	if got := pending.take(secondTID); got != secondSignal {
		t.Fatalf("take(second tid) = %d, want %d", got, secondSignal)
	}
	if got := pending.take(secondTID); got != 0 {
		t.Fatalf("second take(second tid) = %d, want one-shot 0", got)
	}
	if got := pending.take(firstTID); got != firstSignal {
		t.Fatalf("taking second tid lost first signal: got %d, want %d", got, firstSignal)
	}
}

func TestPendingSignalsClearOnlyRequestedScope(t *testing.T) {
	var pending pendingSignals
	const (
		firstTID  = 2001
		secondTID = 2002
	)

	pending.set(0, 11)
	pending.set(firstTID, 0)
	pending.set(firstTID, 11)
	pending.set(firstTID, 12)
	pending.set(secondTID, 6)
	pending.delay(firstTID, 23)
	pending.delay(secondTID, 24)
	pending.clear(firstTID)

	if got := pending.take(firstTID); got != 0 {
		t.Fatalf("take(cleared tid) = %d, want 0", got)
	}
	if got := pending.take(secondTID); got != 6 {
		t.Fatalf("clearing first tid changed second: got %d, want 6", got)
	}
	if got := pending.take(secondTID); got != 24 {
		t.Fatalf("clearing first tid changed second's delayed signal: got %d, want 24", got)
	}

	pending.set(firstTID, 11)
	pending.set(firstTID, 12)
	pending.set(secondTID, 6)
	pending.set(secondTID, 7)
	pending.delay(firstTID, 23)
	pending.delay(secondTID, 24)
	pending.purge()
	if got := pending.take(firstTID); got != 0 {
		t.Fatalf("take(first tid) after purge = %d, want 0", got)
	}
	if got := pending.take(secondTID); got != 0 {
		t.Fatalf("take(second tid) after purge = %d, want 0", got)
	}
}

func TestPendingSignalsDelayedSignalSurvivesLaterCurrentSignal(t *testing.T) {
	var pending pendingSignals
	const (
		tid           = 3001
		delayedSignal = 23
		currentSignal = 10
	)

	pending.delay(tid, delayedSignal)
	pending.set(tid, currentSignal)

	if got := pending.take(tid); got != currentSignal {
		t.Fatalf("first take = %d, want current signal %d", got, currentSignal)
	}
	if got := pending.take(tid); got != delayedSignal {
		t.Fatalf("second take = %d, want delayed signal %d", got, delayedSignal)
	}
	if got := pending.take(tid); got != 0 {
		t.Fatalf("third take = %d, want 0", got)
	}
}

func TestPendingSignalsDelayedSignalIsFirstWins(t *testing.T) {
	var pending pendingSignals
	const tid = 4001

	pending.delay(tid, 23)
	pending.delay(tid, 24)

	if got := pending.take(tid); got != 23 {
		t.Fatalf("take = %d, want first delayed signal 23", got)
	}
	if got := pending.take(tid); got != 0 {
		t.Fatalf("second take = %d, want 0", got)
	}
}

func TestPendingSignalsContinueSeparatesCurrentFromDelayed(t *testing.T) {
	var pending pendingSignals
	const (
		tid           = 5001
		olderSignal   = 12
		delayedSignal = 23
		currentSignal = 10
	)

	pending.delay(tid, delayedSignal)
	pending.set(tid, olderSignal)
	pending.set(tid, currentSignal)

	batch := pending.takeForContinue(tid)
	if batch.current != currentSignal ||
		len(batch.deferred) != 2 ||
		batch.deferred[0] != olderSignal ||
		batch.deferred[1] != delayedSignal {
		t.Fatalf("takeForContinue = %+v, want current %d, backlog [%d], delayed %d", batch, currentSignal, olderSignal, delayedSignal)
	}
	if got := pending.take(tid); got != 0 {
		t.Fatalf("take after takeForContinue = %d, want 0", got)
	}

	pending.restore(tid, batch)
	if got := pending.take(tid); got != currentSignal {
		t.Fatalf("first take after restore = %d, want %d", got, currentSignal)
	}
	if got := pending.take(tid); got != olderSignal {
		t.Fatalf("second take after restore = %d, want %d", got, olderSignal)
	}
	if got := pending.take(tid); got != delayedSignal {
		t.Fatalf("third take after restore = %d, want %d", got, delayedSignal)
	}
}

func TestPendingSignalsContinueCoalescesMatchingSignal(t *testing.T) {
	var pending pendingSignals
	const (
		tid    = 5501
		signal = 23
	)

	pending.delay(tid, signal)
	pending.set(tid, signal)

	batch := pending.takeForContinue(tid)
	if batch.current != signal || len(batch.deferred) != 0 {
		t.Fatalf("takeForContinue = %+v, want current %d only", batch, signal)
	}
}

func TestPendingSignalsFreshSignalCoalescesMatchingBacklog(t *testing.T) {
	var pending pendingSignals
	const (
		tid    = 5601
		first  = 10
		second = 12
	)

	pending.set(tid, first)
	pending.set(tid, second)
	pending.set(tid, first)

	assertPendingSignalSequence(t, &pending, tid, first, second)
}

func TestPendingSignalsFreshSignalCoalescesMatchingDelayed(t *testing.T) {
	var pending pendingSignals
	const (
		tid     = 5651
		current = 10
		delayed = 23
	)

	pending.delay(tid, delayed)
	pending.set(tid, current)
	pending.set(tid, delayed)

	assertPendingSignalSequence(t, &pending, tid, delayed, current)
}

func TestPendingSignalsRestoreCoalescesCurrentWithExistingDelayed(t *testing.T) {
	var pending pendingSignals
	const (
		tid    = 5701
		signal = 23
	)

	pending.delay(tid, signal)
	pending.restore(tid, pendingSignalBatch{current: signal})

	assertPendingSignalSequence(t, &pending, tid, signal)
}

func TestPendingSignalsRestorePreservesNewerAndExtractedState(t *testing.T) {
	var pending pendingSignals
	const tid = 5751

	pending.set(tid, 10)
	pending.delay(tid, 23)
	pending.restore(tid, pendingSignalBatch{current: 11, deferred: []int{24}})

	if got := pending.take(tid); got != 10 {
		t.Fatalf("first take = %d, want existing current signal 10", got)
	}
	if got := pending.take(tid); got != 11 {
		t.Fatalf("second take = %d, want restored current signal 11", got)
	}
	if got := pending.take(tid); got != 24 {
		t.Fatalf("third take = %d, want restored delayed signal 24", got)
	}
	if got := pending.take(tid); got != 23 {
		t.Fatalf("fourth take = %d, want existing delayed signal 23", got)
	}
}

func TestPendingSignalsExplicitResumeRequeuesDelayedSignal(t *testing.T) {
	var pending pendingSignals
	const (
		tid           = 6001
		delayedSignal = 23
	)

	pending.delay(tid, delayedSignal)
	batch, signal := pending.takeForExplicitResume(tid, 0)
	if batch.current != 0 || signal != 0 ||
		len(batch.deferred) != 1 || batch.deferred[0] != delayedSignal {
		t.Fatalf("signal-zero resume = (%+v, %d), want delayed %d and signal 0", batch, signal, delayedSignal)
	}

	pending.delay(tid, delayedSignal)
	batch, signal = pending.takeForExplicitResume(tid, delayedSignal)
	if batch.current != 0 || signal != delayedSignal || len(batch.deferred) != 0 {
		t.Fatalf("matching resume = (%+v, %d), want empty batch and signal %d", batch, signal, delayedSignal)
	}

	pending.set(tid, 11)
	pending.delay(tid, delayedSignal)
	batch, signal = pending.takeForExplicitResume(tid, 10)
	if batch.current != 11 || signal != 10 ||
		len(batch.deferred) != 1 || batch.deferred[0] != delayedSignal {
		t.Fatalf("different resume = (%+v, %d), want current 11, signal 10, delayed %d", batch, signal, delayedSignal)
	}
}

func TestPendingSignalsDistinctCurrentSignalsKeepNewestCurrent(t *testing.T) {
	var pending pendingSignals
	const (
		tid    = 6251
		first  = 10
		second = 12
		third  = 15
	)

	pending.set(tid, first)
	pending.set(tid, second)
	pending.set(tid, third)

	batch := pending.takeForContinue(tid)
	if batch.current != third ||
		len(batch.deferred) != 2 ||
		batch.deferred[0] != first ||
		batch.deferred[1] != second {
		t.Fatalf("takeForContinue = %+v, want current %d and backlog [%d %d]", batch, third, first, second)
	}
}

func assertPendingSignalSequence(t *testing.T, pending *pendingSignals, tid int, want ...int) {
	t.Helper()
	for i, signal := range want {
		if got := pending.take(tid); got != signal {
			t.Fatalf("take %d = %d, want %d", i+1, got, signal)
		}
	}
	if got := pending.take(tid); got != 0 {
		t.Fatalf("take after expected sequence = %d, want 0", got)
	}
}
