//go:build amd64

package debugger

// FS_BASE is not a stable current-g address under external linking because the
// ELF TLS slot can move. The shared reader resolves linux current-g through
// runtime.allm instead.
func (e *engine) archCurrentGoroutinePointer(regs Registers) (uint64, bool) {
	return 0, false
}

// Linux reports kernel TIDs in both ptrace stops and runtime.m.procid.
func (e *engine) archRuntimeMProcID(tid int) (uint64, bool) {
	if tid <= 0 {
		return 0, false
	}
	return uint64(tid), true
}
