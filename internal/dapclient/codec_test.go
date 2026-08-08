package dapclient

import (
	"bufio"
	"bytes"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestReadProtocolMessageAcceptsSessionEventAndNormalMessages(t *testing.T) {
	var wire bytes.Buffer
	session := &SessionEvent{
		Event: godap.Event{
			ProtocolMessage: godap.ProtocolMessage{Seq: 1, Type: "event"},
			Event:           protocol.DAPSessionEventName,
		},
		Body: SessionEventBody{
			Version:   protocol.DAPSessionEventVersion,
			SessionID: "session-123",
		},
	}
	if err := godap.WriteProtocolMessage(&wire, session); err != nil {
		t.Fatal(err)
	}
	initialized := &godap.InitializedEvent{
		Event: godap.Event{
			ProtocolMessage: godap.ProtocolMessage{Seq: 2, Type: "event"},
			Event:           "initialized",
		},
	}
	if err := godap.WriteProtocolMessage(&wire, initialized); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(&wire)
	first, err := ReadProtocolMessage(reader)
	if err != nil {
		t.Fatalf("read session event: %v", err)
	}
	event, ok := first.(*SessionEvent)
	if !ok {
		t.Fatalf("session message type = %T", first)
	}
	if event.Body.Version != protocol.DAPSessionEventVersion ||
		event.Body.SessionID != "session-123" {
		t.Fatalf("session event body = %+v", event.Body)
	}

	second, err := ReadProtocolMessage(reader)
	if err != nil {
		t.Fatalf("read initialized event: %v", err)
	}
	if _, ok := second.(*godap.InitializedEvent); !ok {
		t.Fatalf("normal message type = %T", second)
	}
}
