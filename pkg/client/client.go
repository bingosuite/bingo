// Package client is the reference Go client for the bingo debug server.
// Connects via WebSocket; methods mirror the protocol package. See AGENTS.md
// for the synchronous-vs-fire-and-forget command split.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
)

const listSessionsTimeout = 5 * time.Second

// ErrClosed reports that a client operation was interrupted by connection teardown.
var ErrClosed = errors.New("client closed")

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

	// Goroutines blocks until the server returns the goroutine list. The list
	// is BOUNDED by the wire contract, so on a highly concurrent target it can
	// be a subset — use GoroutineList when you need to know that, and how much
	// was left out. Kept returning a bare slice so existing callers are
	// unaffected.
	Goroutines() ([]protocol.Goroutine, error)

	// GoroutineList is Goroutines plus the honesty channel: the payload's
	// Totals carry the debugger's ORIGINAL counts and whether either runtime
	// scan stopped early, so a caller can report "showing N of M" instead of
	// presenting a bounded list as the whole runtime. Totals is nil exactly
	// when the list is complete and neither scan clipped.
	GoroutineList() (protocol.GoroutinesPayload, error)

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

func parseServerAddress(addr string, explicitScheme bool) (*url.URL, error) {
	if strings.Contains(addr, "#") {
		return nil, errors.New("server address must not contain a fragment")
	}
	if !explicitScheme {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("server address must be host:port: %w", err)
		}
		if port == "" {
			return nil, errors.New("server address must include a port")
		}
		addr = "https://" + addr
	}

	endpoint, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("parse server address: %w", err)
	}
	if endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Opaque != "" ||
		endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return nil, errors.New("server address must contain only a host and optional port, without credentials, path, query, or fragment")
	}
	if strings.HasSuffix(endpoint.Host, ":") {
		return nil, errors.New("server address has an empty port")
	}
	if port := endpoint.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, fmt.Errorf("invalid server port %q", port)
		}
	}
	return endpoint, nil
}

func serverURL(addr, path string, query url.Values, websocket bool) (string, error) {
	explicitScheme := strings.Contains(addr, "://")
	endpoint, err := parseServerAddress(addr, explicitScheme)
	if err != nil {
		return "", err
	}

	switch strings.ToLower(endpoint.Scheme) {
	case "http", "ws":
		endpoint.Scheme = "http"
	case "https", "wss":
		endpoint.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported server URL scheme %q", endpoint.Scheme)
	}
	// Plaintext is the local default only; remote endpoints require TLS unless
	// the caller explicitly opted into plaintext for a trusted network.
	if !explicitScheme {
		ip := net.ParseIP(endpoint.Hostname())
		if strings.EqualFold(endpoint.Hostname(), "localhost") || ip != nil && ip.IsLoopback() {
			endpoint.Scheme = "http"
		}
	}
	if websocket {
		if endpoint.Scheme == "https" {
			endpoint.Scheme = "wss"
		} else {
			endpoint.Scheme = "ws"
		}
	}
	endpoint.Path = path
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), nil
}

// ListSessions queries the server's REST API for all active sessions.
func ListSessions(addr string) ([]SessionInfo, error) {
	return ListSessionsContext(context.Background(), addr)
}

// ListSessionsContext queries the server's REST API for all active sessions.
func ListSessionsContext(ctx context.Context, addr string) ([]SessionInfo, error) {
	address, err := serverURL(addr, "/api/sessions", nil, false)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil) //nolint:gosec // no auth by design
	if err != nil {
		return nil, fmt.Errorf("list sessions: request: %w", err)
	}
	httpClient := http.Client{
		Timeout:       listSessionsTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := httpClient.Do(req)
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
	return CreateContext(context.Background(), addr)
}

// CreateContext connects to the server and creates a new debug session.
func CreateContext(ctx context.Context, addr string) (Client, error) {
	return dial(ctx, addr, url.Values{"create": {"1"}})
}

// Join connects to the server and joins an existing session by UUID.
func Join(addr, sessionID string) (Client, error) {
	return JoinContext(context.Background(), addr, sessionID)
}

// JoinContext connects to the server and joins an existing session by UUID.
func JoinContext(ctx context.Context, addr, sessionID string) (Client, error) {
	return dial(ctx, addr, url.Values{"session": {sessionID}})
}
