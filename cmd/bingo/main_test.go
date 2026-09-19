package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/server"
	"github.com/bingosuite/bingo/pkg/protocol"
)

type fakeServerRunner struct {
	start       chan struct{}
	startExited chan struct{}
	done        chan struct{}
	err         error
}

func (f *fakeServerRunner) Start() error {
	if f.startExited != nil {
		defer close(f.startExited)
	}
	<-f.start
	return f.err
}

func (f *fakeServerRunner) Done() <-chan struct{} {
	return f.done
}

func TestParseConfigDefaultsToPersistentServer(t *testing.T) {
	cfg, err := parseConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if cfg.addr != "127.0.0.1:6060" || cfg.dapAddr != "" || cfg.idleTimeout != 0 || cfg.verbose || cfg.showVersion {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestParseConfigIdleTimeout(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-addr", "127.0.0.1:16060",
		"-dap-addr", "127.0.0.1:14711",
		"-idle-timeout", "30s",
		"-v",
	}, io.Discard)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.addr != "127.0.0.1:16060" ||
		cfg.dapAddr != "127.0.0.1:14711" ||
		cfg.idleTimeout != 30*time.Second ||
		!cfg.verbose {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestParseConfigRejectsNegativeIdleTimeout(t *testing.T) {
	if _, err := parseConfig([]string{"-idle-timeout", "-1s"}, io.Discard); err == nil {
		t.Fatal("expected negative idle timeout error")
	}
}

func TestParseConfigRejectsSubMillisecondIdleTimeout(t *testing.T) {
	if _, err := parseConfig([]string{"-idle-timeout", "1ns"}, io.Discard); err == nil {
		t.Fatal("expected sub-millisecond idle timeout error")
	}
	if _, err := parseConfig([]string{"-idle-timeout", "1500us"}, io.Discard); err == nil {
		t.Fatal("expected fractional-millisecond idle timeout error")
	}
}

func TestParseConfigRejectsInvalidDuration(t *testing.T) {
	if _, err := parseConfig([]string{"-idle-timeout", "later"}, io.Discard); err == nil {
		t.Fatal("expected invalid duration error")
	}
}

func TestParseConfigHelp(t *testing.T) {
	if _, err := parseConfig([]string{"-h"}, io.Discard); err != flag.ErrHelp {
		t.Fatalf("expected flag.ErrHelp, got %v", err)
	}
}

func TestParseConfigVersion(t *testing.T) {
	cfg, err := parseConfig([]string{"-version"}, io.Discard)
	if err != nil || !cfg.showVersion {
		t.Fatalf("parse version: %+v, %v", cfg, err)
	}
}

func TestParseConfigRejectsPositionalArguments(t *testing.T) {
	for _, args := range [][]string{{"target"}, {"-version", "target"}, {"--", "target"}} {
		if _, err := parseConfig(args, io.Discard); err == nil {
			t.Fatalf("accepted positional arguments: %q", args)
		}
	}
}

func TestStartupFlagsDoNotStartServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		code int
		want string
	}{
		{"version", []string{"-addr", "invalid-address", "-dap-addr", "invalid-address", "-version"}, 0, "wire " + protocol.Version},
		{"help", []string{"-help"}, 0, "Usage of bingo:"},
		{"unknown flag", []string{"-not-a-flag"}, 2, "flag provided but not defined"},
		{"positional", []string{"-addr", "invalid-address", "target"}, 2, "unexpected positional arguments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			args := append([]string{"-test.run=^TestStartupMainHelper$", "--"}, tc.args...)
			cmd := exec.CommandContext(ctx, os.Args[0], args...)
			cmd.Env = append(os.Environ(), "BINGO_TEST_STARTUP_MAIN=1")
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("startup flag did not exit: %v\n%s", ctx.Err(), out)
			}
			if cmd.ProcessState.ExitCode() != tc.code {
				t.Fatalf("exit code %d, want %d: %v\n%s", cmd.ProcessState.ExitCode(), tc.code, err, out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("missing %q in %s", tc.want, out)
			}
			for _, unexpected := range []string{"server error", "invalid server configuration", "listening"} {
				if strings.Contains(string(out), unexpected) {
					t.Fatalf("startup reached server lifecycle: %s", out)
				}
			}
		})
	}
}

func TestStartupMainHelper(t *testing.T) {
	if os.Getenv("BINGO_TEST_STARTUP_MAIN") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"bingo"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	t.Fatal("missing helper argument delimiter")
}

func TestRunServerWaitsForShutdownCompletion(t *testing.T) {
	runner := &fakeServerRunner{
		start: make(chan struct{}),
		done:  make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		result <- runServer(runner)
	}()

	close(runner.start)
	select {
	case err := <-result:
		t.Fatalf("runServer returned before shutdown completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(runner.done)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServer did not return after shutdown completed")
	}
}

func TestRunServerWaitsForBlockedStartAfterDone(t *testing.T) {
	runner := &fakeServerRunner{
		start:       make(chan struct{}),
		startExited: make(chan struct{}),
		done:        make(chan struct{}),
	}
	result := make(chan error, 1)
	go func() {
		result <- runServer(runner)
	}()

	close(runner.done)
	select {
	case err := <-result:
		t.Fatalf("runServer abandoned blocked Start after Done: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(runner.start)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServer: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServer did not consume Start after Done")
	}
	select {
	case <-runner.startExited:
	case <-time.After(time.Second):
		t.Fatal("Start goroutine did not exit")
	}
}

func TestRunServerReturnsStartError(t *testing.T) {
	startErr := errors.New("bind failed")
	runner := &fakeServerRunner{
		start: make(chan struct{}),
		done:  make(chan struct{}),
		err:   startErr,
	}
	close(runner.start)

	if err := runServer(runner); !errors.Is(err, startErr) {
		t.Fatalf("expected start error, got %v", err)
	}
}

func TestRunServerReturnsFinalizedBindError(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	srv := server.New(occupied.Addr().String(), nil)
	err = runServer(srv)
	if err == nil {
		t.Fatal("expected bind error")
	}
	var netErr *net.OpError
	if !errors.As(err, &netErr) || netErr.Op != "listen" {
		t.Fatalf("expected listen error, got %v", err)
	}
	select {
	case <-srv.Done():
	default:
		t.Fatal("bind error returned before lifecycle finalization")
	}
}
