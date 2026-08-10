//go:build e2e && linux && amd64

package debugger

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

const steppingSIGURGTargetSource = `
#include <signal.h>
#include <unistd.h>

volatile sig_atomic_t handled;

static void handle_sigurg(int signal) {
	(void)signal;
	handled = 1;
}

extern void step_target(void);

__asm__(
	".text\n"
	".globl step_target\n"
	".type step_target,@function\n"
	"step_target:\n"
	"nop\n"
	"nop\n"
	"cmpl $1, handled(%rip)\n"
	"jne 1f\n"
	"mov $42, %edi\n"
	"jmp 2f\n"
	"1:\n"
	"mov $43, %edi\n"
	"2:\n"
	"mov $60, %eax\n"
	"syscall\n"
	".size step_target, .-step_target\n"
);

int main(void) {
	struct sigaction action = {0};
	action.sa_handler = handle_sigurg;
	sigemptyset(&action.sa_mask);
	if (sigaction(SIGURG, &action, 0) != 0) {
		return 91;
	}
	raise(SIGSTOP);
	for (;;) {
		pause();
	}
}
`

func buildSteppingSIGURGTarget(t *testing.T) (string, uint64) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "target.c")
	binary := filepath.Join(dir, "target")
	if err := os.WriteFile(source, []byte(steppingSIGURGTargetSource), 0o600); err != nil {
		t.Fatalf("write target source: %v", err)
	}
	cmd := exec.Command("cc", "-O0", "-g", "-fno-pie", "-no-pie", "-o", binary, source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build target: %v\n%s", err, output)
	}

	file, err := elf.Open(binary)
	if err != nil {
		t.Fatalf("open target ELF: %v", err)
	}
	defer file.Close()
	symbols, err := file.Symbols()
	if err != nil {
		t.Fatalf("read target symbols: %v", err)
	}
	for _, symbol := range symbols {
		if symbol.Name == "step_target" {
			return binary, symbol.Value
		}
	}
	t.Fatal("step_target symbol not found")
	return "", 0
}

func TestLinuxSteppingThreadSIGURGIsForwardedAfterInstructionStep(t *testing.T) {
	binary, stepPC := buildSteppingSIGURGTarget(t)
	b := newBackend().(*linuxBackend)
	pid, cmd, err := startTracedProcess(b, binary, nil, nil)
	if err != nil {
		b.closeTracer()
		t.Fatalf("start target: %v", err)
	}
	b.setPID(pid)
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill()
			b.reapAfterKill()
		}
		b.closeTracer()
	}()

	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("continue to ready stop: %v", err)
	}
	ready, err := b.Wait()
	if err != nil {
		t.Fatalf("wait for ready stop: %v", err)
	}
	if ready.Reason != StopSignal || ready.TID != pid || ready.Signal != int(syscall.SIGSTOP) {
		t.Fatalf("ready stop = %+v, want main-thread SIGSTOP", ready)
	}

	regs, err := b.GetRegisters(pid)
	if err != nil {
		t.Fatalf("read ready registers: %v", err)
	}
	regs.PC = stepPC
	if err := b.SetRegisters(pid, regs); err != nil {
		t.Fatalf("set step target PC: %v", err)
	}

	var resumeCalls []linuxResumeCall
	b.ptraceSyscall6Fn = func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		resumeCalls = append(resumeCalls, linuxResumeCall{
			trap: trap, request: a1, tid: a2, addr: a3, signal: a4, a5: a5, a6: a6,
		})
		return syscall.Syscall6(trap, a1, a2, a3, a4, a5, a6)
	}
	var tgkillCalls []linuxTgkillCall
	b.tgkillFn = func(tgid, targetTID int, signal syscall.Signal) error {
		tgkillCalls = append(tgkillCalls, linuxTgkillCall{tgid: tgid, tid: targetTID, signal: int(signal)})
		return syscall.Tgkill(tgid, targetTID, signal)
	}

	if err := syscall.Tgkill(pid, pid, syscall.SIGURG); err != nil {
		t.Fatalf("queue initial SIGURG: %v", err)
	}
	if err := b.SingleStep(pid); err != nil {
		t.Fatalf("first SingleStep: %v", err)
	}
	first, err := b.Wait()
	if err != nil {
		t.Fatalf("wait for first step: %v", err)
	}
	if first.Reason != StopSingleStep || first.TID != pid {
		t.Fatalf("first stop = %+v, want StopSingleStep on tid %d", first, pid)
	}
	regs, err = b.GetRegisters(pid)
	if err != nil {
		t.Fatalf("read first-step registers: %v", err)
	}
	if regs.PC != stepPC+1 {
		t.Fatalf("first-step PC = 0x%x, want 0x%x", regs.PC, stepPC+1)
	}

	if err := b.SingleStep(pid); err != nil {
		t.Fatalf("second SingleStep: %v", err)
	}
	second, err := b.Wait()
	if err != nil {
		t.Fatalf("wait for second step: %v", err)
	}
	if second.Reason != StopSingleStep || second.TID != pid {
		t.Fatalf("second stop = %+v, want StopSingleStep on tid %d", second, pid)
	}
	regs, err = b.GetRegisters(pid)
	if err != nil {
		t.Fatalf("read second-step registers: %v", err)
	}
	if regs.PC != stepPC+2 {
		t.Fatalf("second-step PC = 0x%x, want 0x%x", regs.PC, stepPC+2)
	}

	if err := b.ContinueProcess(); err != nil {
		t.Fatalf("continue with delayed SIGURG: %v", err)
	}
	terminal, err := b.Wait()
	if err != nil {
		t.Fatalf("wait for terminal stop: %v", err)
	}
	if terminal.Reason != StopExited || terminal.ExitCode != 42 {
		t.Fatalf("terminal stop = %+v, want handler-returning exit 42", terminal)
	}

	wantResumes := []linuxResumeCall{
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, pid, 0),
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, pid, 0),
		wantLinuxResume(syscall.PTRACE_SINGLESTEP, pid, 0),
		wantLinuxResume(syscall.PTRACE_CONT, pid, 0),
		wantLinuxResume(syscall.PTRACE_CONT, pid, int(syscall.SIGURG)),
		wantLinuxResume(syscall.PTRACE_CONT, pid, 0),
	}
	if !reflect.DeepEqual(resumeCalls, wantResumes) {
		t.Fatalf("resume calls = %+v, want exact delayed-delivery sequence %+v", resumeCalls, wantResumes)
	}
	wantTgkills := []linuxTgkillCall{{tgid: pid, tid: pid, signal: int(syscall.SIGURG)}}
	if !reflect.DeepEqual(tgkillCalls, wantTgkills) {
		t.Fatalf("tgkill calls = %+v, want exact-TID requeue %+v", tgkillCalls, wantTgkills)
	}

	_ = cmd.Wait()
	finished = true
}
