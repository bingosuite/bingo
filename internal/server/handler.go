package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"
)

// upgrader: CheckOrigin is permissive because bingo has no auth — tighten
// when auth is added.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleListSessions: GET /api/sessions
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessions := s.sessions.list()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sessions); err != nil {
		s.log.Error("failed to encode sessions", "err", err)
	}
}

// handleWS upgrades to WebSocket and either creates or joins a session.
//
//	GET /ws?create        — create + join
//	GET /ws?session={id}  — join existing
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	_, wantCreate := query["create"]
	sessionID := query.Get("session")

	if !wantCreate && sessionID == "" {
		http.Error(w, "specify ?create or ?session={id}", http.StatusBadRequest)
		return
	}

	// Upgrade before session logic so we can send descriptive close frames on error.
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("websocket upgrade failed", "err", err)
		return
	}

	log := s.log.With("remote", r.RemoteAddr)

	if wantCreate {
		s.wsCreate(conn, log)
	} else {
		s.wsJoin(conn, sessionID, log)
	}
}

func (s *Server) wsCreate(conn *websocket.Conn, log *slog.Logger) {
	sess := s.sessions.create(s.ctx)
	log = log.With("session", sess.id, "action", "create")
	log.Info("client creating new session")
	sess.hub.AddClient(conn, log)
}

func (s *Server) wsJoin(conn *websocket.Conn, sessionID string, log *slog.Logger) {
	log = log.With("session", sessionID, "action", "join")

	sess := s.sessions.get(sessionID)
	if sess == nil {
		log.Warn("session not found")
		msg := websocket.FormatCloseMessage(
			websocket.CloseNormalClosure,
			"session not found: "+sessionID,
		)
		_ = conn.WriteMessage(websocket.CloseMessage, msg)
		_ = conn.Close()
		return
	}

	log.Info("client joining existing session")
	sess.hub.AddClient(conn, log)
}
