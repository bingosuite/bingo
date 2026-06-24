//go:build linux && amd64 && linuxptrace

package integration

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

const (
	linuxSmokeBreakpointLine = 7
	linuxSmokeEventTimeout   = 10 * time.Second
	linuxSmokeCleanupTimeout = 5 * time.Second
	linuxSmokeSourcePerm     = 0o600
)

const linuxSmokeSource = `package main

var sink int

func main() {
	value := 41
	value++
	sink = value
}
`

func TestLinuxAMD64DebuggerLaunchBreakpointSmoke(t *testing.T) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "main.go")
	binaryPath := filepath.Join(dir, "target")

	if err := os.WriteFile(sourcePath, []byte(linuxSmokeSource), linuxSmokeSourcePerm); err != nil {
		t.Fatalf("write smoke target: %v", err)
	}

	cmd := exec.Command("go", "build", "-buildmode=exe", "-gcflags=all=-N -l", "-o", binaryPath, sourcePath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build smoke target: %v\n%s", err, out)
	}

	dbg := debugger.New()
	defer cleanupDebugger(t, dbg)

	if err := dbg.Launch(binaryPath, nil, []string{"GOMAXPROCS=1", "GODEBUG=asyncpreemptoff=1"}); err != nil {
		t.Fatalf("launch smoke target: %v", err)
	}
	nextDebuggerEvent(t, dbg.Events(), protocol.EventStepped)

	bp, err := dbg.SetBreakpoint(sourcePath, linuxSmokeBreakpointLine)
	if err != nil {
		t.Fatalf("set breakpoint: %v", err)
	}

	if err := dbg.Continue(); err != nil {
		t.Fatalf("continue to breakpoint: %v", err)
	}

	hit := nextDebuggerEvent(t, dbg.Events(), protocol.EventBreakpointHit)
	var payload protocol.BreakpointHitPayload
	if err := protocol.DecodeEventPayload(hit, &payload); err != nil {
		t.Fatalf("decode breakpoint hit: %v", err)
	}
	if payload.Breakpoint.ID != bp.ID {
		t.Fatalf("hit breakpoint ID %d, want %d", payload.Breakpoint.ID, bp.ID)
	}

	if err := dbg.Continue(); err != nil {
		t.Fatalf("continue after breakpoint: %v", err)
	}
	nextDebuggerEvent(t, dbg.Events(), protocol.EventProcessExited)
}

func cleanupDebugger(t *testing.T, dbg debugger.Debugger) {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- dbg.Kill()
	}()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, debugger.ErrProcessExited) {
			t.Logf("kill smoke target: %v", err)
		}
	case <-time.After(linuxSmokeCleanupTimeout):
		t.Log("timed out waiting for debugger cleanup")
	}
}

func nextDebuggerEvent(t *testing.T, events <-chan protocol.Event, want protocol.EventKind) protocol.Event {
	t.Helper()

	timer := time.NewTimer(linuxSmokeEventTimeout)
	defer timer.Stop()

	for {
		select {
		case evt, ok := <-events:
			if !ok {
				t.Fatalf("events channel closed while waiting for %s", want)
			}
			t.Logf("observed event while waiting for %s: kind=%s payload=%s", want, evt.Kind, evt.Payload)
			if evt.Kind == protocol.EventError {
				var payload protocol.ErrorPayload
				if err := protocol.DecodeEventPayload(evt, &payload); err != nil {
					t.Fatalf("debugger emitted undecodable error while waiting for %s: %v", want, err)
				}
				t.Fatalf("debugger emitted error while waiting for %s: %s", want, payload.Message)
			}
			if evt.Kind == protocol.EventProcessExited && want != protocol.EventProcessExited {
				t.Fatalf("process exited while waiting for %s", want)
			}
			if evt.Kind == want {
				return evt
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}
