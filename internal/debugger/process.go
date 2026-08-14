package debugger

import (
	"fmt"
	"os"
	"os/exec"
)

// process tracks the OS handle for the tracee, with platform-specific hooks
// (startTracedProcess, attachToProcess, killProcess) defined per OS.
type process struct {
	pid        int
	cmd        *exec.Cmd // non-nil for launched (not attached) processes
	live       bool
	isAttached bool
}

type attachedBackendDetacher interface {
	detachAttached() error
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
	p.isAttached = false
	return nil
}

func (p *process) attach(b Backend, pid int) error {
	if p.live {
		return ErrAlreadyRunning
	}
	if err := attachToProcess(b, pid); err != nil {
		if detacher, ok := b.(interface{ retainsAttachedOwnership() bool }); ok &&
			detacher.retainsAttachedOwnership() {
			p.pid = pid
			p.cmd = nil
			p.live = true
			p.isAttached = true
		}
		return fmt.Errorf("attach: %w", err)
	}
	p.pid = pid
	p.cmd = nil
	p.live = true
	p.isAttached = true
	return nil
}

func (p *process) attached() bool {
	return p.live && p.isAttached
}

func (p *process) markExited() {
	p.pid = 0
	p.cmd = nil
	p.live = false
	p.isAttached = false
}

// kill terminates the tracee. The Backend argument lets platform kill paths run
// PTRACE_DETACH on the tracer thread; the engine's Kill path also runs
// bps.clearAll. running reports whether a waitLoop is already consuming the
// Linux backend's routed status queue.
func (p *process) kill(b Backend, running bool) error {
	if !p.live {
		return nil
	}
	if p.pid == 0 {
		p.live = false
		p.isAttached = false
		return nil
	}
	var err error
	if p.isAttached {
		if detacher, ok := b.(attachedBackendDetacher); ok {
			err = detacher.detachAttached()
		} else {
			err = killProcess(b, p.pid, p.cmd, running)
		}
	} else {
		err = killProcess(b, p.pid, p.cmd, running)
	}
	if err != nil {
		return fmt.Errorf("kill: %w", err)
	}
	p.live = false
	p.isAttached = false
	p.pid = 0
	p.cmd = nil
	return nil
}
