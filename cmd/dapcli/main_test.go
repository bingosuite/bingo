package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/dapclient"
	"github.com/bingosuite/bingo/pkg/protocol"
	godap "github.com/google/go-dap"
	. "github.com/onsi/gomega"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "clean"},
		{name: "canceled", err: fmt.Errorf("initialize: %w", context.Canceled)},
		{name: "disconnected", err: fmt.Errorf("request: %w", errDAPDisconnected)},
		{name: "failure", err: errors.New("malformed DAP frame"), want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Fatalf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestReadDAPMessageAcceptsSessionAnnouncement(t *testing.T) {
	g := NewWithT(t)
	var stream bytes.Buffer
	writeDAPFrame(&stream, fmt.Sprintf(
		`{"seq":1,"type":"event","event":%q,"body":{"version":%d,"sessionId":"session-1"}}`,
		protocol.DAPSessionEventName,
		protocol.DAPSessionEventVersion,
	))
	writeDAPFrame(&stream, `{"seq":2,"type":"event","event":"initialized"}`)
	reader := bufio.NewReader(&stream)

	message, err := readDAPMessage(reader)
	g.Expect(err).NotTo(HaveOccurred())
	announcement, ok := message.(*dapclient.SessionEvent)
	g.Expect(ok).To(BeTrue())
	g.Expect(announcement.Event.Event).To(Equal(protocol.DAPSessionEventName))
	g.Expect(announcement.Body.Version).To(Equal(protocol.DAPSessionEventVersion))
	g.Expect(announcement.Body.SessionID).To(Equal("session-1"))

	message, err = readDAPMessage(reader)
	g.Expect(err).NotTo(HaveOccurred())
	_, ok = message.(*godap.InitializedEvent)
	g.Expect(ok).To(BeTrue())
}

func TestReadLoopContinuesAfterSessionAnnouncement(t *testing.T) {
	g := NewWithT(t)
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})

	h := newTestDAPCLI(client, io.Discard, func() {})
	pending := make(chan pendingResult, 1)
	h.pending[7] = pending
	go h.readLoop()
	t.Cleanup(func() {
		h.close()
		<-h.readDone
	})

	go func() {
		writeDAPFrameTo(server, fmt.Sprintf(
			`{"seq":1,"type":"event","event":%q,"body":{"version":%d,"sessionId":"session-live"}}`,
			protocol.DAPSessionEventName,
			protocol.DAPSessionEventVersion,
		))
		_ = godap.WriteProtocolMessage(server, &godap.InitializeResponse{
			Response: godap.Response{
				ProtocolMessage: godap.ProtocolMessage{Seq: 2, Type: "response"},
				RequestSeq:      7,
				Success:         true,
				Command:         "initialize",
			},
		})
	}()

	select {
	case result := <-pending:
		_, ok := result.message.(*godap.InitializeResponse)
		g.Expect(ok).To(BeTrue())
	case <-time.After(time.Second):
		t.Fatal("normal response was not delivered after custom event")
	}
	g.Eventually(func() string {
		h.stateMu.Lock()
		defer h.stateMu.Unlock()
		return h.sessionID
	}).WithTimeout(time.Second).Should(Equal("session-live"))
}

func TestSetThreadPreservesUnknownStopIdentity(t *testing.T) {
	h := &dapCLI{curThread: 7}
	h.setThread(0)
	if got := h.thread(); got != 0 {
		t.Fatalf("thread = %d, want unknown stopped thread 0", got)
	}
}

func TestOutputEventUsesAsyncWriter(t *testing.T) {
	var out bytes.Buffer
	h := &dapCLI{out: &out}

	h.onEvent(&godap.OutputEvent{
		Body: godap.OutputEventBody{
			Category: "stderr",
			Output:   "debuggee output\n",
		},
	})

	if got := out.String(); got != "  [stderr] debuggee output\n" {
		t.Fatalf("output = %q", got)
	}
	if strings.Contains(out.String(), prompt) {
		t.Fatal("async output wrote a prompt instead of relying on readline redraw")
	}
}

func TestRequestFailsWhenTransportCloses(t *testing.T) {
	server, client := net.Pipe()
	var out bytes.Buffer
	var editorCloses atomic.Int32
	h := newTestDAPCLI(client, &out, func() { editorCloses.Add(1) })
	go h.readLoop()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_, _ = readDAPMessage(bufio.NewReader(server))
		_ = server.Close()
	}()

	_, err := h.request("threads", &godap.ThreadsRequest{})
	if !errors.Is(err, errDAPDisconnected) {
		t.Fatalf("request error = %v, want DAP disconnect", err)
	}
	select {
	case <-h.readDone:
	case <-time.After(time.Second):
		t.Fatal("read loop did not stop after transport EOF")
	}
	<-serverDone

	if got := editorCloses.Load(); got != 1 {
		t.Fatalf("editor close calls = %d, want 1", got)
	}
	if got := out.String(); got != "  disconnected\n" {
		t.Fatalf("output = %q, want one disconnect notice", got)
	}
}

func TestIntentionalCloseDoesNotReportDisconnect(t *testing.T) {
	server, client := net.Pipe()
	var out bytes.Buffer
	var editorCloses atomic.Int32
	h := newTestDAPCLI(client, &out, func() { editorCloses.Add(1) })
	go h.readLoop()

	h.close()
	_ = server.Close()
	select {
	case <-h.readDone:
	case <-time.After(time.Second):
		t.Fatal("read loop did not stop after intentional close")
	}

	if got := editorCloses.Load(); got != 0 {
		t.Fatalf("editor close calls = %d, want caller-owned close", got)
	}
	if got := out.String(); got != "" {
		t.Fatalf("output = %q, want no disconnect notice", got)
	}
}

func TestReadLoopDoesNotTreatProtocolErrorContainingClosedAsDisconnect(t *testing.T) {
	server, client := net.Pipe()
	var out bytes.Buffer
	h := newTestDAPCLI(client, &out, func() {})
	go h.readLoop()

	writeDAPFrameTo(server, `{"seq":1,"type":"closed"}`)
	select {
	case <-h.readDone:
	case <-time.After(time.Second):
		t.Fatal("read loop did not stop after malformed DAP message")
	}
	_ = server.Close()

	readErr, intentional := h.readOutcome()
	if readErr == nil || intentional {
		t.Fatalf("read outcome = (%v, %v), want unintentional protocol failure", readErr, intentional)
	}
	if err := sessionEndError(readErr, intentional); err == nil {
		t.Fatal("protocol error containing closed was treated as graceful")
	}
	if got := out.String(); !strings.Contains(got, "connection closed:") {
		t.Fatalf("output = %q, want protocol failure notice", got)
	}
}

func TestSessionEndErrorClassifiesOnlyUnexpectedFailures(t *testing.T) {
	if err := sessionEndError(io.EOF, false); err != nil {
		t.Fatalf("EOF error = %v, want graceful end", err)
	}
	if err := sessionEndError(errors.New("malformed DAP frame"), true); err != nil {
		t.Fatalf("intentional close error = %v, want graceful end", err)
	}
	if err := sessionEndError(errors.New("malformed DAP frame"), false); err == nil {
		t.Fatal("unexpected protocol failure was treated as graceful")
	}
}

func TestRequestErrorIsSuppressedAfterConnectionEnds(t *testing.T) {
	var out bytes.Buffer
	h := &dapCLI{
		out:          &out,
		disconnected: make(chan struct{}),
	}
	close(h.disconnected)

	h.printRequestError("threads", errDAPDisconnected)

	if got := out.String(); got != "" {
		t.Fatalf("output = %q, want disconnect notice to own the error", got)
	}
}

func TestReadLoopPublishesConnectionBeforeWakingRequest(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	var out bytes.Buffer
	h := newTestDAPCLI(client, &out, func() {})
	go h.readLoop()

	requestDone := make(chan error, 1)
	go func() {
		_, err := h.request("threads", &godap.ThreadsRequest{})
		h.printRequestError("threads", err)
		requestDone <- err
	}()

	if _, err := readDAPMessage(bufio.NewReader(server)); err != nil {
		t.Fatalf("read request: %v", err)
	}

	h.readMu.Lock()
	writeDAPFrameTo(server, `{"seq":1,"type":"closed"}`)
	runtime.Gosched()
	var requestErr error
	wokeBeforePublication := false
	select {
	case requestErr = <-requestDone:
		wokeBeforePublication = true
	default:
	}
	h.readMu.Unlock()

	select {
	case <-h.readDone:
	case <-time.After(time.Second):
		t.Fatal("read loop did not stop after protocol failure")
	}
	if !wokeBeforePublication {
		select {
		case requestErr = <-requestDone:
		case <-time.After(time.Second):
			t.Fatal("request waiter did not wake after protocol failure")
		}
	}
	if requestErr == nil {
		t.Fatal("request returned nil after protocol failure")
	}
	if wokeBeforePublication {
		t.Error("request waiter woke before connection state was published")
	}

	if got := strings.Count(out.String(), "connection closed:"); got != 1 {
		t.Fatalf("connection-closed notices = %d, output %q", got, out.String())
	}
	if strings.Contains(out.String(), "[error] threads:") {
		t.Fatalf("request printed a second error before disconnect publication: %q", out.String())
	}
	readErr, intentional := h.readOutcome()
	if readErr == nil || intentional {
		t.Fatalf("read outcome = (%v, %v), want unintentional protocol failure", readErr, intentional)
	}
	if err := sessionEndError(readErr, intentional); err == nil {
		t.Fatal("unexpected write/read failure was downgraded to graceful exit")
	}
}

func TestRequestDoesNotRegisterAfterConnectionEnds(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })
	h := newTestDAPCLI(client, io.Discard, func() {})
	h.endConnection(io.EOF)

	start := time.Now()
	_, err := h.request("threads", &godap.ThreadsRequest{})
	if !errors.Is(err, errDAPDisconnected) {
		t.Fatalf("request error = %v, want DAP disconnect", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("request waited %v after terminal disconnect", elapsed)
	}
	h.mu.Lock()
	seq := h.seq
	h.mu.Unlock()
	if seq != 0 {
		t.Fatalf("request sequence = %d, want no registration", seq)
	}
}

func TestDispatchRejectsInvalidFramesBeforeRequest(t *testing.T) {
	for _, frame := range []string{"abc", "-1"} {
		t.Run(frame, func(t *testing.T) {
			h := &dapCLI{}

			if h.dispatch([]string{"locals", frame}) {
				t.Fatal("invalid locals command requested exit")
			}

			h.mu.Lock()
			seq := h.seq
			h.mu.Unlock()
			if seq != 0 {
				t.Fatalf("request sequence = %d, want no request", seq)
			}
		})
	}
}

func newTestDAPCLI(conn net.Conn, out io.Writer, closeEditor func()) *dapCLI {
	return &dapCLI{
		conn:         conn,
		reader:       bufio.NewReader(conn),
		pending:      make(map[int]chan pendingResult),
		bpsByFile:    make(map[string][]breakpoint),
		out:          out,
		closeEditor:  closeEditor,
		disconnected: make(chan struct{}),
		readDone:     make(chan struct{}),
	}
}

func writeDAPFrame(buffer *bytes.Buffer, content string) {
	_, _ = fmt.Fprintf(buffer, "Content-Length: %d\r\n\r\n%s", len(content), content)
}

func writeDAPFrameTo(writer net.Conn, content string) {
	_, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(content), content)
}
