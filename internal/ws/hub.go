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
)

type Hub struct {
	sessionID string
	clients   map[*Client]struct{}

	// Channels for register/unregister clients and broadcast msgs
	register   chan *Client
	unregister chan *Client
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
		clients:      make(map[*Client]struct{}),
		register:     make(chan *Client),
		unregister:   make(chan *Client),
		events:       make(chan Message, eventBufferSize),
		commands:     make(chan Message, commandBufferSize),
		idleTimeout:  idleTimeout,
		lastActivity: time.Now(),
	}
}

func (h *Hub) Run() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check idle timeout
			if h.idleTimeout > 0 && len(h.clients) == 0 {
				if time.Since(h.lastActivity) > h.idleTimeout {
					log.Printf("Session %s idle for %v, shutting down", h.sessionID, h.idleTimeout)
					h.shutdown()
					return
				}
			}

		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = struct{}{}
			h.lastActivity = time.Now()
			h.mu.Unlock()
			log.Printf("Client %s connected to hub %s (%d total)", client.id, h.sessionID, len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				log.Printf("Client %s disconnected from hub %s (%d remaining)", client.id, h.sessionID, len(h.clients))

				// When last client leaves, shutdown hub
				if len(h.clients) == 0 {
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
			var slowClients []*Client
			for client := range h.clients {
				select {
				case client.send <- event:
				default: // when a send operation blocks (client consuming too slowly or reader goroutine died), discard the client
					slowClients = append(slowClients, client)
				}
			}
			h.mu.RUnlock()
			for _, client := range slowClients {
				log.Printf("Client %s is slow; unregistering from hub %s", client.id, h.sessionID)
				h.Unregister(client)
			}

		case cmd := <-h.commands:
			log.Printf("Hub %s command: %s", h.sessionID, cmd.Type)
			//TODO: forward commands to debugger
		}
	}
}

// Public APIs
func (h *Hub) Register(client *Client) {
	h.register <- client
}

func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
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
