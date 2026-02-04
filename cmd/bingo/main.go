package main

import (
	"log"

	"github.com/bingosuite/bingo/config"
	"github.com/bingosuite/bingo/internal/debugger"
	websocket "github.com/bingosuite/bingo/internal/ws"
)

func main() {
	cfg, err := config.Load("config/config.yml")
	if err != nil {
		log.Printf("Failed to load config: %v, using defaults", err)
		cfg = config.Default()
	}

	server := websocket.NewServer(cfg.Server.Addr, &cfg.WebSocket)

	go func() {
		if err := server.Serve(); err != nil {
			log.Printf("WebSocket server error: %v", err)
			panic(err)
		}
	}()

	d := debugger.NewDebugger()
	// TODO: server tells debugger how to start the debugging session by passing path to start with debug or pid to attach
	go d.StartWithDebug("/workspaces/bingo/build/target/target")
	<-d.EndDebugSession
}
