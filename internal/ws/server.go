package ws

import (
	"log"
	"net/http"
	"sync"

	"github.com/bingosuite/bingo/config"
	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/gorilla/websocket"
)

type Server struct {
	addr       string
	hubs       map[string]*Hub
	config     config.WebSocketConfig
	mu         sync.RWMutex
	KillServer chan bool
}

func NewServer(addr string, cfg *config.WebSocketConfig) *Server {
	if cfg == nil {
		cfg = &config.Default().WebSocket
	}
	return newServerWithConfig(addr, *cfg)
}

func newServerWithConfig(addr string, cfg config.WebSocketConfig) *Server {
	s := &Server{
		addr:       addr,
		hubs:       make(map[string]*Hub),
		config:     cfg,
		KillServer: make(chan bool),
	}
	http.HandleFunc("/ws/", s.serveWebSocket)
	return s
}

func (s *Server) Serve() error {
	log.Printf("Bingo WebSocket server on %s", s.addr)
	return http.ListenAndServe(s.addr, nil)
}

func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		log.Println("No session ID provided")
		if err := conn.Close(); err != nil {
			log.Printf("WebSocket close error: %v", err)
		}
		return
	}

	hub := s.GetOrCreateHub(sessionID)
	client := NewConnection(conn, hub, r.RemoteAddr)

	go client.ReadPump()
	go client.WritePump()

	hub.Register(client)
}

func (s *Server) GetOrCreateHub(sessionID string) *Hub {
	s.mu.RLock()
	hub, exists := s.hubs[sessionID]
	s.mu.RUnlock()

	if !exists {
		s.mu.Lock()
		// Check max sessions limit
		if s.config.MaxSessions > 0 && len(s.hubs) >= s.config.MaxSessions {
			s.mu.Unlock()
			log.Printf("Max sessions (%d) reached, rejecting session: %s", s.config.MaxSessions, sessionID)
			return nil
		}

		d := debugger.NewDebugger()
		hub = NewHub(sessionID, s.config.IdleTimeout, d)
		hub.onShutdown = s.removeHub // Set callback for cleanup
		s.hubs[sessionID] = hub
		go hub.Run()
		s.mu.Unlock()
		log.Printf("Created hub for session: %s", sessionID)
	}
	return hub
}

func (s *Server) removeHub(sessionID string) {
	s.mu.Lock()
	delete(s.hubs, sessionID)
	s.mu.Unlock()
	log.Printf("Removed hub for session: %s", sessionID)
}

func (s *Server) Shutdown() {
	log.Printf("Shutting down server, closing %d hub(s)", len(s.hubs))

	s.mu.Lock()
	defer s.mu.Unlock()

	// Close all hubs
	for sessionID, hub := range s.hubs {
		log.Printf("Shutting down hub for session: %s", sessionID)

		// Close all connections in the hub
		for c := range hub.connections {
			close(c.send)
			if err := c.conn.Close(); err != nil {
				log.Printf("Error closing connection %s: %v", c.id, err)
				panic(err)
			}
		}

		delete(s.hubs, sessionID)
	}

	log.Println("All hubs and debuggers closed")
}
