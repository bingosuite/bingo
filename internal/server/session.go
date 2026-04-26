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

type session struct {
	id        string
	hub       *hub.Hub
	createdAt time.Time
}

func (s *session) info() SessionInfo {
	return SessionInfo{
		ID:        s.id,
		State:     s.hub.State(),
		Clients:   s.hub.ClientCount(),
		CreatedAt: s.createdAt,
	}
}

// sessionStore is the goroutine-safe set of active sessions.
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

// create allocates a new session, starts its hub loop, and watches for
// shutdown. The caller adds the first client.
func (ss *sessionStore) create(ctx context.Context) *session {
	id := uuid.New().String()

	// Each launch/re-launch gets a fresh debugger.
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

	go func() {
		h.Run(ctx)
		ss.remove(id)
		log.Info("session removed")
	}()

	ss.log.Info("session created", "id", id)
	return s
}

func (ss *sessionStore) get(id string) *session {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.sessions[id]
}

func (ss *sessionStore) remove(id string) {
	ss.mu.Lock()
	delete(ss.sessions, id)
	ss.mu.Unlock()
}

func (ss *sessionStore) list() []SessionInfo {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	out := make([]SessionInfo, 0, len(ss.sessions))
	for _, s := range ss.sessions {
		out = append(out, s.info())
	}
	return out
}

func (ss *sessionStore) count() int {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return len(ss.sessions)
}
