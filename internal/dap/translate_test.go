package dap

import (
	"encoding/json"
	"strings"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestStoppedThreadIDOmittedWhenUnknown(t *testing.T) {
	cases := map[int]int{-5: 0, 0: 0, 1: 1, 7: 7}
	for in, want := range cases {
		if got := stoppedThreadID(in); got != want {
			t.Errorf("stoppedThreadID(%d) = %d, want %d", in, got, want)
		}
	}
	raw, err := json.Marshal(godap.StoppedEventBody{
		Reason:   "pause",
		ThreadId: stoppedThreadID(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "threadId") {
		t.Fatalf("unknown stopped body = %s, want threadId omitted", raw)
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

func TestInspectionHandlesRemainOpaqueAcrossStops(t *testing.T) {
	h := &Handler{}
	first, err := h.allocVarRef()
	if err != nil {
		t.Fatal(err)
	}
	h.resetVarsLocked()
	second, err := h.allocVarRef()
	if err != nil || second <= first || first <= varRefBase {
		t.Fatalf("handles = %d, %d: %v", first, second, err)
	}
	h.inspectionObjects = maxInspectionObjects
	if _, err := h.allocVarRef(); err == nil {
		t.Fatal("handle budget was not enforced")
	}
	h.resetVarsLocked()
	h.nextVarRef = 1<<53 - 1
	if _, err := h.allocVarRef(); err == nil {
		t.Fatal("safe integer handle limit was not enforced")
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
	if out[0].Id != 0 || out[0].Name != "main.inner" || out[0].Line != 3 {
		t.Errorf("frame 0 = %+v", out[0])
	}
	if out[1].Id != 0 || out[1].Name != "?" {
		t.Errorf("frame 1 = %+v (want unallocated ID, Name=?)", out[1])
	}
}

func TestDapThreadsHaveDistinctSyntheticAndRealHandles(t *testing.T) {
	h := &Handler{}
	gs := []protocol.Goroutine{{ID: 1, Status: "waiting"}}
	out, err := h.inspectionThreadsLocked(gs, true)
	if err != nil || len(out) != 1 || out[0].GoroutineID != 0 || out[0].Id <= 0 {
		t.Fatalf("synthetic current: %+v: %v", out, err)
	}
	synthetic := out[0].Id
	out, err = h.inspectionThreadsLocked(gs, false)
	if err != nil || len(out) != 2 || out[0].Id != synthetic ||
		out[1].GoroutineID != 1 || out[1].Id == synthetic || out[1].Id == 1 {
		t.Fatalf("synthetic alongside real g1: %+v: %v", out, err)
	}
	oldReal := out[1].Id
	assertResolvedInspectionCurrent(t, h, oldReal)
	assertSyntheticThreadMetadata(t, synthetic)
	assertInspectionThreadRetired(t, h, oldReal)
}

func assertResolvedInspectionCurrent(t *testing.T, h *Handler, oldReal int) {
	t.Helper()
	out, err := h.inspectionThreadsLocked([]protocol.Goroutine{{ID: 1, Current: true}}, true)
	if err != nil || len(out) != 1 || out[0].Id != oldReal ||
		h.curThreadID != oldReal || h.curGoroutineID != 1 || h.stopThreadUnknown {
		t.Fatalf("resolved current: %+v: %v", out, err)
	}
}

func assertSyntheticThreadMetadata(t *testing.T, synthetic int) {
	t.Helper()
	raw, err := json.Marshal(bingoThread{Thread: godap.Thread{Id: synthetic}})
	if err != nil || strings.Contains(string(raw), "bingoGoroutineId") {
		t.Fatalf("synthetic claims a real goid: %s: %v", raw, err)
	}
}

func assertInspectionThreadRetired(t *testing.T, h *Handler, oldReal int) {
	t.Helper()
	h.resetVarsLocked()
	if _, exists := h.threadHandles[oldReal]; exists {
		t.Fatal("new stop retained old thread handle")
	}
	fresh, err := h.threadForGoroutineLocked(1)
	if err != nil || fresh <= oldReal {
		t.Fatalf("new stop reused thread handle %d: %d: %v", oldReal, fresh, err)
	}
}

func TestBuildVarTree(t *testing.T) {
	h := &Handler{varCache: make(map[int][]godap.Variable)}
	out, err := h.buildVarTree([]protocol.Variable{
		{Name: "x", Value: "42", Type: "int"},
		{Name: "p", Value: "main.Point{...}", Type: "main.Point", Children: []protocol.Variable{
			{Name: "X", Value: "1", Type: "int"},
			{Name: "Y", Value: "2", Type: "int"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d vars, want 2", len(out))
	}
	if out[0].Name != "x" || out[0].Value != "42" || out[0].VariablesReference != 0 {
		t.Errorf("leaf var = %+v, want ref 0", out[0])
	}
	ref := out[1].VariablesReference
	if ref < varRefBase {
		t.Fatalf("struct var ref = %d, want a child ref >= %d", ref, varRefBase)
	}
	children, ok := h.varCache[ref]
	if !ok || len(children) != 2 || children[0].Name != "X" || children[1].Name != "Y" {
		t.Errorf("cached children = %+v (ok=%v)", children, ok)
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
