package debugger

// Registers is the architecture-independent register snapshot the engine uses.
// It maps platform register names to logical roles:
//
//	amd64:  PC=RIP   SP=RSP   BP=RBP   TLS=FS_BASE
//	arm64:  PC=PC    SP=SP    BP=X29   TLS=X28
//
// Separating the two files avoids an EFlags field
// where it is meaningless and would waste space in every GetRegisters call.
type Registers struct {
	PC  uint64 // program counter
	SP  uint64 // stack pointer
	BP  uint64 // frame/base pointer
	TLS uint64 // goroutine pointer base (Go-specific, not a standard arch register)
}
