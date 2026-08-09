//go:build amd64

package debugger

import "testing"

func TestArchCurrentGoroutinePointerAMD64(t *testing.T) {
	got, ok := (&engine{}).archCurrentGoroutinePointer(Registers{TLS: 0x2000})
	if ok || got != 0 {
		t.Fatalf("current g pointer = 0x%x, %t; want unavailable so allm resolves it", got, ok)
	}
}
