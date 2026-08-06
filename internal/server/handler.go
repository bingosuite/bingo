package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"
	"github.com/gorilla/websocket"
)

const (
	serviceIdentity      = "bingo"
	ManagementAPIVersion = 1
)

type DAPHealth struct {
	Enabled bool   `json:"enabled"`
	Address string `json:"address"`
}

type ManagedIdleShutdownHealth struct {
	Enabled   bool  `json:"enabled"`
	TimeoutMS int64 `json:"timeoutMs"`
}

type HealthResponse struct {
	Service              string                    `json:"service"`
	ManagementAPIVersion int                       `json:"managementApiVersion"`
	WireProtocolVersion  string                    `json:"wireProtocolVersion"`
	InstanceID           string                    `json:"instanceId"`
	DAP                  DAPHealth                 `json:"dap"`
	ManagedIdleShutdown  ManagedIdleShutdownHealth `json:"managedIdleShutdown"`
	SessionCount         int                       `json:"sessionCount"`
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     sameHostOrigin,
}

func sameHostOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// handleHealth is the stable process-discovery contract frontends use before
// deciding whether an existing bingo instance is compatible.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.lifecycleMu.RLock()
	dapAddress := s.dapAddress
	idleTimeout := s.idleTimeout
	instanceID := s.instanceID
	s.lifecycleMu.RUnlock()

	response := HealthResponse{
		Service:              serviceIdentity,
		ManagementAPIVersion: ManagementAPIVersion,
		WireProtocolVersion:  protocol.Version,
		InstanceID:           instanceID,
		DAP: DAPHealth{
			Enabled: dapAddress != "",
			Address: dapAddress,
		},
		ManagedIdleShutdown: ManagedIdleShutdownHealth{
			Enabled:   idleTimeout > 0,
			TimeoutMS: durationMilliseconds(idleTimeout),
		},
		SessionCount: s.sessions.count(),
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		s.log.Error("failed to encode health response", "err", err)
	}
}

func durationMilliseconds(duration time.Duration) int64 {
	return int64(duration / time.Millisecond)
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
	if !s.acceptingSessions() {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}

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
	if !s.beginSessionOperation() {
		s.closeShuttingDown(conn)
		return
	}
	defer s.sessionOps.Done()

	sess := s.sessions.create(s.ctx)
	log = log.With("session", sess.id, "action", "create")
	log.Info("client creating new session")
	if _, err := sess.hub.AddClient(conn, log); err != nil {
		log.Warn("session closed while adding client", "err", err)
	}
}

func (s *Server) wsJoin(conn *websocket.Conn, sessionID string, log *slog.Logger) {
	if !s.beginSessionOperation() {
		s.closeShuttingDown(conn)
		return
	}
	defer s.sessionOps.Done()

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
	if _, err := sess.hub.AddClient(conn, log); err != nil {
		log.Warn("session closed while joining", "err", err)
	}
}

func (s *Server) closeShuttingDown(conn *websocket.Conn) {
	msg := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server is shutting down")
	_ = conn.WriteMessage(websocket.CloseMessage, msg)
	_ = conn.Close()
}
