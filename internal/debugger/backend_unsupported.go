//go:build !(linux && amd64) && !(darwin && arm64 && bingonative)

package debugger

import (
	"fmt"
	"os/exec"
	"runtime"
)

// This file is the fallback for every GOOS/GOARCH combination bingo does not
// ship a native backend for (see backend_linux_amd64.go and
// backend_darwin_arm64.go). Without it, unsupported platforms fail the build
// with a cryptic "undefined: newBackend" from the compiler. Instead we build
// cleanly everywhere and fail loudly, with an actionable message, the moment
// New() actually tries to construct a backend.
//
// Note this also catches darwin/arm64 built without the required
// "bingonative" tag (and entitlement) — that combination has no working
// Mach-based backend either.

func unsupportedPlatformError() error {
	return fmt.Errorf(
		"bingo: unsupported platform %s/%s (only linux/amd64 and darwin/arm64 with -tags bingonative are supported)",
		runtime.GOOS, runtime.GOARCH,
	)
}

func newBackend() Backend {
	panic(unsupportedPlatformError())
}

func startTracedProcess(_ Backend, _ string, _ []string, _ []string) (int, *exec.Cmd, error) {
	return 0, nil, unsupportedPlatformError()
}

func attachToProcess(_ Backend, _ int) error {
	return unsupportedPlatformError()
}

func killProcess(_ Backend, _ int, _ *exec.Cmd) error {
	return unsupportedPlatformError()
}
