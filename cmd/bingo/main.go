package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"

	"github.com/bingosuite/bingo/config"
	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/internal/debuginfo"
	websocket "github.com/bingosuite/bingo/internal/ws"
)

const (
	ptraceOExitKill = 0x100000 // Set option to kill the target process when Bingo exits to true
)

func main() {
	cfg, err := config.Load("config/config.yml")
	if err != nil {
		log.Printf("Failed to load config: %v, using defaults", err)
		cfg = config.Default()
	}

	server := websocket.NewServer(cfg.Server.Addr, &cfg.WebSocket)

	go func() {
		if err := server.Serve(); err != nil {
			log.Printf("WebSocket server error: %v", err)
			panic(err)
		}
	}()

	procName := os.Args[1]
	binLocation := fmt.Sprintf("/workspaces/bingo/build/target/%s", procName)

	// Set up target for execution
	cmd := exec.Command(binLocation)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}

	if err := cmd.Start(); err != nil {
		handleError("Failed to start target: %v", err)
	}

	// We want to catch the initial SIGTRAP sent by process creation. When this is caught, we know that the target just started and we can ask the user where they want to set their breakpoints
	// The message we print to the console will be removed in the future, it's just for debugging purposes for now.
	if err := cmd.Wait(); err != nil {
		log.Printf("Received SIGTRAP from process creation: %v", err)
	}

	db, err := debuginfo.NewDebugInfo(binLocation, cmd.Process.Pid)
	if err != nil {
		log.Printf("Failed to create debug info: %v", err)
		panic(err)
	}
	log.Printf("Started process with PID: %d and PGID: %d\n", db.Target.PID, db.Target.PGID)

	// Enable tracking threads spawned from target and killing target once Bingo exits
	if err := syscall.PtraceSetOptions(db.Target.PID, syscall.PTRACE_O_TRACECLONE|ptraceOExitKill); err != nil {
		handleError("Failed to set TRACECLONE and EXITKILL options on target: %v", err)
	}

	//TODO: client should send over what line we need to set breakpoint at, not hardcoded line 11
	pc, _, err := db.LineToPC(db.Target.Path, 11)
	if err != nil {
		handleError("Failed to get PC of line 11: %v", err)
	}

	if err := debugger.SetBreakpoint(db, pc); err != nil {
		handleError("Failed to set breakpoint: %v", err)
	}

	// Continue after the initial SIGTRAP
	// TODO: tell client to display the initial setup menu so the user can choose to set breakpoint, continue or single-step
	if err := syscall.PtraceCont(db.Target.PID, 0); err != nil {
		handleError("Failed to resume target execution: %v", err)
	}

	for {
		// Wait until any of the child processes of the target is interrupted or ends
		var waitStatus syscall.WaitStatus
		wpid, err := syscall.Wait4(-1*db.Target.PGID, &waitStatus, 0, nil) // TODO: handle concurrency
		if err != nil {
			handleError("Failed to wait for the target or any of its threads: %v", err)
		}

		if waitStatus.Exited() {
			if wpid == db.Target.PID { // If target exited, terminate
				log.Printf("Target %v execution completed", db.Target.Path)
				break
			} else {
				log.Printf("Thread exited with PID: %v", wpid)
			}
		} else {
			// Only stop on breakpoints caused by our debugger, ignore any other event like spawning of new threads
			if waitStatus.StopSignal() == syscall.SIGTRAP && waitStatus.TrapCause() != syscall.PTRACE_EVENT_CLONE {
				//TODO: improve error handling and messages and pull logic out to debugger package

				// Read registers
				var regs syscall.PtraceRegs
				if err := syscall.PtraceGetRegs(wpid, &regs); err != nil {
					handleError("Failed to get registers: %v", err)
				}
				filename, line, fn := db.PCToLine(regs.Rip - 1) // Breakpoint advances PC by 1 on x86, so we need to rewind
				fmt.Printf("Stopped at %s at %d in %s\n", fn.Name, line, filename)

				// Remove the breakpoint
				bpAddr := regs.Rip - 1
				if err := debugger.ClearBreakpoint(db, bpAddr); err != nil {
					handleError("Failed to clear breakpoint: %v", err)
				}
				regs.Rip = bpAddr

				// Rewind Rip by 1
				if err := syscall.PtraceSetRegs(wpid, &regs); err != nil {
					handleError("Failed to restore registers: %v", err)
				}

				// Step over the instruction we previously removed to put the breakpoint
				if err := syscall.PtraceSingleStep(wpid); err != nil {
					handleError("Failed to single-step: %v", err)
				}

				// TODO: only trigger for step over signal
				// Wait until the program lets us know we stepped over (handle cases where we get another signal which Wait4 would consume)
				if _, err := syscall.Wait4(wpid, &waitStatus, 0, nil); err != nil {
					handleError("Failed to wait for the single-step: %v", err)
				}

				// Put the breakpoint back
				if err := debugger.SetBreakpoint(db, bpAddr); err != nil {
					handleError("Failed to set breakpoint: %v", err)
				}

				// Resume execution
				if err := syscall.PtraceCont(wpid, 0); err != nil {
					handleError("Failed to resume target execution: %v", err)
				}

			} else {
				if err := syscall.PtraceCont(wpid, 0); err != nil {
					handleError("Failed to resume target execution: %v", err)
				}
			}
		}

	}

}

func handleError(msg string, err error) {
	log.Printf(msg, err)
	panic(err)

}
