package dap

import (
	"encoding/json"
	"fmt"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// dispatchRequest routes one decoded DAP request to its handler. Requests bingo
// cannot service are acknowledged (success, empty) so a compliant client
// proceeds; truly unknown ones get an error response.
func (h *Handler) dispatchRequest(msg godap.Message) {
	switch r := msg.(type) {
	case *godap.InitializeRequest:
		h.onInitialize(r)
	case *godap.LaunchRequest:
		h.onLaunch(r)
	case *godap.AttachRequest:
		h.onAttach(r)
	case *godap.SetBreakpointsRequest:
		h.onSetBreakpoints(r)
	case *godap.SetExceptionBreakpointsRequest:
		h.send(&godap.SetExceptionBreakpointsResponse{Response: h.response(r.Seq, "setExceptionBreakpoints")})
	case *godap.ConfigurationDoneRequest:
		h.onConfigurationDone(r)
	case *godap.ContinueRequest:
		h.onContinue(r)
	case *godap.NextRequest:
		h.onStep(r.Seq, "next", protocol.CmdStepOver)
	case *godap.StepInRequest:
		h.onStep(r.Seq, "stepIn", protocol.CmdStepInto)
	case *godap.StepOutRequest:
		h.onStep(r.Seq, "stepOut", protocol.CmdStepOut)
	case *godap.PauseRequest:
		h.onPause(r)
	default:
		h.dispatchInspectRequest(msg)
	}
}

// dispatchInspectRequest routes the read-only inspection requests (threads,
// stack, scopes, variables, evaluate) and the teardown requests (disconnect,
// terminate, restart). Split out of dispatchRequest purely to keep each switch's
// cyclomatic complexity in check; the DAP request types are disjoint, so the
// split has no ordering or behavioural effect.
func (h *Handler) dispatchInspectRequest(msg godap.Message) {
	switch r := msg.(type) {
	case *godap.ThreadsRequest:
		h.onThreads(r)
	case *godap.StackTraceRequest:
		h.onStackTrace(r)
	case *godap.ScopesRequest:
		h.onScopes(r)
	case *godap.VariablesRequest:
		h.onVariables(r)
	case *godap.EvaluateRequest:
		h.onEvaluate(r)
	case *godap.DisconnectRequest:
		h.onDisconnect(r)
	case *godap.TerminateRequest:
		h.onTerminate(r)
	case *godap.RestartRequest:
		h.onRestart(r)
	default:
		if rm, ok := msg.(godap.RequestMessage); ok {
			req := rm.GetRequest()
			h.send(h.errorResponse(req.Seq, req.Command, "unsupported request: "+req.Command))
		}
	}
}

func (h *Handler) onInitialize(req *godap.InitializeRequest) {
	h.send(&godap.InitializeResponse{
		Response: h.response(req.Seq, "initialize"),
		Body: godap.Capabilities{
			SupportsConfigurationDoneRequest: true,
			SupportsTerminateRequest:         true,
			SupportsRestartRequest:           true,
			SupportTerminateDebuggee:         true,
			SupportsEvaluateForHovers:        true,
		},
	})
}

func (h *Handler) onLaunch(req *godap.LaunchRequest) {
	var cfg launchConfig
	if len(req.Arguments) > 0 {
		if err := json.Unmarshal(req.Arguments, &cfg); err != nil {
			h.send(h.errorResponse(req.Seq, "launch", "invalid launch arguments: "+err.Error()))
			return
		}
	}
	if cfg.Program == "" {
		h.send(h.errorResponse(req.Seq, "launch", "launch requires 'program'"))
		return
	}
	if err := h.startSession(cfg.Session); err != nil {
		h.send(h.errorResponse(req.Seq, "launch", err.Error()))
		return
	}

	h.mu.Lock()
	h.startReqSeq = req.Seq
	h.startCmd = "launch"
	h.stopOnEntry = cfg.StopOnEntry
	h.attached = false
	h.launching = true
	h.mu.Unlock()

	h.announceSession()

	cmd, err := marshalCommand(protocol.CmdLaunch, protocol.LaunchPayload{
		Program: cfg.Program, Args: cfg.Args, Env: cfg.Env,
	})
	if err != nil {
		h.send(h.errorResponse(req.Seq, "launch", err.Error()))
		return
	}
	h.enqueue(cmd)
}

func (h *Handler) onAttach(req *godap.AttachRequest) {
	var cfg launchConfig
	if len(req.Arguments) > 0 {
		if err := json.Unmarshal(req.Arguments, &cfg); err != nil {
			h.send(h.errorResponse(req.Seq, "attach", "invalid attach arguments: "+err.Error()))
			return
		}
	}
	// A `session` argument with no pid means "join an already-running bingo
	// session as an additional client", not "attach to an OS process". Route it
	// to the join path, which registers as a client without relaunching.
	if cfg.Session != "" && cfg.PID == 0 {
		h.onJoin(req, cfg)
		return
	}
	if cfg.PID == 0 {
		h.send(h.errorResponse(req.Seq, "attach", "attach requires 'pid' (or 'session' to join an existing bingo session)"))
		return
	}
	if err := h.startSession(cfg.Session); err != nil {
		h.send(h.errorResponse(req.Seq, "attach", err.Error()))
		return
	}

	h.mu.Lock()
	h.startReqSeq = req.Seq
	h.startCmd = "attach"
	h.stopOnEntry = cfg.StopOnEntry
	h.attached = true
	h.launching = true
	h.mu.Unlock()

	h.announceSession()

	binary := cfg.BinaryPath
	if binary == "" {
		binary = cfg.Program
	}
	cmd, err := marshalCommand(protocol.CmdAttach, protocol.AttachPayload{PID: cfg.PID, BinaryPath: binary})
	if err != nil {
		h.send(h.errorResponse(req.Seq, "attach", err.Error()))
		return
	}
	h.enqueue(cmd)
}

// onJoin registers this connection as an ADDITIONAL client on an existing bingo
// session (a DAP attach carrying a `session` argument and no pid). Unlike attach,
// it enqueues no Launch/Attach command — the session is already running under
// other clients — so it must not disturb the shared run state. The hub's welcome
// EventSessionState is translated (onSessionState) into the joiner's initial DAP
// state. `initialized` fires immediately: the target image is already loaded, so
// breakpoints resolve right away and there is no entry stop to wait for.
func (h *Handler) onJoin(req *godap.AttachRequest, cfg launchConfig) {
	// Set the join flags BEFORE registering as a client: the hub delivers its
	// welcome EventSessionState as soon as AddClient runs, and onSessionState
	// must see awaitingWelcome=true to translate it into the initial DAP state.
	h.mu.Lock()
	h.startReqSeq = req.Seq
	h.startCmd = "attach"
	h.stopOnEntry = cfg.StopOnEntry
	h.attached = true
	h.joining = true
	h.awaitingWelcome = true
	h.mu.Unlock()

	if err := h.startSession(cfg.Session); err != nil {
		h.mu.Lock()
		h.joining = false
		h.awaitingWelcome = false
		h.mu.Unlock()
		h.send(h.errorResponse(req.Seq, "attach", err.Error()))
		return
	}

	h.announceSession()
	h.send(&godap.InitializedEvent{Event: h.event("initialized")})
}

// startSession creates a new managed session (or joins an existing one) and
// registers this handler as a hub client. Registering BEFORE the Launch/Attach
// command is enqueued guarantees the entry-stop event is delivered to us.
func (h *Handler) startSession(existingID string) error {
	h.mu.Lock()
	if h.session != nil || h.sessionStarting {
		h.mu.Unlock()
		return fmt.Errorf("session already started for this connection")
	}
	h.sessionStarting = true
	h.mu.Unlock()

	started := false
	defer func() {
		if started {
			return
		}
		h.mu.Lock()
		h.sessionStarting = false
		h.mu.Unlock()
	}()

	var sess Session
	if existingID != "" {
		s, ok := h.provider.GetSession(existingID)
		if !ok {
			return fmt.Errorf("unknown session %q", existingID)
		}
		sess = s
	} else {
		s, err := h.provider.CreateSession()
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		sess = s
	}

	client, err := sess.AddClient(h, h.log)
	if err != nil {
		return fmt.Errorf("add client: %w", err)
	}

	h.mu.Lock()
	h.session = sess
	h.client = client
	h.sessionStarting = false
	started = true
	h.mu.Unlock()
	return nil
}

// announceSession publishes the managed-session identity once the handler is
// attached. The state is claimed under mu, but DAP writes happen after unlock
// because hub delivery and request handling can call this path concurrently.
func (h *Handler) announceSession() {
	h.mu.Lock()
	sess := h.session
	if sess == nil || h.sessionAnnounced {
		h.mu.Unlock()
		return
	}
	id := sess.SessionID()
	h.sessionAnnounced = true
	h.mu.Unlock()

	h.send(&sessionEvent{
		Event: h.event(sessionEventName),
		Body: sessionEventBody{
			Version:   protocol.DAPSessionEventVersion,
			SessionID: id,
		},
	})
	h.emitConsole(fmt.Sprintf("bingo session %s ready — observers can join with ?session=%s\n", id, id))
}

func (h *Handler) onConfigurationDone(req *godap.ConfigurationDoneRequest) {
	h.send(&godap.ConfigurationDoneResponse{Response: h.response(req.Seq, "configurationDone")})

	h.mu.Lock()
	startSeq := h.startReqSeq
	startCmd := h.startCmd
	stopOnEntry := h.stopOnEntry
	joining := h.joining
	tid := h.curThreadID
	if !joining && !stopOnEntry {
		h.pendingContinues++
		h.suspended = false
	}
	h.mu.Unlock()

	switch startCmd {
	case "launch":
		h.send(&godap.LaunchResponse{Response: h.response(startSeq, "launch")})
	case "attach":
		h.send(&godap.AttachResponse{Response: h.response(startSeq, "attach")})
	}

	if joining {
		// Joined an existing session: never resume it or fabricate an entry
		// stop. Its current run state was already reflected from the welcome
		// (onSessionState).
		return
	}
	if stopOnEntry {
		h.sendStopped("entry", tid)
		return
	}
	if cmd, err := marshalCommand(protocol.CmdContinue, nil); err == nil {
		h.enqueue(cmd)
	}
}

func (h *Handler) onContinue(req *godap.ContinueRequest) {
	h.mu.Lock()
	h.pendingContinues++
	h.suspended = false
	h.mu.Unlock()

	if cmd, err := marshalCommand(protocol.CmdContinue, nil); err == nil {
		h.enqueue(cmd)
	}
	h.send(&godap.ContinueResponse{
		Response: h.response(req.Seq, "continue"),
		Body:     godap.ContinueResponseBody{AllThreadsContinued: true},
	})
}

func (h *Handler) onStep(reqSeq int, command string, kind protocol.CommandKind) {
	h.mu.Lock()
	h.suspended = false
	h.mu.Unlock()

	if cmd, err := marshalCommand(kind, nil); err == nil {
		h.enqueue(cmd)
	}
	// next/stepIn/stepOut responses are bare acknowledgements sharing the same
	// wire shape; a NextResponse serialises identically for all three.
	h.send(&godap.NextResponse{Response: h.response(reqSeq, command)})
}

func (h *Handler) onPause(req *godap.PauseRequest) {
	if cmd, err := marshalCommand(protocol.CmdPause, nil); err == nil {
		h.enqueue(cmd)
	}
	h.send(&godap.PauseResponse{Response: h.response(req.Seq, "pause")})
}

func (h *Handler) onThreads(req *godap.ThreadsRequest) {
	h.mu.Lock()
	suspended := h.suspended
	if suspended {
		h.threadsQ = append(h.threadsQ, req.Seq)
	}
	h.mu.Unlock()

	if !suspended {
		h.send(&godap.ThreadsResponse{
			Response: h.response(req.Seq, "threads"),
			Body:     godap.ThreadsResponseBody{Threads: dapThreads(nil)},
		})
		return
	}
	if cmd, err := marshalCommand(protocol.CmdGoroutines, nil); err == nil {
		h.enqueue(cmd)
	}
}

func (h *Handler) onStackTrace(req *godap.StackTraceRequest) {
	h.mu.Lock()
	suspended := h.suspended
	currentThreadID := h.curThreadID
	requestedThreadID := req.Arguments.ThreadId
	if suspended && currentThreadID > 0 && requestedThreadID == currentThreadID {
		h.framesQ = append(h.framesQ, req.Seq)
	}
	h.mu.Unlock()

	if !suspended {
		h.send(&godap.StackTraceResponse{
			Response: h.response(req.Seq, "stackTrace"),
			Body:     godap.StackTraceResponseBody{StackFrames: []godap.StackFrame{}},
		})
		return
	}
	if currentThreadID == 0 || requestedThreadID != currentThreadID {
		h.send(&godap.StackTraceResponse{
			Response: h.response(req.Seq, "stackTrace"),
			Body:     godap.StackTraceResponseBody{StackFrames: []godap.StackFrame{}},
		})
		return
	}
	if cmd, err := marshalCommand(protocol.CmdFrames, nil); err == nil {
		h.enqueue(cmd)
	}
}

func (h *Handler) onScopes(req *godap.ScopesRequest) {
	// One synthetic "Locals" scope per frame. Its variablesReference IS the
	// frame id, which the variables request decodes back to a frame index.
	h.send(&godap.ScopesResponse{
		Response: h.response(req.Seq, "scopes"),
		Body: godap.ScopesResponseBody{Scopes: []godap.Scope{{
			Name:               "Locals",
			VariablesReference: req.Arguments.FrameId,
			Expensive:          false,
		}}},
	})
}

func (h *Handler) onVariables(req *godap.VariablesRequest) {
	ref := req.Arguments.VariablesReference

	h.mu.Lock()
	// A child ref addresses an already-computed subtree (cached eagerly from a
	// typed EventLocals/EventEvaluate). Serve it synchronously — no round-trip.
	if children, ok := h.varCache[ref]; ok {
		h.mu.Unlock()
		h.send(&godap.VariablesResponse{
			Response: h.response(req.Seq, "variables"),
			Body:     godap.VariablesResponseBody{Variables: children},
		})
		return
	}
	// Otherwise it is a frame-root ref (a scope's reference == frameIndex+1):
	// fetch that frame's locals from the engine.
	frameIndex := frameIndexFromRef(ref)
	if frameIndex < 0 {
		frameIndex = 0
	}
	suspended := h.suspended
	if suspended {
		h.localsQ = append(h.localsQ, &varsReq{seq: req.Seq, frameIndex: frameIndex})
	}
	h.mu.Unlock()

	if !suspended {
		h.send(&godap.VariablesResponse{
			Response: h.response(req.Seq, "variables"),
			Body:     godap.VariablesResponseBody{Variables: []godap.Variable{}},
		})
		return
	}
	if cmd, err := marshalCommand(protocol.CmdLocals, protocol.LocalsPayloadCmd{FrameIndex: frameIndex}); err == nil {
		h.enqueue(cmd)
	}
}

// onEvaluate answers a DAP evaluate request by resolving a single VARIABLE NAME
// (not an expression) in the given frame. bingo's evaluate is name-only; the
// context ("watch"/"hover"/"repl") is handled uniformly. Not suspended → a
// best-effort empty response rather than blocking (matches threads/variables).
func (h *Handler) onEvaluate(req *godap.EvaluateRequest) {
	name := req.Arguments.Expression
	frameIndex := frameIndexFromRef(req.Arguments.FrameId)
	if frameIndex < 0 {
		frameIndex = 0
	}

	h.mu.Lock()
	suspended := h.suspended
	if suspended {
		h.evalQ = append(h.evalQ, req.Seq)
	}
	h.mu.Unlock()

	if !suspended {
		h.send(&godap.EvaluateResponse{
			Response: h.response(req.Seq, "evaluate"),
			Body:     godap.EvaluateResponseBody{Result: "", VariablesReference: 0},
		})
		return
	}
	if cmd, err := marshalCommand(protocol.CmdEvaluate, protocol.EvaluatePayloadCmd{FrameIndex: frameIndex, Name: name}); err == nil {
		h.enqueue(cmd)
	}
}

func (h *Handler) onDisconnect(req *godap.DisconnectRequest) {
	h.mu.Lock()
	attached := h.attached
	hasSession := h.session != nil
	h.mu.Unlock()

	// Launch sessions terminate the debuggee by default; attach sessions leave
	// it running. terminateDebuggee overrides either way.
	terminate := !attached
	if req.Arguments != nil && req.Arguments.TerminateDebuggee {
		terminate = true
	}

	if terminate && hasSession {
		if cmd, err := marshalCommand(protocol.CmdKill, nil); err == nil {
			h.enqueue(cmd) // drained by ReadMessage's priority path before EOF
		}
	}
	h.send(&godap.DisconnectResponse{Response: h.response(req.Seq, "disconnect")})
	_ = h.Close()
}

func (h *Handler) onTerminate(req *godap.TerminateRequest) {
	h.mu.Lock()
	hasSession := h.session != nil
	h.mu.Unlock()

	if hasSession {
		if cmd, err := marshalCommand(protocol.CmdKill, nil); err == nil {
			h.enqueue(cmd)
		}
	}
	h.send(&godap.TerminateResponse{Response: h.response(req.Seq, "terminate")})
}

func (h *Handler) onRestart(req *godap.RestartRequest) {
	h.mu.Lock()
	// restartReqSeq gates only the unanswered DAP request. restarting stays set
	// longer so the subsequent entry EventStepped is still recognized.
	if h.restartReqSeq != 0 {
		h.mu.Unlock()
		h.send(h.errorResponse(req.Seq, "restart", "restart already in progress"))
		return
	}
	hasSession := h.session != nil
	if hasSession {
		h.restarting = true
		h.restartReqSeq = req.Seq
		// Remember the view being cleared: DAP allows restart while running, so
		// a rejected restart must restore this exact state, not a suspension.
		h.restartWasSuspended = h.suspended
		h.suspended = false
	}
	h.mu.Unlock()

	if !hasSession {
		h.send(h.errorResponse(req.Seq, "restart", "no session to restart"))
		return
	}
	// Reuse the original program/args/env (nil overrides). Response is sent
	// when EventRestarted arrives.
	if cmd, err := marshalCommand(protocol.CmdRestart, protocol.RestartPayload{}); err == nil {
		h.enqueue(cmd)
	}
}
