// Package dapclient decodes the DAP stream extensions shared by bingo clients.
package dapclient

import (
	"bufio"
	"encoding/json"
	"fmt"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type SessionEvent struct {
	godap.Event
	Body SessionEventBody `json:"body"`
}

type SessionEventBody struct {
	Version   int    `json:"version"`
	SessionID string `json:"sessionId"`
}

// ReadProtocolMessage preserves go-dap's normal decoder while recognizing
// bingo's versioned managed-session event.
func ReadProtocolMessage(reader *bufio.Reader) (godap.Message, error) {
	content, err := godap.ReadBaseMessage(reader)
	if err != nil {
		return nil, err
	}
	return DecodeProtocolMessage(content)
}

func DecodeProtocolMessage(content []byte) (godap.Message, error) {
	var envelope struct {
		Type  string `json:"type"`
		Event string `json:"event"`
	}
	if err := json.Unmarshal(content, &envelope); err != nil {
		return nil, fmt.Errorf("decode DAP envelope: %w", err)
	}
	if envelope.Type == "event" && envelope.Event == protocol.DAPSessionEventName {
		var event SessionEvent
		if err := json.Unmarshal(content, &event); err != nil {
			return nil, fmt.Errorf("decode DAP session event: %w", err)
		}
		return &event, nil
	}
	return godap.DecodeProtocolMessage(content)
}
