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

	// Start listening for debugger events if debugger is attached
	if h.debugger != nil {
		go h.listenForDebuggerEvents()
	}

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

// Listen for events from the debugger (breakpoint hits)
func (h *Hub) listenForDebuggerEvents() {
	for {
		select {
		case bpEvent := <-h.debugger.BreakpointHit:
			log.Printf("Received breakpoint event from debugger: %s:%d", bpEvent.Filename, bpEvent.Line)

			// Create and send breakpoint hit event to all clients
			event := BreakpointHitEvent{
				Type:      EventBreakpointHit,
				SessionID: h.sessionID,
				PID:       bpEvent.PID,
				Filename:  bpEvent.Filename,
				Line:      bpEvent.Line,
				Function:  bpEvent.Function,
			}

			eventData, err := json.Marshal(event)
			if err != nil {
				log.Printf("Failed to marshal breakpoint event: %v", err)
				continue
			}

			message := Message{
				Type: string(EventBreakpointHit),
				Data: eventData,
			}

			h.Broadcast(message)

			// Also send state update to indicate we're at a breakpoint
			stateEvent := StateUpdateEvent{
				Type:      EventStateUpdate,
				SessionID: h.sessionID,
				NewState:  StateBreakpoint,
			}

			stateData, err := json.Marshal(stateEvent)
			if err != nil {
				log.Printf("Failed to marshal state update event: %v", err)
				continue
			}

			stateMessage := Message{
				Type: string(EventStateUpdate),
				Data: stateData,
			}

			h.Broadcast(stateMessage)

		case initialBpEvent := <-h.debugger.InitialBreakpointHit:
			log.Printf("Received initial breakpoint event from debugger for PID %d", initialBpEvent.PID)

			// Create and send initial breakpoint event to all clients
			event := InitialBreakpointEvent{
				Type:      EventInitialBreakpoint,
				SessionID: h.sessionID,
				PID:       initialBpEvent.PID,
			}

			eventData, err := json.Marshal(event)
			if err != nil {
				log.Printf("Failed to marshal initial breakpoint event: %v", err)
				continue
			}

			message := Message{
				Type: string(EventInitialBreakpoint),
				Data: eventData,
			}

			h.Broadcast(message)

			// Also send state update to indicate we're at a breakpoint
			stateEvent := StateUpdateEvent{
				Type:      EventStateUpdate,
				SessionID: h.sessionID,
				NewState:  StateBreakpoint,
			}

			stateData, err := json.Marshal(stateEvent)
			if err != nil {
				log.Printf("Failed to marshal state update event: %v", err)
				continue
			}

			stateMessage := Message{
				Type: string(EventStateUpdate),
				Data: stateData,
			}

			h.Broadcast(stateMessage)

		case <-h.debugger.EndDebugSession:
			log.Println("Debugger event listener ending")
			return
		}
	}
}

// Forward commands from client to debugger
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
		log.Printf("Sending continue command to debugger for session %s", h.sessionID)

		// Send executing state update
		h.sendStateUpdate(StateExecuting)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "continue",
		}
		select {
		case h.debugger.DebugCommand <- debugCmd:
		default:
			log.Printf("Failed to send continue command to debugger - channel full")
		}

	case CmdStepOver:
		var stepOverCmd StepOverCmd
		if err := json.Unmarshal(cmd.Data, &stepOverCmd); err != nil {
			log.Printf("Failed to unmarshal stepOver command: %v", err)
			return
		}
		log.Printf("Sending step command to debugger for session %s", h.sessionID)

		// Send executing state update
		h.sendStateUpdate(StateExecuting)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "step",
		}
		select {
		case h.debugger.DebugCommand <- debugCmd:
		default:
			log.Printf("Failed to send step command to debugger - channel full")
		}

	case CmdSetBreakpoint:
		var setBreakpointCmd SetBreakpointCmd
		if err := json.Unmarshal(cmd.Data, &setBreakpointCmd); err != nil {
			log.Printf("Failed to unmarshal setBreakpoint command: %v", err)
			return
		}
		log.Printf("Sending set breakpoint command for line %d in session %s", setBreakpointCmd.Line, h.sessionID)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "setBreakpoint",
			Data: map[string]interface{}{
				"line":     setBreakpointCmd.Line,
				"filename": setBreakpointCmd.Filename,
			},
		}
		select {
		case h.debugger.DebugCommand <- debugCmd:
		default:
			log.Printf("Failed to send set breakpoint command to debugger - channel full")
		}

	case CmdExit:
		var exitCmd ExitCmd
		if err := json.Unmarshal(cmd.Data, &exitCmd); err != nil {
			log.Printf("Failed to unmarshal exit command: %v", err)
			return
		}
		log.Printf("Sending quit command to debugger for session %s", h.sessionID)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "quit",
		}
		select {
		case h.debugger.DebugCommand <- debugCmd:
		default:
			log.Printf("Failed to send quit command to debugger - channel full")
		}

	default:
		log.Printf("Unknown command type: %s", cmd.Type)
	}
}

// Helper method to send state updates
func (h *Hub) sendStateUpdate(state State) {
	stateEvent := StateUpdateEvent{
		Type:      EventStateUpdate,
		SessionID: h.sessionID,
		NewState:  state,
	}

	stateData, err := json.Marshal(stateEvent)
	if err != nil {
		log.Printf("Failed to marshal state update event: %v", err)
		return
	}

	stateMessage := Message{
		Type: string(EventStateUpdate),
		Data: stateData,
	}

	h.Broadcast(stateMessage)
}
