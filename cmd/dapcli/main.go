// Command dapcli is an interactive terminal client for the bingo debug server
// that drives a session over the Debug Adapter Protocol (DAP) instead of the
// native WebSocket protocol (cmd/cli). It mirrors cmd/cli's command set and UX,
// translating each command into DAP requests and printing DAP events.
//
//	dapcli [-addr host:port] [-session id]
//
// With -session it JOINS an already-running bingo session as an additional
// client (a DAP attach carrying the session id and no pid). Any number of DAP
// clients and WebSocket clients (cmd/cli) can connect to and drive the same
// session concurrently — DAP clients through the -dap-addr listener, WebSocket
// clients through the HTTP listener.
//
// Without -session the session is created lazily by the first `launch`/`attach`
// command (the DAP model has no standalone "create empty session" request). The
// adapter prints the new session id on an `output` event so other clients can
// join it.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
	godap "github.com/google/go-dap"
)

const (
	prompt       = "bingo> "
	replyTimeout = 15 * time.Second
)

type breakpoint struct {
	line     int
	id       int
	verified bool
}

// dapCLI is a minimal interactive DAP client: a demuxing read loop matches
// responses to waiters by request seq and prints events, while the REPL
// goroutine sends requests.
type dapCLI struct {
	conn   net.Conn
	reader *bufio.Reader

	// mu guards seq and pending (the response-waiter registry).
	mu      sync.Mutex
	seq     int
	pending map[int]chan godap.Message

	// writeMu serialises WriteProtocolMessage across the REPL goroutine, the
	// initialized-handler goroutine, and any command handler — concurrent writes
	// to one net.Conn would interleave frames.
	writeMu sync.Mutex

	// stateMu guards the fields below.
	stateMu    sync.Mutex
	configured bool // handshake past `initialized`: breakpoints go on the wire now
	curThread  int  // most recent stopped threadId (DAP requires one on control ops)
	bpsByFile  map[string][]breakpoint
	sessionID  string

	// printMu serialises console output so async event prints don't interleave.
	printMu sync.Mutex
}

func main() {
	addr := flag.String("addr", "localhost:4711", "DAP server address (host:port)")
	sessionID := flag.String("session", "", "existing bingo session ID to join (omit to create on launch)")
	flag.Parse()

	conn, err := net.Dial("tcp4", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: dial DAP %s: %v\n", *addr, err)
		os.Exit(1)
	}

	h := &dapCLI{
		conn:      conn,
		reader:    bufio.NewReader(conn),
		pending:   make(map[int]chan godap.Message),
		bpsByFile: make(map[string][]breakpoint),
		curThread: 1,
	}
	go h.readLoop()

	if *sessionID != "" {
		fmt.Printf("joining session %s on %s over DAP...\n", *sessionID, *addr)
	} else {
		fmt.Printf("connecting to %s over DAP (session created on launch)...\n", *addr)
	}

	// initialize handshake — block for the capabilities response.
	if _, err := h.request("initialize", &godap.InitializeRequest{
		Arguments: godap.InitializeRequestArguments{
			AdapterID:                    "bingo",
			LinesStartAt1:                true,
			ColumnsStartAt1:              true,
			PathFormat:                   "path",
			ClientID:                     "bingo-dapcli",
			ClientName:                   "bingo dapcli",
			SupportsRunInTerminalRequest: false,
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: initialize: %v\n", err)
		os.Exit(1)
	}

	if *sessionID != "" {
		// Join: attach carrying the session id and no pid. The response arrives
		// after our auto-configurationDone, so fire it and let the read loop
		// drive the rest of the handshake.
		args, _ := json.Marshal(map[string]any{"session": *sessionID})
		h.setSessionID(*sessionID)
		h.fire("attach", &godap.AttachRequest{Arguments: args})
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          prompt,
		HistoryFile:     os.ExpandEnv("$HOME/.bingo_dap_history"),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error initializing readline: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = rl.Close() }()

	printHelp()
	for {
		line, err := rl.Readline()
		if err != nil {
			if err == readline.ErrInterrupt || err == io.EOF {
				fmt.Println("bye")
				h.close()
				return
			}
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if h.dispatch(strings.Fields(line)) {
			h.close()
			return
		}
	}
}

// dispatch runs one REPL command, returning true when the CLI should exit.
//
//nolint:gocyclo // command routing is a flat switch while the command set is small.
func (h *dapCLI) dispatch(args []string) bool {
	switch cmd := args[0]; cmd {
	case "state":
		h.stateMu.Lock()
		fmt.Printf("  session=%s  configured=%v  thread=%d\n", h.sessionID, h.configured, h.curThread)
		h.stateMu.Unlock()

	case "launch":
		if len(args) < 2 {
			fmt.Println("  usage: launch <binary> [args...]")
			return false
		}
		payload := map[string]any{"program": args[1], "stopOnEntry": true}
		if len(args) > 2 {
			payload["args"] = args[2:]
		}
		raw, _ := json.Marshal(payload)
		h.fire("launch", &godap.LaunchRequest{Arguments: raw})

	case "attach":
		if len(args) < 2 {
			fmt.Println("  usage: attach <pid> [binary-path]")
			return false
		}
		pid, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Printf("  invalid pid: %s\n", args[1])
			return false
		}
		payload := map[string]any{"pid": pid, "stopOnEntry": true}
		if len(args) > 2 {
			payload["binaryPath"] = args[2]
		}
		raw, _ := json.Marshal(payload)
		h.fire("attach", &godap.AttachRequest{Arguments: raw})

	case "kill":
		h.fire("terminate", &godap.TerminateRequest{})

	case "restart":
		if _, err := h.request("restart", &godap.RestartRequest{}); err != nil {
			printErr(err)
		} else {
			fmt.Println("  restarted (breakpoints reinstalled)")
		}

	case "c", "continue":
		h.fire("continue", &godap.ContinueRequest{Arguments: godap.ContinueArguments{ThreadId: h.thread()}})

	case "n", "next":
		h.fire("next", &godap.NextRequest{Arguments: godap.NextArguments{ThreadId: h.thread()}})

	case "s", "step":
		h.fire("stepIn", &godap.StepInRequest{Arguments: godap.StepInArguments{ThreadId: h.thread()}})

	case "out", "finish":
		h.fire("stepOut", &godap.StepOutRequest{Arguments: godap.StepOutArguments{ThreadId: h.thread()}})

	case "p", "pause":
		h.fire("pause", &godap.PauseRequest{Arguments: godap.PauseArguments{ThreadId: h.thread()}})

	case "b", "break":
		if len(args) < 2 {
			fmt.Println("  usage: break <file>:<line>")
			return false
		}
		file, line, ok := parseFileLine(args[1])
		if !ok {
			fmt.Println("  usage: break <file>:<line>  (e.g. main.go:42)")
			return false
		}
		h.addBreakpoint(file, line)

	case "clear":
		if len(args) < 2 {
			fmt.Println("  usage: clear <breakpoint-id>")
			return false
		}
		id, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Printf("  invalid breakpoint id: %s\n", args[1])
			return false
		}
		h.clearBreakpoint(id)

	case "locals":
		frame := 0
		if len(args) > 1 {
			frame, _ = strconv.Atoi(args[1])
		}
		h.showLocals(frame)

	case "bt", "backtrace":
		h.showStackTrace()

	case "threads", "goroutines", "grs":
		h.showThreads()

	case "help", "h", "?":
		printHelp()

	case "quit", "q", "exit":
		fmt.Println("bye")
		return true

	default:
		fmt.Printf("  unknown command: %s (type 'help' for usage)\n", cmd)
	}
	return false
}

// --- request/response plumbing -------------------------------------------------

// readLoop demuxes responses (to waiters, by request seq) and events (printed /
// handshake-driven) until the connection closes.
func (h *dapCLI) readLoop() {
	for {
		msg, err := godap.ReadProtocolMessage(h.reader)
		if err != nil {
			h.failPending()
			if !isClosed(err) {
				h.printAsync("connection closed: " + err.Error())
			} else {
				h.printAsync("disconnected")
			}
			return
		}
		switch m := msg.(type) {
		case godap.ResponseMessage:
			h.onResponse(m, msg)
		case godap.EventMessage:
			h.onEvent(m)
		}
	}
}

func (h *dapCLI) onResponse(rm godap.ResponseMessage, msg godap.Message) {
	rs := rm.GetResponse()
	h.mu.Lock()
	ch := h.pending[rs.RequestSeq]
	h.mu.Unlock()
	if ch != nil {
		ch <- msg
		return
	}
	// No waiter (a fire-and-forget command): surface failures so they aren't
	// silently dropped; ignore successes (continue/step/launch acks etc.).
	if !rs.Success {
		h.printAsync(fmt.Sprintf("[error] %s: %s", rs.Command, rs.Message))
	}
}

// fire sends a request without registering a waiter (for control commands whose
// ack we don't surface). Errors still surface via onResponse.
func (h *dapCLI) fire(command string, m godap.RequestMessage) {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	h.mu.Unlock()
	h.write(command, seq, m)
}

// request sends a request and blocks for its response (or a timeout).
func (h *dapCLI) request(command string, m godap.RequestMessage) (godap.Message, error) {
	h.mu.Lock()
	h.seq++
	seq := h.seq
	ch := make(chan godap.Message, 1)
	h.pending[seq] = ch
	h.mu.Unlock()

	h.write(command, seq, m)

	select {
	case msg := <-ch:
		h.mu.Lock()
		delete(h.pending, seq)
		h.mu.Unlock()
		if rm, ok := msg.(godap.ResponseMessage); ok {
			if rs := rm.GetResponse(); !rs.Success {
				return msg, fmt.Errorf("%s", rs.Message)
			}
		}
		return msg, nil
	case <-time.After(replyTimeout):
		h.mu.Lock()
		delete(h.pending, seq)
		h.mu.Unlock()
		return nil, fmt.Errorf("timeout awaiting %s response", command)
	}
}

func (h *dapCLI) write(command string, seq int, m godap.RequestMessage) {
	req := m.GetRequest()
	req.Seq = seq
	req.Type = "request"
	req.Command = command
	h.writeMu.Lock()
	err := godap.WriteProtocolMessage(h.conn, m)
	h.writeMu.Unlock()
	if err != nil {
		h.printAsync(fmt.Sprintf("[error] write %s: %v", command, err))
	}
}

// failPending unblocks every outstanding waiter when the connection drops.
func (h *dapCLI) failPending() {
	h.mu.Lock()
	for seq, ch := range h.pending {
		close(ch)
		delete(h.pending, seq)
	}
	h.mu.Unlock()
}

func (h *dapCLI) close() { _ = h.conn.Close() }

// --- event handling ------------------------------------------------------------

func (h *dapCLI) onEvent(em godap.EventMessage) {
	switch e := em.(type) {
	case *godap.InitializedEvent:
		h.printAsync("[initialized] session ready")
		go h.onInitialized()
	case *godap.StoppedEvent:
		h.setThread(e.Body.ThreadId)
		desc := e.Body.Reason
		if e.Body.Description != "" {
			desc += " (" + e.Body.Description + ")"
		}
		h.printAsync(fmt.Sprintf("[stopped] %s  thread=%d", desc, e.Body.ThreadId))
	case *godap.ContinuedEvent:
		h.printAsync("[continued]")
	case *godap.OutputEvent:
		h.captureSession(e.Body.Output)
		cat := e.Body.Category
		if cat == "" {
			cat = "output"
		}
		h.printAsync(fmt.Sprintf("[%s] %s", cat, strings.TrimRight(e.Body.Output, "\n")))
	case *godap.ExitedEvent:
		h.printAsync(fmt.Sprintf("[exited] code=%d", e.Body.ExitCode))
	case *godap.TerminatedEvent:
		h.printAsync("[terminated]")
	case *godap.BreakpointEvent:
		h.printAsync(fmt.Sprintf("[breakpoint] %s id=%d", e.Body.Reason, e.Body.Breakpoint.Id))
	case *godap.ThreadEvent:
		// Noise for an interactive CLI; ignore.
	default:
		h.printAsync("[" + em.GetEvent().Event + "]")
	}
}

// onInitialized runs the standard client response to the `initialized` event:
// flush every buffered breakpoint, then configurationDone. It runs in its own
// goroutine so its blocking setBreakpoints awaits are served by readLoop.
func (h *dapCLI) onInitialized() {
	h.stateMu.Lock()
	h.configured = true
	files := make([]string, 0, len(h.bpsByFile))
	for f := range h.bpsByFile {
		files = append(files, f)
	}
	h.stateMu.Unlock()

	for _, f := range files {
		h.sendBreakpoints(f, true)
	}
	h.fire("configurationDone", &godap.ConfigurationDoneRequest{})
}

func (h *dapCLI) captureSession(output string) {
	// The adapter announces: "bingo session <id> ready — observers can join...".
	const marker = "bingo session "
	i := strings.Index(output, marker)
	if i < 0 {
		return
	}
	rest := output[i+len(marker):]
	if j := strings.IndexByte(rest, ' '); j > 0 {
		h.setSessionID(rest[:j])
	}
}

// --- breakpoints ---------------------------------------------------------------

func (h *dapCLI) addBreakpoint(file string, line int) {
	h.stateMu.Lock()
	for _, bp := range h.bpsByFile[file] {
		if bp.line == line {
			h.stateMu.Unlock()
			fmt.Printf("  breakpoint already set at %s:%d\n", file, line)
			return
		}
	}
	h.bpsByFile[file] = append(h.bpsByFile[file], breakpoint{line: line})
	configured := h.configured
	h.stateMu.Unlock()

	if !configured {
		fmt.Printf("  breakpoint buffered at %s:%d (installed on launch)\n", file, line)
		return
	}
	h.sendBreakpoints(file, true)
}

func (h *dapCLI) clearBreakpoint(id int) {
	h.stateMu.Lock()
	var file string
	found := false
	for f, bps := range h.bpsByFile {
		for i, bp := range bps {
			if bp.id == id {
				h.bpsByFile[f] = append(bps[:i:i], bps[i+1:]...)
				file, found = f, true
				break
			}
		}
		if found {
			break
		}
	}
	configured := h.configured
	h.stateMu.Unlock()

	if !found {
		fmt.Printf("  no breakpoint with id %d\n", id)
		return
	}
	if configured {
		h.sendBreakpoints(file, false)
	}
	fmt.Printf("  breakpoint %d cleared\n", id)
}

// sendBreakpoints replaces the adapter's breakpoints for file with the currently
// tracked set (DAP setBreakpoints is replace-all per source), then records the
// returned ids. announce prints each resulting breakpoint.
func (h *dapCLI) sendBreakpoints(file string, announce bool) {
	h.stateMu.Lock()
	bps := h.bpsByFile[file]
	sbps := make([]godap.SourceBreakpoint, len(bps))
	for i, bp := range bps {
		sbps[i] = godap.SourceBreakpoint{Line: bp.line}
	}
	h.stateMu.Unlock()

	msg, err := h.request("setBreakpoints", &godap.SetBreakpointsRequest{
		Arguments: godap.SetBreakpointsArguments{
			Source:      godap.Source{Path: file, Name: file},
			Breakpoints: sbps,
		},
	})
	if err != nil {
		h.printAsync("[error] setBreakpoints: " + err.Error())
		return
	}
	resp, ok := msg.(*godap.SetBreakpointsResponse)
	if !ok {
		return
	}

	h.stateMu.Lock()
	cur := h.bpsByFile[file]
	for i := range resp.Body.Breakpoints {
		if i < len(cur) {
			cur[i].id = resp.Body.Breakpoints[i].Id
			cur[i].verified = resp.Body.Breakpoints[i].Verified
		}
	}
	h.stateMu.Unlock()

	if !announce {
		return
	}
	for _, bp := range resp.Body.Breakpoints {
		if bp.Verified {
			h.printAsync(fmt.Sprintf("[breakpoint] %d set at %s:%d", bp.Id, file, bp.Line))
		} else {
			h.printAsync(fmt.Sprintf("[breakpoint] unresolved at %s:%d: %s", file, bp.Line, bp.Message))
		}
	}
}

// --- data requests -------------------------------------------------------------

func (h *dapCLI) showThreads() {
	msg, err := h.request("threads", &godap.ThreadsRequest{})
	if err != nil {
		printErr(err)
		return
	}
	resp, ok := msg.(*godap.ThreadsResponse)
	if !ok || len(resp.Body.Threads) == 0 {
		fmt.Println("  (no threads)")
		return
	}
	for _, t := range resp.Body.Threads {
		fmt.Printf("  thread %-4d %s\n", t.Id, t.Name)
	}
}

func (h *dapCLI) showStackTrace() {
	msg, err := h.request("stackTrace", &godap.StackTraceRequest{
		Arguments: godap.StackTraceArguments{ThreadId: h.thread()},
	})
	if err != nil {
		printErr(err)
		return
	}
	resp, ok := msg.(*godap.StackTraceResponse)
	if !ok || len(resp.Body.StackFrames) == 0 {
		fmt.Println("  (no frames)")
		return
	}
	for _, f := range resp.Body.StackFrames {
		src := ""
		if f.Source != nil {
			src = f.Source.Name
		}
		fmt.Printf("  #%d  %s at %s:%d\n", f.Id-1, f.Name, src, f.Line)
	}
}

func (h *dapCLI) showLocals(frame int) {
	// variablesReference == frameIndex+1 (the adapter decodes it back).
	msg, err := h.request("variables", &godap.VariablesRequest{
		Arguments: godap.VariablesArguments{VariablesReference: frame + 1},
	})
	if err != nil {
		printErr(err)
		return
	}
	resp, ok := msg.(*godap.VariablesResponse)
	if !ok || len(resp.Body.Variables) == 0 {
		fmt.Println("  (no locals)")
		return
	}
	for _, v := range resp.Body.Variables {
		fmt.Printf("  %s %s = %s\n", v.Name, v.Type, v.Value)
	}
}

// --- small helpers -------------------------------------------------------------

func (h *dapCLI) thread() int {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	return h.curThread
}

func (h *dapCLI) setThread(tid int) {
	if tid == 0 {
		return
	}
	h.stateMu.Lock()
	h.curThread = tid
	h.stateMu.Unlock()
}

func (h *dapCLI) setSessionID(id string) {
	h.stateMu.Lock()
	h.sessionID = id
	h.stateMu.Unlock()
}

// printAsync writes an out-of-band line (event / async result) and redraws the
// prompt, mirroring cmd/cli's event printer.
func (h *dapCLI) printAsync(msg string) {
	h.printMu.Lock()
	fmt.Printf("\n  %s\n%s", msg, prompt)
	h.printMu.Unlock()
}

func parseFileLine(s string) (string, int, bool) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 || idx == len(s)-1 {
		return "", 0, false
	}
	line, err := strconv.Atoi(s[idx+1:])
	if err != nil || line <= 0 {
		return "", 0, false
	}
	return s[:idx], line, true
}

func isClosed(err error) bool {
	return err == io.EOF || errStringContains(err, "closed")
}

func errStringContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}

func printErr(err error) { fmt.Printf("  error: %v\n", err) }

func printHelp() {
	fmt.Println(`commands (Debug Adapter Protocol):
  state                      show session id / handshake / current thread

  launch <binary> [args...]  start a process under the debugger (stops at entry)
  attach <pid> [binary]      attach to a running process (stops on attach)
  kill                       terminate the debuggee
  restart                    kill and relaunch, reinstalling breakpoints

  c / continue               resume execution
  n / next                   step over
  s / step                   step into
  out / finish               step out (run until function returns)
  p / pause                  interrupt a running process and suspend it

  b / break <file>:<line>    set breakpoint (buffered before launch, e.g. main.go:42)
  clear <id>                 remove breakpoint by ID

  locals [frame]             show local variables (default frame 0)
  bt / backtrace             show call stack
  threads / goroutines       list goroutines

  help / h / ?               show this help
  quit / q / exit            disconnect and exit

Join an existing session from another terminal:
  dapcli -session <id>       (id is printed on launch; also via cmd/cli's 'sessions')`)
}
