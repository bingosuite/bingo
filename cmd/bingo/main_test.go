package main

import (
	"errors"
	"flag"
	"io"
	"testing"
	"time"
)

type fakeServerRunner struct {
	start chan struct{}
	done  chan struct{}
	err   error
}

func (f *fakeServerRunner) Start() error {
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
