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
	pending.set(secondTID, 6)
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

func TestPendingSignalsContinueSeparatesCurrentFromDelayed(t *testing.T) {
	var pending pendingSignals
	const (
		tid           = 5001
		delayedSignal = 23
		currentSignal = 10
	)

	pending.delay(tid, delayedSignal)
	pending.set(tid, currentSignal)

	signal, delayed := pending.takeForContinue(tid)
	if signal != currentSignal || delayed != delayedSignal {
		t.Fatalf("takeForContinue = (%d, %d), want (%d, %d)", signal, delayed, currentSignal, delayedSignal)
	}
	if got := pending.take(tid); got != 0 {
		t.Fatalf("take after takeForContinue = %d, want 0", got)
	}

	pending.restore(tid, signal, delayed)
	if got := pending.take(tid); got != currentSignal {
		t.Fatalf("first take after restore = %d, want %d", got, currentSignal)
	}
	if got := pending.take(tid); got != delayedSignal {
		t.Fatalf("second take after restore = %d, want %d", got, delayedSignal)
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

	current, delayed := pending.takeForContinue(tid)
	if current != signal || delayed != 0 {
		t.Fatalf("takeForContinue = (%d, %d), want (%d, 0)", current, delayed, signal)
	}
}

func TestPendingSignalsExplicitResumeRequeuesDelayedSignal(t *testing.T) {
	var pending pendingSignals
	const (
		tid           = 6001
		delayedSignal = 23
	)

	pending.delay(tid, delayedSignal)
	signal, delayed := pending.takeForExplicitResume(tid, 0)
	if signal != 0 || delayed != delayedSignal {
		t.Fatalf("signal-zero resume = (%d, %d), want (0, %d)", signal, delayed, delayedSignal)
	}

	pending.delay(tid, delayedSignal)
	signal, delayed = pending.takeForExplicitResume(tid, delayedSignal)
	if signal != delayedSignal || delayed != 0 {
		t.Fatalf("matching resume = (%d, %d), want (%d, 0)", signal, delayed, delayedSignal)
	}

	pending.delay(tid, delayedSignal)
	signal, delayed = pending.takeForExplicitResume(tid, 10)
	if signal != 10 || delayed != delayedSignal {
		t.Fatalf("different resume = (%d, %d), want (10, %d)", signal, delayed, delayedSignal)
	}
}
