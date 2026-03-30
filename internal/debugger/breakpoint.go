package debugger

import (
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// errBreakpointExists is the sentinel returned by breakpointTable.set when a
// breakpoint is already installed at the requested address.
var errBreakpointExists = errors.New("breakpoint already installed at address")

// breakpointEntry holds everything we need to manage one patched instruction.
type breakpointEntry struct {
	id            int
	addr          uint64
	file          string
	line          int
	originalBytes []byte // instruction bytes overwritten by the trap instruction
	enabled       bool
}

func (b *breakpointEntry) toProtocol() protocol.Breakpoint {
	return protocol.Breakpoint{
		ID:      b.id,
		Enabled: b.enabled,
		Location: protocol.Location{
			File: b.file,
			Line: b.line,
		},
	}
}

// breakpointTable owns the installed breakpoints for one debug session.
// It is not concurrency-safe; the engine's event loop serialises all access.
type breakpointTable struct {
	byID   map[int]*breakpointEntry
	byAddr map[uint64]*breakpointEntry
	nextID atomic.Int64
}

func newBreakpointTable() *breakpointTable {
	return &breakpointTable{
		byID:   make(map[int]*breakpointEntry),
		byAddr: make(map[uint64]*breakpointEntry),
	}
}

// set writes the arch trap instruction at addr in the tracee, saves the
// overwritten bytes, and records the entry. Returns errBreakpointExists
// (wrapped) if a breakpoint is already at that address.
func (t *breakpointTable) set(b Backend, file string, line int, addr uint64) (*breakpointEntry, error) {
	if _, exists := t.byAddr[addr]; exists {
		return nil, fmt.Errorf("%w: 0x%x (%s:%d)", errBreakpointExists, addr, file, line)
	}

	trap := archTrapInstruction()
	orig := make([]byte, len(trap))

	if err := b.ReadMemory(addr, orig); err != nil {
		return nil, fmt.Errorf("breakpoint set: read original bytes at 0x%x: %w", addr, err)
	}
	if err := b.WriteMemory(addr, trap); err != nil {
		return nil, fmt.Errorf("breakpoint set: write trap at 0x%x: %w", addr, err)
	}

	id := int(t.nextID.Add(1))
	entry := &breakpointEntry{
		id:            id,
		addr:          addr,
		file:          file,
		line:          line,
		originalBytes: orig,
		enabled:       true,
	}
	t.byID[id] = entry
	t.byAddr[addr] = entry
	return entry, nil
}

// clear restores the original instruction bytes and removes the entry.
func (t *breakpointTable) clear(b Backend, id int) error {
	entry, ok := t.byID[id]
	if !ok {
		return fmt.Errorf("breakpoint %d not found", id)
	}
	if err := b.WriteMemory(entry.addr, entry.originalBytes); err != nil {
		return fmt.Errorf("breakpoint clear: restore bytes at 0x%x: %w", entry.addr, err)
	}
	delete(t.byID, id)
	delete(t.byAddr, entry.addr)
	return nil
}

// atAddr returns the entry for the given address, or nil if none exists.
func (t *breakpointTable) atAddr(addr uint64) *breakpointEntry {
	return t.byAddr[addr]
}

// all returns protocol representations of all current breakpoints.
func (t *breakpointTable) all() []protocol.Breakpoint {
	out := make([]protocol.Breakpoint, 0, len(t.byID))
	for _, e := range t.byID {
		out = append(out, e.toProtocol())
	}
	return out
}

// clearAll is a best-effort restore of all breakpoints, used during Kill.
// Continues on individual errors so a single bad write doesn't block shutdown.
func (t *breakpointTable) clearAll(b Backend) {
	for id := range t.byID {
		_ = t.clear(b, id)
	}
}
