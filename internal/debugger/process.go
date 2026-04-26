package debugger

import (
	"fmt"
	"os"
	"os/exec"
)

// process tracks the OS handle for the tracee, with platform-specific hooks
// (startTracedProcess, attachToProcess, killProcess) defined per OS.
type process struct {
	pid  int
	cmd  *exec.Cmd // non-nil for launched (not attached) processes
	live bool
}

func (p *process) launch(binaryPath string, args []string, env []string) error {
	if p.live {
		return ErrAlreadyRunning
	}
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("launch: %w", err)
	}

	pid, cmd, err := startTracedProcess(binaryPath, args, env)
	if err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	p.pid = pid
	p.cmd = cmd
	p.live = true
	return nil
}

func (p *process) attach(pid int) error {
	if p.live {
		return ErrAlreadyRunning
	}
	if err := attachToProcess(pid); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	p.pid = pid
	p.cmd = nil
	p.live = true
	return nil
}

// kill terminates the tracee. The Backend argument is unused but kept for
// interface symmetry with the engine's Kill path which also runs bps.clearAll.
func (p *process) kill(_ Backend) error {
	if !p.live {
		return nil
	}
	if p.pid == 0 {
		p.live = false
		return nil
	}
	p.live = false
	if err := killProcess(p.pid, p.cmd); err != nil {
		return fmt.Errorf("kill: %w", err)
	}
	return nil
}
