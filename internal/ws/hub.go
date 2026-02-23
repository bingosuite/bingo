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
					log.Printf("[Hub] Session %s idle for %v, shutting down", h.sessionID, h.idleTimeout)
					h.shutdown()
					return
				}
			}

		case client := <-h.register:
			h.mu.Lock()
			h.connections[client] = struct{}{}
			h.lastActivity = time.Now()
			h.mu.Unlock()
			log.Printf("[Hub] Client %s connected to hub %s (%d total)", client.id, h.sessionID, len(h.connections))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.connections[client]; ok {
				delete(h.connections, client)
				client.CloseSend()
				log.Printf("[Hub] Client %s disconnected from hub %s (%d remaining)", client.id, h.sessionID, len(h.connections))

				// When last client leaves, shutdown hub
				if len(h.connections) == 0 {
					h.mu.Unlock()
					log.Printf("[Hub] Session %s has no clients, shutting down hub", h.sessionID)
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
				log.Printf("[Hub] Connection %s is slow; unregistering from hub %s", connection.id, h.sessionID)
				h.Unregister(connection)
			}

		case cmd := <-h.commands:
			log.Printf("[Hub] Hub %s command: %s", h.sessionID, cmd.Type)
			h.handleCommand(cmd)

		case <-h.debugger.EndDebugSession:
			log.Printf("[Hub] Debugger signaled end of session %s, shutting down hub", h.sessionID)
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
			log.Printf("[Hub] [Debugger Event] Breakpoint hit at %s:%d in %s", bpEvent.Filename, bpEvent.Line, bpEvent.Function)

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
				log.Printf("[Hub] Failed to marshal breakpoint event: %v", err)
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
				log.Printf("[Hub] Failed to marshal state update event: %v", err)
				continue
			}

			stateMessage := Message{
				Type: string(EventStateUpdate),
				Data: stateData,
			}

			h.Broadcast(stateMessage)

		case initialBpEvent := <-h.debugger.InitialBreakpointHit:
			log.Printf("[Hub] [Debugger Event] Initial breakpoint hit (PID: %d, session: %s)", initialBpEvent.PID, h.sessionID)

			// Create and send initial breakpoint event to all clients
			event := InitialBreakpointHitEvent{
				Type:      EventInitialBreakpoint,
				SessionID: h.sessionID,
				PID:       initialBpEvent.PID,
			}

			eventData, err := json.Marshal(event)
			if err != nil {
				log.Printf("[Hub] Failed to marshal initial breakpoint event: %v", err)
				continue
			}

			message := Message{
				Type: string(EventInitialBreakpoint),
				Data: eventData,
			}

			h.Broadcast(message)

			// Also send state update to indicate we're at a breakpoint
			log.Printf("[Hub] [State Change] Transitioning to breakpoint state (session: %s)", h.sessionID)
			stateEvent := StateUpdateEvent{
				Type:      EventStateUpdate,
				SessionID: h.sessionID,
				NewState:  StateBreakpoint,
			}

			stateData, err := json.Marshal(stateEvent)
			if err != nil {
				log.Printf("[Hub] Failed to marshal state update event: %v", err)
				continue
			}

			stateMessage := Message{
				Type: string(EventStateUpdate),
				Data: stateData,
			}

			h.Broadcast(stateMessage)

		case <-h.debugger.EndDebugSession:
			log.Println("[Hub] Debugger event listener ending, sending state update to ready")
			h.sendStateUpdate(StateReady)
			return
		}
	}
}

// Forward commands from client to debugger
func (h *Hub) handleCommand(cmd Message) {
	switch CommandType(cmd.Type) {
	case CmdStartDebug:
		var startDebugCmd StartDebugCmd
		if err := json.Unmarshal(cmd.Data, &startDebugCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal startDebug command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] StartDebug received: %s (session: %s)", startDebugCmd.TargetPath, h.sessionID)

		// Start listening for events from this new debugger
		go h.listenForDebuggerEvents()

		// Start the debug session
		go h.debugger.StartWithDebug(startDebugCmd.TargetPath)

	case CmdContinue:
		var continueCmd ContinueCmd
		if err := json.Unmarshal(cmd.Data, &continueCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal continue command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] Continue received (session: %s)", h.sessionID)

		// Send executing state update
		h.sendStateUpdate(StateExecuting)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "continue",
		}
		h.sendCommandToDebugger(debugCmd, "Continue")

	case CmdStepOver:
		var stepOverCmd StepOverCmd
		if err := json.Unmarshal(cmd.Data, &stepOverCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal stepOver command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] StepOver received (session: %s)", h.sessionID)

		// Send executing state update
		h.sendStateUpdate(StateExecuting)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "step",
		}
		h.sendCommandToDebugger(debugCmd, "StepOver")

	case CmdSetBreakpoint:
		var setBreakpointCmd SetBreakpointCmd
		if err := json.Unmarshal(cmd.Data, &setBreakpointCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal setBreakpoint command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] SetBreakpoint received: %s:%d (session: %s)", setBreakpointCmd.Filename, setBreakpointCmd.Line, h.sessionID)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "setBreakpoint",
			Data: map[string]any{
				"line":     setBreakpointCmd.Line,
				"filename": setBreakpointCmd.Filename,
			},
		}
		h.sendCommandToDebugger(debugCmd, "SetBreakpoint")

	case CmdExit:
		var exitCmd ExitCmd
		if err := json.Unmarshal(cmd.Data, &exitCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal exit command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] Exit received (session: %s)", h.sessionID)

		h.sendStateUpdate(StateReady)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "quit",
		}
		h.sendCommandToDebugger(debugCmd, "Exit")

	default:
		log.Printf("[Hub] [Error] Unknown command type: %s", cmd.Type)
	}
}

// Helper method to send state updates
func (h *Hub) sendStateUpdate(state State) {
	log.Printf("[Hub] [State Update] Broadcasting state '%s' to all clients (session: %s)", state, h.sessionID)
	stateEvent := StateUpdateEvent{
		Type:      EventStateUpdate,
		SessionID: h.sessionID,
		NewState:  state,
	}

	stateData, err := json.Marshal(stateEvent)
	if err != nil {
		log.Printf("[Hub] Failed to marshal state update event: %v", err)
		return
	}

	stateMessage := Message{
		Type: string(EventStateUpdate),
		Data: stateData,
	}

	h.Broadcast(stateMessage)
}

// Helper method to send commands to debugger
func (h *Hub) sendCommandToDebugger(cmd debugger.DebugCommand, cmdName string) {
	select {
	case h.debugger.DebugCommand <- cmd:
		log.Printf("[Hub] [Command] %s command sent to debugger (session: %s)", cmdName, h.sessionID)
	default:
		log.Printf("[Hub] [Error] Failed to send %s command to debugger - channel full", cmdName)
	}
}
