package hub

import (
	"fmt"
	"sort"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// breakpointMapping is one logical breakpoint's record: the id the *currently
// active* engine knows it by, plus the source location Restart reinstalls it
// from.
type breakpointMapping struct {
	physicalID int
	loc        protocol.Location

	// installed marks a breakpoint the hub itself put in place, and is what
	// makes it a Restart target. An adopted mapping (see logicalFor) only names
	// something the hub observed; reinstalling it would arm a trap on the new
	// process that no client ever asked for.
	installed bool
}

// retiredCap bounds the per-engine record of removed breakpoints. Only a very
// recent retirement can still be named by an in-flight event, so evicting the
// oldest entries costs nothing while keeping a set/clear-churning session from
// growing this without limit.
const retiredCap = 1024

// breakpointIDs owns the translation between the session-stable logical ids
// clients hold and the per-engine physical ids a debugger allocates.
//
// Every engine restarts its allocation at 1 (internal/debugger/breakpoint.go),
// so a physical id only means anything to the engine that issued it. A command
// can still be *generated* against one engine and *injected* after that engine
// has been replaced: the DAP adapter marshals a ClearBreakpoint while its hub
// read pump is descheduled, another client completes a Restart in the meantime,
// and the stale id then names a different breakpoint in the fresh process.
// Passing raw engine ids through the protocol therefore lets a delayed clear
// disarm the wrong trap (issue #200). Owning the identity here means such a
// command either resolves to the same breakpoint in the replacement process or
// is rejected outright.
type breakpointIDs struct {
	// next is monotonic for the whole hub lifetime and is deliberately NOT
	// reset by a later Launch/Attach. Re-minting a logical id is what would let
	// a delayed command from a previous target alias a breakpoint in the new
	// one — exactly the aliasing this type exists to prevent.
	next int

	byLogical  map[int]*breakpointMapping
	byPhysical map[int]int

	// retired remembers physical ids this engine's breakpoints were removed
	// under, so an event the engine had already queued when the removal ran
	// still reports the identity the client held. Engine ids are never reused
	// within one engine, and reset() drops the whole record with the engine.
	retired      map[int]int
	retiredOrder []int
}

func newBreakpointIDs() *breakpointIDs {
	return &breakpointIDs{
		byLogical:  make(map[int]*breakpointMapping),
		byPhysical: make(map[int]int),
		retired:    make(map[int]int),
	}
}

// track mints a logical id for a freshly installed physical breakpoint.
func (b *breakpointIDs) track(physicalID int, loc protocol.Location) int {
	b.next++
	logical := b.next
	b.bind(logical, physicalID, loc)
	return logical
}

// adopt mints a logical id for a physical breakpoint the hub did not install,
// so it can be reported without leaking (or colliding with) an engine id. The
// mapping is deliberately not a Restart target.
func (b *breakpointIDs) adopt(physicalID int, loc protocol.Location) int {
	logical := b.track(physicalID, loc)
	b.byLogical[logical].installed = false
	return logical
}

// bind points an existing logical id at the physical id the active engine gave
// it. Restart uses this to re-home every surviving breakpoint onto the
// replacement engine without changing the identity clients hold.
func (b *breakpointIDs) bind(logical, physicalID int, loc protocol.Location) {
	if prev, ok := b.byLogical[logical]; ok {
		b.dropPhysical(prev.physicalID, logical)
	}
	b.byLogical[logical] = &breakpointMapping{physicalID: physicalID, loc: loc, installed: true}
	b.byPhysical[physicalID] = logical
}

func (b *breakpointIDs) lookup(logical int) (*breakpointMapping, bool) {
	m, ok := b.byLogical[logical]
	return m, ok
}

// untrack forgets a logical breakpoint. Called only once a removal is
// confirmed: a rejected clear leaves the trap armed, so dropping the mapping
// optimistically would make that breakpoint permanently unremovable.
func (b *breakpointIDs) untrack(logical int) {
	m, ok := b.byLogical[logical]
	if !ok {
		return
	}
	delete(b.byLogical, logical)
	b.dropPhysical(m.physicalID, logical)
	b.retire(m.physicalID, logical)
}

// retire records a removed breakpoint's physical id, evicting the oldest entry
// once the record is full.
func (b *breakpointIDs) retire(physicalID, logical int) {
	if _, dup := b.retired[physicalID]; dup {
		return
	}
	if len(b.retiredOrder) >= retiredCap {
		delete(b.retired, b.retiredOrder[0])
		b.retiredOrder = b.retiredOrder[1:]
	}
	b.retired[physicalID] = logical
	b.retiredOrder = append(b.retiredOrder, physicalID)
}

// dropPhysical removes a reverse entry only while it still points at logical,
// so re-homing a mapping cannot delete another breakpoint's reverse lookup.
func (b *breakpointIDs) dropPhysical(physicalID, logical int) {
	if cur, ok := b.byPhysical[physicalID]; ok && cur == logical {
		delete(b.byPhysical, physicalID)
	}
}

// logicalFor resolves an engine-reported physical id to the client-visible
// logical id.
//
// A hit the engine had already queued when the matching clear ran resolves
// through the retirement record, so the client sees the id it actually held
// rather than one it was never told about. Anything still unknown (a raw hub
// whose debugger was driven directly) is adopted rather than passed through:
// passing it through would let it collide with a logical id that names a
// different breakpoint.
func (b *breakpointIDs) logicalFor(physicalID int, loc protocol.Location) int {
	if logical, ok := b.byPhysical[physicalID]; ok {
		return logical
	}
	if logical, ok := b.retired[physicalID]; ok {
		return logical
	}
	return b.adopt(physicalID, loc)
}

// installedLogical returns the logical ids the hub itself installed, ascending
// so Restart reinstalls in a deterministic sequence across runs. Adopted
// mappings are excluded — the hub never armed them, so it must not re-arm them
// on the replacement process.
func (b *breakpointIDs) installedLogical() []int {
	ids := make([]int, 0, len(b.byLogical))
	for id, m := range b.byLogical {
		if !m.installed {
			continue
		}
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// reset drops every mapping for a target that no longer exists, keeping next
// where it is. Called on a fresh Launch/Attach, never on Restart — Restart's
// whole point is that the logical identities survive.
func (b *breakpointIDs) reset() {
	b.byLogical = make(map[int]*breakpointMapping)
	b.byPhysical = make(map[int]int)
	b.retired = make(map[int]int)
	b.retiredOrder = nil
}

// setBreakpoint installs a breakpoint and reports it under a newly minted
// logical id. The engine's physical id never leaves the hub.
func (h *Hub) setBreakpoint(dbg debugger.Debugger, cmd protocol.Command) (dispatchResult, error) {
	var p protocol.SetBreakpointPayload
	if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
		return dispatchResult{}, err
	}
	bp, err := dbg.SetBreakpoint(p.File, p.Line)
	if err != nil {
		return dispatchResult{}, err
	}
	bp.ID = h.bps.track(bp.ID, bp.Location)
	evt, err := protocol.NewEvent(protocol.EventBreakpointSet, 0, protocol.BreakpointSetPayload{
		Breakpoint: bp,
	})
	if err != nil {
		return dispatchResult{}, err
	}
	return dispatchResult{event: &evt}, nil
}

// clearBreakpoint translates a logical id to the active engine's physical id
// and removes it.
//
// An id the hub does not currently know is rejected without touching the
// debugger. That is the load-bearing half of the fix: a clear generated against
// a replaced or relaunched target names a breakpoint that no longer exists, and
// forwarding it would remove whichever breakpoint the fresh engine happened to
// give that number.
func (h *Hub) clearBreakpoint(dbg debugger.Debugger, cmd protocol.Command) (dispatchResult, error) {
	var p protocol.ClearBreakpointPayload
	if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
		return dispatchResult{}, err
	}
	logicalID := p.ID
	m, ok := h.bps.lookup(logicalID)
	if !ok {
		return dispatchResult{}, fmt.Errorf("breakpoint %d not found", logicalID)
	}
	if err := dbg.ClearBreakpoint(m.physicalID); err != nil {
		// Retain the mapping: a failed clear leaves the trap armed (see
		// breakpointTable.clear), so the client must keep being able to name it.
		return dispatchResult{}, err
	}
	h.bps.untrack(logicalID)
	evt, err := protocol.NewEvent(protocol.EventBreakpointCleared, 0, protocol.BreakpointClearedPayload{
		ID: logicalID,
	})
	if err != nil {
		return dispatchResult{}, err
	}
	return dispatchResult{event: &evt}, nil
}

// localizeBreakpointIDs rewrites engine-owned physical breakpoint ids in a
// debugger event to the hub's logical ids before it reaches clients.
// EventBreakpointHit is the only engine-generated event carrying one; the
// internal step-over/step-out sentinels report EventStepped and never surface a
// breakpoint id at all.
func (h *Hub) localizeBreakpointIDs(evt protocol.Event) protocol.Event {
	if evt.Kind != protocol.EventBreakpointHit {
		return evt
	}
	var p protocol.BreakpointHitPayload
	if err := protocol.DecodeEventPayload(evt, &p); err != nil {
		h.log.Warn("undecodable BreakpointHit payload — forwarding unchanged", "err", err)
		return evt
	}
	p.Breakpoint.ID = h.bps.logicalFor(p.Breakpoint.ID, p.Breakpoint.Location)
	localized, err := protocol.NewEvent(evt.Kind, evt.Seq, p)
	if err != nil {
		h.log.Error("failed to re-encode BreakpointHit — forwarding unchanged", "err", err)
		return evt
	}
	return localized
}
