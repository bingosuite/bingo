package ws

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
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

	debugger *debugger.Debugger

	mu sync.RWMutex
}

func NewHub(sessionID string, idleTimeout time.Duration, dbg *debugger.Debugger) *Hub {
	return &Hub{
		sessionID:    sessionID,
		connections:  make(map[*Connection]struct{}),
		register:     make(chan *Connection),
		unregister:   make(chan *Connection),
		events:       make(chan Message, eventBufferSize),
		commands:     make(chan Message, commandBufferSize),
		idleTimeout:  idleTimeout,
		lastActivity: time.Now(),
		debugger:     dbg,
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
			h.handleCommand(cmd)

		case <-h.debugger.EndDebugSession:
			log.Printf("Debugger signaled end of session %s, shutting down hub", h.sessionID)
			h.shutdown()
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

func (h *Hub) handleCommand(cmd Message) {
	if h.debugger == nil {
		log.Printf("No debugger attached to hub %s, ignoring command", h.sessionID)
		return
	}

	switch CommandType(cmd.Type) {
	case CmdStartDebug:
		var startDebugCmd StartDebugCmd
		if err := json.Unmarshal(cmd.Data, &startDebugCmd); err != nil {
			log.Printf("Failed to unmarshal startDebug command: %v", err)
			return
		}
		log.Printf("Starting debug session for %s in session %s", startDebugCmd.TargetPath, h.sessionID)
		go h.debugger.StartWithDebug(startDebugCmd.TargetPath)

	case CmdContinue:
		var continueCmd ContinueCmd
		if err := json.Unmarshal(cmd.Data, &continueCmd); err != nil {
			log.Printf("Failed to unmarshal continue command: %v", err)
			return
		}
		log.Printf("Forwarding continue command to debugger for session %s", h.sessionID)
		h.debugger.Continue(h.debugger.DebugInfo.Target.PID)

	case CmdStepOver:
		var stepOverCmd StepOverCmd
		if err := json.Unmarshal(cmd.Data, &stepOverCmd); err != nil {
			log.Printf("Failed to unmarshal stepOver command: %v", err)
			return
		}
		log.Printf("Forwarding stepOver command to debugger for session %s", h.sessionID)
		h.debugger.SingleStep(h.debugger.DebugInfo.Target.PID)

	case CmdExit:
		var exitCmd ExitCmd
		if err := json.Unmarshal(cmd.Data, &exitCmd); err != nil {
			log.Printf("Failed to unmarshal exit command: %v", err)
			return
		}
		log.Printf("Forwarding exit command to debugger for session %s", h.sessionID)
		h.debugger.StopDebug()

	default:
		log.Printf("Unknown command type: %s", cmd.Type)
	}
}
