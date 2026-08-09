package debugger

import (
	"debug/dwarf"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

const callerScopeFixtureSrc = `package main

var observed int

//go:noinline
func perturb(v int) int {
	return v + 1
}

//go:noinline
func callerWithBlock(n int) int {
	if n > 0 {
		result := n + 7
		if n&1 == 0 {
			result += perturb(n)
		}
		callee(result) // caller-call-marker
	}
	return n
}

//go:noinline
func callee(v int) {
	observed = v // callee-marker
}

func main() {
	callerWithBlock(2)
}
`

func TestFrameLookupPC(t *testing.T) {
	if got := frameLookupPC(0x1000, 0); got != 0x1000 {
		t.Fatalf("top frame lookup PC = %#x, want live PC %#x", got, uint64(0x1000))
	}
	if got := frameLookupPC(0x2000, 1); got != 0x1fff {
		t.Fatalf("caller lookup PC = %#x, want call PC %#x", got, uint64(0x1fff))
	}
	if got := frameLookupPC(0, 1); got != 0 {
		t.Fatalf("zero caller PC underflowed to %#x", got)
	}

	discontiguous := [][2]uint64{{0x1000, 0x1010}, {0x2000, 0x2010}}
	if classifyScopePC(0x2010, discontiguous, nil) != scopePCOutside {
		t.Fatal("saved return PC should be outside the exclusive second range")
	}
	if classifyScopePC(frameLookupPC(0x2010, 1), discontiguous, nil) != scopePCInside {
		t.Fatal("adjusted caller PC should be inside the second discontiguous range")
	}
}

func TestCallerFrameUsesCallPCForScopesAndCFI(t *testing.T) {
	r := buildDWARFScopeFixture(t, callerScopeFixtureSrc, false)
	ranges := variableScopeRanges(t, r, "callerWithBlock", "result")
	if len(ranges) == 0 {
		t.Fatal("caller fixture result scope has no ranges")
	}
	returnPC := scopeEndAtMarker(t, r, ranges, callerScopeFixtureSrc, "caller-call-marker")
	calleePC := pcAtMarker(t, r, callerScopeFixtureSrc, "callee-marker")
	r.frame = &frameTable{fdes: []fde{{
		loc: returnPC - 0x10,
		end: returnPC,
		c: &cie{
			codeAlign: 1,
			initInstr: []byte{0x0c, byte(dwarfFPReg()), 0x40}, // DW_CFA_def_cfa fp, 64
		},
	}}}

	if names := entryNameCounts(requiredSubprogramVars(t, r, returnPC)); names["result"] != 0 {
		t.Fatalf("raw return PC unexpectedly includes result: %v", names)
	}
	if names := entryNameCounts(requiredSubprogramVars(t, r, returnPC-1)); names["result"] != 1 {
		t.Fatalf("call PC does not include result: %v", names)
	}

	const (
		calleeSP = uint64(0x700000)
		calleeBP = uint64(0x700100)
		callerBP = uint64(0x700200)
	)
	backend := &callerFrameBackend{
		regs: Registers{PC: calleePC, SP: calleeSP, BP: calleeBP},
		mem:  make(map[uint64]byte),
	}
	backend.seedUint64(calleeBP, callerBP)
	backend.seedUint64(calleeBP+8, returnPC)
	backend.seedUint64(callerBP, 0)
	backend.seedUint64(callerBP+8, 0)

	e := newEngine(backend, nil)
	if err := e.dispatch(func() error {
		e.dw = r
		e.proc.live = true
		e.curTID = 1
		e.setState(stateSuspended)
		return nil
	}); err != nil {
		t.Fatalf("prepare engine: %v", err)
	}
	t.Cleanup(func() {
		_ = e.Kill()
		<-e.done
	})

	var callerPC, callerCFA uint64
	if err := e.dispatch(func() error {
		var frameErr error
		callerPC, callerCFA, frameErr = e.frameLocation(1)
		return frameErr
	}); err != nil {
		t.Fatalf("frameLocation(caller): %v", err)
	}
	if callerPC != returnPC-1 {
		t.Fatalf("caller frame PC = %#x, want call PC %#x", callerPC, returnPC-1)
	}
	if callerCFA != callerBP+0x40 {
		t.Fatalf("caller CFA = %#x, want adjusted-PC CFI result %#x", callerCFA, callerBP+0x40)
	}

	frames, err := e.StackFrames()
	if err != nil {
		t.Fatalf("StackFrames: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("StackFrames returned %d frames, want caller frame", len(frames))
	}
	callLine := markerLine(t, callerScopeFixtureSrc, "caller-call-marker")
	if got := frames[1].Location; !strings.HasSuffix(got.Function, ".callerWithBlock") || got.Line != callLine {
		t.Fatalf("caller frame location = %+v, want callerWithBlock line %d", got, callLine)
	}

	vars, err := e.Locals(1)
	if err != nil {
		t.Fatalf("Locals(caller): %v", err)
	}
	if names := variableNameCounts(vars); names["result"] != 1 {
		t.Fatalf("caller locals omit block result: %v", names)
	}
	result, err := e.Evaluate(1, "result")
	if err != nil {
		t.Fatalf("Evaluate(caller result): %v", err)
	}
	if result.Name != "result" {
		t.Fatalf("Evaluate result = %+v", result)
	}
}

func variableScopeRanges(t *testing.T, r *dwarfReader, function, variable string) [][2]uint64 {
	t.Helper()
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil {
			t.Fatalf("read fixture DWARF: %v", err)
		}
		if entry == nil {
			break
		}
		name, _ := entry.Val(dwarf.AttrName).(string)
		if entry.Tag != dwarf.TagSubprogram || !strings.HasSuffix(name, "."+function) {
			continue
		}
		return variableRangesInSubprogram(t, r, rd, variable)
	}
	t.Fatalf("subprogram %q not found", function)
	return nil
}

func variableRangesInSubprogram(t *testing.T, r *dwarfReader, rd *dwarf.Reader, variable string) [][2]uint64 {
	t.Helper()
	depth := 1
	scopeAtDepth := []*dwarf.Entry{nil, nil}
	for depth > 0 {
		entry := requiredDIE(t, rd, "subprogram variables")
		if entry.Tag == 0 {
			depth--
			continue
		}
		name, _ := entry.Val(dwarf.AttrName).(string)
		if entry.Tag == dwarf.TagVariable && name == variable {
			scope := scopeAtDepth[depth]
			if scope == nil {
				t.Fatalf("variable %q has no lexical scope", variable)
			}
			return requiredRanges(t, r, scope, "variable scope")
		}
		if !entry.Children {
			continue
		}
		parentScope := scopeAtDepth[depth]
		depth++
		if depth >= len(scopeAtDepth) {
			scopeAtDepth = append(scopeAtDepth, nil)
		}
		scopeAtDepth[depth] = parentScope
		if isVariableScope(entry.Tag) {
			scopeAtDepth[depth] = entry
		}
	}
	t.Fatalf("variable %q not found", variable)
	return nil
}

func scopeEndAtMarker(t *testing.T, r *dwarfReader, ranges [][2]uint64, source, marker string) uint64 {
	t.Helper()
	line := markerLine(t, source, marker)
	for _, pcs := range ranges {
		if pcs[1] == 0 || pcs[1] <= pcs[0] {
			continue
		}
		runtimeEnd := uint64(int64(pcs[1]) + r.slide)
		if r.locationForPC(runtimeEnd-1).Line == line {
			return runtimeEnd
		}
	}
	t.Fatalf("no scope range ends at marker %q: %v", marker, ranges)
	return 0
}

func requiredSubprogramVars(t *testing.T, r *dwarfReader, pc uint64) []*dwarf.Entry {
	t.Helper()
	entries, err := r.subprogramVars(pc)
	if err != nil {
		t.Fatalf("subprogramVars(%#x): %v", pc, err)
	}
	return entries
}

func variableNameCounts(vars []protocol.Variable) map[string]int {
	names := make(map[string]int)
	for _, variable := range vars {
		names[variable.Name]++
	}
	return names
}

type callerFrameBackend struct {
	regs Registers
	mem  map[uint64]byte
}

func (b *callerFrameBackend) seedUint64(addr, value uint64) {
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], value)
	for i, byt := range data {
		b.mem[addr+uint64(i)] = byt
	}
}

func (*callerFrameBackend) ContinueProcess() error           { return nil }
func (*callerFrameBackend) SingleStep(int) error             { return nil }
func (*callerFrameBackend) StopProcess() error               { return nil }
func (*callerFrameBackend) PauseSignal() int                 { return 0 }
func (*callerFrameBackend) WriteMemory(uint64, []byte) error { return nil }
func (b *callerFrameBackend) GetRegisters(int) (Registers, error) {
	return b.regs, nil
}
func (*callerFrameBackend) SetRegisters(int, Registers) error { return nil }
func (*callerFrameBackend) Threads() ([]int, error)           { return []int{1}, nil }
func (*callerFrameBackend) Wait() (StopEvent, error)          { return StopEvent{}, ErrProcessExited }

func (b *callerFrameBackend) ReadMemory(addr uint64, dst []byte) error {
	for i := range dst {
		dst[i] = b.mem[addr+uint64(i)]
	}
	return nil
}
