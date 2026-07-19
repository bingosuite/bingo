//go:build darwin && arm64 && bingonative

package debugger

import (
	"fmt"
	"syscall"
)

// stopProcessSignal sends sig to pid. It is the darwin backend's Pause
// interrupt primitive (StopProcess passes darwinPauseSignal); the linux backend
// directs its own SIGSTOP at the main thread via tgkill and never calls this, so
// the helper is darwin-only and lives in a darwin-tagged file to avoid an
// unused-symbol lint on linux. syscall.Kill is used directly instead of
// os.FindProcess(pid).Signal: os.FindProcess never fails to find a process on
// Unix (it just wraps the pid), so that pairing never actually distinguished
// "no such process" from any other error. ESRCH (process already gone) is
// treated as an idempotent no-op, matching process.kill's idempotency.
func stopProcessSignal(pid int, sig syscall.Signal) error {
	if pid == 0 {
		return fmt.Errorf("StopProcess: no process")
	}
	if err := syscall.Kill(pid, sig); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("StopProcess: %w", err)
	}
	return nil
}
