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

const listSessionsTimeout = 5 * time.Second

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

	// Restart kills the current process (if any launched via Launch) and
	// relaunches it, reinstalling previously-set breakpoints. Pass nil for
	// args/env to reuse the values from the original Launch; pass a non-nil
	// slice (including an empty one, to clear them) to override. Blocks until
	// the server confirms via EventRestarted.
	Restart(args, env []string) (protocol.RestartedPayload, error)

	Continue() error
	StepOver() error
	StepInto() error
	StepOut() error

	// Pause asynchronously interrupts a running process, forcing it to
	// suspend. Fire-and-forget like Continue: it returns as soon as the
	// command is sent; the halt is reported later via EventPaused on Events().
	Pause() error

	// SetBreakpoint blocks until the server confirms the resolved Breakpoint.
	SetBreakpoint(file string, line int) (protocol.Breakpoint, error)
	ClearBreakpoint(id int) error

	Locals(frameIndex int) ([]protocol.Variable, error)

	// Evaluate resolves a single variable NAME in the given frame (local or
	// parameter, then a package global) and blocks until the server returns its
	// typed value tree. Name-only — no expressions.
	Evaluate(frameIndex int, name string) (protocol.Variable, error)

	StackFrames() ([]protocol.Frame, error)
	Goroutines() ([]protocol.Goroutine, error)

	// RequestGoroutineSnapshot asks the server for a full concurrency snapshot.
	// Fire-and-forget like Pause: it returns as soon as the command is sent.
	// EventGoroutineSnapshot is dual-purpose — the server also pushes it
	// automatically on every entry/breakpoint/pause stop — so it can never be
	// correlated to a request by kind. Every snapshot, requested or automatic,
	// is delivered on Events(). Requested snapshots carry no created/exited
	// deltas; only the automatic ones do.
	//
	// Delivery is best-effort: like every event it goes through the shared
	// Events() buffer, which drops when a caller stops draining, and a rejected
	// request answers with EventError (Command == CmdGoroutineSnapshot) rather
	// than a snapshot. A caller that waits for one must handle both — and must
	// not read that error as its own answer: it is broadcast and carries no
	// requester, so it may be another client's rejection with a valid snapshot
	// still coming. Bound such a wait with a deadline, not with the error.
	// The answer is also broadcast to every client on the session, so other
	// observers see this refresh — with empty deltas — as an ordinary snapshot.
	RequestGoroutineSnapshot() error

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

	httpClient := http.Client{Timeout: listSessionsTimeout}
	resp, err := httpClient.Get(url) //nolint:gosec // no auth by design
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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
