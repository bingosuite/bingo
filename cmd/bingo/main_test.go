package main

import (
	"errors"
	"flag"
	"io"
	"net"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/server"
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
	if cfg.addr != ":6060" || cfg.dapAddr != "" || cfg.idleTimeout != 0 || cfg.verbose {
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
