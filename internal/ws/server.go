package ws

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/bingosuite/bingo/config"
	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Server struct {
	addr   string
	hubs   map[string]*Hub
	config config.WebSocketConfig
	mu     sync.RWMutex
}

func NewServer(addr string, cfg *config.WebSocketConfig) *Server {
	if cfg == nil {
		cfg = &config.Default().WebSocket
	}
	return newServerWithConfig(addr, *cfg)
}

func newServerWithConfig(addr string, cfg config.WebSocketConfig) *Server {
	s := &Server{
		addr:   addr,
		hubs:   make(map[string]*Hub),
		config: cfg,
	}

	http.HandleFunc("/ws/", s.getOrCreateSession)
	http.HandleFunc("/sessions", s.getSessions)
	return s
}

func (s *Server) Serve() error {
	log.Printf("[Server] Bingo WebSocket server on %s", s.addr)
	return http.ListenAndServe(s.addr, nil)
}

func (s *Server) getSessions(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sessions := make([]string, 0, len(s.hubs))
	for sessionID := range s.hubs {
		sessions = append(sessions, sessionID)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sessions); err != nil {
		log.Printf("[Server] Error encoding sessions: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *Server) getOrCreateSession(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[Server] WebSocket upgrade failed: %v", err)
		return
	}

	sessionID := r.URL.Query().Get("session")
	sessionProvided := sessionID != ""
	if !sessionProvided {
		sessionID = uuid.New().String()
		log.Printf("[Server] No session ID provided, generating new session ID: %v", sessionID)
	}

	var hub *Hub
	if sessionProvided {
		// Only get existing hub if session ID was provided by client
		hub, err = s.GetHub(sessionID)
		if err != nil {
			log.Printf("[Server] Session %s not found: %v", sessionID, err)
			if err := conn.Close(); err != nil {
				log.Printf("[Server] WebSocket close error: %v", err)
			}
			return
		}
	} else {
		// Create new hub only for server-generated session IDs
		hub, err = s.CreateHub(sessionID)
		if err != nil {
			log.Printf("[Server] Unable to create hub for session %s: %v, shutting down...", sessionID, err)
			if err := conn.Close(); err != nil {
				log.Printf("[Server] WebSocket close error: %v", err)
			}
			return
		}
	}
	client := NewConnection(conn, hub, r.RemoteAddr)

	go client.ReadPump()
	go client.WritePump()

	hub.Register(client)
	ack := SessionStartedEvent{
		Type:      EventSessionStarted,
		SessionID: sessionID,
		PID:       0,
	}
	data, err := json.Marshal(ack)
	if err != nil {
		log.Printf("[Server] Failed to marshal sessionStarted: %v", err)
		return
	}
	client.send <- Message{
		Type: string(EventSessionStarted),
		Data: data,
	}
}

// GetHub retrieves an existing hub for the given session ID.
func (s *Server) GetHub(sessionID string) (*Hub, error) {
	s.mu.RLock()
	hub, exists := s.hubs[sessionID]
	s.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}
	return hub, nil
}

// CreateHub creates a new hub for the given session ID.
func (s *Server) CreateHub(sessionID string) (*Hub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if hub already exists
	if _, exists := s.hubs[sessionID]; exists {
		return nil, fmt.Errorf("session already exists: %s", sessionID)
	}

	// Check max sessions limit
	if s.config.MaxSessions > 0 && len(s.hubs) >= s.config.MaxSessions {
		log.Printf("[Server] Max sessions (%d) reached, rejecting session: %s", s.config.MaxSessions, sessionID)
		return nil, fmt.Errorf("max sessions (%d) reached", s.config.MaxSessions)
	}

	d := debugger.NewDebugger()
	hub := NewHub(sessionID, s.config.IdleTimeout, d)
	hub.onShutdown = s.removeHub // Set callback for cleanup
	s.hubs[sessionID] = hub
	go hub.Run()
	log.Printf("[Server] Created hub for session: %s", sessionID)

	return hub, nil
}

func (s *Server) removeHub(sessionID string) {
	s.mu.Lock()
	delete(s.hubs, sessionID)
	s.mu.Unlock()
	log.Printf("[Server] Removed hub for session: %s", sessionID)
}

func (s *Server) Shutdown() {
	log.Printf("[Server] Shutting down server, closing %d hub(s)", len(s.hubs))

	s.mu.Lock()
	defer s.mu.Unlock()

	// Close all hubs
	for sessionID, hub := range s.hubs {
		log.Printf("[Server] Shutting down hub for session: %s", sessionID)

		// Close all connections in the hub
		for c := range hub.connections {
			c.CloseSend()
			c.Close()
		}

		delete(s.hubs, sessionID)
	}

	log.Println("[Server] All hubs and debuggers closed")
}
