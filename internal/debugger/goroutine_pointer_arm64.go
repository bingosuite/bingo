//go:build arm64

package debugger

// Go reserves X28 for the current *g on arm64.
func (e *engine) archCurrentGoroutinePointer(regs Registers) (uint64, bool) {
	return regs.TLS, regs.TLS != 0
}
