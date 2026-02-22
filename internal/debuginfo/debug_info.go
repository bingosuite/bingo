package debuginfo

import (
	"debug/elf"
	"debug/gosym"
	"fmt"
	"syscall"
)

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
	lineTableSection := exe.Section(".gopclntab")
	if lineTableSection == nil {
		return nil, fmt.Errorf("missing .gopclntab section")
	}
	lineTableData, err := lineTableSection.Data()
	if err != nil {
		return nil, fmt.Errorf("failed to read Line Table data into memory: %v", err)
	}
	// Address where the machine code for the target starts
	textSection := exe.Section(".text")
	if textSection == nil {
		return nil, fmt.Errorf("missing .text section")
	}
	addr := textSection.Addr
	// Create line table object for PCToLine and LineToPC translation
	lineTable := gosym.NewLineTable(lineTableData, addr)
	// Create symbol table object for looking up functions, variables and types
	var symTableData []byte
	if symSection := exe.Section(".gosymtab"); symSection != nil {
		symTableData, err = symSection.Data()
		if err != nil {
			return nil, fmt.Errorf("failed to read Symbol Table data into memory: %v", err)
		}
	}
	symTable, err := gosym.NewTable(symTableData, lineTable)
	if err != nil {
		return nil, fmt.Errorf("failed to create Symbol Table: %v", err)
	}

	//Need to get this to dynamically get the path to the main source Go file (ex. target.exe's source might be called /workspaces/bingo/cmd/target/target.go or /workspaces/bingo/cmd/target/main.go)
	mainFunc := symTable.LookupFunc("main.main")
	if mainFunc == nil {
		return nil, fmt.Errorf("missing main.main symbol; build without -s -w and with debug info")
	}
	sourceFile, _, _ := symTable.PCToLine(mainFunc.Entry)

	// Need this to wait on threads
	pgid, err := syscall.Getpgid(pid)
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
