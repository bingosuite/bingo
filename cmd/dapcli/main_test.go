package main

import (
	"bufio"
	"bytes"
	"fmt"
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
	h := &dapCLI{
		conn:      client,
		reader:    bufio.NewReader(client),
		pending:   map[int]chan godap.Message{7: response},
		bpsByFile: make(map[string][]breakpoint),
	}
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

func TestSetThreadPreservesUnknownStopIdentity(t *testing.T) {
	h := &dapCLI{curThread: 7}
	h.setThread(0)
	if got := h.thread(); got != 0 {
		t.Fatalf("thread = %d, want unknown stopped thread 0", got)
	}
}

func writeDAPFrame(buffer *bytes.Buffer, content string) {
	_, _ = fmt.Fprintf(buffer, "Content-Length: %d\r\n\r\n%s", len(content), content)
}

func writeDAPFrameTo(writer net.Conn, content string) {
	_, _ = fmt.Fprintf(writer, "Content-Length: %d\r\n\r\n%s", len(content), content)
}
