package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"

	"github.com/google/uuid"
)

// SessionInfo is the public view of a session, returned by the listing API.
type SessionInfo struct {
	ID        string                `json:"id"`
	State     protocol.SessionState `json:"state"`
	Clients   int                   `json:"clients"`
	CreatedAt time.Time             `json:"createdAt"`
}

// session is the internal bookkeeping for one debug session.
type session struct {
	id        string
	hub       *hub.Hub
	createdAt time.Time
}

// info returns a snapshot of the session's public metadata.
func (s *session) info() SessionInfo {
	return SessionInfo{
		ID:        s.id,
		State:     s.hub.State(),
		Clients:   s.hub.ClientCount(),
		CreatedAt: s.createdAt,
	}
}

// sessionStore manages the set of active sessions. All methods are safe
// to call from multiple goroutines.
type sessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*session
	log      *slog.Logger
}

func newSessionStore(log *slog.Logger) *sessionStore {
	return &sessionStore{
		sessions: make(map[string]*session),
		log:      log,
	}
}

// create allocates a new session: generates a UUID, creates a debugger factory
// and a Hub, starts the hub's Run loop, and watches for shutdown. Returns the
// session so the caller can add the first client.
func (ss *sessionStore) create(ctx context.Context) *session {
	id := uuid.New().String()

	// Each launch/re-launch gets a fresh debugger instance.
	factory := func() debugger.Debugger {
		return debugger.New()
	}

	log := ss.log.With("session", id)
	h := hub.NewSession(id, factory, log)

	s := &session{
		id:        id,
		hub:       h,
		createdAt: time.Now(),
	}

	ss.mu.Lock()
	ss.sessions[id] = s
	ss.mu.Unlock()

	// Start the hub's event loop.
	go func() {
		h.Run(ctx)
		// Hub finished (last client left or context cancelled) — remove session.
		ss.remove(id)
		log.Info("session removed")
	}()

	ss.log.Info("session created", "id", id)
	return s
}

// get returns the session with the given ID, or nil if not found.
func (ss *sessionStore) get(id string) *session {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.sessions[id]
}

// remove deletes a session from the store.
func (ss *sessionStore) remove(id string) {
	ss.mu.Lock()
	delete(ss.sessions, id)
	ss.mu.Unlock()
}

// list returns a snapshot of all active sessions.
func (ss *sessionStore) list() []SessionInfo {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	out := make([]SessionInfo, 0, len(ss.sessions))
	for _, s := range ss.sessions {
		out = append(out, s.info())
	}
	return out
}

// count returns the number of active sessions.
func (ss *sessionStore) count() int {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return len(ss.sessions)
}
