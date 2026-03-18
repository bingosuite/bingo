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
	debugEvents   chan debugger.DebugEvent
	debugCommands chan debugger.DebugCommand
	stop          chan bool

	mu sync.RWMutex
}

func NewHub(sessionID string, idleTimeout time.Duration) *Hub {
	debugEvents := make(chan debugger.DebugEvent, 1)
	debugCommands := make(chan debugger.DebugCommand, 1)
	stop := make(chan bool, 1)

	dbg := debugger.NewDebugger(debugCommands, debugEvents, stop)

	return &Hub{
		sessionID:     sessionID,
		connections:   make(map[*Connection]struct{}),
		register:      make(chan *Connection),
		unregister:    make(chan *Connection),
		events:        make(chan Message, eventBufferSize),
		commands:      make(chan Message, commandBufferSize),
		idleTimeout:   idleTimeout,
		lastActivity:  time.Now(),
		dbg:           dbg,
		debugEvents:   debugEvents,
		debugCommands: debugCommands,
		stop:          stop,
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

		case <-h.stop:
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
		case e := <-h.debugEvents:
			switch e.Type {
			case debugger.DbgEventBreakpointHit:
				var dbgEvent debugger.BreakpointHitEvent
				if err := json.Unmarshal(e.Data, &dbgEvent); err != nil {
					log.Printf("[Hub] Failed to unmarshal breakpoint hit event: %v", err)
					continue
				}
				log.Printf("[Hub] [Debugger Event] Breakpoint hit at %s:%d in %s", dbgEvent.Filename, dbgEvent.Line, dbgEvent.Function)

				wsEvent := BreakpointHitEvent{
					Type:      HubEventBreakpointHit,
					SessionID: h.sessionID,
					PID:       dbgEvent.PID,
					Filename:  dbgEvent.Filename,
					Line:      dbgEvent.Line,
					Function:  dbgEvent.Function,
				}
				eventData, err := json.Marshal(wsEvent)
				if err != nil {
					log.Printf("[Hub] Failed to marshal breakpoint event: %v", err)
					continue
				}
				h.Broadcast(Message{Type: string(HubEventBreakpointHit), Data: eventData})
				h.sendStateUpdate(StateBreakpoint)

			case debugger.DbgEventInitialBreakpointHit:
				var dbgEvent debugger.InitialBreakpointHitEvent
				if err := json.Unmarshal(e.Data, &dbgEvent); err != nil {
					log.Printf("[Hub] Failed to unmarshal initial breakpoint hit event: %v", err)
					continue
				}
				log.Printf("[Hub] [Debugger Event] Initial breakpoint hit (PID: %d, session: %s)", dbgEvent.PID, h.sessionID)

				wsEvent := InitialBreakpointHitEvent{
					Type:      HubEventInitialBreakpoint,
					SessionID: h.sessionID,
					PID:       dbgEvent.PID,
				}
				eventData, err := json.Marshal(wsEvent)
				if err != nil {
					log.Printf("[Hub] Failed to marshal initial breakpoint event: %v", err)
					continue
				}
				h.Broadcast(Message{Type: string(HubEventInitialBreakpoint), Data: eventData})
				log.Printf("[Hub] [State Change] Transitioning to breakpoint state (session: %s)", h.sessionID)
				h.sendStateUpdate(StateBreakpoint)

			case debugger.DbgEventDebugEnded:
				log.Printf("[Hub] [Debugger Event] Debug session ended (session: %s)", h.sessionID)
				h.sendStateUpdate(StateReady)
				return
			}
		}
	}
}

// Forward commands from client to debugger
func (h *Hub) handleCommand(cmd Message) {
	switch HubCommandType(cmd.Type) {
	case HubCmdStartDebug:
		var startDebugCmd StartDebugCmd
		if err := json.Unmarshal(cmd.Data, &startDebugCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal startDebug command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] StartDebug received: %s (session: %s)", startDebugCmd.TargetPath, h.sessionID)

		go h.listenForDebuggerEvents()
		go h.dbg.StartWithDebug(startDebugCmd.TargetPath)

	case HubCmdContinue:
		var continueCmd ContinueCmd
		if err := json.Unmarshal(cmd.Data, &continueCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal continue command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] Continue received (session: %s)", h.sessionID)
		h.sendStateUpdate(StateExecuting)
		h.sendCommandToDebugger(debugger.DebugCommand{Type: debugger.DbgCommandContinue}, "Continue")

	case HubCmdSingleStep:
		var singleStepCmd SingleStepCmd
		if err := json.Unmarshal(cmd.Data, &singleStepCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal singleStep command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] SingleStep received (session: %s)", h.sessionID)
		h.sendStateUpdate(StateExecuting)
		h.sendCommandToDebugger(debugger.DebugCommand{Type: debugger.DbgCommandSingleStep}, "SingleStep")

	case HubCmdStepOver:
		var stepOverCmd StepOverCmd
		if err := json.Unmarshal(cmd.Data, &stepOverCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal stepOver command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] StepOver received (session: %s)", h.sessionID)
		h.sendStateUpdate(StateExecuting)
		h.sendCommandToDebugger(debugger.DebugCommand{Type: debugger.DbgCommandStepOver}, "StepOver")

	case HubCmdSetBreakpoint:
		var setBreakpointCmd SetBreakpointCmd
		if err := json.Unmarshal(cmd.Data, &setBreakpointCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal setBreakpoint command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] SetBreakpoint received: %s:%d (session: %s)", setBreakpointCmd.Filename, setBreakpointCmd.Line, h.sessionID)
		h.sendCommandToDebugger(debugger.DebugCommand{
			Type: debugger.DbgCommandSetBreakpoint,
			Data: debugger.SetBreakpointCommand{
				Filename: setBreakpointCmd.Filename,
				Line:     setBreakpointCmd.Line,
			},
		}, "SetBreakpoint")

	case HubCmdExit:
		var exitCmd ExitCmd
		if err := json.Unmarshal(cmd.Data, &exitCmd); err != nil {
			log.Printf("[Hub] Failed to unmarshal exit command: %v", err)
			return
		}
		log.Printf("[Hub] [Command] Exit received (session: %s)", h.sessionID)
		h.sendStateUpdate(StateReady)
		h.sendCommandToDebugger(debugger.DebugCommand{Type: debugger.DbgCommandQuit}, "Exit")

	default:
		log.Printf("[Hub] [Error] Unknown command type: %s", cmd.Type)
	}
}

// Helper method to send state updates
func (h *Hub) sendStateUpdate(state State) {
	log.Printf("[Hub] [State Update] Broadcasting state '%s' to all clients (session: %s)", state, h.sessionID)
	stateEvent := StateUpdateEvent{
		Type:      HubEventStateUpdate,
		SessionID: h.sessionID,
		NewState:  state,
	}

	stateData, err := json.Marshal(stateEvent)
	if err != nil {
		log.Printf("[Hub] Failed to marshal state update event: %v", err)
		return
	}

	h.Broadcast(Message{Type: string(HubEventStateUpdate), Data: stateData})
}

// Helper method to send commands to debugger
func (h *Hub) sendCommandToDebugger(cmd debugger.DebugCommand, cmdName string) {
	select {
	case h.debugCommands <- cmd:
		log.Printf("[Hub] [Command] %s command sent to debugger (session: %s)", cmdName, h.sessionID)
	default:
		log.Printf("[Hub] [Error] Failed to send %s command to debugger - channel full", cmdName)
	}
}
