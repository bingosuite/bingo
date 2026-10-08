package debugger

import (
	"bytes"
	"debug/dwarf"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// Go's DWARF extensions preserve the element type even though hchan.buf is
// unsafe.Pointer. Named and directional channels must not be parsed as strings.
const (
	dwarfAttrGoKind = dwarf.Attr(0x2900)
	dwarfAttrGoElem = dwarf.Attr(0x2902)
	dwarfGoChanKind = int64(18)
)

type channelType struct {
	header *dwarf.StructType
	elem   dwarf.Type
}

func (r *dwarfReader) channelTypeInfo(typ dwarf.Type) (channelType, bool) {
	r.channelTypesOnce.Do(func() {
		r.channelTypes = make(map[dwarf.Type]channelType)
		if r.data == nil {
			return
		}
		rd := r.data.Reader()
		for {
			entry, err := rd.Next()
			if err != nil || entry == nil {
				return
			}
			if entry.Val(dwarfAttrGoKind) != dwarfGoChanKind {
				continue
			}
			t, err := r.data.Type(entry.Offset)
			if err != nil {
				continue
			}
			info := channelType{}
			if off, ok := entry.Val(dwarfAttrGoElem).(dwarf.Offset); ok {
				info.elem, _ = r.data.Type(off)
			}
			if ptr, ok := channelUnderlying(t).(*dwarf.PtrType); ok {
				info.header, _ = channelUnderlying(ptr.Type).(*dwarf.StructType)
			}
			r.channelTypes[t] = info
		}
	})
	seen := make(map[dwarf.Type]bool)
	for typ != nil && !seen[typ] {
		if info, ok := r.channelTypes[typ]; ok {
			return info, true
		}
		seen[typ] = true
		switch t := typ.(type) {
		case *dwarf.TypedefType:
			typ = t.Type
		case *dwarf.QualType:
			typ = t.Type
		default:
			return channelType{}, false
		}
	}
	return channelType{}, false
}

func channelUnderlying(typ dwarf.Type) dwarf.Type {
	seen := make(map[dwarf.Type]bool)
	for typ != nil && !seen[typ] {
		seen[typ] = true
		switch t := typ.(type) {
		case *dwarf.TypedefType:
			typ = t.Type
		case *dwarf.QualType:
			typ = t.Type
		default:
			return typ
		}
	}
	return nil
}

type channelHeader struct {
	count, capacity, buffer, elemSize, closed, receive uint64
}

type channelLayout struct {
	fields [6]*dwarf.StructField
	size   int
}

func validateChannelDWARFField(field *dwarf.StructField, name string, headerSize int64) error {
	if field == nil || field.Type == nil || field.ByteOffset < 0 || field.BitSize != 0 {
		return fmt.Errorf("channel DWARF field %s unavailable", name)
	}
	size := field.Type.Size()
	if size != 2 && size != 4 && size != 8 {
		return fmt.Errorf("unsupported channel DWARF field %s width", name)
	}
	if name == "buf" {
		if ptr, ok := channelUnderlying(field.Type).(*dwarf.PtrType); !ok || ptr.Size() != 8 {
			return fmt.Errorf("unsupported channel buffer pointer")
		}
	} else if _, ok := channelUnderlying(field.Type).(*dwarf.UintType); !ok {
		return fmt.Errorf("unsupported channel DWARF field %s type", name)
	}
	if field.ByteOffset > maxScalarBytes-size || field.ByteOffset+size > headerSize {
		return fmt.Errorf("channel DWARF field %s outside header", name)
	}
	return nil
}

func resolveChannelLayout(info channelType) (channelLayout, error) {
	var layout channelLayout
	if info.header == nil || info.elem == nil || info.header.Incomplete {
		return layout, fmt.Errorf("channel DWARF layout or element type unavailable")
	}
	names := [...]string{"qcount", "dataqsiz", "buf", "elemsize", "closed", "recvx"}
	for i, name := range names {
		for _, field := range info.header.Field {
			if field != nil && field.Name == name {
				if layout.fields[i] != nil {
					return layout, fmt.Errorf("duplicate channel DWARF field %s", name)
				}
				layout.fields[i] = field
			}
		}
		field := layout.fields[i]
		if err := validateChannelDWARFField(field, name, info.header.Size()); err != nil {
			return layout, err
		}
		size := field.Type.Size()
		for _, previous := range layout.fields[:i] {
			if field.ByteOffset < previous.ByteOffset+previous.Type.Size() &&
				previous.ByteOffset < field.ByteOffset+size {
				return layout, fmt.Errorf("overlapping channel DWARF field %s", name)
			}
		}
		layout.size = max(layout.size, int(field.ByteOffset+size))
	}
	return layout, nil
}

func (layout channelLayout) decode(raw []byte) channelHeader {
	var values [6]uint64
	for i, field := range layout.fields {
		b := raw[int(field.ByteOffset):int(field.ByteOffset+field.Type.Size())]
		for j := len(b) - 1; j >= 0; j-- {
			values[i] = values[i]<<8 | uint64(b[j])
		}
	}
	return channelHeader{values[0], values[1], values[2], values[3], values[4], values[5]}
}

func (h channelHeader) validate(elem dwarf.Type) error {
	if h.count > h.capacity || h.capacity > math.MaxInt64 || h.closed > 1 ||
		(h.capacity == 0 && h.receive != 0) || (h.capacity > 0 && h.receive >= h.capacity) {
		return fmt.Errorf("inconsistent channel header")
	}
	size := elem.Size()
	if size < 0 || size > math.MaxUint16 || uint64(size) != h.elemSize {
		return fmt.Errorf("channel element size does not match DWARF")
	}
	if h.capacity > 0 && size > 0 && h.buffer == 0 {
		return fmt.Errorf("buffered channel has a nil buffer")
	}
	if h.elemSize > 0 && h.capacity > (math.MaxUint64-h.buffer)/h.elemSize {
		return fmt.Errorf("channel buffer address overflow")
	}
	return nil
}

func (r *dwarfReader) formatChannel(out *protocol.Variable, info channelType, addr uint64, depth, ptrDepth int, ctx *formatCtx) {
	out.Kind = kindChan
	pointer, err := ctx.read(addr, 8)
	if err != nil {
		out.Value = readErrorValue(err)
		return
	}
	p := binary.LittleEndian.Uint64(pointer)
	if p == 0 {
		out.Value = out.Type + " (nil)"
		return
	}
	fail := func(err error) {
		out.Value = fmt.Sprintf("%s (0x%x) %s", out.Type, p, readErrorValue(err))
		out.Children = nil
	}
	layout, err := resolveChannelLayout(info)
	if err != nil {
		fail(err)
		return
	}
	if p > math.MaxUint64-uint64(layout.size) {
		fail(fmt.Errorf("channel header address overflow"))
		return
	}
	raw, err := ctx.read(p, layout.size)
	if err != nil {
		fail(err)
		return
	}
	header := layout.decode(raw)
	if err := header.validate(info.elem); err != nil {
		fail(err)
		return
	}
	out.Value = fmt.Sprintf("%s len:%d cap:%d closed:%t (0x%x)",
		out.Type, header.count, header.capacity, header.closed != 0, p)
	if header.capacity == 0 {
		out.Value += " (unbuffered; no stored values)"
	}
	if depth >= maxValueDepth {
		return
	}
	leave, ok := ctx.enterPointer(p)
	if !ok {
		out.Value += " <cycle>"
		return
	}
	defer leave()
	for _, child := range []protocol.Variable{
		{Name: "len", Type: "int", Kind: kindInt, Value: strconv.FormatUint(header.count, 10)},
		{Name: "cap", Type: "int", Kind: kindInt, Value: strconv.FormatUint(header.capacity, 10)},
		{Name: "closed", Type: "bool", Kind: kindBool, Value: strconv.FormatBool(header.closed != 0)},
	} {
		if !ctx.takeNode() {
			out.Children = append(out.Children, truncatedNode())
			break
		}
		out.Children = append(out.Children, child)
	}
	shown := min(header.count, uint64(maxChildren))
	for i := uint64(0); i < shown; i++ {
		if ctx.budgetExhausted() {
			fail(errInspectionBudgetExhausted)
			return
		}
		slot := header.receive + i
		if slot >= header.capacity {
			slot -= header.capacity
		}
		elementAddr := header.buffer + slot*header.elemSize
		out.Children = append(out.Children,
			r.formatNode(fmt.Sprintf("[%d]", i), info.elem, elementAddr, depth+1, ptrDepth, ctx))
	}
	if header.count > shown {
		out.Children = append(out.Children, moreNode(int(header.count-shown)))
	}
	// Rechecking metadata catches visible buffer movement, not ABA or mutations
	// of pointees. Linux only stops the reporting thread; this is not atomic.
	after, err := ctx.read(p, layout.size)
	if err != nil {
		fail(err)
		return
	}
	currentPointer, err := ctx.read(addr, 8)
	if err != nil {
		fail(err)
		return
	}
	if layout.decode(after) != header || !bytes.Equal(pointer, currentPointer) {
		fail(fmt.Errorf("channel changed during inspection"))
	}
}
