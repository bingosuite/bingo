package protocol

import (
	"encoding/json"
	"fmt"
)

// ─── Encoding ────────────────────────────────────────────────────────────────

// NewEvent constructs a versioned Event envelope, marshalling payload into
// the raw JSON field. seq should be monotonically increasing per hub session.
func NewEvent(kind EventKind, seq uint64, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("protocol.NewEvent: marshal payload: %w", err)
	}
	return Event{
		Version: Version,
		Kind:    kind,
		Seq:     seq,
		Payload: raw,
	}, nil
}

// MustEvent is like NewEvent but panics on error. Use only in tests.
func MustEvent(kind EventKind, seq uint64, payload any) Event {
	e, err := NewEvent(kind, seq, payload)
	if err != nil {
		panic(err)
	}
	return e
}

// ─── Decoding ────────────────────────────────────────────────────────────────

// DecodeEventPayload unmarshals the raw payload of an Event into dst.
// dst must be a pointer to the correct payload type for e.Kind.
//
//	var p BreakpointHitPayload
//	if err := protocol.DecodeEventPayload(e, &p); err != nil { ... }
func DecodeEventPayload(e Event, dst any) error {
	if err := json.Unmarshal(e.Payload, dst); err != nil {
		return fmt.Errorf("protocol.DecodeEventPayload(%s): %w", e.Kind, err)
	}
	return nil
}

// DecodeCommandPayload unmarshals the raw payload of a Command into dst.
//
//	var p SetBreakpointPayload
//	if err := protocol.DecodeCommandPayload(cmd, &p); err != nil { ... }
func DecodeCommandPayload(cmd Command, dst any) error {
	if err := json.Unmarshal(cmd.Payload, dst); err != nil {
		return fmt.Errorf("protocol.DecodeCommandPayload(%s): %w", cmd.Kind, err)
	}
	return nil
}

// ─── WebSocket helpers ───────────────────────────────────────────────────────

// MarshalEvent serialises an Event to JSON bytes ready to write to a
// WebSocket text frame.
func MarshalEvent(e Event) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("protocol.MarshalEvent: %w", err)
	}
	return b, nil
}

// UnmarshalCommand parses raw WebSocket bytes into a Command envelope.
// The caller is responsible for decoding the typed payload via
// DecodeCommandPayload.
func UnmarshalCommand(data []byte) (Command, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return Command{}, fmt.Errorf("protocol.UnmarshalCommand: %w", err)
	}
	return cmd, nil
}

// UnmarshalEvent parses raw WebSocket bytes into an Event envelope.
// Used by the client SDK when reading from the server.
func UnmarshalEvent(data []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return Event{}, fmt.Errorf("protocol.UnmarshalEvent: %w", err)
	}
	return e, nil
}
