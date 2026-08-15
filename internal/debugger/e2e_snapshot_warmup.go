//go:build e2e

package debugger

import "fmt"

type SnapshotDWARFWarmupState struct {
	FunctionIndexEntries int
	GoLayoutCached       bool
	RuntimeSymbols       map[string]uint64
}

// WarmSnapshotDWARFForE2E keeps image-only initialization outside native
// acceptance deadlines; tracee-memory reads still run on the real stop path.
func WarmSnapshotDWARFForE2E(d Debugger) (SnapshotDWARFWarmupState, error) {
	e, ok := d.(*engine)
	if !ok {
		return SnapshotDWARFWarmupState{}, fmt.Errorf("unsupported debugger type %T", d)
	}

	state := SnapshotDWARFWarmupState{
		RuntimeSymbols: make(map[string]uint64),
	}
	err := e.dispatch(func() error {
		if e.dw == nil {
			return fmt.Errorf("no DWARF info")
		}
		e.dw.funcIndexOnce.Do(e.dw.buildFuncIndex)
		_, state.GoLayoutCached = e.getGoLayout()
		for name, addr := range e.dw.runtimeVarAddrs(
			"runtime.allglen",
			"runtime.allgptr",
			"runtime.allgs",
		) {
			state.RuntimeSymbols[name] = addr
		}
		if addr, found := e.dw.runtimeVarAddr("runtime.allm"); found {
			state.RuntimeSymbols["runtime.allm"] = addr
		}
		state.FunctionIndexEntries = len(e.dw.funcIndex)
		return nil
	})
	return state, err
}
