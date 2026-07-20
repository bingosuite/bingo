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

	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// cmdBufferSize bounds the queue of bingo commands awaiting the hub's read
// pump. The pump drains it promptly (injectCommand is a channel send), so a
// small buffer is plenty; it exists mainly so the DAP read loop never blocks
// while a burst of setBreakpoints commands is enqueued.
const cmdBufferSize = 64

// dapWriteTimeout bounds a single socket write so a DAP client that stops
// reading cannot park the writer goroutine indefinitely. Generous because DAP
// clients are local (IDE on the same host); exceeding it means the peer is
// genuinely wedged, so send() tears the connection down.
const dapWriteTimeout = 10 * time.Second

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
	// hub's read pump (via ReadMessage). ReadMessage drains it with priority
	// over done so a command enqueued immediately before Close (e.g. Kill on
	// disconnect) is still delivered.
	cmdOut chan []byte

	done      chan struct{}
	closeOnce sync.Once

	// writeMu serialises DAP writes to conn — two writers race here: the DAP
	// read loop (request responses) and the hub write pump (event
	// translations via WriteMessage). It also guards seq.
	writeMu sync.Mutex
	seq     int

	// mu guards all coordination state below. Never held across a socket
	// write (release mu, then take writeMu) or a cmdOut enqueue.
	mu sync.Mutex

	session Session
	client  *hub.Client

	// Handshake / lifecycle flags.
	launching   bool
	restarting  bool
	suspended   bool
	stopOnEntry bool
	attached    bool

	startReqSeq   int
	startCmd      string
	restartReqSeq int
	curThreadID   int

	// pendingContinues counts Continue commands THIS adapter issued whose
	// resulting EventContinued must be suppressed (a DAP adapter must not emit
	// `continued` for a resume it initiated). Out-of-band continues from other
	// clients arrive with the counter at 0 and ARE surfaced. See AGENTS.md →
	// Suspend/resume (EventContinued).
	pendingContinues int

	// Breakpoint bookkeeping. bpByFile maps file -> requested line -> bingo
	// breakpoint id (0 = set enqueued, awaiting confirmation). setQ/clearQ are
	// FIFOs of in-flight confirmations, correlated to the hub's ordered
	// event stream (valid while the DAP client is the sole breakpoint driver).
	bpByFile map[string]map[int]int
	setQ     []*bpSlot
	clearQ   []int

	// Data-request correlation FIFOs, one per bingo confirmation event kind.
	threadsQ []int
	framesQ  []int
	localsQ  []*varsReq

	cachedFrames []protocol.Frame
}

// bpSlot is one requested breakpoint within a setBreakpoints request, awaiting
// (or already holding) its resolved DAP breakpoint.
type bpSlot struct {
	req      *bpRequest
	file     string
	line     int
	resolved bool
	bp       godap.Breakpoint
}

// bpRequest collects the slots of a single setBreakpoints request; its response
// is sent once every slot is resolved (in request order — see AGENTS.md).
type bpRequest struct {
	reqSeq int
	slots  []*bpSlot
}

func (r *bpRequest) done() bool {
	for _, s := range r.slots {
		if !s.resolved {
			return false
		}
	}
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
		bpByFile: make(map[string]map[int]int),
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

// ReadMessage delivers the next bingo command to the hub's read pump. It
// prefers a buffered command over done so a command enqueued right before Close
// (Kill on disconnect) is still handed off.
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
		return 0, nil, io.EOF
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
