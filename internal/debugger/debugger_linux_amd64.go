package debugger

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/bingosuite/bingo/internal/debuginfo"

	"golang.org/x/sys/unix"
)

var (
	bpCode = []byte{0xCC}
)

const (
	ptraceOExitKill = 0x100000 // Set option to kill the target process when Bingo exits to true
)

type linuxAMD64Debugger struct {
	DebugInfo   debuginfo.DebugInfo
	Breakpoints map[uint64][]byte

	// stop is closed by StopDebug to signal all internal loops to exit.
	stop     chan struct{}
	stopOnce sync.Once

	// Communication with hub
	DebuggerEvents chan DebuggerEvent
	DebugCommand   chan DebugCommand
}

func NewDebugger(debuggerEvents chan DebuggerEvent, debugCommand chan DebugCommand) Debugger {
	return &linuxAMD64Debugger{
		Breakpoints:    make(map[uint64][]byte),
		stop:           make(chan struct{}),
		DebuggerEvents: debuggerEvents,
		DebugCommand:   debugCommand,
	}
}

// sendEvent sends an event to the hub, aborting silently if the session is stopping.
func (d *linuxAMD64Debugger) sendEvent(event DebuggerEvent) {
	select {
	case d.DebuggerEvents <- event:
	case <-d.stop:
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

	// notifyEnd always sends a SessionEndedEvent before returning, so the hub
	// learns of both clean exits and failures through the same channel.
	notifyEnd := func(err error) { d.sendEvent(SessionEndedEvent{Err: err}) }

	// Validate and sanitise the user-supplied path before passing it to exec.
	validatedPath, err := validateTargetPath(path)
	if err != nil {
		log.Printf("[Debugger] Rejected target path %q: %v", path, err)
		notifyEnd(err)
		return
	}

	// Set up target for execution
	cmd := exec.Command(validatedPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &unix.SysProcAttr{Ptrace: true}

	if err := cmd.Start(); err != nil {
		log.Printf("[Debugger] Failed to start target: %v", err)
		notifyEnd(fmt.Errorf("starting target: %w", err))
		return
	}

	dbInf, err := debuginfo.NewDebugInfo(validatedPath, cmd.Process.Pid)
	if err != nil {
		log.Printf("[Debugger] Failed to create debug info: %v", err)
		notifyEnd(fmt.Errorf("creating debug info: %w", err))
		return
	}
	log.Printf("[Debugger] Started process with PID: %d and PGID: %d\n", dbInf.GetTarget().PID, dbInf.GetTarget().PGID)

	// Enable tracking threads spawned from target and killing target once Bingo exits
	if err := unix.PtraceSetOptions(dbInf.GetTarget().PID, unix.PTRACE_O_TRACECLONE|ptraceOExitKill); err != nil {
		log.Printf("[Debugger] Failed to set TRACECLONE and EXITKILL options on target: %v", err)
		notifyEnd(fmt.Errorf("setting ptrace options: %w", err))
		return
	}

	d.DebugInfo = dbInf

	// We want to catch the initial SIGTRAP sent by process creation. When this is caught, we know that the target just started and we can ask the user where they want to set their breakpoints
	// The message we print to the console will be removed in the future, it's just for debugging purposes for now.

	var waitStatus unix.WaitStatus
	if _, status := unix.Wait4(d.DebugInfo.GetTarget().PID, &waitStatus, 0, nil); status != nil {
		log.Printf("[Debugger] Received SIGTRAP from process creation: %v", status)
	}

	// Set initial breakpoints while the process is stopped at the initial SIGTRAP
	if err := d.initialBreakpointHit(); err != nil {
		notifyEnd(fmt.Errorf("initial breakpoint: %w", err))
		return
	}

	// Check if we were stopped during initial breakpoint
	select {
	case <-d.stop:
		log.Println("[Debugger] Debug session ended during initial breakpoint, cleaning up")
		notifyEnd(nil)
		return
	default:
		// Continue to debug loop
	}

	log.Println("[Debugger] STARTING DEBUG LOOP")

	if err := d.mainDebugLoop(); err != nil {
		notifyEnd(fmt.Errorf("debug loop: %w", err))
		return
	}

	log.Println("[Debugger] Debug loop ended")
	notifyEnd(nil)
}

func (d *linuxAMD64Debugger) continueExec(pid int) error {
	// Read registers
	var regs unix.PtraceRegs
	if err := unix.PtraceGetRegs(pid, &regs); err != nil {
		return fmt.Errorf("getting registers for pid %d: %w", pid, err)
	}
	_, line, _ := d.DebugInfo.PCToLine(regs.Rip - 1) // Breakpoint advances PC by 1 on x86, so we need to rewind

	// Remove the breakpoint
	bpAddr := regs.Rip - 1
	if err := d.clearBreakpoint(pid, line); err != nil {
		return fmt.Errorf("clearing breakpoint at line %d: %w", line, err)
	}
	regs.Rip = bpAddr

	// Rewind Rip by 1
	if err := unix.PtraceSetRegs(pid, &regs); err != nil {
		return fmt.Errorf("restoring registers for pid %d: %w", pid, err)
	}

	// Step over the instruction we previously removed to put the breakpoint
	// TODO: decide if we want to call debugger.SingleStep() for this or just the system
	if err := unix.PtraceSingleStep(pid); err != nil {
		return fmt.Errorf("single-stepping pid %d: %w", pid, err)
	}

	// TODO: only trigger for step over signal
	var waitStatus unix.WaitStatus
	// Wait until the program lets us know we stepped over (handle cases where we get another signal which Wait4 would consume)
	if _, err := unix.Wait4(pid, &waitStatus, 0, nil); err != nil {
		return fmt.Errorf("waiting for single-step on pid %d: %w", pid, err)
	}

	// Put the breakpoint back
	if err := d.setBreakpoint(pid, line); err != nil {
		return fmt.Errorf("re-setting breakpoint at line %d: %w", line, err)
	}

	// Resume execution
	// TODO: decide if we want to call debugger.Continue() for this or just the system call
	if err := unix.PtraceCont(pid, 0); err != nil {
		return fmt.Errorf("resuming pid %d: %w", pid, err)
	}
	return nil
}

func (d *linuxAMD64Debugger) singleStep(pid int) error {
	if err := unix.PtraceSingleStep(pid); err != nil {
		return fmt.Errorf("single-stepping pid %d: %w", pid, err)
	}
	return nil
}

func (d *linuxAMD64Debugger) stepOver(pid int) error {
	// TODO
	return nil
}

func (d *linuxAMD64Debugger) stopDebug() {
	if d.DebugInfo.GetTarget().PID > 0 {
		log.Printf("[Debugger] Detaching from target process (PID: %d)", d.DebugInfo.GetTarget().PID)
		if err := unix.PtraceDetach(d.DebugInfo.GetTarget().PID); err != nil {
			// Process may have already exited; log but don't treat as fatal
			log.Printf("[Debugger] Failed to detach from target process: %v (might have already exited)", err)
		}
	}
	// Close the stop channel to signal all internal loops to exit.
	// sync.Once ensures this is safe to call multiple times.
	d.stopOnce.Do(func() { close(d.stop) })
}

func (d *linuxAMD64Debugger) setBreakpoint(pid int, line int) error {

	pc, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return fmt.Errorf("resolving PC for line %d: %w", line, err)
	}

	original := make([]byte, len(bpCode))
	if _, err := unix.PtracePeekData(pid, uintptr(pc), original); err != nil {
		return fmt.Errorf("reading original instruction at line %d (pid %d): %w", line, pid, err)
	}
	if _, err := unix.PtracePokeData(pid, uintptr(pc), bpCode); err != nil {
		return fmt.Errorf("writing breakpoint at line %d: %w", line, err)
	}
	d.Breakpoints[pc] = original
	return nil
}

func (d *linuxAMD64Debugger) clearBreakpoint(pid int, line int) error {

	pc, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return fmt.Errorf("resolving PC for line %d: %w", line, err)
	}
	if _, err := unix.PtracePokeData(pid, uintptr(pc), d.Breakpoints[pc]); err != nil {
		return fmt.Errorf("restoring original instruction at line %d: %w", line, err)
	}
	return nil
}

// TODO: pass the correct pid to the debugger methods, keep an eye on this
func (d *linuxAMD64Debugger) mainDebugLoop() error {
	for {
		// Check if we should stop debugging
		select {
		case <-d.stop:
			log.Println("[Debugger] Debug session ending, exiting debug loop")
			return nil
		default:
			// Continue with wait
		}

		// Wait until any of the child processes of the target is interrupted or ends
		var waitStatus unix.WaitStatus
		wpid, err := unix.Wait4(-1*d.DebugInfo.GetTarget().PGID, &waitStatus, unix.WNOHANG, nil)
		if err != nil {
			return fmt.Errorf("waiting for target or its threads: %w", err)
		}

		// TODO: change 10ms polling approach to goroutine
		if wpid == 0 { // if no process state changed, sleep briefly to avoid busy waiting and consuming 100% cpu
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if waitStatus.Exited() {
			if wpid == d.DebugInfo.GetTarget().PID { // If target exited, return and let StartWithDebug send SessionEndedEvent
				log.Printf("[Debugger] Target %v execution completed", d.DebugInfo.GetTarget().Path)
				return nil
			} else {
				log.Printf("[Debugger] Thread exited with PID: %v", wpid)
			}
		} else {
			// Only stop on breakpoints caused by our debugger, ignore any other event like spawning of new threads
			if waitStatus.StopSignal() == unix.SIGTRAP && waitStatus.TrapCause() != unix.PTRACE_EVENT_CLONE {
				if err := d.breakpointHit(wpid); err != nil {
					return fmt.Errorf("handling breakpoint for pid %d: %w", wpid, err)
				}
			} else {
				if err := unix.PtraceCont(wpid, 0); err != nil {
					return fmt.Errorf("resuming pid %d: %w", wpid, err)
				}
			}
		}
	}
}

// TODO: maybe refactor later
func (d *linuxAMD64Debugger) initialBreakpointHit() error {
	event := InitialBreakpointHitEvent{
		PID: d.DebugInfo.GetTarget().PID,
	}
	log.Println("[Debugger] Initial breakpoint hit, debugger ready for commands")
	d.sendEvent(event)

	// Wait for commands from hub (typically to set breakpoints)
	for {
		select {
		case cmd := <-d.DebugCommand:
			log.Printf("[Debugger] Received command: %s", cmd.Type)

			switch cmd.Type {
			case "setBreakpoint":
				if data, ok := cmd.Data.(map[string]any); ok {
					if line, ok := data["line"].(int); ok {
						if err := d.setBreakpoint(d.DebugInfo.GetTarget().PID, line); err != nil {
							return fmt.Errorf("setting initial breakpoint at line %d: %w", line, err)
						}
						log.Printf("[Debugger] Breakpoint set at line %d while at breakpoint", line)
					}
				}
			case "continue":
				log.Println("[Debugger] Continuing from initial breakpoint")
				if err := unix.PtraceCont(d.DebugInfo.GetTarget().PID, 0); err != nil {
					return fmt.Errorf("resuming target after initial stop: %w", err)
				}
				return // Exit initial breakpoint handling
			case "stepOver":
				log.Println("[Debugger] Cannot stepover from initial breakpoint")
			case "singleStep":
				log.Println("[Debugger] Cannot single-step from initial breakpoint")
			case "quit":
				d.stopDebug()
				return nil
			default:
				log.Printf("[Debugger] Unknown command during initial breakpoint: %s", cmd.Type)
			}
		case <-d.stop:
			log.Println("[Debugger] Debug session ending during initial breakpoint")
			return nil
		}
	}
}

func (d *linuxAMD64Debugger) breakpointHit(pid int) error {
	var regs unix.PtraceRegs
	if err := unix.PtraceGetRegs(pid, &regs); err != nil {
		return fmt.Errorf("getting registers for pid %d: %w", pid, err)
	}

	filename, line, fn := d.DebugInfo.PCToLine(regs.Rip - 1)
	event := BreakpointEvent{
		PID:      pid,
		Filename: filename,
		Line:     line,
		Function: fn.Name,
	}

	log.Printf("[Debugger] Breakpoint hit at %s:%d in %s, waiting for command", filename, line, fn.Name)
	d.sendEvent(event)

	// Wait for command from hub
	select {
	case cmd := <-d.DebugCommand:
		log.Printf("[Debugger] Received command: %s", cmd.Type)
		switch cmd.Type {
		case "continue":
			if err := d.continueExec(pid); err != nil {
				return fmt.Errorf("continuing from breakpoint: %w", err)
			}
		case "stepOver":
			if err := d.stepOver(pid); err != nil {
				return fmt.Errorf("stepping over at breakpoint: %w", err)
			}
		case "singleStep":
			if err := d.singleStep(pid); err != nil {
				return fmt.Errorf("single stepping at breakpoint: %w", err)
			}
		case "setBreakpoint":
			if data, ok := cmd.Data.(map[string]any); ok {
				if line, ok := data["line"].(int); ok {
					if err := d.setBreakpoint(pid, line); err != nil {
						log.Printf("[Debugger] Failed to set breakpoint at line %d: %v", line, err)
					} else {
						log.Printf("[Debugger] Breakpoint set at line %d while at breakpoint", line)
					}
				}
			}
		case "quit":
			d.stopDebug()
			return nil
		default:
			log.Printf("[Debugger] Unknown command: %s", cmd.Type)
			if err := d.continueExec(pid); err != nil {
				return fmt.Errorf("continuing from breakpoint (default): %w", err)
			}
		}
	case <-d.stop:
		log.Println("[Debugger] Debug session ending, stopping breakpoint handler")
		return nil
	}
	return nil
}
