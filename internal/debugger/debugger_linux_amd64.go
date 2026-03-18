package debugger

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/bingosuite/bingo/internal/debuginfo"

	"golang.org/x/sys/unix"
)

var (
	bpCode = []byte{0xCC}
)

const (
	logPrefixDebugger = "[DBG]"

	ptraceOExitKill = 0x100000 // Set option to kill the target process when Bingo exits to true
)

type linuxAMD64Debugger struct {
	DebugInfo       debuginfo.DebugInfo
	Breakpoints     map[uint64][]byte
	EndDebugSession chan bool

	// Communication with hub
	BreakpointHit        chan BreakpointEvent
	InitialBreakpointHit chan InitialBreakpointHitEvent
	DebugCommand         chan DebugCommand
}

func NewDebugger(breakpointHit chan BreakpointEvent, initialBreakpointHit chan InitialBreakpointHitEvent, debugCommand chan DebugCommand, endDebugSession chan bool) Debugger {
	return &linuxAMD64Debugger{
		Breakpoints:          make(map[uint64][]byte),
		EndDebugSession:      endDebugSession,
		BreakpointHit:        breakpointHit,
		InitialBreakpointHit: initialBreakpointHit,
		DebugCommand:         debugCommand,
	}
}

// validateTargetPath resolves path to an absolute path, ensures it stays
// within the current working directory, and confirms it is a regular
// executable file. This prevents command injection from user-supplied input.
func validateTargetPath(path string) (string, error) {
	// Resolve to absolute path to eliminate any relative traversal tricks
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("invalid target path: %w", err)
	}

	// Restrict execution to paths within the working directory
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("could not determine working directory: %w", err)
	}
	if !strings.HasPrefix(abs, cwd+string(filepath.Separator)) {
		return "", fmt.Errorf("target path %q is outside the working directory %q", abs, cwd)
	}

	// Confirm it exists and is a regular file (not a directory or device)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("target path %q not accessible: %w", abs, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("target path %q is not a regular file", abs)
	}
	if info.Mode()&0111 == 0 {
		return "", fmt.Errorf("target path %q is not executable", abs)
	}

	return abs, nil
}

func (d *linuxAMD64Debugger) StartWithDebug(path string) {
	// Lock this goroutine to the current OS thread.
	// Linux ptrace requires that all ptrace calls for a given traced process originate from the same OS thread that performed the initial attach.
	// Without this, the Go scheduler may migrate the goroutine to a different OS thread, causing ptrace calls to fail with ESRCH ("no such process").
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Validate and sanitise the user-supplied path before passing it to exec.
	validatedPath, err := validateTargetPath(path)
	if err != nil {
		log.Printf("%s Rejected target path %q: %v", logPrefixDebugger, path, err)
		panic(err)
	}

	// Set up target for execution
	cmd := exec.Command(validatedPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &unix.SysProcAttr{Ptrace: true}

	if err := cmd.Start(); err != nil {
		log.Printf("%s Failed to start target: %v", logPrefixDebugger, err)
		panic(err)
	}

	dbInf, err := debuginfo.NewDebugInfo(validatedPath, cmd.Process.Pid)
	if err != nil {
		log.Printf("%s Failed to create debug info: %v", logPrefixDebugger, err)
		panic(err)
	}
	log.Printf("%s Started process with PID: %d and PGID: %d\n", logPrefixDebugger, dbInf.GetTarget().PID, dbInf.GetTarget().PGID)

	// Enable tracking threads spawned from target and killing target once Bingo exits
	if err := unix.PtraceSetOptions(dbInf.GetTarget().PID, unix.PTRACE_O_TRACECLONE|ptraceOExitKill); err != nil {
		log.Printf("%s Failed to set TRACECLONE and EXITKILL options on target: %v", logPrefixDebugger, err)
		panic(err)
	}

	d.DebugInfo = dbInf

	// We want to catch the initial SIGTRAP sent by process creation. When this is caught, we know that the target just started and we can ask the user where they want to set their breakpoints
	// The message we print to the console will be removed in the future, it's just for debugging purposes for now.

	var waitStatus unix.WaitStatus
	if _, status := unix.Wait4(d.DebugInfo.GetTarget().PID, &waitStatus, 0, nil); status != nil {
		log.Printf("%s Received SIGTRAP from process creation: %v", logPrefixDebugger, status)
	}

	// Set initial breakpoints while the process is stopped at the initial SIGTRAP
	d.initialBreakpointHit()

	// Check if we were stopped during initial breakpoint
	select {
	case <-d.EndDebugSession:
		log.Printf("%s Debug session ended during initial breakpoint, cleaning up", logPrefixDebugger)
		return
	default:
		// Continue to debug loop
	}

	log.Printf("%s STARTING DEBUG LOOP", logPrefixDebugger)

	d.mainDebugLoop()

	log.Printf("%s Debug loop ended", logPrefixDebugger)

}

func (d *linuxAMD64Debugger) Continue(pid int) {
	// Read registers
	var regs unix.PtraceRegs
	if err := unix.PtraceGetRegs(pid, &regs); err != nil {
		log.Printf("%s Failed to get registers: %v", logPrefixDebugger, err)
		panic(err)
	}
	_, line, _ := d.DebugInfo.PCToLine(regs.Rip - 1) // Breakpoint advances PC by 1 on x86, so we need to rewind
	//fmt.Printf("[Debugger] Stopped at %s at %d in %s\n", fn.Name, line, filename)

	// Remove the breakpoint
	bpAddr := regs.Rip - 1
	if err := d.ClearBreakpoint(pid, line); err != nil {
		log.Printf("%s Failed to clear breakpoint: %v", logPrefixDebugger, err)
		panic(err)
	}
	regs.Rip = bpAddr

	// Rewind Rip by 1
	if err := unix.PtraceSetRegs(pid, &regs); err != nil {
		log.Printf("%s Failed to restore registers: %v", logPrefixDebugger, err)
		panic(err)
	}

	// Step over the instruction we previously removed to put the breakpoint
	// TODO: decide if we want to call debugger.SingleStep() for this or just the system
	if err := unix.PtraceSingleStep(pid); err != nil {
		log.Printf("%s Failed to single-step: %v", logPrefixDebugger, err)
		panic(err)
	}

	// TODO: only trigger for step over signal
	var waitStatus unix.WaitStatus
	// Wait until the program lets us know we stepped over (handle cases where we get another signal which Wait4 would consume)
	if _, err := unix.Wait4(pid, &waitStatus, 0, nil); err != nil {
		log.Printf("%s Failed to wait for the single-step: %v", logPrefixDebugger, err)
		panic(err)
	}

	// Put the breakpoint back
	if err := d.SetBreakpoint(pid, line); err != nil {
		log.Printf("%s Failed to set breakpoint: %v", logPrefixDebugger, err)
		panic(err)
	}

	// Resume execution
	// TODO: decide if we want to call debugger.Continue() for this or just the system call
	if err := unix.PtraceCont(pid, 0); err != nil {
		log.Printf("%s Failed to resume target execution: %v", logPrefixDebugger, err)
		panic(err)
	}

}

func (d *linuxAMD64Debugger) SingleStep(pid int) {

	if err := unix.PtraceSingleStep(pid); err != nil {
		log.Printf("%s Failed to single-step: %v", logPrefixDebugger, err)
		panic(err)
	}

}

func (d *linuxAMD64Debugger) StepOver(pid int) {
	// TODO
}

func (d *linuxAMD64Debugger) StopDebug() {
	// Detach from the target process, letting it continue running
	if d.DebugInfo.GetTarget().PID > 0 {
		log.Printf("%s Detaching from target process (PID: %d)", logPrefixDebugger, d.DebugInfo.GetTarget().PID)
		if err := unix.PtraceDetach(d.DebugInfo.GetTarget().PID); err != nil {
			log.Printf("%s Failed to detach from target process: %v (might have already exited)", logPrefixDebugger, err)
			panic(err)
		}
	}
	// Signal the debug loop to exit
	select {
	case d.EndDebugSession <- true:
	default:
		// Channel might be full, meaning debug session end already triggered
	}
}

func (d *linuxAMD64Debugger) SetBreakpoint(pid int, line int) error {

	pc, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return fmt.Errorf("failed to get PC of line %v: %v", line, err)
	}

	original := make([]byte, len(bpCode))
	if _, err := unix.PtracePeekData(pid, uintptr(pc), original); err != nil {
		return fmt.Errorf("failed to read original machine code into memory: %v for PID: %d", err, pid)
	}
	if _, err := unix.PtracePokeData(pid, uintptr(pc), bpCode); err != nil {
		return fmt.Errorf("failed to write breakpoint into memory: %v", err)
	}
	d.Breakpoints[pc] = original
	return nil
}

func (d *linuxAMD64Debugger) ClearBreakpoint(pid int, line int) error {

	pc, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return fmt.Errorf("failed to get PC of line %v: %v", line, err)
	}
	if _, err := unix.PtracePokeData(pid, uintptr(pc), d.Breakpoints[pc]); err != nil {
		return fmt.Errorf("failed to write breakpoint into memory: %v", err)
	}
	return nil
}

// TODO: pass the correct pid to the debugger methods, keep an eye on this
func (d *linuxAMD64Debugger) mainDebugLoop() {
	for {
		// Check if we should stop debugging
		select {
		case <-d.EndDebugSession:
			log.Printf("%s Debug session ending, exiting debug loop", logPrefixDebugger)
			return
		default:
			// Continue with wait
		}

		// Wait until any of the child processes of the target is interrupted or ends
		var waitStatus unix.WaitStatus
		wpid, err := unix.Wait4(-1*d.DebugInfo.GetTarget().PGID, &waitStatus, unix.WNOHANG, nil)
		if err != nil {
			log.Printf("%s Failed to wait for the target or any of its threads: %v", logPrefixDebugger, err)
			// Don't panic, just exit gracefully
			return
		}

		// TODO: change 10ms polling approach to goroutine
		if wpid == 0 { // if no process state changed, sleep briefly to avoid busy waiting and consuming 100% cpu
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if waitStatus.Exited() {
			if wpid == d.DebugInfo.GetTarget().PID { // If target exited, terminate
				log.Printf("%s Target %v execution completed", logPrefixDebugger, d.DebugInfo.GetTarget().Path)
				// Signal the end of debug session to hub
				select {
				case d.EndDebugSession <- true:
				default:
					// Channel might be full, meaning debug session end already triggered
				}
				return
			} else {
				log.Printf("%s Thread exited with PID: %v", logPrefixDebugger, wpid)
			}
		} else {
			// Only stop on breakpoints caused by our debugger, ignore any other event like spawning of new threads
			if waitStatus.StopSignal() == unix.SIGTRAP && waitStatus.TrapCause() != unix.PTRACE_EVENT_CLONE {
				//TODO: improve error handling and messages

				d.breakpointHit(wpid)

			} else {
				if err := unix.PtraceCont(wpid, 0); err != nil {
					log.Printf("%s Failed to resume target execution: %v for PID: %d", logPrefixDebugger, err, wpid)
					// Don't panic, might have been detached
					return
				}
			}
		}
	}
}

// TODO: maybe refactor later
func (d *linuxAMD64Debugger) initialBreakpointHit() {
	// Create initial breakpoint event
	event := InitialBreakpointHitEvent{
		PID: d.DebugInfo.GetTarget().PID,
	}

	// Send initial breakpoint hit event to hub
	log.Printf("%s Initial breakpoint hit, debugger ready for commands", logPrefixDebugger)
	d.InitialBreakpointHit <- event

	// Wait for commands from hub (typically to set breakpoints)
	for {
		select {
		case cmd := <-d.DebugCommand:
			log.Printf("%s Received command: %s", logPrefixDebugger, cmd.Type)

			switch cmd.Type {
			case "setBreakpoint":
				if data, ok := cmd.Data.(map[string]any); ok {
					if line, ok := data["line"].(int); ok {
						if err := d.SetBreakpoint(d.DebugInfo.GetTarget().PID, int(line)); err != nil {
							log.Printf("%s Failed to set breakpoint at line %d: %v", logPrefixDebugger, int(line), err)
							panic(err)
						} else {
							log.Printf("%s Breakpoint set at line %d while at breakpoint", logPrefixDebugger, int(line))
						}
					}
				}
			case "continue":
				log.Printf("%s Continuing from initial breakpoint", logPrefixDebugger)
				if err := unix.PtraceCont(d.DebugInfo.GetTarget().PID, 0); err != nil {
					log.Printf("%s Failed to resume target execution: %v", logPrefixDebugger, err)
					panic(err)
				}
				return // Exit initial breakpoint handling
			case "stepOver":
				log.Printf("%s Cannot stepover from initial breakpoint", logPrefixDebugger)
			case "singleStep":
				log.Printf("%s Cannot single-step from initial breakpoint", logPrefixDebugger)
			case "quit":
				d.StopDebug()
				return
			default:
				log.Printf("%s Unknown command during initial breakpoint: %s", logPrefixDebugger, cmd.Type)
			}
		case <-d.EndDebugSession:
			log.Printf("%s Debug session ending during initial breakpoint", logPrefixDebugger)
			return
		}
	}
}

func (d *linuxAMD64Debugger) breakpointHit(pid int) {
	// Get register information to determine location
	var regs unix.PtraceRegs
	if err := unix.PtraceGetRegs(pid, &regs); err != nil {
		log.Printf("%s Failed to get registers: %v", logPrefixDebugger, err)
		panic(err)
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
	log.Printf("%s Breakpoint hit at %s:%d in %s, waiting for command", logPrefixDebugger, filename, line, fn.Name)
	d.BreakpointHit <- event

	// Wait for command from hub
	select {
	case cmd := <-d.DebugCommand:
		log.Printf("%s Received command: %s", logPrefixDebugger, cmd.Type)
		switch cmd.Type {
		case "continue":
			d.Continue(pid)
		case "stepOver":
			d.StepOver(pid)
		case "singleStep":
			d.SingleStep(pid)
		case "setBreakpoint":
			if data, ok := cmd.Data.(map[string]any); ok {
				if line, ok := data["line"].(int); ok {
					if err := d.SetBreakpoint(pid, int(line)); err != nil {
						log.Printf("%s Failed to set breakpoint at line %d: %v", logPrefixDebugger, int(line), err)
					} else {
						log.Printf("%s Breakpoint set at line %d while at breakpoint", logPrefixDebugger, int(line))
					}
				}
			}
		case "quit":
			d.StopDebug()
			return
		default:
			log.Printf("%s Unknown command: %s", logPrefixDebugger, cmd.Type)
			d.Continue(pid) // Default to continue
		}
	case <-d.EndDebugSession:
		log.Printf("%s Debug session ending, stopping breakpoint handler", logPrefixDebugger)
		return
	}
}
