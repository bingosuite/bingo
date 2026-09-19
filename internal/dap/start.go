package dap

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/bingosuite/bingo/internal/launch"
	"github.com/bingosuite/bingo/pkg/protocol"
)

func decodeStartConfig(data json.RawMessage) (launchConfig, error) {
	var cfg launchConfig
	if len(data) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return cfg, err
	}
	if err := validateStartNulls(fields); err != nil {
		return cfg, err
	}
	if _, ok := fields["mode"]; ok && cfg.Mode == "" {
		return cfg, fmt.Errorf("mode must be 'debug' or 'exec'")
	}
	if _, ok := fields["session"]; ok && strings.TrimSpace(cfg.Session) == "" {
		return cfg, fmt.Errorf("session must be nonempty")
	}
	if cfg.NoDebug {
		return cfg, fmt.Errorf("noDebug is not supported")
	}
	if strings.ContainsRune(cfg.Session, 0) || strings.ContainsRune(cfg.BinaryPath, 0) ||
		strings.ContainsRune(cfg.Program, 0) || strings.ContainsRune(cfg.Cwd, 0) {
		return cfg, fmt.Errorf("launch and attach paths or session IDs must not contain NUL")
	}
	return cfg, launch.ValidateArguments(cfg.Args, cfg.Env)
}

func validateStartNulls(fields map[string]json.RawMessage) error {
	for _, name := range []string{"program", "mode", "cwd", "pid", "session", "binaryPath", "stopOnEntry", "noDebug"} {
		if value, ok := fields[name]; ok && string(value) == "null" {
			return fmt.Errorf("%s must not be null", name)
		}
	}
	for _, name := range []string{"args", "env"} {
		if value, ok := fields[name]; ok {
			var elements []json.RawMessage
			if err := json.Unmarshal(value, &elements); err != nil {
				return err
			}
			for _, element := range elements {
				if string(element) == "null" {
					return fmt.Errorf("%s elements must be strings, not null", name)
				}
			}
		}
	}
	return nil
}

func prepareLaunch(cfg launchConfig) (launchConfig, error) {
	if cfg.Session != "" || cfg.PID != nil || cfg.BinaryPath != "" {
		return cfg, fmt.Errorf("launch cannot include session, pid, or binaryPath; use attach")
	}
	if cfg.Mode != "" && cfg.Mode != "exec" && cfg.Mode != "debug" {
		return cfg, fmt.Errorf("mode must be 'debug' or 'exec'")
	}
	if cfg.Program == "" {
		return cfg, fmt.Errorf("launch requires 'program'")
	}
	program, cwd, err := launch.Resolve(cfg.Program, cfg.Cwd)
	if err != nil {
		return cfg, err
	}
	cfg.Program, cfg.Cwd = program, cwd
	if cfg.Mode == "debug" {
		info, err := os.Stat(program)
		if err != nil {
			return cfg, fmt.Errorf("source program: %w", err)
		}
		if !info.IsDir() {
			return cfg, fmt.Errorf("source program must be a local Go package directory")
		}
		if cwd == "" {
			cfg.Cwd = program
		}
	}
	return cfg, nil
}

func validateAttach(cfg launchConfig) error {
	if cfg.Mode != "" || cfg.Cwd != "" || len(cfg.Args) > 0 || len(cfg.Env) > 0 {
		return fmt.Errorf("mode, cwd, args, and env are launch-only arguments")
	}
	if cfg.Session != "" {
		if cfg.PID != nil || cfg.Program != "" || cfg.BinaryPath != "" {
			return fmt.Errorf("session join cannot include pid, program, or binaryPath")
		}
		return nil
	}
	if cfg.PID == nil || *cfg.PID <= 0 || *cfg.PID > math.MaxInt32 {
		return fmt.Errorf("attach requires a positive 'pid' (or 'session' to join an existing bingo session)")
	}
	if cfg.Program != "" && cfg.BinaryPath != "" {
		return fmt.Errorf("attach accepts binaryPath or legacy program, not both")
	}
	return nil
}

// Claim before mutating handshake state: a duplicate launch or join must not
// overwrite the request or flags still owned by the first startup.
func (h *Handler) claimStart(seq int, command string, cfg launchConfig, joining bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx.Err() != nil || h.terminating || h.terminated {
		return fmt.Errorf("debug connection is closing")
	}
	if h.session != nil || h.sessionStarting || h.startReqSeq != 0 || h.launching || h.joining {
		return fmt.Errorf("session already started for this connection")
	}
	h.startReqSeq, h.startCmd = seq, command
	h.stopOnEntry = cfg.StopOnEntry
	h.attached = command == "attach"
	h.joining, h.awaitingWelcome = joining, joining
	h.launching = !joining
	return nil
}

func (h *Handler) abandonStart(seq int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.startReqSeq == seq {
		h.startReqSeq = 0
		h.launching, h.joining, h.awaitingWelcome = false, false, false
	}
}

func (h *Handler) enqueueStart(kind protocol.CommandKind, payload any) {
	cmd, err := marshalCommand(kind, payload)
	if err != nil {
		h.failStart(err.Error())
		return
	}
	h.mu.Lock()
	if h.ctx.Err() == nil && !h.terminating && !h.terminated {
		h.queueCommandLocked(cmd)
	}
	h.mu.Unlock()
	h.flushCommands()
}
