package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/protocol"

	"github.com/gorilla/websocket"
)

type acceptedWebSocket struct {
	conn *websocket.Conn
	err  error
}

type loopbackClient struct {
	client    *wsClient
	conn      *websocket.Conn
	server    *httptest.Server
	release   chan struct{}
	closeOnce sync.Once
}

func newLoopbackClient(t *testing.T) *loopbackClient {
	t.Helper()

	accepted := make(chan acceptedWebSocket, 1)
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			accepted <- acceptedWebSocket{err: err}
			return
		}

		welcome, err := protocol.MarshalEvent(protocol.MustEvent(
			protocol.EventSessionState,
			1,
			protocol.SessionStatePayload{
				SessionID: "test-session",
				State:     protocol.StateIdle,
				Clients:   1,
			},
		))
		if err != nil {
			accepted <- acceptedWebSocket{err: err}
			_ = conn.Close()
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, welcome); err != nil {
			accepted <- acceptedWebSocket{err: err}
			_ = conn.Close()
			return
		}

		accepted <- acceptedWebSocket{conn: conn}
		<-release
		_ = conn.Close()
	}))

	rawClient, err := Create(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		close(release)
		server.Close()
		t.Fatalf("Create: %v", err)
	}

	result := <-accepted
	if result.err != nil {
		_ = rawClient.Close()
		close(release)
		server.Close()
		t.Fatalf("accept WebSocket: %v", result.err)
	}

	client, ok := rawClient.(*wsClient)
	if !ok {
		_ = rawClient.Close()
		close(release)
		server.Close()
		t.Fatalf("client type = %T, want *wsClient", rawClient)
	}

	h := &loopbackClient{
		client:  client,
		conn:    result.conn,
		server:  server,
		release: release,
	}
	t.Cleanup(h.close)
	return h
}

func (h *loopbackClient) close() {
	h.closeOnce.Do(func() {
		_ = h.client.Close()
		close(h.release)
		h.server.Close()
	})
}

func (h *loopbackClient) readCommand(t *testing.T) protocol.Command {
	t.Helper()

	if err := h.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set command read deadline: %v", err)
	}
	_, data, err := h.conn.ReadMessage()
	if err != nil {
		t.Fatalf("read command: %v", err)
	}
	if err := h.conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear command read deadline: %v", err)
	}

	cmd, err := protocol.UnmarshalCommand(data)
	if err != nil {
		t.Fatalf("unmarshal command: %v", err)
	}
	return cmd
}

func (h *loopbackClient) writeEvent(t *testing.T, evt protocol.Event) {
	t.Helper()

	data, err := protocol.MarshalEvent(evt)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if err := h.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set event write deadline: %v", err)
	}
	if err := h.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write event: %v", err)
	}
	if err := h.conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("clear event write deadline: %v", err)
	}
}

func useSyncTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()

	previous := syncTimeout
	syncTimeout = timeout
	t.Cleanup(func() {
		syncTimeout = previous
	})
}

type breakpointResult struct {
	breakpoint protocol.Breakpoint
	err        error
}

func callSetBreakpoint(c *wsClient, file string, line int) <-chan breakpointResult {
	result := make(chan breakpointResult, 1)
	go func() {
		breakpoint, err := c.SetBreakpoint(file, line)
		result <- breakpointResult{breakpoint: breakpoint, err: err}
	}()
	return result
}

func awaitBreakpoint(t *testing.T, result <-chan breakpointResult) breakpointResult {
	t.Helper()

	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("SetBreakpoint did not return")
		return breakpointResult{}
	}
}

func awaitEvent(t *testing.T, events <-chan protocol.Event) protocol.Event {
	t.Helper()

	select {
	case evt, ok := <-events:
		if !ok {
			t.Fatal("events channel closed")
		}
		return evt
	case <-time.After(2 * time.Second):
		t.Fatal("event did not arrive")
		return protocol.Event{}
	}
}

func assertCommandKind(t *testing.T, cmd protocol.Command, want protocol.CommandKind) {
	t.Helper()

	if cmd.Kind != want {
		t.Fatalf("command kind = %s, want %s", cmd.Kind, want)
	}
}

func TestTimedOutReplyDebtDoesNotSatisfyLaterRequest(t *testing.T) {
	useSyncTimeout(t, 100*time.Millisecond)
	h := newLoopbackClient(t)

	first := callSetBreakpoint(h.client, "first.go", 10)
	assertCommandKind(t, h.readCommand(t), protocol.CmdSetBreakpoint)
	if result := awaitBreakpoint(t, first); result.err == nil || !strings.Contains(result.err.Error(), "timeout") {
		t.Fatalf("first SetBreakpoint error = %v, want timeout", result.err)
	}

	localsResult := make(chan struct {
		variables []protocol.Variable
		err       error
	}, 1)
	go func() {
		variables, err := h.client.Locals(0)
		localsResult <- struct {
			variables []protocol.Variable
			err       error
		}{variables: variables, err: err}
	}()
	assertCommandKind(t, h.readCommand(t), protocol.CmdLocals)
	h.writeEvent(t, protocol.MustEvent(protocol.EventOutput, 2, protocol.OutputPayload{
		Stream:  "stdout",
		Content: "other-kind event",
	}))
	h.writeEvent(t, protocol.MustEvent(protocol.EventLocals, 3, protocol.LocalsPayload{
		FrameIndex: 0,
		Variables:  []protocol.Variable{{Name: "answer", Value: "42", Type: "int"}},
	}))

	select {
	case result := <-localsResult:
		if result.err != nil {
			t.Fatalf("Locals: %v", result.err)
		}
		if len(result.variables) != 1 || result.variables[0].Value != "42" {
			t.Fatalf("Locals variables = %+v", result.variables)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Locals did not return")
	}
	if evt := awaitEvent(t, h.client.Events()); evt.Kind != protocol.EventOutput {
		t.Fatalf("async event kind = %s, want %s", evt.Kind, protocol.EventOutput)
	}

	second := callSetBreakpoint(h.client, "second.go", 20)
	assertCommandKind(t, h.readCommand(t), protocol.CmdSetBreakpoint)
	h.writeEvent(t, protocol.MustEvent(protocol.EventBreakpointSet, 4, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{
			ID:       1,
			Location: protocol.Location{File: "first.go", Line: 10},
		},
	}))
	h.writeEvent(t, protocol.MustEvent(protocol.EventBreakpointSet, 5, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{
			ID:       2,
			Location: protocol.Location{File: "second.go", Line: 20},
		},
	}))
	h.writeEvent(t, protocol.MustEvent(protocol.EventOutput, 6, protocol.OutputPayload{
		Stream:  "stdout",
		Content: "read pump active",
	}))

	result := awaitBreakpoint(t, second)
	if result.err != nil {
		t.Fatalf("second SetBreakpoint: %v", result.err)
	}
	if result.breakpoint.ID != 2 || result.breakpoint.Location.File != "second.go" {
		t.Fatalf("second SetBreakpoint = %+v, want its own confirmation", result.breakpoint)
	}
	if evt := awaitEvent(t, h.client.Events()); evt.Kind != protocol.EventOutput {
		t.Fatalf("post-reply event kind = %s, want %s", evt.Kind, protocol.EventOutput)
	}

	goroutinesResult := make(chan struct {
		goroutines []protocol.Goroutine
		err        error
	}, 1)
	go func() {
		goroutines, err := h.client.Goroutines()
		goroutinesResult <- struct {
			goroutines []protocol.Goroutine
			err        error
		}{goroutines: goroutines, err: err}
	}()
	assertCommandKind(t, h.readCommand(t), protocol.CmdGoroutines)
	h.writeEvent(t, protocol.MustEvent(protocol.EventGoroutines, 7, protocol.GoroutinesPayload{
		Goroutines: []protocol.Goroutine{{ID: 9, Status: "waiting"}},
	}))

	select {
	case result := <-goroutinesResult:
		if result.err != nil {
			t.Fatalf("Goroutines: %v", result.err)
		}
		if len(result.goroutines) != 1 || result.goroutines[0].ID != 9 {
			t.Fatalf("Goroutines = %+v", result.goroutines)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Goroutines did not return")
	}
}

func TestMatchingErrorConsumesReplyDebt(t *testing.T) {
	useSyncTimeout(t, 100*time.Millisecond)
	h := newLoopbackClient(t)

	first := callSetBreakpoint(h.client, "first.go", 10)
	assertCommandKind(t, h.readCommand(t), protocol.CmdSetBreakpoint)
	if result := awaitBreakpoint(t, first); result.err == nil || !strings.Contains(result.err.Error(), "timeout") {
		t.Fatalf("first SetBreakpoint error = %v, want timeout", result.err)
	}

	second := callSetBreakpoint(h.client, "second.go", 20)
	assertCommandKind(t, h.readCommand(t), protocol.CmdSetBreakpoint)
	h.writeEvent(t, protocol.MustEvent(protocol.EventError, 2, protocol.ErrorPayload{
		Command: protocol.CmdSetBreakpoint,
		Message: "first request failed late",
	}))
	h.writeEvent(t, protocol.MustEvent(protocol.EventBreakpointSet, 3, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{
			ID:       2,
			Location: protocol.Location{File: "second.go", Line: 20},
		},
	}))

	result := awaitBreakpoint(t, second)
	if result.err != nil {
		t.Fatalf("second SetBreakpoint: %v", result.err)
	}
	if result.breakpoint.ID != 2 {
		t.Fatalf("second SetBreakpoint = %+v, want ID 2", result.breakpoint)
	}
}

func TestUnresolvedReplyDebtMakesLaterRequestTimeout(t *testing.T) {
	useSyncTimeout(t, 100*time.Millisecond)
	h := newLoopbackClient(t)

	first := callSetBreakpoint(h.client, "first.go", 10)
	assertCommandKind(t, h.readCommand(t), protocol.CmdSetBreakpoint)
	if result := awaitBreakpoint(t, first); result.err == nil || !strings.Contains(result.err.Error(), "timeout") {
		t.Fatalf("first SetBreakpoint error = %v, want timeout", result.err)
	}

	second := callSetBreakpoint(h.client, "second.go", 20)
	assertCommandKind(t, h.readCommand(t), protocol.CmdSetBreakpoint)
	h.writeEvent(t, protocol.MustEvent(protocol.EventBreakpointSet, 2, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{
			ID:       2,
			Location: protocol.Location{File: "second.go", Line: 20},
		},
	}))

	result := awaitBreakpoint(t, second)
	if result.err == nil || !strings.Contains(result.err.Error(), "timeout") {
		t.Fatalf("second SetBreakpoint error = %v, want safe timeout", result.err)
	}

	localsResult := make(chan error, 1)
	go func() {
		_, err := h.client.Locals(0)
		localsResult <- err
	}()
	assertCommandKind(t, h.readCommand(t), protocol.CmdLocals)
	h.writeEvent(t, protocol.MustEvent(protocol.EventLocals, 3, protocol.LocalsPayload{FrameIndex: 0}))
	select {
	case err := <-localsResult:
		if err != nil {
			t.Fatalf("Locals after unresolved debt: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Locals after unresolved debt did not return")
	}
}

// The synchronous snapshot query this test was written against is gone; the
// telemetry guarantee it protected is not. A snapshot request that is never
// answered leaves no pending entry and no reply debt behind, so a later stop's
// breakpoint event and its auto-pushed snapshot both reach Events() intact and
// in order — there is nothing left that could absorb either of them.
func TestUnansweredGoroutineSnapshotRequestDoesNotConsumeAutoSnapshot(t *testing.T) {
	useSyncTimeout(t, 100*time.Millisecond)
	h := newLoopbackClient(t)

	if err := h.client.RequestGoroutineSnapshot(); err != nil {
		t.Fatalf("RequestGoroutineSnapshot: %v", err)
	}
	assertCommandKind(t, h.readCommand(t), protocol.CmdGoroutineSnapshot)

	h.client.pendingMu.Lock()
	pending := len(h.client.pending)
	h.client.pendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending entries after snapshot request = %d, want 0", pending)
	}

	// Well past the old synchronous deadline, so a debt-based implementation
	// would have had its retired entry armed by now.
	time.Sleep(2 * syncTimeout)

	h.writeEvent(t, protocol.MustEvent(
		protocol.EventBreakpointHit,
		2,
		protocol.BreakpointHitPayload{},
	))
	h.writeEvent(t, protocol.MustEvent(
		protocol.EventGoroutineSnapshot,
		3,
		protocol.GoroutineSnapshotPayload{Current: 42},
	))

	if evt := awaitEvent(t, h.client.Events()); evt.Kind != protocol.EventBreakpointHit {
		t.Fatalf("first async event kind = %s, want %s", evt.Kind, protocol.EventBreakpointHit)
	}
	evt := awaitEvent(t, h.client.Events())
	if evt.Kind != protocol.EventGoroutineSnapshot {
		t.Fatalf("second async event kind = %s, want %s", evt.Kind, protocol.EventGoroutineSnapshot)
	}
	var snapshot protocol.GoroutineSnapshotPayload
	if err := protocol.DecodeEventPayload(evt, &snapshot); err != nil {
		t.Fatalf("decode auto snapshot: %v", err)
	}
	if snapshot.Current != 42 {
		t.Fatalf("auto snapshot current = %d, want 42", snapshot.Current)
	}
}

func TestRouteToPendingClaimsBeforeDelivery(t *testing.T) {
	ch := make(chan protocol.Event, 1)
	client := &wsClient{
		pending: []*pendingReq{{
			wantKind: protocol.EventBreakpointSet,
			cmdKind:  protocol.CmdSetBreakpoint,
			ch:       ch,
		}},
	}
	first := protocol.MustEvent(protocol.EventBreakpointSet, 1, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 1},
	})
	second := protocol.MustEvent(protocol.EventBreakpointSet, 2, protocol.BreakpointSetPayload{
		Breakpoint: protocol.Breakpoint{ID: 2},
	})

	result := make(chan [2]bool, 1)
	go func() {
		result <- [2]bool{
			client.routeToPending(first),
			client.routeToPending(second),
		}
	}()

	select {
	case routed := <-result:
		if !routed[0] || routed[1] {
			t.Fatalf("route results = %v, want [true false]", routed)
		}
	case <-time.After(time.Second):
		t.Fatal("back-to-back matching events blocked pending delivery")
	}

	select {
	case evt := <-ch:
		if evt.Seq != first.Seq {
			t.Fatalf("delivered seq = %d, want %d", evt.Seq, first.Seq)
		}
	default:
		t.Fatal("first matching event was not delivered")
	}
}
