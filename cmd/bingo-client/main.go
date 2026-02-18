package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bingosuite/bingo/config"
	"github.com/bingosuite/bingo/pkg/client"
	"github.com/peterh/liner"
	"golang.org/x/term"
)

func main() {
	cfg, err := config.Load("config/config.yml")
	if err != nil {
		log.Printf("Failed to load config: %v", err)
	}

	if cfg == nil {
		cfg = config.Default()
	}

	defaultAddr := cfg.CLI.Host
	if cfg.CLI.Host == "" {
		if cfg.Server.Addr != "" {
			if strings.HasPrefix(cfg.Server.Addr, ":") {
				defaultAddr = "localhost" + cfg.Server.Addr
			} else {
				defaultAddr = cfg.Server.Addr
			}
		}
	}

	server := flag.String("server", defaultAddr, "WebSocket server host:port")
	session := flag.String("session", "", "Existing session ID (optional)")
	flag.Parse()

	c := client.NewClient(*server, *session)
	if err := c.Connect(); err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	if err := c.Run(); err != nil {
		log.Fatalf("Failed to start client: %v", err)
	}

	log.Println("Connected. Commands: start <path>, c=continue, s=step, b=<file> <line>, state, q=quit")

	inputReader := bufio.NewReader(os.Stdin)
	useRawInput := term.IsTerminal(int(os.Stdin.Fd()))
	var lineEditor *liner.State
	if useRawInput {
		lineEditor = liner.NewLiner()
		lineEditor.SetCtrlCAborts(true)
		lineEditor.SetTabCompletionStyle(liner.TabPrints)
		defer func() {
			_ = lineEditor.Close()
		}()
	}

	history := make([]string, 0, 64)

	for {
		time.Sleep(100 * time.Millisecond)
		prompt := fmt.Sprintf("[%s] > ", c.State())
		var rawLine string
		var readErr error
		if useRawInput {
			rawLine, readErr = lineEditor.Prompt(prompt)
		} else {
			fmt.Print(prompt)
			line, err := inputReader.ReadString('\n')
			if err != nil {
				if err != io.EOF {
					log.Printf("Stdin error: %v", err)
				}
				break
			}
			rawLine = strings.TrimRight(line, "\r\n")
		}
		if readErr != nil {
			if readErr == liner.ErrPromptAborted || readErr == io.EOF {
				break
			}
			log.Printf("Stdin error: %v", readErr)
			break
		}

		raw := strings.TrimSpace(rawLine)
		if raw != "" {
			if len(history) == 0 || history[len(history)-1] != rawLine {
				history = append(history, rawLine)
				if lineEditor != nil {
					lineEditor.AppendHistory(rawLine)
				}
			}
		}

		input := strings.ToLower(raw)
		fields := strings.Fields(raw)
		var cmdErr error
		switch input {
		case "c", "continue":
			cmdErr = c.Continue()
			time.Sleep(100 * time.Millisecond)
		case "s", "step", "stepover":
			cmdErr = c.StepOver()
			time.Sleep(100 * time.Millisecond)
		case "state":
			fmt.Printf("state=%s session=%s\n", c.State(), c.SessionID())
		case "q", "quit", "exit":
			_ = c.Close()
			return
		case "":
			continue
		default:
			if len(fields) > 0 && strings.EqualFold(fields[0], "start") {
				cmdErr = handleStartCommand(c, fields)
				if cmdErr != nil {
					fmt.Println(cmdErr.Error())
					cmdErr = nil
					break
				}
				// Give async state updates time to arrive before showing next prompt
				time.Sleep(100 * time.Millisecond)
				break
			}
			cmdErr = handleBreakpointCommand(c, raw)
			if cmdErr != nil {
				fmt.Println(cmdErr.Error())
				cmdErr = nil
			}
		}
		if cmdErr != nil {
			log.Printf("Command error: %v", cmdErr)
		}
	}

	_ = c.Close()
}

func handleBreakpointCommand(c *client.Client, raw string) error {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	cmd := strings.ToLower(fields[0])
	if cmd != "b" && cmd != "break" && cmd != "breakpoint" {
		return fmt.Errorf("unknown command")
	}
	if len(fields) < 2 || len(fields) > 3 {
		return fmt.Errorf("usage: b <line> or b <file> <line>")
	}
	filename := ""
	lineStr := ""
	if len(fields) == 2 {
		lineStr = fields[1]
	} else {
		filename = fields[1]
		lineStr = fields[2]
	}
	line, err := strconv.Atoi(lineStr)
	if err != nil || line <= 0 {
		return fmt.Errorf("invalid line number")
	}
	return c.SetBreakpoint(filename, line)
}

func handleStartCommand(c *client.Client, fields []string) error {
	if len(fields) != 2 {
		return fmt.Errorf("usage: start <path>")
	}
	return c.StartDebug(fields[1])
}
