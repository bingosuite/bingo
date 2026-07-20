// Command bingo starts the bingo debug server.
//
//	bingo [-addr host:port] [-dap-addr host:port] [-v]
package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bingosuite/bingo/internal/server"
)

func main() {
	addr := flag.String("addr", ":6060", "listen address (host:port)")
	dapAddr := flag.String("dap-addr", "", "DAP listen address (host:port); empty disables the DAP server")
	verbose := flag.Bool("v", false, "enable verbose (debug) logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))

	srv := server.New(*addr, log)

	if *dapAddr != "" {
		if err := srv.StartDAP(*dapAddr); err != nil {
			log.Error("dap server error", "err", err)
			os.Exit(1)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Info("received shutdown signal")
		srv.Shutdown(10 * time.Second)
	}()

	if err := srv.Start(); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}
