package main

import (
	"debug/elf"
	"debug/gosym"
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"

	config "github.com/bingosuite/bingo/config"
	"github.com/bingosuite/bingo/internal/cli"
	websocket "github.com/bingosuite/bingo/internal/ws"
)

var (
	targetFile    string
	line          int
	pc            uint64
	fn            *gosym.Func
	symTable      *gosym.Table
	regs          syscall.PtraceRegs
	ws            syscall.WaitStatus
	originalCode  []byte
	breakpointSet bool
	interruptCode = []byte{0xCC}
)

func main() {
	cfg, err := config.Load("config/config.yml")
	if err != nil {
		log.Printf("Failed to load config: %v", err)
		panic(err)
	}

	server := websocket.NewServerWithConfig(cfg.Server.Addr, cfg.WebSocket)

	go func() {
		if err := server.Serve(); err != nil {
			log.Printf("WebSocket server error: %v", err)
			panic(err)
		}
	}()

	procName := os.Args[1]
	path := "/workspaces/bingo/build/target/%s"
	binLocation := fmt.Sprintf(path, procName)

	// Load Go symbol table from ELF
	symTable = getSymbolTable(binLocation)
	fn = symTable.LookupFunc("main.main")
	targetFile, line, fn = symTable.PCToLine(fn.Entry)
	run(binLocation)

}

func run(target string) {
	var filename string

	cmd := exec.Command(target)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}

	// Start the target
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting process: %v\n", err)
	}

	if err := cmd.Wait(); err != nil { // Will catch the SIGTRAP generated from starting a new process
		fmt.Fprintf(os.Stderr, "Wait returned: %v\n", err)
	}

	pid := cmd.Process.Pid
	// Need this to wait on threads
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting PGID: %v\n", err)
	}
	fmt.Printf("Starting process with PID: %d and PGID: %d\n", pid, pgid)
	//Enables thead tracking
	if err := syscall.PtraceSetOptions(pid, syscall.PTRACE_O_TRACECLONE); err != nil {
		panic(err)
	}

	cont := false
	cont, breakpointSet, originalCode, line = cli.Resume(pid, targetFile, line, breakpointSet, originalCode, setBreak)
	if cont {
		if err := syscall.PtraceCont(pid, 0); err != nil {
			panic(err)
		}
	} else {
		if err := syscall.PtraceSingleStep(pid); err != nil {
			panic(err)
		}
	}

	for {
		// Wait until next breakpoint
		wpid, err := syscall.Wait4(-1*pgid, &ws, 0, nil)
		if err != nil {
			panic(err)
		}

		if ws.Exited() {
			if wpid == pid {
				break
			}
		} else {
			//Tracing only if stopped by breakpoint we set. Cloning child process creates trap so we want to ignore it
			if ws.StopSignal() == syscall.SIGTRAP && ws.TrapCause() != syscall.PTRACE_EVENT_CLONE {
				if err := syscall.PtraceGetRegs(wpid, &regs); err != nil {
					panic(err)
				}
				filename, line, fn = symTable.PCToLine(regs.Rip) // TODO: chat says interrupt advances RIP by 1 so it should be -1, check if true
				fmt.Printf("Stopped at %s at %d in %s\n", fn.Name, line, filename)
				//outputStack(symTable, wpid, regs.Rip, regs.Rsp, regs.Rbp)

				if breakpointSet {
					// TODO: chat says should step past breakpoint instead. normally: restore instruction, step, reinsert breakpoint
					replaceCode(wpid, pc, originalCode)
					breakpointSet = false

				}

				cont, breakpointSet, originalCode, line = cli.Resume(wpid, targetFile, line, breakpointSet, originalCode, setBreak)
				if cont {
					if err := syscall.PtraceCont(wpid, 0); err != nil {
						panic(err)
					}
				} else {
					if err := syscall.PtraceSingleStep(wpid); err != nil {
						panic(err)
					}
				}
			} else {
				if err := syscall.PtraceCont(wpid, 0); err != nil {
					panic(err)
				}
			}
		}
	}
}

func setBreak(pid int, filename string, line int) (bool, []byte) {
	var err error

	// Map source (actual lines in the code) to the program counter
	pc, _, err = symTable.LineToPC(filename, line)
	if err != nil {
		fmt.Printf("Can't find breakpoint for %s, %d\n", filename, line)
		return false, []byte{}
	}

	return true, replaceCode(pid, pc, interruptCode)
}

func replaceCode(pid int, breakpoint uint64, code []byte) []byte {
	og := make([]byte, len(code))
	_, err := syscall.PtracePeekData(pid, uintptr(breakpoint), og) // Save old data at breakpoint
	if err != nil {
		panic(err)
	}
	_, err = syscall.PtracePokeData(pid, uintptr(breakpoint), code) // replace with interrupt code
	if err != nil {
		panic(err)
	}
	return og
}

func getSymbolTable(proc string) *gosym.Table {

	exe, err := elf.Open(proc)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := exe.Close(); err != nil {
			panic(err)
		}
	}()

	addr := exe.Section(".text").Addr

	lineTableData, err := exe.Section(".gopclntab").Data()
	if err != nil {
		panic(err)
	}
	lineTable := gosym.NewLineTable(lineTableData, addr)

	symTableData, err := exe.Section(".gosymtab").Data()
	if err != nil {
		panic(err)
	}

	symTable, err := gosym.NewTable(symTableData, lineTable)
	if err != nil {
		panic(err)
	}

	return symTable
}
