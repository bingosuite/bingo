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

	// The tombstone record for breakpoints this engine has removed, kept in
	// BOTH directions:
	//
	//   - retiredLogical (physical→logical) lets an event the engine had
	//     already queued when the removal ran still report the identity the
	//     client held, rather than a number it was never told about.
	//   - retiredPhysical (logical→physical) keeps that reported id CLEARABLE.
	//     Clearing the breakpoint the process is currently parked on is undone
	//     by the engine's step-off: resumeFromBreakpoint replays the entry
	//     pointer it stashed at hit time, and reinstall re-adds it under the
	//     SAME physical id. Without this direction the resurrected trap would
	//     be reported under a logical id the hub had already forgotten, so no
	//     client could ever remove it again — and the location could not be
	//     re-set either, since the engine still has it armed.
	//
	// Physical ids are monotonic within one engine (breakpointTable.nextID
	// only ever increments), and reset() drops the whole record whenever the
	// engine is replaced, so a tombstone can only ever name the breakpoint it
	// was created for — never a later one, and never one in another generation.
	retiredLogical  map[int]int
	retiredPhysical map[int]int
	retiredOrder    []int
}

func newBreakpointIDs() *breakpointIDs {
	return &breakpointIDs{
		byLogical:       make(map[int]*breakpointMapping),
		byPhysical:      make(map[int]int),
		retiredLogical:  make(map[int]int),
		retiredPhysical: make(map[int]int),
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

// retire records a removed breakpoint's physical id in both directions,
// evicting the oldest entry once the record is full.
func (b *breakpointIDs) retire(physicalID, logical int) {
	if _, dup := b.retiredLogical[physicalID]; dup {
		return
	}
	if len(b.retiredOrder) >= retiredCap {
		oldest := b.retiredOrder[0]
		if staleLogical, ok := b.retiredLogical[oldest]; ok {
			delete(b.retiredPhysical, staleLogical)
		}
		delete(b.retiredLogical, oldest)
		b.retiredOrder = b.retiredOrder[1:]
	}
	b.retiredLogical[physicalID] = logical
	b.retiredPhysical[logical] = physicalID
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
	if logical, ok := b.retiredLogical[physicalID]; ok {
		return logical
	}
	return b.adopt(physicalID, loc)
}

// physicalForClear resolves a logical id to the physical id the active engine
// knows it by.
//
// A retired id still resolves, because a removal the hub confirmed can be undone
// by the engine: stepping off the breakpoint the process is parked on reinstalls
// it under the same physical id. Refusing the retired id would strand that
// resurrected trap — unremovable (the hub forgot the mapping) and unsettable
// (the engine still has the address armed). Resolving it instead is safe because
// a physical id is never reused within an engine and the record dies with the
// engine.
func (b *breakpointIDs) physicalForClear(logical int) (int, bool) {
	if m, ok := b.byLogical[logical]; ok {
		return m.physicalID, true
	}
	physicalID, ok := b.retiredPhysical[logical]
	return physicalID, ok
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
// where it is. Called on a fresh Launch/Attach and by Restart before it re-homes
// the surviving identities: both replace the engine, and every physical id —
// live or retired — becomes meaningless the moment that happens.
func (b *breakpointIDs) reset() {
	b.byLogical = make(map[int]*breakpointMapping)
	b.byPhysical = make(map[int]int)
	b.retiredLogical = make(map[int]int)
	b.retiredPhysical = make(map[int]int)
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
	physicalID, ok := h.bps.physicalForClear(logicalID)
	if !ok {
		return dispatchResult{}, fmt.Errorf("breakpoint %d not found", logicalID)
	}
	if err := dbg.ClearBreakpoint(physicalID); err != nil {
		// Retain the mapping: a failed clear leaves the trap armed (see
		// breakpointTable.clear), so the client must keep being able to name it.
		return dispatchResult{}, err
	}
	// A no-op when the id was already retired, which is what makes clearing a
	// step-off-resurrected breakpoint repeatable: the tombstone survives so the
	// next resurrection is still reported — and removable — under the same id.
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
