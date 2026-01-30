package ws

import "encoding/json"

type Message struct {
    Type string          `json:"type"`  // EventType or CommandType
    Data json.RawMessage `json:"data,omitempty"`
}

// Event messages (server -> client)
type EventType string
const (
    EventSessionStarted   EventType = "sessionStarted"
    EventStateUpdate      EventType = "stateUpdate"
    EventGoroutineEvent   EventType = "goroutineEvent"
    EventInspectResult    EventType = "inspectResult"
)

type GoroutineEvent struct {
    Type        EventType `json:"type"`
    SessionID   string    `json:"sessionId"`
    GoroutineID uint64    `json:"goroutineId"`
    State       string    `json:"state"`
    PC          string    `json:"pc"`
    Source      struct {
        File   string `json:"file"`
        Line   int    `json:"line"`
    } `json:"source"`
}

type InspectResult struct {
    Type        EventType `json:"type"`
    SessionID   string    `json:"sessionId"`
    GoroutineID uint64    `json:"goroutineId"`
    Vars        map[string]string `json:"vars"`
}

// Command messages (client -> server)
type CommandType string
const (
    CmdContinue      CommandType = "continue"
    CmdStepOver      CommandType = "stepOver"
    CmdInspectGoroutine CommandType = "inspectGoroutine"
)

type InspectGoroutineCmd struct {
    Type        CommandType `json:"type"`
    SessionID   string      `json:"sessionId"`
    GoroutineID uint64      `json:"goroutineId"`
}

type ContinueCmd struct {
    Type      CommandType `json:"type"`
    SessionID string      `json:"sessionId"`
}

// Helpers
func (e EventType) Message() Message {
    return Message{Type: string(e)}
}

func (c CommandType) Message() Message {
    return Message{Type: string(c)}
}
