package debugger

import (
	"debug/dwarf"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClassifyScopePC(t *testing.T) {
	tests := []struct {
		name   string
		pc     uint64
		ranges [][2]uint64
		err    error
		want   scopePCMatch
	}{
		{name: "second discontiguous range", pc: 0x205, ranges: [][2]uint64{{0x100, 0x110}, {0x200, 0x210}}, want: scopePCInside},
		{name: "outside all ranges", pc: 0x180, ranges: [][2]uint64{{0x100, 0x110}, {0x200, 0x210}}, want: scopePCOutside},
		{name: "empty ranges are unknown", pc: 0x100, want: scopePCUnknown},
		{name: "range error is unknown", pc: 0x100, err: errors.New("bad ranges"), want: scopePCUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyScopePC(tt.pc, tt.ranges, tt.err); got != tt.want {
				t.Fatalf("classifyScopePC() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVariableScopeTags(t *testing.T) {
	if !isVariableScope(dwarf.TagLexDwarfBlock) {
		t.Error("DW_TAG_lexical_block must define variable scope")
	}
	if !isVariableScope(dwarf.TagInlinedSubroutine) {
		t.Error("DW_TAG_inlined_subroutine must define variable scope")
	}
	if isVariableScope(dwarf.TagSubprogram) {
		t.Error("the containing subprogram is selected before nested scope traversal")
	}
}

const lexicalScopeFixtureSrc = `package main

//go:noinline
func lexicalScopes(n int) int {
	sink := n
	shadow := n + 10
	if n > 0 {
		shadow := n + 20
		if shadow > 0 {
			shadow := n + 30
			sink += shadow // inner-shadow-marker
		}
		sink += shadow // middle-shadow-marker
	}
	sink += shadow // outer-shadow-marker
	if n%2 == 0 {
		left := n + 40
		sink += left // left-marker
	} else {
		right := n + 50
		sink += right // right-marker
	}
	for i := 0; i < 1; i++ {
		loop := n + i
		sink += loop // loop-marker
	}
	sink++ // after-marker
	return sink
}

func main() {
	println(lexicalScopes(1))
}
`

const inlineScopeFixtureSrc = `package main

func inlineAdd(v int) int {
	inlineLocal := v + 1
	if inlineLocal > 2 {
		inlineLocal *= 2
	}
	return inlineLocal
}

//go:noinline
func optimizedScopes(n int) int {
	result := inlineAdd(n) // inline-marker
	result++ // after-inline-marker
	return result
}

func main() {
	println(optimizedScopes(1))
}
`

func TestSubprogramVarsRespectLexicalScopes(t *testing.T) {
	r := buildDWARFScopeFixture(t, lexicalScopeFixtureSrc, false)
	r.slide = 0x12345000

	assertNames := func(marker string, present, absent []string) {
		t.Helper()
		entries := varsAtMarker(t, r, lexicalScopeFixtureSrc, marker)
		names := entryNameCounts(entries)
		for _, name := range present {
			if names[name] != 1 {
				t.Errorf("%s: expected exactly one %q, got names %v", marker, name, names)
			}
		}
		for _, name := range absent {
			if names[name] != 0 {
				t.Errorf("%s: expected %q to be out of scope, got names %v", marker, name, names)
			}
		}
	}

	assertNames("left-marker", []string{"n", "sink", "shadow", "left"}, []string{"right", "i", "loop"})
	assertNames("right-marker", []string{"n", "sink", "shadow", "right"}, []string{"left", "i", "loop"})
	assertNames("loop-marker", []string{"n", "sink", "shadow", "i", "loop"}, []string{"left", "right"})
	assertNames("after-marker", []string{"n", "sink", "shadow"}, []string{"left", "right", "i", "loop"})

	outer := variableAddressAtMarker(t, r, lexicalScopeFixtureSrc, "outer-shadow-marker", "shadow")
	middle := variableAddressAtMarker(t, r, lexicalScopeFixtureSrc, "middle-shadow-marker", "shadow")
	inner := variableAddressAtMarker(t, r, lexicalScopeFixtureSrc, "inner-shadow-marker", "shadow")
	if outer == middle || middle == inner || outer == inner {
		t.Fatalf("shadowed variables must resolve to distinct deepest-active stack slots: outer=%#x middle=%#x inner=%#x", outer, middle, inner)
	}

	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		assertLinuxLexicalRangeForm(t, r)
	}
}

func TestSubprogramVarsPruneInactiveInlineScopes(t *testing.T) {
	r := buildDWARFScopeFixture(t, inlineScopeFixtureSrc, true)
	r.slide = 0x12345000
	pc := pcAtMarker(t, r, inlineScopeFixtureSrc, "after-inline-marker")
	inlineScopes, inactive := inlineScopesAtPC(t, r, pc)
	if inlineScopes == 0 {
		t.Fatal("optimized fixture did not emit a DW_TAG_inlined_subroutine")
	}
	if inactive == 0 {
		t.Fatal("optimized fixture did not emit an inactive inline scope at the post-inline PC")
	}

	entries, err := r.subprogramVars(pc)
	if err != nil {
		t.Fatalf("subprogramVars: %v", err)
	}
	names := entryNameCounts(entries)
	if names[""] != 0 {
		t.Fatalf("inactive abstract-origin variables leaked as nameless locals: %v", names)
	}
	for name, count := range names {
		if count != 1 {
			t.Fatalf("local %q appeared %d times: %v", name, count, names)
		}
	}

	outside, err := r.subprogramVars(0)
	if err != nil {
		t.Fatalf("subprogramVars outside executable ranges: %v", err)
	}
	if len(outside) != 0 {
		t.Fatalf("abstract no-range subprogram used as a frame fallback: %v", entryNameCounts(outside))
	}
}

func buildDWARFScopeFixture(t *testing.T, source string, optimized bool) *dwarfReader {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "scope_fixture.go")
	if err := os.WriteFile(src, []byte(source), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	bin := filepath.Join(dir, "scope_fixture")
	args := []string{"build", "-o", bin}
	if !optimized {
		args = append(args, "-gcflags=all=-N -l")
	}
	args = append(args, src)
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	r, err := openDWARF(bin)
	if err != nil {
		t.Fatalf("open fixture DWARF: %v", err)
	}
	return r
}

func varsAtMarker(t *testing.T, r *dwarfReader, source, marker string) []*dwarf.Entry {
	t.Helper()
	pc := pcAtMarker(t, r, source, marker)
	entries, err := r.subprogramVars(pc)
	if err != nil {
		t.Fatalf("%s: subprogramVars: %v", marker, err)
	}
	return entries
}

func pcAtMarker(t *testing.T, r *dwarfReader, source, marker string) uint64 {
	t.Helper()
	line := markerLine(t, source, marker)
	pc, err := r.PCForFileLine("scope_fixture.go", line)
	if err != nil {
		t.Fatalf("%s: PCForFileLine: %v", marker, err)
	}
	return pc
}

func markerLine(t *testing.T, source, marker string) int {
	t.Helper()
	for i, line := range strings.Split(source, "\n") {
		if strings.Contains(line, marker) {
			return i + 1
		}
	}
	t.Fatalf("marker %q not found", marker)
	return 0
}

func entryNameCounts(entries []*dwarf.Entry) map[string]int {
	names := make(map[string]int)
	for _, entry := range entries {
		name, _ := entry.Val(dwarf.AttrName).(string)
		names[name]++
	}
	return names
}

func variableAddressAtMarker(t *testing.T, r *dwarfReader, source, marker, name string) uint64 {
	t.Helper()
	entries := varsAtMarker(t, r, source, marker)
	for _, entry := range entries {
		if entryName, _ := entry.Val(dwarf.AttrName).(string); entryName == name {
			addr, ok := r.varAddress(entry, 0x100000)
			if !ok {
				t.Fatalf("%s: %q has no supported location", marker, name)
			}
			return addr
		}
	}
	t.Fatalf("%s: no variable named %q in %v", marker, name, entryNameCounts(entries))
	return 0
}

func assertLinuxLexicalRangeForm(t *testing.T, r *dwarfReader) {
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
		if entry.Tag == dwarf.TagLexDwarfBlock && entry.Val(dwarf.AttrRanges) != nil {
			return
		}
	}
	t.Fatal("linux fixture emitted no range-list lexical block")
}

func inlineScopesAtPC(t *testing.T, r *dwarfReader, runtimePC uint64) (total, inactive int) {
	t.Helper()
	dwarfPC := uint64(int64(runtimePC) - r.slide)
	rd := r.data.Reader()
	for {
		entry, err := rd.Next()
		if err != nil {
			t.Fatalf("read fixture DWARF: %v", err)
		}
		if entry == nil {
			return 0, 0
		}
		if entry.Tag != dwarf.TagSubprogram {
			continue
		}
		ranges, err := r.data.Ranges(entry)
		if err != nil {
			t.Fatalf("read subprogram ranges: %v", err)
		}
		if !pcInRanges(dwarfPC, ranges) {
			rd.SkipChildren()
			continue
		}
		if !entry.Children {
			t.Fatal("containing subprogram has no children")
		}
		depth := 1
		for depth > 0 {
			child, err := rd.Next()
			if err != nil {
				t.Fatalf("read subprogram children: %v", err)
			}
			if child == nil {
				t.Fatal("unexpected end of subprogram children")
			}
			if child.Tag == 0 {
				depth--
				continue
			}
			if child.Tag == dwarf.TagInlinedSubroutine {
				total++
				ranges, err := r.data.Ranges(child)
				if err != nil {
					t.Fatalf("read inline ranges: %v", err)
				}
				if len(ranges) > 0 && !pcInRanges(dwarfPC, ranges) {
					inactive++
				}
			}
			if child.Children {
				depth++
			}
		}
		return total, inactive
	}
}

func pcInRanges(pc uint64, ranges [][2]uint64) bool {
	for _, pcs := range ranges {
		if pc >= pcs[0] && pc < pcs[1] {
			return true
		}
	}
	return false
}
