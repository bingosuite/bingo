package ws

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

type Server struct {
	addr string
	hubs map[string]*Hub
	mu   sync.RWMutex
}

func NewServer(addr string) *Server {
	s := &Server{
		addr: addr,
		hubs: make(map[string]*Hub),
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
	client := NewClient(conn, hub)

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
		hub = NewHub(sessionID)
		s.hubs[sessionID] = hub
		s.mu.Unlock()
		log.Printf("Created hub for session: %s", sessionID)
	}
	return hub
}
