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

	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
	"github.com/chzyer/readline"
)

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
			grs, err := c.Goroutines()
			if err != nil {
				printErr(err)
				continue
			}
			for _, g := range grs.Goroutines {
				printGoroutine(g)
			}
			printTotals(len(grs.Goroutines), 0, grs.Totals)

		case "snapshot", "snap":
			snap, err := c.GoroutineSnapshot()
			if err != nil {
				printErr(err)
				continue
			}
			printSnapshot(snap)

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
			msg := fmt.Sprintf("\n  [goroutines] %s, %s, current G%d",
				countOf(len(p.Goroutines), "live goroutine", totalsGoroutines(p.Totals)),
				countOf(len(p.Threads), "thread", totalsThreads(p.Totals)), p.Current)
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
func printGoroutine(g protocol.Goroutine) {
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
	fmt.Println(line)
}

// printSnapshot renders a full concurrency snapshot: goroutines (with spawn
// linkage), OS threads, and the created/exited lifecycle deltas.
func printSnapshot(snap protocol.GoroutineSnapshotPayload) {
	fmt.Printf("  goroutines: %d  threads: %d  current: G%d\n",
		len(snap.Goroutines), len(snap.Threads), snap.Current)
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
	printTotals(len(snap.Goroutines), len(snap.Threads), snap.Totals)
}

// printTotals says so when the server sent less than it had. Totals is present
// only on an incomplete result, so silence here means the listing above is the
// whole truth. shownThreads is 0 for the goroutine-only listing, whose payload
// carries no thread collection.
func printTotals(shownGoroutines, shownThreads int, totals *protocol.SnapshotTotals) {
	if totals == nil {
		return
	}
	if omitted := totals.Goroutines - shownGoroutines; omitted > 0 {
		fmt.Printf("  ! %d of %d goroutines omitted from this event\n", omitted, totals.Goroutines)
	}
	if omitted := totals.Threads - shownThreads; omitted > 0 {
		fmt.Printf("  ! %d of %d threads omitted from this event\n", omitted, totals.Threads)
	}
	// A clipped scan means the total itself is a floor, so a listing that looks
	// complete against it still is not.
	if totals.GoroutinesClipped {
		fmt.Printf("  ! the debugger stopped after finding %d goroutines — more may exist\n",
			totals.Goroutines)
	}
	if totals.ThreadsClipped {
		fmt.Printf("  ! the debugger stopped after finding %d threads — more may exist\n",
			totals.Threads)
	}
}

// countOf renders a count the server may have understated, so a streamed line
// never presents a trimmed subset as the live population. total is 0 when the
// payload made no claim, which means nothing was omitted. The noun agrees with
// the number the reader is actually looking at — the shown count, since that is
// what the line is reporting.
func countOf(shown int, singular string, total totalCount) string {
	label := singular
	if shown != 1 {
		label += "s"
	}
	switch {
	case total.count > shown && total.clipped:
		return fmt.Sprintf("%d of at least %d %ss", shown, total.count, singular)
	case total.count > shown:
		return fmt.Sprintf("%d of %d %ss", shown, total.count, singular)
	case total.clipped:
		return fmt.Sprintf("%d+ %s", shown, label)
	default:
		return fmt.Sprintf("%d %s", shown, label)
	}
}

// totalCount pairs an original count with whether the scan that produced it
// stopped early, which is what makes the count a floor rather than a census.
type totalCount struct {
	count   int
	clipped bool
}

func totalsGoroutines(t *protocol.SnapshotTotals) totalCount {
	if t == nil {
		return totalCount{}
	}
	return totalCount{count: t.Goroutines, clipped: t.GoroutinesClipped}
}

func totalsThreads(t *protocol.SnapshotTotals) totalCount {
	if t == nil {
		return totalCount{}
	}
	return totalCount{count: t.Threads, clipped: t.ThreadsClipped}
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
  snapshot / snap            full concurrency snapshot (goroutines + threads + deltas)

  help / h / ?               show this help
  quit / q / exit            disconnect and exit`)
}
