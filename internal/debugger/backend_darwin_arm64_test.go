//go:build darwin && arm64 && bingonative

package debugger

import (
	"strings"
	"testing"
)

func TestTaskPortAccessError(t *testing.T) {
	message := taskPortAccessError(1234, "(os/kern) failure").Error()
	for _, detail := range []string{
		"task_for_pid(1234): (os/kern) failure",
		"does not by itself prove a missing entitlement",
		"Developer Tools",
		"DevToolsSecurity -status",
		"reloading the editor does not replace a shared running server",
	} {
		if !strings.Contains(message, detail) {
			t.Errorf("missing %q in %q", detail, message)
		}
	}
}
