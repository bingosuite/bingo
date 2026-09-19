package launch

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Resolve fixes relative paths before either a compiler or tracee changes its
// working directory. Empty cwd preserves the server's inherited directory.
func Resolve(program, cwd string) (string, string, error) {
	if strings.TrimSpace(program) == "" || strings.ContainsRune(program, 0) {
		return "", "", fmt.Errorf("program must be a nonempty local path without NUL")
	}
	if strings.ContainsRune(cwd, 0) {
		return "", "", fmt.Errorf("cwd must not contain NUL")
	}
	if cwd != "" {
		absolute, err := filepath.Abs(cwd)
		if err != nil {
			return "", "", fmt.Errorf("resolve cwd: %w", err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return "", "", fmt.Errorf("cwd: %w", err)
		}
		if !info.IsDir() {
			return "", "", fmt.Errorf("cwd %q is not a directory", cwd)
		}
		cwd = absolute
		if !filepath.IsAbs(program) {
			program = filepath.Join(cwd, program)
		}
	}
	absolute, err := filepath.Abs(program)
	if err != nil {
		return "", "", fmt.Errorf("resolve program: %w", err)
	}
	return absolute, cwd, nil
}

func ValidateArguments(args, env []string) error {
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("launch argument must not contain NUL")
		}
	}
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, 0) {
			return fmt.Errorf("launch environment entries must be KEY=VALUE without NUL")
		}
	}
	return nil
}

// Environment uses exec's last-value-wins semantics on both native backends.
// PWD must describe the requested child directory, not a stale inherited value
// (Go's os.Getwd may otherwise trust the wrong spelling or directory).
func Environment(cwd string, overrides []string) []string {
	cmd := &exec.Cmd{Dir: cwd}
	cmd.Env = append(cmd.Environ(), overrides...)
	if cwd != "" {
		cmd.Env = append(cmd.Env, "PWD="+cwd)
	}
	return cmd.Environ()
}
