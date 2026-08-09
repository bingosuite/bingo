package dap

import (
	"strings"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// These specs pin the setBreakpoints transaction: installed facts versus
// latest-wins intent, request-owned removals, and exactly one response per
// request under pipelining. See AGENTS.md → DAP (breakpoint transaction).

var bpSource = godap.Source{Path: "/x/main.go", Name: "main.go"}
var otherSource = godap.Source{Path: "/x/other.go", Name: "other.go"}

// suspendedHarness runs the handshake and parks the adapter at a breakpoint,
// the state a client sets breakpoints from.
func suspendedHarness(t *testing.T) *harness {
	t.Helper()
	hh := newHarness(t)
	hh.doHandshake(t)
	hh.inject(protocol.EventBreakpointHit, protocol.BreakpointHitPayload{Goroutine: protocol.Goroutine{ID: 1}})
	_ = recvType[*godap.StoppedEvent](hh)
	return hh
}

func setBreakpointsFor(source godap.Source, lines ...int) *godap.SetBreakpointsRequest {
	bps := make([]godap.SourceBreakpoint, len(lines))
	for i, line := range lines {
		bps[i] = godap.SourceBreakpoint{Line: line}
	}
	return &godap.SetBreakpointsRequest{Arguments: godap.SetBreakpointsArguments{
		Source:      source,
		Breakpoints: bps,
	}}
}

func (hh *harness) confirmSet(source godap.Source, line, id int) {
	hh.t.Helper()
	hh.inject(protocol.EventBreakpointSet, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: id, Location: protocol.Location{File: source.Path, Line: line}},
	})
}

// installBreakpoint drives one line to a confirmed install and consumes its
// response, leaving the source with exactly that breakpoint armed.
func (hh *harness) installBreakpoint(source godap.Source, lines []int, ids []int) {
	hh.t.Helper()
	hh.sendReq("setBreakpoints", setBreakpointsFor(source, lines...))
	hh.cmds.waitForCommands(hh.t, protocol.CmdSetBreakpoint, len(lines))
	for i, line := range lines {
		hh.confirmSet(source, line, ids[i])
	}
	_ = recvType[*godap.SetBreakpointsResponse](hh)
}

// clearIDs decodes the ClearBreakpoint commands the handler has issued so far.
func (hh *harness) clearIDs(count int) []int {
	hh.t.Helper()
	cmds := hh.cmds.waitForCommands(hh.t, protocol.CmdClearBreakpoint, count)
	out := make([]int, len(cmds))
	for i, cmd := range cmds {
		var p protocol.ClearBreakpointPayload
		if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
			hh.t.Fatal(err)
		}
		out[i] = p.ID
	}
	return out
}

func (hh *harness) countCommands(kind protocol.CommandKind) int {
	hh.t.Helper()
	n := 0
	for _, k := range hh.cmds.kinds() {
		if k == kind {
			n++
		}
	}
	return n
}

// lineState copies one line's transaction state for assertions.
func (hh *harness) lineState(file string, line int) (bpLine, bool) {
	hh.t.Helper()
	hh.handler.mu.Lock()
	defer hh.handler.mu.Unlock()
	st := hh.handler.bpByFile[file][line]
	if st == nil {
		return bpLine{}, false
	}
	return *st, true
}

// waitForLine polls until pred holds, so a test can synchronise on a request
// that issues no command of its own — a removal parked behind an in-flight set.
func (hh *harness) waitForLine(file string, line int, pred func(bpLine) bool) {
	hh.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := hh.lineState(file, line); ok && pred(st) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	hh.t.Fatalf("timed out waiting for %s:%d state", file, line)
}

func requireErrorResponse(t *testing.T, resp *godap.ErrorResponse, reqSeq int, contains string) {
	t.Helper()
	if resp.RequestSeq != reqSeq || resp.Success {
		t.Fatalf("error response = %+v, want failure for request %d", resp.Response, reqSeq)
	}
	if !strings.Contains(resp.Message, contains) {
		t.Fatalf("error message = %q, want it to mention %q", resp.Message, contains)
	}
}

// A remove-all must not report success before its clear is confirmed, and a
// rejected clear must fail the request while keeping the still-armed id.
func TestRemoveAllFailedClearRetainsIDAndFailsRequest(t *testing.T) {
	hh := suspendedHarness(t)
	hh.installBreakpoint(bpSource, []int{10}, []int{101})

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	if ids := hh.clearIDs(1); ids[0] != 101 {
		t.Fatalf("clear id = %d, want 101", ids[0])
	}
	hh.expectNoResponse()

	hh.inject(protocol.EventError, protocol.ErrorPayload{
		Command: protocol.CmdClearBreakpoint,
		Message: "breakpoint clear: restore bytes at 0x1000: boom",
	})
	requireErrorResponse(t, recvType[*godap.ErrorResponse](hh), removeSeq, "boom")

	st, ok := hh.lineState(bpSource.Path, 10)
	if !ok || st.installedID != 101 {
		t.Fatalf("line state after failed clear = %+v (present=%v), want installedID 101 retained", st, ok)
	}

	// The retained mapping is what makes the removal retryable at all.
	hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	if ids := hh.clearIDs(2); ids[1] != 101 {
		t.Fatalf("retry clear id = %d, want 101", ids[1])
	}
}

// One rejected clear among several fails the whole request, exactly once.
func TestPartialClearFailureFailsRequestExactlyOnce(t *testing.T) {
	hh := suspendedHarness(t)
	hh.installBreakpoint(bpSource, []int{10, 20}, []int{101, 102})

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	if ids := hh.clearIDs(2); ids[0] != 101 || ids[1] != 102 {
		t.Fatalf("clear ids = %v, want [101 102]", ids)
	}

	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})
	hh.expectNoResponse()
	hh.inject(protocol.EventError, protocol.ErrorPayload{
		Command: protocol.CmdClearBreakpoint,
		Message: "breakpoint 102 not found",
	})

	requireErrorResponse(t, recvType[*godap.ErrorResponse](hh), removeSeq, "breakpoint 102 not found")
	hh.expectNoResponse()

	if _, ok := hh.lineState(bpSource.Path, 10); ok {
		t.Fatal("confirmed clear left line 10 in the cache")
	}
	st, ok := hh.lineState(bpSource.Path, 20)
	if !ok || st.installedID != 102 {
		t.Fatalf("line 20 state = %+v (present=%v), want installedID 102 retained", st, ok)
	}
}

// A pending set superseded by a later empty request is cleared the moment its id
// arrives, and neither request is answered before that removal completes.
func TestSupersededPendingSetIsClearedOnConfirmation(t *testing.T) {
	hh := suspendedHarness(t)

	setSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 1)

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	hh.waitForLine(bpSource.Path, 10, func(st bpLine) bool { return !st.desired && len(st.clearOwners) == 1 })
	hh.expectNoResponse()

	hh.confirmSet(bpSource, 10, 101)
	if ids := hh.clearIDs(1); ids[0] != 101 {
		t.Fatalf("clear id = %d, want the id that just arrived (101)", ids[0])
	}
	hh.expectNoResponse()

	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})

	// Request order: the superseded set answers first, the authoritative empty
	// request last.
	first := recvType[*godap.SetBreakpointsResponse](hh)
	if first.RequestSeq != setSeq || len(first.Body.Breakpoints) != 1 || first.Body.Breakpoints[0].Verified {
		t.Fatalf("superseded response = %+v (seq %d), want one unverified breakpoint for %d", first.Body, first.RequestSeq, setSeq)
	}
	second := recvType[*godap.SetBreakpointsResponse](hh)
	if second.RequestSeq != removeSeq || len(second.Body.Breakpoints) != 0 {
		t.Fatalf("remove response = %+v (seq %d), want empty for %d", second.Body, second.RequestSeq, removeSeq)
	}

	if _, ok := hh.lineState(bpSource.Path, 10); ok {
		t.Fatal("superseded breakpoint survived in the cache")
	}
	if n := hh.countCommands(protocol.CmdSetBreakpoint); n != 1 {
		t.Fatalf("SetBreakpoint commands = %d, want 1", n)
	}
	if n := hh.countCommands(protocol.CmdClearBreakpoint); n != 1 {
		t.Fatalf("ClearBreakpoint commands = %d, want exactly 1", n)
	}
}

// An overlapping request wanting an already-pending line attaches to that
// operation instead of issuing a second SetBreakpoint for the same location.
func TestOverlappingPendingSetIssuesOneCommand(t *testing.T) {
	hh := suspendedHarness(t)

	firstSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 1)

	secondSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10, 20))
	sets := hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 2)
	var retried protocol.SetBreakpointPayload
	if err := protocol.DecodeCommandPayload(sets[1], &retried); err != nil {
		t.Fatal(err)
	}
	if retried.Line != 20 {
		t.Fatalf("second SetBreakpoint = %+v, want the new line 20 only", retried)
	}

	hh.confirmSet(bpSource, 10, 101)
	first := recvType[*godap.SetBreakpointsResponse](hh)
	if first.RequestSeq != firstSeq {
		t.Fatalf("first response seq = %d, want %d", first.RequestSeq, firstSeq)
	}
	requireBreakpointIDs(t, first, 101)

	hh.confirmSet(bpSource, 20, 102)
	second := recvType[*godap.SetBreakpointsResponse](hh)
	if second.RequestSeq != secondSeq {
		t.Fatalf("second response seq = %d, want %d", second.RequestSeq, secondSeq)
	}
	requireBreakpointIDs(t, second, 101, 102)

	if n := hh.countCommands(protocol.CmdSetBreakpoint); n != 2 {
		t.Fatalf("SetBreakpoint commands = %d, want 2 (no duplicate for line 10)", n)
	}
	if n := hh.countCommands(protocol.CmdClearBreakpoint); n != 0 {
		t.Fatalf("ClearBreakpoint commands = %d, want 0", n)
	}
}

// set → remove → re-add while the first set is still in flight: the latest
// intent wins and leaves exactly one live breakpoint, with no wasted round trip.
func TestSetRemoveReaddLeavesOneLiveBreakpoint(t *testing.T) {
	hh := suspendedHarness(t)

	setSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 1)

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	hh.waitForLine(bpSource.Path, 10, func(st bpLine) bool { return !st.desired })

	readdSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.waitForLine(bpSource.Path, 10, func(st bpLine) bool { return st.desired })
	hh.expectNoResponse()

	hh.confirmSet(bpSource, 10, 101)

	for _, want := range []int{setSeq, removeSeq, readdSeq} {
		resp := recvType[*godap.SetBreakpointsResponse](hh)
		if resp.RequestSeq != want {
			t.Fatalf("response seq = %d, want %d (requests answer in order)", resp.RequestSeq, want)
		}
		switch want {
		case removeSeq:
			if len(resp.Body.Breakpoints) != 0 {
				t.Fatalf("superseded removal response = %+v, want empty", resp.Body)
			}
		default:
			requireBreakpointIDs(t, resp, 101)
		}
	}

	st, ok := hh.lineState(bpSource.Path, 10)
	if !ok || st.installedID != 101 || !st.desired {
		t.Fatalf("final line state = %+v (present=%v), want one live breakpoint", st, ok)
	}
	if n := hh.countCommands(protocol.CmdSetBreakpoint); n != 1 {
		t.Fatalf("SetBreakpoint commands = %d, want 1", n)
	}
	if n := hh.countCommands(protocol.CmdClearBreakpoint); n != 0 {
		t.Fatalf("ClearBreakpoint commands = %d, want 0", n)
	}
}

// A source blocked on its own removal must not hold up another source.
func TestBreakpointSourcesSettleIndependently(t *testing.T) {
	hh := suspendedHarness(t)
	hh.installBreakpoint(bpSource, []int{10}, []int{101})

	blockedSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	hh.clearIDs(1)

	otherSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(otherSource, 20))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 2)
	hh.confirmSet(otherSource, 20, 102)

	resp := recvType[*godap.SetBreakpointsResponse](hh)
	if resp.RequestSeq != otherSeq {
		t.Fatalf("response seq = %d, want the independent source %d", resp.RequestSeq, otherSeq)
	}
	requireBreakpointIDs(t, resp, 102)
	hh.expectNoResponse()

	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})
	blocked := recvType[*godap.SetBreakpointsResponse](hh)
	if blocked.RequestSeq != blockedSeq || len(blocked.Body.Breakpoints) != 0 {
		t.Fatalf("blocked response = %+v (seq %d), want empty for %d", blocked.Body, blocked.RequestSeq, blockedSeq)
	}
}

// A restart re-identifies settled lines but must leave an in-flight operation —
// and the FIFO slot correlating it — untouched.
func TestRestartReconcilesAroundPendingOperation(t *testing.T) {
	hh := suspendedHarness(t)
	hh.installBreakpoint(bpSource, []int{10}, []int{41})

	pendingSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10, 20))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 2)
	hh.expectNoResponse()

	restartSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{
		Breakpoints: []protocol.Breakpoint{
			{ID: 101, Location: protocol.Location{File: bpSource.Path, Line: 10}},
		},
	})
	restarted := recvType[*godap.RestartResponse](hh)
	if restarted.RequestSeq != restartSeq || !restarted.Success {
		t.Fatalf("restart response = %+v, want success for %d", restarted.Response, restartSeq)
	}

	settled, ok := hh.lineState(bpSource.Path, 10)
	if !ok || settled.installedID != 101 || settled.dapID != 41 {
		t.Fatalf("settled line = %+v (present=%v), want installedID 101 with stable dapID 41", settled, ok)
	}
	pending, ok := hh.lineState(bpSource.Path, 20)
	if !ok || pending.pending == nil || pending.installedID != 0 {
		t.Fatalf("in-flight line = %+v (present=%v), want its operation preserved", pending, ok)
	}

	// The preserved FIFO slot still correlates the confirmation that arrives
	// after the relaunch.
	hh.confirmSet(bpSource, 20, 55)
	resp := recvType[*godap.SetBreakpointsResponse](hh)
	if resp.RequestSeq != pendingSeq {
		t.Fatalf("response seq = %d, want %d", resp.RequestSeq, pendingSeq)
	}
	requireBreakpointIDs(t, resp, 41, 55)
}

// A removal is complete as soon as its trap is gone: a re-add pipelined behind
// it must not hold the removing request open until the new install lands.
func TestRemovalCompletesBeforeReaddIsInstalled(t *testing.T) {
	hh := suspendedHarness(t)
	hh.installBreakpoint(bpSource, []int{10}, []int{101})

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	hh.clearIDs(1)

	readdSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.waitForLine(bpSource.Path, 10, func(st bpLine) bool { return st.desired })
	hh.expectNoResponse()

	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})

	removed := recvType[*godap.SetBreakpointsResponse](hh)
	if removed.RequestSeq != removeSeq || len(removed.Body.Breakpoints) != 0 {
		t.Fatalf("remove response = %+v (seq %d), want empty for %d", removed.Body, removed.RequestSeq, removeSeq)
	}

	// Only now is the re-add issued, against a line the debugger has released.
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 2)
	hh.confirmSet(bpSource, 10, 102)
	readded := recvType[*godap.SetBreakpointsResponse](hh)
	if readded.RequestSeq != readdSeq {
		t.Fatalf("re-add response seq = %d, want %d", readded.RequestSeq, readdSeq)
	}
	requireBreakpointIDs(t, readded, 102)
}

// A rejected clear ends the line's only in-flight operation, so it must also
// answer the earlier pipelined request whose Set it was cancelling — that
// breakpoint really is armed, and nothing else will ever settle it.
func TestFailedClearStillAnswersSupersededSet(t *testing.T) {
	hh := suspendedHarness(t)

	setSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 1)

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	hh.waitForLine(bpSource.Path, 10, func(st bpLine) bool { return !st.desired && len(st.clearOwners) == 1 })

	hh.confirmSet(bpSource, 10, 101)
	hh.clearIDs(1)
	hh.expectNoResponse()

	hh.inject(protocol.EventError, protocol.ErrorPayload{
		Command: protocol.CmdClearBreakpoint,
		Message: "breakpoint clear: restore bytes at 0x1000: boom",
	})

	installed, ok := hh.recv().(*godap.SetBreakpointsResponse)
	if !ok {
		t.Fatal("superseded set was not answered before the failed removal")
	}
	if installed.RequestSeq != setSeq {
		t.Fatalf("response seq = %d, want %d", installed.RequestSeq, setSeq)
	}
	// The clear failed, so the breakpoint this request asked for is genuinely
	// armed — reporting it unverified would be a lie.
	requireBreakpointIDs(t, installed, 101)
	if !installed.Body.Breakpoints[0].Verified {
		t.Fatalf("superseded breakpoint = %+v, want verified (its removal failed)", installed.Body.Breakpoints[0])
	}

	failed, ok := hh.recv().(*godap.ErrorResponse)
	if !ok {
		t.Fatal("failed removal was not reported as an error response")
	}
	requireErrorResponse(t, failed, removeSeq, "boom")
	hh.expectNoResponse()

	st, ok := hh.lineState(bpSource.Path, 10)
	if !ok || st.installedID != 101 {
		t.Fatalf("line state = %+v (present=%v), want the armed id retained", st, ok)
	}
}

// Confirmations the adapter did not ask for must never produce a second
// response for an already-answered request.
func TestSetBreakpointsRespondsExactlyOnce(t *testing.T) {
	hh := suspendedHarness(t)
	hh.installBreakpoint(bpSource, []int{10}, []int{101})

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	hh.clearIDs(1)
	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})

	resp := recvType[*godap.SetBreakpointsResponse](hh)
	if resp.RequestSeq != removeSeq || len(resp.Body.Breakpoints) != 0 {
		t.Fatalf("remove response = %+v (seq %d), want empty for %d", resp.Body, resp.RequestSeq, removeSeq)
	}

	// Duplicate and out-of-band confirmations for the same work.
	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})
	hh.inject(protocol.EventError, protocol.ErrorPayload{Command: protocol.CmdClearBreakpoint, Message: "already gone"})
	hh.confirmSet(bpSource, 10, 102)
	hh.expectNoResponse()
}
