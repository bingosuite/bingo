package debugger_test

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// These specs pin the invariant that every asynchronous halt in handleStop is
// reported with a SUSPENDING event, not a bare EventError. handleStop runs long
// after the resume command was answered — the client has already been told the
// process resumed (EventContinued) and the hub has already left its suspend
// wait loop. A non-suspending EventError therefore leaves the hub believing the
// tracee runs while it is in fact halted forever, and since resumeCh is drained
// only inside that wait loop, every later resume is stranded unread.

// haltEvents is the Error-then-Paused pair every asynchronous stop-handling
// failure must produce, in that order.
func expectHaltReported(d debugger.Debugger, msgSubstring string) {
	errEvt := mustNextEvent(d)
	ExpectWithOffset(1, errEvt.Kind).To(Equal(protocol.EventError),
		"the detailed error must be reported first")
	var ep protocol.ErrorPayload
	ExpectWithOffset(1, protocol.DecodeEventPayload(errEvt, &ep)).To(Succeed())
	if msgSubstring != "" {
		ExpectWithOffset(1, ep.Message).To(ContainSubstring(msgSubstring))
	}

	pausedEvt := mustNextEvent(d)
	ExpectWithOffset(1, pausedEvt.Kind).To(Equal(protocol.EventPaused),
		"a suspending event must follow so the hub re-enters its suspend wait loop")
}

// runWithWaitLoop moves a suspended engine to running through the real Continue
// path, which is what starts the waitLoop goroutine that consumes backend
// stops. ExportedForceRunning only flips the state field, so a stop pushed
// after it would never be read.
func runWithWaitLoop(d debugger.Debugger) {
	debugger.ExportedForceSuspended(d)
	ExpectWithOffset(1, d.Continue()).To(Succeed())
	ExpectWithOffset(1, mustNextEvent(d).Kind).To(Equal(protocol.EventContinued))
}

// arriveAtBreakpoint drives the engine to a real user-breakpoint suspend at
// addr, leaving lastBP set so the next resume takes the step-over path.
func arriveAtBreakpoint(fb *fakeBackend, d debugger.Debugger, addr uint64) {
	fb.seedMem(addr, []byte{0x90, 0x90, 0x90, 0x90, 0x90})
	debugger.ExportedForceSuspended(d)
	debugger.ExportedSetBreakpointAt(d, addr)

	runWithWaitLoop(d)
	fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 1, PC: addr})
	ExpectWithOffset(1, mustNextEvent(d).Kind).To(Equal(protocol.EventBreakpointHit))
}

func retireInternalBreakpoint(fb *fakeBackend, d debugger.Debugger, addr uint64) {
	fb.seedMem(addr, []byte{0x90, 0x90, 0x90, 0x90})
	fb.regs[1] = debugger.Registers{PC: addr}
	debugger.ExportedForceSuspended(d)
	debugger.ExportedSetStepOverBreakpointAt(d, addr)
	runWithWaitLoop(d)
	fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 1, PC: addr})
	ExpectWithOffset(1, mustNextEvent(d).Kind).To(Equal(protocol.EventStepped))
}

var _ = Describe("asynchronous halts in handleStop", func() {
	var (
		fb *fakeBackend
		d  debugger.Debugger
	)

	BeforeEach(func() {
		fb = newFakeBackend()
		d = debugger.NewWithBackend(fb, nil)
	})

	AfterEach(func() {
		fb.clearFaults()
		_ = d.Kill()
		if !fb.stopped {
			close(fb.stopCh)
			fb.stopped = true
		}
	})

	// Site A — StopBreakpoint: populateBreakpointStop failure.
	It("reports a halt when the breakpoint stop cannot be located", func() {
		runWithWaitLoop(d)

		fb.failThreads(errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint})

		expectHaltReported(d, "find breakpoint thread")

		fb.clearFaults()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
	})

	// Site B — StopSingleStep: populateStopPC failure.
	It("reports a halt when the single-step PC cannot be read", func() {
		debugger.ExportedForceSuspended(d)
		Expect(d.StepInto()).To(Succeed())

		fb.failRegisters(errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep})

		expectHaltReported(d, "get stop PC")

		fb.clearFaults()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
	})

	// Site C — StopSingleStep: breakpoint reinstall failure. This is the
	// canonical stranding case: the resume was accepted and EventContinued
	// already told every client the process runs again.
	It("reports a halt when the step-over breakpoint cannot be reinstalled", func() {
		const bpAddr = uint64(0x4100)
		arriveAtBreakpoint(fb, d, bpAddr)

		continueAndConsumeContinued(d)

		fb.failWriteAt(bpAddr, errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + 4})

		expectHaltReported(d, "reinstall breakpoint")

		fb.clearFaults()
		before := fb.continueCount()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
		Expect(fb.continueCount()).To(BeNumerically(">", before),
			"the retry must actually reach the backend")
	})

	It("reports a halt when a dead step owner's breakpoint cannot be reconciled", func() {
		const bpAddr = uint64(0x4140)
		arriveAtBreakpoint(fb, d, bpAddr)
		continueAndConsumeContinued(d)

		fb.failWriteAt(bpAddr, errInjected)
		fb.pushStop(debugger.StopEvent{
			Reason: debugger.StopStepThreadExited,
			TID:    2,
		})

		expectHaltReported(d, "after stepped thread exited")
	})

	It("reports a halt when an internal breakpoint cannot be auto-cleared", func() {
		const addr = uint64(0x4180)
		fb.seedMem(addr, []byte{0x90, 0x90, 0x90, 0x90})
		fb.regs[1] = debugger.Registers{PC: addr}
		debugger.ExportedForceSuspended(d)
		debugger.ExportedSetStepOverBreakpointAt(d, addr)
		runWithWaitLoop(d)

		fb.failWriteAt(addr, errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 1, PC: addr})

		expectHaltReported(d, "clear internal breakpoint")
		fb.clearFaults()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
	})

	It("reports a halt when a delayed internal-breakpoint hit cannot read its thread", func() {
		const addr = uint64(0x4190)
		retireInternalBreakpoint(fb, d, addr)
		runWithWaitLoop(d)

		fb.failRegisters(errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 2, PC: addr})

		expectHaltReported(d, "read registers for retired breakpoint on thread 2")
		fb.clearFaults()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
	})

	It("reports a halt when retired-sentinel bytes cannot be verified", func() {
		const addr = uint64(0x4198)
		retireInternalBreakpoint(fb, d, addr)
		runWithWaitLoop(d)

		fb.failReadAt(addr, errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 2, PC: addr})

		expectHaltReported(d, "inspect retired breakpoint")
		fb.clearFaults()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
	})

	It("reports a halt when a delayed internal-breakpoint hit cannot rewind its thread", func() {
		if len(debugger.ExportedTrapInstruction()) != 1 {
			Skip("the delayed-PC rewind failure requires an architecture that advances past its trap")
		}
		const addr = uint64(0x41a0)
		retireInternalBreakpoint(fb, d, addr)
		fb.regs[2] = debugger.Registers{PC: addr + 1}
		runWithWaitLoop(d)

		fb.failSetRegisters(errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 2, PC: addr})

		expectHaltReported(d, "rewind retired breakpoint on thread 2")
		fb.clearFaults()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
	})

	It("reports a halt when a delayed internal-breakpoint hit cannot resume", func() {
		const addr = uint64(0x41b0)
		retireInternalBreakpoint(fb, d, addr)
		fb.regs[2] = debugger.Registers{PC: addr}
		runWithWaitLoop(d)

		fb.failContinue(errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 2, PC: addr})

		expectHaltReported(d, "continue after retired breakpoint on thread 2")
		fb.clearFaults()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
	})

	// Site D — StopSingleStep/bpResumeStepOut: the temporary return breakpoint
	// cannot be set. Uniquely, this site left the engine in stateRunning while
	// the tracee was halted, so it also has to correct the state.
	It("reports a halt when the StepOut return breakpoint cannot be set", func() {
		const bpAddr = uint64(0x4200)
		const retAddr = uint64(0x9900)
		arriveAtBreakpoint(fb, d, bpAddr)

		// Frame chain so stepOut can resolve the return address.
		seedFrameChain(fb, bpAddr, 0x7000, 0x7100, retAddr)
		fb.seedMem(retAddr, []byte{0x90, 0x90, 0x90, 0x90, 0x90})

		Expect(d.StepOut()).To(Succeed())

		fb.failReadAt(retAddr, errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + 4})

		expectHaltReported(d, "return breakpoint")

		fb.clearFaults()
		before := fb.continueCount()
		Expect(d.Continue()).To(Succeed(),
			"the engine must be suspended, not left believing it is running")
		Expect(fb.continueCount()).To(BeNumerically(">", before))
	})

	// Site E — StopSignal: in-flight step-over reinstall failure.
	It("reports a halt when a signal interrupts an unreinstallable step-over", func() {
		const bpAddr = uint64(0x4300)
		arriveAtBreakpoint(fb, d, bpAddr)

		continueAndConsumeContinued(d)

		fb.failWriteAt(bpAddr, errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSignal, TID: 1, Signal: 11})

		expectHaltReported(d, "after signal")

		fb.clearFaults()
		before := fb.continueCount()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
		Expect(fb.continueCount()).To(BeNumerically(">", before))
	})

	// The suspending event is the load-bearing half: emit drops events when the
	// buffer is full, so a saturated buffer used to lose the Paused (and, once
	// the cause was also dropped, report nothing at all) and recreate the exact
	// strand this change prevents. The reserved slot must make the suspend
	// survive even with zero ordinary slots left.
	It("keeps the suspend and stays resumable with zero free ordinary slots", func() {
		const bpAddr = uint64(0x4700)
		arriveAtBreakpoint(fb, d, bpAddr)
		continueAndConsumeContinued(d)

		debugger.ExportedFillEventBuffer(d, 0)

		fb.failWriteAt(bpAddr, errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + 4})

		// Continue is dispatched and requires stateSuspended, so it starts
		// succeeding only once the halt has been fully handled. Retrying it
		// synchronises on that without draining the buffer, and its success
		// proves the session re-entered the suspended gate. The write fault
		// stays armed: after the halt lastBP is nil, so the retry is a plain
		// ContinueProcess that touches no memory.
		before := fb.continueCount()
		Eventually(d.Continue).Should(Succeed(),
			"the halt never suspended the engine")
		Expect(fb.continueCount()).To(BeNumerically(">", before),
			"the retried Continue never reached the backend")

		var kinds []protocol.EventKind
		for {
			select {
			case evt := <-d.Events():
				kinds = append(kinds, evt.Kind)
				continue
			default:
			}
			break
		}
		Expect(kinds).To(ContainElement(protocol.EventPaused),
			"the reserved slot must carry the suspend even when the buffer is full")
	})

	// A halt whose stop never resolved a thread must not overwrite curTID with
	// the Threads()[0] guess. curTID is what every later step primitive
	// targets, and on darwin threads[0] is frequently an idle runtime M.
	It("keeps the previous current thread when the stop's TID is unresolved", func() {
		const bpAddr = uint64(0x4600)
		const stoppedTID = 7

		Expect(fb.tids).To(Equal([]int{1}), "threads[0] must differ from the stopped thread")

		fb.seedMem(bpAddr, []byte{0x90, 0x90, 0x90, 0x90, 0x90})
		debugger.ExportedForceSuspended(d)
		debugger.ExportedSetBreakpointAt(d, bpAddr)
		runWithWaitLoop(d)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: stoppedTID, PC: bpAddr})
		Expect(mustNextEvent(d).Kind).To(Equal(protocol.EventBreakpointHit))

		Expect(d.StepInto()).To(Succeed())

		// The step's stop carries no TID, and resolving one fails.
		fb.failRegisters(errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep})
		expectHaltReported(d, "get stop PC")

		fb.clearFaults()
		before := len(fb.singleStepCalls)
		Expect(d.StepInto()).To(Succeed())
		Expect(fb.singleStepCalls).To(HaveLen(before+1), "a step must have been issued")
		Expect(fb.singleStepCalls[before]).To(Equal(stoppedTID),
			"the guessed thread leaked into curTID")
	})

	// Site F — StopSignal/manual Pause: populateStopPC failure.
	It("reports a halt when a Pause stop's PC cannot be read", func() {
		runWithWaitLoop(d)
		Expect(d.Pause()).To(Succeed())

		fb.failRegisters(errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSignal, Signal: fb.PauseSignal()})

		expectHaltReported(d, "get stop PC")

		fb.clearFaults()
		Expect(d.Continue()).To(Succeed(), "the session must remain resumable")
	})
})

// haltConn is a minimal hub.WSConn for the integration spec below. The hub's
// own fakeWSConn lives in package hub_test and cannot be reused from here.
type haltConn struct {
	mu       sync.Mutex
	incoming chan []byte
	outgoing chan []byte
	closed   bool
}

func newHaltConn() *haltConn {
	return &haltConn{
		incoming: make(chan []byte, 256),
		outgoing: make(chan []byte, 32),
	}
}

func (c *haltConn) send(cmd protocol.Command) {
	data, err := json.Marshal(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if !closed {
		c.outgoing <- data
	}
}

// awaitEvent drains broadcasts until kind arrives, so auxiliary events
// (snapshots, session state) don't make the assertion order-sensitive.
func (c *haltConn) awaitEvent(kind protocol.EventKind) bool {
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-c.incoming:
			var evt protocol.Event
			if err := json.Unmarshal(msg, &evt); err != nil {
				continue
			}
			if evt.Kind == kind {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func (c *haltConn) ReadMessage() (int, []byte, error) {
	data, ok := <-c.outgoing
	if !ok {
		return 0, nil, errInjected
	}
	return hub.TextMessage, data, nil
}

func (c *haltConn) WriteMessage(_ int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errInjected
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	c.incoming <- cp
	return nil
}

func (c *haltConn) SetReadLimit(int64)                {}
func (c *haltConn) SetReadDeadline(time.Time) error   { return nil }
func (c *haltConn) SetWriteDeadline(time.Time) error  { return nil }
func (c *haltConn) SetPongHandler(func(string) error) {}

func (c *haltConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.outgoing)
	}
	return nil
}

// This is the end-to-end proof: a REAL hub driving a REAL engine over a
// fault-injected backend. It is deliberately not in internal/hub, whose
// fakeDebugger cannot exercise the engine's asynchronous stop handling.
var _ = Describe("hub recovery from an asynchronous engine halt", func() {
	It("keeps a session resumable after a failed breakpoint reinstall", func() {
		const bpAddr = uint64(0x4400)

		fb := newFakeBackend()
		d := debugger.NewWithBackend(fb, nil)
		h := hub.New(d, nil)

		conn := newHaltConn()
		_, err := h.AddClient(conn, nil)
		Expect(err).NotTo(HaveOccurred())

		ctx, cancel := context.WithCancel(context.Background())
		go h.Run(ctx)
		defer func() {
			cancel()
			fb.clearFaults()
			_ = d.Kill()
			if !fb.stopped {
				close(fb.stopCh)
				fb.stopped = true
			}
		}()

		arriveAtBreakpoint := func() {
			// Deliberately inlined rather than sharing the engine-only helper:
			// the hub's Run loop is the sole consumer of d.Events(), so the
			// setup must never read that channel itself.
			fb.seedMem(bpAddr, []byte{0x90, 0x90, 0x90, 0x90, 0x90})
			debugger.ExportedForceSuspended(d)
			debugger.ExportedSetBreakpointAt(d, bpAddr)

			// Drive the first resume straight at the engine: it is what starts
			// the waitLoop, and a CmdContinue sent while the hub is running
			// would sit unread in resumeCh (that is the very bug under test).
			Expect(d.Continue()).To(Succeed())
			fb.pushStop(debugger.StopEvent{Reason: debugger.StopBreakpoint, TID: 1, PC: bpAddr})
		}

		arriveAtBreakpoint()
		Expect(conn.awaitEvent(protocol.EventBreakpointHit)).To(BeTrue())
		Eventually(h.State).Should(Equal(protocol.StateSuspended))

		// A resume the hub accepts: the engine arms the step-over single-step,
		// answers nil, and the hub leaves its suspend wait loop.
		conn.send(protocol.Command{Version: protocol.Version, Kind: protocol.CmdContinue})
		Expect(conn.awaitEvent(protocol.EventContinued)).To(BeTrue())
		Eventually(h.State).Should(Equal(protocol.StateRunning))

		// The step-over now fails asynchronously, far from the resume that the
		// hub already reported as successful.
		fb.failWriteAt(bpAddr, errInjected)
		fb.pushStop(debugger.StopEvent{Reason: debugger.StopSingleStep, TID: 1, PC: bpAddr + 4})

		Expect(conn.awaitEvent(protocol.EventError)).To(BeTrue())
		Eventually(h.State).Should(Equal(protocol.StateSuspended),
			"the halted tracee must be reported as suspended, not running")

		fb.clearFaults()
		before := fb.continueCount()

		// The decisive assertion: resumeCh is drained only inside the hub's
		// suspend wait loop, so this second Continue reaches the engine only if
		// the halt was reported as a suspending event.
		conn.send(protocol.Command{Version: protocol.Version, Kind: protocol.CmdContinue})
		Eventually(fb.continueCount, 2*time.Second).
			Should(BeNumerically(">", before),
				"the retried Continue never reached the backend — the session is stranded")
	})
})
