package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/dapclient"
	"github.com/bingosuite/bingo/pkg/protocol"
	godap "github.com/google/go-dap"
	. "github.com/onsi/gomega"
)

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

	response := make(chan godap.Message, 1)
	h := newDAPCLI(client)
	h.pending[7] = response
	go h.readLoop()

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
	case message := <-response:
		_, ok := message.(*godap.InitializeResponse)
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

func writeDAPFrame(buffer *bytes.Buffer, content string) {
	_, _ = fmt.Fprintf(buffer, "Content-Length: %d\r\n\r\n%s", len(content), content)
}

func writeDAPFrameTo(writer net.Conn, content string) {
	_, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(content), content)
}

// --- connection-death regressions ----------------------------------------------

// An in-flight request must not read a dead transport as a successful response.
// The read loop used to close each waiter channel, and request received without
// comma-ok, so the waiter got a nil message and returned (nil, nil).
func TestRequestInFlightFailsWhenConnectionDies(t *testing.T) {
	g := NewWithT(t)
	h, server := newPipedCLI(t)
	go h.readLoop()

	go func() {
		_, _ = readDAPMessage(bufio.NewReader(server))
		_ = server.Close()
	}()

	type result struct {
		message godap.Message
		err     error
	}
	done := make(chan result, 1)
	go func() {
		message, err := h.request("restart", &godap.RestartRequest{})
		done <- result{message, err}
	}()

	select {
	case got := <-done:
		g.Expect(got.err).To(HaveOccurred())
		g.Expect(got.err.Error()).To(ContainSubstring("restart failed"))
		g.Expect(got.err.Error()).To(ContainSubstring("connection to DAP server closed"))
		g.Expect(got.message).To(BeNil())
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never unblocked after the connection died")
	}
}

// The `restart` command prints success purely from a nil error, so a dead
// transport must not let it announce a relaunch that never happened.
func TestRestartRequestDoesNotSucceedOnDeadConnection(t *testing.T) {
	g := NewWithT(t)
	h, server := newPipedCLI(t)
	go h.readLoop()
	_ = server.Close()

	g.Eventually(h.isDisconnected).WithTimeout(2 * time.Second).Should(BeTrue())

	start := time.Now()
	message, err := h.request("restart", &godap.RestartRequest{})

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("restart failed"))
	g.Expect(err.Error()).To(ContainSubstring("connection to DAP server closed"))
	g.Expect(message).To(BeNil())
	g.Expect(time.Since(start)).To(BeNumerically("<", time.Second))
}

// The startup handshake exits on a non-nil error; a false success would drop
// the operator into a REPL bound to a dead socket.
func TestInitializeRequestDoesNotSucceedOnDeadConnection(t *testing.T) {
	g := NewWithT(t)
	h, server := newPipedCLI(t)
	go h.readLoop()
	_ = server.Close()

	g.Eventually(h.isDisconnected).WithTimeout(2 * time.Second).Should(BeTrue())

	start := time.Now()
	message, err := h.request("initialize", &godap.InitializeRequest{})

	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("initialize failed"))
	g.Expect(err.Error()).To(ContainSubstring("connection to DAP server closed"))
	g.Expect(message).To(BeNil())
	g.Expect(time.Since(start)).To(BeNumerically("<", time.Second))
}

// A request registered after the read loop has died has nobody left to answer
// it, so it must fail immediately rather than burn the full replyTimeout.
func TestRequestAfterReadLoopDeathFailsImmediately(t *testing.T) {
	g := NewWithT(t)
	h, server := newPipedCLI(t)
	go h.readLoop()
	_ = server.Close()

	g.Eventually(h.isDisconnected).WithTimeout(2 * time.Second).Should(BeTrue())

	start := time.Now()
	message, err := h.request("threads", &godap.ThreadsRequest{})
	elapsed := time.Since(start)

	g.Expect(err).To(HaveOccurred())
	g.Expect(message).To(BeNil())
	g.Expect(elapsed).To(BeNumerically("<", time.Second))
	g.Expect(elapsed).To(BeNumerically("<", replyTimeout))
	g.Expect(err.Error()).NotTo(ContainSubstring("timeout"))
}

// A response already delivered must still win when it becomes ready at the same
// moment as the transport's death, or correlation is lost on a server that
// replies and immediately closes.
func TestRequestPrefersDeliveredResponseOverConnectionDeath(t *testing.T) {
	g := NewWithT(t)
	h, server := newPipedCLI(t)

	go func() {
		request, err := readDAPMessage(bufio.NewReader(server))
		if err != nil {
			return
		}
		seq := request.(godap.RequestMessage).GetRequest().Seq
		_ = godap.WriteProtocolMessage(server, &godap.ThreadsResponse{
			Response: godap.Response{
				ProtocolMessage: godap.ProtocolMessage{Seq: 1, Type: "response"},
				RequestSeq:      seq,
				Success:         true,
				Command:         "threads",
			},
			Body: godap.ThreadsResponseBody{Threads: []godap.Thread{{Id: 1, Name: "main"}}},
		})
		_ = server.Close()
	}()
	go h.readLoop()

	message, err := h.request("threads", &godap.ThreadsRequest{})
	g.Expect(err).NotTo(HaveOccurred())
	response, ok := message.(*godap.ThreadsResponse)
	g.Expect(ok).To(BeTrue())
	g.Expect(response.Body.Threads).To(HaveLen(1))
}

// Concurrent requests racing a disconnect must all unblock with an error and
// must not trip the race detector on the pending map, done, or a waiter channel.
func TestConcurrentRequestsSurviveDisconnect(t *testing.T) {
	g := NewWithT(t)
	h, server := newPipedCLI(t)
	go h.readLoop()
	go func() {
		reader := bufio.NewReader(server)
		for i := 0; i < 4; i++ {
			if _, err := readDAPMessage(reader); err != nil {
				break
			}
		}
		_ = server.Close()
	}()

	const callers = 16
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, err := h.request("threads", &godap.ThreadsRequest{})
			errs <- err
		}()
	}
	go h.close()

	deadline := time.After(10 * time.Second)
	for i := 0; i < callers; i++ {
		select {
		case err := <-errs:
			g.Expect(err).To(HaveOccurred())
		case <-deadline:
			t.Fatalf("only %d of %d concurrent requests unblocked", i, callers)
		}
	}

	// close and the read loop both mark death; neither may double-close done.
	h.close()
	h.markDisconnected(io.EOF)
}

func (h *dapCLI) isDisconnected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.disconnected
}

func newPipedCLI(t *testing.T) (*dapCLI, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return newDAPCLI(client), server
}
