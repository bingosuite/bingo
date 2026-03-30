//go:build windows

package debugger

// Registers on Windows adds EFlags to the base set.
// SingleStep works by setting the Trap Flag (TF, bit 8) in EFLAGS before
// resuming the thread. The CPU delivers EXCEPTION_SINGLE_STEP after one
// instruction and the TF bit is automatically cleared.
// We include EFlags here so SetRegisters can write the TF bit atomically with
// the other registers via a single SetThreadContext call.
type Registers struct {
	PC     uint64
	SP     uint64
	BP     uint64
	TLS    uint64
	EFlags uint32
}
