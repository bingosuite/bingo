package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
	"github.com/chzyer/readline"
)

func testSnapshotEvent(t *testing.T, seq uint64) protocol.Event {
	t.Helper()

	return protocol.MustEvent(protocol.EventGoroutineSnapshot, seq, protocol.GoroutineSnapshotPayload{
		Goroutines: []protocol.Goroutine{
			{ID: 1, Status: "running", Current: true},
			{ID: 18, Status: "waiting", ParentID: 1},
		},
		Threads: []protocol.Thread{{ID: 4242, MID: 3, GoID: 1, Current: true}},
		Current: 1,
		Created: []int{18},
	})
}

// The `snapshot` command arms a one-shot full render; every other snapshot —
// including the automatic pushes — stays a one-line summary. Both are printed:
// the flag only picks the renderer, it never consumes or correlates an event.
func TestFullSnapshotNextRendersOnceThenSummarises(t *testing.T) {
	t.Cleanup(func() { fullSnapshotNext.Store(false) })

	fullSnapshotNext.Store(true)
	var out bytes.Buffer
	printEvent(&out, testSnapshotEvent(t, 2))
	full := out.String()
	if !strings.Contains(full, "goroutines: 2 live  threads: 1 live  current: G1") {
		t.Fatalf("requested snapshot render = %q, want the full view", full)
	}
	if !strings.Contains(full, "created: [18]") {
		t.Fatalf("requested snapshot render = %q, want the lifecycle deltas", full)
	}
	if fullSnapshotNext.Load() {
		t.Fatal("fullSnapshotNext still armed after one snapshot")
	}

	out.Reset()
	printEvent(&out, testSnapshotEvent(t, 3))
	summary := out.String()
	if !strings.Contains(summary, "[goroutines] 2 live, 1 live threads, current G1") {
		t.Fatalf("automatic snapshot render = %q, want the summary line", summary)
	}
	if strings.Contains(summary, "goroutines: 2 live") {
		t.Fatalf("automatic snapshot render = %q, want no full view", summary)
	}
}

// A rejected request (e.g. the process isn't suspended) must disarm the full
// render, or the next automatic push would be expanded in its place.
func TestSnapshotErrorDisarmsFullRender(t *testing.T) {
	t.Cleanup(func() { fullSnapshotNext.Store(false) })

	fullSnapshotNext.Store(true)
	rejected := protocol.MustEvent(protocol.EventError, 2, protocol.ErrorPayload{
		Command: protocol.CmdGoroutineSnapshot,
		Message: "process not suspended",
	})
	var out bytes.Buffer
	printEvent(&out, rejected)
	if got := out.String(); !strings.Contains(got, "process not suspended") {
		t.Fatalf("error render = %q, want the server message", got)
	}
	if fullSnapshotNext.Load() {
		t.Fatal("fullSnapshotNext still armed after a rejected request")
	}

	out.Reset()
	printEvent(&out, testSnapshotEvent(t, 3))
	summary := out.String()
	if !strings.Contains(summary, "[goroutines] 2 live") {
		t.Fatalf("automatic snapshot render = %q, want the summary line", summary)
	}
}

// TestCountOf pins the CLI's honesty contract. Goroutine events are bounded by
// the wire contract, so printing the delivered length alone would state a
// truncated event as the live truth. The two collections have independent scan
// ceilings, so neither may borrow the other's lower-bound marker.
func TestCountOf(t *testing.T) {
	tests := []struct {
		name   string
		shown  int
		totals *protocol.SnapshotTotals
		which  collection
		want   string
	}{
		{
			name:  "complete list reports a live count",
			shown: 12, totals: nil, which: totalGoroutines,
			want: "12 live",
		},
		{
			name:   "omitted goroutines report included out of total",
			shown:  5000,
			totals: &protocol.SnapshotTotals{Goroutines: 41203, Threads: 64},
			which:  totalGoroutines,
			want:   "5000 of 41203",
		},
		{
			name:   "omitted threads report their own total",
			shown:  32,
			totals: &protocol.SnapshotTotals{Goroutines: 41203, Threads: 64},
			which:  totalThreads,
			want:   "32 of 64",
		},
		{
			name:   "a clipped goroutine scan marks its total a floor",
			shown:  10,
			totals: &protocol.SnapshotTotals{Goroutines: 10, Threads: 4, GoroutinesClipped: true},
			which:  totalGoroutines,
			want:   "10 of 10+",
		},
		{
			name:   "and does not mark the thread total",
			shown:  4,
			totals: &protocol.SnapshotTotals{Goroutines: 10, Threads: 4, GoroutinesClipped: true},
			which:  totalThreads,
			want:   "4 live",
		},
		{
			name:   "a clipped thread scan marks only threads",
			shown:  4,
			totals: &protocol.SnapshotTotals{Goroutines: 10, Threads: 4, ThreadsClipped: true},
			which:  totalThreads,
			want:   "4 of 4+",
		},
		{
			name:   "complete totals still read as live",
			shown:  10,
			totals: &protocol.SnapshotTotals{Goroutines: 10, Threads: 4},
			which:  totalGoroutines,
			want:   "10 live",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOf(tc.shown, tc.totals, tc.which); got != tc.want {
				t.Errorf("countOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

type testEditor struct {
	lines  chan *readline.Result
	closed chan struct{}
	once   sync.Once
	closes atomic.Int32
	out    bytes.Buffer
}

func newTestEditor() *testEditor {
	return &testEditor{
		lines:  make(chan *readline.Result),
		closed: make(chan struct{}),
	}
}

func (e *testEditor) Line() *readline.Result {
	select {
	case line := <-e.lines:
		return line
	case <-e.closed:
		return &readline.Result{Error: io.EOF}
	}
}

func (e *testEditor) Stdout() io.Writer { return &e.out }

func (e *testEditor) Close() error {
	e.once.Do(func() {
		e.closes.Add(1)
		close(e.closed)
	})
	return nil
}

func TestRunInteractiveStopsOnEventStreamClosure(t *testing.T) {
	editor := newTestEditor()
	events := make(chan protocol.Event)
	var transportCloses atomic.Int32
	done := make(chan struct{})
	go func() {
		runInteractive(context.Background(), editor, events, func() error {
			transportCloses.Add(1)
			return nil
		}, func(context.Context, []string) bool {
			t.Error("dispatch called after disconnect")
			return false
		})
		close(done)
	}()

	close(events)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("interactive session stayed blocked after event stream closure")
	}
	if got := editor.closes.Load(); got != 1 {
		t.Fatalf("editor close calls = %d, want 1", got)
	}
	if got := transportCloses.Load(); got != 1 {
		t.Fatalf("transport close calls = %d, want 1", got)
	}
	if got := editor.out.String(); got != "  disconnected\n" {
		t.Fatalf("output = %q, want one disconnect notice", got)
	}
}

func TestRunInteractiveCancellationIsSilentAndClosesResources(t *testing.T) {
	editor := newTestEditor()
	events := make(chan protocol.Event)
	var transportCloses atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runInteractive(ctx, editor, events, func() error {
			transportCloses.Add(1)
			return nil
		}, func(context.Context, []string) bool {
			t.Error("dispatch called after cancellation")
			return false
		})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("interactive session stayed blocked after cancellation")
	}
	if got := editor.closes.Load(); got != 1 {
		t.Fatalf("editor close calls = %d, want 1", got)
	}
	if got := transportCloses.Load(); got != 1 {
		t.Fatalf("transport close calls = %d, want 1", got)
	}
	if got := editor.out.String(); got != "" {
		t.Fatalf("output = %q, want no disconnect or goodbye on cancellation", got)
	}
}

func TestRunInteractiveReturnsTerminalDisconnectError(t *testing.T) {
	editor := newTestEditor()
	events := make(chan protocol.Event)
	close(events)
	versionErr := &protocol.VersionError{Expected: protocol.Version, Received: "999.0"}

	err := runInteractive(context.Background(), editor, events, func() error {
		return fmt.Errorf("server event: %w", versionErr)
	}, func(context.Context, []string) bool {
		t.Error("dispatch called after terminal disconnect")
		return false
	})

	var gotVersionErr *protocol.VersionError
	if !errors.As(err, &gotVersionErr) {
		t.Fatalf("runInteractive error = %v, want VersionError", err)
	}
	if got := editor.out.String(); got != "  disconnected\n" {
		t.Fatalf("output = %q, want one disconnect notice", got)
	}
	if !commandErrorSuppressed(context.Background(), err) {
		t.Fatal("terminal error should be owned by runInteractive, not a racing command")
	}
}

func TestPrintEventSurfacesOutputWithoutHandwrittenPrompt(t *testing.T) {
	event, err := protocol.NewEvent(protocol.EventOutput, 1, protocol.OutputPayload{
		Stream:  "stderr",
		Content: "debuggee output\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	printEvent(&out, event)

	if got := out.String(); got != "  [stderr] debuggee output\n" {
		t.Fatalf("output = %q", got)
	}
	if strings.Contains(out.String(), "bingo>") {
		t.Fatal("async output wrote a prompt instead of relying on readline redraw")
	}
}

type recordingLocalsClient struct {
	frames []int
}

func (c *recordingLocalsClient) Locals(frameIndex int) ([]protocol.Variable, error) {
	c.frames = append(c.frames, frameIndex)
	return nil, nil
}

func TestShowLocalsRejectsInvalidFramesBeforeDispatch(t *testing.T) {
	for _, frame := range []string{"abc", "-1"} {
		t.Run(frame, func(t *testing.T) {
			client := &recordingLocalsClient{}
			var out bytes.Buffer

			showLocals(context.Background(), client, []string{frame}, &out)

			if len(client.frames) != 0 {
				t.Fatalf("Locals called with %v", client.frames)
			}
			if !strings.Contains(out.String(), "invalid frame index") {
				t.Fatalf("output = %q", out.String())
			}
		})
	}
}

// TestFormatGoroutineListStatesWhatItOmits pins the listing's honesty. The
// goroutine list is bounded by the wire contract, so a caller that prints only
// the delivered entries presents a packed subset — or a scan that stopped early
// — as the whole runtime. The trailing count is the only thing distinguishing
// those, so it must be present and must reflect the totals.
func TestFormatGoroutineListStatesWhatItOmits(t *testing.T) {
	two := []protocol.Goroutine{{ID: 1, Status: "running"}, {ID: 2, Status: "waiting"}}

	tests := []struct {
		name string
		in   protocol.GoroutinesPayload
		want string
	}{
		{
			name: "a complete list says so",
			in:   protocol.GoroutinesPayload{Goroutines: two},
			want: "(2 live)",
		},
		{
			name: "a packed subset reports the original count",
			in: protocol.GoroutinesPayload{Goroutines: two,
				Totals: &protocol.SnapshotTotals{Goroutines: 7413}},
			want: "(2 of 7413)",
		},
		{
			name: "a clipped scan marks the total a floor",
			in: protocol.GoroutinesPayload{Goroutines: two,
				Totals: &protocol.SnapshotTotals{Goroutines: 8192, GoroutinesClipped: true}},
			want: "(2 of 8192+)",
		},
		{
			name: "a degraded read is never presented as exact",
			in: protocol.GoroutinesPayload{Goroutines: two[:1],
				Totals: &protocol.SnapshotTotals{Goroutines: 1, GoroutinesClipped: true}},
			want: "(1 of 1+)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := formatGoroutineList(tt.in)
			if !strings.Contains(out, tt.want) {
				t.Fatalf("listing does not state what it omits:\ngot:  %q\nwant substring: %q", out, tt.want)
			}
			for _, g := range tt.in.Goroutines {
				if !strings.Contains(out, fmt.Sprintf("G%d", g.ID)) {
					t.Fatalf("listing dropped G%d:\n%s", g.ID, out)
				}
			}
		})
	}
}

type blockingLocalsClient struct {
	started chan struct{}
	release chan struct{}
}

func (c *blockingLocalsClient) Locals(int) ([]protocol.Variable, error) {
	close(c.started)
	<-c.release
	return nil, errors.New("client closed")
}

func TestShowLocalsSuppressesCancellationError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &blockingLocalsClient{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		showLocals(ctx, client, nil, &out)
		close(done)
	}()

	<-client.started
	cancel()
	close(client.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("showLocals did not return after cancellation")
	}
	if got := out.String(); got != "" {
		t.Fatalf("output = %q, want silent cancellation", got)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "clean"},
		{name: "canceled", err: fmt.Errorf("connect: %w", context.Canceled)},
		{name: "failure", err: errors.New("dial failed"), want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Fatalf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestCommandErrorSuppressionUsesTypedShutdownErrors(t *testing.T) {
	shutdownErrors := []error{
		client.ErrClosed,
		io.EOF,
		io.ErrUnexpectedEOF,
		io.ErrClosedPipe,
		net.ErrClosed,
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.EPIPE,
	}
	for _, err := range shutdownErrors {
		if !commandErrorSuppressed(context.Background(), fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("typed shutdown error %v was not suppressed", err)
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if !commandErrorSuppressed(canceled, errors.New("server error")) {
		t.Fatal("context cancellation did not suppress command output")
	}
}

func TestCommandErrorSuppressionPreservesServerMessages(t *testing.T) {
	for _, phrase := range []string{
		"client closed",
		"closed network connection",
		"connection reset",
		"broken pipe",
		"websocket: close",
	} {
		err := errors.New("server: debuggee reported " + phrase)
		if commandErrorSuppressed(context.Background(), err) {
			t.Errorf("server message %q was incorrectly suppressed", phrase)
		}
	}
}
