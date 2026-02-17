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
	EventBreakpointHit  EventType = "breakpointHit"
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

type BreakpointHitEvent struct {
	Type      EventType `json:"type"`
	SessionID string    `json:"sessionId"`
	PID       int       `json:"pid"`
	Filename  string    `json:"filename"`
	Line      int       `json:"line"`
	Function  string    `json:"function"`
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
