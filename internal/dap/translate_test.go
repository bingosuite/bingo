package dap

import (
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestThreadIDClampsToOne(t *testing.T) {
	cases := map[int]int{-5: 1, 0: 1, 1: 1, 7: 7}
	for in, want := range cases {
		if got := threadID(in); got != want {
			t.Errorf("threadID(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestStoppedReason(t *testing.T) {
	cases := map[protocol.EventKind]string{
		protocol.EventBreakpointHit: "breakpoint",
		protocol.EventStepped:       "step",
		protocol.EventPanic:         "exception",
		protocol.EventPaused:        "pause",
		protocol.EventProcessExited: "pause", // fallback
	}
	for kind, want := range cases {
		if got := stoppedReason(kind); got != want {
			t.Errorf("stoppedReason(%s) = %q, want %q", kind, got, want)
		}
	}
}

func TestFrameIDRoundTrip(t *testing.T) {
	for idx := 0; idx < 5; idx++ {
		ref := frameID(idx)
		if ref == 0 {
			t.Errorf("frameID(%d) = 0, must be non-zero", idx)
		}
		if back := frameIndexFromRef(ref); back != idx {
			t.Errorf("frameIndexFromRef(frameID(%d)) = %d, want %d", idx, back, idx)
		}
	}
}

func TestDapSourceNilOnEmpty(t *testing.T) {
	if s := dapSource(protocol.Location{}); s != nil {
		t.Errorf("dapSource(empty) = %+v, want nil", s)
	}
	s := dapSource(protocol.Location{File: "/abs/path/main.go", Line: 10})
	if s == nil || s.Path != "/abs/path/main.go" || s.Name != "main.go" {
		t.Errorf("dapSource = %+v, want Path=/abs/path/main.go Name=main.go", s)
	}
}

func TestDapStackFrames(t *testing.T) {
	frames := []protocol.Frame{
		{Index: 0, Location: protocol.Location{Function: "main.inner", File: "/x/main.go", Line: 3}},
		{Index: 1, Location: protocol.Location{Function: "", File: "/x/main.go", Line: 9}},
	}
	out := dapStackFrames(frames)
	if len(out) != 2 {
		t.Fatalf("got %d frames, want 2", len(out))
	}
	if out[0].Id != 1 || out[0].Name != "main.inner" || out[0].Line != 3 {
		t.Errorf("frame 0 = %+v", out[0])
	}
	if out[1].Id != 2 || out[1].Name != "?" {
		t.Errorf("frame 1 = %+v (want Id=2 Name=?)", out[1])
	}
}

func TestDapThreadsSyntheticWhenEmpty(t *testing.T) {
	out := dapThreads(nil)
	if len(out) != 1 || out[0].Id != 1 || out[0].Name != "main" {
		t.Errorf("dapThreads(nil) = %+v, want [{1 main}]", out)
	}
}

func TestDapThreadsMapsGoroutines(t *testing.T) {
	out := dapThreads([]protocol.Goroutine{
		{ID: 0, Status: "running"},
		{ID: 18, Status: "waiting"},
	})
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].Id != 1 { // clamped
		t.Errorf("goroutine 0 threadId = %d, want 1", out[0].Id)
	}
	if out[1].Id != 18 {
		t.Errorf("goroutine 18 threadId = %d, want 18", out[1].Id)
	}
}

func TestDapVariables(t *testing.T) {
	out := dapVariables([]protocol.Variable{{Name: "x", Value: "0x1", Type: "int"}})
	if len(out) != 1 || out[0].Name != "x" || out[0].Value != "0x1" || out[0].VariablesReference != 0 {
		t.Errorf("dapVariables = %+v", out)
	}
}

func TestMarshalCommandNilPayload(t *testing.T) {
	b, err := marshalCommand(protocol.CmdContinue, nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := protocol.UnmarshalCommand(b)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Kind != protocol.CmdContinue {
		t.Errorf("kind = %q, want %q", cmd.Kind, protocol.CmdContinue)
	}
	if string(cmd.Payload) != "{}" {
		t.Errorf("payload = %q, want {}", string(cmd.Payload))
	}
	if cmd.Version != protocol.Version {
		t.Errorf("version = %q, want %q", cmd.Version, protocol.Version)
	}
}

func TestMarshalCommandWithPayload(t *testing.T) {
	b, err := marshalCommand(protocol.CmdSetBreakpoint, protocol.SetBreakpointPayload{File: "main.go", Line: 12})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := protocol.UnmarshalCommand(b)
	if err != nil {
		t.Fatal(err)
	}
	var p protocol.SetBreakpointPayload
	if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
		t.Fatal(err)
	}
	if p.File != "main.go" || p.Line != 12 {
		t.Errorf("payload = %+v, want {main.go 12}", p)
	}
}
