// Command cli is an interactive terminal client for the bingo debug server.
//
//	cli [-addr host:port] [-session id]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/bingosuite/bingo/cmd/internal/repl"
	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
	"github.com/chzyer/readline"
)

// fullSnapshotNext renders the next EventGoroutineSnapshot in full rather than
// as the usual one-line summary, so a user-typed `snapshot` gets the detailed
// view. It is a display preference, not correlation: the arm is consumed by
// whichever snapshot arrives first and is cleared by any broadcast snapshot
// error. Shared between the REPL goroutine and the event printer.
var fullSnapshotNext atomic.Bool

func main() {
	os.Exit(mainExitCode())
}

func mainExitCode() int {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	addr := flag.String("addr", "localhost:6060", "server address (host:port)")
	sessionID := flag.String("session", "", "session ID to join (omit to create)")
	flag.Parse()

	var c client.Client
	var err error

	if *sessionID != "" {
		fmt.Printf("joining session %s on %s...\n", *sessionID, *addr)
		c, err = client.JoinContext(ctx, *addr, *sessionID)
	} else {
		fmt.Printf("creating new session on %s...\n", *addr)
		c, err = client.CreateContext(ctx, *addr)
	}
	if err != nil {
		code := exitCode(err)
		if code != 0 {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		return code
	}
	if ctx.Err() != nil {
		_ = c.Close()
		return 0
	}

	fmt.Printf("connected — session %s (state: %s)\n\n", c.SessionID(), c.State())

	rl, err := repl.NewEditor(&readline.Config{
		Prompt:          "bingo> ",
		HistoryFile:     os.ExpandEnv("$HOME/.bingo_history"),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error initializing readline: %v\n", err)
		_ = c.Close()
		return 1
	}

	printHelp()
	runInteractive(ctx, rl, c.Events(), c.Close, func(ctx context.Context, args []string) bool {
		return dispatch(ctx, c, *addr, args)
	})
	return 0
}

type lineEditor interface {
	repl.Reader
	Close() error
}

func runInteractive(
	ctx context.Context,
	editor lineEditor,
	events <-chan protocol.Event,
	closeTransport func() error,
	dispatchCommand func(context.Context, []string) bool,
) {
	runCtx, cancel := context.WithCancel(ctx)

	var closeEditorOnce sync.Once
	closeEditor := func() {
		closeEditorOnce.Do(func() { _ = editor.Close() })
	}
	var closeTransportOnce sync.Once
	closeClient := func() {
		closeTransportOnce.Do(func() { _ = closeTransport() })
	}
	shutdown := func() {
		closeClient()
		closeEditor()
	}
	stopShutdown := context.AfterFunc(runCtx, shutdown)

	disconnected := make(chan struct{})
	eventDone := make(chan struct{})
	go func() {
		defer close(eventDone)
		if eventPrinter(runCtx, events, editor.Stdout()) {
			close(disconnected)
			cancel()
			closeEditor()
		}
	}()

	repl.Loop(runCtx, editor, closeEditor, disconnected, func(args []string) bool {
		return dispatchCommand(runCtx, args)
	})

	cancel()
	shutdown()
	stopShutdown()
	<-eventDone
}

//nolint:gocognit,gocyclo // The CLI keeps command routing in one switch while commands are still small.
func dispatch(ctx context.Context, c client.Client, addr string, args []string) bool {
	switch cmd := args[0]; cmd {
	case "sessions", "ls":
		sessions, err := client.ListSessionsContext(ctx, addr)
		if err != nil {
			printErrUnlessCanceled(ctx, err)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		if len(sessions) == 0 {
			fmt.Println("  (no active sessions)")
			return false
		}
		for _, s := range sessions {
			fmt.Printf("  %s  state=%-10s clients=%d  created=%s\n",
				s.ID, s.State, s.Clients, s.CreatedAt.Format("15:04:05"))
		}

	case "state":
		fmt.Printf("  session=%s  state=%s\n", c.SessionID(), c.State())

	case "launch":
		if len(args) < 2 {
			fmt.Println("  usage: launch <binary> [args...]")
			return false
		}
		var launchArgs []string
		if len(args) > 2 {
			launchArgs = args[2:]
		}
		if err := c.Launch(args[1], launchArgs, nil); err != nil {
			printErrUnlessCanceled(ctx, err)
		}

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
		var binPath string
		if len(args) > 2 {
			binPath = args[2]
		}
		if err := c.Attach(pid, binPath); err != nil {
			printErrUnlessCanceled(ctx, err)
		}

	case "kill":
		if err := c.Kill(); err != nil {
			printErrUnlessCanceled(ctx, err)
		}

	case "restart":
		p, err := c.Restart(nil, nil)
		if err != nil {
			printErrUnlessCanceled(ctx, err)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		fmt.Printf("  restarted %s\n", p.Program)
		for _, bp := range p.Breakpoints {
			fmt.Printf("  breakpoint %d reinstalled at %s:%d\n", bp.ID, bp.Location.File, bp.Location.Line)
		}
		for _, d := range p.Discarded {
			fmt.Printf("  breakpoint at %s:%d discarded: %s\n", d.Location.File, d.Location.Line, d.Reason)
		}

	case "c", "continue":
		if err := c.Continue(); err != nil {
			printErrUnlessCanceled(ctx, err)
		}

	case "n", "next":
		if err := c.StepOver(); err != nil {
			printErrUnlessCanceled(ctx, err)
		}

	case "s", "step":
		if err := c.StepInto(); err != nil {
			printErrUnlessCanceled(ctx, err)
		}

	case "out", "finish":
		if err := c.StepOut(); err != nil {
			printErrUnlessCanceled(ctx, err)
		}

	case "p", "pause":
		if err := c.Pause(); err != nil {
			printErrUnlessCanceled(ctx, err)
		}

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
		bp, err := c.SetBreakpoint(file, line)
		if err != nil {
			printErrUnlessCanceled(ctx, err)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		fmt.Printf("  breakpoint %d set at %s:%d\n",
			bp.ID, bp.Location.File, bp.Location.Line)

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
		if err := c.ClearBreakpoint(id); err != nil {
			printErrUnlessCanceled(ctx, err)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		fmt.Printf("  breakpoint %d cleared\n", id)

	case "locals":
		showLocals(ctx, c, args[1:], os.Stdout)

	case "bt", "backtrace":
		frames, err := c.StackFrames()
		if err != nil {
			printErrUnlessCanceled(ctx, err)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		for _, f := range frames {
			fmt.Printf("  #%d  %s at %s:%d\n",
				f.Index, f.Location.Function, f.Location.File, f.Location.Line)
		}

	case "goroutines", "grs":
		grs, err := c.GoroutineList()
		if err != nil {
			printErrUnlessCanceled(ctx, err)
			return false
		}
		if ctx.Err() != nil {
			return false
		}
		fmt.Print(formatGoroutineList(grs))

	case "snapshot", "snap":
		// This is only a display arm: snapshot events are broadcasts and carry
		// no requester identity, so whichever snapshot arrives next consumes it.
		fullSnapshotNext.Store(true)
		if err := c.RequestGoroutineSnapshot(); err != nil {
			fullSnapshotNext.Store(false)
			printErrUnlessCanceled(ctx, err)
			return false
		}

	case "help", "h", "?":
		printHelp()

	case "quit", "q", "exit":
		return true

	default:
		fmt.Printf("  unknown command: %s (type 'help' for usage)\n", cmd)
	}
	return false
}

type localsClient interface {
	Locals(frameIndex int) ([]protocol.Variable, error)
}

func showLocals(ctx context.Context, c localsClient, args []string, out io.Writer) {
	frame, err := repl.FrameIndex(args)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  %s\n", err)
		return
	}
	vars, err := c.Locals(frame)
	if err != nil {
		if !commandErrorSuppressed(ctx, err) {
			_, _ = fmt.Fprintf(out, "  error: %v\n", err)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	if len(vars) == 0 {
		_, _ = fmt.Fprintln(out, "  (no locals)")
		return
	}
	for _, v := range vars {
		_, _ = fmt.Fprintf(out, "  %s %s = %s\n", v.Name, v.Type, v.Value)
	}
}

func eventPrinter(ctx context.Context, events <-chan protocol.Event, out io.Writer) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case evt, ok := <-events:
			if !ok {
				if ctx.Err() != nil {
					return false
				}
				repl.PrintAsync(out, "disconnected")
				return true
			}
			printEvent(out, evt)
		}
	}
}

func printEvent(out io.Writer, evt protocol.Event) {
	switch evt.Kind {

	case protocol.EventSessionState:
		var p protocol.SessionStatePayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			repl.PrintAsync(out, fmt.Sprintf("[state] %s (clients: %d)", p.State, p.Clients))
		}

	case protocol.EventBreakpointHit:
		var p protocol.BreakpointHitPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			repl.PrintAsync(out, fmt.Sprintf("[hit] breakpoint %d at %s:%d",
				p.Breakpoint.ID, p.Breakpoint.Location.File, p.Breakpoint.Location.Line))
		}

	case protocol.EventPanic:
		var p protocol.PanicPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			repl.PrintAsync(out, "[panic] "+p.Message)
		}

	case protocol.EventOutput:
		var p protocol.OutputPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			repl.PrintOutput(out, p.Stream, p.Content)
		}

	case protocol.EventProcessExited:
		var p protocol.ProcessExitedPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			repl.PrintAsync(out, fmt.Sprintf("[exited] code=%d reason=%s", p.ExitCode, p.Reason))
		}

	case protocol.EventStepped:
		var p protocol.SteppedPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			repl.PrintAsync(out, fmt.Sprintf("[stepped] %s:%d in %s",
				p.Location.File, p.Location.Line, p.Location.Function))
		}

	case protocol.EventPaused:
		var p protocol.PausedPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			repl.PrintAsync(out, fmt.Sprintf("[paused] %s:%d in %s",
				p.Location.File, p.Location.Line, p.Location.Function))
		}

	default:
		printAuxEvent(out, evt)
	}
}

// printAuxEvent renders the non-stop events. Split out of printEvent so neither
// switch trips the cyclomatic-complexity linter as event kinds grow.
func printAuxEvent(out io.Writer, evt protocol.Event) {
	switch evt.Kind {

	case protocol.EventContinued:
		repl.PrintAsync(out, "[continued]")

	case protocol.EventError:
		var p protocol.ErrorPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			if p.Command == protocol.CmdGoroutineSnapshot {
				// Disarm on any broadcast snapshot rejection, including one
				// caused by another client: EventError carries no requester, so
				// leaving the arm set would expand an unrelated automatic push.
				fullSnapshotNext.Store(false)
			}
			repl.PrintAsync(out, fmt.Sprintf("[error] %s: %s", p.Command, p.Message))
		}

	case protocol.EventRestarted:
		var p protocol.RestartedPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			repl.PrintAsync(out, fmt.Sprintf("[restarted] %s (%d breakpoint(s), %d discarded)",
				p.Program, len(p.Breakpoints), len(p.Discarded)))
		}

	case protocol.EventGoroutineSnapshot:
		var p protocol.GoroutineSnapshotPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			if fullSnapshotNext.CompareAndSwap(true, false) {
				printSnapshot(out, p)
				return
			}
			msg := fmt.Sprintf("[goroutines] %s, %s threads, current G%d",
				countOf(len(p.Goroutines), p.Totals, totalGoroutines),
				countOf(len(p.Threads), p.Totals, totalThreads), p.Current)
			if len(p.Created) > 0 {
				msg += fmt.Sprintf(", +%v", p.Created)
			}
			if len(p.Exited) > 0 {
				msg += fmt.Sprintf(", -%v", p.Exited)
			}
			repl.PrintAsync(out, msg)
		}

	default:
		repl.PrintAsync(out, fmt.Sprintf("[%s] seq=%d", evt.Kind, evt.Seq))
	}
}

// printGoroutine renders one goroutine line, marking the current one and
// showing parent linkage and its start function so a spawn tree is visible.
// formatGoroutineList renders the listing plus what it does not show. Without
// the trailing count the listing itself is the claim: the reader sees the
// delivered set with no way to tell the whole runtime from a packed subset or a
// scan that stopped early. Returns a string so the honesty line is testable.
func formatGoroutineList(list protocol.GoroutinesPayload) string {
	var b strings.Builder
	for _, g := range list.Goroutines {
		b.WriteString(formatGoroutine(g) + "\n")
	}
	fmt.Fprintf(&b, "  (%s)\n",
		countOf(len(list.Goroutines), list.Totals, totalGoroutines))
	return b.String()
}

func printGoroutine(g protocol.Goroutine) {
	fmt.Println(formatGoroutine(g))
}

func formatGoroutine(g protocol.Goroutine) string {
	marker := " "
	if g.Current {
		marker = "*"
	}
	loc := fmt.Sprintf("%s:%d", g.CurrentLoc.File, g.CurrentLoc.Line)
	line := fmt.Sprintf("%s G%-4d %-10s %s", marker, g.ID, g.Status, loc)
	if g.ParentID != 0 {
		line += fmt.Sprintf("  <-G%d", g.ParentID)
	}
	if g.StartLoc.Function != "" {
		line += fmt.Sprintf("  start=%s", g.StartLoc.Function)
	}
	if g.WaitReason != "" {
		line += fmt.Sprintf("  (%s)", g.WaitReason)
	}
	return line
}

// printSnapshot renders a full concurrency snapshot: goroutines (with spawn
// linkage), OS threads, and the created/exited lifecycle deltas.
func printSnapshot(out io.Writer, snap protocol.GoroutineSnapshotPayload) {
	_, _ = fmt.Fprintf(out, "  goroutines: %s  threads: %s  current: G%d\n",
		countOf(len(snap.Goroutines), snap.Totals, totalGoroutines),
		countOf(len(snap.Threads), snap.Totals, totalThreads), snap.Current)
	for _, g := range snap.Goroutines {
		_, _ = fmt.Fprintln(out, formatGoroutine(g))
	}
	for _, t := range snap.Threads {
		marker := " "
		if t.Current {
			marker = "*"
		}
		spin := ""
		if t.Spinning {
			spin = " spinning"
		}
		_, _ = fmt.Fprintf(out, "%s M%-4d tid=%-6d G%d%s\n", marker, t.MID, t.ID, t.GoID, spin)
	}
	if len(snap.Created) > 0 {
		_, _ = fmt.Fprintf(out, "  created: %v\n", snap.Created)
	}
	if len(snap.Exited) > 0 {
		_, _ = fmt.Fprintf(out, "  exited:  %v\n", snap.Exited)
	}
}

// which collection countOf is describing; the two have independent scan
// ceilings so they cannot share a lower-bound marker.
type collection int

const (
	totalGoroutines collection = iota
	totalThreads
)

// countOf renders a delivered count against what the debugger actually had.
// Printing the delivered length alone would state a truncated event as the live
// truth — the totals exist precisely so a client does not have to.
func countOf(shown int, totals *protocol.SnapshotTotals, which collection) string {
	if totals == nil {
		return fmt.Sprintf("%d live", shown)
	}
	total, clipped := totals.Goroutines, totals.GoroutinesClipped
	if which == totalThreads {
		total, clipped = totals.Threads, totals.ThreadsClipped
	}
	bound := ""
	if clipped {
		bound = "+" // the scan stopped early, so the total is a floor
	}
	if shown >= total && bound == "" {
		return fmt.Sprintf("%d live", shown)
	}
	return fmt.Sprintf("%d of %d%s", shown, total, bound)
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

func printErr(err error) {
	fmt.Printf("  error: %v\n", err)
}

func printErrUnlessCanceled(ctx context.Context, err error) {
	if !commandErrorSuppressed(ctx, err) {
		printErr(err)
	}
}

func commandErrorSuppressed(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, client.ErrClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE)
}

func exitCode(err error) int {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	return 1
}

func printHelp() {
	fmt.Println(`commands:
  sessions / ls              list active sessions on the server
  state                      show current session state

  launch <binary> [args...]  start a process under the debugger
  attach <pid> [binary]      attach to a running process  (find pid: pgrep <name>)
  kill                       terminate the debuggee
  restart                    kill and relaunch, reinstalling breakpoints

  c / continue               resume execution
  n / next                   step over
  s / step                   step into
  out / finish               step out (run until function returns)
  p / pause                  interrupt a running process and suspend it

  b / break <file>:<line>    set breakpoint  (e.g. break main.go:42)
  clear <id>                 remove breakpoint by ID

  locals [frame]             show local variables (default frame 0)
  bt / backtrace             show call stack
  goroutines / grs           list goroutines (with parent/start)
  snapshot / snap            request a full concurrency snapshot (printed when it arrives)

  help / h / ?               show this help
  quit / q / exit            disconnect and exit`)
}
