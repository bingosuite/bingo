package protocol_test

import (
	"math"
	"math/rand"
	"testing"
	"unicode/utf16"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// TestPackExactSizeUnderAdversarialStrings is the regression guard for the
// packer's additivity assumption: it charges each element its standalone
// marshal length, which is only exact if encoding/json's escaping is
// context-free and a json.RawMessage payload survives the envelope verbatim.
// Randomized hostile input — HTML-escaped runes, U+2028/U+2029, control
// characters, lone surrogates, invalid UTF-8, and long values — must never make
// the measured size disagree with the real one, or a frame could slip past the
// cap and be rejected below the consumer's decoder. See issue #194.
func TestPackExactSizeUnderAdversarialStrings(t *testing.T) {
	rng := rand.New(rand.NewSource(20260809))
	nasty := []string{
		"<", ">", "&", "\"", "\\", "\u2028", "\u2029", "\x00", "\x1f", "\x7f",
		"\U0001F600", "服务", "café", string([]byte{0xff, 0xfe}), string([]byte{0xc3, 0x28}),
		string(utf16.Decode([]uint16{0xD800})), "\ufffd", "\n\r\t",
	}
	pick := func() string {
		s := ""
		for i := 0; i < rng.Intn(6); i++ {
			s += nasty[rng.Intn(len(nasty))]
		}
		if rng.Intn(10) == 0 {
			s += string(make([]byte, rng.Intn(3000)))
		}
		return s
	}

	for round := 0; round < 120; round++ {
		n := 1 + rng.Intn(200)
		gs := make([]protocol.Goroutine, 0, n)
		for i := 1; i <= n; i++ {
			gs = append(gs, protocol.Goroutine{
				ID: i, ParentID: rng.Intn(n + 1), Status: pick(), WaitReason: pick(),
				CurrentLoc: protocol.Location{File: pick(), Line: rng.Intn(9999), Function: pick()},
				StartLoc:   protocol.Location{File: pick(), Function: pick()},
				CreatedLoc: protocol.Location{File: pick(), Function: pick()},
				ThreadID:   rng.Intn(64), Current: i == 1,
			})
		}
		ts := make([]protocol.Thread, 0, 40)
		for i := 0; i < rng.Intn(40); i++ {
			ts = append(ts, protocol.Thread{
				ID: i, MID: rng.Intn(100), GoID: rng.Intn(n + 1),
				CurrentLoc: protocol.Location{File: pick(), Function: pick()},
				Current:    i == 0,
			})
		}
		created := make([]int, rng.Intn(50))
		for i := range created {
			created[i] = math.MaxInt64 - i
		}

		snap, rep := protocol.PackSnapshot(protocol.GoroutineSnapshotPayload{
			Goroutines: gs, Threads: ts, Current: 1, Created: created,
		}, rng.Intn(2) == 0)
		actual := eventBytesPlain(t, protocol.EventGoroutineSnapshot, snap)
		if actual > protocol.MaxGoroutineEventBytes {
			t.Fatalf("round %d: snapshot %d bytes exceeds cap", round, actual)
		}
		if rep.Bytes != actual {
			t.Fatalf("round %d: reported %d, actual %d", round, rep.Bytes, actual)
		}

		list, rep2 := protocol.PackGoroutines(gs, false)
		actual2 := eventBytesPlain(t, protocol.EventGoroutines, list)
		if actual2 > protocol.MaxGoroutineEventBytes {
			t.Fatalf("round %d: list %d bytes exceeds cap", round, actual2)
		}
		if rep2.Bytes != actual2 {
			t.Fatalf("round %d: list reported %d, actual %d", round, rep2.Bytes, actual2)
		}
	}
}

func eventBytesPlain(t *testing.T, kind protocol.EventKind, payload any) int {
	t.Helper()
	evt, err := protocol.NewEvent(kind, math.MaxUint64, payload)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	raw, err := protocol.MarshalEvent(evt)
	if err != nil {
		t.Fatalf("MarshalEvent: %v", err)
	}
	return len(raw)
}
