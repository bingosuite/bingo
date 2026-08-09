// Command cli is an interactive terminal client for the bingo debug server.
//
//	cli [-addr host:port] [-session id]
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

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

//nolint:gocognit,gocyclo // The CLI keeps command routing in one switch while commands are still small.
func main() {
	addr := flag.String("addr", "localhost:6060", "server address (host:port)")
	sessionID := flag.String("session", "", "session ID to join (omit to create)")
	flag.Parse()

	var c client.Client
	var err error

	if *sessionID != "" {
		fmt.Printf("joining session %s on %s...\n", *sessionID, *addr)
		c, err = client.Join(*addr, *sessionID)
	} else {
		fmt.Printf("creating new session on %s...\n", *addr)
		c, err = client.Create(*addr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = c.Close() }()

	fmt.Printf("connected — session %s (state: %s)\n\n", c.SessionID(), c.State())

	go eventPrinter(c.Events())

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "bingo> ",
		HistoryFile:     os.ExpandEnv("$HOME/.bingo_history"),
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
				return
			}
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		args := strings.Fields(line)
		cmd := args[0]

		switch cmd {

		case "sessions", "ls":
			sessions, err := client.ListSessions(*addr)
			if err != nil {
				printErr(err)
				continue
			}
			if len(sessions) == 0 {
				fmt.Println("  (no active sessions)")
				continue
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
				continue
			}
			var launchArgs []string
			if len(args) > 2 {
				launchArgs = args[2:]
			}
			if err := c.Launch(args[1], launchArgs, nil); err != nil {
				printErr(err)
			}

		case "attach":
			if len(args) < 2 {
				fmt.Println("  usage: attach <pid> [binary-path]")
				continue
			}
			pid, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Printf("  invalid pid: %s\n", args[1])
				continue
			}
			var binPath string
			if len(args) > 2 {
				binPath = args[2]
			}
			if err := c.Attach(pid, binPath); err != nil {
				printErr(err)
			}

		case "kill":
			if err := c.Kill(); err != nil {
				printErr(err)
			}

		case "restart":
			p, err := c.Restart(nil, nil)
			if err != nil {
				printErr(err)
				continue
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
				printErr(err)
			}

		case "n", "next":
			if err := c.StepOver(); err != nil {
				printErr(err)
			}

		case "s", "step":
			if err := c.StepInto(); err != nil {
				printErr(err)
			}

		case "out", "finish":
			if err := c.StepOut(); err != nil {
				printErr(err)
			}

		case "p", "pause":
			if err := c.Pause(); err != nil {
				printErr(err)
			}

		case "b", "break":
			if len(args) < 2 {
				fmt.Println("  usage: break <file>:<line>")
				continue
			}
			file, line, ok := parseFileLine(args[1])
			if !ok {
				fmt.Println("  usage: break <file>:<line>  (e.g. main.go:42)")
				continue
			}
			bp, err := c.SetBreakpoint(file, line)
			if err != nil {
				printErr(err)
				continue
			}
			fmt.Printf("  breakpoint %d set at %s:%d\n",
				bp.ID, bp.Location.File, bp.Location.Line)

		case "clear":
			if len(args) < 2 {
				fmt.Println("  usage: clear <breakpoint-id>")
				continue
			}
			id, err := strconv.Atoi(args[1])
			if err != nil {
				fmt.Printf("  invalid breakpoint id: %s\n", args[1])
				continue
			}
			if err := c.ClearBreakpoint(id); err != nil {
				printErr(err)
				continue
			}
			fmt.Printf("  breakpoint %d cleared\n", id)

		case "locals":
			frame := 0
			if len(args) > 1 {
				frame, _ = strconv.Atoi(args[1])
			}
			vars, err := c.Locals(frame)
			if err != nil {
				printErr(err)
				continue
			}
			if len(vars) == 0 {
				fmt.Println("  (no locals)")
				continue
			}
			for _, v := range vars {
				fmt.Printf("  %s %s = %s\n", v.Name, v.Type, v.Value)
			}

		case "bt", "backtrace":
			frames, err := c.StackFrames()
			if err != nil {
				printErr(err)
				continue
			}
			for _, f := range frames {
				fmt.Printf("  #%d  %s at %s:%d\n",
					f.Index, f.Location.Function, f.Location.File, f.Location.Line)
			}

		case "goroutines", "grs":
			grs, err := c.GoroutineList()
			if err != nil {
				printErr(err)
				continue
			}
			fmt.Print(formatGoroutineList(grs))

		case "snapshot", "snap":
			// Fire-and-forget: the snapshot answers on the event stream, where
			// automatic stop snapshots also arrive. fullSnapshotNext is a
			// display arm only — it selects the renderer for the NEXT snapshot,
			// whichever that turns out to be. An automatic push (or another
			// client's requested one) can consume the arm, and any broadcast
			// snapshot error can disarm it, so the detailed view may land on a
			// different snapshot than the one this command asked for. No
			// snapshot data is lost either way: every one is printed, in order,
			// in one form or the other.
			fullSnapshotNext.Store(true)
			if err := c.RequestGoroutineSnapshot(); err != nil {
				fullSnapshotNext.Store(false)
				printErr(err)
				continue
			}

		case "help", "h", "?":
			printHelp()

		case "quit", "q", "exit":
			fmt.Println("bye")
			return

		default:
			fmt.Printf("  unknown command: %s (type 'help' for usage)\n", cmd)
		}
	}
}

func eventPrinter(events <-chan protocol.Event) {
	for evt := range events {
		printEvent(evt)
	}
}

func printEvent(evt protocol.Event) {
	switch evt.Kind {

	case protocol.EventSessionState:
		var p protocol.SessionStatePayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			fmt.Printf("\n  [state] %s (clients: %d)\nbingo> ", p.State, p.Clients)
		}

	case protocol.EventBreakpointHit:
		var p protocol.BreakpointHitPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			fmt.Printf("\n  [hit] breakpoint %d at %s:%d\nbingo> ",
				p.Breakpoint.ID, p.Breakpoint.Location.File, p.Breakpoint.Location.Line)
		}

	case protocol.EventPanic:
		var p protocol.PanicPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			fmt.Printf("\n  [panic] %s\nbingo> ", p.Message)
		}

	case protocol.EventOutput:
		var p protocol.OutputPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			fmt.Printf("\n  [%s] %s\nbingo> ", p.Stream, p.Content)
		}

	case protocol.EventProcessExited:
		var p protocol.ProcessExitedPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			fmt.Printf("\n  [exited] code=%d reason=%s\nbingo> ", p.ExitCode, p.Reason)
		}

	case protocol.EventStepped:
		var p protocol.SteppedPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			fmt.Printf("\n  [stepped] %s:%d in %s\nbingo> ",
				p.Location.File, p.Location.Line, p.Location.Function)
		}

	case protocol.EventPaused:
		var p protocol.PausedPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			fmt.Printf("\n  [paused] %s:%d in %s\nbingo> ",
				p.Location.File, p.Location.Line, p.Location.Function)
		}

	default:
		printAuxEvent(evt)
	}
}

// printAuxEvent renders the non-stop events. Split out of printEvent so neither
// switch trips the cyclomatic-complexity linter as event kinds grow.
func printAuxEvent(evt protocol.Event) {
	switch evt.Kind {

	case protocol.EventContinued:
		fmt.Print("\n  [continued]\nbingo> ")

	case protocol.EventError:
		var p protocol.ErrorPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			if p.Command == protocol.CmdGoroutineSnapshot {
				// Disarm on any broadcast snapshot rejection, including one
				// caused by another client: EventError carries no requester, so
				// leaving the arm set would expand an unrelated automatic push.
				fullSnapshotNext.Store(false)
			}
			fmt.Printf("\n  [error] %s: %s\nbingo> ", p.Command, p.Message)
		}

	case protocol.EventRestarted:
		var p protocol.RestartedPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			fmt.Printf("\n  [restarted] %s (%d breakpoint(s), %d discarded)\nbingo> ",
				p.Program, len(p.Breakpoints), len(p.Discarded))
		}

	case protocol.EventGoroutineSnapshot:
		var p protocol.GoroutineSnapshotPayload
		if protocol.DecodeEventPayload(evt, &p) == nil {
			if fullSnapshotNext.CompareAndSwap(true, false) {
				fmt.Println()
				printSnapshot(p)
				fmt.Print("bingo> ")
				return
			}
			msg := fmt.Sprintf("\n  [goroutines] %s, %s threads, current G%d",
				countOf(len(p.Goroutines), p.Totals, totalGoroutines),
				countOf(len(p.Threads), p.Totals, totalThreads), p.Current)
			if len(p.Created) > 0 {
				msg += fmt.Sprintf(", +%v", p.Created)
			}
			if len(p.Exited) > 0 {
				msg += fmt.Sprintf(", -%v", p.Exited)
			}
			fmt.Print(msg + "\nbingo> ")
		}

	default:
		fmt.Printf("\n  [%s] seq=%d\nbingo> ", evt.Kind, evt.Seq)
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
func printSnapshot(snap protocol.GoroutineSnapshotPayload) {
	fmt.Printf("  goroutines: %s  threads: %s  current: G%d\n",
		countOf(len(snap.Goroutines), snap.Totals, totalGoroutines),
		countOf(len(snap.Threads), snap.Totals, totalThreads), snap.Current)
	for _, g := range snap.Goroutines {
		printGoroutine(g)
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
		fmt.Printf("%s M%-4d tid=%-6d G%d%s\n", marker, t.MID, t.ID, t.GoID, spin)
	}
	if len(snap.Created) > 0 {
		fmt.Printf("  created: %v\n", snap.Created)
	}
	if len(snap.Exited) > 0 {
		fmt.Printf("  exited:  %v\n", snap.Exited)
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
