package debugger

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/bingosuite/bingo/internal/debuginfo"

	"golang.org/x/sys/unix"
)

var (
	// ARM64 breakpoint instruction (BRK #0) - 4 bytes
	bpCode = []byte{0x00, 0x00, 0x20, 0xd4}
)

// Darwin ptrace request types
const (
	PT_TRACE_ME  = 0  // Child declares it's being traced
	PT_READ_I    = 1  // Read word in child's I space
	PT_READ_D    = 2  // Read word in child's D space
	PT_WRITE_I   = 4  // Write word in child's I space
	PT_WRITE_D   = 5  // Write word in child's D space
	PT_CONTINUE  = 7  // Continue the child
	PT_STEP      = 9  // Single step the child
	PT_DETACH    = 11 // Stop tracing a process
	PT_ATTACHEXC = 14 // Attach to running process with signal exception
	PT_THUPDATE  = 13 // Update thread list
	PT_GETREGS   = 15 // Get all registers
	PT_SETREGS   = 16 // Set all registers
)

// ARM64ThreadState represents ARM64 thread state for Darwin
type ARM64ThreadState struct {
	X    [29]uint64 // General purpose registers x0-x28
	Fp   uint64     // Frame pointer x29
	Lr   uint64     // Link register x30
	Sp   uint64     // Stack pointer
	Pc   uint64     // Program counter
	Cpsr uint32     // Current program status register
	Pad  uint32     // Padding for alignment
}

type darwinARM64Debugger struct {
	DebugInfo       debuginfo.DebugInfo
	Breakpoints     map[uint64][]byte
	EndDebugSession chan bool

	// Communication with hub
	BreakpointHit        chan BreakpointEvent
	InitialBreakpointHit chan InitialBreakpointHitEvent
	DebugCommand         chan DebugCommand
}

func NewDebugger(breakpointHit chan BreakpointEvent, initialBreakpointHit chan InitialBreakpointHitEvent, debugCommand chan DebugCommand, endDebugSession chan bool) Debugger {
	return &darwinARM64Debugger{
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

// ptrace wraps the ptrace system call for Darwin
func ptrace(request int, pid int, addr uintptr, data uintptr) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, uintptr(request), uintptr(pid), addr, data, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ptraceGetRegs retrieves ARM64 register state from the target process
func ptraceGetRegs(pid int) (*ARM64ThreadState, error) {
	var regs ARM64ThreadState
	err := ptrace(PT_GETREGS, pid, uintptr(unsafe.Pointer(&regs)), 0)
	if err != nil {
		return nil, err
	}
	return &regs, nil
}

// ptraceSetRegs sets ARM64 register state in the target process
func ptraceSetRegs(pid int, regs *ARM64ThreadState) error {
	return ptrace(PT_SETREGS, pid, uintptr(unsafe.Pointer(regs)), 0)
}

// ptracePeekData reads a word from the target process memory
// For ARM64, we read 4 bytes at a time
func ptracePeekData(pid int, addr uintptr) ([]byte, error) {
	// Align to 4-byte boundary for ARM64
	alignedAddr := addr & ^uintptr(3)

	var data int32
	err := ptrace(PT_READ_D, pid, alignedAddr, uintptr(unsafe.Pointer(&data)))
	if err != nil {
		return nil, fmt.Errorf("ptrace PT_READ_D failed at 0x%x: %v", addr, err)
	}

	// Convert int32 to byte slice
	result := make([]byte, 4)
	result[0] = byte(data)
	result[1] = byte(data >> 8)
	result[2] = byte(data >> 16)
	result[3] = byte(data >> 24)

	return result, nil
}

// ptracePokeData writes data to the target process memory
func ptracePokeData(pid int, addr uintptr, data []byte) error {
	// Align to 4-byte boundary for ARM64
	alignedAddr := addr & ^uintptr(3)

	// Ensure data is exactly 4 bytes for ARM64
	if len(data) < 4 {
		// Pad with zeros if needed
		padded := make([]byte, 4)
		copy(padded, data)
		data = padded
	}

	// Convert byte slice to int32
	word := int32(data[0]) | int32(data[1])<<8 | int32(data[2])<<16 | int32(data[3])<<24

	err := ptrace(PT_WRITE_D, pid, alignedAddr, uintptr(word))
	if err != nil {
		return fmt.Errorf("ptrace PT_WRITE_D failed at 0x%x: %v", addr, err)
	}

	return nil
}

// ptraceCont continues execution of the target process
func ptraceCont(pid int, signal int) error {
	return ptrace(PT_CONTINUE, pid, 1, uintptr(signal))
}

// ptraceSingleStep executes a single instruction
func ptraceSingleStep(pid int) error {
	return ptrace(PT_STEP, pid, 1, 0)
}

// ptraceDetach detaches from the target process
func ptraceDetach(pid int) error {
	return ptrace(PT_DETACH, pid, 0, 0)
}

// StartWithDebug launches the target binary at the given path under debugger control
func (d *darwinARM64Debugger) StartWithDebug(path string) {
	// Lock this goroutine to the current OS thread.
	// Darwin ptrace requires that all ptrace calls for a given traced process originate from the same OS thread that performed the initial attach.
	// Without this, the Go scheduler may migrate the goroutine to a different OS thread, causing ptrace calls to fail.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Validate and sanitise the user-supplied path before passing it to exec.
	validatedPath, err := validateTargetPath(path)
	if err != nil {
		log.Printf("[Debugger] Rejected target path %q: %v", path, err)
		panic(err)
	}

	// Set up target for execution
	cmd := exec.Command(validatedPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &unix.SysProcAttr{Ptrace: true}

	if err := cmd.Start(); err != nil {
		log.Printf("[Debugger] Failed to start target: %v", err)
		panic(err)
	}

	dbInf, err := debuginfo.NewDebugInfo(validatedPath, cmd.Process.Pid)
	if err != nil {
		log.Printf("[Debugger] Failed to create debug info: %v", err)
		panic(err)
	}
	log.Printf("[Debugger] Started process with PID: %d and PGID: %d\n", dbInf.GetTarget().PID, dbInf.GetTarget().PGID)

	d.DebugInfo = dbInf

	// We want to catch the initial SIGTRAP sent by process creation. When this is caught, we know that the target just started and we can ask the user where they want to set their breakpoints
	var waitStatus unix.WaitStatus
	if _, status := unix.Wait4(d.DebugInfo.GetTarget().PID, &waitStatus, 0, nil); status != nil {
		log.Printf("[Debugger] Received SIGTRAP from process creation: %v", status)
	}

	// Set initial breakpoints while the process is stopped at the initial SIGTRAP
	d.initialBreakpointHit()

	// Check if we were stopped during initial breakpoint
	select {
	case <-d.EndDebugSession:
		log.Println("[Debugger] Debug session ended during initial breakpoint, cleaning up")
		return
	default:
		// Continue to debug loop
	}

	log.Println("[Debugger] STARTING DEBUG LOOP")

	d.mainDebugLoop()

	log.Println("[Debugger] Debug loop ended")
}

// Continue resumes execution of the process with the given PID after a breakpoint
func (d *darwinARM64Debugger) Continue(pid int) {
	// Read registers
	regs, err := ptraceGetRegs(pid)
	if err != nil {
		log.Printf("[Debugger] Failed to get registers: %v", err)
		panic(err)
	}
	// ARM64 breakpoint is 4 bytes, so we rewind by 4
	_, line, _ := d.DebugInfo.PCToLine(regs.Pc - 4)

	// Remove the breakpoint
	bpAddr := regs.Pc - 4
	if err := d.ClearBreakpoint(pid, line); err != nil {
		log.Printf("[Debugger] Failed to clear breakpoint: %v", err)
		panic(err)
	}
	regs.Pc = bpAddr

	// Set the registers back with the rewound PC
	if err := ptraceSetRegs(pid, regs); err != nil {
		log.Printf("[Debugger] Failed to restore registers: %v", err)
		panic(err)
	}

	// Step over the instruction we previously removed to put the breakpoint
	if err := ptraceSingleStep(pid); err != nil {
		log.Printf("[Debugger] Failed to single-step: %v", err)
		panic(err)
	}

	var waitStatus unix.WaitStatus
	// Wait until the program lets us know we stepped over
	if _, err := unix.Wait4(pid, &waitStatus, 0, nil); err != nil {
		log.Printf("[Debugger] Failed to wait for the single-step: %v", err)
		panic(err)
	}

	// Put the breakpoint back
	if err := d.SetBreakpoint(pid, line); err != nil {
		log.Printf("[Debugger] Failed to set breakpoint: %v", err)
		panic(err)
	}

	// Resume execution
	if err := ptraceCont(pid, 0); err != nil {
		log.Printf("[Debugger] Failed to resume target execution: %v", err)
		panic(err)
	}
}

// SingleStep executes a single instruction in the process with the given PID
func (d *darwinARM64Debugger) SingleStep(pid int) {
	if err := ptraceSingleStep(pid); err != nil {
		log.Printf("[Debugger] Failed to single-step: %v", err)
		panic(err)
	}
}

// StopDebug detaches from the target and ends the debug session
func (d *darwinARM64Debugger) StopDebug() {
	// Detach from the target process, letting it continue running
	if d.DebugInfo.GetTarget().PID > 0 {
		log.Printf("[Debugger] Detaching from target process (PID: %d)", d.DebugInfo.GetTarget().PID)
		if err := ptraceDetach(d.DebugInfo.GetTarget().PID); err != nil {
			log.Printf("[Debugger] Failed to detach from target process: %v (might have already exited)", err)
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

// SetBreakpoint inserts a breakpoint at the given source line in the target
func (d *darwinARM64Debugger) SetBreakpoint(pid int, line int) error {
	pc, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return fmt.Errorf("failed to get PC of line %v: %v", line, err)
	}

	log.Printf("[Debugger] Setting breakpoint at line %d, PC: 0x%x, PID: %d", line, pc, pid)

	original, err := ptracePeekData(pid, uintptr(pc))
	if err != nil {
		return fmt.Errorf("failed to read original machine code into memory: %v for PID: %d", err, pid)
	}
	log.Printf("[Debugger] Read original instruction: %x", original)

	if err := ptracePokeData(pid, uintptr(pc), bpCode); err != nil {
		return fmt.Errorf("failed to write breakpoint into memory: %v", err)
	}
	log.Printf("[Debugger] Wrote breakpoint instruction at 0x%x", pc)

	d.Breakpoints[pc] = original
	return nil
}

// ClearBreakpoint removes the breakpoint at the given source line
func (d *darwinARM64Debugger) ClearBreakpoint(pid int, line int) error {
	pc, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return fmt.Errorf("failed to get PC of line %v: %v", line, err)
	}
	if err := ptracePokeData(pid, uintptr(pc), d.Breakpoints[pc]); err != nil {
		return fmt.Errorf("failed to write breakpoint into memory: %v", err)
	}
	return nil
}

// mainDebugLoop continuously monitors the target process for debug events
func (d *darwinARM64Debugger) mainDebugLoop() {
	for {
		// Check if we should stop debugging
		select {
		case <-d.EndDebugSession:
			log.Println("[Debugger] Debug session ending, exiting debug loop")
			return
		default:
			// Continue with wait
		}

		// Wait for the target process to be interrupted or end
		// On Darwin, we use the direct PID and WNOHANG for non-blocking wait
		var waitStatus unix.WaitStatus
		wpid, err := unix.Wait4(d.DebugInfo.GetTarget().PID, &waitStatus, unix.WNOHANG, nil)
		if err != nil {
			log.Printf("[Debugger] Failed to wait for the target: %v", err)
			// Don't panic, just exit gracefully
			return
		}

		// If no process state changed, sleep briefly to avoid busy waiting
		if wpid == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if waitStatus.Exited() {
			if wpid == d.DebugInfo.GetTarget().PID { // If target exited, terminate
				log.Printf("[Debugger] Target %v execution completed", d.DebugInfo.GetTarget().Path)
				// Signal the end of debug session to hub
				select {
				case d.EndDebugSession <- true:
				default:
					// Channel might be full, meaning debug session end already triggered
				}
				return
			} else {
				log.Printf("[Debugger] Thread exited with PID: %v", wpid)
			}
		} else {
			// Only stop on breakpoints caused by our debugger
			if waitStatus.StopSignal() == unix.SIGTRAP {
				d.breakpointHit(wpid)
			} else {
				if err := ptraceCont(wpid, 0); err != nil {
					log.Printf("[Debugger] Failed to resume target execution: %v for PID: %d", err, wpid)
					// Don't panic, might have been detached
					return
				}
			}
		}
	}
}

// initialBreakpointHit handles the initial SIGTRAP and allows setting breakpoints
func (d *darwinARM64Debugger) initialBreakpointHit() {
	// Create initial breakpoint event
	event := InitialBreakpointHitEvent{
		PID: d.DebugInfo.GetTarget().PID,
	}

	// Send initial breakpoint hit event to hub
	log.Println("[Debugger] Initial breakpoint hit, debugger ready for commands")
	d.InitialBreakpointHit <- event

	// Wait for commands from hub (typically to set breakpoints)
	for {
		select {
		case cmd := <-d.DebugCommand:
			log.Printf("[Debugger] Received command: %s", cmd.Type)

			switch cmd.Type {
			case "setBreakpoint":
				if data, ok := cmd.Data.(map[string]any); ok {
					if line, ok := data["line"].(int); ok {
						if err := d.SetBreakpoint(d.DebugInfo.GetTarget().PID, int(line)); err != nil {
							log.Printf("[Debugger] Failed to set breakpoint at line %d: %v", int(line), err)
							panic(err)
						} else {
							log.Printf("[Debugger] Breakpoint set at line %d while at breakpoint", int(line))
						}
					}
				}
			case "continue":
				log.Println("[Debugger] Continuing from initial breakpoint")
				if err := ptraceCont(d.DebugInfo.GetTarget().PID, 0); err != nil {
					log.Printf("[Debugger] Failed to resume target execution: %v", err)
					panic(err)
				}
				return // Exit initial breakpoint handling
			case "step":
				log.Println("[Debugger] Cannot single-step from initial breakpoint")
			case "quit":
				d.StopDebug()
				return
			default:
				log.Printf("[Debugger] Unknown command during initial breakpoint: %s", cmd.Type)
			}
		case <-d.EndDebugSession:
			log.Println("[Debugger] Debug session ending during initial breakpoint")
			return
		}
	}
}

// breakpointHit handles a breakpoint event and waits for debugger commands
func (d *darwinARM64Debugger) breakpointHit(pid int) {
	// Get register information to determine location
	regs, err := ptraceGetRegs(pid)
	if err != nil {
		log.Printf("[Debugger] Failed to get registers: %v", err)
		panic(err)
	}

	// Get location information (rewind PC by 4 bytes for ARM64)
	filename, line, fn := d.DebugInfo.PCToLine(regs.Pc - 4)

	// Create breakpoint event
	event := BreakpointEvent{
		PID:      pid,
		Filename: filename,
		Line:     line,
		Function: fn.Name,
	}

	// Send breakpoint hit event to hub
	log.Printf("[Debugger] Breakpoint hit at %s:%d in %s, waiting for command", filename, line, fn.Name)
	d.BreakpointHit <- event

	// Wait for command from hub
	select {
	case cmd := <-d.DebugCommand:
		log.Printf("[Debugger] Received command: %s", cmd.Type)
		switch cmd.Type {
		case "continue":
			d.Continue(pid)
		case "step":
			d.SingleStep(pid)
		case "setBreakpoint":
			if data, ok := cmd.Data.(map[string]any); ok {
				if line, ok := data["line"].(int); ok {
					if err := d.SetBreakpoint(pid, int(line)); err != nil {
						log.Printf("[Debugger] Failed to set breakpoint at line %d: %v", int(line), err)
					} else {
						log.Printf("[Debugger] Breakpoint set at line %d while at breakpoint", int(line))
					}
				}
			}
		case "quit":
			d.StopDebug()
			return
		default:
			log.Printf("[Debugger] Unknown command: %s", cmd.Type)
			d.Continue(pid) // Default to continue
		}
	case <-d.EndDebugSession:
		log.Println("[Debugger] Debug session ending, stopping breakpoint handler")
		return
	}
}
