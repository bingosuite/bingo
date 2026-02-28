package debuginfo

import (
	"debug/elf"
	"debug/gosym"
	"fmt"

	"golang.org/x/sys/unix"
)

// TODO: maybe put all of this under the debugger package. either way will need to refactor to account for cross-compilation

type Target struct {
	Path string
	PID  int
	PGID int
}

type DebugInfo struct {
	SymTable  *gosym.Table
	LineTable *gosym.LineTable
	Target    Target
}

func NewDebugInfo(path string, pid int) (*DebugInfo, error) {
	exe, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open target ELF file: %v", err)
	}
	defer func() {
		if err := exe.Close(); err != nil {
			fmt.Printf("failed to close target ELF file: %v\n", err)
		}
	}()
	// Read line table (mapping between memory addresses and source file + line number)
	lineTableData, err := exe.Section(".gopclntab").Data()
	if err != nil {
		return nil, fmt.Errorf("failed to read Line Table data into memory: %v", err)
	}
	// Address where the machine code for the target starts
	addr := exe.Section(".text").Addr
	// Create line table object for PCToLine and LineToPC translation
	lineTable := gosym.NewLineTable(lineTableData, addr)
	// Create symbol table object for looking up functions, variables and types
	symTable, err := gosym.NewTable([]byte{}, lineTable)
	if err != nil {
		return nil, fmt.Errorf("failed to create Symbol Table: %v", err)
	}

	//Need to get this to dynamically get the path to the main source Go file (ex. target.exe's source might be called /workspaces/bingo/cmd/target/target.go or /workspaces/bingo/cmd/target/main.go)
	sourceFile, _, _ := symTable.PCToLine(symTable.LookupFunc("main.main").Entry)

	// Need this to wait on threads
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		return nil, fmt.Errorf("error getting PGID: %v", err)
	}

	return &DebugInfo{
		SymTable:  symTable,
		LineTable: lineTable,
		Target: Target{
			Path: sourceFile, PID: pid, PGID: pgid,
		},
	}, nil
}

func (d *DebugInfo) PCToLine(pc uint64) (file string, line int, fn *gosym.Func) {
	return d.SymTable.PCToLine(pc)
}

func (d *DebugInfo) LineToPC(file string, line int) (pc uint64, fn *gosym.Func, err error) {
	return d.SymTable.LineToPC(file, line)
}

func (d *DebugInfo) LookupFunc(fn string) *gosym.Func {
	return d.SymTable.LookupFunc(fn)
}
