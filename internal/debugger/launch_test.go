package debugger

import (
	"math"
	"testing"
)

func TestAttachRejectsInvalidProcessIDsBeforeBackend(t *testing.T) {
	for _, pid := range []int{0, -1, math.MaxInt32 + 1} {
		p := process{}
		if err := p.attach(nil, pid); err == nil || p.live || p.pid != 0 {
			t.Fatalf("invalid pid %d reached the backend or changed ownership: %+v, %v", pid, p, err)
		}
	}
}
