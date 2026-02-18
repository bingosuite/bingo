package debugger

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/bingosuite/bingo/internal/debuginfo"
)

var (
	bpCode = []byte{0xCC}
)

const (
	ptraceOExitKill = 0x100000 // Set option to kill the target process when Bingo exits to true
)

type Debugger struct {
	DebugInfo       debuginfo.DebugInfo
	Breakpoints     map[uint64][]byte
	EndDebugSession chan bool

	// Communication with hub
	BreakpointHit        chan BreakpointEvent
	InitialBreakpointHit chan InitialBreakpointEvent
	DebugCommand         chan DebugCommand
}

// BreakpointEvent represents a breakpoint hit event
type BreakpointEvent struct {
	PID      int    `json:"pid"`
	Filename string `json:"filename"`
	Line     int    `json:"line"`
	Function string `json:"function"`
}

// InitialBreakpointEvent represents the initial breakpoint hit when debugging starts
type InitialBreakpointEvent struct {
	PID int `json:"pid"`
}

// DebugCommand represents commands that can be sent to the debugger
type DebugCommand struct {
	Type string      `json:"type"` // "continue", "step", "quit", "setBreakpoint"
	Data interface{} `json:"data,omitempty"`
}

func NewDebugger() *Debugger {
	return &Debugger{
		Breakpoints:          make(map[uint64][]byte),
		EndDebugSession:      make(chan bool, 1),
		BreakpointHit:        make(chan BreakpointEvent, 1),
		InitialBreakpointHit: make(chan InitialBreakpointEvent, 1),
		DebugCommand:         make(chan DebugCommand, 1),
	}
}

func (d *Debugger) StartWithDebug(path string) {

	// Set up target for execution
	cmd := exec.Command(path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start target: %v", err)
		panic(err)
	}

	dbInf, err := debuginfo.NewDebugInfo(path, cmd.Process.Pid)
	if err != nil {
		log.Printf("Failed to create debug info: %v", err)
		panic(err)
	}
	log.Printf("Started process with PID: %d and PGID: %d\n", dbInf.Target.PID, dbInf.Target.PGID)

	// Enable tracking threads spawned from target and killing target once Bingo exits
	if err := syscall.PtraceSetOptions(dbInf.Target.PID, syscall.PTRACE_O_TRACECLONE|ptraceOExitKill); err != nil {
		log.Printf("Failed to set TRACECLONE and EXITKILL options on target: %v", err)
		panic(err)
	}

	d.DebugInfo = *dbInf

	// We want to catch the initial SIGTRAP sent by process creation. When this is caught, we know that the target just started and we can ask the user where they want to set their breakpoints
	// The message we print to the console will be removed in the future, it's just for debugging purposes for now.
	if err := cmd.Wait(); err != nil {
		log.Printf("Received SIGTRAP from process creation: %v", err)
	}

	// Set initial breakpoints while the process is stopped at the initial SIGTRAP
	d.initialBreakpointHit()

	// Check if we were stopped during initial breakpoint
	select {
	case <-d.EndDebugSession:
		log.Println("Debug session ended during initial breakpoint, cleaning up")
		return
	default:
		// Continue to debug loop
	}

	log.Println("STARTING DEBUG LOOP")

	d.debug()

	log.Println("Debug loop ended, signaling completion")

}

// TODO: figure out how to do
func (d *Debugger) AttachAndDebug(pid int) {

}

func (d *Debugger) Continue(pid int) {
	// Read registers
	var regs syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(pid, &regs); err != nil {
		log.Printf("Failed to get registers: %v", err)
		return // Process likely exited, gracefully return
	}
	filename, line, fn := d.DebugInfo.PCToLine(regs.Rip - 1) // Breakpoint advances PC by 1 on x86, so we need to rewind
	fmt.Printf("Stopped at %s at %d in %s\n", fn.Name, line, filename)

	// Remove the breakpoint
	bpAddr := regs.Rip - 1
	if err := d.ClearBreakpoint(line); err != nil {
		log.Printf("Failed to clear breakpoint: %v", err)
		panic(err)
	}
	regs.Rip = bpAddr

	// Rewind Rip by 1
	if err := syscall.PtraceSetRegs(pid, &regs); err != nil {
		log.Printf("Failed to restore registers: %v", err)
		panic(err)
	}

	// Step over the instruction we previously removed to put the breakpoint
	// TODO: decide if we want to call debugger.SingleStep() for this or just the system call
	if err := syscall.PtraceSingleStep(pid); err != nil {
		log.Printf("Failed to single-step: %v", err)
		panic(err)
	}

	// TODO: only trigger for step over signal
	var waitStatus syscall.WaitStatus
	// Wait until the program lets us know we stepped over (handle cases where we get another signal which Wait4 would consume)
	if _, err := syscall.Wait4(pid, &waitStatus, 0, nil); err != nil {
		log.Printf("Failed to wait for the single-step: %v", err)
		panic(err)
	}

	// Put the breakpoint back
	if err := d.SetBreakpoint(line); err != nil {
		log.Printf("Failed to set breakpoint: %v", err)
		panic(err)
	}

	// Resume execution
	// TODO: decide if we want to call debugger.Continue() for this or just the system call
	if err := syscall.PtraceCont(pid, 0); err != nil {
		log.Printf("Failed to resume target execution: %v", err)
		panic(err)
	}

}

func (d *Debugger) SingleStep(pid int) {

	if err := syscall.PtraceSingleStep(pid); err != nil {
		log.Printf("Failed to single-step: %v", err)
		panic(err)
	}

}

func (d *Debugger) StopDebug() {
	// Detach from the target process, letting it continue running
	if d.DebugInfo.Target.PID > 0 {
		log.Printf("Detaching from target process (PID: %d)", d.DebugInfo.Target.PID)
		if err := syscall.PtraceDetach(d.DebugInfo.Target.PID); err != nil {
			log.Printf("Failed to detach from target process: %v (might have already exited)", err)
		}
	}
	// Signal the debug loop to exit
	select {
	case d.EndDebugSession <- true:
	default:
		// Channel might be full, that's ok
	}
}

func (d *Debugger) SetBreakpoint(line int) error {

	pc, _, err := d.DebugInfo.LineToPC(d.DebugInfo.Target.Path, line)
	if err != nil {
		log.Printf("Failed to get PC of line %v: %v", line, err)
		panic(err)
	}

	original := make([]byte, len(bpCode))
	if _, err := syscall.PtracePeekData(d.DebugInfo.Target.PID, uintptr(pc), original); err != nil {
		return fmt.Errorf("failed to read original machine code into memory: %v", err)
	}
	if _, err := syscall.PtracePokeData(d.DebugInfo.Target.PID, uintptr(pc), bpCode); err != nil {
		return fmt.Errorf("failed to write breakpoint into memory: %v", err)
	}
	d.Breakpoints[pc] = original
	return nil
}

func (d *Debugger) ClearBreakpoint(line int) error {

	pc, _, err := d.DebugInfo.LineToPC(d.DebugInfo.Target.Path, line)
	if err != nil {
		log.Printf("Failed to get PC of line %v: %v", line, err)
		panic(err)
	}
	if _, err := syscall.PtracePokeData(d.DebugInfo.Target.PID, uintptr(pc), d.Breakpoints[pc]); err != nil {
		return fmt.Errorf("failed to write breakpoint into memory: %v", err)
	}
	return nil
}

func (d *Debugger) debug() {
	for {
		// Check if we should stop debugging
		select {
		case <-d.EndDebugSession:
			log.Println("Debug session ending, exiting debug loop")
			return
		default:
			// Continue with wait
		}

		// Wait until any of the child processes of the target is interrupted or ends
		var waitStatus syscall.WaitStatus
		wpid, err := syscall.Wait4(-1*d.DebugInfo.Target.PGID, &waitStatus, syscall.WNOHANG, nil)
		if err != nil {
			log.Printf("Failed to wait for the target or any of its threads: %v", err)
			// Don't panic, just exit gracefully
			return
		}

		// No process state changed yet
		if wpid == 0 {
			// Sleep briefly to avoid busy waiting
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if waitStatus.Exited() {
			if wpid == d.DebugInfo.Target.PID { // If target exited, terminate
				log.Printf("Target %v execution completed", d.DebugInfo.Target.Path)
				break
			} else {
				log.Printf("Thread exited with PID: %v", wpid)
			}
		} else {
			// Only stop on breakpoints caused by our debugger, ignore any other event like spawning of new threads
			if waitStatus.StopSignal() == syscall.SIGTRAP && waitStatus.TrapCause() != syscall.PTRACE_EVENT_CLONE {
				//TODO: improve error handling and messages

				d.breakpointHit(wpid)

				// Check if we were signaled to stop during breakpoint handling
				select {
				case <-d.EndDebugSession:
					log.Println("Debug session ending after breakpoint handling")
					return
				default:
				}

			} else {
				if err := syscall.PtraceCont(wpid, 0); err != nil {
					log.Printf("Failed to resume target execution: %v", err)
					// Don't panic, might have been detached
					return
				}
			}
		}
	}
}

func (d *Debugger) initialBreakpointHit() {
	// Create initial breakpoint event
	event := InitialBreakpointEvent{
		PID: d.DebugInfo.Target.PID,
	}

	// Send initial breakpoint hit event to hub
	log.Println("Initial breakpoint hit, debugger ready for commands")
	d.InitialBreakpointHit <- event

	// Wait for commands from hub (typically to set breakpoints)
	for {
		select {
		case cmd := <-d.DebugCommand:
			log.Printf("Initial state - received command: %s", cmd.Type)
			switch cmd.Type {
			case "setBreakpoint":
				if data, ok := cmd.Data.(map[string]interface{}); ok {
					if line, ok := data["line"].(int); ok { // JSON numbers are float64
						if err := d.SetBreakpoint(int(line)); err != nil {
							log.Printf("Failed to set breakpoint at line %d: %v", int(line), err)
						} else {
							log.Printf("Breakpoint set at line %d, waiting for next command", int(line))
						}
					} else {
						log.Printf("Invalid setBreakpoint command: line field missing or wrong type")
					}
				} else {
					log.Printf("Invalid setBreakpoint command: data field missing or wrong format")
				}
			case "continue":
				log.Println("Continuing from initial breakpoint")
				if err := syscall.PtraceCont(d.DebugInfo.Target.PID, 0); err != nil {
					log.Printf("Failed to resume target execution: %v", err)
					panic(err)
				}
				return // Exit initial breakpoint handling
			case "step":
				log.Println("Stepping from initial breakpoint")
				d.SingleStep(d.DebugInfo.Target.PID)
				return // Exit initial breakpoint handling
			case "quit":
				d.StopDebug()
				return
			default:
				log.Printf("Unknown command during initial breakpoint: %s", cmd.Type)
			}
		case <-d.EndDebugSession:
			log.Println("Debug session ending during initial breakpoint")
			return
		}
	}
}

func (d *Debugger) breakpointHit(pid int) {
	// Get register information to determine location
	var regs syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(pid, &regs); err != nil {
		log.Printf("Failed to get registers: %v", err)
		return
	}

	// Get location information
	filename, line, fn := d.DebugInfo.PCToLine(regs.Rip - 1)

	// Create breakpoint event
	event := BreakpointEvent{
		PID:      pid,
		Filename: filename,
		Line:     line,
		Function: fn.Name,
	}

	// Send breakpoint hit event to hub
	log.Printf("Breakpoint hit at %s:%d in %s, waiting for command", filename, line, fn.Name)
	d.BreakpointHit <- event

	// Wait for command from hub
	select {
	case cmd := <-d.DebugCommand:
		log.Printf("Received command: %s", cmd.Type)
		switch cmd.Type {
		case "continue":
			d.Continue(pid)
		case "step":
			d.SingleStep(pid)
		case "setBreakpoint":
			if data, ok := cmd.Data.(map[string]interface{}); ok {
				if line, ok := data["line"].(float64); ok { // JSON numbers are float64
					if err := d.SetBreakpoint(int(line)); err != nil {
						log.Printf("Failed to set breakpoint at line %d: %v", int(line), err)
					} else {
						log.Printf("Breakpoint set at line %d while at breakpoint", int(line))
					}
				}
			}
			// After setting breakpoint, continue waiting for next command
			d.Continue(pid)
		case "quit":
			d.StopDebug()
			return
		default:
			log.Printf("Unknown command: %s", cmd.Type)
			d.Continue(pid) // Default to continue
		}
	case <-d.EndDebugSession:
		log.Println("Debug session ending, stopping breakpoint handler")
		return
	}
}
