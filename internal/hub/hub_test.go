package hub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
	setBPResult      protocol.Breakpoint
	setBPErr         error
	clearBPErr       error
	continueErr      error
	stepOverErr      error
	stepIntoErr      error
	stepOutErr       error
	localsResult     []protocol.Variable
	framesResult     []protocol.Frame
	goroutinesResult []protocol.Goroutine
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
func (f *fakeDebugger) Kill() error     { f.record("Kill"); return nil }
func (f *fakeDebugger) Continue() error { f.record("Continue"); return f.continueErr }
func (f *fakeDebugger) StepOver() error { f.record("StepOver"); return f.stepOverErr }
func (f *fakeDebugger) StepInto() error { f.record("StepInto"); return f.stepIntoErr }
func (f *fakeDebugger) StepOut() error  { f.record("StepOut"); return f.stepOutErr }
func (f *fakeDebugger) ClearBreakpoint(id int) error {
	f.record("ClearBreakpoint")
	return f.clearBPErr
}
func (f *fakeDebugger) SetBreakpoint(file string, line int) (protocol.Breakpoint, error) {
	f.record("SetBreakpoint")
	return f.setBPResult, f.setBPErr
}
func (f *fakeDebugger) Locals(fi int) ([]protocol.Variable, error) {
	f.record("Locals")
	return f.localsResult, nil
}
func (f *fakeDebugger) StackFrames() ([]protocol.Frame, error) {
	f.record("StackFrames")
	return f.framesResult, nil
}
func (f *fakeDebugger) Goroutines() ([]protocol.Goroutine, error) {
	f.record("Goroutines")
	return f.goroutinesResult, nil
}


type fakeWSConn struct {
	mu       sync.Mutex
	incoming chan []byte // messages written by the server (server → client)
	outgoing chan []byte // messages injected by the test  (client → server)
	closed   bool
}

func newFakeWSConn() *fakeWSConn {
	return &fakeWSConn{
		// Large buffer so WriteMessage (called from the hub's event loop)
		// never blocks if a test doesn't drain — blocking would deadlock.
		incoming: make(chan []byte, 256),
		outgoing: make(chan []byte, 32),
	}
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

func (f *fakeWSConn) WriteMessage(_ int, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return &connClosedErr{}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.incoming <- cp
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
			defer func() { recover() }()
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
			h.AddClient(conn1, nil)
			h.AddClient(conn2, nil)
			conn1.Close()
			conn2.Close()
		})
	})


	Describe("event sequence numbers", func() {
		It("assigns strictly increasing hub-managed seq to all outbound events", func() {
			conn := newFakeWSConn()
			h.AddClient(conn, nil)

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
			h.AddClient(conn, nil)
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
			h.AddClient(conn1, nil)
			h.AddClient(conn2, nil)

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
			h.AddClient(conn, nil)
			conn.Close()
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
			h.AddClient(conn, nil)

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
			h.AddClient(conn, nil)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdStepOver, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("StepOver"))
		})

		It("accepts StepInto as a resuming command", func() {
			conn := newFakeWSConn()
			h.AddClient(conn, nil)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdStepInto, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("StepInto"))
		})

		It("accepts StepOut as a resuming command", func() {
			conn := newFakeWSConn()
			h.AddClient(conn, nil)

			fd.push(protocol.MustEvent(protocol.EventBreakpointHit, 1,
				protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{ID: 1}}))
			_, _ = recvEvent(conn)

			conn.inject(mustCommand(protocol.CmdStepOut, struct{}{}))

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("StepOut"))
		})

		It("allows non-resuming commands (SetBreakpoint) while suspended", func() {
			conn := newFakeWSConn()
			h.AddClient(conn, nil)
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
			h.AddClient(conn, nil)
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

		It("only the first resuming command wins when multiple clients race", func() {
			conn1 := newFakeWSConn()
			conn2 := newFakeWSConn()
			h.AddClient(conn1, nil)
			h.AddClient(conn2, nil)

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


	Describe("SetBreakpoint confirmation", func() {
		It("broadcasts BreakpointSet with the assigned breakpoint", func() {
			conn := newFakeWSConn()
			h.AddClient(conn, nil)
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
			h.AddClient(conn, nil)

			conn.inject(mustCommand(protocol.CmdClearBreakpoint,
				protocol.ClearBreakpointPayload{ID: 5}))

			Eventually(func() protocol.EventKind {
				e, ok := recvEvent(conn)
				if !ok {
					return ""
				}
				return e.Kind
			}, "500ms", "10ms").Should(Equal(protocol.EventBreakpointCleared))
		})
	})


	Describe("command error propagation", func() {
		It("broadcasts EventError when a command fails", func() {
			conn := newFakeWSConn()
			h.AddClient(conn, nil)
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
			h.AddClient(conn, nil)
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
			h.AddClient(conn, nil)
			conn.Close()

			Eventually(fd.recordedCalls, "500ms", "10ms").
				Should(ContainElement("Kill"))
		})

		It("calls Kill exactly once even when cancel and disconnect race", func() {
			conn := newFakeWSConn()
			h.AddClient(conn, nil)
			conn.Close()
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
			h.AddClient(conn, nil)

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
			h.AddClient(conn, nil)

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
