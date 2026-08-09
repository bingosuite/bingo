//go:build arm64

package debugger

import "testing"

func TestArchCurrentGoroutinePointerArm64(t *testing.T) {
	const gptr = uint64(0x12345678)
	got, ok := (&engine{}).archCurrentGoroutinePointer(Registers{TLS: gptr})
	if !ok || got != gptr {
		t.Fatalf("current g pointer = 0x%x, %t; want 0x%x, true", got, ok, gptr)
	}
}

func TestArchRuntimeMProcIDArm64(t *testing.T) {
	got, ok := (&engine{}).archRuntimeMProcID(42)
	if ok || got != 0 {
		t.Fatalf("runtime m procid = %d, %t; want unavailable for Mach thread ports", got, ok)
	}
}
