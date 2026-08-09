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
	pending.clear(firstTID)

	if got := pending.take(firstTID); got != 0 {
		t.Fatalf("take(cleared tid) = %d, want 0", got)
	}
	if got := pending.take(secondTID); got != 6 {
		t.Fatalf("clearing first tid changed second: got %d, want 6", got)
	}

	pending.set(firstTID, 11)
	pending.set(secondTID, 6)
	pending.purge()
	if got := pending.take(firstTID); got != 0 {
		t.Fatalf("take(first tid) after purge = %d, want 0", got)
	}
	if got := pending.take(secondTID); got != 0 {
		t.Fatalf("take(second tid) after purge = %d, want 0", got)
	}
}
