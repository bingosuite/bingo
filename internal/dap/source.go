package dap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bingosuite/bingo/internal/launch"
	"github.com/bingosuite/bingo/pkg/protocol"
)

const (
	sourceBuildTimeout = 2 * time.Minute
	buildOutputLimit   = 64 * 1024
	buildOutputCut     = "\n[build output truncated]\n"
)

type sourceArtifact struct {
	dir         string
	program     string
	diagnostics string
}

func (a *sourceArtifact) remove() error {
	if err := os.RemoveAll(a.dir); err != nil {
		return fmt.Errorf("remove Go build directory %q: %w", a.dir, err)
	}
	return nil
}

type sourceBuilder struct {
	tempDir string
	run     func(*exec.Cmd) error
}

func (b sourceBuilder) build(ctx context.Context, cfg launchConfig) (artifact *sourceArtifact, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("Go build: %w", err)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("Go build requires 'go' on the server PATH: %w", err)
	}
	dir, err := os.MkdirTemp(b.tempDir, "bingo-dap-build-")
	if err != nil {
		return nil, fmt.Errorf("create Go build directory: %w", err)
	}
	owned := &sourceArtifact{dir: dir, program: filepath.Join(dir, "debuggee")}
	defer func() {
		if err != nil {
			err = errors.Join(err, owned.remove())
		}
	}()

	// A compiler/linker belongs to this build, never to the shared debugger
	// server. Killing only the go driver would leave children writing the
	// supposedly retired output directory after cancellation.
	cmd := exec.CommandContext(ctx, goTool, "build", "-gcflags=all=-N -l", "-o", owned.program, ".")
	cmd.Dir = cfg.Program
	env := append([]string(nil), cfg.Env...)
	env = append(env, "GOTMPDIR="+dir, "TMPDIR="+dir)
	cmd.Env = launch.Environment(cfg.Program, env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	output := &buildOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	run := b.run
	if run == nil {
		run = (*exec.Cmd).Run
	}
	runErr := run(cmd)
	if ctx.Err() != nil {
		runErr = errors.Join(ctx.Err(), runErr)
	}
	if runErr != nil {
		return nil, fmt.Errorf("Go build: %w%s", runErr, output.diagnosticSuffix())
	}
	info, err := os.Stat(owned.program)
	if err != nil {
		return nil, fmt.Errorf("Go build output: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("Go build: program must be a main package producing an executable")
	}
	owned.diagnostics = output.text()
	return owned, nil
}

type buildOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (o *buildOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := min(len(p), buildOutputLimit-len(buildOutputCut)-len(o.data))
	o.data = append(o.data, p[:n]...)
	o.truncated = o.truncated || n < len(p)
	return len(p), nil
}

func (o *buildOutput) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	text := strings.ToValidUTF8(string(o.data), "?")
	if o.truncated {
		text += buildOutputCut
	}
	return text
}

func (o *buildOutput) diagnosticSuffix() string {
	if text := o.text(); text != "" {
		return "\n" + text
	}
	return ""
}

// Artifact retirement is session-owned, not connection-owned. A disconnected
// driver can leave observers and a restartable hub alive, and Hub.Done also
// waits for retained native cleanup before the binary may be unlinked.
type sourceArtifacts struct {
	wg sync.WaitGroup
}

func (a *sourceArtifacts) retain(sess Session, artifact *sourceArtifact, log *slog.Logger) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		<-sess.Done()
		if err := artifact.remove(); err != nil {
			log.Error("dap: retire source build", "session", sess.SessionID(), "err", err)
		}
	}()
}

type sourceBuild struct {
	ctx       context.Context
	cancel    context.CancelFunc
	submitted bool
}

func (h *Handler) startSourceBuild(cfg launchConfig) {
	h.mu.Lock()
	if h.ctx.Err() != nil || h.terminating || h.terminated {
		h.mu.Unlock()
		return
	}
	if h.session.Done() == nil {
		h.mu.Unlock()
		h.failStart("source launch requires a session completion signal")
		_ = h.Close()
		return
	}
	ctx, cancel := context.WithTimeout(h.ctx, sourceBuildTimeout)
	build := &sourceBuild{ctx: ctx, cancel: cancel}
	h.sourceBuild = build
	h.builds.Add(1)
	h.mu.Unlock()
	go h.finishSourceBuild(build, cfg)
}

func (h *Handler) finishSourceBuild(build *sourceBuild, cfg launchConfig) {
	defer h.builds.Done()
	defer build.cancel()
	artifact, err := h.buildSource(build.ctx, cfg)
	if err == nil {
		err = build.ctx.Err()
	}
	if err == nil && artifact == nil {
		err = fmt.Errorf("Go build returned no executable")
	}
	if err == nil {
		if artifact.diagnostics != "" {
			h.emitConsole(artifact.diagnostics)
		}
		cmd, marshalErr := marshalCommand(protocol.CmdLaunch, protocol.LaunchPayload{
			Program: artifact.program, Args: cfg.Args, Env: cfg.Env, Cwd: cfg.Cwd,
		})
		err = marshalErr
		if err == nil {
			h.mu.Lock()
			if build.ctx.Err() == nil && !h.terminating && !h.terminated {
				h.artifacts.retain(h.session, artifact, h.log)
				h.queueCommandLocked(cmd)
				build.submitted = true
				artifact = nil
			} else {
				err = fmt.Errorf("source launch canceled before launch")
			}
			h.mu.Unlock()
		}
	}
	if artifact != nil {
		err = errors.Join(err, artifact.remove())
	}
	if err != nil {
		h.log.Warn("dap: source launch failed", "err", err)
		h.failStart(err.Error())
		_ = h.Close()
		return
	}
	h.flushCommands()
}
