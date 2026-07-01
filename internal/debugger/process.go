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

func (p *process) launch(b Backend, binaryPath string, args []string, env []string) error {
	if p.live {
		return ErrAlreadyRunning
	}
	// codeql-suppress[go/path-injection]: The debugger must stat the operator-selected local binary before launching it.
	if _, err := os.Stat(binaryPath); err != nil {
		return fmt.Errorf("launch: %w", err)
	}

	pid, cmd, err := startTracedProcess(b, binaryPath, args, env)
	if err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	p.pid = pid
	p.cmd = cmd
	p.live = true
	return nil
}

func (p *process) attach(b Backend, pid int) error {
	if p.live {
		return ErrAlreadyRunning
	}
	if err := attachToProcess(b, pid); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	p.pid = pid
	p.cmd = nil
	p.live = true
	return nil
}

// kill terminates the tracee. The Backend argument lets platform kill paths run
// PTRACE_DETACH on the tracer thread; the engine's Kill path also runs
// bps.clearAll.
func (p *process) kill(b Backend) error {
	if !p.live {
		return nil
	}
	if p.pid == 0 {
		p.live = false
		return nil
	}
	p.live = false
	if err := killProcess(b, p.pid, p.cmd); err != nil {
		return fmt.Errorf("kill: %w", err)
	}
	return nil
}
