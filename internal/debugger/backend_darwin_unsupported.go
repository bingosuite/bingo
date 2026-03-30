//go:build darwin && !bingonative

package debugger

// backend_darwin_stub.go provides stub implementations of all symbols that
// backend_darwin_amd64.go and backend_darwin_arm64.go define when built with
// the bingonative tag.
//
// This file compiles on Darwin WITHOUT the bingonative tag, which means it
// also satisfies gopls running on any platform — gopls sees this file as the
// Darwin backend when bingonative is not set, and never tries to load the
// real cgo files.
//
// To build the real Darwin backend on a Mac:
//   go build -tags bingonative ./...
//   go test -tags bingonative ./...
//
// On Linux/Windows this file is also excluded (darwin tag), but that is fine
// because backend_linux.go and backend_windows.go already provide newBackend,
// startTracedProcess, attachToProcess, killProcess, and isAlreadyExited for
// those platforms.

import (
	"fmt"
	"os/exec"
)

func newBackend() Backend {
	panic("bingo: Darwin native backend requires building with -tags bingonative on a Mac")
}

func startTracedProcess(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	return 0, nil, fmt.Errorf("Darwin native backend requires -tags bingonative")
}

func attachToProcess(pid int) error {
	return fmt.Errorf("Darwin native backend requires -tags bingonative")
}

func killProcess(pid int, cmd *exec.Cmd) error {
	return fmt.Errorf("Darwin native backend requires -tags bingonative")
}

func isAlreadyExited(err error) bool {
	return err != nil && err.Error() == "os: process already finished"
}
