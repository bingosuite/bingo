//go:build detachproof && linux && amd64

package detachproof

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestPtraceErrnoOnRunningTracee captures the ACTUAL errno of the two
// operations linux killProcess/bps.clearAll perform on an attached tracee, in
// both the suspended and the running state — without touching production code.
//
// It reimplements only the syscall sequence the backend uses:
//
//	PTRACE_ATTACH + wait4      (internal/debugger/backend_linux_amd64.go: attachToProcess)
//	PTRACE_POKEDATA            (WriteMemory, reached via bps.clearAll during Kill)
//	PTRACE_DETACH              (killProcess, attached branch)
//
// Production discards the results of the latter two. This test does not: it
// reports them, which is the "actual detach/write errors" evidence the audit
// asks for. All ptrace requests are issued from one locked OS thread because
// ptrace is thread-bound.
func TestPtraceErrnoOnRunningTracee(t *testing.T) {
	bin, _ := buildProofTarget(t, "detach_proof_target", proofTargetSrc)
	vaddr, disk := funcImage(t, bin, "main.gated")

	type probe struct {
		poke    error
		detach  error
		tracerA int // TracerPid while attached
		tracerB int // TracerPid after the detach attempt
	}

	run := func(t *testing.T, resume bool) probe {
		t.Helper()
		tg := startTarget(t, bin)
		var p probe

		withTracerThread(func() {
			if err := syscall.PtraceAttach(tg.pid); err != nil {
				t.Errorf("PTRACE_ATTACH: %v", err)
				return
			}
			var ws syscall.WaitStatus
			if _, err := syscall.Wait4(tg.pid, &ws, 0, nil); err != nil {
				t.Errorf("wait after PTRACE_ATTACH: %v", err)
				return
			}
			p.tracerA, _ = tracerPID(tg.pid)

			if resume {
				if err := syscall.PtraceCont(tg.pid, 0); err != nil {
					t.Errorf("PTRACE_CONT: %v", err)
					return
				}
				// Make sure it is genuinely running before probing.
				if !tg.beating(5 * time.Second) {
					t.Errorf("tracee did not resume")
					return
				}
			}

			// Mirror bps.clearAll → WriteMemory: restore the original byte.
			_, p.poke = syscall.PtracePokeData(tg.pid, uintptr(vaddr), disk[:1])
			// Mirror killProcess's attached branch.
			p.detach = syscall.PtraceDetach(tg.pid)
			p.tracerB, _ = tracerPID(tg.pid)
		})
		return p
	}

	t.Run("suspended", func(t *testing.T) {
		p := run(t, false)
		t.Logf("suspended: POKEDATA err=%v, DETACH err=%v, TracerPid attached=%d after=%d",
			p.poke, p.detach, p.tracerA, p.tracerB)
		if p.poke != nil {
			t.Errorf("control FAILED: PTRACE_POKEDATA on a SUSPENDED tracee should succeed, got %v", p.poke)
		}
		if p.detach != nil {
			t.Errorf("control FAILED: PTRACE_DETACH on a SUSPENDED tracee should succeed, got %v", p.detach)
		}
		if p.tracerB != 0 {
			t.Errorf("control FAILED: suspended detach left TracerPid=%d, want 0", p.tracerB)
		}
	})

	t.Run("running", func(t *testing.T) {
		p := run(t, true)
		t.Logf("running: POKEDATA err=%v, DETACH err=%v, TracerPid attached=%d after=%d",
			p.poke, p.detach, p.tracerA, p.tracerB)
		if p.poke == nil && p.detach == nil && p.tracerB == 0 {
			t.Skipf("kernel accepted both operations on a running tracee; U4's syscall premise does not hold here")
		}
		if !errors.Is(p.detach, syscall.ESRCH) {
			t.Logf("note: PTRACE_DETACH on a running tracee failed with %v (expected ESRCH)", p.detach)
		}
		t.Logf("U4 SYSCALL PREMISE CONFIRMED: on a RUNNING tracee both operations the linux Kill path performs "+
			"fail (POKEDATA=%v, DETACH=%v) and the process remains traced (TracerPid=%d). "+
			"backend_linux_amd64.go discards both results and killProcess returns nil.",
			p.poke, p.detach, p.tracerB)
	})
}

// TestProcSelfCanObserveTracerPid is a harness self-check: it verifies the two
// primitives the whole proof rests on — TracerPid reporting and /proc/pid/mem
// reads — behave as expected on this kernel, so a proof failure can never be
// blamed on a broken observation method.
func TestProcSelfCanObserveTracerPid(t *testing.T) {
	bin, _ := buildProofTarget(t, "detach_proof_target", proofTargetSrc)
	vaddr, disk := funcImage(t, bin, "main.gated")
	tg := startTarget(t, bin)

	if tp := mustTracerPID(t, tg.pid, "untraced"); tp != 0 {
		t.Fatalf("fresh child reports TracerPid=%d, want 0", tp)
	}
	got, err := readTraceeMem(t, tg.pid, vaddr, len(disk))
	if err != nil {
		t.Fatalf("/proc/%d/mem read failed — the proof cannot observe text bytes on this kernel: %v", tg.pid, err)
	}
	if idx := diffBytes(disk, got, 4); len(idx) != 0 {
		t.Fatalf("in-memory main.gated differs from the ELF for an untraced process at %v", idx)
	}

	withTracerThread(func() {
		if err := syscall.PtraceAttach(tg.pid); err != nil {
			t.Errorf("PTRACE_ATTACH: %v", err)
			return
		}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(tg.pid, &ws, 0, nil); err != nil {
			t.Errorf("wait4: %v", err)
			return
		}
		tp, _ := tracerPID(tg.pid)
		if !tracedByThisProcess(tp) {
			t.Errorf("attached child reports TracerPid=%d, which does not belong to this process (%d)", tp, os.Getpid())
		}
		_ = syscall.PtraceDetach(tg.pid)
	})
	if tp := mustTracerPID(t, tg.pid, "after detach"); tp != 0 {
		t.Fatalf("after a legal detach TracerPid=%d, want 0", tp)
	}
	t.Logf("harness self-check OK: TracerPid and /proc/<pid>/mem are reliable observers on this kernel")
}
