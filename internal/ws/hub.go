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

	dbg debugger.Debugger

	// Debugger channels owned by the hub
	debuggerEvents chan debugger.DebuggerEvent
	debugCommand   chan debugger.DebugCommand

	mu sync.RWMutex
}

func NewHub(sessionID string, idleTimeout time.Duration) *Hub {
	debuggerEvents := make(chan debugger.DebuggerEvent, 8)
	debugCommand := make(chan debugger.DebugCommand, 1)

	dbg := debugger.NewDebugger(debuggerEvents, debugCommand)

	return &Hub{
		sessionID:      sessionID,
		connections:    make(map[*Connection]struct{}),
		register:       make(chan *Connection),
		unregister:     make(chan *Connection),
		events:         make(chan Message, eventBufferSize),
		commands:       make(chan Message, commandBufferSize),
		idleTimeout:    idleTimeout,
		lastActivity:   time.Now(),
		dbg:            dbg,
		debuggerEvents: debuggerEvents,
		debugCommand:   debugCommand,
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

		case event := <-h.debuggerEvents:
			if done := h.handleDebuggerEvent(event); done {
				return
			}
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

// handleDebuggerEvent processes a single event from the debugger. It returns
// true when the hub should shut down (i.e. the session has ended).
func (h *Hub) handleDebuggerEvent(event debugger.DebuggerEvent) bool {
	switch e := event.(type) {
	case debugger.BreakpointEvent:
		log.Printf("[Hub] [Debugger Event] Breakpoint hit at %s:%d in %s", e.Filename, e.Line, e.Function)

		eventData, err := json.Marshal(BreakpointHitEvent{
			Type:      EventBreakpointHit,
			SessionID: h.sessionID,
			PID:       e.PID,
			Filename:  e.Filename,
			Line:      e.Line,
			Function:  e.Function,
		})
		if err != nil {
			log.Printf("[Hub] Failed to marshal breakpoint event: %v", err)
			return false
		}
		h.Broadcast(Message{Type: string(EventBreakpointHit), Data: eventData})
		h.sendStateUpdate(StateBreakpoint)
		return false

	case debugger.InitialBreakpointHitEvent:
		log.Printf("[Hub] [Debugger Event] Initial breakpoint hit (PID: %d, session: %s)", e.PID, h.sessionID)

		eventData, err := json.Marshal(InitialBreakpointHitEvent{
			Type:      EventInitialBreakpoint,
			SessionID: h.sessionID,
			PID:       e.PID,
		})
		if err != nil {
			log.Printf("[Hub] Failed to marshal initial breakpoint event: %v", err)
			return false
		}
		h.Broadcast(Message{Type: string(EventInitialBreakpoint), Data: eventData})
		log.Printf("[Hub] [State Change] Transitioning to breakpoint state (session: %s)", h.sessionID)
		h.sendStateUpdate(StateBreakpoint)
		return false

	case debugger.SessionEndedEvent:
		if e.Err != nil {
			log.Printf("[Hub] Debug session %s ended with error: %v", h.sessionID, e.Err)
			h.broadcastError(e.Err)
		} else {
			log.Printf("[Hub] Debugger completed session %s", h.sessionID)
		}
		h.sendStateUpdate(StateReady)
		h.shutdown()
		return true

	default:
		log.Printf("[Hub] Unknown debugger event type: %T", event)
		return false
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
		// StartWithDebug delivers all outcomes via the debuggerEvents channel as a
		// SessionEndedEvent, so the hub's Run loop handles success and failure uniformly.
		go h.dbg.StartWithDebug(startDebugCmd.TargetPath)

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

	case CmdSingleStep:
		var singleStepCmd SingleStepCmd
		if err := json.Unmarshal(cmd.Data, &singleStepCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal singleStep command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] SingleStep received (session: %s)", h.sessionID)

		// Send executing state update
		h.sendStateUpdate(StateExecuting)

		// Send command to debugger
		debugCmd := debugger.DebugCommand{
			Type: "singleStep",
		}
		h.sendCommandToDebugger(debugCmd, "SingleStep")

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
			Type: "stepOver",
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
	case h.debugCommand <- cmd:
		log.Printf("[Hub] [Command] %s command sent to debugger (session: %s)", cmdName, h.sessionID)
	default:
		log.Printf("[Hub] [Error] Failed to send %s command to debugger - channel full", cmdName)
	}
}

// broadcastError sends an error event to all connected clients.
func (h *Hub) broadcastError(err error) {
	event := ErrorEvent{
		Type:      EventError,
		SessionID: h.sessionID,
		Message:   err.Error(),
	}
	data, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		log.Printf("[Hub] Failed to marshal error event: %v", marshalErr)
		return
	}
	h.Broadcast(Message{Type: string(EventError), Data: data})
}
