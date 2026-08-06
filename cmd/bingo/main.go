// Command bingo starts the bingo debug server.
//
//	bingo [-addr host:port] [-dap-addr host:port] [-idle-timeout duration] [-v]
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bingosuite/bingo/internal/server"
)

type config struct {
	addr        string
	dapAddr     string
	idleTimeout time.Duration
	verbose     bool
}

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "bingo:", err)
		os.Exit(2)
	}

	level := slog.LevelInfo
	if cfg.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	}))

	srv, err := server.NewWithIdleTimeout(cfg.addr, cfg.idleTimeout, log)
	if err != nil {
		log.Error("invalid server configuration", "err", err)
		os.Exit(2)
	}

	if cfg.dapAddr != "" {
		if err := srv.StartDAP(cfg.dapAddr); err != nil {
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

	if err := runServer(srv); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}
}

type serverRunner interface {
	Start() error
	Done() <-chan struct{}
}

func runServer(srv serverRunner) error {
	if err := srv.Start(); err != nil {
		return err
	}
	<-srv.Done()
	return nil
}

func parseConfig(args []string, output io.Writer) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("bingo", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.addr, "addr", ":6060", "listen address (host:port)")
	flags.StringVar(&cfg.dapAddr, "dap-addr", "", "DAP listen address (host:port); empty disables the DAP server")
	flags.DurationVar(&cfg.idleTimeout, "idle-timeout", 0, "exit after no managed sessions for this duration; 0 disables")
	flags.BoolVar(&cfg.verbose, "v", false, "enable verbose (debug) logging")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if err := server.ValidateIdleTimeout(cfg.idleTimeout); err != nil {
		return config{}, err
	}
	return cfg, nil
}
