//go:build windows && amd64

package debugger

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsGetRegisters and windowsSetRegisters read/write the AMD64 CONTEXT
// structure via GetThreadContext / SetThreadContext.
//
// The AMD64 CONTEXT structure is 1232 bytes and must be 16-byte aligned.
// We use a raw byte buffer with manual alignment and read fields by offset.
//
// Verified offsets from winnt.h (AMD64 CONTEXT):
//
//	0x030  (48)   ContextFlags      uint32
//	0x098  (152)  Rsp               uint64
//	0x0A0  (160)  Rbp               uint64
//	0x0C4  (196)  EFlags            uint32
//	0x0F8  (248)  Rip               uint64
//
// CONTEXT_FULL = CONTEXT_AMD64 | CONTEXT_CONTROL | CONTEXT_INTEGER | CONTEXT_SEGMENTS
const (
	ctxAMD64   = 0x00100000
	ctxControl = ctxAMD64 | 0x1
	ctxInteger = ctxAMD64 | 0x2
	ctxFull    = ctxControl | ctxInteger | 0x8

	ctxSize      = 1232
	ctxFlagsOff  = 48
	ctxRspOff    = 152
	ctxRbpOff    = 160
	ctxEFlagsOff = 196
	ctxRipOff    = 248
)

func windowsGetRegisters(h windows.Handle) (Registers, error) {
	buf := alignedContextBuf()

	// Set ContextFlags before calling GetThreadContext.
	*(*uint32)(unsafe.Pointer(&buf[ctxFlagsOff])) = ctxFull

	r, _, e := procGetThreadContext.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 {
		return Registers{}, fmt.Errorf("GetThreadContext: %w", e)
	}

	return Registers{
		PC:     readU64(buf, ctxRipOff),
		SP:     readU64(buf, ctxRspOff),
		BP:     readU64(buf, ctxRbpOff),
		EFlags: readU32(buf, ctxEFlagsOff),
	}, nil
}

func windowsSetRegisters(h windows.Handle, reg Registers) error {
	buf := alignedContextBuf()
	*(*uint32)(unsafe.Pointer(&buf[ctxFlagsOff])) = ctxFull

	// Read the full context first so we preserve all fields we don't modify.
	r, _, e := procGetThreadContext.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 {
		return fmt.Errorf("GetThreadContext (pre-set): %w", e)
	}

	writeU64(buf, ctxRipOff, reg.PC)
	writeU64(buf, ctxRspOff, reg.SP)
	writeU64(buf, ctxRbpOff, reg.BP)
	writeU32(buf, ctxEFlagsOff, reg.EFlags)

	r, _, e = procSetThreadContext.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])))
	if r == 0 {
		return fmt.Errorf("SetThreadContext: %w", e)
	}
	return nil
}

// alignedContextBuf returns a ctxSize-byte slice whose first element is
// 16-byte aligned, as required by GetThreadContext / SetThreadContext.
func alignedContextBuf() []byte {
	// Allocate extra room so we can find a 16-byte aligned start.
	raw := make([]byte, ctxSize+16)
	off := 16 - (uintptr(unsafe.Pointer(&raw[0])) & 0xf)
	if off == 16 {
		off = 0
	}
	return raw[off : off+ctxSize]
}

func readU64(b []byte, off int) uint64 {
	p := b[off:]
	return uint64(p[0]) | uint64(p[1])<<8 | uint64(p[2])<<16 | uint64(p[3])<<24 |
		uint64(p[4])<<32 | uint64(p[5])<<40 | uint64(p[6])<<48 | uint64(p[7])<<56
}

func readU32(b []byte, off int) uint32 {
	p := b[off:]
	return uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24
}

func writeU64(b []byte, off int, v uint64) {
	b[off+0] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
	b[off+4] = byte(v >> 32)
	b[off+5] = byte(v >> 40)
	b[off+6] = byte(v >> 48)
	b[off+7] = byte(v >> 56)
}

func writeU32(b []byte, off int, v uint32) {
	b[off+0] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
}
