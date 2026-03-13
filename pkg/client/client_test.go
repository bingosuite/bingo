package client

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/ws"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Client Suite")
}

var _ = Describe("Client", func() {
	Describe("NewClient", func() {
		It("initializes defaults", func() {
			client := NewClient("example.com:8080", "session-1")

			Expect(client.serverURL).To(Equal("example.com:8080"))
			Expect(client.SessionID()).To(Equal("session-1"))
			Expect(client.State()).To(Equal(ws.StateReady))
			Expect(client.send).NotTo(BeNil())
			Expect(client.done).NotTo(BeNil())
		})
	})

	Describe("Run", func() {
		It("returns an error when no connection is established", func() {
			client := NewClient("example.com:8080", "session-1")
			Expect(client.Run()).To(HaveOccurred())
		})
	})

	Describe("handleMessage", func() {
		It("updates session and state based on server events", func() {
			client := NewClient("example.com:8080", "old-session")

			sessionStartedData, err := json.Marshal(ws.SessionStartedEvent{
				Type:      ws.EventSessionStarted,
				SessionID: "new-session",
				PID:       42,
			})
			Expect(err).NotTo(HaveOccurred())

			client.handleMessage(ws.Message{Type: string(ws.EventSessionStarted), Data: sessionStartedData})
			Expect(client.SessionID()).To(Equal("new-session"))

			stateData, err := json.Marshal(ws.StateUpdateEvent{
				Type:      ws.EventStateUpdate,
				SessionID: "new-session",
				NewState:  ws.StateExecuting,
			})
			Expect(err).NotTo(HaveOccurred())

			client.handleMessage(ws.Message{Type: string(ws.EventStateUpdate), Data: stateData})
			Expect(client.State()).To(Equal(ws.StateExecuting))

			hitData, err := json.Marshal(ws.BreakpointHitEvent{
				Type:      ws.EventBreakpointHit,
				SessionID: "new-session",
				Filename:  "main.go",
				Line:      12,
				Function:  "main.main",
			})
			Expect(err).NotTo(HaveOccurred())

			client.handleMessage(ws.Message{Type: string(ws.EventBreakpointHit), Data: hitData})
			Expect(client.State()).To(Equal(ws.StateBreakpoint))

			client.setState(ws.StateReady)

			initialData, err := json.Marshal(ws.InitialBreakpointHitEvent{
				Type:      ws.EventInitialBreakpoint,
				SessionID: "new-session",
				PID:       100,
			})
			Expect(err).NotTo(HaveOccurred())

			client.handleMessage(ws.Message{Type: string(ws.EventInitialBreakpoint), Data: initialData})
			Expect(client.State()).To(Equal(ws.StateBreakpoint))
		})
	})

	Describe("command helpers", func() {
		var client *Client

		BeforeEach(func() {
			client = NewClient("example.com:8080", "session-1")
		})

		assertEnqueued := func(invoke func() error, wantType string, decode func([]byte)) {
			GinkgoHelper()
			Expect(invoke()).To(Succeed())

			var msg ws.Message
			Eventually(client.send, time.Second).Should(Receive(&msg))
			Expect(msg.Type).To(Equal(wantType))
			decode(msg.Data)
		}

		It("enqueues continue commands", func() {
			assertEnqueued(client.Continue, string(ws.CmdContinue), func(body []byte) {
				var cmd ws.ContinueCmd
				Expect(json.Unmarshal(body, &cmd)).To(Succeed())
				Expect(cmd.Type).To(Equal(ws.CmdContinue))
				Expect(cmd.SessionID).To(Equal("session-1"))
			})
		})

		It("enqueues step over commands", func() {
			assertEnqueued(client.StepOver, string(ws.CmdStepOver), func(body []byte) {
				var cmd ws.StepOverCmd
				Expect(json.Unmarshal(body, &cmd)).To(Succeed())
				Expect(cmd.Type).To(Equal(ws.CmdStepOver))
				Expect(cmd.SessionID).To(Equal("session-1"))
			})
		})

		It("enqueues start debug commands", func() {
			assertEnqueued(func() error { return client.StartDebug("./bin/app") }, string(ws.CmdStartDebug), func(body []byte) {
				var cmd ws.StartDebugCmd
				Expect(json.Unmarshal(body, &cmd)).To(Succeed())
				Expect(cmd.Type).To(Equal(ws.CmdStartDebug))
				Expect(cmd.SessionID).To(Equal("session-1"))
				Expect(cmd.TargetPath).To(Equal("./bin/app"))
			})
		})

		It("enqueues stop commands", func() {
			assertEnqueued(client.Stop, string(ws.CmdExit), func(body []byte) {
				var cmd ws.ExitCmd
				Expect(json.Unmarshal(body, &cmd)).To(Succeed())
				Expect(cmd.Type).To(Equal(ws.CmdExit))
				Expect(cmd.SessionID).To(Equal("session-1"))
			})
		})

		It("enqueues set breakpoint commands", func() {
			assertEnqueued(func() error { return client.SetBreakpoint("main.go", 27) }, string(ws.CmdSetBreakpoint), func(body []byte) {
				var cmd ws.SetBreakpointCmd
				Expect(json.Unmarshal(body, &cmd)).To(Succeed())
				Expect(cmd.Type).To(Equal(ws.CmdSetBreakpoint))
				Expect(cmd.SessionID).To(Equal("session-1"))
				Expect(cmd.Filename).To(Equal("main.go"))
				Expect(cmd.Line).To(Equal(27))
			})
		})
	})

	Describe("Wait", func() {
		It("returns ErrDisconnected when the done channel is closed", func() {
			client := NewClient("example.com:8080", "session-1")
			close(client.done)

			Expect(client.Wait()).To(Equal(ErrDisconnected))
		})
	})
})
