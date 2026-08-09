package hub_test

import (
	"context"
	"fmt"
	"sort"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// tableDebugger is a debugger fake with a faithful breakpoint table: every
// instance allocates ids from its own counter, so a replacement instance hands
// out the same numbers its predecessor did — the reuse that makes a stale
// physical id dangerous (issue #200).
type tableDebugger struct {
	mu     sync.Mutex
	events chan protocol.Event
	calls  []string

	nextID int
	armed  map[int]protocol.Location

	// setErrFor fails SetBreakpoint for a specific line (0 disables).
	setErrFor int
	clearErr  error
}

func newTableDebugger() *tableDebugger {
	return &tableDebugger{
		events: make(chan protocol.Event, 32),
		armed:  make(map[int]protocol.Location),
	}
}

func (d *tableDebugger) record(call string) {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
}

func (d *tableDebugger) recordedCalls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	cp := make([]string, len(d.calls))
	copy(cp, d.calls)
	return cp
}

// armedLines returns the source lines of every armed breakpoint, sorted, so a
// test can assert exactly which traps survive an operation.
func (d *tableDebugger) armedLines() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	lines := make([]int, 0, len(d.armed))
	for _, loc := range d.armed {
		lines = append(lines, loc.Line)
	}
	sort.Ints(lines)
	return lines
}

// physicalIDFor returns the id this engine gave the breakpoint at line, or 0.
func (d *tableDebugger) physicalIDFor(line int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, loc := range d.armed {
		if loc.Line == line {
			return id
		}
	}
	return 0
}

func (d *tableDebugger) setNextID(n int) {
	d.mu.Lock()
	d.nextID = n
	d.mu.Unlock()
}

// rearm models the engine's step-off resurrection: resuming from the breakpoint
// the process is parked on replays the entry pointer stashed at hit time and
// reinstalls it under the SAME physical id, even though the client's clear had
// already removed it (internal/debugger: resumeFromBreakpoint →
// breakpointTable.reinstall → addToTable).
func (d *tableDebugger) rearm(physicalID int, loc protocol.Location) {
	d.mu.Lock()
	d.armed[physicalID] = loc
	d.mu.Unlock()
}

func (d *tableDebugger) setClearErr(err error) {
	d.mu.Lock()
	d.clearErr = err
	d.mu.Unlock()
}

func (d *tableDebugger) Events() <-chan protocol.Event { return d.events }
func (d *tableDebugger) closeEvents()                  { close(d.events) }

func (d *tableDebugger) Launch(string, []string, []string) error { d.record("Launch"); return nil }
func (d *tableDebugger) Attach(int, string) error                { d.record("Attach"); return nil }
func (d *tableDebugger) Kill() error                             { d.record("Kill"); return nil }
func (d *tableDebugger) Continue() error                         { d.record("Continue"); return nil }
func (d *tableDebugger) StepOver() error                         { d.record("StepOver"); return nil }
func (d *tableDebugger) StepInto() error                         { d.record("StepInto"); return nil }
func (d *tableDebugger) StepOut() error                          { d.record("StepOut"); return nil }
func (d *tableDebugger) Pause() error                            { d.record("Pause"); return nil }

func (d *tableDebugger) SetBreakpoint(file string, line int) (protocol.Breakpoint, error) {
	d.record(fmt.Sprintf("SetBreakpoint(%s:%d)", file, line))
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.setErrFor != 0 && d.setErrFor == line {
		return protocol.Breakpoint{}, fmt.Errorf("no such line %d", line)
	}
	d.nextID++
	loc := protocol.Location{File: file, Line: line}
	d.armed[d.nextID] = loc
	return protocol.Breakpoint{ID: d.nextID, Location: loc, Enabled: true}, nil
}

func (d *tableDebugger) ClearBreakpoint(id int) error {
	d.record(fmt.Sprintf("ClearBreakpoint(%d)", id))
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.clearErr != nil {
		return d.clearErr
	}
	if _, ok := d.armed[id]; !ok {
		return fmt.Errorf("breakpoint %d not found", id)
	}
	delete(d.armed, id)
	return nil
}

func (d *tableDebugger) Locals(int) ([]protocol.Variable, error) { return nil, nil }
func (d *tableDebugger) Evaluate(int, string) (protocol.Variable, error) {
	return protocol.Variable{}, nil
}
func (d *tableDebugger) StackFrames() ([]protocol.Frame, error)    { return nil, nil }
func (d *tableDebugger) Goroutines() ([]protocol.Goroutine, error) { return nil, nil }
func (d *tableDebugger) GoroutineSnapshot() (protocol.GoroutineSnapshotPayload, error) {
	return protocol.GoroutineSnapshotPayload{}, nil
}

// debuggerFleet hands the hub a fresh tableDebugger per Launch/Restart and
// keeps every instance so a test can inspect the physical state of the engine
// that actually holds the traps.
type debuggerFleet struct {
	mu        sync.Mutex
	created   []*tableDebugger
	configure func(index int, d *tableDebugger)
}

func (f *debuggerFleet) factory() func() debugger.Debugger {
	return func() debugger.Debugger {
		f.mu.Lock()
		defer f.mu.Unlock()
		d := newTableDebugger()
		if f.configure != nil {
			f.configure(len(f.created), d)
		}
		f.created = append(f.created, d)
		return d
	}
}

func (f *debuggerFleet) at(i int) *tableDebugger {
	f.mu.Lock()
	defer f.mu.Unlock()
	ExpectWithOffset(1, len(f.created)).To(BeNumerically(">", i), "engine %d was never created", i)
	return f.created[i]
}

func (f *debuggerFleet) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created)
}

// setBP installs a breakpoint through the hub and returns the logical id the
// hub broadcast for it.
func setBP(conn *fakeWSConn, file string, line int) int {
	GinkgoHelper()
	conn.inject(mustCommand(protocol.CmdSetBreakpoint,
		protocol.SetBreakpointPayload{File: file, Line: line}))
	var p protocol.BreakpointSetPayload
	waitForEventKind(conn, protocol.EventBreakpointSet, &p)
	return p.Breakpoint.ID
}

// Logical breakpoint identity (issue #200). Physical ids belong to one engine
// and are reused by its replacement, so the hub owns a session-stable logical
// id per breakpoint and translates in both directions.
var _ = Describe("logical breakpoint ids", func() {
	var (
		fleet  *debuggerFleet
		h      *hub.Hub
		conn   *fakeWSConn
		cancel context.CancelFunc
	)

	// startManaged brings up a managed session whose first engine allocates
	// physical ids from 101, so any logical id that leaked a physical one is
	// immediately visible.
	startManaged := func(configure func(int, *tableDebugger)) {
		fleet = &debuggerFleet{configure: func(i int, d *tableDebugger) {
			if i == 0 {
				d.setNextID(100)
			}
			if configure != nil {
				configure(i, d)
			}
		}}
		h = hub.NewSession("session", fleet.factory(), nil)
		cancel = runHub(h)
		conn = newFakeWSConn()
		_, err := h.AddClient(conn, nil)
		Expect(err).NotTo(HaveOccurred())
		_, _ = recvEvent(conn) // welcome
		conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "myapp"}))
		waitForEventKind(conn, protocol.EventSessionState, nil)
	}

	AfterEach(func() {
		if cancel != nil {
			cancel()
		}
	})

	It("broadcasts a logical id, never the engine's physical id", func() {
		startManaged(nil)

		logical := setBP(conn, "main.go", 10)
		Expect(fleet.at(0).physicalIDFor(10)).To(Equal(101),
			"engine should have allocated its own physical id")
		Expect(logical).NotTo(Equal(101), "clients must not see raw engine ids")
		Expect(logical).To(Equal(1), "logical ids start at 1 for the session")
	})

	It("translates a clear to the active engine's physical id", func() {
		startManaged(nil)

		logical := setBP(conn, "main.go", 10)
		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))

		var cleared protocol.BreakpointClearedPayload
		waitForEventKind(conn, protocol.EventBreakpointCleared, &cleared)
		Expect(cleared.ID).To(Equal(logical), "confirmation reports the logical id")
		Expect(fleet.at(0).recordedCalls()).To(ContainElement("ClearBreakpoint(101)"))
		Expect(fleet.at(0).armedLines()).To(BeEmpty())
	})

	It("rejects an unknown logical id without touching the debugger", func() {
		startManaged(nil)

		logical := setBP(conn, "main.go", 10)
		Expect(logical).To(Equal(1))

		// 101 is the *physical* id of the live breakpoint. A client that
		// somehow held it must not be able to disarm anything with it.
		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: 101}))

		var errPayload protocol.ErrorPayload
		waitForEventKind(conn, protocol.EventError, &errPayload)
		Expect(errPayload.Command).To(Equal(protocol.CmdClearBreakpoint))
		Expect(fleet.at(0).armedLines()).To(Equal([]int{10}), "breakpoint must stay armed")
		for _, call := range fleet.at(0).recordedCalls() {
			Expect(call).NotTo(HavePrefix("ClearBreakpoint"),
				"an unknown logical id must never reach the debugger")
		}
	})

	It("retains the mapping when the debugger rejects a clear", func() {
		startManaged(nil)

		logical := setBP(conn, "main.go", 10)
		fleet.at(0).setClearErr(fmt.Errorf("restore bytes failed"))

		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))
		waitForEventKind(conn, protocol.EventError, nil)

		// The trap is still armed, so the client must still be able to name it.
		fleet.at(0).setClearErr(nil)
		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))
		var cleared protocol.BreakpointClearedPayload
		waitForEventKind(conn, protocol.EventBreakpointCleared, &cleared)
		Expect(cleared.ID).To(Equal(logical))
		Expect(fleet.at(0).armedLines()).To(BeEmpty())
	})

	It("translates a BreakpointHit's physical id to the logical id", func() {
		startManaged(nil)

		logical := setBP(conn, "main.go", 10)
		physical := fleet.at(0).physicalIDFor(10)

		fleet.at(0).events <- protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{
				ID:       physical,
				Location: protocol.Location{File: "main.go", Line: 10},
			}})

		var hit protocol.BreakpointHitPayload
		waitForEventKind(conn, protocol.EventBreakpointHit, &hit)
		Expect(hit.Breakpoint.ID).To(Equal(logical))
		Expect(hit.Breakpoint.Location.Line).To(Equal(10), "location must survive re-encoding")
	})

	It("preserves logical ids across Restart while physical ids compact", func() {
		startManaged(nil)

		logicalA := setBP(conn, "main.go", 10)
		logicalB := setBP(conn, "main.go", 20)
		Expect(fleet.at(0).physicalIDFor(10)).To(Equal(101))
		Expect(fleet.at(0).physicalIDFor(20)).To(Equal(102))

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)

		Expect(fleet.count()).To(Equal(2))
		fresh := fleet.at(1)
		Expect(fresh.physicalIDFor(10)).To(Equal(1), "fresh engine reuses low ids")
		Expect(fresh.physicalIDFor(20)).To(Equal(2))

		ids := make([]int, 0, len(restarted.Breakpoints))
		for _, bp := range restarted.Breakpoints {
			ids = append(ids, bp.ID)
		}
		Expect(ids).To(Equal([]int{logicalA, logicalB}),
			"restart must report the identities clients already hold")

		// A clear for logical A must reach the *new* engine's A, not whatever
		// breakpoint inherited A's old number.
		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logicalA}))
		waitForEventKind(conn, protocol.EventBreakpointCleared, nil)
		Expect(fresh.armedLines()).To(Equal([]int{20}))
		Expect(fresh.recordedCalls()).To(ContainElement("ClearBreakpoint(1)"))
	})

	It("rejects a later clear for a breakpoint the restart discarded", func() {
		startManaged(func(i int, d *tableDebugger) {
			if i == 1 {
				d.setErrFor = 10 // line 10 no longer resolves after the relaunch
			}
		})

		logicalA := setBP(conn, "main.go", 10)
		setBP(conn, "main.go", 20)

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)
		Expect(restarted.Discarded).To(HaveLen(1))
		Expect(restarted.Discarded[0].Location.Line).To(Equal(10))
		Expect(restarted.Breakpoints).To(HaveLen(1))

		fresh := fleet.at(1)
		Expect(fresh.armedLines()).To(Equal([]int{20}))

		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logicalA}))
		waitForEventKind(conn, protocol.EventError, nil)
		Expect(fresh.armedLines()).To(Equal([]int{20}),
			"a discarded logical id must not disarm the surviving breakpoint")
	})

	// The engine emits a hit into a buffered channel and ClearBreakpoint has no
	// suspend guard, so Run's select can execute a queued clear before draining
	// a hit that was generated first. The hit must still report the id the
	// client held, and must not resurrect the breakpoint it just removed.
	It("reports a hit that raced its own clear under the id the client held", func() {
		startManaged(nil)

		logical := setBP(conn, "main.go", 10)
		physical := fleet.at(0).physicalIDFor(10)

		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))
		waitForEventKind(conn, protocol.EventBreakpointCleared, nil)

		fleet.at(0).events <- protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{
				ID:       physical,
				Location: protocol.Location{File: "main.go", Line: 10},
			}})

		var hit protocol.BreakpointHitPayload
		waitForEventKind(conn, protocol.EventBreakpointHit, &hit)
		Expect(hit.Breakpoint.ID).To(Equal(logical),
			"a late hit must name the breakpoint the client knew, not a new id")

		// Having reported that id, the hub must still accept it: a client that
		// reacts to the hit by clearing it again has to reach the debugger
		// rather than be told the id does not exist.
		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))
		Eventually(func() []string { return fleet.at(0).recordedCalls() }).
			Should(ContainElement(fmt.Sprintf("ClearBreakpoint(%d)", physical)),
				"a reported id must stay resolvable to this engine's breakpoint")

		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)
		Expect(restarted.Breakpoints).To(BeEmpty())
		Expect(fleet.at(1).armedLines()).To(BeEmpty(),
			"restart must not re-arm a breakpoint the client removed")
	})

	// The engine undoes a clear issued while the process is parked on that very
	// breakpoint: stepping off replays the entry stashed at hit time and
	// reinstalls it under the same physical id. The hub had already retired the
	// mapping, so without a tombstone the resurrected trap is reported under an
	// id the hub no longer accepts — permanently unremovable, and unsettable too
	// because the engine still has the address armed.
	It("keeps a step-off-resurrected breakpoint clearable", func() {
		startManaged(nil)

		logical := setBP(conn, "main.go", 10)
		physical := fleet.at(0).physicalIDFor(10)

		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))
		waitForEventKind(conn, protocol.EventBreakpointCleared, nil)
		Expect(fleet.at(0).armedLines()).To(BeEmpty())

		fleet.at(0).rearm(physical, protocol.Location{File: "main.go", Line: 10})
		fleet.at(0).events <- protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{
				ID:       physical,
				Location: protocol.Location{File: "main.go", Line: 10},
			}})

		var hit protocol.BreakpointHitPayload
		waitForEventKind(conn, protocol.EventBreakpointHit, &hit)
		Expect(hit.Breakpoint.ID).To(Equal(logical))

		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))
		var cleared protocol.BreakpointClearedPayload
		waitForEventKind(conn, protocol.EventBreakpointCleared, &cleared)
		Expect(cleared.ID).To(Equal(logical))
		Expect(fleet.at(0).armedLines()).To(BeEmpty(),
			"the resurrected trap must actually be removed from the engine")
	})

	// The tombstone that keeps a resurrected id clearable must not outlive its
	// engine, or it would reintroduce the aliasing this whole layer prevents.
	It("does not let a retired id survive into a replacement engine", func() {
		startManaged(nil)

		logical := setBP(conn, "main.go", 10)
		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))
		waitForEventKind(conn, protocol.EventBreakpointCleared, nil)

		// A breakpoint the restart DOES carry over, so the fresh engine hands
		// out the physical id the retired one used to hold.
		survivor := setBP(conn, "main.go", 20)
		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		waitForEventKind(conn, protocol.EventRestarted, nil)

		fresh := fleet.at(1)
		Expect(fresh.armedLines()).To(Equal([]int{20}))

		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: logical}))
		var errPayload protocol.ErrorPayload
		waitForEventKind(conn, protocol.EventError, &errPayload)
		Expect(fresh.armedLines()).To(Equal([]int{20}),
			"a retired id from the previous engine must not disarm anything")
		Expect(fresh.recordedCalls()).NotTo(ContainElement(
			ContainSubstring("ClearBreakpoint")),
			"the replacement engine must never see a retired id")
		Expect(survivor).NotTo(Equal(logical))
	})

	It("never re-arms a breakpoint the hub did not install", func() {
		startManaged(nil)

		// A physical id the hub never minted — a raw debugger driven directly.
		fleet.at(0).events <- protocol.MustEvent(protocol.EventBreakpointHit, 1,
			protocol.BreakpointHitPayload{Breakpoint: protocol.Breakpoint{
				ID:       999,
				Location: protocol.Location{File: "other.go", Line: 42},
			}})

		var hit protocol.BreakpointHitPayload
		waitForEventKind(conn, protocol.EventBreakpointHit, &hit)
		Expect(hit.Breakpoint.ID).NotTo(Equal(999), "engine ids must not reach clients")

		installed := setBP(conn, "main.go", 10)
		conn.inject(mustCommand(protocol.CmdRestart, protocol.RestartPayload{}))
		var restarted protocol.RestartedPayload
		waitForEventKind(conn, protocol.EventRestarted, &restarted)

		Expect(restarted.Breakpoints).To(HaveLen(1))
		Expect(restarted.Breakpoints[0].ID).To(Equal(installed))
		Expect(fleet.at(1).armedLines()).To(Equal([]int{10}),
			"only hub-installed breakpoints are restart targets")
	})

	It("does not re-mint logical ids after a fresh Launch", func() {
		startManaged(nil)

		staleLogical := setBP(conn, "main.go", 10)

		// The target exits; the session goes idle and is launched again.
		fleet.at(0).closeEvents()
		Eventually(h.State, "1s", "10ms").Should(Equal(protocol.StateIdle))

		conn.inject(mustCommand(protocol.CmdLaunch, protocol.LaunchPayload{Program: "myapp"}))
		Eventually(h.State, "1s", "10ms").Should(Equal(protocol.StateRunning))
		Expect(fleet.count()).To(Equal(2))

		fresh := setBP(conn, "main.go", 20)
		Expect(fresh).To(BeNumerically(">", staleLogical),
			"logical ids stay monotonic across a fresh Launch")

		// A clear generated against the previous target can still arrive. It
		// must be rejected, not aliased onto the new target's breakpoint.
		conn.inject(mustCommand(protocol.CmdClearBreakpoint,
			protocol.ClearBreakpointPayload{ID: staleLogical}))
		waitForEventKind(conn, protocol.EventError, nil)
		Expect(fleet.at(1).armedLines()).To(Equal([]int{20}))
	})
})
