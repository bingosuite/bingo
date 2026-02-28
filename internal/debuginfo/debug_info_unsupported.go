//go:build !linux

package debuginfo

import (
	"debug/gosym"
	"fmt"
)

type unsupportedDebugInfo struct {
	target Target
}

func NewDebugInfo(path string, pid int) (DebugInfo, error) {
	return nil, fmt.Errorf("bingo: debug info is not supported on this platform")
}

func (u *unsupportedDebugInfo) GetTarget() Target {
	return u.target
}

func (u *unsupportedDebugInfo) PCToLine(pc uint64) (string, int, *gosym.Func) {
	panic("bingo: debug info is not supported on this platform")
}

func (u *unsupportedDebugInfo) LineToPC(file string, line int) (uint64, *gosym.Func, error) {
	return 0, nil, fmt.Errorf("bingo: debug info is not supported on this platform")
}

func (u *unsupportedDebugInfo) LookupFunc(fn string) *gosym.Func {
	panic("bingo: debug info is not supported on this platform")
}
