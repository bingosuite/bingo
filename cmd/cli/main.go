// Command cli is an interactive terminal client for the bingo debug server.
//
// Usage:
//
//	cli [flags]
//	  -addr string    server address (default "localhost:6060")
//	  -session string session ID to join (omit to create a new session)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
)

func main() {
	addr := flag.String("addr", "localhost:6060", "server address (host:port)")
	sessionID := flag.String("session", "", "session ID to join (omit to create)")
	flag.Parse()

	// Connect to the server.
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
	defer c.Close()

	fmt.Printf("connected — session %s (state: %s)\n\n", c.SessionID(), c.State())

	// Start a goroutine that prints incoming events.
	go eventPrinter(c.Events())

	// Interactive command loop.
	scanner := bufio.NewScanner(os.Stdin)
	printHelp()
	for {
		fmt.Print("bingo> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		args := strings.Fields(line)
		cmd := args[0]

		switch cmd {

		// ── Session ──────────────────────────────────────────────────────

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

		// ── Process lifecycle ────────────────────────────────────────────

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

		// ── Execution control ────────────────────────────────────────────

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

		// ── Breakpoints ──────────────────────────────────────────────────

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

		// ── Inspection ───────────────────────────────────────────────────

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
			for _, g := range grs {
				loc := fmt.Sprintf("%s:%d", g.CurrentLoc.File, g.CurrentLoc.Line)
				if g.WaitReason != "" {
					fmt.Printf("  G%-4d %-10s %s  (%s)\n", g.ID, g.Status, loc, g.WaitReason)
				} else {
					fmt.Printf("  G%-4d %-10s %s\n", g.ID, g.Status, loc)
				}
			}

		// ── Meta ─────────────────────────────────────────────────────────

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

// ── Helpers ──────────────────────────────────────────────────────────────────

// eventPrinter drains the events channel and prints each event to stdout.
func eventPrinter(events <-chan protocol.Event) {
	for evt := range events {
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

		case protocol.EventContinued:
			fmt.Print("\n  [continued]\nbingo> ")

		case protocol.EventError:
			var p protocol.ErrorPayload
			if protocol.DecodeEventPayload(evt, &p) == nil {
				fmt.Printf("\n  [error] %s: %s\nbingo> ", p.Command, p.Message)
			}

		default:
			fmt.Printf("\n  [%s] seq=%d\nbingo> ", evt.Kind, evt.Seq)
		}
	}
}

// parseFileLine splits "file.go:42" into ("file.go", 42).
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
  attach <pid> [binary]      attach to a running process
  kill                       terminate the debuggee

  c / continue               resume execution
  n / next                   step over
  s / step                   step into
  out / finish               step out (run until function returns)

  b / break <file>:<line>    set breakpoint  (e.g. break main.go:42)
  clear <id>                 remove breakpoint by ID

  locals [frame]             show local variables (default frame 0)
  bt / backtrace             show call stack
  goroutines / grs           list goroutines

  help / h / ?               show this help
  quit / q / exit            disconnect and exit`)
}
