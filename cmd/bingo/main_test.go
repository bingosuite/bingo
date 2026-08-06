package main

import (
	"flag"
	"io"
	"testing"
	"time"
)

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
