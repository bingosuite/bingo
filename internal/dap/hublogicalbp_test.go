package dap

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/internal/hub"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// This file pins the fix for issue #200 through the real stack: a real
// hub.Hub, a real dap.Handler, and a gating hub.WSConn interposed at the exact
// Handler.ReadMessage -> Client.readPump boundary where a marshalled command
// has left the handler but has not yet been injected into the hub.
//
// The hazard: the handler decides to clear breakpoint A and hands the bytes to
// the read pump. Before the pump injects them, another client completes a
// Restart. The replacement engine allocates physical ids from 1 again, so A's
// old physical id can name a *different* breakpoint in the new process. With
// hub-owned logical ids the delayed clear still resolves to A.

// raceDebugger is a debugger fake with a faithful per-instance breakpoint
// table (ids from 1) plus an entry stop on Launch, mirroring the engine's
// emitStoppedAtCurrentPC so the DAP handshake can complete.
type raceDebugger struct {
	mu     sync.Mutex
	events chan protocol.Event
	nextID int
	armed  map[int]protocol.Location
	closed bool
}

func newRaceDebugger() *raceDebugger {
	return &raceDebugger{
		events: make(chan protocol.Event, 64),
		armed:  make(map[int]protocol.Location),
	}
}

func (d *raceDebugger) Events() <-chan protocol.Event { return d.events }

func (d *raceDebugger) emitEntryStop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.events <- protocol.MustEvent(protocol.EventStepped, 0, protocol.SteppedPayload{
		Location: protocol.Location{File: "main.go", Line: 1},
	})
}

func (d *raceDebugger) Launch(string, []string, []string) error {
	go d.emitEntryStop()
	return nil
}

func (d *raceDebugger) Attach(int, string) error { return nil }

func (d *raceDebugger) Kill() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

func (d *raceDebugger) Continue() error { return nil }
func (d *raceDebugger) StepOver() error { return nil }
func (d *raceDebugger) StepInto() error { return nil }
func (d *raceDebugger) StepOut() error  { return nil }
func (d *raceDebugger) Pause() error    { return nil }

func (d *raceDebugger) SetBreakpoint(file string, line int) (protocol.Breakpoint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	loc := protocol.Location{File: file, Line: line}
	d.armed[d.nextID] = loc
	return protocol.Breakpoint{ID: d.nextID, Location: loc, Enabled: true}, nil
}

func (d *raceDebugger) ClearBreakpoint(id int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.armed[id]; !ok {
		return fmt.Errorf("breakpoint %d not found", id)
	}
	delete(d.armed, id)
	return nil
}

func (d *raceDebugger) Locals(int) ([]protocol.Variable, error) { return nil, nil }
func (d *raceDebugger) Evaluate(int, string) (protocol.Variable, error) {
	return protocol.Variable{}, nil
}
func (d *raceDebugger) StackFrames() ([]protocol.Frame, error)    { return nil, nil }
func (d *raceDebugger) Goroutines() ([]protocol.Goroutine, error) { return nil, nil }
func (d *raceDebugger) GoroutineSnapshot() (protocol.GoroutineSnapshotPayload, error) {
	return protocol.GoroutineSnapshotPayload{}, nil
}

// armedLines returns the lines this engine still has armed, sorted.
func (d *raceDebugger) armedLines() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]int, 0, len(d.armed))
	for _, loc := range d.armed {
		out = append(out, loc.Line)
	}
	sort.Ints(out)
	return out
}

func (d *raceDebugger) physicalIDFor(line int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, loc := range d.armed {
		if loc.Line == line {
			return id
		}
	}
	return 0
}

// engineFleet hands the hub a fresh raceDebugger per Launch/Restart, retaining
// each one so the test can assert against the engine that actually holds the
// traps.
type engineFleet struct {
	mu      sync.Mutex
	created []*raceDebugger
}

func (f *engineFleet) factory() func() debugger.Debugger {
	return func() debugger.Debugger {
		d := newRaceDebugger()
		f.mu.Lock()
		f.created = append(f.created, d)
		f.mu.Unlock()
		return d
	}
}

func (f *engineFleet) at(t *testing.T, i int) *raceDebugger {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) <= i {
		t.Fatalf("engine %d was never created (have %d)", i, len(f.created))
	}
	return f.created[i]
}

// gatingConn wraps the DAP handler as the hub sees it and can hold a
// ClearBreakpoint command after ReadMessage has produced it but before the hub
// read pump injects it — the exact window issue #200 describes.
type gatingConn struct {
	hub.WSConn

	mu   sync.Mutex
	hold chan struct{}
	held chan struct{}
}

func (g *gatingConn) arm() {
	g.mu.Lock()
	g.hold = make(chan struct{})
	g.held = make(chan struct{}, 1)
	g.mu.Unlock()
}

func (g *gatingConn) waitHeld(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	held := g.held
	g.mu.Unlock()
	select {
	case <-held:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the handler to produce a ClearBreakpoint")
	}
}

func (g *gatingConn) release() {
	g.mu.Lock()
	hold := g.hold
	g.hold = nil
	g.mu.Unlock()
	if hold != nil {
		close(hold)
	}
}

func (g *gatingConn) ReadMessage() (int, []byte, error) {
	mt, data, err := g.WSConn.ReadMessage()
	if err != nil {
		return mt, data, err
	}
	cmd, cerr := protocol.UnmarshalCommand(data)
	if cerr != nil || cmd.Kind != protocol.CmdClearBreakpoint {
		return mt, data, err
	}
	g.mu.Lock()
	hold, held := g.hold, g.held
	g.mu.Unlock()
	if hold == nil {
		return mt, data, err
	}
	select {
	case held <- struct{}{}:
	default:
	}
	<-hold
	return mt, data, err
}

// hubSession adapts a real *hub.Hub to the DAP Provider/Session interfaces,
// interposing the gating connection so the test controls command delivery.
type hubSession struct {
	hb   *hub.Hub
	gate *gatingConn
}

func (s *hubSession) SessionID() string { return "race-session" }

func (s *hubSession) AddClient(conn hub.WSConn, log *slog.Logger) (*hub.Client, error) {
	s.gate.WSConn = conn
	return s.hb.AddClient(s.gate, log)
}

type raceHubProvider struct{ sess *hubSession }

func (p *raceHubProvider) CreateSession() (Session, error)   { return p.sess, nil }
func (p *raceHubProvider) GetSession(string) (Session, bool) { return p.sess, true }

// observer is a plain WebSocket client on the same hub: it both drives Restart
// and records the bingo events every client sees, so the test can assert that
// WebSocket and DAP observe the same stable ids.
type observer struct {
	mu     sync.Mutex
	out    chan []byte
	events []protocol.Event
	closed chan struct{}
	once   sync.Once
}

func newObserver() *observer {
	return &observer{out: make(chan []byte, 64), closed: make(chan struct{})}
}

func (o *observer) ReadMessage() (int, []byte, error) {
	select {
	case data := <-o.out:
		return hub.TextMessage, data, nil
	case <-o.closed:
		return 0, nil, net.ErrClosed
	}
}

func (o *observer) WriteMessage(_ int, data []byte) error {
	evt, err := protocol.UnmarshalEvent(data)
	if err != nil {
		return nil
	}
	o.mu.Lock()
	o.events = append(o.events, evt)
	o.mu.Unlock()
	return nil
}

func (o *observer) Close() error {
	o.once.Do(func() { close(o.closed) })
	return nil
}

func (o *observer) SetReadLimit(int64)                {}
func (o *observer) SetReadDeadline(time.Time) error   { return nil }
func (o *observer) SetWriteDeadline(time.Time) error  { return nil }
func (o *observer) SetPongHandler(func(string) error) {}

func (o *observer) send(t *testing.T, kind protocol.CommandKind, payload any) {
	t.Helper()
	data, err := marshalCommand(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case o.out <- data:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out sending %s", kind)
	}
}

// waitEvent polls for the first event of kind, decoding it into out.
func (o *observer) waitEvent(t *testing.T, kind protocol.EventKind, out any) protocol.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		for _, e := range o.events {
			if e.Kind == kind {
				o.mu.Unlock()
				if out != nil {
					if err := protocol.DecodeEventPayload(e, out); err != nil {
						t.Fatal(err)
					}
				}
				return e
			}
		}
		o.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for event %s", kind)
	return protocol.Event{}
}

// setIDs returns the breakpoint ids from every BreakpointSet event seen so far.
func (o *observer) setIDs(t *testing.T) []int {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	var ids []int
	for _, e := range o.events {
		if e.Kind != protocol.EventBreakpointSet {
			continue
		}
		var p protocol.BreakpointSetPayload
		if err := protocol.DecodeEventPayload(e, &p); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.Breakpoint.ID)
	}
	return ids
}

// raceRig is a real hub + real DAP handler + a WebSocket observer client.
type raceRig struct {
	t       *testing.T
	hub     *hub.Hub
	fleet   *engineFleet
	gate    *gatingConn
	handler *Handler
	client  net.Conn
	reader  *bufio.Reader
	codec   *godap.Codec
	obs     *observer
	seq     int
}

func newRaceRig(t *testing.T) *raceRig {
	t.Helper()
	log := slog.New(slog.NewTextHandler(nopWriter{}, nil))

	fleet := &engineFleet{}
	hb := hub.NewSession("race-session", fleet.factory(), log)
	done := make(chan struct{})
	go func() {
		defer close(done)
		hb.Run(t.Context())
	}()

	gate := &gatingConn{}
	prov := &raceHubProvider{sess: &hubSession{hb: hb, gate: gate}}

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	serverConn := <-accepted

	handler := NewHandler(serverConn, prov, log)
	go handler.Serve()

	codec := godap.NewCodec()
	if err := codec.RegisterEvent(sessionEventName, func() godap.Message { return new(sessionEvent) }); err != nil {
		t.Fatal(err)
	}

	obs := newObserver()
	if _, err := hb.AddClient(obs, log); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		gate.release()
		_ = client.Close()
		_ = handler.Close()
		_ = obs.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	return &raceRig{
		t: t, hub: hb, fleet: fleet, gate: gate, handler: handler,
		client: client, reader: bufio.NewReader(client), codec: codec, obs: obs,
	}
}

func (r *raceRig) sendReq(command string, m godap.RequestMessage) int {
	r.t.Helper()
	r.seq++
	req := m.GetRequest()
	req.Seq = r.seq
	req.Type = "request"
	req.Command = command
	if err := godap.WriteProtocolMessage(r.client, m); err != nil {
		r.t.Fatalf("write %s: %v", command, err)
	}
	return r.seq
}

func (r *raceRig) recvUntil(want string) godap.Message {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_ = r.client.SetReadDeadline(time.Now().Add(3 * time.Second))
		data, err := godap.ReadBaseMessage(r.reader)
		if err != nil {
			r.t.Fatalf("recv: %v", err)
		}
		m, err := r.codec.DecodeMessage(data)
		if err != nil {
			continue
		}
		switch typed := m.(type) {
		case *godap.SetBreakpointsResponse:
			if want == "setBreakpoints" {
				return typed
			}
		case *godap.InitializeResponse:
			if want == "initialize" {
				return typed
			}
		case *godap.InitializedEvent:
			if want == "initialized" {
				return typed
			}
		case *godap.ConfigurationDoneResponse:
			if want == "configurationDone" {
				return typed
			}
		}
	}
	r.t.Fatalf("timed out waiting for %s", want)
	return nil
}

// setBreakpoints issues a replace-all setBreakpoints for main.go and waits for
// the response (the handler answers only once every operation it owns settled).
func (r *raceRig) setBreakpoints(lines ...int) *godap.SetBreakpointsResponse {
	r.t.Helper()
	bps := make([]godap.SourceBreakpoint, 0, len(lines))
	for _, l := range lines {
		bps = append(bps, godap.SourceBreakpoint{Line: l})
	}
	r.sendReq("setBreakpoints", &godap.SetBreakpointsRequest{
		Arguments: godap.SetBreakpointsArguments{
			Source:      godap.Source{Path: "main.go"},
			Breakpoints: bps,
		},
	})
	return r.recvUntil("setBreakpoints").(*godap.SetBreakpointsResponse)
}

// setBreakpointsAsync issues a setBreakpoints from its own goroutine, which is
// what lets a test hold that request's command at the gate while driving the
// session from a second client.
//
// Joining the returned channel is not optional: setBreakpoints reports failures
// through t, and doing that once the test has returned panics the whole package
// run. The registered cleanup releases the gate before waiting so every exit
// path joins — including a t.Fatalf on the test goroutine, which would
// otherwise leave this one parked at the gate forever.
func (r *raceRig) setBreakpointsAsync(lines ...int) <-chan struct{} {
	r.t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.setBreakpoints(lines...)
	}()
	r.t.Cleanup(func() {
		r.gate.release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			r.t.Error("the held setBreakpoints goroutine never finished")
		}
	})
	return done
}

// handshake runs initialize -> launch(stopOnEntry) -> initialized ->
// configurationDone against the real hub, leaving the session suspended at the
// entry stop (where the hub still drains ordinary commands).
func (r *raceRig) handshake() {
	r.t.Helper()
	r.sendReq("initialize", initArgs())
	r.recvUntil("initialize")
	r.sendReq("launch", &godap.LaunchRequest{
		Arguments: json.RawMessage(`{"program":"/bin/x","stopOnEntry":true}`),
	})
	r.recvUntil("initialized")
}

// installedID returns the bingo breakpoint id the handler believes is armed at
// main.go:line, or 0.
func (r *raceRig) installedID(line int) int {
	r.handler.mu.Lock()
	defer r.handler.mu.Unlock()
	for _, lines := range r.handler.bpByFile {
		if st, ok := lines[line]; ok {
			return st.installedID
		}
	}
	return 0
}

// prepareGap installs A(main.go:10) and B(main.go:20) behind a throwaway
// breakpoint so their physical ids are 2 and 3 — leaving the compaction gap a
// relaunch needs for a stale physical id to alias the wrong breakpoint.
func (r *raceRig) prepareGap() (logicalA, logicalB int) {
	r.t.Helper()
	r.setBreakpoints(5, 10, 20)
	r.setBreakpoints(10, 20)

	first := r.fleet.at(r.t, 0)
	if got := first.physicalIDFor(10); got != 2 {
		r.t.Fatalf("A should hold physical id 2 in the first engine, got %d", got)
	}
	if got := first.physicalIDFor(20); got != 3 {
		r.t.Fatalf("B should hold physical id 3 in the first engine, got %d", got)
	}
	logicalA, logicalB = r.installedID(10), r.installedID(20)
	if logicalA == 0 || logicalB == 0 {
		r.t.Fatalf("handler lost track of the breakpoints (A=%d B=%d)", logicalA, logicalB)
	}
	return logicalA, logicalB
}

// TestStaleClearAcrossRestartHitsTheRightBreakpoint is the permanent
// regression for issue #200: a clear generated before a restart, injected
// after it, must still disarm the breakpoint it named.
func TestStaleClearAcrossRestartHitsTheRightBreakpoint(t *testing.T) {
	rig := newRaceRig(t)
	rig.handshake()

	logicalA, logicalB := rig.prepareGap()
	if logicalA == logicalB {
		t.Fatal("A and B must have distinct ids")
	}

	// Ask the handler to remove A. Its ClearBreakpoint is held at the
	// ReadMessage -> readPump boundary, before the hub ever sees it.
	rig.gate.arm()
	held := rig.setBreakpointsAsync(20)
	rig.gate.waitHeld(t)

	// A second client restarts the session. The replacement engine allocates
	// physical ids from 1, so A's old physical id (2) now names B.
	rig.obs.send(t, protocol.CmdRestart, protocol.RestartPayload{})
	var restarted protocol.RestartedPayload
	rig.obs.waitEvent(t, protocol.EventRestarted, &restarted)

	fresh := rig.fleet.at(t, 1)
	if got := fresh.physicalIDFor(10); got != 1 {
		t.Fatalf("fresh engine should have compacted A to physical id 1, got %d", got)
	}
	if got := fresh.physicalIDFor(20); got != 2 {
		t.Fatalf("fresh engine should have compacted B to physical id 2, got %d", got)
	}

	// Logical identity survives the relaunch, so the client's ids stay valid.
	gotIDs := make([]int, 0, len(restarted.Breakpoints))
	for _, bp := range restarted.Breakpoints {
		gotIDs = append(gotIDs, bp.ID)
	}
	sort.Ints(gotIDs)
	want := []int{logicalA, logicalB}
	sort.Ints(want)
	if len(gotIDs) != 2 || gotIDs[0] != want[0] || gotIDs[1] != want[1] {
		t.Fatalf("restart must preserve logical ids: want %v, got %v", want, gotIDs)
	}

	rig.gate.release()
	<-held

	// The delayed clear must land on A in the *new* process.
	deadline := time.Now().Add(3 * time.Second)
	var lines []int
	for time.Now().Before(deadline) {
		lines = fresh.armedLines()
		if len(lines) == 1 && lines[0] == 20 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(lines) != 1 || lines[0] != 20 {
		t.Fatalf("stale clear disarmed the wrong breakpoint: armed lines %v (want [20])", lines)
	}

	// The handler's view must agree with the physical effect.
	if id := rig.installedID(10); id != 0 {
		t.Fatalf("handler still believes A is installed (id %d)", id)
	}
	if id := rig.installedID(20); id != logicalB {
		t.Fatalf("handler lost B's stable id: want %d, got %d", logicalB, id)
	}
}

// TestClearBeforeRestartIsUnaffected is the in-order negative control: with no
// gating, the same sequence must behave normally.
func TestClearBeforeRestartIsUnaffected(t *testing.T) {
	rig := newRaceRig(t)
	rig.handshake()

	_, logicalB := rig.prepareGap()

	rig.setBreakpoints(20)

	first := rig.fleet.at(t, 0)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lines := first.armedLines(); len(lines) == 1 && lines[0] == 20 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lines := first.armedLines(); len(lines) != 1 || lines[0] != 20 {
		t.Fatalf("in-order clear should leave only B armed, got %v", lines)
	}

	rig.obs.send(t, protocol.CmdRestart, protocol.RestartPayload{})
	var restarted protocol.RestartedPayload
	rig.obs.waitEvent(t, protocol.EventRestarted, &restarted)

	if len(restarted.Breakpoints) != 1 || restarted.Breakpoints[0].ID != logicalB {
		t.Fatalf("restart should reinstall only B with its stable id %d, got %+v",
			logicalB, restarted.Breakpoints)
	}
	fresh := rig.fleet.at(t, 1)
	if lines := fresh.armedLines(); len(lines) != 1 || lines[0] != 20 {
		t.Fatalf("fresh engine should hold only B, got %v", lines)
	}
}

// TestWebSocketAndDAPSeeTheSameIDs pins that both client protocols on one
// session are handed the same stable identities.
func TestWebSocketAndDAPSeeTheSameIDs(t *testing.T) {
	rig := newRaceRig(t)
	rig.handshake()

	logicalA, logicalB := rig.prepareGap()

	// The WebSocket observer saw the same ids the DAP handler recorded.
	ids := rig.obs.setIDs(t)
	if len(ids) < 3 {
		t.Fatalf("observer should have seen three BreakpointSet events, got %v", ids)
	}
	if ids[1] != logicalA || ids[2] != logicalB {
		t.Fatalf("WebSocket ids %v disagree with DAP ids (A=%d B=%d)", ids, logicalA, logicalB)
	}

	// Restart so the fresh engine's physical ids no longer coincide with the
	// logical ones — only then does a missing translation actually show.
	rig.obs.send(t, protocol.CmdRestart, protocol.RestartPayload{})
	rig.obs.waitEvent(t, protocol.EventRestarted, &protocol.RestartedPayload{})

	fresh := rig.fleet.at(t, 1)
	physicalA := fresh.physicalIDFor(10)
	if physicalA == logicalA {
		t.Fatalf("test is blind: physical and logical ids for A both %d", physicalA)
	}

	// A breakpoint hit reported by the engine with a physical id must reach
	// every client as the logical id.
	fresh.events <- protocol.MustEvent(protocol.EventBreakpointHit, 0, protocol.BreakpointHitPayload{
		Breakpoint: protocol.Breakpoint{
			ID:       physicalA,
			Location: protocol.Location{File: "main.go", Line: 10},
		},
	})

	var hit protocol.BreakpointHitPayload
	rig.obs.waitEvent(t, protocol.EventBreakpointHit, &hit)
	if hit.Breakpoint.ID != logicalA {
		t.Fatalf("BreakpointHit must carry the logical id %d, got %d", logicalA, hit.Breakpoint.ID)
	}
	if hit.Breakpoint.Location.Line != 10 {
		t.Fatalf("BreakpointHit location must survive re-encoding, got %+v", hit.Breakpoint.Location)
	}
}
