//go:build amd64

package debugger

import "testing"

func TestArchCurrentGoroutinePointerAMD64(t *testing.T) {
	got, ok := (&engine{}).archCurrentGoroutinePointer(Registers{TLS: 0x2000})
	if ok || got != 0 {
		t.Fatalf("current g pointer = 0x%x, %t; want unavailable so allm resolves it", got, ok)
	}
}

func TestArchRuntimeMProcIDAMD64(t *testing.T) {
	got, ok := (&engine{}).archRuntimeMProcID(42)
	if !ok || got != 42 {
		t.Fatalf("runtime m procid = %d, %t; want 42, true", got, ok)
	}
	if got, ok := (&engine{}).archRuntimeMProcID(0); ok || got != 0 {
		t.Fatalf("zero TID procid = %d, %t; want unavailable", got, ok)
	}
}
