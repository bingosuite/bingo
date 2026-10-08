package dap

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
	godap "github.com/google/go-dap"
)

func selectedThread(t *testing.T, hh *harness, goid int) int {
	t.Helper()
	before := hh.cmds.count(protocol.CmdGoroutines)
	hh.sendReq("threads", &godap.ThreadsRequest{})
	hh.cmds.waitForCommands(t, protocol.CmdGoroutines, before+1)
	hh.inject(protocol.EventGoroutines, protocol.GoroutinesPayload{Goroutines: []protocol.Goroutine{
		{ID: 1, Status: "waiting"}, {ID: 7, Status: "running", Current: true},
	}})
	response := recvType[*godap.ThreadsResponse](hh)
	hh.handler.mu.Lock()
	handle := hh.handler.goroutineThreads[goid]
	hh.handler.mu.Unlock()
	for _, thread := range response.Body.Threads {
		if thread.Id == handle && handle > varRefBase {
			return handle
		}
	}
	t.Fatalf("goid %d has no returned opaque thread handle: %+v", goid, response.Body.Threads)
	return 0
}

func selectedStack(t *testing.T, hh *harness, thread, goid int) int {
	t.Helper()
	before := hh.cmds.count(protocol.CmdFrames)
	hh.sendReq("stackTrace", &godap.StackTraceRequest{
		Arguments: godap.StackTraceArguments{ThreadId: thread},
	})
	commands := hh.cmds.waitForCommands(t, protocol.CmdFrames, before+1)
	command := commands[before]
	var payload protocol.FramesPayloadCmd
	if err := protocol.DecodeCommandPayload(command, &payload); err != nil || payload.GoroutineID != goid {
		t.Fatalf("selected frame command = %+v: %v", payload, err)
	}
	hh.inject(protocol.EventFrames, protocol.FramesPayload{
		GoroutineID: goid, Frames: []protocol.Frame{{Index: 2, Location: protocol.Location{Function: "selected.worker"}}},
	})
	response := recvType[*godap.StackTraceResponse](hh)
	if len(response.Body.StackFrames) != 1 {
		t.Fatalf("selected frames: %+v", response.Body.StackFrames)
	}
	return response.Body.StackFrames[0].Id
}

func TestSelectedFrameContextFlowsThroughScopesLocalsAndEvaluate(t *testing.T) {
	hh := newSuspendedHarness(t)
	thread := selectedThread(t, hh, 1)
	frame := selectedStack(t, hh, thread, 1)
	hh.sendReq("scopes", &godap.ScopesRequest{Arguments: godap.ScopesArguments{FrameId: frame}})
	scope := recvType[*godap.ScopesResponse](hh)
	if len(scope.Body.Scopes) != 1 {
		t.Fatalf("selected scopes: %+v", scope.Body.Scopes)
	}
	hh.sendReq("variables", &godap.VariablesRequest{Arguments: godap.VariablesArguments{
		VariablesReference: scope.Body.Scopes[0].VariablesReference,
	}})
	command := hh.cmds.waitForCommand(t, protocol.CmdLocals)
	var locals protocol.LocalsPayloadCmd
	if err := protocol.DecodeCommandPayload(command, &locals); err != nil || locals.GoroutineID != 1 || locals.FrameIndex != 2 {
		t.Fatalf("selected locals = %+v: %v", locals, err)
	}
	hh.inject(protocol.EventLocals, protocol.LocalsPayload{
		GoroutineID: 1, FrameIndex: 2, Variables: []protocol.Variable{{Name: "label", Value: "101"}},
	})
	_ = recvType[*godap.VariablesResponse](hh)
	hh.sendReq("evaluate", &godap.EvaluateRequest{Arguments: godap.EvaluateArguments{FrameId: frame, Expression: "label"}})
	command = hh.cmds.waitForCommand(t, protocol.CmdEvaluate)
	var evaluate protocol.EvaluatePayloadCmd
	if err := protocol.DecodeCommandPayload(command, &evaluate); err != nil || evaluate.GoroutineID != 1 || evaluate.FrameIndex != 2 {
		t.Fatalf("selected evaluate = %+v: %v", evaluate, err)
	}
	hh.inject(protocol.EventEvaluate, protocol.EvaluatePayload{
		GoroutineID: 1, Result: protocol.Variable{Name: "label", Value: "101"},
	})
	if response := recvType[*godap.EvaluateResponse](hh); response.Body.Result != "101" {
		t.Fatalf("selected evaluate result: %+v", response.Body)
	}
}

func TestSelectedThreadRejectsUnprovenLegacyFrames(t *testing.T) {
	hh := newSuspendedHarness(t)
	thread := selectedThread(t, hh, 1)
	hh.sendReq("stackTrace", &godap.StackTraceRequest{Arguments: godap.StackTraceArguments{ThreadId: thread}})
	hh.cmds.waitForCommand(t, protocol.CmdFrames)
	hh.inject(protocol.EventFrames, protocol.FramesPayload{Frames: []protocol.Frame{{Index: 0}}})
	if response := recvType[*godap.ErrorResponse](hh); response.Success || response.Message == "" {
		t.Fatalf("unproven selected response was accepted: %+v", response)
	}
	selectedStack(t, hh, thread, 1)
}

func TestOpaqueThreadsSurviveRejectedResumeButNotNewStopOrRestart(t *testing.T) {
	for _, transition := range []string{"stop", "restart"} {
		t.Run(transition, func(t *testing.T) {
			hh := newSuspendedHarness(t)
			thread := selectedThread(t, hh, 1)
			selectedStack(t, hh, thread, 1)
			driveContinue(t, hh)
			rejectResume(t, hh, protocol.CmdContinue, "continue", "rejected")
			requireResyncStopped(t, hh, "rejected")
			selectedStack(t, hh, thread, 1)
			if transition == "restart" {
				hh.sendReq("restart", &godap.RestartRequest{})
				hh.cmds.waitForCommand(t, protocol.CmdRestart)
				hh.inject(protocol.EventRestarted, protocol.RestartedPayload{})
				_ = recvType[*godap.RestartResponse](hh)
			}
			hh.inject(protocol.EventPaused, protocol.PausedPayload{Goroutine: protocol.Goroutine{ID: 7}})
			_ = recvType[*godap.StoppedEvent](hh)
			before := hh.cmds.count(protocol.CmdFrames)
			hh.sendReq("stackTrace", &godap.StackTraceRequest{Arguments: godap.StackTraceArguments{ThreadId: thread}})
			_ = recvType[*godap.ErrorResponse](hh)
			hh.cmds.requireNoAdditionalCommands(t, protocol.CmdFrames, before)
			if fresh := selectedThread(t, hh, 1); fresh <= thread {
				t.Fatalf("thread handle reused: old %d, new %d", thread, fresh)
			}
		})
	}
}

func TestThreadMappingMetadataDoesNotInventSyntheticGoids(t *testing.T) {
	response := &bingoThreadsResponse{}
	response.Body.Threads = []bingoThread{
		{Thread: godap.Thread{Id: 100}},
		{Thread: godap.Thread{Id: 101}, GoroutineID: 1},
	}
	wire, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Body struct {
			Threads []map[string]json.RawMessage `json:"threads"`
		} `json:"body"`
	}
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, exists := decoded.Body.Threads[0]["bingoGoroutineId"]; exists || string(decoded.Body.Threads[1]["bingoGoroutineId"]) != "1" {
		t.Fatalf("invalid synthetic/real metadata: %s", wire)
	}
}

func inspectionStepRequest(t *testing.T, hh *harness, request string, thread int) int {
	t.Helper()
	switch request {
	case "next":
		return hh.sendReq(request, &godap.NextRequest{Arguments: godap.NextArguments{ThreadId: thread}})
	case "stepIn":
		return hh.sendReq(request, &godap.StepInRequest{Arguments: godap.StepInArguments{ThreadId: thread}})
	case "stepOut":
		return hh.sendReq(request, &godap.StepOutRequest{Arguments: godap.StepOutArguments{ThreadId: thread}})
	default:
		t.Fatalf("unknown step %s", request)
		return 0
	}
}

func TestInspectionOnlyStepRejectsNonCurrentUnknownAndStaleHandles(t *testing.T) {
	for _, request := range []string{"next", "stepIn", "stepOut"} {
		for _, target := range []string{"non-current", "unknown", "stale", "restart-stale"} {
			t.Run(request+"/"+target, func(t *testing.T) {
				testRejectedInspectionStep(t, request, target)
			})
		}
	}
}

func testRejectedInspectionStep(t *testing.T, request, target string) {
	hh := newSuspendedHarness(t)
	thread := selectedThread(t, hh, 1)
	switch target {
	case "unknown":
		thread = 123
	case "stale":
		suspendAtBreakpoint(t, hh)
	case "restart-stale":
		hh.sendReq("restart", &godap.RestartRequest{})
		hh.cmds.waitForCommand(t, protocol.CmdRestart)
		hh.inject(protocol.EventRestarted, protocol.RestartedPayload{})
		_ = recvType[*godap.RestartResponse](hh)
		hh.inject(protocol.EventPaused, protocol.PausedPayload{Goroutine: protocol.Goroutine{ID: 7}})
		_ = recvType[*godap.StoppedEvent](hh)
	}
	frame := inspectCurrentFrame(t, hh)
	hh.handler.mu.Lock()
	generation := hh.handler.stopGeneration
	current := hh.handler.curThreadID
	reference, err := hh.handler.allocVarRef()
	if err != nil {
		hh.handler.mu.Unlock()
		t.Fatal(err)
	}
	cached := []godap.Variable{{Name: "retained", Value: "101"}}
	hh.handler.varCache[reference] = cached
	hh.handler.mu.Unlock()
	before := hh.cmds.count(protocol.CmdStepOver) + hh.cmds.count(protocol.CmdStepInto) + hh.cmds.count(protocol.CmdStepOut)
	seq := inspectionStepRequest(t, hh, request, thread)
	response := recvType[*godap.ErrorResponse](hh)
	if response.RequestSeq != seq || response.Success || response.Message == "" {
		t.Fatalf("invalid step target accepted: %+v", response)
	}
	hh.handler.mu.Lock()
	preserved := hh.handler.suspended && hh.handler.stopGeneration == generation &&
		hh.handler.curThreadID == current && reflect.DeepEqual(hh.handler.varCache[reference], cached)
	hh.handler.mu.Unlock()
	if !preserved {
		t.Fatal("rejected selection invalidated the suspended context or cache")
	}
	after := hh.cmds.count(protocol.CmdStepOver) + hh.cmds.count(protocol.CmdStepInto) + hh.cmds.count(protocol.CmdStepOut)
	if after != before {
		t.Fatal("rejected selected step reached the hub")
	}
	hh.sendReq("scopes", &godap.ScopesRequest{Arguments: godap.ScopesArguments{FrameId: frame}})
	if scopes := recvType[*godap.ScopesResponse](hh); len(scopes.Body.Scopes) != 1 {
		t.Fatal("rejected selected step invalidated the frame handle")
	}
	hh.sendReq("variables", &godap.VariablesRequest{Arguments: godap.VariablesArguments{VariablesReference: reference}})
	if variables := recvType[*godap.VariablesResponse](hh); !reflect.DeepEqual(variables.Body.Variables, cached) {
		t.Fatal("rejected selected step invalidated cached variable handles")
	}
}

func TestInspectionOnlyStepPreservesCurrentAndSyntheticTargets(t *testing.T) {
	for _, request := range []string{"next", "stepIn", "stepOut"} {
		for _, target := range []string{"zero", "current", "synthetic", "resolved synthetic"} {
			t.Run(request+"/"+target, func(t *testing.T) {
				assertInspectionCurrentStepTarget(t, request, target)
			})
		}
	}
}

func assertInspectionCurrentStepTarget(t *testing.T, request, target string) {
	t.Helper()
	hh := newSuspendedHarness(t)
	thread := 0
	if strings.Contains(target, "synthetic") {
		hh.inject(protocol.EventStepped, protocol.SteppedPayload{})
		_ = recvType[*godap.StoppedEvent](hh)
	}
	if target != "zero" {
		thread = currentThreadHandle(hh)
	}
	if target == "resolved synthetic" {
		_ = selectedThread(t, hh, 7)
	}
	inspectionStepRequest(t, hh, request, thread)
	switch request {
	case "next":
		_ = recvType[*godap.NextResponse](hh)
		hh.cmds.waitForCommand(t, protocol.CmdStepOver)
	case "stepIn":
		_ = recvType[*godap.StepInResponse](hh)
		hh.cmds.waitForCommand(t, protocol.CmdStepInto)
	case "stepOut":
		_ = recvType[*godap.StepOutResponse](hh)
		hh.cmds.waitForCommand(t, protocol.CmdStepOut)
	}
}
