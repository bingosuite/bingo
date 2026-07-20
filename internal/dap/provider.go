package dap

import (
	"log/slog"

	"github.com/bingosuite/bingo/internal/hub"
)

// Session is the subset of a bingo hub session the DAP handler drives. A
// *hub.Hub satisfies it directly; internal/server's Provider hands these out.
type Session interface {
	// SessionID is the managed-session id WebSocket observers join with
	// ?session=<id>.
	SessionID() string
	// AddClient registers a hub client (here, the DAP handler as a WSConn) so it
	// receives the session's event stream and can inject commands. Must be called
	// before the Launch/Attach command is enqueued so the entry stop is delivered.
	AddClient(conn hub.WSConn, log *slog.Logger) *hub.Client
}

// Provider creates and looks up managed sessions. internal/server implements it
// over its sessionStore; the DAP handler uses it to start a fresh session on
// launch/attach or to join an existing one by id.
type Provider interface {
	CreateSession() (Session, error)
	GetSession(id string) (Session, bool)
}
