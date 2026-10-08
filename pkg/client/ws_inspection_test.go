package client

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestSelectedInspectionRequiresMatchingEcho(t *testing.T) {
	for _, kind := range []protocol.CommandKind{protocol.CmdFrames, protocol.CmdLocals, protocol.CmdEvaluate} {
		for _, echo := range []int{0, 41, 42} {
			t.Run(fmt.Sprintf("%s/echo-%d", kind, echo), func(t *testing.T) {
				assertSelectedInspectionEcho(t, kind, echo)
			})
		}
	}
}

func assertSelectedInspectionEcho(t *testing.T, kind protocol.CommandKind, echo int) {
	t.Helper()
	h := newLoopbackClient(t)
	result := selectedInspectionRequest(h, kind)
	cmd := h.readCommand(t)
	assertCommandKind(t, cmd, kind)
	var selection struct {
		GoroutineID int `json:"goroutineId"`
	}
	if err := protocol.DecodeCommandPayload(cmd, &selection); err != nil || selection.GoroutineID != 42 {
		t.Fatalf("selection = %+v: %v", selection, err)
	}
	h.writeEvent(t, selectedInspectionResponse(kind, echo))
	select {
	case err := <-result:
		if echo == 42 && err != nil {
			t.Fatal(err)
		}
		if echo != 42 && !errors.Is(err, ErrGoroutineInspectionUnsupported) {
			t.Fatalf("unproven selection accepted: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("selected inspection did not settle")
	}
}

func selectedInspectionRequest(h *loopbackClient, kind protocol.CommandKind) <-chan error {
	result := make(chan error, 1)
	go func() {
		var err error
		switch kind {
		case protocol.CmdFrames:
			_, err = h.client.StackFramesForGoroutine(42)
		case protocol.CmdLocals:
			_, err = h.client.LocalsForGoroutine(42, 2)
		case protocol.CmdEvaluate:
			_, err = h.client.EvaluateForGoroutine(42, 2, "label")
		}
		result <- err
	}()
	return result
}

func selectedInspectionResponse(kind protocol.CommandKind, echo int) protocol.Event {
	switch kind {
	case protocol.CmdFrames:
		return protocol.MustEvent(protocol.EventFrames, 2, protocol.FramesPayload{GoroutineID: echo})
	case protocol.CmdLocals:
		return protocol.MustEvent(protocol.EventLocals, 2, protocol.LocalsPayload{GoroutineID: echo, FrameIndex: 2})
	default:
		return protocol.MustEvent(protocol.EventEvaluate, 2, protocol.EvaluatePayload{GoroutineID: echo})
	}
}

func TestLegacyClientDoesNotBorrowSelectedContext(t *testing.T) {
	h := newLoopbackClient(t)
	legacy := struct{ Client }{h.client}
	if _, err := StackFramesForGoroutine(legacy, 42); !errors.Is(err, ErrGoroutineInspectionUnsupported) {
		t.Fatalf("legacy frames: %v", err)
	}
	if _, err := LocalsForGoroutine(legacy, 42, 0); !errors.Is(err, ErrGoroutineInspectionUnsupported) {
		t.Fatalf("legacy locals: %v", err)
	}
	if _, err := EvaluateForGoroutine(legacy, 42, 0, "label"); !errors.Is(err, ErrGoroutineInspectionUnsupported) {
		t.Fatalf("legacy evaluate: %v", err)
	}
	if pendingCount(h.client) != 0 {
		t.Fatal("unsupported selection sent a command")
	}
}

func TestInspectionSelectionBounds(t *testing.T) {
	for _, id := range []int{-1, 1 << 53} {
		if err := validateInspectionSelection(id); err == nil {
			t.Fatalf("accepted invalid id %d", id)
		}
	}
}
