package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gorilla/websocket"
)

// upgrader configures the WebSocket handshake. CheckOrigin is permissive
// because bingo has no auth layer yet — tighten this when auth is added.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// handleListSessions returns a JSON array of all active sessions.
//
//	GET /api/sessions
//
// Response: [{"id":"...","state":"idle","clients":1,"createdAt":"..."},...]
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

// handleWS upgrades the HTTP connection to a WebSocket and either creates a
// new session or joins an existing one based on the query parameters:
//
//	GET /ws?create         — create a new session and join it
//	GET /ws?session={id}   — join an existing session by UUID
//
// On success the WebSocket receives an initial SessionState event. On failure
// the connection is closed with a WebSocket close frame carrying the reason.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	// Determine whether to create or join.
	_, wantCreate := query["create"]
	sessionID := query.Get("session")

	if !wantCreate && sessionID == "" {
		http.Error(w, "specify ?create or ?session={id}", http.StatusBadRequest)
		return
	}

	// Upgrade to WebSocket before any session logic so we can send close
	// frames with descriptive reasons on error.
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote the HTTP error.
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

// wsCreate creates a new session and adds the WebSocket as its first client.
func (s *Server) wsCreate(conn *websocket.Conn, log *slog.Logger) {
	sess := s.sessions.create(s.ctx)
	log = log.With("session", sess.id, "action", "create")
	log.Info("client creating new session")
	sess.hub.AddClient(conn, log)
}

// wsJoin adds the WebSocket to an existing session, or closes it if the
// session does not exist.
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
