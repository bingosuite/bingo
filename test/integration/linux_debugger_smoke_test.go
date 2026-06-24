//go:build linux && amd64 && linuxptrace

package integration

import (
	"errors"
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
	linuxSmokeSourcePerm     = 0o600
)

const linuxSmokeSource = `package main

import "fmt"

func main() {
	value := 41
	value++
	fmt.Println(value)
}
`

func TestLinuxAMD64DebuggerLaunchBreakpointSmoke(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "main.go")
	binaryPath := filepath.Join(dir, "target")

	if err := os.WriteFile(sourcePath, []byte(linuxSmokeSource), linuxSmokeSourcePerm); err != nil {
		t.Fatalf("write smoke target: %v", err)
	}

	cmd := exec.Command("go", "build", "-gcflags=all=-N -l", "-o", binaryPath, sourcePath)
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build smoke target: %v\n%s", err, out)
	}

	dbg := debugger.New()
	defer func() {
		if err := dbg.Kill(); err != nil && !errors.Is(err, debugger.ErrProcessExited) {
			t.Logf("kill smoke target: %v", err)
		}
	}()

	if err := dbg.Launch(binaryPath, nil, nil); err != nil {
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
