package debugger

import (
	"debug/dwarf"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// readableBackend is a minimal Backend whose ReadMemory always succeeds (returns
// zeroed bytes), so the pure formatter can be exercised without a real tracee.
// Only ReadMemory is used by the value walk; the rest satisfy the interface.
type readableBackend struct{}

func (readableBackend) ContinueProcess() error                { return nil }
func (readableBackend) SingleStep(int) error                  { return nil }
func (readableBackend) StopProcess() error                    { return nil }
func (readableBackend) PauseSignal() int                      { return 0 }
func (readableBackend) ReadMemory(_ uint64, dst []byte) error { return nil }
func (readableBackend) WriteMemory(uint64, []byte) error      { return nil }
func (readableBackend) GetRegisters(int) (Registers, error)   { return Registers{}, nil }
func (readableBackend) SetRegisters(int, Registers) error     { return nil }
func (readableBackend) Threads() ([]int, error)               { return []int{1}, nil }
func (readableBackend) Wait() (StopEvent, error)              { return StopEvent{}, nil }

type valueMemoryBackend struct {
	readableBackend
	mem map[uint64]byte
}

func newValueMemoryBackend() *valueMemoryBackend {
	return &valueMemoryBackend{mem: make(map[uint64]byte)}
}

func (b *valueMemoryBackend) ReadMemory(addr uint64, dst []byte) error {
	for i := range dst {
		value, ok := b.mem[addr+uint64(i)]
		if !ok {
			return errors.New("unreadable memory")
		}
		dst[i] = value
	}
	return nil
}

func (b *valueMemoryBackend) seedUint64(addr, value uint64) {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, value)
	for i := range buf {
		b.mem[addr+uint64(i)] = buf[i]
	}
}

func testIntType() *dwarf.IntType {
	return &dwarf.IntType{BasicType: dwarf.BasicType{
		CommonType: dwarf.CommonType{ByteSize: 8, Name: "int"},
	}}
}

func testPointerType(name string, target dwarf.Type) *dwarf.PtrType {
	return &dwarf.PtrType{
		CommonType: dwarf.CommonType{ByteSize: 8, Name: name},
		Type:       target,
	}
}

func countNodes(v protocol.Variable) int {
	n := 1
	for i := range v.Children {
		n += countNodes(v.Children[i])
	}
	return n
}

func hasTruncationNode(v protocol.Variable) bool {
	if v.Value == truncatedValue {
		return true
	}
	for i := range v.Children {
		if hasTruncationNode(v.Children[i]) {
			return true
		}
	}
	return false
}

func TestFormatTypedExpandsSiblingPointerAliases(t *testing.T) {
	intType := testIntType()
	ptrType := testPointerType("*int", intType)
	pairType := &dwarf.StructType{
		CommonType: dwarf.CommonType{ByteSize: 16, Name: "pair"},
		StructName: "pair",
		Kind:       "struct",
		Field: []*dwarf.StructField{
			{Name: "Left", Type: ptrType, ByteOffset: 0},
			{Name: "Right", Type: ptrType, ByteOffset: 8},
		},
	}
	backend := newValueMemoryBackend()
	backend.seedUint64(0x1000, 0x2000)
	backend.seedUint64(0x1008, 0x2000)
	backend.seedUint64(0x2000, 42)

	root := (&dwarfReader{}).formatTyped(backend, "pair", pairType, 0x1000)

	if len(root.Children) != 2 {
		t.Fatalf("pair children = %d, want 2", len(root.Children))
	}
	for _, pointer := range root.Children {
		if len(pointer.Children) != 1 {
			t.Errorf("%s children = %d, want 1", pointer.Name, len(pointer.Children))
			continue
		}
		if got := pointer.Children[0].Value; got != "42" {
			t.Errorf("%s pointee = %q, want 42", pointer.Name, got)
		}
	}
}

func TestFormatCtxPointerPathOwnership(t *testing.T) {
	ctx := &formatCtx{activePointers: make(map[uint64]bool)}

	leaveAncestor, ok := ctx.enterPointer(0x2000)
	if !ok {
		t.Fatal("first pointer entry was rejected")
	}
	if leaveCycle, ok := ctx.enterPointer(0x2000); ok || leaveCycle != nil {
		t.Fatal("cycle entry must not own ancestor cleanup")
	}
	if !ctx.activePointers[0x2000] {
		t.Fatal("rejected cycle entry removed its ancestor")
	}

	leaveAncestor()
	leaveAlias, ok := ctx.enterPointer(0x2000)
	if !ok {
		t.Fatal("sibling alias was rejected after ancestor unwound")
	}
	leaveAlias()
}

func TestFormatTypedPointerCyclesRemainBounded(t *testing.T) {
	t.Run("self cycle", func(t *testing.T) {
		nodeType := &dwarf.StructType{
			CommonType: dwarf.CommonType{ByteSize: 8, Name: "node"},
			StructName: "node",
			Kind:       "struct",
		}
		nodePtr := testPointerType("*node", nodeType)
		nodeType.Field = []*dwarf.StructField{
			{Name: "Next", Type: nodePtr, ByteOffset: 0},
		}
		backend := newValueMemoryBackend()
		backend.seedUint64(0x1000, 0x2000)
		backend.seedUint64(0x2000, 0x2000)

		root := (&dwarfReader{}).formatTyped(backend, "head", nodePtr, 0x1000)

		if total := countNodes(root); total > 4 {
			t.Fatalf("self-cycle node count = %d, want <= 4", total)
		}
	})

	t.Run("mutual cycle", func(t *testing.T) {
		leftType := &dwarf.StructType{
			CommonType: dwarf.CommonType{ByteSize: 8, Name: "leftNode"},
			StructName: "leftNode",
			Kind:       "struct",
		}
		rightType := &dwarf.StructType{
			CommonType: dwarf.CommonType{ByteSize: 8, Name: "rightNode"},
			StructName: "rightNode",
			Kind:       "struct",
		}
		leftType.Field = []*dwarf.StructField{
			{Name: "Right", Type: testPointerType("*rightNode", rightType), ByteOffset: 0},
		}
		rightType.Field = []*dwarf.StructField{
			{Name: "Left", Type: testPointerType("*leftNode", leftType), ByteOffset: 0},
		}
		backend := newValueMemoryBackend()
		backend.seedUint64(0x2000, 0x3000)
		backend.seedUint64(0x3000, 0x2000)

		root := (&dwarfReader{}).formatTyped(backend, "left", leftType, 0x2000)

		if total := countNodes(root); total > 5 {
			t.Fatalf("mutual-cycle node count = %d, want <= 5", total)
		}
	})
}

func TestFormatTypedUnreadablePointerTarget(t *testing.T) {
	backend := newValueMemoryBackend()
	backend.seedUint64(0x1000, 0x2000)

	root := (&dwarfReader{}).formatTyped(
		backend,
		"value",
		testPointerType("*int", testIntType()),
		0x1000,
	)

	if len(root.Children) != 1 {
		t.Fatalf("pointer children = %d, want 1", len(root.Children))
	}
	if got, want := root.Children[0].Value, "<unreadable: unreadable memory>"; got != want {
		t.Errorf("unreadable pointee = %q, want %q", got, want)
	}
}

// TestFormatTypedBudget guards the shared per-inspection node/byte ceiling: a
// pathologically wide, deep aggregate ([100][100][100][100]int, ~10^8 potential
// nodes) must be truncated to O(maxTotalNodes) with a truncation node present,
// so one Locals/Evaluate request can never wedge the single-threaded engine loop.
func TestFormatTypedBudget(t *testing.T) {
	intT := testIntType()
	arr := func(elem dwarf.Type, count int64) *dwarf.ArrayType {
		return &dwarf.ArrayType{Type: elem, Count: count}
	}
	// Four nested levels, each wider than maxChildren, so the product dwarfs the
	// global budget while each level's own width cap alone would not save us.
	nested := arr(arr(arr(arr(intT, 100), 100), 100), 100)

	r := &dwarfReader{}
	root := r.formatTyped(readableBackend{}, "grid", nested, 0x1000)

	total := countNodes(root)
	// Real nodes are capped at maxTotalNodes; truncation leaves add only O(depth)
	// along the unwinding path, so a small slack is plenty.
	if total > maxTotalNodes+1000 {
		t.Fatalf("node count %d exceeds budget ceiling %d(+slack)", total, maxTotalNodes)
	}
	if !hasTruncationNode(root) {
		t.Fatalf("expected a truncation node when the inspection budget is exhausted")
	}
}

// Pure leaf-formatter tests: these take raw little-endian bytes plus an implied
// width and assert the rendered string, so they need no DWARF data or backend.

func TestFormatInt(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
		want string
	}{
		{"int8 -1", []byte{0xff}, "-1"},
		{"int8 127", []byte{0x7f}, "127"},
		{"int16 -2", []byte{0xfe, 0xff}, "-2"},
		{"int32 258", []byte{0x02, 0x01, 0x00, 0x00}, "258"},
		{"int64 -1", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "-1"},
		{"int64 max", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}, "9223372036854775807"},
	}
	for _, c := range cases {
		if got := formatInt(c.buf); got != c.want {
			t.Errorf("%s: formatInt(%x) = %q, want %q", c.name, c.buf, got, c.want)
		}
	}
}

func TestFormatUint(t *testing.T) {
	cases := []struct {
		buf  []byte
		want string
	}{
		{[]byte{0xff}, "255"},
		{[]byte{0x00, 0x01}, "256"},
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "18446744073709551615"},
	}
	for _, c := range cases {
		if got := formatUint(c.buf); got != c.want {
			t.Errorf("formatUint(%x) = %q, want %q", c.buf, got, c.want)
		}
	}
}

func TestFormatBool(t *testing.T) {
	if got := formatBool([]byte{0x00}); got != "false" {
		t.Errorf("formatBool(00) = %q, want false", got)
	}
	if got := formatBool([]byte{0x01}); got != "true" {
		t.Errorf("formatBool(01) = %q, want true", got)
	}
}

func TestFormatFloat(t *testing.T) {
	// 3.5 as float64 = 0x400C000000000000 (LE)
	f64 := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0c, 0x40}
	if got := formatFloat(f64); got != "3.5" {
		t.Errorf("formatFloat(f64 3.5) = %q, want 3.5", got)
	}
	// 1.5 as float32 = 0x3FC00000 (LE)
	f32 := []byte{0x00, 0x00, 0xc0, 0x3f}
	if got := formatFloat(f32); got != "1.5" {
		t.Errorf("formatFloat(f32 1.5) = %q, want 1.5", got)
	}
}

func TestFormatComplex(t *testing.T) {
	// complex128 (3 + 4i): re float64=3, im float64=4
	c128 := []byte{
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x08, 0x40, // 3.0
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x40, // 4.0
	}
	if got := formatComplex(c128); got != "(3+4i)" {
		t.Errorf("formatComplex(3+4i) = %q, want (3+4i)", got)
	}
	// complex64 (1 - 2i)
	c64 := []byte{
		0x00, 0x00, 0x80, 0x3f, // 1.0
		0x00, 0x00, 0x00, 0xc0, // -2.0
	}
	if got := formatComplex(c64); got != "(1-2i)" {
		t.Errorf("formatComplex(1-2i) = %q, want (1-2i)", got)
	}
}
