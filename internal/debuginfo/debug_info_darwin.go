package debuginfo

import (
	"debug/gosym"
	"debug/macho"
	"fmt"

	"golang.org/x/sys/unix"
)

type darwinDebugInfo struct {
	SymTable  *gosym.Table
	LineTable *gosym.LineTable
	Target    Target
}

func NewDebugInfo(path string, pid int) (DebugInfo, error) {
	exe, err := macho.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open target Mach-O file: %v", err)
	}
	defer func() {
		if err := exe.Close(); err != nil {
			fmt.Printf("failed to close target Mach-O file: %v\n", err)
		}
	}()

	// Read line table (mapping between memory addresses and source file + line number)
	lineTableData, err := exe.Section("__gopclntab").Data()
	if err != nil {
		return nil, fmt.Errorf("failed to read Line Table data into memory: %v", err)
	}

	// Address where the machine code for the target starts
	textSection := exe.Section("__text")
	if textSection == nil {
		return nil, fmt.Errorf("failed to find __text section in Mach-O file")
	}
	addr := textSection.Addr

	// Create line table object for PCToLine and LineToPC translation
	lineTable := gosym.NewLineTable(lineTableData, addr)

	// Create symbol table object for looking up functions, variables and types
	symTable, err := gosym.NewTable([]byte{}, lineTable)
	if err != nil {
		return nil, fmt.Errorf("failed to create Symbol Table: %v", err)
	}

	// Get the path to the source file from the symbol table
	mainFunc := symTable.LookupFunc("main.main")
	if mainFunc == nil {
		return nil, fmt.Errorf("failed to find main.main function in symbol table")
	}
	sourceFile, _, _ := symTable.PCToLine(mainFunc.Entry)

	// Get process group ID for managing child processes
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		return nil, fmt.Errorf("error getting PGID: %v", err)
	}

	return &darwinDebugInfo{
		SymTable:  symTable,
		LineTable: lineTable,
		Target: Target{
			Path: sourceFile,
			PID:  pid,
			PGID: pgid,
		},
	}, nil
}

func (d *darwinDebugInfo) GetTarget() Target {
	return d.Target
}

func (d *darwinDebugInfo) LineToPC(file string, line int) (pc uint64, fn *gosym.Func, err error) {
	return d.SymTable.LineToPC(file, line)
}

func (d *darwinDebugInfo) LookupFunc(fn string) *gosym.Func {
	return d.SymTable.LookupFunc(fn)
}

func (d *darwinDebugInfo) PCToLine(pc uint64) (file string, line int, fn *gosym.Func) {
	return d.SymTable.PCToLine(pc)
}
