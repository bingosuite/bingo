package hub_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestHub(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Hub Suite")
}

type fakeDebugger struct {
	mu     sync.Mutex
	events chan protocol.Event
	calls  []string

	launchErr        error
	attachErr        error
	killErr          error
	setBPResult      protocol.Breakpoint
	setBPErr         error
	setBPGate        chan struct{}
	setBPStarted     chan struct{}
	setBPStartOnce   sync.Once
	setBPFunc        func(file string, line int) (protocol.Breakpoint, error)
	clearBPErr       error
	continueErr      error
	emitContinued    bool
	stepOverErr      error
	stepIntoErr      error
	stepOutErr       error
	pauseErr         error
	localsResult     []protocol.Variable
	evalResult       protocol.Variable
	framesResult     []protocol.Frame
	goroutinesResult protocol.GoroutinesPayload
	snapshotResult   protocol.GoroutineSnapshotPayload
}

func newFakeDebugger() *fakeDebugger {
	return &fakeDebugger{events: make(chan protocol.Event, 32)}
}

func (f *fakeDebugger) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeDebugger) recordedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]string, len(f.calls))
	copy(cp, f.calls)
	return cp
}

func (f *fakeDebugger) push(evt protocol.Event) { f.events <- evt }

func (f *fakeDebugger) closeEvents() { close(f.events) }

func (f *fakeDebugger) Events() <-chan protocol.Event { return f.events }
func (f *fakeDebugger) Launch(p string, a []string, env []string) error {
	f.record("Launch")
	return f.launchErr
}
func (f *fakeDebugger) Attach(pid int, binaryPath string) error {
	f.record("Attach")
	return f.attachErr
}
func (f *fakeDebugger) Kill() error {
	f.record("Kill")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.killErr
}

func (f *fakeDebugger) setKillError(err error) {
	f.mu.Lock()
	f.killErr = err
	f.mu.Unlock()
}
func (f *fakeDebugger) Continue() error {
	f.record("Continue")
	if f.continueErr == nil && f.emitContinued {
		f.push(protocol.MustEvent(protocol.EventContinued, 0, protocol.ContinuedPayload{}))
	}
	return f.continueErr
}
func (f *fakeDebugger) StepOver() error { f.record("StepOver"); return f.stepOverErr }
func (f *fakeDebugger) StepInto() error { f.record("StepInto"); return f.stepIntoErr }
func (f *fakeDebugger) StepOut() error  { f.record("StepOut"); return f.stepOutErr }
func (f *fakeDebugger) Pause() error    { f.record("Pause"); return f.pauseErr }
func (f *fakeDebugger) ClearBreakpoint(id int) error {
	f.record("ClearBreakpoint")
	return f.clearBPErr
}
func (f *fakeDebugger) SetBreakpoint(file string, line int) (protocol.Breakpoint, error) {
	f.record("SetBreakpoint")
	if f.setBPStarted != nil {
		f.setBPStartOnce.Do(func() { close(f.setBPStarted) })
	}
	if f.setBPGate != nil {
		<-f.setBPGate
	}
	if f.setBPFunc != nil {
		return f.setBPFunc(file, line)
	}
	return f.setBPResult, f.setBPErr
}
func (f *fakeDebugger) Locals(fi int) ([]protocol.Variable, error) {
	f.record("Locals")
	return f.localsResult, nil
}
func (f *fakeDebugger) Evaluate(fi int, name string) (protocol.Variable, error) {
	f.record("Evaluate")
	return f.evalResult, nil
}
func (f *fakeDebugger) StackFrames() ([]protocol.Frame, error) {
	f.record("StackFrames")
	return f.framesResult, nil
}
func (f *fakeDebugger) Goroutines() (protocol.GoroutinesPayload, error) {
	f.record("Goroutines")
	return f.goroutinesResult, nil
}
func (f *fakeDebugger) GoroutineSnapshot() (protocol.GoroutineSnapshotPayload, error) {
	f.record("GoroutineSnapshot")
	return f.snapshotResult, nil
}

type blockingLaunchDebugger struct {
	*fakeDebugger
	started chan struct{}
	release <-chan struct{}
}

func (f *blockingLaunchDebugger) Launch(string, []string, []string) error {
	f.record("Launch")
	close(f.started)
	<-f.release
	return f.launchErr
}

type blockingKillDebugger struct {
	*fakeDebugger
	started chan struct{}
	release <-chan struct{}
}

func (f *blockingKillDebugger) Kill() error {
	f.record("Kill")
	close(f.started)
	<-f.release
	return nil
}

// leakyLaunchDebugger models a partially started engine: Launch acquires a
// goroutine that only a disposal Kill can release — the same shape as the real
// engine's LockOSThread'd loop, whose sole exit is Kill driving it to
// stateExited (internal/debugger/engine.go) — and only then reports failure.
// A hub that abandons it without killing it strands that goroutine, so exited
// never closes.
type leakyLaunchDebugger struct {
	*fakeDebugger
	releaseOnce sync.Once
	released    chan struct{}
	exited      chan struct{}
}

func newLeakyLaunchDebugger(launchErr error) *leakyLaunchDebugger {
	d := &leakyLaunchDebugger{
		fakeDebugger: newFakeDebugger(),
		released:     make(chan struct{}),
		exited:       make(chan struct{}),
	}
	d.launchErr = launchErr
	return d
}

func (d *leakyLaunchDebugger) Launch(string, []string, []string) error {
	d.record("Launch")
	go func() {
		<-d.released
		close(d.exited)
	}()
	return d.launchErr
}

func (d *leakyLaunchDebugger) Kill() error {
	d.record("Kill")
	d.releaseOnce.Do(func() { close(d.released) })
	return d.killErr
}

type fakeWSConn struct {
	mu       sync.Mutex
	incoming chan []byte // messages written by the server (server → client)
	outgoing chan []byte // messages injected by the test  (client → server)
	writes   []fakeWSWrite
	closed   bool
}

type fakeWSWrite struct {
	messageType int
	data        []byte
}

func newFakeWSConn() *fakeWSConn {
	return newFakeWSConnWithOutgoingBuffer(32)
}

func newFakeWSConnWithOutgoingBuffer(size int) *fakeWSConn {
	return &fakeWSConn{
		// Large buffer so WriteMessage (called from the hub's event loop)
		// never blocks if a test doesn't drain — blocking would deadlock.
		incoming: make(chan []byte, 256),
		outgoing: make(chan []byte, size),
	}
}

func mustAddClient(h *hub.Hub, conn *fakeWSConn) {
	_, err := h.AddClient(conn, nil)
	Expect(err).NotTo(HaveOccurred())
}

func (f *fakeWSConn) recv() ([]byte, bool) {
	select {
	case msg := <-f.incoming:
		return msg, true
	case <-time.After(300 * time.Millisecond):
		return nil, false
	}
}

func (f *fakeWSConn) inject(cmd protocol.Command) {
	data, _ := json.Marshal(cmd)
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if !closed {
		f.outgoing <- data
	}
}

func (f *fakeWSConn) ReadMessage() (int, []byte, error) {
	data, ok := <-f.outgoing
	if !ok {
		return 0, nil, &connClosedErr{}
	}
	return hub.TextMessage, data, nil
}

func (f *fakeWSConn) WriteMessage(messageType int, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return &connClosedErr{}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.writes = append(f.writes, fakeWSWrite{messageType: messageType, data: cp})
	if messageType == hub.TextMessage {
		f.incoming <- cp
	}
	return nil
}

func (f *fakeWSConn) SetReadLimit(int64)                {}
func (f *fakeWSConn) SetReadDeadline(time.Time) error   { return nil }
func (f *fakeWSConn) SetWriteDeadline(time.Time) error  { return nil }
func (f *fakeWSConn) SetPongHandler(func(string) error) {}

func (f *fakeWSConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.outgoing)
	}
	return nil
}

func (f *fakeWSConn) closeFrame() (int, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.writes) - 1; i >= 0; i-- {
		write := f.writes[i]
		if write.messageType != hub.CloseMessage || len(write.data) < 2 {
			continue
		}
		code := int(write.data[0])<<8 | int(write.data[1])
		return code, string(write.data[2:]), true
	}
	return 0, "", false
}

type connClosedErr struct{}

func (e *connClosedErr) Error() string { return "use of closed network connection" }

func mustCommand(kind protocol.CommandKind, payload any) protocol.Command {
	raw, _ := json.Marshal(payload)
	return protocol.Command{Version: protocol.Version, Kind: kind, Payload: raw}
}

func decodeEvent(data []byte) protocol.Event {
	var evt protocol.Event
	ExpectWithOffset(1, json.Unmarshal(data, &evt)).To(Succeed())
	return evt
}

func runHub(h *hub.Hub) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	return cancel
}

func recvEvent(conn *fakeWSConn) (protocol.Event, bool) {
	msg, ok := conn.recv()
	if !ok {
		return protocol.Event{}, false
	}
	return decodeEvent(msg), true
}

func closeFakeWS(conn *fakeWSConn) {
	ExpectWithOffset(1, conn.Close()).To(Succeed())
}

var _ = Describe("Hub", func() {

	var (
		fd     *fakeDebugger
		h      *hub.Hub
		cancel context.CancelFunc
	)

	BeforeEach(func() {
		fd = newFakeDebugger()
		h = hub.New(fd, nil)
		cancel = runHub(h)
	})

	AfterEach(func() {
		cancel()
		func() {
			defer func() { _ = recover() }()
			close(fd.events)
		}()
		select {
		case <-h.Done():
		case <-time.After(2 * time.Second):
		}
	})

	Describe("AddClient", func() {
		It("accepts multiple concurrent clients without panicking", func() {
			conn1 := newFakeWSConn()
			conn2 := newFakeWSConn()
			_, err := h.AddClient(conn1, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = h.AddClient(conn2, nil)
			Expect(err).NotTo(HaveOccurred())
			closeFakeWS(conn1)
			closeFakeWS(conn2)
		})

		It("does not panic when managed-session welcome delivery races shutdown", func() {
			for i := 0; i < 50; i++ {
				managed := hub.NewSession("session", func() debugger.Debugger { return fd }, nil)
				cancelManaged := runHub(managed)
				conn := newFakeWSConn()
				done := make(chan struct{})

				go func() {
					defer GinkgoRecover()
					_, err := managed.AddClient(conn, nil)
					Expect(err).To(Or(BeNil(), MatchError(hub.ErrHubClosed)))
					close(done)
				}()
				cancelManaged()

				Eventually(done, "500ms", "10ms").Should(BeClosed())
				closeFakeWS(conn)
				Eventually(managed.Done(), "500ms", "10ms").Should(BeClosed())
			}
		})

		It("rejects and closes a client after teardown begins", func() {
			cancel()
			Eventually(h.Done(), "500ms", "10ms").Should(BeClosed())

			conn := newFakeWSConn()
			client, err := h.AddClient(conn, nil)

			Expect(err).To(MatchError(hub.ErrHubClosed))
			Expect(client).To(BeNil())
			Expect(h.ClientCount()).To(Equal(0))
			conn.mu.Lock()
			closed := conn.closed
			conn.mu.Unlock()
			Expect(closed).To(BeTrue())
		})
	})

	Describe("managed session start failures", func() {
		It("tears down a failed debugger so a later launch can retry", func() {
			managed := hub.NewSession("session", func() debugger.Debugger { return fd }, nil)
			cancelManaged := runHub(managed)
			defer cancelManaged()

			conn := newFakeWSConn()
			_, err := managed.AddClient(conn, nil)
			Expect(err).NotTo(HaveOccurred())
			_, _ = recvEvent(conn)

			fd.launchErr = errors.New("launch failed")
			conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "bad"}))
			Eventually(fd.recordedCalls, "500ms", "10ms").Should(ContainElement("Kill"))
			evt, ok := recvEvent(conn)
			Expect(ok).To(BeTrue())
			Expect(evt.Kind).To(Equal(protocol.EventError))

			fd.launchErr = nil
			conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "good"}))

			Eventually(func() int {
				count := 0
				for _, call := range fd.recordedCalls() {
					if call == "Launch" {
						count++
					}
				}
				return count
			}, "500ms", "10ms").Should(Equal(2))
		})
	})

	Describe("wire protocol version enforcement", func() {
		It("rejects an incompatible command before execution without affecting another client", func() {
			bad := newFakeWSConn()
			good := newFakeWSConn()
			mustAddClient(h, bad)
			mustAddClient(h, good)
			fd.setBPResult = protocol.Breakpoint{ID: 1}

			cmd := mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 42})
			cmd.Version = "999.0"
			bad.inject(cmd)

			Eventually(h.ClientCount, "500ms", "10ms").Should(Equal(1))
			Eventually(func() bool {
				_, _, ok := bad.closeFrame()
				return ok
			}, "500ms", "10ms").Should(BeTrue())
			code, reason, _ := bad.closeFrame()
			Expect(code).To(Equal(1002))
			Expect(reason).To(Equal(protocol.ValidateVersion("999.0").Error()))
			Consistently(fd.recordedCalls, "100ms", "10ms").
				ShouldNot(ContainElement("SetBreakpoint"))
			Expect(len(good.incoming)).To(Equal(0),
				"an incompatible peer must not broadcast an error to other clients")

			good.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 42}))
			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("SetBreakpoint"))
			evt, ok := recvEvent(good)
			Expect(ok).To(BeTrue())
			Expect(evt.Kind).To(Equal(protocol.EventBreakpointSet))
			Expect(evt.Seq).To(Equal(uint64(1)),
				"rejecting one peer must not consume a shared hub sequence")
		})

		It("cannot resume a suspended session with an incompatible command", func() {
			bad := newFakeWSConn()
			good := newFakeWSConn()
			mustAddClient(h, bad)
			mustAddClient(h, good)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, ok := recvEvent(bad)
			Expect(ok).To(BeTrue())
			stop, ok := recvEvent(good)
			Expect(ok).To(BeTrue())

			cmd := mustCommand(protocol.CmdContinue, struct{}{})
			cmd.Version = "999.0"
			bad.inject(cmd)

			Eventually(h.ClientCount, "500ms", "10ms").Should(Equal(1))
			Consistently(fd.recordedCalls, "100ms", "10ms").
				ShouldNot(ContainElement("Continue"))
			Expect(h.State()).To(Equal(protocol.StateSuspended))

			good.inject(mustCommand(protocol.CmdContinue, struct{}{}))
			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Continue"))

			fd.push(protocol.MustEvent(protocol.EventOutput, 2,
				protocol.OutputPayload{Content: "still connected"}))
			next, ok := recvEvent(good)
			Expect(ok).To(BeTrue())
			Expect(next.Kind).To(Equal(protocol.EventOutput))
			Expect(next.Seq).To(Equal(stop.Seq+1),
				"rejecting a resume must not create a private event or sequence gap")
		})
	})

	Describe("event sequence numbers", func() {
		It("keeps existing clients on the managed-session sequence stream when another client joins", func() {
			managedFD := newFakeDebugger()
			managed := hub.NewSession("session", func() debugger.Debugger { return managedFD }, nil)
			cancelManaged := runHub(managed)
			defer cancelManaged()

			conn1 := newFakeWSConn()
			mustAddClient(managed, conn1)
			first, ok := recvEvent(conn1)
			Expect(ok).To(BeTrue())
			Expect(first.Kind).To(Equal(protocol.EventSessionState))
			Expect(first.Seq).To(Equal(uint64(1)))

			conn2 := newFakeWSConn()
			mustAddClient(managed, conn2)

			existingState, ok := recvEvent(conn1)
			Expect(ok).To(BeTrue())
			joiningState, ok := recvEvent(conn2)
			Expect(ok).To(BeTrue())
			Expect(existingState.Kind).To(Equal(protocol.EventSessionState))
			Expect(joiningState.Kind).To(Equal(protocol.EventSessionState))
			Expect(existingState.Seq).To(Equal(first.Seq + 1))
			Expect(joiningState.Seq).To(Equal(existingState.Seq))
		})

		It("serializes concurrent joins and broadcasts into contiguous client streams", func() {
			managedFD := newFakeDebugger()
			managed := hub.NewSession("session", func() debugger.Debugger { return managedFD }, nil)
			cancelManaged := runHub(managed)
			defer cancelManaged()

			const (
				joiners = 24
				outputs = 64
			)

			conns := make([]*fakeWSConn, joiners+1)
			conns[0] = newFakeWSConn()
			mustAddClient(managed, conns[0])
			welcome, ok := recvEvent(conns[0])
			Expect(ok).To(BeTrue())
			Expect(welcome.Seq).To(Equal(uint64(1)))

			conns[0].inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "stress"}))
			running, ok := recvEvent(conns[0])
			Expect(ok).To(BeTrue())
			Expect(running.Kind).To(Equal(protocol.EventSessionState))
			Expect(running.Seq).To(Equal(welcome.Seq + 1))

			start := make(chan struct{})
			addErrs := make(chan error, joiners)
			var joins sync.WaitGroup
			joins.Add(joiners)
			for i := 1; i <= joiners; i++ {
				conns[i] = newFakeWSConn()
				go func(conn *fakeWSConn) {
					defer joins.Done()
					<-start
					_, err := managed.AddClient(conn, nil)
					addErrs <- err
				}(conns[i])
			}

			emitted := make(chan struct{})
			go func() {
				<-start
				for i := 0; i < outputs; i++ {
					managedFD.push(protocol.MustEvent(protocol.EventOutput, uint64(i+1),
						protocol.OutputPayload{Content: fmt.Sprintf("output-%d", i)}))
				}
				close(emitted)
			}()

			close(start)
			joins.Wait()
			for i := 0; i < joiners; i++ {
				Expect(<-addErrs).NotTo(HaveOccurred())
			}
			Eventually(emitted, "2s").Should(BeClosed())

			managedFD.push(protocol.MustEvent(protocol.EventOutput, 1,
				protocol.OutputPayload{Content: "sentinel"}))

			for i, conn := range conns {
				previous := uint64(0)
				if i == 0 {
					previous = running.Seq
				} else {
					first, ok := recvEvent(conn)
					Expect(ok).To(BeTrue(), "client %d did not receive its join state", i)
					Expect(first.Kind).To(Equal(protocol.EventSessionState))
					Expect(first.Seq).To(BeNumerically(">=", uint64(1)))
					previous = first.Seq
				}

				for {
					evt, ok := recvEvent(conn)
					Expect(ok).To(BeTrue(), "client %d stream ended before sentinel", i)
					Expect(evt.Seq).To(Equal(previous+1),
						"client %d received a non-contiguous stream", i)
					previous = evt.Seq

					if evt.Kind != protocol.EventOutput {
						continue
					}
					var payload protocol.OutputPayload
					Expect(protocol.DecodeEventPayload(evt, &payload)).To(Succeed())
					if payload.Content == "sentinel" {
						break
					}
				}
			}
		})

		It("admits a client while Run is waiting for a suspended-session resume", func() {
			managedFD := newFakeDebugger()
			managed := hub.NewSession("session", func() debugger.Debugger { return managedFD }, nil)
			cancelManaged := runHub(managed)
			defer cancelManaged()

			conn1 := newFakeWSConn()
			mustAddClient(managed, conn1)
			_, _ = recvEvent(conn1)
			conn1.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "paused"}))
			_, _ = recvEvent(conn1)

			managedFD.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			waitForEventKind(conn1, protocol.EventBreakpointHit, nil)
			suspended, ok := recvEvent(conn1)
			Expect(ok).To(BeTrue())
			Expect(suspended.Kind).To(Equal(protocol.EventSessionState))

			conn2 := newFakeWSConn()
			addResult := make(chan error, 1)
			go func() {
				_, err := managed.AddClient(conn2, nil)
				addResult <- err
			}()
			Eventually(addResult, "500ms").Should(Receive(BeNil()))

			existingState, ok := recvEvent(conn1)
			Expect(ok).To(BeTrue())
			joiningState, ok := recvEvent(conn2)
			Expect(ok).To(BeTrue())
			Expect(existingState.Seq).To(Equal(suspended.Seq + 1))
			Expect(joiningState.Seq).To(Equal(existingState.Seq))

			var payload protocol.SessionStatePayload
			Expect(protocol.DecodeEventPayload(joiningState, &payload)).To(Succeed())
			Expect(payload.State).To(Equal(protocol.StateSuspended))
			Expect(payload.Clients).To(Equal(2))
		})

		It("assigns strictly increasing hub-managed seq to all outbound events", func() {
			conn := newFakeWSConn()
			_, err := h.AddClient(conn, nil)
			Expect(err).NotTo(HaveOccurred())

			// Engine-level seq values 99 and 100 must both be rewritten.
			evt1, _ := protocol.NewEvent(protocol.EventOutput, 99,
				protocol.OutputPayload{Stream: "stdout", Content: "first"})
			evt2, _ := protocol.NewEvent(protocol.EventOutput, 100,
				protocol.OutputPayload{Stream: "stdout", Content: "second"})
			fd.push(evt1)
			fd.push(evt2)

			e1, ok1 := recvEvent(conn)
			e2, ok2 := recvEvent(conn)
			Expect(ok1).To(BeTrue())
			Expect(ok2).To(BeTrue())
			Expect(e1.Seq).To(BeNumerically(">", uint64(0)))
			Expect(e2.Seq).To(BeNumerically(">", e1.Seq),
				"hub seq must be strictly increasing")
		})

		It("interleaves debugger events and confirmation events in a single seq stream", func() {
			conn := newFakeWSConn()
			_, err := h.AddClient(conn, nil)
			Expect(err).NotTo(HaveOccurred())
			fd.setBPResult = protocol.Breakpoint{ID: 1}

			fd.push(protocol.MustEvent(protocol.EventOutput, 1,
				protocol.OutputPayload{Content: "x"}))
			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 1}))

			e1, ok1 := recvEvent(conn)
			e2, ok2 := recvEvent(conn)
			Expect(ok1).To(BeTrue())
			Expect(ok2).To(BeTrue())
			Expect(e2.Seq).To(BeNumerically(">", e1.Seq))
		})
	})

	Describe("event broadcast", func() {
		It("delivers an informational event to all connected clients", func() {
			conn1 := newFakeWSConn()
			conn2 := newFakeWSConn()
			_, err := h.AddClient(conn1, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = h.AddClient(conn2, nil)
			Expect(err).NotTo(HaveOccurred())

			fd.push(protocol.MustEvent(protocol.EventOutput, 1,
				protocol.OutputPayload{Stream: "stdout", Content: "hello bingo"}))

			e1, ok1 := recvEvent(conn1)
			e2, ok2 := recvEvent(conn2)
			Expect(ok1).To(BeTrue(), "conn1 should receive the event")
			Expect(ok2).To(BeTrue(), "conn2 should receive the event")
			Expect(e1.Kind).To(Equal(protocol.EventOutput))
			Expect(e2.Kind).To(Equal(protocol.EventOutput))
		})

		It("does not deliver events to clients that disconnected before the event", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			closeFakeWS(conn)
			time.Sleep(30 * time.Millisecond) // let readPump notice the close

			fd.push(protocol.MustEvent(protocol.EventOutput, 1,
				protocol.OutputPayload{Content: "late"}))

			_, ok := conn.recv()
			Expect(ok).To(BeFalse(), "disconnected client must not receive events")
		})
	})

	Describe("BreakpointHit suspend/resume cycle", func() {
		It("broadcasts the event then waits before calling Continue", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))

			e, ok := recvEvent(conn)
			Expect(ok).To(BeTrue())
			Expect(e.Kind).To(Equal(protocol.EventBreakpointHit))

			time.Sleep(20 * time.Millisecond)
			Expect(fd.recordedCalls()).NotTo(ContainElement("Continue"))

			conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Continue"))
		})

		It("accepts StepOver as a resuming command", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdStepOver, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("StepOver"))
		})

		It("accepts StepInto as a resuming command", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdStepInto, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("StepInto"))
		})

		It("accepts StepOut as a resuming command", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdStepOut, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("StepOut"))
		})

		It("allows non-resuming commands (SetBreakpoint) while suspended", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.setBPResult = protocol.Breakpoint{ID: 2}

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn) // consume BreakpointHit

			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 10}))

			Eventually(func() protocol.EventKind {
				e, ok := recvEvent(conn)
				if !ok {
					return ""
				}
				return e.Kind
			}, "500ms", "10ms").Should(Equal(protocol.EventBreakpointSet))

			conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))
			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Continue"))
		})

		It("allows Locals while suspended", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.localsResult = []protocol.Variable{{Name: "x", Value: "42", Type: "int"}}

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdLocals,
				protocol.LocalsPayloadCmd{FrameIndex: 0}))

			Eventually(func() protocol.EventKind {
				e, ok := recvEvent(conn)
				if !ok {
					return ""
				}
				return e.Kind
			}, "500ms", "10ms").Should(Equal(protocol.EventLocals))
		})

		It("allows Evaluate while suspended", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.evalResult = protocol.Variable{Name: "total", Value: "7", Type: "int", Kind: "int"}

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdEvaluate,
				protocol.EvaluatePayloadCmd{FrameIndex: 0, Name: "total"}))

			Eventually(func() protocol.EventKind {
				e, ok := recvEvent(conn)
				if !ok {
					return ""
				}
				return e.Kind
			}, "500ms", "10ms").Should(Equal(protocol.EventEvaluate))
			Expect(fd.recordedCalls()).To(ContainElement("Evaluate"))
		})

		It("only the first resuming command wins when multiple clients race", func() {
			conn1 := newFakeWSConn()
			conn2 := newFakeWSConn()
			mustAddClient(h, conn1)
			mustAddClient(h, conn2)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 2,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn1)
			_, _ = recvEvent(conn2)

			conn1.inject(mustCommand(protocol.CmdContinue, struct{}{}))
			conn2.inject(mustCommand(protocol.CmdContinue, struct{}{}))

			time.Sleep(100 * time.Millisecond)

			count := 0
			for _, c := range fd.recordedCalls() {
				if c == "Continue" {
					count++
				}
			}
			Expect(count).To(Equal(1), "Continue must be called exactly once")
		})
	})

	Describe("Kill while running", func() {
		It("terminates the process even with no breakpoint set (no suspend first)", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			// A runaway target with no breakpoints never suspends. Kill must
			// still reach the debugger: it rides cmdCh (main loop), not
			// resumeCh (drained only while suspended).
			conn.inject(mustCommand(protocol.CmdKill, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Kill"))
		})

		It("still terminates the process while suspended", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdKill, struct{}{}))
			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Kill"))

			// A successful Kill performs no state transition of its own, so
			// the hub stays parked in the suspend-wait loop until the debugger
			// reports its own teardown. A real engine closes Events (AGENTS.md
			// → engine shutdown sequence); the loop must end the session on
			// that signal rather than inferring death from the command kind.
			fd.closeEvents()
			Eventually(h.Done(), "1s", "10ms").Should(BeClosed())
		})
	})

	Describe("retained shutdown ownership", func() {
		It("retries an incomplete attached detach before reporting the hub done", func() {
			fd.setKillError(fmt.Errorf("%w: injected detach failure",
				debugger.ErrAttachedDetachIncomplete))
			h := hub.New(fd, nil)
			ctx, cancel := context.WithCancel(context.Background())
			go h.Run(ctx)
			cancel()

			Eventually(func() int {
				return countCalls(fd.recordedCalls(), "Kill")
			}, "1s", "10ms").Should(BeNumerically(">=", 2))
			Consistently(h.Done(), "100ms", "10ms").ShouldNot(BeClosed(),
				"hub completion would discard the only owner of the attached process")

			fd.setKillError(nil)
			Eventually(h.Done(), "1s", "10ms").Should(BeClosed())
		})

		It("does not retry after the engine has already lost attached ownership", func() {
			fd.setKillError(debugger.ErrAttachedOwnershipLost)
			h := hub.New(fd, nil)
			ctx, cancel := context.WithCancel(context.Background())
			go h.Run(ctx)
			cancel()

			Eventually(h.Done(), "1s", "10ms").Should(BeClosed())
			expectCallCount(fd, "Kill", 1,
				"a terminal ownership loss cannot be recovered by retrying a dead engine")
		})
	})

	Describe("failed startup ownership retention", func() {
		It("keeps failed Attach cleanup owned through hub cancellation", func() {
			fd := newFakeDebugger()
			fd.attachErr = errors.New("attach failed")
			fd.setKillError(fmt.Errorf("%w: retained partial attach",
				debugger.ErrAttachedDetachIncomplete))
			h := hub.NewSession("session", func() debugger.Debugger { return fd }, nil)
			ctx, cancel := context.WithCancel(context.Background())
			go h.Run(ctx)
			defer cancel()
			defer fd.setKillError(nil)

			conn := newFakeWSConn()
			mustAddClient(h, conn)
			conn.inject(mustCommand(protocol.CmdAttach,
				protocol.AttachPayload{PID: 1234}))
			var payload protocol.ErrorPayload
			waitForEventKind(conn, protocol.EventError, &payload)
			Expect(payload.Message).To(ContainSubstring("attach failed"))

			Eventually(func() int {
				return countCalls(fd.recordedCalls(), "Kill")
			}, "1s", "10ms").Should(BeNumerically(">=", 2))
			cancel()
			Consistently(h.ExportedShutdownCh(), "100ms", "10ms").ShouldNot(BeClosed())
			Consistently(h.Done(), "100ms", "10ms").ShouldNot(BeClosed(),
				"hub completion would discard the failed Attach cleanup owner")

			fd.setKillError(nil)
			Eventually(h.ExportedShutdownCh(), "1s", "10ms").Should(BeClosed())
			Eventually(h.Done(), "1s", "10ms").Should(BeClosed())
		})

		It("waits for every failed startup candidate before completing shutdown", func() {
			first := newFakeDebugger()
			first.attachErr = errors.New("first attach failed")
			first.setKillError(fmt.Errorf("%w: first partial attach",
				debugger.ErrAttachedDetachIncomplete))
			second := newFakeDebugger()
			second.attachErr = errors.New("second attach failed")
			second.setKillError(fmt.Errorf("%w: second partial attach",
				debugger.ErrAttachedDetachIncomplete))
			candidates := []debugger.Debugger{first, second}
			h := hub.NewSession("session", func() debugger.Debugger {
				candidate := candidates[0]
				candidates = candidates[1:]
				return candidate
			}, nil)
			ctx, cancel := context.WithCancel(context.Background())
			go h.Run(ctx)
			defer cancel()
			defer first.setKillError(nil)
			defer second.setKillError(nil)

			conn := newFakeWSConn()
			mustAddClient(h, conn)
			for _, pid := range []int{1234, 5678} {
				conn.inject(mustCommand(protocol.CmdAttach, protocol.AttachPayload{PID: pid}))
				waitForEventKind(conn, protocol.EventError, nil)
			}
			Eventually(func() int {
				return countCalls(first.recordedCalls(), "Kill")
			}, "1s", "10ms").Should(BeNumerically(">=", 2))
			Eventually(func() int {
				return countCalls(second.recordedCalls(), "Kill")
			}, "1s", "10ms").Should(BeNumerically(">=", 2))

			cancel()
			first.setKillError(nil)
			Consistently(h.Done(), "150ms", "10ms").ShouldNot(BeClosed(),
				"the second retained candidate still owns its partial attach")
			second.setKillError(nil)
			Eventually(h.Done(), "1s", "10ms").Should(BeClosed())
		})

		It("linearizes successful candidate cleanup with concurrent shutdown", func() {
			releaseKill := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseKill) }) }
			DeferCleanup(release)
			fd := &blockingKillDebugger{
				fakeDebugger: newFakeDebugger(),
				started:      make(chan struct{}),
				release:      releaseKill,
			}
			fd.attachErr = errors.New("attach failed")
			h := hub.NewSession("session", func() debugger.Debugger { return fd }, nil)
			ctx, cancel := context.WithCancel(context.Background())
			go h.Run(ctx)
			defer cancel()

			conn := newFakeWSConn()
			mustAddClient(h, conn)
			conn.inject(mustCommand(protocol.CmdAttach, protocol.AttachPayload{PID: 1234}))
			Eventually(fd.started, "1s").Should(BeClosed())
			closeFakeWS(conn)
			Eventually(func() bool { return hub.ExportedIsClosing(h) }, "500ms").Should(BeTrue())
			Consistently(h.ExportedShutdownCh(), "100ms", "10ms").ShouldNot(BeClosed())

			release()
			Eventually(h.Done(), "1s", "10ms").Should(BeClosed())
			expectCallCount(fd.fakeDebugger, "Kill", 1,
				"candidate cleanup and shutdown must share one ownership obligation")
		})

		It("does not retain a cleanup obligation for a nil factory result", func() {
			h := hub.NewSession("session", func() debugger.Debugger { return nil }, nil)
			ctx, cancel := context.WithCancel(context.Background())
			go h.Run(ctx)
			defer cancel()

			conn := newFakeWSConn()
			mustAddClient(h, conn)
			conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "myapp"}))
			var payload protocol.ErrorPayload
			waitForEventKind(conn, protocol.EventError, &payload)
			Expect(payload.Message).To(ContainSubstring("debugger factory returned nil"))
			cancel()
			Eventually(h.Done(), "1s", "10ms").Should(BeClosed())
		})
	})

	// A Restart rejected before it touches the debugger, or a Kill the
	// debugger refuses, leaves the original process suspended. The hub must
	// stay in the suspend-wait loop — the only loop that drains resumeCh
	// (Run's outer select never selects on it) — or every later resume is
	// stranded and the session is wedged with a live, frozen tracee.
	Describe("rejected Restart or failed Kill keeps the hub suspended", func() {
		It("stays resumable after a raw hub rejects Restart", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			suspendAtBreakpoint(conn, fd)

			// hub.New has no session ID and no debugger factory, so Restart is
			// refused without touching h.dbg or the state.
			expectRejectedWhileSuspended(h, conn,
				mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
			expectResumable(conn, fd)
		})

		It("stays resumable after a failed Kill", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			refuseKillWhileSuspended(h, conn, fd)

			expectResumable(conn, fd)
		})

		It("lets a retry Kill reach the debugger after the first fails", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			refuseKillWhileSuspended(h, conn, fd)
			expectCallCount(fd, "Kill", 1, "the rejected Kill ran exactly once")

			fd.killErr = nil
			conn.inject(mustCommand(protocol.CmdKill, struct{}{}))
			expectCallCount(fd, "Kill", 2,
				"retry Kill must reach the debugger after a failed Kill")
		})
	})

	Describe("ordinary command admission", func() {
		It("executes a burst larger than cmdCh capacity without losing Kill", func() {
			conn := newFakeWSConnWithOutgoingBuffer(1)
			mustAddClient(h, conn)

			fd.setBPGate = make(chan struct{})
			fd.setBPStarted = make(chan struct{})
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() { close(fd.setBPGate) })
			}
			DeferCleanup(release)

			const breakpointCount = 48
			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 1}))
			select {
			case <-fd.setBPStarted:
			case <-time.After(500 * time.Millisecond):
				Fail("first SetBreakpoint did not reach the debugger")
			}

			burstSent := make(chan struct{})
			go func() {
				defer close(burstSent)
				for line := 2; line <= breakpointCount; line++ {
					conn.inject(mustCommand(protocol.CmdSetBreakpoint,
						protocol.SetBreakpointPayload{File: "main.go", Line: line}))
				}
				conn.inject(mustCommand(protocol.CmdKill, struct{}{}))
			}()

			select {
			case <-burstSent:
				Fail("burst bypassed backpressure while the debugger was gated")
			case <-time.After(100 * time.Millisecond):
			}

			release()
			select {
			case <-burstSent:
			case <-time.After(2 * time.Second):
				Fail("command producer remained blocked after debugger release")
			}

			Eventually(func() int {
				return countCalls(fd.recordedCalls(), "SetBreakpoint")
			}, "2s", "10ms").Should(Equal(breakpointCount))
			Eventually(func() int {
				return countCalls(fd.recordedCalls(), "Kill")
			}, "2s", "10ms").Should(Equal(1))
			Expect(h.ClientCount()).To(Equal(1))
		})
	})

	Describe("stale resume handling", func() {
		It("discards a resume buffered while running so it can't auto-continue a later suspend", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.setBPResult = protocol.Breakpoint{ID: 7}

			// Erroneously send Continue while the process is still running: it
			// lands in resumeCh. The SetBreakpoint that follows rides cmdCh from
			// the SAME read-pump, so once its confirmation arrives the stale
			// Continue is guaranteed to already be buffered in resumeCh.
			conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))
			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 3}))
			waitForEventKind(conn, protocol.EventBreakpointSet, nil)

			// Now the process suspends. The stale Continue must be dropped, not
			// consumed to auto-resume.
			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn) // BreakpointHit

			time.Sleep(50 * time.Millisecond)
			Expect(fd.recordedCalls()).NotTo(ContainElement("Continue"),
				"stale resume must not auto-continue past the fresh suspend")

			// A fresh Continue (sent after the suspend) resumes normally.
			conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))
			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Continue"))
			Expect(countCalls(fd.recordedCalls(), "Continue")).To(Equal(1),
				"exactly one (fresh) Continue should have run")
		})
	})

	Describe("failed resume keeps the hub suspended", func() {
		// A resuming command can be rejected by the debugger while the process
		// stays suspended (e.g. a transient backend error reinstalling a
		// software breakpoint — see AGENTS.md → step-over flow). The hub must
		// NOT abandon the suspend on that failure: a retry resume lands in
		// resumeCh, which only the suspend-wait loop drains (Run's outer loop
		// never selects on it), so leaving the loop would strand the client and
		// wedge the session with no way to resume.
		It("lets a retry Continue reach the debugger after the first fails", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.continueErr = errors.New("reinstall failed")
			suspendAtBreakpoint(conn, fd)

			// The rejected resume surfaces as an EventError; it also serves as a
			// happens-before barrier before we clear the transient error below.
			expectRejectedWhileSuspended(h, conn, mustCommand(protocol.CmdContinue, struct{}{}))
			expectCallCount(fd, "Continue", 1, "the rejected resume ran exactly once")

			fd.continueErr = nil
			expectResumable(conn, fd)
		})

		It("still resumes via Continue after a failed StepOver", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.stepOverErr = errors.New("step failed")

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			waitForEventKind(conn, protocol.EventBreakpointHit, nil)

			conn.inject(mustCommand(protocol.CmdStepOver, struct{}{}))
			waitForEventKind(conn, protocol.EventError, nil)
			Expect(fd.recordedCalls()).NotTo(ContainElement("Continue"))

			conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))
			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Continue"))
		})

		It("still services non-resuming commands after a failed resume", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.setBPResult = protocol.Breakpoint{ID: 9}
			fd.continueErr = errors.New("boom")

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			waitForEventKind(conn, protocol.EventBreakpointHit, nil)

			conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))
			waitForEventKind(conn, protocol.EventError, nil)

			// The hub is still parked in the suspend-wait loop, so a SetBreakpoint
			// (non-resuming) is executed immediately while suspended.
			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 5}))
			waitForEventKind(conn, protocol.EventBreakpointSet, nil)
		})
	})

	Describe("Pause", func() {
		It("routes CmdPause to the debugger while the process runs", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			// CmdPause is not a resuming command: it reaches the debugger via
			// the main-loop cmdCh path, without any suspending event first.
			conn.inject(mustCommand(protocol.CmdPause, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Pause"))
		})

		It("suspends the hub on EventPaused, then resumes on Continue", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			fd.push(protocol.MustEvent(protocol.EventPaused, 1,
				protocol.PausedPayload{Location: protocol.Location{File: "main.go", Line: 7}}))

			e, ok := recvEvent(conn)
			Expect(ok).To(BeTrue())
			Expect(e.Kind).To(Equal(protocol.EventPaused))

			// Suspending: no auto-resume until a resuming command arrives.
			time.Sleep(20 * time.Millisecond)
			Expect(fd.recordedCalls()).NotTo(ContainElement("Continue"))

			conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))
			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Continue"))
		})
	})

	Describe("SetBreakpoint confirmation", func() {
		It("broadcasts BreakpointSet with the assigned breakpoint", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.setBPResult = protocol.Breakpoint{
				ID:       1,
				Location: protocol.Location{File: "main.go", Line: 42},
			}

			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 42}))

			Eventually(func() protocol.EventKind {
				e, ok := recvEvent(conn)
				if !ok {
					return ""
				}
				return e.Kind
			}, "500ms", "10ms").Should(Equal(protocol.EventBreakpointSet))
		})
	})

	Describe("ClearBreakpoint confirmation", func() {
		It("broadcasts BreakpointCleared with the removed ID", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.setBPResult = protocol.Breakpoint{
				ID:       7,
				Location: protocol.Location{File: "main.go", Line: 42},
			}

			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "main.go", Line: 42}))
			var set protocol.BreakpointSetPayload
			waitForEventKind(conn, protocol.EventBreakpointSet, &set)

			conn.inject(mustCommand(protocol.CmdClearBreakpoint,
				protocol.ClearBreakpointPayload{ID: set.Breakpoint.ID}))

			var cleared protocol.BreakpointClearedPayload
			waitForEventKind(conn, protocol.EventBreakpointCleared, &cleared)
			Expect(cleared.ID).To(Equal(set.Breakpoint.ID))
		})
	})

	Describe("command error propagation", func() {
		It("broadcasts EventError when a command fails", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.setBPErr = fmt.Errorf("address not found")

			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "ghost.go", Line: 999}))

			Eventually(func() protocol.EventKind {
				e, ok := recvEvent(conn)
				if !ok {
					return ""
				}
				return e.Kind
			}, "500ms", "10ms").Should(Equal(protocol.EventError))
		})

		It("includes the failing command kind in the error payload", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			fd.setBPErr = fmt.Errorf("no such file")

			conn.inject(mustCommand(protocol.CmdSetBreakpoint,
				protocol.SetBreakpointPayload{File: "missing.go", Line: 1}))

			Eventually(func() bool {
				e, ok := recvEvent(conn)
				if !ok || e.Kind != protocol.EventError {
					return false
				}
				var p protocol.ErrorPayload
				_ = protocol.DecodeEventPayload(e, &p)
				return p.Command == protocol.CmdSetBreakpoint
			}, "500ms", "10ms").Should(BeTrue())
		})
	})

	Describe("shutdown when last client disconnects", func() {
		It("calls Kill on the debugger", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			closeFakeWS(conn)

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Kill"))
		})

		It("calls Kill exactly once even when cancel and disconnect race", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)
			closeFakeWS(conn)
			cancel() // races with the disconnect-triggered shutdown

			time.Sleep(50 * time.Millisecond)

			count := 0
			for _, c := range fd.recordedCalls() {
				if c == "Kill" {
					count++
				}
			}
			Expect(count).To(Equal(1), "Kill must be called exactly once")
		})
	})

	Describe("shutdown when debugger exits", func() {
		It("Run returns when the debugger's Events channel closes", func() {
			fd.closeEvents()

			select {
			case <-h.Done():
			case <-time.After(500 * time.Millisecond):
				Fail("Run did not return after debugger Events channel closed")
			}
		})

		It("unblocks the suspend loop when the process exits while paused", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			// Suspend the hub.
			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			// Process exits while paused (Kill called externally).
			fd.push(protocol.MustEvent(protocol.EventProcessExited, 2,
				protocol.ProcessExitedPayload{ExitCode: 0}))

			Eventually(func() protocol.EventKind {
				e, ok := recvEvent(conn)
				if !ok {
					return ""
				}
				return e.Kind
			}, "500ms", "10ms").Should(Equal(protocol.EventProcessExited))
		})
	})

	Describe("unknown command kind", func() {
		It("broadcasts EventError without panicking", func() {
			conn := newFakeWSConn()
			mustAddClient(h, conn)

			conn.inject(protocol.Command{
				Version: protocol.Version,
				Kind:    protocol.CommandKind("UnknownCmd"),
			})

			Eventually(func() protocol.EventKind {
				e, ok := recvEvent(conn)
				if !ok {
					return ""
				}
				return e.Kind
			}, "500ms", "10ms").Should(Equal(protocol.EventError))
		})
	})
})

// newManagedRestartHub starts a managed session backed by fd, connects one
// client, and drains the initial welcome/state event. The returned cancel
// must be deferred by the caller.
func newManagedRestartHub(fd *fakeDebugger) (*hub.Hub, *fakeWSConn, context.CancelFunc) {
	return newManagedHub(func() debugger.Debugger { return fd })
}

// newManagedHub starts a managed session driven by an arbitrary factory,
// connects one client, and drains the initial welcome/state event.
func newManagedHub(factory func() debugger.Debugger) (*hub.Hub, *fakeWSConn, context.CancelFunc) {
	managed := hub.NewSession("session", factory, nil)
	cancel := runHub(managed)
	conn := newFakeWSConn()
	_, err := managed.AddClient(conn, nil)
	Expect(err).NotTo(HaveOccurred())
	_, _ = recvEvent(conn) // welcome/state event
	return managed, conn, cancel
}

// recordingFactory hands out a FRESH fake per call and remembers every one. A
// Restart replacement must be a distinct instance from the debugger it
// replaces, otherwise a shared fake's call log cannot say which of the two was
// killed — exactly the distinction the abandoned-candidate specs assert on.
type recordingFactory struct {
	mu    sync.Mutex
	setup func(n int, fd *fakeDebugger)
	made  []*fakeDebugger
}

func newRecordingFactory(setup func(n int, fd *fakeDebugger)) *recordingFactory {
	return &recordingFactory{setup: setup}
}

func (r *recordingFactory) create() debugger.Debugger {
	r.mu.Lock()
	defer r.mu.Unlock()
	fd := newFakeDebugger()
	if r.setup != nil {
		r.setup(len(r.made), fd)
	}
	r.made = append(r.made, fd)
	return fd
}

func (r *recordingFactory) instances() []*fakeDebugger {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*fakeDebugger(nil), r.made...)
}

func (r *recordingFactory) instance(n int) *fakeDebugger {
	made := r.instances()
	ExpectWithOffset(1, len(made)).To(BeNumerically(">", n))
	return made[n]
}

// killCounter reports how many times the nth created debugger was killed,
// shaped for Eventually.
func (r *recordingFactory) killCounter(n int) func() int {
	return func() int {
		made := r.instances()
		if len(made) <= n {
			return -1
		}
		return countCalls(made[n].recordedCalls(), "Kill")
	}
}

// launchManaged injects a Launch and waits for it to reach the fake debugger
// and the resulting running-state event to drain.
func launchManaged(conn *fakeWSConn, fd *fakeDebugger, program string) {
	conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: program}))
	Eventually(fd.recordedCalls, "500ms", "10ms").Should(ContainElement("Launch"))
	_, _ = recvEvent(conn) // state -> running
}

// attachManaged is launchManaged's counterpart for an Attach-based session.
// Attach clears lastLaunch, so the session has no binary Restart could
// relaunch — the setup for every "Restart is refused" scenario.
func attachManaged(conn *fakeWSConn, fd *fakeDebugger, pid int) {
	conn.inject(mustCommand(protocol.CmdAttach, protocol.AttachPayload{PID: pid}))
	EventuallyWithOffset(1, fd.recordedCalls, "500ms", "10ms").Should(ContainElement("Attach"))
	_, _ = recvEvent(conn) // state -> running
}

// launchManagedFactory injects a Launch against a factory-driven hub, waits for
// the factory to produce the initial debugger, and returns it. The factory only
// runs when the Launch command reaches the Run loop, so the instance cannot be
// read before the command is injected.
func launchManagedFactory(conn *fakeWSConn, factory *recordingFactory, program string) *fakeDebugger {
	conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: program}))
	EventuallyWithOffset(1, factory.instances, "500ms", "10ms").Should(HaveLen(1))
	fd := factory.instances()[0]
	EventuallyWithOffset(1, fd.recordedCalls, "500ms", "10ms").Should(ContainElement("Launch"))
	_, _ = recvEvent(conn) // state -> running
	return fd
}

// waitForEventKind polls conn until an event of the given kind arrives,
// decoding it into out (if non-nil) before returning.
func waitForEventKind(conn *fakeWSConn, kind protocol.EventKind, out any) {
	Eventually(func() bool {
		e, ok := recvEvent(conn)
		if !ok || e.Kind != kind {
			return false
		}
		if out != nil {
			ExpectWithOffset(1, protocol.DecodeEventPayload(e, out)).To(Succeed())
		}
		return true
	}, "500ms", "10ms").Should(BeTrue())
}

// countCalls returns how many times name appears in calls.
func countCalls(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

func newManagedTimeoutHub(timeout time.Duration) (*fakeDebugger, *hub.Hub, *fakeWSConn, context.CancelFunc) {
	fd := newFakeDebugger()
	managed := hub.NewSession("timeout-session", func() debugger.Debugger { return fd }, nil)
	hub.ExportedSetSuspendTimeout(managed, timeout)
	cancel := runHub(managed)
	conn := newFakeWSConn()
	mustAddClient(managed, conn)
	waitForEventKind(conn, protocol.EventSessionState, nil)
	launchManaged(conn, fd, "timeout-target")
	return fd, managed, conn, cancel
}

var _ = Describe("Suspend safety timeout", func() {
	It("uses the normal resume state and event path after auto-continue succeeds", func() {
		const timeout = 50 * time.Millisecond
		fd, managed, conn, cancel := newManagedTimeoutHub(timeout)
		defer func() {
			cancel()
			Eventually(managed.Done(), "500ms", "10ms").Should(BeClosed())
		}()
		fd.emitContinued = true

		fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
		waitForEventKind(conn, protocol.EventBreakpointHit, nil)
		var suspended protocol.SessionStatePayload
		waitForEventKind(conn, protocol.EventSessionState, &suspended)
		Expect(suspended.State).To(Equal(protocol.StateSuspended))

		runningEvent, ok := recvEvent(conn)
		Expect(ok).To(BeTrue())
		Expect(runningEvent.Kind).To(Equal(protocol.EventSessionState))
		var running protocol.SessionStatePayload
		Expect(protocol.DecodeEventPayload(runningEvent, &running)).To(Succeed())
		Expect(running.State).To(Equal(protocol.StateRunning))

		continued, ok := recvEvent(conn)
		Expect(ok).To(BeTrue())
		Expect(continued.Kind).To(Equal(protocol.EventContinued))
		Expect(continued.Seq).To(BeNumerically(">", runningEvent.Seq))
		Expect(managed.State()).To(Equal(protocol.StateRunning))
		Consistently(func() int {
			return countCalls(fd.recordedCalls(), "Continue")
		}, 2*timeout, 5*time.Millisecond).Should(Equal(1))
	})

	It("reports a rejected auto-continue and still accepts a client retry", func() {
		const timeout = 50 * time.Millisecond
		fd, managed, conn, cancel := newManagedTimeoutHub(timeout)
		defer func() {
			cancel()
			Eventually(managed.Done(), "500ms", "10ms").Should(BeClosed())
		}()
		fd.continueErr = errors.New("reinstall failed")
		fd.emitContinued = true

		fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
		waitForEventKind(conn, protocol.EventBreakpointHit, nil)
		waitForEventKind(conn, protocol.EventSessionState, nil)

		var commandErr protocol.ErrorPayload
		waitForEventKind(conn, protocol.EventError, &commandErr)
		Expect(commandErr.Command).To(Equal(protocol.CmdContinue))
		Expect(managed.State()).To(Equal(protocol.StateSuspended))
		Expect(countCalls(fd.recordedCalls(), "Continue")).To(Equal(1))

		fd.continueErr = nil
		conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))

		var running protocol.SessionStatePayload
		waitForEventKind(conn, protocol.EventSessionState, &running)
		Expect(running.State).To(Equal(protocol.StateRunning))
		waitForEventKind(conn, protocol.EventContinued, nil)
		Expect(managed.State()).To(Equal(protocol.StateRunning))
		Expect(countCalls(fd.recordedCalls(), "Continue")).To(Equal(2))
	})

	It("re-arms the full interval after each rejected auto-continue", func() {
		const timeout = 120 * time.Millisecond
		fd, managed, conn, cancel := newManagedTimeoutHub(timeout)
		defer func() {
			cancel()
			Eventually(managed.Done(), "500ms", "10ms").Should(BeClosed())
		}()
		fd.continueErr = errors.New("still suspended")

		fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
		waitForEventKind(conn, protocol.EventBreakpointHit, nil)
		waitForEventKind(conn, protocol.EventSessionState, nil)
		waitForEventKind(conn, protocol.EventError, nil)

		Consistently(func() int {
			return countCalls(fd.recordedCalls(), "Continue")
		}, timeout/2, 5*time.Millisecond).Should(Equal(1))

		waitForEventKind(conn, protocol.EventError, nil)
		Expect(countCalls(fd.recordedCalls(), "Continue")).To(Equal(2))
		Consistently(func() int {
			return countCalls(fd.recordedCalls(), "Continue")
		}, timeout/2, 5*time.Millisecond).Should(Equal(2))
		Expect(managed.State()).To(Equal(protocol.StateSuspended))
	})
})

// expectCallCount waits until name has been dispatched to fd exactly want
// times, then holds — catching both a missing dispatch and a duplicate one.
func expectCallCount(fd *fakeDebugger, name string, want int, msg string) {
	EventuallyWithOffset(1, func() int {
		return countCalls(fd.recordedCalls(), name)
	}, "500ms", "10ms").Should(Equal(want), msg)
}

// suspendAtBreakpoint pushes a BreakpointHit from fd and waits for conn to
// observe it, leaving the hub parked in the suspend-wait loop. The pushed seq
// is cosmetic — the hub re-stamps every outbound event with its own counter.
func suspendAtBreakpoint(conn *fakeWSConn, fd *fakeDebugger) {
	fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
		protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
	waitForEventKind(conn, protocol.EventBreakpointHit, nil)
}

// expectRejectedWhileSuspended injects cmd and waits for the resulting
// EventError, then asserts the session is untouched — still suspended, with
// the original process alive. The error also serves as a happens-before
// barrier for anything the caller mutates afterwards.
func expectRejectedWhileSuspended(h *hub.Hub, conn *fakeWSConn, cmd protocol.Command) {
	conn.inject(cmd)
	waitForEventKind(conn, protocol.EventError, nil)
	ExpectWithOffset(1, h.State()).To(Equal(protocol.StateSuspended))
}

// expectResumable proves the hub is still parked in the suspend-wait loop by
// driving one more Continue through to the debugger. That loop is the only
// drainer of resumeCh (Run's outer select never reads it), so a Continue that
// lands is proof the suspend was not abandoned.
func expectResumable(conn *fakeWSConn, fd *fakeDebugger) {
	want := countCalls(fd.recordedCalls(), "Continue") + 1
	conn.inject(mustCommand(protocol.CmdContinue, struct{}{}))
	EventuallyWithOffset(1, func() int {
		return countCalls(fd.recordedCalls(), "Continue")
	}, "500ms", "10ms").Should(Equal(want),
		"resume must still reach the debugger while suspended")
}

// refuseKillWhileSuspended parks the hub at a breakpoint behind a debugger
// that refuses Kill, then drives that rejected Kill — the shared starting
// state for every "a failed Kill must not dissolve the suspend" scenario.
func refuseKillWhileSuspended(h *hub.Hub, conn *fakeWSConn, fd *fakeDebugger) {
	fd.killErr = errors.New("kill failed")
	suspendAtBreakpoint(conn, fd)
	expectRejectedWhileSuspended(h, conn, mustCommand(protocol.CmdKill, struct{}{}))
}

// bpAtLine resolves a breakpoint whose id IS its line, so a restart's saved
// locations sort deterministically and each one is identifiable in the
// reinstall results.
func bpAtLine(file string, line int) (protocol.Breakpoint, error) {
	return protocol.Breakpoint{ID: line, Location: protocol.Location{File: file, Line: line}}, nil
}

// setManagedBreakpoints installs one breakpoint per line and waits for each
// confirmation, so they are all recorded for a later Restart to reinstall.
func setManagedBreakpoints(conn *fakeWSConn, lines ...int) {
	for _, line := range lines {
		conn.inject(mustCommand(protocol.CmdSetBreakpoint, protocol.SetBreakpointPayload{File: "main.go", Line: line}))
		waitForEventKind(conn, protocol.EventBreakpointSet, nil)
	}
}

var _ = Describe("Restart", func() {
	var fd *fakeDebugger

	BeforeEach(func() {
		fd = newFakeDebugger()
	})

	It("rejects Restart on a raw (non-managed) hub", func() {
		h := hub.New(fd, nil)
		cancel := runHub(h)
		defer cancel()

		conn := newFakeWSConn()
		mustAddClient(h, conn)

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventError, nil)
	})

	It("rejects Restart before any successful Launch", func() {
		_, conn, cancel := newManagedRestartHub(fd)
		defer cancel()

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventError, nil)
	})

	It("rejects Restart after Attach (no relaunchable binary)", func() {
		_, conn, cancel := newManagedRestartHub(fd)
		defer cancel()

		attachManaged(conn, fd, 123)

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventError, nil)
	})

	It("kills the old debugger, relaunches, and reinstalls breakpoints", func() {
		_, conn, cancel := newManagedRestartHub(fd)
		defer cancel()
		launchManaged(conn, fd, "myapp")

		fd.setBPResult = protocol.Breakpoint{ID: 1, Location: protocol.Location{File: "main.go", Line: 10}}
		conn.inject(mustCommand(protocol.CmdSetBreakpoint, protocol.SetBreakpointPayload{File: "main.go", Line: 10}))
		waitForEventKind(conn, protocol.EventBreakpointSet, nil)

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)

		Expect(restarted.Program).To(Equal("myapp"))
		Expect(restarted.Breakpoints).To(HaveLen(1))
		Expect(restarted.Discarded).To(BeEmpty())

		calls := fd.recordedCalls()
		Expect(calls).To(ContainElement("Kill"))
		Expect(countCalls(calls, "Launch")).To(Equal(2),
			"expected a Launch for the original start plus one for restart")
		Expect(countCalls(calls, "SetBreakpoint")).To(Equal(2),
			"expected the original SetBreakpoint plus a reinstall on restart")
	})

	It("reports discarded breakpoints that fail to reinstall", func() {
		_, conn, cancel := newManagedRestartHub(fd)
		defer cancel()
		launchManaged(conn, fd, "myapp")

		fd.setBPResult = protocol.Breakpoint{ID: 1, Location: protocol.Location{File: "main.go", Line: 10}}
		conn.inject(mustCommand(protocol.CmdSetBreakpoint, protocol.SetBreakpointPayload{File: "main.go", Line: 10}))
		waitForEventKind(conn, protocol.EventBreakpointSet, nil)

		fd.setBPErr = fmt.Errorf("no such line")
		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)

		Expect(restarted.Breakpoints).To(BeEmpty())
		Expect(restarted.Discarded).To(HaveLen(1))
		Expect(restarted.Discarded[0].Reason).To(Equal("no such line"))
	})

	It("leaves the suspend loop once the relaunch succeeds", func() {
		managed, conn, cancel := newManagedRestartHub(fd)
		defer cancel()
		launchManaged(conn, fd, "myapp")
		suspendAtBreakpoint(conn, fd)

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventRestarted, nil)
		Expect(managed.State()).To(Equal(protocol.StateRunning))

		// Run's outer loop owns the relaunched debugger's events again: a
		// fresh stop suspends the hub and a Continue resumes it.
		suspendAtBreakpoint(conn, fd)
		expectResumable(conn, fd)
	})

	// Restart is refused before it touches the debugger or the session state
	// when there is no relaunchable binary. The suspended process is untouched,
	// so the hub must stay in the suspend-wait loop — the only loop that
	// drains resumeCh — instead of stranding every later resume.
	It("stays resumable when Restart is rejected while suspended", func() {
		managed, conn, cancel := newManagedRestartHub(fd)
		defer cancel()
		attachManaged(conn, fd, 123)
		suspendAtBreakpoint(conn, fd)

		expectRejectedWhileSuspended(managed, conn,
			mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		expectResumable(conn, fd)
	})

	It("goes idle and leaves the suspend loop when the relaunch fails", func() {
		managed, conn, cancel := newManagedRestartHub(fd)
		defer cancel()
		launchManaged(conn, fd, "myapp")
		suspendAtBreakpoint(conn, fd)

		fd.launchErr = errors.New("relaunch failed")
		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventError, nil)

		// A failed relaunch discards the old debugger, so the suspended
		// process is genuinely gone and the loop must not keep waiting.
		Eventually(managed.State, "500ms", "10ms").Should(Equal(protocol.StateIdle))
	})
})

// A relaunch replacement is never installed in h.dbg until its Launch
// succeeds, so shutdown's snapshot cannot reach it and Run never selects on its
// events. handleRestart is therefore its sole owner and must dispose of it on
// every failure exit — a dropped engine keeps a LockOSThread'd loop goroutine
// (and, on linux, a tracer thread) alive forever.
var _ = Describe("Restart relaunch failure", func() {
	relaunchFails := func(n int, fd *fakeDebugger) {
		if n > 0 {
			fd.launchErr = errors.New("relaunch boom")
		}
	}

	It("kills the replacement whose relaunch failed", func() {
		factory := newRecordingFactory(relaunchFails)
		_, conn, cancel := newManagedHub(factory.create)
		defer cancel()
		launchManagedFactory(conn, factory, "myapp")

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventError, nil)

		Eventually(factory.killCounter(1), "500ms", "10ms").Should(Equal(1),
			"the abandoned replacement must be killed exactly once")
		Consistently(factory.killCounter(1), "100ms", "10ms").Should(Equal(1),
			"no second owner may kill the same replacement")
		Expect(factory.instance(1).recordedCalls()).To(Equal([]string{"Launch", "Kill"}))
	})

	It("releases resources a partially started replacement acquired", func() {
		original := newFakeDebugger()
		leaky := newLeakyLaunchDebugger(errors.New("relaunch boom"))
		calls := 0
		_, conn, cancel := newManagedHub(func() debugger.Debugger {
			calls++
			if calls == 1 {
				return original
			}
			return leaky
		})
		defer cancel()
		launchManaged(conn, original, "myapp")

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventError, nil)

		Eventually(leaky.exited, "500ms", "10ms").Should(BeClosed(),
			"disposal must let the partially started replacement tear down")
		Expect(countCalls(leaky.recordedCalls(), "Kill")).To(Equal(1))
	})

	It("does not accumulate abandoned replacements across repeated failures", func() {
		const attempts = 3
		factory := newRecordingFactory(relaunchFails)
		_, conn, cancel := newManagedHub(factory.create)
		defer cancel()
		launchManagedFactory(conn, factory, "myapp")

		for i := 0; i < attempts; i++ {
			conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
			waitForEventKind(conn, protocol.EventError, nil)
			Eventually(factory.killCounter(i+1), "500ms", "10ms").Should(Equal(1))
		}

		Expect(factory.instances()).To(HaveLen(attempts+1),
			"one debugger per Launch plus one per Restart attempt")
		for i := 1; i <= attempts; i++ {
			Expect(countCalls(factory.instance(i).recordedCalls(), "Kill")).To(Equal(1),
				"replacement %d must be killed exactly once", i)
		}
	})

	It("reports the failure and leaves the session relaunchable", func() {
		factory := newRecordingFactory(func(n int, fd *fakeDebugger) {
			if n == 1 {
				fd.launchErr = errors.New("relaunch boom")
			}
		})
		_, conn, cancel := newManagedHub(factory.create)
		defer cancel()
		launchManagedFactory(conn, factory, "myapp")

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var errPayload protocol.ErrorPayload
		waitForEventKind(conn, protocol.EventError, &errPayload)
		Expect(errPayload.Message).To(ContainSubstring("restart: relaunch failed"))
		Expect(errPayload.Message).To(ContainSubstring("relaunch boom"))

		var state protocol.SessionStatePayload
		waitForEventKind(conn, protocol.EventSessionState, &state)
		Expect(state.State).To(Equal(protocol.StateIdle))

		// lastLaunch survives a failed relaunch, so a retry still works.
		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)
		Expect(restarted.Program).To(Equal("myapp"))
	})

	It("keeps the original error when disposal of the replacement fails", func() {
		factory := newRecordingFactory(func(n int, fd *fakeDebugger) {
			if n > 0 {
				fd.launchErr = errors.New("relaunch boom")
				fd.killErr = errors.New("kill boom")
			}
		})
		_, conn, cancel := newManagedHub(factory.create)
		defer cancel()
		launchManagedFactory(conn, factory, "myapp")

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var errPayload protocol.ErrorPayload
		waitForEventKind(conn, protocol.EventError, &errPayload)

		Expect(errPayload.Message).To(ContainSubstring("relaunch boom"))
		Expect(errPayload.Message).NotTo(ContainSubstring("kill boom"))
		Eventually(factory.killCounter(1), "500ms", "10ms").Should(Equal(1))
	})

	It("keeps a successfully relaunched replacement installed and alive", func() {
		factory := newRecordingFactory(nil)
		_, conn, cancel := newManagedHub(factory.create)
		defer cancel()
		launchManagedFactory(conn, factory, "myapp")

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventRestarted, nil)

		replacement := factory.instance(1)
		Expect(replacement.recordedCalls()).To(ContainElement("Launch"))
		Consistently(func() []string { return replacement.recordedCalls() }, "100ms", "10ms").
			ShouldNot(ContainElement("Kill"))
		Expect(factory.instance(0).recordedCalls()).To(ContainElement("Kill"))

		// Only an installed debugger has its events selected on by Run.
		replacement.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 7}}))
		waitForEventKind(conn, protocol.EventBreakpointHit, nil)
	})

	It("discards a replacement whose failing Launch finishes after shutdown", func() {
		old := newFakeDebugger()
		releaseLaunch := make(chan struct{})
		replacement := &blockingLaunchDebugger{
			fakeDebugger: newFakeDebugger(),
			started:      make(chan struct{}),
			release:      releaseLaunch,
		}
		replacement.launchErr = errors.New("relaunch boom")
		calls := 0
		managed, conn, cancel := newManagedHub(func() debugger.Debugger {
			calls++
			if calls == 1 {
				return old
			}
			return replacement
		})
		defer cancel()
		launchManaged(conn, old, "myapp")

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		Eventually(replacement.started, "500ms").Should(BeClosed())

		closeFakeWS(conn)
		Eventually(func() bool { return hub.ExportedIsClosing(managed) }, "500ms").Should(BeTrue())
		Consistently(managed.ExportedShutdownCh(), "100ms").ShouldNot(BeClosed())
		close(releaseLaunch)

		Eventually(managed.Done(), "500ms").Should(BeClosed())
		Expect(replacement.recordedCalls()).To(Equal([]string{"Launch", "Kill"}),
			"a candidate rejected by shutdown is killed once by its caller, not by shutdown")
	})

	It("kills the replacement when a relaunch with saved breakpoints fails", func() {
		factory := newRecordingFactory(func(n int, fd *fakeDebugger) {
			fd.setBPFunc = bpAtLine
			if n > 0 {
				fd.launchErr = errors.New("relaunch boom")
			}
		})
		_, conn, cancel := newManagedHub(factory.create)
		defer cancel()
		launchManagedFactory(conn, factory, "myapp")
		setManagedBreakpoints(conn, 10, 20)

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventError, nil)

		Eventually(factory.killCounter(1), "500ms", "10ms").Should(Equal(1))
		Expect(factory.instance(1).recordedCalls()).To(Equal([]string{"Launch", "Kill"}),
			"a replacement that never launched must not be asked to reinstall breakpoints")

		// Saved locations survive a failed relaunch, so a retry disposes of its
		// own candidate too rather than reusing the abandoned one.
		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventError, nil)
		Eventually(factory.killCounter(2), "500ms", "10ms").Should(Equal(1))
	})
})

// Reinstall failures are a REPORTED OUTCOME, not a relaunch failure: handleRestart
// collects them as DiscardedBreakpoint and keeps the relaunched process running,
// mirroring Delve's Restart. By then the replacement has already been installed
// via installDebugger, so it is hub-owned — killing it here would terminate a
// healthy tracee and contradict the documented contract. These specs pin that
// boundary from the other side of the launch-failure disposal specs above.
var _ = Describe("Restart breakpoint reinstall failure", func() {
	It("keeps a partially reinstalled replacement installed and alive", func() {
		factory := newRecordingFactory(func(n int, fd *fakeDebugger) {
			if n == 0 {
				fd.setBPFunc = bpAtLine
				return
			}
			fd.setBPFunc = func(file string, line int) (protocol.Breakpoint, error) {
				if line == 20 {
					return protocol.Breakpoint{}, errors.New("no such line")
				}
				return bpAtLine(file, line)
			}
		})
		_, conn, cancel := newManagedHub(factory.create)
		defer cancel()
		launchManagedFactory(conn, factory, "myapp")
		setManagedBreakpoints(conn, 10, 20, 30)

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)

		Expect(restarted.Breakpoints).To(HaveLen(2))
		Expect(restarted.Discarded).To(HaveLen(1))
		Expect(restarted.Discarded[0].Location.Line).To(Equal(20))
		Expect(restarted.Discarded[0].Reason).To(Equal("no such line"))

		replacement := factory.instance(1)
		Consistently(func() []string { return replacement.recordedCalls() }, "100ms", "10ms").
			ShouldNot(ContainElement("Kill"),
				"a discarded breakpoint must not tear down a successfully relaunched process")

		// Only an installed debugger has its events selected on by Run.
		replacement.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 10}}))
		waitForEventKind(conn, protocol.EventBreakpointHit, nil)
	})

	It("keeps the replacement alive when no breakpoint reinstalls", func() {
		factory := newRecordingFactory(func(n int, fd *fakeDebugger) {
			if n == 0 {
				fd.setBPFunc = bpAtLine
				return
			}
			fd.setBPErr = errors.New("no such line")
		})
		managed, conn, cancel := newManagedHub(factory.create)
		defer cancel()
		launchManagedFactory(conn, factory, "myapp")
		setManagedBreakpoints(conn, 10, 20)

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)

		Expect(restarted.Breakpoints).To(BeEmpty())
		Expect(restarted.Discarded).To(HaveLen(2))
		Expect(managed.State()).To(Equal(protocol.StateRunning),
			"a total reinstall failure still leaves a running process, not an idle session")
		Consistently(func() []string { return factory.instance(1).recordedCalls() }, "100ms", "10ms").
			ShouldNot(ContainElement("Kill"))
	})
})

var _ = Describe("debugger ownership during shutdown", func() {
	It("retains a restart replacement whose cleanup remains incomplete after shutdown", func() {
		old := newFakeDebugger()
		releaseLaunch := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseLaunch) }) }
		DeferCleanup(release)
		replacement := &blockingLaunchDebugger{
			fakeDebugger: newFakeDebugger(),
			started:      make(chan struct{}),
			release:      releaseLaunch,
		}
		replacement.setKillError(fmt.Errorf("%w: retained restart candidate",
			debugger.ErrAttachedDetachIncomplete))
		defer replacement.setKillError(nil)
		factoryCalls := 0
		managed := hub.NewSession("session", func() debugger.Debugger {
			factoryCalls++
			if factoryCalls == 1 {
				return old
			}
			return replacement
		}, nil)
		cancel := runHub(managed)
		defer cancel()

		conn := newFakeWSConn()
		mustAddClient(managed, conn)
		_, _ = recvEvent(conn)
		launchManaged(conn, old, "myapp")

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		Eventually(replacement.started, "500ms").Should(BeClosed())

		closeFakeWS(conn)
		Eventually(func() bool { return hub.ExportedIsClosing(managed) }, "500ms").Should(BeTrue())
		Consistently(managed.ExportedShutdownCh(), "100ms").ShouldNot(BeClosed())
		release()

		Eventually(func() int {
			return countCalls(replacement.recordedCalls(), "Kill")
		}, "1s", "10ms").Should(BeNumerically(">=", 2))
		Consistently(managed.ExportedShutdownCh(), "100ms", "10ms").ShouldNot(BeClosed())
		replacement.setKillError(nil)
		Eventually(managed.Done(), "500ms").Should(BeClosed())
		Expect(replacement.recordedCalls()).To(ContainElements("Launch", "Kill"))
	})

	It("discards an initial debugger constructed after shutdown", func() {
		candidate := newFakeDebugger()
		factoryStarted := make(chan struct{})
		releaseFactory := make(chan struct{})
		managed := hub.NewSession("session", func() debugger.Debugger {
			close(factoryStarted)
			<-releaseFactory
			return candidate
		}, nil)
		cancel := runHub(managed)
		defer cancel()

		conn := newFakeWSConn()
		mustAddClient(managed, conn)
		_, _ = recvEvent(conn)
		conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "myapp"}))
		Eventually(factoryStarted, "500ms").Should(BeClosed())

		closeFakeWS(conn)
		Eventually(func() bool { return hub.ExportedIsClosing(managed) }, "500ms").Should(BeTrue())
		Consistently(managed.ExportedShutdownCh(), "100ms").ShouldNot(BeClosed())
		close(releaseFactory)

		Eventually(managed.Done(), "500ms").Should(BeClosed())
		Expect(candidate.recordedCalls()).To(ContainElement("Kill"))
		Expect(candidate.recordedCalls()).NotTo(ContainElement("Launch"))
	})

	It("discards an initial debugger whose Launch finishes after shutdown", func() {
		releaseLaunch := make(chan struct{})
		candidate := &blockingLaunchDebugger{
			fakeDebugger: newFakeDebugger(),
			started:      make(chan struct{}),
			release:      releaseLaunch,
		}
		managed := hub.NewSession("session", func() debugger.Debugger {
			return candidate
		}, nil)
		cancel := runHub(managed)
		defer cancel()

		conn := newFakeWSConn()
		mustAddClient(managed, conn)
		_, _ = recvEvent(conn)
		conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "myapp"}))
		Eventually(candidate.started, "500ms").Should(BeClosed())

		closeFakeWS(conn)
		Eventually(func() bool { return hub.ExportedIsClosing(managed) }, "500ms").Should(BeTrue())
		Consistently(managed.ExportedShutdownCh(), "100ms").ShouldNot(BeClosed())
		close(releaseLaunch)

		Eventually(managed.Done(), "500ms").Should(BeClosed())
		Expect(candidate.recordedCalls()).To(ContainElements("Launch", "Kill"))
	})
})
