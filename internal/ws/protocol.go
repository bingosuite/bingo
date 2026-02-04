package ws

import "encoding/json"

type Message struct {
	Type string          `json:"type"` // EventType or CommandType
	Data json.RawMessage `json:"data,omitempty"`
}

type State string

const (
	StateExecuting  State = "executing"
	StateBreakpoint State = "breakpoint"
)

// Event messages (server -> client)
type EventType string

const (
	EventSessionStarted EventType = "sessionStarted"
	EventStateUpdate    EventType = "stateUpdate"
)

type SessionStartedEvent struct {
	Type      EventType `json:"type"`
	SessionID string    `json:"sessionId"`
	PID       int       `json:"pid"`
}

type StateUpdateEvent struct {
	Type      EventType `json:"type"`
	SessionID string    `json:"sessionId"`
	NewState  State     `json:"newState"`
}

// Command messages (client -> server)
type CommandType string

const (
	CmdStartDebug CommandType = "startDebug"
	CmdContinue   CommandType = "continue"
	CmdStepOver   CommandType = "stepOver"
	CmdExit       CommandType = "exit"
)

type StartDebugCmd struct {
	Type       CommandType `json:"type"`
	SessionID  string      `json:"sessionId"`
	TargetPath string      `json:"targetPath"`
}

type ContinueCmd struct {
	Type      CommandType `json:"type"`
	SessionID string      `json:"sessionId"`
}

type StepOverCmd struct {
	Type      CommandType `json:"type"`
	SessionID string      `json:"sessionId"`
}

type ExitCmd struct {
	Type      CommandType `json:"type"`
	SessionID string      `json:"sessionId"`
}
