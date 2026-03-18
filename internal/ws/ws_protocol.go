package ws

import "encoding/json"

type Message struct {
	Type string          `json:"type"` // EventType or CommandType
	Data json.RawMessage `json:"data,omitempty"`
}

type State string

const (
	StateReady      State = "ready"
	StateExecuting  State = "executing"
	StateBreakpoint State = "breakpoint"
)

// Event messages (hub -> client)
type HubEventType string

const (
	HubEventSessionStarted    HubEventType = "sessionStarted"
	HubEventStateUpdate       HubEventType = "stateUpdate"
	HubEventBreakpointHit     HubEventType = "breakpointHit"
	HubEventInitialBreakpoint HubEventType = "initialBreakpoint"
)

type SessionStartedEvent struct {
	Type      HubEventType `json:"type"`
	SessionID string       `json:"sessionId"`
	PID       int          `json:"pid"`
}

type StateUpdateEvent struct {
	Type      HubEventType `json:"type"`
	SessionID string       `json:"sessionId"`
	NewState  State        `json:"newState"`
}

type BreakpointHitEvent struct {
	Type      HubEventType `json:"type"`
	SessionID string       `json:"sessionId"`
	PID       int          `json:"pid"`
	Filename  string       `json:"filename"`
	Line      int          `json:"line"`
	Function  string       `json:"function"`
}

type InitialBreakpointHitEvent struct {
	Type      HubEventType `json:"type"`
	SessionID string       `json:"sessionId"`
	PID       int          `json:"pid"`
}

// Command messages (client -> hub)
type HubCommandType string

const (
	HubCmdStartDebug    HubCommandType = "startDebug"
	HubCmdSetBreakpoint HubCommandType = "setBreakpoint"
	HubCmdContinue      HubCommandType = "continue"
	HubCmdStepOver      HubCommandType = "stepOver"
	HubCmdSingleStep    HubCommandType = "singleStep"
	HubCmdExit          HubCommandType = "exit"
)

type StartDebugCmd struct {
	Type       HubCommandType `json:"type"`
	SessionID  string         `json:"sessionId"`
	TargetPath string         `json:"targetPath"`
}

type ContinueCmd struct {
	Type      HubCommandType `json:"type"`
	SessionID string         `json:"sessionId"`
}

type StepOverCmd struct {
	Type      HubCommandType `json:"type"`
	SessionID string         `json:"sessionId"`
}

type SingleStepCmd struct {
	Type      HubCommandType `json:"type"`
	SessionID string         `json:"sessionId"`
}

type SetBreakpointCmd struct {
	Type      HubCommandType `json:"type"`
	SessionID string         `json:"sessionId"`
	Filename  string         `json:"filename"`
	Line      int            `json:"line"`
}

type ExitCmd struct {
	Type      HubCommandType `json:"type"`
	SessionID string         `json:"sessionId"`
}
