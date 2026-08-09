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

// A restart re-identifies EVERY line it retains, including one with an
// operation still in flight: the in-flight clear was marshalled with the
// pre-restart id and will fail against the relaunched process, and retain-on-
// failure would otherwise reissue that dead id forever while the real new
// breakpoint stayed armed.
func TestRestartReidentifiesLineUnderPendingClear(t *testing.T) {
	hh := suspendedHarness(t)
	hh.installBreakpoint(bpSource, []int{10}, []int{101})

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	if ids := hh.clearIDs(1); ids[0] != 101 {
		t.Fatalf("clear id = %d, want 101", ids[0])
	}

	restartSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{
		Breakpoints: []protocol.Breakpoint{
			{ID: 1, Location: protocol.Location{File: bpSource.Path, Line: 10}},
		},
	})
	restarted := recvType[*godap.RestartResponse](hh)
	if restarted.RequestSeq != restartSeq || !restarted.Success {
		t.Fatalf("restart response = %+v, want success for %d", restarted.Response, restartSeq)
	}

	st, ok := hh.lineState(bpSource.Path, 10)
	if !ok || st.installedID != 1 {
		t.Fatalf("line state = %+v (present=%v), want the fresh debugger id 1", st, ok)
	}
	if st.dapID != 101 {
		t.Fatalf("dapID = %d, want the stable client identity 101", st.dapID)
	}
	if st.pending == nil {
		t.Fatal("restart cancelled the in-flight operation")
	}
	hh.handler.mu.Lock()
	inFlight := len(hh.handler.clearQ)
	hh.handler.mu.Unlock()
	if inFlight != 1 {
		t.Fatalf("clearQ length = %d, want the in-flight operation preserved", inFlight)
	}

	// The stale clear, carrying the pre-restart id, is rejected by the new
	// engine — but the mapping has already self-healed.
	hh.inject(protocol.EventError, protocol.ErrorPayload{
		Command: protocol.CmdClearBreakpoint,
		Message: "breakpoint 101 not found",
	})
	requireErrorResponse(t, recvType[*godap.ErrorResponse](hh), removeSeq, "breakpoint 101 not found")

	hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	ids := hh.clearIDs(2)
	if ids[1] != 1 {
		t.Fatalf("retry clear id = %d, want the fresh id 1 (not the dead %d)", ids[1], ids[0])
	}
	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 1})
	_ = recvType[*godap.SetBreakpointsResponse](hh)

	if _, ok := hh.lineState(bpSource.Path, 10); ok {
		t.Fatal("confirmed clear left the line in the cache")
	}
}

// A line whose Set is still in flight owns no identity yet, so a restart
// payload naming it describes another driver's breakpoint and must not be
// adopted — neither as a retained id nor as a discard.
func TestRestartIgnoresLineWithNoInstalledIdentity(t *testing.T) {
	hh := suspendedHarness(t)

	hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 1)

	restartSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{
		Breakpoints: []protocol.Breakpoint{
			{ID: 7, Location: protocol.Location{File: bpSource.Path, Line: 10}},
		},
		Discarded: []protocol.DiscardedBreakpoint{
			{Location: protocol.Location{File: bpSource.Path, Line: 10}, Reason: "no such line"},
		},
	})
	restarted := recvType[*godap.RestartResponse](hh)
	if restarted.RequestSeq != restartSeq {
		t.Fatalf("restart response seq = %d, want %d", restarted.RequestSeq, restartSeq)
	}

	st, ok := hh.lineState(bpSource.Path, 10)
	if !ok || st.installedID != 0 || st.pending == nil || !st.desired {
		t.Fatalf("line state = %+v (present=%v), want the in-flight set untouched", st, ok)
	}

	// Our own confirmation still lands on it and settles the request normally.
	hh.confirmSet(bpSource, 10, 55)
	resp := recvType[*godap.SetBreakpointsResponse](hh)
	requireBreakpointIDs(t, resp, 55)
}

// The adapter must hand every breakpoint command to the hub, in the order their
// FIFO slots were reserved, however large the burst — it blocks rather than
// drops. This is one half of the delivery contract the transaction depends on:
// a command that never reaches the debugger produces no confirmation, so its
// line stays `pending` forever and its request is never answered. Hub-side
// admission is the other half (#160 / #162).
func TestLargeSetBreakpointsDeliversEveryCommandInOrder(t *testing.T) {
	hh := suspendedHarness(t)

	// Comfortably past the handler's own command buffer, so the staged outbox
	// has to block on the hub read pump rather than discard.
	count := 3 * cmdBufferSize
	lines := make([]int, count)
	for i := range lines {
		lines[i] = 10 + i
	}
	hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, lines...))

	cmds := hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, count)
	if len(cmds) != count {
		t.Fatalf("SetBreakpoint commands = %d, want %d", len(cmds), count)
	}
	for i, cmd := range cmds {
		var p protocol.SetBreakpointPayload
		if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
			t.Fatal(err)
		}
		if p.Line != lines[i] {
			t.Fatalf("command %d = line %d, want %d (wire order must match reservation order)", i, p.Line, lines[i])
		}
	}
}

// discardUnderPendingClear installs a breakpoint, starts removing it, then has a
// restart discard the line while that clear is still in flight. The removal is
// satisfied by the discard itself, so its request completes successfully and the
// line is forgotten — leaving the clear abandoned, still queued, still owed an
// answer by the debugger.
func discardUnderPendingClear(t *testing.T, hh *harness) int {
	t.Helper()
	hh.installBreakpoint(bpSource, []int{10}, []int{101})

	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	hh.clearIDs(1)

	restartSeq := hh.sendReq("restart", &godap.RestartRequest{})
	hh.cmds.waitForCommand(t, protocol.CmdRestart)
	hh.inject(protocol.EventRestarted, protocol.RestartedPayload{
		Discarded: []protocol.DiscardedBreakpoint{
			{Location: protocol.Location{File: bpSource.Path, Line: 10}, Reason: "no such line"},
		},
	})

	var removed *godap.SetBreakpointsResponse
	var restarted *godap.RestartResponse
	for range 3 {
		switch msg := hh.recv().(type) {
		case *godap.SetBreakpointsResponse:
			removed = msg
		case *godap.RestartResponse:
			restarted = msg
		case *godap.ErrorResponse:
			t.Fatalf("removal failed although the discard already satisfied it: %+v", msg.Response)
		}
	}
	if removed == nil || removed.RequestSeq != removeSeq || len(removed.Body.Breakpoints) != 0 {
		t.Fatalf("removal response = %+v, want empty success for %d", removed, removeSeq)
	}
	if restarted == nil || restarted.RequestSeq != restartSeq {
		t.Fatalf("restart response = %+v, want %d", restarted, restartSeq)
	}
	if st, ok := hh.lineState(bpSource.Path, 10); ok {
		t.Fatalf("discarded line survived as %+v, want it forgotten", st)
	}
	return restartSeq
}

func (hh *harness) staleClearIsStillQueued() bool {
	hh.handler.mu.Lock()
	defer hh.handler.mu.Unlock()
	return len(hh.handler.clearQ) == 1
}

// After a discard abandons its clear, a later request that wants the line back
// must converge immediately, and the stale clear's rejection must not touch it.
func TestDiscardedLineAcceptsLaterSetAndIgnoresStaleClearError(t *testing.T) {
	hh := suspendedHarness(t)
	discardUnderPendingClear(t, hh)
	if !hh.staleClearIsStillQueued() {
		t.Fatal("abandoned clear lost its FIFO slot")
	}

	// The abandoned operation must not latch the line: this has to issue a Set.
	readdSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 2)

	hh.inject(protocol.EventError, protocol.ErrorPayload{
		Command: protocol.CmdClearBreakpoint,
		Message: "breakpoint 101 not found",
	})
	// The stale rejection belongs to nobody: it must not fail or resolve the
	// request now waiting on this line.
	hh.expectNoResponse()

	hh.confirmSet(bpSource, 10, 7)
	resp := recvType[*godap.SetBreakpointsResponse](hh)
	if resp.RequestSeq != readdSeq {
		t.Fatalf("response seq = %d, want %d", resp.RequestSeq, readdSeq)
	}
	requireBreakpointIDs(t, resp, 7)
	if !resp.Body.Breakpoints[0].Verified {
		t.Fatalf("re-added breakpoint = %+v, want verified", resp.Body.Breakpoints[0])
	}
}

// A removal the discard already satisfied must not be failed by the stale
// rejection that arrives afterwards.
func TestDiscardedLineDoesNotFailAlreadySatisfiedRemoval(t *testing.T) {
	hh := suspendedHarness(t)
	discardUnderPendingClear(t, hh)

	// Nothing is armed, so this replace-all owns no removal and answers at once.
	emptySeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	resp := recvType[*godap.SetBreakpointsResponse](hh)
	if resp.RequestSeq != emptySeq || len(resp.Body.Breakpoints) != 0 {
		t.Fatalf("empty response = %+v (seq %d), want empty success for %d", resp.Body, resp.RequestSeq, emptySeq)
	}

	hh.inject(protocol.EventError, protocol.ErrorPayload{
		Command: protocol.CmdClearBreakpoint,
		Message: "breakpoint 101 not found",
	})
	hh.expectNoResponse()

	if n := hh.countCommands(protocol.CmdClearBreakpoint); n != 1 {
		t.Fatalf("ClearBreakpoint commands = %d, want no retry of the discarded line", n)
	}
}

// A stale clear that succeeds late must also be a pure FIFO pop: it must not
// disarm the line's new identity, and the next real removal must still
// correlate to its own confirmation.
func TestStaleClearSuccessDoesNotDisturbReidentifiedLine(t *testing.T) {
	hh := suspendedHarness(t)
	discardUnderPendingClear(t, hh)

	hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource, 10))
	hh.cmds.waitForCommands(t, protocol.CmdSetBreakpoint, 2)
	hh.confirmSet(bpSource, 10, 7)
	_ = recvType[*godap.SetBreakpointsResponse](hh)

	// The abandoned clear finally succeeds against the old process.
	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 101})
	hh.expectNoResponse()
	st, ok := hh.lineState(bpSource.Path, 10)
	if !ok || st.installedID != 7 {
		t.Fatalf("line state = %+v (present=%v), want the new identity 7 intact", st, ok)
	}

	// The FIFO is still aligned: a real removal targets the new id and is
	// answered by its own confirmation.
	removeSeq := hh.sendReq("setBreakpoints", setBreakpointsFor(bpSource))
	ids := hh.clearIDs(2)
	if ids[1] != 7 {
		t.Fatalf("clear id = %d, want the current identity 7", ids[1])
	}
	hh.expectNoResponse()
	hh.inject(protocol.EventBreakpointCleared, protocol.BreakpointClearedPayload{ID: 7})
	resp := recvType[*godap.SetBreakpointsResponse](hh)
	if resp.RequestSeq != removeSeq || len(resp.Body.Breakpoints) != 0 {
		t.Fatalf("removal response = %+v (seq %d), want empty success for %d", resp.Body, resp.RequestSeq, removeSeq)
	}
	if _, ok := hh.lineState(bpSource.Path, 10); ok {
		t.Fatal("confirmed clear left the line in the cache")
	}
}

// A rejected clear is only un-retryable while its breakpoint is still armed.
// With nothing armed there is nothing to retry and nothing to report: the
// removal did happen, so its requests complete rather than fail, and the line
// resumes converging on the newest intent instead of stalling.
func TestFailedClearWithNothingArmedCompletesAndResumes(t *testing.T) {
	h := NewHandler(nil, nil, nil)

	removal := &bpRequest{reqSeq: 1, openClears: 1}
	readd := &bpRequest{reqSeq: 2}
	slot := &bpSlot{req: readd, line: 10, source: bpSource}
	readd.slots = []*bpSlot{slot}
	h.bpByFile[bpSource.Path] = map[int]*bpLine{10: {
		desired:     true,
		setWaiters:  []*bpSlot{slot},
		clearOwners: []*bpRequest{removal},
	}}

	h.mu.Lock()
	ready := h.failClearLocked(bpSource.Path, 10, "breakpoint 101 not found")
	h.mu.Unlock()

	if len(ready) != 1 || ready[0] != removal {
		t.Fatalf("ready = %+v, want only the removal request", ready)
	}
	if removal.clearFailure != "" {
		t.Fatalf("removal reported %q, want success — nothing was armed to fail on", removal.clearFailure)
	}
	if len(h.setQ) != 1 || len(h.outbox) != 1 {
		t.Fatalf("setQ=%d outbox=%d, want the desired line to resume converging", len(h.setQ), len(h.outbox))
	}
	if slot.resolved {
		t.Fatal("waiting slot was resolved before its breakpoint was installed")
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
