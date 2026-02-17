package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/bingosuite/bingo/config"
	"github.com/bingosuite/bingo/pkg/client"
)

func main() {
	cfg, err := config.Load("config/config.yml")
	if err != nil {
		log.Printf("Failed to load config: %v", err)
	}

	defaultAddr := "localhost:8080"
	if cfg != nil && cfg.Server.Addr != "" {
		if strings.HasPrefix(cfg.Server.Addr, ":") {
			defaultAddr = "localhost" + cfg.Server.Addr
		} else {
			defaultAddr = cfg.Server.Addr
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
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Printf("[%s] > ", c.State())
		if !scanner.Scan() {
			break
		}
		raw := strings.TrimSpace(scanner.Text())
		input := strings.ToLower(raw)
		fields := strings.Fields(raw)
		var cmdErr error
		switch input {
		case "c", "continue":
			cmdErr = c.Continue()
		case "s", "step", "stepover":
			cmdErr = c.StepOver()
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

	if err := scanner.Err(); err != nil {
		log.Printf("Stdin error: %v", err)
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
