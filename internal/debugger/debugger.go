package debugger

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"

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
}

func NewDebugger() *Debugger {
	return &Debugger{
		Breakpoints:     make(map[uint64][]byte),
		EndDebugSession: make(chan bool, 1),
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

	/*
		// We want to catch the initial SIGTRAP sent by process creation. When this is caught, we know that the target just started and we can ask the user where they want to set their breakpoints
		// The message we print to the console will be removed in the future, it's just for debugging purposes for now.
		if err := cmd.Wait(); err != nil {
			log.Printf("Received SIGTRAP from process creation: %v", err)
		}*/

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

	log.Println("STARTING DEBUG LOOP")

	d.debug()

	// Wait until debug session exits
	d.EndDebugSession <- true

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
	d.EndDebugSession <- true
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
		// Wait until any of the child processes of the target is interrupted or ends
		var waitStatus syscall.WaitStatus
		wpid, err := syscall.Wait4(-1*d.DebugInfo.Target.PGID, &waitStatus, 0, nil) // TODO: handle concurrency
		if err != nil {
			log.Printf("Failed to wait for the target or any of its threads: %v", err)
			panic(err)
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

			} else {
				if err := syscall.PtraceCont(wpid, 0); err != nil {
					log.Printf("Failed to resume target execution: %v", err)
					panic(err)
				}
			}
		}
	}
}

func (d *Debugger) initialBreakpointHit() {
	// TODO: NUKE, forward necessary information to the server instead
	log.Println("INITIAL BREAKPOINT HIT")

	//TODO: tell server we hit the initial breakpoint and need to know what to do (continue, set bp, step over, quit)
	if err := d.SetBreakpoint(9); err != nil {
		log.Printf("Failed to set breakpoint: %v", err)
		panic(err)
	}
	// When initial breakpoint is hit, resume execution like this instead of d.Continue() after receiving continue from the server
	if err := syscall.PtraceCont(d.DebugInfo.Target.PID, 0); err != nil {
		log.Printf("Failed to resume target execution: %v", err)
		panic(err)
	}
}

func (d *Debugger) breakpointHit(pid int) {
	// TODO: NUKE, forward necessary information to the server instead
	log.Println("BREAKPOINT HIT")

	//TODO: select with channels from server that tell the debugger whether to continue, single step, set breakpoint or quite
	d.Continue(pid)
}
