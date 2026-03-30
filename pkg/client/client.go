// Package client provides a Go client for the bingo debug server.
//
// The client connects over WebSocket and exposes a typed Go API that mirrors
// the bingo protocol. It is intended as the reference implementation — the
// community is free to implement clients in any language using the WebSocket
// protocol directly.
//
// # Usage
//
//	sessions, _ := client.ListSessions("localhost:6060")
//	c, _ := client.Create("localhost:6060")   // or client.Join(addr, id)
//	defer c.Close()
//
//	_ = c.Launch("/path/to/binary", nil, nil)
//	bp, _ := c.SetBreakpoint("main.go", 42)
//	_ = c.Continue()
//
//	for evt := range c.Events() {
//	    switch evt.Kind { ... }
//	}
//
// # Command categories
//
// Methods are split into two categories based on the protocol's event model:
//
//   - Synchronous: SetBreakpoint, ClearBreakpoint, Locals, StackFrames,
//     Goroutines — these block until the server sends the confirmation event
//     (or an error event for the same command kind).
//
//   - Fire-and-forget: Launch, Attach, Kill, Continue, StepOver, StepInto,
//     StepOut — these return as soon as the command is written to the wire.
//     Results arrive asynchronously on the Events channel (e.g. SessionState
//     transitions, BreakpointHit, ProcessExited, etc.).
package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// Client is the interface for interacting with a bingo debug server.
// All methods are goroutine-safe.
type Client interface {
	// ── Session info ─────────────────────────────────────────────────────

	// SessionID returns the server-assigned session UUID.
	SessionID() string

	// State returns the last known session state. Updated automatically
	// from server-sent SessionState events.
	State() protocol.SessionState

	// ── Events ───────────────────────────────────────────────────────────

	// Events returns a read-only channel that carries asynchronous events
	// from the server: BreakpointHit, Panic, Output, ProcessExited,
	// Stepped, Continued, SessionState, and any errors from fire-and-forget
	// commands.
	//
	// The channel is closed when the connection drops or Close is called.
	// Callers must drain it continuously to avoid backpressure.
	Events() <-chan protocol.Event

	// ── Process lifecycle (fire-and-forget) ──────────────────────────────

	// Launch starts a new process under the debugger.
	Launch(program string, args, env []string) error

	// Attach connects to an already-running process by PID.
	Attach(pid int, binaryPath string) error

	// Kill terminates the debuggee.
	Kill() error

	// ── Execution control (fire-and-forget) ──────────────────────────────

	// Continue resumes execution.
	Continue() error

	// StepOver executes the current source line, stepping over calls.
	StepOver() error

	// StepInto executes the current source line, stepping into calls.
	StepInto() error

	// StepOut runs until the current function returns.
	StepOut() error

	// ── Breakpoints (synchronous) ────────────────────────────────────────

	// SetBreakpoint installs a breakpoint at file:line and blocks until
	// the server confirms with the resolved Breakpoint (including its ID).
	SetBreakpoint(file string, line int) (protocol.Breakpoint, error)

	// ClearBreakpoint removes the breakpoint with the given ID and blocks
	// until the server confirms.
	ClearBreakpoint(id int) error

	// ── Inspection (synchronous) ─────────────────────────────────────────

	// Locals returns the local variables for the given stack frame.
	// Frame 0 is the innermost (currently executing) frame.
	Locals(frameIndex int) ([]protocol.Variable, error)

	// StackFrames returns the current call stack.
	StackFrames() ([]protocol.Frame, error)

	// Goroutines returns a snapshot of all live goroutines.
	Goroutines() ([]protocol.Goroutine, error)

	// ── Lifecycle ────────────────────────────────────────────────────────

	// Close disconnects from the server. Safe to call multiple times.
	Close() error
}

// ── Session discovery ────────────────────────────────────────────────────────

// SessionInfo describes an active debug session on the server.
type SessionInfo struct {
	ID        string                `json:"id"`
	State     protocol.SessionState `json:"state"`
	Clients   int                   `json:"clients"`
	CreatedAt time.Time             `json:"createdAt"`
}

// ListSessions queries the server's REST API for all active debug sessions.
//
//	sessions, err := client.ListSessions("localhost:6060")
func ListSessions(addr string) ([]SessionInfo, error) {
	url := fmt.Sprintf("http://%s/api/sessions", addr)

	resp, err := http.Get(url) //nolint:gosec // no auth by design
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list sessions: HTTP %d", resp.StatusCode)
	}

	var sessions []SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("list sessions: decode: %w", err)
	}
	return sessions, nil
}

// ── Constructors ─────────────────────────────────────────────────────────────

// Create connects to the server and creates a new debug session.
// Blocks until the server confirms with the assigned session ID.
//
//	c, err := client.Create("localhost:6060")
func Create(addr string) (Client, error) {
	return dial(addr, "create=1")
}

// Join connects to the server and joins an existing debug session.
// Blocks until the server confirms the join with a SessionState event.
//
//	c, err := client.Join("localhost:6060", "550e8400-e29b-41d4-a716-446655440000")
func Join(addr, sessionID string) (Client, error) {
	return dial(addr, fmt.Sprintf("session=%s", sessionID))
}
