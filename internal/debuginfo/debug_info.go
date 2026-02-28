package debuginfo

import (
	"debug/gosym"
)

type Target struct {
	Path string
	PID  int
	PGID int
}

type DebugInfo interface {
	GetTarget() Target
	PCToLine(pc uint64) (file string, line int, fn *gosym.Func)
	LineToPC(file string, line int) (pc uint64, fn *gosym.Func, err error)
	LookupFunc(fn string) *gosym.Func
}
