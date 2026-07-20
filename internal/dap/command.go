package dap

import (
	"encoding/json"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// marshalCommand builds a versioned bingo Command envelope with payload
// marshalled into its raw-JSON Payload field, then serialises the whole
// envelope to the bytes the hub's read pump expects (protocol.UnmarshalCommand).
// A nil payload marshals to an empty object, which is correct for the
// payload-less commands (Continue, Step*, Kill, Pause, Frames, Goroutines).
func marshalCommand(kind protocol.CommandKind, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload == nil {
		raw = json.RawMessage("{}")
	} else {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(protocol.Command{
		Version: protocol.Version,
		Kind:    kind,
		Payload: raw,
	})
}
