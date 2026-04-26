// Package client is the reference Go client for the bingo debug server.
// Connects via WebSocket; methods mirror the protocol package. See AGENTS.md
// for the synchronous-vs-fire-and-forget command split.
package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// Client interacts with a bingo debug server. All methods are goroutine-safe.
type Client interface {
	SessionID() string
	State() protocol.SessionState

	// Events delivers async server events. Closed when the connection drops or
	// Close is called. Callers must drain continuously to avoid backpressure.
	Events() <-chan protocol.Event

	Launch(program string, args, env []string) error
	Attach(pid int, binaryPath string) error
	Kill() error

	Continue() error
	StepOver() error
	StepInto() error
	StepOut() error

	// SetBreakpoint blocks until the server confirms the resolved Breakpoint.
	SetBreakpoint(file string, line int) (protocol.Breakpoint, error)
	ClearBreakpoint(id int) error

	Locals(frameIndex int) ([]protocol.Variable, error)
	StackFrames() ([]protocol.Frame, error)
	Goroutines() ([]protocol.Goroutine, error)

	Close() error
}

// SessionInfo describes an active debug session, returned by ListSessions.
type SessionInfo struct {
	ID        string                `json:"id"`
	State     protocol.SessionState `json:"state"`
	Clients   int                   `json:"clients"`
	CreatedAt time.Time             `json:"createdAt"`
}

// ListSessions queries the server's REST API for all active sessions.
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

// Create connects to the server and creates a new debug session.
func Create(addr string) (Client, error) {
	return dial(addr, "create=1")
}

// Join connects to the server and joins an existing session by UUID.
func Join(addr, sessionID string) (Client, error) {
	return dial(addr, fmt.Sprintf("session=%s", sessionID))
}
