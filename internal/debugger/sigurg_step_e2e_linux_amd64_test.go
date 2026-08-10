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

type steppingSIGURGTracee struct {
	t        *testing.T
	backend  *linuxBackend
	pid      int
	cmd      *exec.Cmd
	finished bool
}

func startSteppingSIGURGTracee(t *testing.T, binary string) *steppingSIGURGTracee {
	t.Helper()
	b := newBackend().(*linuxBackend)
	pid, cmd, err := startTracedProcess(b, binary, nil, nil)
	if err != nil {
		b.closeTracer()
		t.Fatalf("start target: %v", err)
	}
	b.setPID(pid)
	tracee := &steppingSIGURGTracee{t: t, backend: b, pid: pid, cmd: cmd}
	t.Cleanup(tracee.cleanup)
	return tracee
}

func (p *steppingSIGURGTracee) cleanup() {
	if !p.finished {
		_ = p.cmd.Process.Kill()
		p.backend.reapAfterKill()
	}
	p.backend.closeTracer()
}

func (p *steppingSIGURGTracee) prepare(stepPC uint64) {
	p.t.Helper()
	if err := p.backend.ContinueProcess(); err != nil {
		p.t.Fatalf("continue to ready stop: %v", err)
	}
	ready, err := p.backend.Wait()
	if err != nil {
		p.t.Fatalf("wait for ready stop: %v", err)
	}
	if ready.Reason != StopSignal || ready.TID != p.pid || ready.Signal != int(syscall.SIGSTOP) {
		p.t.Fatalf("ready stop = %+v, want main-thread SIGSTOP", ready)
	}

	regs, err := p.backend.GetRegisters(p.pid)
	if err != nil {
		p.t.Fatalf("read ready registers: %v", err)
	}
	regs.PC = stepPC
	if err := p.backend.SetRegisters(p.pid, regs); err != nil {
		p.t.Fatalf("set step target PC: %v", err)
	}
}

func (p *steppingSIGURGTracee) recordCalls(resumeCalls *[]linuxResumeCall, tgkillCalls *[]linuxTgkillCall) {
	p.backend.ptraceSyscall6Fn = func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		*resumeCalls = append(*resumeCalls, linuxResumeCall{
			trap: trap, request: a1, tid: a2, addr: a3, signal: a4, a5: a5, a6: a6,
		})
		return syscall.Syscall6(trap, a1, a2, a3, a4, a5, a6)
	}
	p.backend.tgkillFn = func(tgid, targetTID int, signal syscall.Signal) error {
		*tgkillCalls = append(*tgkillCalls, linuxTgkillCall{tgid: tgid, tid: targetTID, signal: int(signal)})
		return syscall.Tgkill(tgid, targetTID, signal)
	}
}

func (p *steppingSIGURGTracee) queueSIGURG() {
	p.t.Helper()
	if err := syscall.Tgkill(p.pid, p.pid, syscall.SIGURG); err != nil {
		p.t.Fatalf("queue initial SIGURG: %v", err)
	}
}

func (p *steppingSIGURGTracee) requireStep(name string, wantPC uint64) {
	p.t.Helper()
	if err := p.backend.SingleStep(p.pid); err != nil {
		p.t.Fatalf("%s SingleStep: %v", name, err)
	}
	stop, err := p.backend.Wait()
	if err != nil {
		p.t.Fatalf("wait for %s step: %v", name, err)
	}
	if stop.Reason != StopSingleStep || stop.TID != p.pid {
		p.t.Fatalf("%s stop = %+v, want StopSingleStep on tid %d", name, stop, p.pid)
	}
	regs, err := p.backend.GetRegisters(p.pid)
	if err != nil {
		p.t.Fatalf("read %s-step registers: %v", name, err)
	}
	if regs.PC != wantPC {
		p.t.Fatalf("%s-step PC = 0x%x, want 0x%x", name, regs.PC, wantPC)
	}
}

func (p *steppingSIGURGTracee) continueToHandlerExit() {
	p.t.Helper()
	if err := p.backend.ContinueProcess(); err != nil {
		p.t.Fatalf("continue with delayed SIGURG: %v", err)
	}
	terminal, err := p.backend.Wait()
	if err != nil {
		p.t.Fatalf("wait for terminal stop: %v", err)
	}
	if terminal.Reason != StopExited || terminal.ExitCode != 42 {
		p.t.Fatalf("terminal stop = %+v, want handler-returning exit 42", terminal)
	}
}

func (p *steppingSIGURGTracee) finish() {
	_ = p.cmd.Wait()
	p.finished = true
}

func requireSteppingSIGURGCalls(
	t *testing.T,
	pid int,
	resumeCalls []linuxResumeCall,
	tgkillCalls []linuxTgkillCall,
) {
	t.Helper()
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
}

func TestLinuxSteppingThreadSIGURGIsForwardedAfterInstructionStep(t *testing.T) {
	binary, stepPC := buildSteppingSIGURGTarget(t)
	tracee := startSteppingSIGURGTracee(t, binary)
	tracee.prepare(stepPC)

	var resumeCalls []linuxResumeCall
	var tgkillCalls []linuxTgkillCall
	tracee.recordCalls(&resumeCalls, &tgkillCalls)
	tracee.queueSIGURG()
	tracee.requireStep("first", stepPC+1)
	tracee.requireStep("second", stepPC+2)
	tracee.continueToHandlerExit()
	requireSteppingSIGURGCalls(t, tracee.pid, resumeCalls, tgkillCalls)
	tracee.finish()
}
