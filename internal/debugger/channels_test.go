package debugger

import (
	"debug/dwarf"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func channelFixture(elem dwarf.Type) (*dwarfReader, dwarf.Type, channelType) {
	uintType := func(size int64) dwarf.Type {
		return &dwarf.UintType{BasicType: dwarf.BasicType{
			CommonType: dwarf.CommonType{ByteSize: size, Name: fmt.Sprintf("uint%d", size*8)},
		}}
	}
	header := &dwarf.StructType{
		CommonType: dwarf.CommonType{ByteSize: 64}, Kind: "struct",
		Field: []*dwarf.StructField{
			{Name: "recvx", Type: uintType(8), ByteOffset: 56},
			{Name: "closed", Type: uintType(4), ByteOffset: 44},
			{Name: "elemsize", Type: uintType(2), ByteOffset: 40},
			{Name: "buf", Type: testPointerType("", &dwarf.VoidType{}), ByteOffset: 24},
			{Name: "dataqsiz", Type: uintType(8), ByteOffset: 16},
			{Name: "qcount", Type: uintType(8), ByteOffset: 8},
		},
	}
	typ := &dwarf.TypedefType{
		CommonType: dwarf.CommonType{Name: "chan " + typeDisplayName(elem), ByteSize: 8},
		Type:       testPointerType("", header),
	}
	info := channelType{header: header, elem: elem}
	r := &dwarfReader{channelTypes: map[dwarf.Type]channelType{typ: info}}
	r.channelTypesOnce.Do(func() {})
	return r, typ, info
}

func seedChannel(t *testing.T, b *valueMemoryBackend, info channelType, h channelHeader) {
	t.Helper()
	b.seedUint64(0x1000, 0x2000)
	layout, err := resolveChannelLayout(info)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < layout.size; i++ {
		b.mem[0x2000+uint64(i)] = 0
	}
	for i, v := range []uint64{h.count, h.capacity, h.buffer, h.elemSize, h.closed, h.receive} {
		field := layout.fields[i]
		for j := int64(0); j < field.Type.Size(); j++ {
			b.mem[0x2000+uint64(field.ByteOffset+j)] = byte(v >> (j * 8))
		}
	}
}

func variableChild(t *testing.T, v protocol.Variable, name string) protocol.Variable {
	t.Helper()
	for _, child := range v.Children {
		if child.Name == name {
			return child
		}
	}
	t.Fatalf("child %q absent in %#v", name, v)
	return protocol.Variable{}
}

func TestChannelContentsFIFOAndReadOnly(t *testing.T) {
	r, typ, info := channelFixture(testIntType())
	b := newValueMemoryBackend()
	seedChannel(t, b, info, channelHeader{count: 3, capacity: 3, buffer: 0x3000, elemSize: 8, closed: 1, receive: 2})
	b.seedUint64(0x3000, 20)
	b.seedUint64(0x3008, 30)
	b.seedUint64(0x3010, 10)
	original := make(map[uint64]byte)
	for addr, v := range b.mem {
		original[addr] = v
	}
	backend := &observedChannelBackend{valueMemoryBackend: b}
	root := r.formatTyped(backend, "values", typ, 0x1000)
	if root.Kind != kindChan || !strings.Contains(root.Value, "len:3 cap:3 closed:true") {
		t.Fatalf("channel summary = %#v", root)
	}
	for i, want := range []string{"10", "20", "30"} {
		child := variableChild(t, root, fmt.Sprintf("[%d]", i))
		if child.Value != want || child.Type != "int" || child.Kind != kindInt {
			t.Errorf("element %d = %#v, want typed %s", i, child, want)
		}
	}
	for name, want := range map[string]string{"len": "3", "cap": "3", "closed": "true"} {
		if got := variableChild(t, root, name).Value; got != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
	if backend.writes != 0 || !reflect.DeepEqual(b.mem, original) {
		t.Fatal("inspection mutated the channel")
	}
}

func TestChannelEmptyStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    channelHeader
		nil  bool
		want string
	}{
		{"nil", channelHeader{}, true, "(nil)"},
		{"unbuffered", channelHeader{elemSize: 8}, false, "unbuffered; no stored values"},
		{"closed drained", channelHeader{capacity: 3, buffer: 0x3000, elemSize: 8, closed: 1}, false, "len:0 cap:3 closed:true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, typ, info := channelFixture(testIntType())
			b := newValueMemoryBackend()
			seedChannel(t, b, info, tc.h)
			if tc.nil {
				b.seedUint64(0x1000, 0)
			}
			root := r.formatTyped(b, "ch", typ, 0x1000)
			if !strings.Contains(root.Value, tc.want) {
				t.Fatalf("summary = %q, want %q", root.Value, tc.want)
			}
			wantChildren := 3
			if tc.nil {
				wantChildren = 0
			}
			if len(root.Children) != wantChildren {
				t.Fatalf("children = %#v", root.Children)
			}
		})
	}
}

func TestChannelRejectsInvalidOrUnreadableMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    channelHeader
	}{
		{"count", channelHeader{count: 2, capacity: 1, elemSize: 8}},
		{"negative capacity", channelHeader{capacity: math.MaxUint64, elemSize: 8}},
		{"receive index", channelHeader{capacity: 1, receive: 1, elemSize: 8}},
		{"unbuffered index", channelHeader{receive: 1, elemSize: 8}},
		{"closed flag", channelHeader{closed: 2, elemSize: 8}},
		{"size", channelHeader{elemSize: 4}},
		{"nil buffer", channelHeader{count: 1, capacity: 1, elemSize: 8}},
		{"empty nil buffer", channelHeader{capacity: 1, elemSize: 8}},
		{"overflow", channelHeader{count: 1, capacity: 2, buffer: math.MaxUint64 - 8, elemSize: 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, typ, info := channelFixture(testIntType())
			b := newValueMemoryBackend()
			seedChannel(t, b, info, tc.h)
			root := r.formatTyped(b, "ch", typ, 0x1000)
			if !strings.Contains(root.Value, "<unreadable:") || len(root.Children) != 0 {
				t.Fatalf("invalid metadata exposed values: %#v", root)
			}
		})
	}
	t.Run("missing layout", func(t *testing.T) {
		r, typ, _ := channelFixture(testIntType())
		r.channelTypes[typ] = channelType{}
		b := newValueMemoryBackend()
		b.seedUint64(0x1000, 0x2000)
		root := r.formatTyped(b, "ch", typ, 0x1000)
		if !strings.Contains(root.Value, "DWARF layout or element type unavailable") {
			t.Fatalf("missing layout = %#v", root)
		}
	})
	t.Run("unreadable header", func(t *testing.T) {
		r, typ, _ := channelFixture(testIntType())
		b := newValueMemoryBackend()
		b.seedUint64(0x1000, 0x2000)
		root := r.formatTyped(b, "ch", typ, 0x1000)
		if !strings.Contains(root.Value, "unreadable memory") || len(root.Children) != 0 {
			t.Fatalf("unreadable header = %#v", root)
		}
	})
}

func TestChannelRejectsInvalidDWARFLayout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*dwarf.StructType)
	}{
		{"incomplete", func(h *dwarf.StructType) { h.Incomplete = true }},
		{"missing field", func(h *dwarf.StructType) { h.Field = h.Field[1:] }},
		{"negative offset", func(h *dwarf.StructType) { h.Field[0].ByteOffset = -1 }},
		{"bitfield", func(h *dwarf.StructType) { h.Field[0].BitSize = 1 }},
		{"nil type", func(h *dwarf.StructType) { h.Field[0].Type = nil }},
		{"wrong type", func(h *dwarf.StructType) { h.Field[0].Type = testIntType() }},
		{"unsupported width", func(h *dwarf.StructType) {
			h.Field[0].Type = &dwarf.UintType{BasicType: dwarf.BasicType{CommonType: dwarf.CommonType{ByteSize: 1}}}
		}},
		{"non-pointer buffer", func(h *dwarf.StructType) { h.Field[3].Type = testIntType() }},
		{"outside header", func(h *dwarf.StructType) { h.Field[0].ByteOffset = h.Size() }},
		{"outside read budget", func(h *dwarf.StructType) {
			h.ByteSize = maxScalarBytes + 8
			h.Field[0].ByteOffset = maxScalarBytes
		}},
		{"overlap", func(h *dwarf.StructType) { h.Field[0].ByteOffset = h.Field[1].ByteOffset }},
		{"duplicate", func(h *dwarf.StructType) { h.Field = append(h.Field, h.Field[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, typ, info := channelFixture(testIntType())
			tc.mutate(info.header)
			b := newValueMemoryBackend()
			b.seedUint64(0x1000, 0x2000)
			backend := &observedChannelBackend{valueMemoryBackend: b}
			root := r.formatTyped(backend, "ch", typ, 0x1000)
			if !strings.Contains(root.Value, "<unreadable:") || len(root.Children) != 0 || backend.bytes != 8 {
				t.Fatalf("invalid DWARF read a runtime header: %#v, bytes=%d", root, backend.bytes)
			}
		})
	}
}

func TestChannelLayoutIgnoresUnrelatedFields(t *testing.T) {
	_, _, info := channelFixture(testIntType())
	want, err := resolveChannelLayout(info)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := &dwarf.StructField{Name: "sendx", ByteOffset: -1}
	info.header.Field = append([]*dwarf.StructField{nil, unrelated, unrelated}, info.header.Field...)
	got, err := resolveChannelLayout(info)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("layout = %#v, want %#v", got, want)
	}
}

type observedChannelBackend struct {
	*valueMemoryBackend
	onRead func(uint64)
	bytes  int
	writes int
}

func (b *observedChannelBackend) ReadMemory(addr uint64, dst []byte) error {
	b.bytes += len(dst)
	if b.onRead != nil {
		b.onRead(addr)
	}
	return b.valueMemoryBackend.ReadMemory(addr, dst)
}

func (b *observedChannelBackend) WriteMemory(uint64, []byte) error {
	b.writes++
	return fmt.Errorf("unexpected channel write")
}

func TestChannelDetectsConcurrentChanges(t *testing.T) {
	for _, changePointer := range []bool{false, true} {
		t.Run(fmt.Sprintf("pointer=%t", changePointer), func(t *testing.T) {
			r, typ, info := channelFixture(testIntType())
			memory := newValueMemoryBackend()
			seedChannel(t, memory, info, channelHeader{count: 1, capacity: 2, buffer: 0x3000, elemSize: 8})
			memory.seedUint64(0x3000, 42)
			b := &observedChannelBackend{valueMemoryBackend: memory, onRead: func(addr uint64) {
				if addr == 0x3000 {
					if changePointer {
						memory.seedUint64(0x1000, 0x4000)
					} else {
						memory.seedUint64(0x2008, 0)
					}
				}
			}}
			root := r.formatTyped(b, "ch", typ, 0x1000)
			if !strings.Contains(root.Value, "changed during inspection") || len(root.Children) != 0 {
				t.Fatalf("changing channel exposed stale values: %#v", root)
			}
		})
	}
}

func TestChannelTypedElementsCyclesAliasesAndZeroSize(t *testing.T) {
	t.Run("struct and pointer", func(t *testing.T) {
		elem := &dwarf.StructType{
			CommonType: dwarf.CommonType{ByteSize: 8, Name: "item"}, StructName: "item", Kind: "struct",
			Field: []*dwarf.StructField{{Name: "Value", Type: testPointerType("*int", testIntType())}},
		}
		r, typ, info := channelFixture(elem)
		b := newValueMemoryBackend()
		seedChannel(t, b, info, channelHeader{count: 1, capacity: 1, buffer: 0x3000, elemSize: 8})
		b.seedUint64(0x3000, 0x4000)
		b.seedUint64(0x4000, 42)
		root := r.formatTyped(b, "ch", typ, 0x1000)
		pointer := variableChild(t, variableChild(t, root, "[0]"), "Value")
		if len(pointer.Children) != 1 || pointer.Children[0].Value != "42" {
			t.Fatalf("typed element = %#v", root)
		}
	})
	t.Run("zero sized", func(t *testing.T) {
		elem := &dwarf.StructType{StructName: "struct {}", Kind: "struct"}
		r, typ, info := channelFixture(elem)
		b := newValueMemoryBackend()
		seedChannel(t, b, info, channelHeader{count: 2, capacity: 2})
		root := r.formatTyped(b, "ch", typ, 0x1000)
		for _, name := range []string{"[0]", "[1]"} {
			if v := variableChild(t, root, name); v.Kind != kindStruct || v.Address != 0 {
				t.Fatalf("zero-size element = %#v", v)
			}
		}
	})
	t.Run("channel cycle and cross-root alias", func(t *testing.T) {
		r, typ, info := channelFixture(testIntType())
		info.elem = typ
		r.channelTypes[typ] = info
		b := newValueMemoryBackend()
		seedChannel(t, b, info, channelHeader{count: 1, capacity: 1, buffer: 0x3000, elemSize: 8})
		b.seedUint64(0x3000, 0x2000)
		ctx := newFormatCtx(b)
		for i := 0; i < 2; i++ {
			root := r.formatRoot(ctx, "ch", typ, 0x1000)
			child := variableChild(t, root, "[0]")
			if !strings.Contains(child.Value, "<cycle>") || len(child.Children) != 0 || len(ctx.activePointers) != 0 {
				t.Fatalf("cycle/alias = %#v, active = %v", root, ctx.activePointers)
			}
		}
	})
}

func TestChannelSharesInspectionBudgets(t *testing.T) {
	r, typ, info := channelFixture(testIntType())
	memory := newValueMemoryBackend()
	seedChannel(t, memory, info, channelHeader{count: 200, capacity: 200, buffer: 0x3000, elemSize: 8})
	for i := uint64(0); i < 100; i++ {
		memory.seedUint64(0x3000+i*8, i)
	}
	b := &observedChannelBackend{valueMemoryBackend: memory}
	root := r.formatTyped(b, "ch", typ, 0x1000)
	if len(root.Children) != maxChildren+4 || !strings.Contains(root.Children[len(root.Children)-1].Value, "100 more") {
		t.Fatalf("element cap = %#v", root)
	}
	ctx := newFormatCtx(b)
	ctx.bytesLeft = 140
	before := b.bytes
	root = r.formatRoot(ctx, "ch", typ, 0x1000)
	if !strings.Contains(root.Value, truncatedValue) || len(root.Children) != 0 || b.bytes-before > 140 {
		t.Fatalf("byte ceiling = %#v, bytes = %d", root, b.bytes-before)
	}
	ctx = newFormatCtx(b)
	ctx.nodesLeft = 8
	roots := formatRequestRoots(ctx, 3, func(i int) protocol.Variable {
		return r.formatRoot(ctx, "ch", typ, 0x1000)
	})
	if len(roots) != 2 || roots[1].Value != truncatedValue || ctx.nodesLeft != 0 {
		t.Fatalf("shared node ceiling = %#v, remaining = %d", roots, ctx.nodesLeft)
	}
}

func TestChannelDWARFNamedAndDirectionalTypes(t *testing.T) {
	const source = `package main
import "fmt"
type Item struct { Name string; Value *int }
type Named chan Item
var Plain = make(chan int, 3)
var Receive <-chan int = Plain
var Send chan<- int = Plain
var NamedChannel = make(Named, 2)
var Empty = make(chan struct{}, 2)
var Nested = make(chan chan int, 2)
func main() { fmt.Println(Plain, Receive, Send, NamedChannel, Empty, Nested) }
`
	r := buildDWARFScopeFixture(t, source, false)
	for _, name := range []string{"Plain", "Receive", "Send", "NamedChannel", "Empty", "Nested"} {
		t.Run(name, func(t *testing.T) {
			_, typ, ok := r.globalVar(0, "main."+name)
			if !ok {
				t.Fatalf("global %s missing", name)
			}
			info, ok := r.channelTypeInfo(typ)
			if !ok {
				t.Fatalf("channel type %s unrecognized: %#v", name, typ)
			}
			if _, err := resolveChannelLayout(info); err != nil {
				t.Fatalf("channel type %s: %v", name, err)
			}
			b := newValueMemoryBackend()
			seedChannel(t, b, info, channelHeader{capacity: 2, buffer: 0x3000, elemSize: uint64(info.elem.Size())})
			root := r.formatTyped(b, "ch", typ, 0x1000)
			if root.Kind != kindChan || len(root.Children) != 3 {
				t.Fatalf("real DWARF channel = %#v", root)
			}
		})
	}
}
