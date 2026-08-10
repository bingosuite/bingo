//go:build linux && amd64 && e2e

package debugger

import (
	"bytes"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

const steppingSIGURGHelperSource = `
#include <fcntl.h>
#include <signal.h>
#include <stdlib.h>
#include <unistd.h>

static int marker_fd;

__asm__(
".global step_target\n"
".type step_target,@function\n"
"step_target:\n"
"nop\n"
"ret\n"
".size step_target, .-step_target\n"
);

static void handle_sigurg(int signal) {
	(void)signal;
	const char marker = 'H';
	(void)write(marker_fd, &marker, 1);
	_exit(42);
}

int main(int argc, char **argv) {
	if (argc != 2) {
		return 90;
	}

	marker_fd = open(argv[1], O_WRONLY | O_CREAT | O_TRUNC, 0600);
	if (marker_fd < 0) {
		return 91;
	}

	struct sigaction action = {0};
	action.sa_handler = handle_sigurg;
	sigemptyset(&action.sa_mask);
	if (sigaction(SIGURG, &action, NULL) != 0) {
		return 92;
	}

	const char ready = 'R';
	if (write(marker_fd, &ready, 1) != 1) {
		return 93;
	}

	for (;;) {
		__asm__ volatile("nop");
	}
}
`

func TestLinuxBackendSteppingThreadSIGURGIsRedelivered(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "stepping_sigurg.c")
	binaryPath := filepath.Join(dir, "stepping_sigurg")
	markerPath := filepath.Join(dir, "marker")
	if err := os.WriteFile(sourcePath, []byte(steppingSIGURGHelperSource), 0o600); err != nil {
		t.Fatalf("write helper source: %v", err)
	}
	if output, err := exec.Command("cc", "-O0", "-g", "-fno-pie", "-no-pie", "-o", binaryPath, sourcePath).CombinedOutput(); err != nil {
		t.Fatalf("compile helper: %v\n%s", err, output)
	}
	stepPC := findELFSymbol(t, binaryPath, "step_target")

	b := newBackend().(*linuxBackend)
	pid, _, err := startTracedProcess(b, binaryPath, []string{markerPath}, nil)
	if err != nil {
		b.closeTracer()
		t.Fatalf("start helper: %v", err)
	}
	b.setPID(pid)

	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			_, _ = b.Wait()
		}
		b.closeTracer()
	})

	var (
		resumeMu sync.Mutex
		resumes  []linuxResumeCall
	)
	b.ptraceSyscall6Fn = func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		resumeMu.Lock()
		resumes = append(resumes, linuxResumeCall{
			trap: trap, request: a1, tid: a2, addr: a3, signal: a4, a5: a5, a6: a6,
		})
		resumeMu.Unlock()
		return syscall.Syscall6(trap, a1, a2, a3, a4, a5, a6)
	}

	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("continue helper to readiness: %v", err)
	}
	if !waitForFileMarker(markerPath, 'R', 5*time.Second) {
		t.Fatal("helper did not install its SIGURG handler")
	}

	if err := b.StopProcess(); err != nil {
		t.Fatalf("stop helper: %v", err)
	}
	stop, err := b.Wait()
	if err != nil {
		t.Fatalf("wait for SIGSTOP: %v", err)
	}
	if stop.Reason != StopSignal || stop.TID != pid || stop.Signal != int(syscall.SIGSTOP) {
		t.Fatalf("pause stop = %+v, want SIGSTOP on exact tid %d", stop, pid)
	}
	regs, err := b.GetRegisters(pid)
	if err != nil {
		t.Fatalf("read registers before step: %v", err)
	}
	regs.PC = stepPC
	if err := b.SetRegisters(pid, regs); err != nil {
		t.Fatalf("set controlled step PC: %v", err)
	}

	if err := syscall.Tgkill(pid, pid, syscall.SIGURG); err != nil {
		t.Fatalf("queue SIGURG on tid %d: %v", pid, err)
	}
	if err := b.SingleStep(pid); err != nil {
		t.Fatalf("start single-step on tid %d: %v", pid, err)
	}

	step, err := b.Wait()
	if err != nil {
		t.Fatalf("wait for single-step completion: %v", err)
	}
	if step.Reason != StopSingleStep || step.TID != pid {
		t.Fatalf("step stop = %+v, want StopSingleStep on exact tid %d", step, pid)
	}
	regs, err = b.GetRegisters(pid)
	if err != nil {
		t.Fatalf("read registers after step: %v", err)
	}
	if regs.PC != stepPC+1 {
		t.Fatalf("PC after step = 0x%x, want controlled instruction completed at 0x%x", regs.PC, stepPC+1)
	}

	regs.PC = stepPC
	if err := b.SetRegisters(pid, regs); err != nil {
		t.Fatalf("reset controlled step PC: %v", err)
	}
	if err := b.SingleStep(pid); err != nil {
		t.Fatalf("start second single-step on tid %d: %v", pid, err)
	}
	step, err = b.Wait()
	if err != nil {
		t.Fatalf("wait for second single-step completion: %v", err)
	}
	if step.Reason != StopSingleStep || step.TID != pid {
		t.Fatalf("second step stop = %+v, want StopSingleStep on exact tid %d", step, pid)
	}
	regs, err = b.GetRegisters(pid)
	if err != nil {
		t.Fatalf("read registers after second step: %v", err)
	}
	if regs.PC != stepPC+1 {
		t.Fatalf("PC after second step = 0x%x, want delayed SIGURG retained and instruction completed at 0x%x", regs.PC, stepPC+1)
	}

	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("continue from single-step completion: %v", err)
	}

	resumeMu.Lock()
	gotResumes := append([]linuxResumeCall(nil), resumes...)
	resumeMu.Unlock()
	wantResumes := []linuxResumeCall{
		wantLinuxResume(syscall.PTRACE_CONT, pid, 0),
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, pid, 0),
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, pid, 0),
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, pid, 0),
		wantLinuxResume(syscall.PTRACE_CONT, pid, int(syscall.SIGURG)),
	}
	if !reflect.DeepEqual(gotResumes, wantResumes) {
		t.Errorf("ptrace resume calls = %+v, want original step completion before exact signal delivery %+v", gotResumes, wantResumes)
	}
	if !waitForFileMarker(markerPath, 'H', 2*time.Second) {
		t.Errorf("SIGURG handler did not run on tid %d", pid)
		return
	}

	exited, err := b.Wait()
	if err != nil {
		t.Fatalf("wait for helper exit: %v", err)
	}
	if exited.Reason != StopExited || exited.TID != pid || exited.ExitCode != 42 {
		t.Fatalf("terminal stop = %+v, want exit 42 on tid %d", exited, pid)
	}
	reaped = true
}

func findELFSymbol(t *testing.T, path, name string) uint64 {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		t.Fatalf("open helper ELF: %v", err)
	}
	defer f.Close()
	symbols, err := f.Symbols()
	if err != nil {
		t.Fatalf("read helper symbols: %v", err)
	}
	for _, symbol := range symbols {
		if symbol.Name == name {
			return symbol.Value
		}
	}
	t.Fatalf("helper symbol %q not found", name)
	return 0
}

func waitForFileMarker(path string, marker byte, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && bytes.Contains(data, []byte{marker}) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
