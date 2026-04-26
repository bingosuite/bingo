package protocol

import (
	"encoding/json"
	"fmt"
)

// NewEvent constructs a versioned Event with payload marshalled into the
// raw-JSON Payload field.
func NewEvent(kind EventKind, seq uint64, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("protocol.NewEvent: marshal payload: %w", err)
	}
	return Event{Version: Version, Kind: kind, Seq: seq, Payload: raw}, nil
}

// MustEvent is like NewEvent but panics on error. Tests only.
func MustEvent(kind EventKind, seq uint64, payload any) Event {
	e, err := NewEvent(kind, seq, payload)
	if err != nil {
		panic(err)
	}
	return e
}

// DecodeEventPayload unmarshals e.Payload into dst (a pointer to the payload
// type matching e.Kind).
func DecodeEventPayload(e Event, dst any) error {
	if err := json.Unmarshal(e.Payload, dst); err != nil {
		return fmt.Errorf("protocol.DecodeEventPayload(%s): %w", e.Kind, err)
	}
	return nil
}

// DecodeCommandPayload unmarshals cmd.Payload into dst.
func DecodeCommandPayload(cmd Command, dst any) error {
	if err := json.Unmarshal(cmd.Payload, dst); err != nil {
		return fmt.Errorf("protocol.DecodeCommandPayload(%s): %w", cmd.Kind, err)
	}
	return nil
}

// MarshalEvent serialises an Event to JSON for a WebSocket text frame.
func MarshalEvent(e Event) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("protocol.MarshalEvent: %w", err)
	}
	return b, nil
}

// UnmarshalCommand parses raw WebSocket bytes into a Command envelope.
func UnmarshalCommand(data []byte) (Command, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return Command{}, fmt.Errorf("protocol.UnmarshalCommand: %w", err)
	}
	return cmd, nil
}

// UnmarshalEvent parses raw WebSocket bytes into an Event envelope.
func UnmarshalEvent(data []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return Event{}, fmt.Errorf("protocol.UnmarshalEvent: %w", err)
	}
	return e, nil
}
