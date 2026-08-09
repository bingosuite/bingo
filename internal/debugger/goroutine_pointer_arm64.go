//go:build arm64

package debugger

// Go reserves X28 for the current *g on arm64.
func (e *engine) archCurrentGoroutinePointer(regs Registers) (uint64, bool) {
	return regs.TLS, regs.TLS != 0
}

// Darwin stops identify a Mach thread port, while runtime.m.procid stores
// pthread_self. X28 provides the stopped g directly instead.
func (e *engine) archRuntimeMProcID(int) (uint64, bool) {
	return 0, false
}
