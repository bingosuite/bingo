package ws

import (
	"log"
	"sync"
)

type Hub struct {
    sessionID string
    clients   map[*Client]struct{}
    
    // Channels for register/unregister clients and broadcast msgs
    register   chan *Client
    unregister chan *Client
    events     chan Message
    commands   chan Message
    
    mu sync.RWMutex
}

func NewHub(sessionID string) *Hub {
    h := &Hub{
        sessionID: sessionID,
        clients:   make(map[*Client]struct{}),
        register:  make(chan *Client),
        unregister: make(chan *Client),
        events:    make(chan Message, 256),
        commands:  make(chan Message, 32),
    }
    go h.Run()
    return h
}

func (h *Hub) Run() {
    for {
        select {
        case client := <-h.register:
            h.mu.Lock()
            h.clients[client] = struct{}{}
            h.mu.Unlock()
            log.Printf("Client connected to hub %s (%d total)", h.sessionID, len(h.clients))
            
        case client := <-h.unregister:
            h.mu.Lock()
            if _, ok := h.clients[client]; ok {
                delete(h.clients, client)
                close(client.send)
            }
            h.mu.Unlock()
            
        case event := <-h.events:
            h.mu.RLock()
            for client := range h.clients {
                select {
                case client.send <- event:
                default:
                    // drop client if too slow or err
                }
            }
            h.mu.RUnlock()
            
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
