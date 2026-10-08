//go:build e2e && linux && amd64

package debugger

import (
	"context"
	"debug/elf"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const inspectionNativeTarget = `
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <unistd.h>

_Atomic unsigned long entered;
_Atomic unsigned long heartbeat;
_Atomic unsigned long handled;
_Atomic unsigned long terminating;

static void handler(int signal) {
	if (signal == SIGTERM) {
		atomic_store(&terminating, 1);
		return;
	}
	atomic_fetch_add(&handled, 1);
}
__attribute__((noinline)) void tick(void) {
	atomic_fetch_add(&heartbeat, 1);
}
static void *worker(void *arg) {
	(void)arg;
	while (!atomic_load(&terminating)) {
		tick();
		usleep(1000);
	}
	return 0;
}
int main(void) {
	atomic_store(&entered, 1);
	struct sigaction action = {0};
	action.sa_handler = handler;
	sigemptyset(&action.sa_mask);
	if (sigaction(SIGUSR1, &action, 0) || sigaction(SIGUSR2, &action, 0) ||
		sigaction(SIGTERM, &action, 0)) return 91;
	pthread_t a, b;
	if (pthread_create(&a, 0, worker, 0) || pthread_create(&b, 0, worker, 0)) return 92;
	usleep(10000);
	raise(SIGUSR1);
	while (!atomic_load(&terminating)) usleep(1000);
	pthread_join(a, 0);
	pthread_join(b, 0);
	return 0;
}
`

type observedInspectionWaits struct {
	linuxWaitSource
	results []linuxWaitResult
}

func (w *observedInspectionWaits) next(ctx context.Context) (linuxWaitResult, error) {
	result, err := w.linuxWaitSource.next(ctx)
	if err == nil {
		w.results = append(w.results, result)
	}
	return result, err
}

func (w *observedInspectionWaits) tryNext() (linuxWaitResult, bool, error) {
	result, ok, err := w.linuxWaitSource.tryNext()
	if ok {
		w.results = append(w.results, result)
	}
	return result, ok, err
}

func buildNativeInspectionTarget(t *testing.T) (string, map[string]uint64) {
	t.Helper()
	dir := t.TempDir()
	source, bin := filepath.Join(dir, "target.c"), filepath.Join(dir, "target")
	if err := os.WriteFile(source, []byte(inspectionNativeTarget), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cc", "-O0", "-g", "-fno-pie", "-no-pie", "-pthread", "-o", bin, source).CombinedOutput(); err != nil {
		t.Fatalf("build target: %v\n%s", err, out)
	}
	file, err := elf.Open(bin)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	symbols, err := file.Symbols()
	if err != nil {
		t.Fatal(err)
	}
	addresses := make(map[string]uint64)
	for _, symbol := range symbols {
		addresses[symbol.Name] = symbol.Value
	}
	return bin, addresses
}

func nativeInspectionWord(t *testing.T, pid int, addresses map[string]uint64, name string) uint64 {
	t.Helper()
	addr := addresses[name]
	if addr == 0 {
		t.Fatalf("missing symbol %s", name)
	}
	buf := make([]byte, 8)
	n, err := unix.ProcessVMReadv(pid, []unix.Iovec{{Base: &buf[0], Len: 8}},
		[]unix.RemoteIovec{{Base: uintptr(addr), Len: 8}}, 0)
	if err != nil || n != len(buf) {
		t.Fatalf("read %s: %d bytes, %v", name, n, err)
	}
	return binary.LittleEndian.Uint64(buf)
}

func TestLinuxNativeInspectionSeizeAdmissionAndDurableHold(t *testing.T) {
	bin, addresses := buildNativeInspectionTarget(t)
	b := newBackend().(*linuxBackend)
	observed := &observedInspectionWaits{linuxWaitSource: b.waits}
	b.waits = observed
	var startup []Registers
	b.startupRegistersFn = func(tid int) (Registers, error) {
		regs, err := b.GetRegisters(tid)
		startup = append(startup, regs)
		return regs, err
	}
	var controls []linuxResumeCall
	b.ptraceSyscall6Fn = func(trap, a1, a2, a3, a4, a5, a6 uintptr) (uintptr, uintptr, syscall.Errno) {
		controls = append(controls, linuxResumeCall{request: a1, tid: a2, signal: a4})
		return syscall.Syscall6(trap, a1, a2, a3, a4, a5, a6)
	}
	pid, cmd, err := startTracedProcess(b, bin, nil, nil, "")
	t.Cleanup(func() {
		if cmd != nil {
			_ = cmd.Process.Kill()
			if err := b.reapAfterKill(); err != nil {
				t.Errorf("reap exact test-owned target: %v", err)
			}
		} else if b.failedLaunch != nil {
			if err := b.cleanupFailedLaunch(); err != nil {
				t.Errorf("cleanup partial start: %v", err)
			}
		}
		b.closeTracer()
	})
	if err != nil {
		t.Fatal(err)
	}
	b.setPID(pid)
	if len(startup) != 2 || startup[0].PC != startup[1].PC || startup[0].SP != startup[1].SP {
		t.Fatalf("entry context changed across transfer: %+v", startup)
	}
	var groupStop, seize bool
	for _, result := range observed.results {
		groupStop = groupStop || result.tid == pid && result.status.Stopped() &&
			result.status.StopSignal() == syscall.SIGSTOP && int(uint32(result.status)>>16) == unix.PTRACE_EVENT_STOP
	}
	for _, call := range controls {
		seize = seize || call.request == unix.PTRACE_SEIZE && call.tid == uintptr(pid) &&
			call.signal == uintptr(linuxPtraceOptions)
	}
	if !groupStop || !seize {
		t.Fatalf("missing exact group-stop acknowledgement or inherited option set: stops=%+v controls=%+v", observed.results, controls)
	}
	read := func(name string) uint64 {
		t.Helper()
		return nativeInspectionWord(t, pid, addresses, name)
	}

	if read("entered") != 0 {
		t.Fatal("user code escaped before initial admission")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := b.SingleStep(pid); err != nil {
		t.Fatal(err)
	}
	stop, err := b.wait(ctx)
	if err != nil || stop.Reason != StopSingleStep {
		t.Fatalf("entry step after owned group-stop clearance: %+v, %v", stop, err)
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	stop, err = b.wait(ctx)
	if err != nil || stop.Reason != StopSignal || stop.Signal != int(syscall.SIGUSR1) || stop.TID != pid {
		t.Fatalf("ready signal after clone admission: %+v, %v", stop, err)
	}
	var cloneStop bool
	for _, result := range observed.results {
		cloneStop = cloneStop || result.tid != pid && result.status.Stopped() &&
			result.status.TrapCause() == unix.PTRACE_EVENT_STOP
	}
	if !cloneStop {
		t.Fatal("no inherited SEIZE clone EVENT_STOP was observed")
	}
	tids, err := b.inspectionThreads()
	if err != nil || len(tids) != 3 || len(b.inspection.synthetic) != 2 {
		t.Fatalf("native all-TID hold lacks acknowledgements: %v, %v", tids, err)
	}
	regs := make(map[int]Registers)
	for _, tid := range tids {
		regs[tid], err = b.GetRegisters(tid)
		if err != nil {
			t.Fatal(err)
		}
	}
	before := read("heartbeat")
	time.Sleep(20 * time.Millisecond)
	again, err := b.inspectionThreads()
	if err != nil || !reflect.DeepEqual(tids, again) || read("heartbeat") != before {
		t.Fatalf("inspection hold was not durable: %v, %v", again, err)
	}
	for _, tid := range tids {
		current, err := b.GetRegisters(tid)
		if err != nil || current != regs[tid] {
			t.Fatalf("held tid %d changed context: %+v, %v", tid, current, err)
		}
	}
	foreign := tids[0]
	if foreign == pid {
		foreign = tids[1]
	}
	if err := syscall.Tgkill(pid, foreign, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	stop, err = b.wait(ctx)
	if err != nil || stop.TID != foreign || stop.Signal != int(syscall.SIGUSR2) {
		t.Fatalf("held real signal was lost or misrouted: %+v, %v", stop, err)
	}
	if b.inspection != nil {
		t.Fatal("successful primary resume retained synthetic holds")
	}
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for read("handled") != 2 || read("heartbeat") <= before {
		if time.Now().After(deadline) {
			t.Fatal("resumed threads did not deliver both real signals and make progress")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLinuxNativeInspectionAttachedHoldRestoresAndDetaches(t *testing.T) {
	bin, addresses := buildNativeInspectionTarget(t)
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	b := newBackend().(*linuxBackend)
	joined := false
	t.Cleanup(func() {
		if !joined {
			_ = cmd.Process.Kill()
			if b.attached() {
				if err := b.reapAfterKill(); err != nil {
					t.Errorf("reap exact test-owned attached target: %v", err)
				}
				_ = cmd.Process.Release()
			} else {
				_ = cmd.Wait()
			}
		}
		b.closeTracer()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	taskPath := filepath.Join("/proc", strconv.Itoa(pid), "task")
	for {
		tasks, err := os.ReadDir(taskPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(tasks) == 3 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if err := attachToProcess(b, pid); err != nil {
		t.Fatal(err)
	}
	b.recordStop(pid)
	if err := b.ContinueProcess(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Tgkill(pid, pid, syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	stop, err := b.wait(ctx)
	if err != nil || stop.Reason != StopSignal || stop.Signal != int(syscall.SIGUSR1) || stop.TID != pid {
		t.Fatalf("attached real signal stop: %+v, %v", stop, err)
	}
	tids, err := b.inspectionThreads()
	if err != nil || len(tids) != 3 || len(b.inspection.synthetic) != 2 {
		t.Fatalf("attached inspection hold: %v, %v", tids, err)
	}
	read := func(name string) uint64 { return nativeInspectionWord(t, pid, addresses, name) }
	heartbeat, handled := read("heartbeat"), read("handled")
	time.Sleep(20 * time.Millisecond)
	if read("heartbeat") != heartbeat {
		t.Fatal("attached hold released a worker between inspections")
	}
	original := make([]byte, 1)
	if err := b.ReadMemory(addresses["tick"], original); err != nil {
		t.Fatal(err)
	}
	bps := newBreakpointTable()
	if _, err := bps.set(b, "target.c", 1, addresses["tick"]); err != nil {
		t.Fatal(err)
	}
	foreign := tids[0]
	if foreign == pid {
		foreign = tids[1]
	}
	if err := syscall.Tgkill(pid, foreign, syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	if _, err := b.quiesceAttached(ctx); err != nil {
		t.Fatal(err)
	}
	if b.inspection != nil || len(b.inspectionReplay) != 0 {
		t.Fatal("detach quiescence did not adopt the inspection ownership")
	}
	if _, err := b.selectAttachedWriteTID(); err != nil {
		t.Fatal(err)
	}
	if err := bps.clearAll(b); err != nil {
		t.Fatal(err)
	}
	restored := make([]byte, 1)
	if err := b.ReadMemory(addresses["tick"], restored); err != nil || !reflect.DeepEqual(restored, original) {
		t.Fatalf("attached instruction restoration: %x, %v", restored, err)
	}
	if err := b.detachAttachedWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tid := range tids {
		raw, err := os.ReadFile(filepath.Join(taskPath, strconv.Itoa(tid), "status"))
		if err != nil || !strings.Contains(string(raw), "TracerPid:\t0\n") {
			t.Fatalf("tid %d retained its tracer: %v\n%s", tid, err, raw)
		}
	}
	for read("handled") < handled+2 || read("heartbeat") <= heartbeat {
		if ctx.Err() != nil {
			t.Fatal("detached target lost a real signal or worker progress")
		}
		time.Sleep(time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		joined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		err := <-waited
		joined = true
		t.Fatalf("detached target failed to exit normally: %v, %v", ctx.Err(), err)
	}
}
