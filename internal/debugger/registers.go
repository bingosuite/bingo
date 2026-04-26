package debugger

// Registers is the architecture-independent register snapshot the engine uses.
//
//	amd64:  PC=RIP   SP=RSP   BP=RBP   TLS=FS_BASE
//	arm64:  PC=PC    SP=SP    BP=X29   TLS=X28
type Registers struct {
	PC  uint64
	SP  uint64
	BP  uint64
	TLS uint64 // goroutine pointer base (Go-specific)
}
