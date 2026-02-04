package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bingosuite/bingo/config"
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

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Wait for shutdown signal
	<-sigChan
	log.Println("Received interrupt signal, shutting down gracefully...")

	// Graceful shutdown
	server.Shutdown()
	log.Println("Server shutdown complete, exiting")
}
