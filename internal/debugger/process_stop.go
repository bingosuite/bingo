//go:build darwin && arm64 && bingonative

package debugger

import (
	"fmt"
	"syscall"
)

// stopProcessSignal sends a whole-thread-group SIGSTOP to pid. It is the darwin
// backend's StopProcess() mechanism; the linux backend overrides StopProcess to
// direct the signal at the main thread via tgkill (see backend_linux_amd64.go
// for why), so this helper is darwin-only and lives in a darwin-tagged file to
// avoid an unused-symbol lint on linux. syscall.Kill is used directly instead of
// os.FindProcess(pid).Signal: os.FindProcess never fails to find a process on
// Unix (it just wraps the pid), so that pairing never actually distinguished
// "no such process" from any other error. ESRCH (process already gone) is
// treated as an idempotent no-op, matching process.kill's idempotency.
// NOTE: on darwin this is Pause *groundwork* only — macOS delivers a SIGSTOP to
// a ptraced tracee as a job-control stop that our wait4 loop never reports, so
// it does not yet surface as EventPaused. A functional darwin Pause needs Mach
// thread_suspend instead; see AGENTS.md → Pause (darwin caveat).
func stopProcessSignal(pid int) error {
	if pid == 0 {
		return fmt.Errorf("StopProcess: no process")
	}
	if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("StopProcess: %w", err)
	}
	return nil
}
