package hub_test

import (
	"testing"

	"github.com/bingosuite/bingo/internal/debugger"
)

func TestLegacyDebuggerLaunchOptions(t *testing.T) {
	d := newFakeDebugger()
	if err := debugger.LaunchWithOptions(d, "/app", nil, nil, debugger.LaunchOptions{Cwd: "/work"}); err == nil {
		t.Fatal("legacy debugger silently ignored cwd")
	}
	if len(d.recordedCalls()) != 0 {
		t.Fatal("unsupported cwd reached the legacy Launch")
	}
	if err := debugger.LaunchWithOptions(d, "/app", nil, nil, debugger.LaunchOptions{}); err != nil {
		t.Fatalf("legacy launch failed: %v", err)
	}
	if got := d.recordedCalls(); len(got) != 1 || got[0] != "Launch" {
		t.Fatalf("legacy launch calls = %v", got)
	}
}
