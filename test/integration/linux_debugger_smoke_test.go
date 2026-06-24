//go:build linux && amd64 && linuxptrace

package integration

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
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
	nextDebuggerEvent(t, dbg.Events(), protocol.EventStepped, nil)

	bp, err := dbg.SetBreakpoint(sourcePath, linuxSmokeBreakpointLine)
	if err != nil {
		t.Fatalf("set breakpoint: %v", err)
	}

	if err := dbg.Continue(); err != nil {
		t.Fatalf("continue to breakpoint: %v", err)
	}

	hit := nextDebuggerEvent(t, dbg.Events(), protocol.EventBreakpointHit, func() {
		dumpTraceeTasks(t, binaryPath)
	})
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
	nextDebuggerEvent(t, dbg.Events(), protocol.EventProcessExited, nil)
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

func nextDebuggerEvent(
	t *testing.T,
	events <-chan protocol.Event,
	want protocol.EventKind,
	onTimeout func(),
) protocol.Event {
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
			if onTimeout != nil {
				onTimeout()
			}
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func dumpTraceeTasks(t *testing.T, binaryPath string) {
	t.Helper()

	pids := traceePIDs(t, binaryPath)
	if len(pids) == 0 {
		t.Logf("no tracee process found for %q", binaryPath)
		return
	}
	for _, pid := range pids {
		status, err := os.ReadFile(filepath.Join("/proc", pid, "status"))
		if err != nil {
			t.Logf("read /proc/%s/status: %v", pid, err)
		} else {
			t.Logf("/proc/%s/status:\n%s", pid, summarizeStatus(status))
		}
		tasks, err := os.ReadDir(filepath.Join("/proc", pid, "task"))
		if err != nil {
			t.Logf("read /proc/%s/task: %v", pid, err)
			continue
		}
		for _, task := range tasks {
			tid := task.Name()
			taskStatus, err := os.ReadFile(filepath.Join("/proc", pid, "task", tid, "status"))
			if err != nil {
				t.Logf("read /proc/%s/task/%s/status: %v", pid, tid, err)
				continue
			}
			wchan, _ := os.ReadFile(filepath.Join("/proc", pid, "task", tid, "wchan"))
			t.Logf("/proc/%s/task/%s: %s wchan=%s", pid, tid, summarizeStatus(taskStatus), strings.TrimSpace(string(wchan)))
		}
	}
}

func traceePIDs(t *testing.T, binaryPath string) []string {
	t.Helper()

	cmdlines, err := filepath.Glob("/proc/[0-9]*/cmdline")
	if err != nil {
		t.Logf("glob proc cmdlines: %v", err)
		return nil
	}
	var pids []string
	for _, cmdlinePath := range cmdlines {
		cmdline, err := os.ReadFile(cmdlinePath)
		if err != nil || !bytes.Contains(cmdline, []byte(binaryPath)) {
			continue
		}
		pids = append(pids, path.Base(path.Dir(cmdlinePath)))
	}
	return pids
}

func summarizeStatus(status []byte) string {
	var b strings.Builder
	for _, line := range strings.Split(string(status), "\n") {
		switch {
		case strings.HasPrefix(line, "Name:"),
			strings.HasPrefix(line, "State:"),
			strings.HasPrefix(line, "Tgid:"),
			strings.HasPrefix(line, "Pid:"),
			strings.HasPrefix(line, "PPid:"),
			strings.HasPrefix(line, "TracerPid:"),
			strings.HasPrefix(line, "Threads:"):
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}
