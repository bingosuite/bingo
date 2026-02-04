package ws

import (
	"log"
	"sync"
	"time"
)

const (
	clientSendBufferSize = 256
	eventBufferSize      = 256
	commandBufferSize    = 32
	hubTickerInterval    = 1 * time.Minute
)

type Hub struct {
	sessionID   string
	connections map[*Connection]struct{}

	// Channels for register/unregister clients and broadcast msgs
	register   chan *Connection
	unregister chan *Connection
	events     chan Message
	commands   chan Message

	onShutdown func(sessionID string) // callback for shutdown on server

	// idle detection
	idleTimeout  time.Duration
	lastActivity time.Time

	mu sync.RWMutex
}

func NewHub(sessionID string, idleTimeout time.Duration) *Hub {
	return &Hub{
		sessionID:    sessionID,
		connections:  make(map[*Connection]struct{}),
		register:     make(chan *Connection),
		unregister:   make(chan *Connection),
		events:       make(chan Message, eventBufferSize),
		commands:     make(chan Message, commandBufferSize),
		idleTimeout:  idleTimeout,
		lastActivity: time.Now(),
	}
}

func (h *Hub) Run() {
	ticker := time.NewTicker(hubTickerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check idle timeout
			if h.idleTimeout > 0 && len(h.connections) == 0 {
				if time.Since(h.lastActivity) > h.idleTimeout {
					log.Printf("Session %s idle for %v, shutting down", h.sessionID, h.idleTimeout)
					h.shutdown()
					return
				}
			}

		case client := <-h.register:
			h.mu.Lock()
			h.connections[client] = struct{}{}
			h.lastActivity = time.Now()
			h.mu.Unlock()
			log.Printf("Client %s connected to hub %s (%d total)", client.id, h.sessionID, len(h.connections))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.connections[client]; ok {
				delete(h.connections, client)
				close(client.send)
				log.Printf("Client %s disconnected from hub %s (%d remaining)", client.id, h.sessionID, len(h.connections))

				// When last client leaves, shutdown hub
				if len(h.connections) == 0 {
					h.mu.Unlock()
					log.Printf("Session %s has no clients, shutting down hub", h.sessionID)
					h.shutdown()
					return
				}

			}
			h.mu.Unlock()

		case event := <-h.events:
			h.lastActivity = time.Now()
			h.mu.RLock()
			var slowConnections []*Connection
			for connection := range h.connections {
				select {
				case connection.send <- event:
				default: // when a send operation blocks (connection consuming too slowly or reader goroutine died), discard the connection
					slowConnections = append(slowConnections, connection)
				}
			}
			h.mu.RUnlock()
			for _, connection := range slowConnections {
				log.Printf("Connection %s is slow; unregistering from hub %s", connection.id, h.sessionID)
				h.Unregister(connection)
			}

		case cmd := <-h.commands:
			log.Printf("Hub %s command: %s", h.sessionID, cmd.Type)
			//TODO: forward commands to debugger
		}
	}
}

// Public APIs
func (h *Hub) Register(connection *Connection) {
	h.register <- connection
}

func (h *Hub) Unregister(connection *Connection) {
	h.unregister <- connection
}

func (h *Hub) Broadcast(event Message) {
	h.events <- event
}

func (h *Hub) SendCommand(cmd Message) {
	h.commands <- cmd
}

func (h *Hub) shutdown() {
	if h.onShutdown != nil {
		h.onShutdown(h.sessionID)
	}
}
