package dap

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/internal/dapclient"
	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// cmdBufferSize bounds the queue of bingo commands awaiting the hub's read
// pump. It absorbs normal setBreakpoints bursts while the hub applies its own
// bounded backpressure; persistent hub overload closes the handler and unblocks
// enqueue through done.
const cmdBufferSize = 64

// dapWriteTimeout bounds a single socket write so a DAP client that stops
// reading cannot park the writer goroutine indefinitely. Generous because DAP
// clients are local (IDE on the same host); exceeding it means the peer is
// genuinely wedged, so send() tears the connection down.
const dapWriteTimeout = 10 * time.Second

const sessionEventName = protocol.DAPSessionEventName

type sessionEvent = dapclient.SessionEvent
type sessionEventBody = dapclient.SessionEventBody

// Handler bridges one DAP TCP client to a bingo hub session. It implements
// hub.WSConn: the hub's write pump feeds it bingo events via WriteMessage
// (translated to DAP), and the hub's read pump pulls bingo commands from it via
// ReadMessage (produced by the DAP read loop in Serve). See AGENTS.md → DAP.
type Handler struct {
	conn     net.Conn
	reader   *bufio.Reader
	provider Provider
	log      *slog.Logger

	// cmdOut carries marshalled bingo Commands from the DAP read loop to the
	// hub's read pump (via ReadMessage). ReadMessage checks it before waiting
	// and once more when done wins so a command enqueued before Close (e.g.
	// Kill on disconnect) is still delivered.
	cmdOut chan []byte

	done      chan struct{}
	closeOnce sync.Once

	// writeMu serialises DAP writes to conn — two writers race here: the DAP
	// read loop (request responses) and the hub write pump (event
	// translations via WriteMessage). It also guards seq.
	writeMu sync.Mutex
	seq     int

	// flushMu serialises outbox drains so staged commands are handed to the hub
	// in staging order. Never taken while holding mu.
	flushMu sync.Mutex

	// mu guards all coordination state below. Never held across a socket
	// write (release mu, then take writeMu) or a cmdOut enqueue.
	mu sync.Mutex

	session Session
	client  *hub.Client

	sessionStarting  bool
	sessionAnnounced bool

	// Handshake / lifecycle flags.
	launching   bool
	restarting  bool
	suspended   bool
	stopOnEntry bool
	attached    bool

	// joining marks a connection that joined an EXISTING bingo session (a DAP
	// attach with a `session` argument and no pid) rather than launching or
	// attaching to a debuggee. A joiner never enqueues Launch/Attach and never
	// auto-continues at configurationDone — it must not disturb the run state of
	// a session other clients are already driving. awaitingWelcome is set until
	// the hub's welcome EventSessionState is consumed to reflect the session's
	// current state (a `stopped` if already suspended).
	joining         bool
	awaitingWelcome bool

	startReqSeq   int
	startCmd      string
	restartReqSeq int
	curThreadID   int
	// stopThreadUnknown records that the suspending event honestly omitted
	// stopped.threadId. The first subsequent threads response is collapsed to
	// one resolved or synthetic stopped-thread handle so clients do not request
	// the same stopped stack once for every goroutine.
	stopThreadUnknown bool

	// restartWasSuspended is the suspended view this connection held when it
	// issued the in-flight restart, captured before onRestart clears it
	// optimistically. DAP permits restart while the tracee is RUNNING, and the
	// hub rejects some restarts (an attach-created session, no prior Launch)
	// without touching the process at all — so a rejected restart must restore
	// exactly the prior view rather than unconditionally claiming a suspension
	// that never existed.
	restartWasSuspended bool

	// sessionState is the last lifecycle state a managed hub broadcast. It stays
	// empty for a raw hub (hub.New), which broadcasts none. idle/exited mean no
	// live process, which is the one case a resume rejection must NOT be
	// resynchronized into a suspension (see failResume/failRestart): the hub
	// reports a failed relaunch as an error followed by idle, and answers every
	// later command with "no active debugger".
	sessionState protocol.SessionState

	// pendingContinues counts Continue commands THIS adapter issued whose
	// resulting EventContinued must be suppressed (a DAP adapter must not emit
	// `continued` for a resume it initiated). Out-of-band continues from other
	// clients arrive with the counter at 0 and ARE surfaced. See AGENTS.md →
	// Suspend/resume (EventContinued).
	pendingContinues int

	// Breakpoint bookkeeping. bpByFile maps file -> line -> that line's
	// transaction state (see bpLine). setQ/clearQ are FIFOs of in-flight
	// operations, correlated to the hub's ordered event stream (valid while the
	// DAP client is the sole breakpoint driver).
	bpByFile map[string]map[int]*bpLine
	setQ     []*bpOp
	clearQ   []*bpOp

	// outbox stages breakpoint commands so they reach the hub in the exact
	// order their setQ/clearQ slot was reserved. Two goroutines produce them —
	// the DAP read loop (a new setBreakpoints) and the hub write pump (a
	// confirmation that advances a line) — and the FIFOs are only meaningful
	// if the wire order matches the reservation order. See flushCommands.
	outbox [][]byte

	// Data-request correlation FIFOs, one per bingo confirmation event kind.
	threadsQ []int
	framesQ  []int
	localsQ  []*varsReq
	evalQ    []int

	cachedFrames []protocol.Frame

	// varCache maps a child variablesReference to the DAP variables it expands
	// to. It is populated eagerly from a typed EventLocals/EventEvaluate subtree
	// (buildVarTree) and read synchronously by a follow-up variables request.
	// nextVarRef allocates those child refs from varRefBase upward. Both reset
	// at every stop — the tree reflects one memory snapshot, so refs from a
	// prior suspension are stale. Since the reset happens at the next stop,
	// onVariables also gates cache hits on the current suspended state.
	varCache   map[int][]godap.Variable
	nextVarRef int
}

// varRefBase is the first child variablesReference. Child refs start well above
// any frame-root ref (a scope's reference IS the frameID == frameIndex+1,
// bounded by the max stack depth), so the two ranges never collide and
// onVariables can distinguish a child ref from a frame-root ref by magnitude.
const varRefBase = 1 << 16

// bpSlot is one requested breakpoint within a setBreakpoints request, awaiting
// (or already holding) its resolved DAP breakpoint.
type bpSlot struct {
	req      *bpRequest
	line     int
	source   godap.Source
	resolved bool
	bp       godap.Breakpoint
}

// bpOp is one in-flight debugger command for a single file:line. A line has at
// most one LIVE operation at a time, which is what keeps the id-less FIFO
// correlation unambiguous: the head of setQ/clearQ is the operation the next
// confirmation belongs to — and which queue it sits in is what makes it a set or
// a clear. An operation whose line was discarded by a restart is abandoned and
// no longer speaks for that line, but stays queued so its answer still pops the
// head it reserved (see liveLineLocked).
type bpOp struct {
	file string
	line int
}

// bpLine keeps what is INSTALLED in the debugger separate from what the client
// last asked for, because pipelined setBreakpoints requests make the two
// legitimately disagree. installedID is a fact — set only by a confirmed
// SetBreakpoint, dropped only by a confirmed ClearBreakpoint — while desired is
// the latest-wins intent of the most recent request for the source. A line with
// the two in agreement is converged; otherwise exactly one operation is live and
// every waiter parks here until it completes. dapID is the stable client-facing
// identity, which deliberately outlives a debugger id change across restart.
type bpLine struct {
	dapID       int
	installedID int
	loc         protocol.Location
	desired     bool
	failure     string

	pending     *bpOp
	setWaiters  []*bpSlot
	clearOwners []*bpRequest
}

// bpRequest collects one setBreakpoints request's outstanding work: a slot per
// requested line plus openClears, the removals it owns. Its response is sent
// once BOTH are settled, so a request can never report success while a clear it
// caused is still in flight (or has failed).
type bpRequest struct {
	reqSeq       int
	slots        []*bpSlot
	openClears   int
	clearFailure string
	responded    bool
}

// ready reports whether the request may be answered now, claiming the right to
// answer it. Slots and clear obligations settle on different goroutines and can
// complete in either order, so the claim is what guarantees exactly one
// response per request. Caller MUST hold h.mu.
func (r *bpRequest) ready() bool {
	if r.responded || r.openClears > 0 {
		return false
	}
	for _, s := range r.slots {
		if !s.resolved {
			return false
		}
	}
	r.responded = true
	return true
}

// varsReq correlates a DAP variables request to its EventLocals confirmation.
type varsReq struct {
	seq        int
	frameIndex int
}

// launchConfig is the union of bingo's custom launch/attach arguments. DAP
// leaves the shape of these to the adapter (LaunchRequest.Arguments is raw
// JSON).
type launchConfig struct {
	Program     string   `json:"program"`
	Args        []string `json:"args,omitempty"`
	Env         []string `json:"env,omitempty"`
	StopOnEntry bool     `json:"stopOnEntry,omitempty"`
	NoDebug     bool     `json:"noDebug,omitempty"`

	// Attach.
	PID        int    `json:"pid,omitempty"`
	BinaryPath string `json:"binaryPath,omitempty"`

	// Session, when set, joins an existing managed session as its driver
	// instead of creating a new one.
	Session string `json:"session,omitempty"`
}

// NewHandler wraps an accepted DAP connection. Call Serve to run the read loop.
func NewHandler(conn net.Conn, provider Provider, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		conn:     conn,
		reader:   bufio.NewReader(conn),
		provider: provider,
		log:      log,
		cmdOut:   make(chan []byte, cmdBufferSize),
		done:     make(chan struct{}),
		bpByFile: make(map[string]map[int]*bpLine),
		varCache: make(map[int][]godap.Variable),
	}
}

// Serve runs the DAP read loop until the connection closes. Blocking; the
// server's accept loop runs it in its own goroutine.
func (h *Handler) Serve() {
	defer func() { _ = h.Close() }()
	for {
		msg, err := godap.ReadProtocolMessage(h.reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !isClosedConn(err) {
				h.log.Warn("dap: read error", "err", err)
			}
			return
		}
		h.dispatchRequest(msg)
	}
}

func isClosedConn(err error) bool {
	return errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
}

// --- hub.WSConn implementation -------------------------------------------------

// ReadMessage delivers the next bingo command to the hub's read pump. It checks
// for a buffered command before waiting and again when done wins so a command
// enqueued before Close (Kill on disconnect) is handed off before EOF.
func (h *Handler) ReadMessage() (int, []byte, error) {
	select {
	case b := <-h.cmdOut:
		return hub.TextMessage, b, nil
	default:
	}
	select {
	case b := <-h.cmdOut:
		return hub.TextMessage, b, nil
	case <-h.done:
		select {
		case b := <-h.cmdOut:
			return hub.TextMessage, b, nil
		default:
			return 0, nil, io.EOF
		}
	}
}

// WriteMessage receives a marshalled bingo event from the hub write pump and
// translates it to DAP. Non-text frames (ping/close) are ignored. It always
// returns nil so the hub never treats the DAP client as a failed writer — the
// DAP socket lifecycle is owned by Serve/Close.
func (h *Handler) WriteMessage(messageType int, data []byte) error {
	if messageType != hub.TextMessage {
		return nil
	}
	evt, err := protocol.UnmarshalEvent(data)
	if err != nil {
		h.log.Warn("dap: undecodable event", "err", err)
		return nil
	}
	h.translateEvent(evt)
	return nil
}

func (h *Handler) SetReadLimit(int64)              {}
func (h *Handler) SetReadDeadline(time.Time) error { return nil }

// SetWriteDeadline is a no-op: the hub's write pump calls it to bound each
// write, but the DAP read loop also writes (responses) outside the pump, so the
// bound is applied uniformly inside send() instead — see dapWriteTimeout.
func (h *Handler) SetWriteDeadline(time.Time) error          { return nil }
func (h *Handler) SetPongHandler(func(appData string) error) {}

// Close is idempotent: it unblocks ReadMessage (EOF), stops Serve, and closes
// the socket. The hub reacts to the EOF by removing this client.
func (h *Handler) Close() error {
	h.closeOnce.Do(func() {
		close(h.done)
		_ = h.conn.Close()
	})
	return nil
}

// --- outbound DAP writes -------------------------------------------------------

func (h *Handler) send(m godap.Message) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.seq++
	setSeqField(reflect.ValueOf(m).Elem(), h.seq)
	// Bound the write so a DAP client that stops reading can't park this
	// goroutine (and leak its fd) forever. On any write error the socket is
	// wedged or gone, so tear the connection down: that unblocks Serve's read
	// and makes the hub drop this client (WriteMessage always returns nil, so
	// nothing else would).
	_ = h.conn.SetWriteDeadline(time.Now().Add(dapWriteTimeout))
	if err := godap.WriteProtocolMessage(h.conn, m); err != nil {
		if !isClosedConn(err) {
			h.log.Warn("dap: write error", "err", err)
		}
		_ = h.Close()
	}
}

// setSeqField sets the embedded ProtocolMessage.Seq on any go-dap message via
// reflection. Every message embeds ProtocolMessage (through Response/Event), so
// a single int field named "Seq" is reachable at some anonymous-embed depth.
func setSeqField(v reflect.Value, seq int) bool {
	if v.Kind() != reflect.Struct {
		return false
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		fv := v.Field(i)
		if f.Name == "Seq" && fv.Kind() == reflect.Int && fv.CanSet() {
			fv.SetInt(int64(seq))
			return true
		}
		if f.Anonymous && fv.Kind() == reflect.Struct {
			if setSeqField(fv, seq) {
				return true
			}
		}
	}
	return false
}

func (h *Handler) response(reqSeq int, command string) godap.Response {
	return godap.Response{
		ProtocolMessage: godap.ProtocolMessage{Type: "response"},
		RequestSeq:      reqSeq,
		Success:         true,
		Command:         command,
	}
}

func (h *Handler) event(name string) godap.Event {
	return godap.Event{ProtocolMessage: godap.ProtocolMessage{Type: "event"}, Event: name}
}

func (h *Handler) errorResponse(reqSeq int, command, msg string) *godap.ErrorResponse {
	return &godap.ErrorResponse{
		Response: godap.Response{
			ProtocolMessage: godap.ProtocolMessage{Type: "response"},
			RequestSeq:      reqSeq,
			Success:         false,
			Command:         command,
			Message:         msg,
		},
		Body: godap.ErrorResponseBody{Error: &godap.ErrorMessage{Format: msg, ShowUser: true}},
	}
}

func (h *Handler) emitConsole(msg string) {
	h.send(&godap.OutputEvent{
		Event: h.event("output"),
		Body:  godap.OutputEventBody{Category: "console", Output: msg},
	})
}

func (h *Handler) sendStopped(reason string, tid int) {
	h.send(&godap.StoppedEvent{
		Event: h.event("stopped"),
		Body:  godap.StoppedEventBody{Reason: reason, ThreadId: tid, AllThreadsStopped: true},
	})
}

// enqueue hands a marshalled bingo command to the hub read pump.
func (h *Handler) enqueue(cmd []byte) {
	if cmd == nil {
		return
	}
	select {
	case h.cmdOut <- cmd:
	case <-h.done:
	}
}

// queueCommandLocked stages a breakpoint command for the hub. Staging (rather
// than enqueuing directly) is what keeps the wire order equal to the setQ/clearQ
// reservation order: the reservation happens under mu, but the enqueue cannot
// (it may block), so two producers could otherwise interleave and correlate a
// confirmation to the wrong operation. Caller MUST hold h.mu.
func (h *Handler) queueCommandLocked(cmd []byte) {
	if cmd == nil {
		return
	}
	h.outbox = append(h.outbox, cmd)
}

// flushCommands drains staged commands to the hub in staging order. flushMu
// serialises concurrent drains; mu is released around each enqueue so the rule
// against holding mu across a cmdOut send still holds.
func (h *Handler) flushCommands() {
	h.flushMu.Lock()
	defer h.flushMu.Unlock()
	for {
		h.mu.Lock()
		if len(h.outbox) == 0 {
			h.mu.Unlock()
			return
		}
		cmd := h.outbox[0]
		h.outbox = h.outbox[1:]
		h.mu.Unlock()
		h.enqueue(cmd)
	}
}

// allocVarRef reserves the next child variablesReference. Caller MUST hold h.mu.
func (h *Handler) allocVarRef() int {
	if h.nextVarRef < varRefBase {
		h.nextVarRef = varRefBase
	}
	h.nextVarRef++
	return h.nextVarRef
}

// resetVarsLocked drops the cached variable subtrees and ref allocator. Child
// references are only valid within one suspension (they expand a memory
// snapshot taken at that stop); the cache is rebuilt from the next
// EventLocals/EventEvaluate. Caller MUST hold h.mu.
func (h *Handler) resetVarsLocked() {
	if len(h.varCache) > 0 {
		h.varCache = make(map[int][]godap.Variable)
	}
	h.nextVarRef = 0
}

// sessionEndedLocked reports whether the hub has told us there is no live
// process: a failed relaunch leaves a managed session idle, and a terminated one
// exited. Caller MUST hold h.mu.
func (h *Handler) sessionEndedLocked() bool {
	return h.sessionState == protocol.StateIdle || h.sessionState == protocol.StateExited
}
