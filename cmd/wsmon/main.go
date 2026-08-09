// Command wsmon joins an existing bingo WebSocket session and renders the
// streaming goroutine/thread telemetry in a terminal.
//
//	wsmon -session id [-addr host:port] [-once] [-timeout d]
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bingosuite/bingo/pkg/client"
	"github.com/bingosuite/bingo/pkg/protocol"
)

type monitor struct {
	sessionID string
	state     protocol.SessionState
	clients   int

	lastEvent  string
	lastUpdate time.Time

	snapshot    protocol.GoroutineSnapshotPayload
	hasSnapshot bool
}

func main() {
	addr := flag.String("addr", "localhost:6060", "server address (host:port)")
	sessionID := flag.String("session", "", "session ID to join")
	once := flag.Bool("once", false, "print one snapshot then exit")
	timeout := flag.Duration("timeout", 30*time.Second,
		"with -once, how long to wait for a snapshot (0 waits forever)")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "usage: wsmon -session <id> [-addr host:port] [-once] [-timeout d]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if strings.TrimSpace(*sessionID) == "" {
		fmt.Fprintln(os.Stderr, "error: -session is required")
		flag.Usage()
		os.Exit(2)
	}

	fmt.Printf("joining session %s on %s...\n", *sessionID, *addr)
	c, err := client.Join(*addr, *sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: join session %s on %s: %v\n", *sessionID, *addr, err)
		os.Exit(1)
	}
	defer func() { _ = c.Close() }()

	m := &monitor{
		sessionID:  c.SessionID(),
		state:      c.State(),
		clients:    -1,
		lastEvent:  "joined",
		lastUpdate: time.Now(),
	}
	fmt.Printf("joined — session %s (state: %s)\n", m.sessionID, m.state)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		_ = c.Close()
	}()

	// The snapshot answers on the event stream like every automatic push, so
	// this is fire-and-forget; the render loop below applies whichever arrives
	// first. -once therefore waits for the first snapshot event either way.
	if err := c.RequestGoroutineSnapshot(); err != nil {
		fmt.Fprintf(os.Stderr, "error: request goroutine snapshot: %v\n", err)
		os.Exit(1)
	}
	if !*once {
		m.render()
	}

	if err := m.run(c.Events(), *once, *timeout, m.render); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if !*once {
		fmt.Println("\nconnection closed; monitor stopped")
	}
}

// run drains events until the stream closes, -once has rendered its snapshot,
// or (under -once) the deadline expires. render is injected so tests can count
// redraws without terminal output.
//
// A snapshot rejection is ADVISORY, never fatal on its own: EventError is
// broadcast, so an error naming CmdGoroutineSnapshot may belong to another
// client entirely, and a valid snapshot can still arrive right after it (the
// target reaching a stop pushes one unprompted). Attributing it to our request
// would reintroduce exactly the correlate-by-kind mistake this command was
// changed to avoid. So a later snapshot always wins; the last rejection seen is
// only reported as context if the wait ends without one. The deadline is what
// keeps -once bounded when our own request really was rejected.
func (m *monitor) run(events <-chan protocol.Event, once bool, timeout time.Duration, render func()) error {
	var deadline <-chan time.Time
	if once && timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}

	var lastRejection string
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				if once {
					return fmt.Errorf("connection closed before a snapshot arrived%s",
						rejectionContext(lastRejection))
				}
				return nil
			}

			if msg, rejected := snapshotRejection(evt); rejected {
				lastRejection = msg
			}

			renderedSnapshot, redraw := m.applyEvent(evt)
			if !redraw {
				continue
			}
			if once && !renderedSnapshot {
				continue
			}
			render()
			if once && renderedSnapshot {
				return nil
			}

		case <-deadline:
			return fmt.Errorf("no snapshot within %s%s", timeout, rejectionContext(lastRejection))
		}
	}
}

// snapshotRejection reports a failed CmdGoroutineSnapshot seen on the stream.
// EventError carries no requester, so this says only "some snapshot request was
// rejected" — it can never be attributed to this client's request. Callers must
// treat it as advisory context, not as an answer.
func snapshotRejection(evt protocol.Event) (string, bool) {
	if evt.Kind != protocol.EventError {
		return "", false
	}
	var p protocol.ErrorPayload
	if protocol.DecodeEventPayload(evt, &p) != nil || p.Command != protocol.CmdGoroutineSnapshot {
		return "", false
	}
	return p.Message, true
}

func rejectionContext(msg string) string {
	if msg == "" {
		return ""
	}
	return fmt.Sprintf(" (a snapshot request was rejected meanwhile: %s)", msg)
}

func (m *monitor) applyEvent(evt protocol.Event) (bool, bool) {
	switch evt.Kind {
	case protocol.EventGoroutineSnapshot:
		var p protocol.GoroutineSnapshotPayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return false, false
		}
		m.applySnapshot(p)
		return true, true

	case protocol.EventSessionState:
		var p protocol.SessionStatePayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return false, false
		}
		if p.SessionID != "" {
			m.sessionID = p.SessionID
		}
		m.state = p.State
		m.clients = p.Clients
		m.lastUpdate = time.Now()
		return false, true

	default:
		last, ok := describeEvent(evt)
		if !ok {
			return false, false
		}
		m.lastEvent = last
		m.lastUpdate = time.Now()
		return false, true
	}
}

func (m *monitor) applySnapshot(snap protocol.GoroutineSnapshotPayload) {
	m.snapshot = snap
	m.hasSnapshot = true
	m.lastUpdate = time.Now()
}

func describeEvent(evt protocol.Event) (string, bool) {
	switch evt.Kind {
	case protocol.EventBreakpointHit:
		var p protocol.BreakpointHitPayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return "", false
		}
		return fmt.Sprintf("BreakpointHit #%d at %s", p.Breakpoint.ID, locString(p.Breakpoint.Location)), true

	case protocol.EventPaused:
		var p protocol.PausedPayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return "", false
		}
		return fmt.Sprintf("Paused at %s", locString(p.Location)), true

	case protocol.EventStepped:
		var p protocol.SteppedPayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return "", false
		}
		return fmt.Sprintf("Stepped at %s", locString(p.Location)), true

	case protocol.EventProcessExited:
		var p protocol.ProcessExitedPayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return "", false
		}
		reason := ""
		if p.Reason != "" {
			reason = " reason=" + p.Reason
		}
		return fmt.Sprintf("exited code=%d%s", p.ExitCode, reason), true

	case protocol.EventPanic:
		var p protocol.PanicPayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return "", false
		}
		return fmt.Sprintf("Panic %q at %s", oneLine(p.Message), locString(p.Goroutine.CurrentLoc)), true

	case protocol.EventOutput:
		var p protocol.OutputPayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return "", false
		}
		return fmt.Sprintf("Output %s: %s", p.Stream, oneLine(p.Content)), true

	case protocol.EventError:
		var p protocol.ErrorPayload
		if protocol.DecodeEventPayload(evt, &p) != nil {
			return "", false
		}
		if p.Command == protocol.CmdNone {
			return fmt.Sprintf("Error: %s", oneLine(p.Message)), true
		}
		return fmt.Sprintf("Error %s: %s", p.Command, oneLine(p.Message)), true

	default:
		return "", false
	}
}

func (m *monitor) render() {
	fmt.Print("\033[H\033[2J")
	fmt.Printf("bingo telemetry monitor — session %s — state %s", m.sessionID, m.state)
	if m.clients >= 0 {
		fmt.Printf(" — clients %d", m.clients)
	}
	fmt.Println()
	fmt.Printf("updated: %s\n", m.lastUpdate.Format(time.RFC3339))
	fmt.Printf("last: %s\n\n", m.lastEvent)

	fmt.Println("Goroutine spawn tree")
	if !m.hasSnapshot {
		fmt.Println("  (waiting for GoroutineSnapshot)")
	} else {
		renderGoroutineTree(m.snapshot.Goroutines)
	}

	fmt.Println()
	fmt.Println("OS threads")
	if !m.hasSnapshot {
		fmt.Println("  (waiting for GoroutineSnapshot)")
	} else {
		renderThreads(m.snapshot.Threads)
	}

	fmt.Println()
	fmt.Println("Lifecycle deltas")
	if !m.hasSnapshot {
		fmt.Println("  created: -  exited: -")
	} else {
		fmt.Printf("  created: %s  exited: %s\n", formatIDs(m.snapshot.Created), formatIDs(m.snapshot.Exited))
	}

	fmt.Println()
	fmt.Printf("counts: goroutines=%d threads=%d\n", len(m.snapshot.Goroutines), len(m.snapshot.Threads))
}

func renderGoroutineTree(goroutines []protocol.Goroutine) {
	if len(goroutines) == 0 {
		fmt.Println("  (none)")
		return
	}

	nodes := make(map[int]protocol.Goroutine, len(goroutines))
	for _, g := range goroutines {
		nodes[g.ID] = g
	}

	children := make(map[int][]int, len(goroutines))
	roots := make(map[int]bool, len(goroutines))
	for _, g := range goroutines {
		if g.ParentID == 0 || g.ParentID == g.ID {
			roots[g.ID] = true
			continue
		}
		if _, ok := nodes[g.ParentID]; !ok {
			roots[g.ID] = true
			continue
		}
		children[g.ParentID] = append(children[g.ParentID], g.ID)
	}

	visited := make(map[int]bool, len(nodes))
	rootIDs := sortedKeys(roots)
	for i, id := range rootIDs {
		renderTreeNode(id, "", i == len(rootIDs)-1, nodes, children, visited)
	}

	extras := unvisitedIDs(nodes, visited)
	for i, id := range extras {
		renderTreeNode(id, "", i == len(extras)-1, nodes, children, visited)
	}
}

func renderTreeNode(
	id int,
	prefix string,
	last bool,
	nodes map[int]protocol.Goroutine,
	children map[int][]int,
	visited map[int]bool,
) {
	connector, childPrefix := "├─", prefix+"│ "
	if last {
		connector, childPrefix = "└─", prefix+"  "
	}

	if visited[id] {
		fmt.Printf("  %s%s G%d (cycle)\n", prefix, connector, id)
		return
	}

	g, ok := nodes[id]
	if !ok {
		return
	}
	visited[id] = true
	fmt.Printf("  %s%s %s\n", prefix, connector, goroutineLine(g))

	childIDs := sortedInts(children[id])
	for i, childID := range childIDs {
		renderTreeNode(childID, childPrefix, i == len(childIDs)-1, nodes, children, visited)
	}
}

func renderThreads(threads []protocol.Thread) {
	if len(threads) == 0 {
		fmt.Println("  (none)")
		return
	}

	sorted := append([]protocol.Thread(nil), threads...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ID == sorted[j].ID {
			return sorted[i].MID < sorted[j].MID
		}
		return sorted[i].ID < sorted[j].ID
	})

	for _, t := range sorted {
		marker := " "
		if t.Current {
			marker = "*"
		}
		spin := ""
		if t.Spinning {
			spin = " [spinning]"
		}
		fmt.Printf("  %s T%d mid=%d goid=%d%s %s\n",
			marker, t.ID, t.MID, t.GoID, spin, locString(t.CurrentLoc))
	}
}

func goroutineLine(g protocol.Goroutine) string {
	marker := " "
	if g.Current {
		marker = "*"
	}

	status := g.Status
	if status == "" {
		status = "unknown"
	}
	if g.WaitReason != "" {
		status += " (" + g.WaitReason + ")"
	}

	parts := []string{fmt.Sprintf("%s G%d", marker, g.ID), status}
	if g.CurrentLoc.Function != "" {
		parts = append(parts, g.CurrentLoc.Function)
	}
	parts = append(parts, locString(g.CurrentLoc))
	if g.StartLoc.Function != "" {
		parts = append(parts, "start="+g.StartLoc.Function)
	}
	if g.ThreadID != 0 {
		parts = append(parts, fmt.Sprintf("thr=%d", g.ThreadID))
	}
	return strings.Join(parts, " ")
}

func locString(loc protocol.Location) string {
	if loc.File != "" {
		base := filepath.Base(loc.File)
		if loc.Line > 0 {
			return fmt.Sprintf("%s:%d", base, loc.Line)
		}
		return base
	}
	if loc.Function != "" {
		return loc.Function
	}
	return "-"
}

func formatIDs(ids []int) string {
	if len(ids) == 0 {
		return "-"
	}
	return fmt.Sprintf("%v", sortedInts(ids))
}

func sortedKeys(values map[int]bool) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func sortedInts(values []int) []int {
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	return sorted
}

func unvisitedIDs(nodes map[int]protocol.Goroutine, visited map[int]bool) []int {
	ids := make([]int, 0, len(nodes))
	for id := range nodes {
		if !visited[id] {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", `\n`))
	if len(s) > 120 {
		return s[:117] + "..."
	}
	if s == "" {
		return "-"
	}
	return s
}
