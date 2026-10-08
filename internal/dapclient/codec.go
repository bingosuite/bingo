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

// ReadProtocolMessageWithThreadMetadata retains the goroutine identity fields
// go-dap discards, without changing the message types existing clients consume.
func ReadProtocolMessageWithThreadMetadata(reader *bufio.Reader) (godap.Message, map[int]int64, error) {
	content, err := godap.ReadBaseMessage(reader)
	if err != nil {
		return nil, nil, err
	}
	return DecodeProtocolMessageWithThreadMetadata(content)
}

func DecodeProtocolMessageWithThreadMetadata(content []byte) (godap.Message, map[int]int64, error) {
	message, err := DecodeProtocolMessage(content)
	if err != nil {
		return nil, nil, err
	}
	response, ok := message.(*godap.ThreadsResponse)
	if !ok || !response.Success {
		return message, nil, nil
	}

	var metadata struct {
		Body struct {
			Threads []struct {
				GoroutineID json.RawMessage `json:"bingoGoroutineId"`
			} `json:"threads"`
		} `json:"body"`
	}
	if err := json.Unmarshal(content, &metadata); err != nil {
		return nil, nil, fmt.Errorf("decode DAP thread metadata: %w", err)
	}
	if len(metadata.Body.Threads) != len(response.Body.Threads) {
		return nil, nil, fmt.Errorf("decode DAP thread metadata: thread count differs from decoded response")
	}

	const maximumSafeID = 1<<53 - 1
	var goroutineIDs map[int]int64
	seenGoroutines := make(map[int64]struct{})
	for i, thread := range metadata.Body.Threads {
		if len(thread.GoroutineID) == 0 {
			continue
		}
		var goid int64
		if err := json.Unmarshal(thread.GoroutineID, &goid); err != nil {
			return nil, nil, fmt.Errorf("decode DAP thread %d goroutine identity: %w", i, err)
		}
		if goid <= 0 || goid > maximumSafeID {
			return nil, nil, fmt.Errorf("decode DAP thread %d goroutine identity: expected a positive safe integer", i)
		}
		if _, duplicate := seenGoroutines[goid]; duplicate {
			return nil, nil, fmt.Errorf("decode DAP thread metadata: duplicate goroutine identity %d", goid)
		}
		seenGoroutines[goid] = struct{}{}
		if goroutineIDs == nil {
			goroutineIDs = make(map[int]int64)
		}
		goroutineIDs[response.Body.Threads[i].Id] = goid
	}
	if goroutineIDs == nil {
		return message, nil, nil
	}

	seen := make(map[int]struct{}, len(response.Body.Threads))
	for _, thread := range response.Body.Threads {
		if thread.Id <= 0 || int64(thread.Id) > maximumSafeID {
			return nil, nil, fmt.Errorf("decode DAP thread metadata: thread handle %d is not a positive safe integer", thread.Id)
		}
		if _, duplicate := seen[thread.Id]; duplicate {
			return nil, nil, fmt.Errorf("decode DAP thread metadata: duplicate thread handle %d", thread.Id)
		}
		seen[thread.Id] = struct{}{}
	}
	return message, goroutineIDs, nil
}
