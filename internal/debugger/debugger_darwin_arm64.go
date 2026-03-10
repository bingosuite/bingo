package debugger

/*
#cgo LDFLAGS: -framework CoreFoundation
#include "darwin_helper.h"
*/
import "C"
import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/bingosuite/bingo/internal/debuginfo"
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
	slide           uint64

	// Communication with hub
	BreakpointHit        chan BreakpointEvent
	InitialBreakpointHit chan InitialBreakpointHitEvent
	DebugCommand         chan DebugCommand

	task       C.mach_port_t
	mainThread C.thread_act_t
	excPort    C.mach_port_t

	// Temporary breakpoint state for implementing "continue" command
	tempBreakpoint uint64
	tempOriginal   []byte
	tempIsStepOver bool // true if temp breakpoint is from stepOver (stop), false if from continue (auto-resume)

	// Single-step state: track if we removed a breakpoint for stepping
	singleStepRemovedBP uint64
	isSingleStepping    bool // Track if we're currently single-stepping
}

func (d *darwinARM64Debugger) resetSessionState() {
	d.DebugInfo = nil
	d.slide = 0
	d.Breakpoints = make(map[uint64][]byte)

	d.task = 0
	d.mainThread = 0
	d.excPort = 0

	d.tempBreakpoint = 0
	d.tempOriginal = nil
	d.tempIsStepOver = false

	d.singleStepRemovedBP = 0
	d.isSingleStepping = false
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

func (d *darwinARM64Debugger) computeSlide() error {
	if d.task == 0 {
		return fmt.Errorf("cannot compute slide: debugger task not initialized")
	}

	var slide C.mach_vm_address_t

	kr := C.find_image_slide(d.task, &slide)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("find_image_slide failed: %d", kr)
	}

	d.slide = uint64(slide)

	log.Printf("[Debugger] ASLR slide = %#x", d.slide)

	return nil
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

func (d *darwinARM64Debugger) getRegs() (*ARM64ThreadState, error) {
	var st ARM64ThreadState
	count := C.mach_msg_type_number_t(C.ARM_THREAD_STATE64_COUNT)
	kr := C.get_arm64_thread_state(d.mainThread, (*C.arm_thread_state64_t)(unsafe.Pointer(&st)), &count)
	if kr != C.KERN_SUCCESS {
		return nil, fmt.Errorf("thread_get_state failed: %d", kr)
	}
	return &st, nil
}

func (d *darwinARM64Debugger) readWord(addr uint64) ([]byte, error) {
	var word C.uint32_t
	kr := C.read_word(d.task, C.mach_vm_address_t(addr), &word)
	if kr != C.KERN_SUCCESS {
		return nil, fmt.Errorf("read_word failed: %d", kr)
	}
	v := uint32(word)
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}, nil
}

func (d *darwinARM64Debugger) writeWord(addr uint64, data []byte) error {
	if len(data) < 4 {
		tmp := make([]byte, 4)
		copy(tmp, data)
		data = tmp
	}
	v := uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
	kr := C.write_word(d.task, C.mach_vm_address_t(addr), C.uint32_t(v))
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("write_word failed: %d", kr)
	}
	return nil
}

// exceptionLoop runs in its own OS thread and handles all Mach exceptions
func (d *darwinARM64Debugger) exceptionLoop() {
	runtime.LockOSThread() // Mach msg requires fixed thread
	defer runtime.UnlockOSThread()

	log.Println("[Exception Loop] In top of exception loop")
	for {
		select {
		case <-d.EndDebugSession:
			log.Println("[Debugger] Exception loop ending")
			return
		default:
		}

		var excBuf [4096]byte
		var reply C.exc_msg_reply_t

		log.Println("[Exception Loop] about to receive exception message...")

		// Receive exception message (blocks until breakpoint hit)
		kr := C.mach_msg(
			(*C.mach_msg_header_t)(unsafe.Pointer(&excBuf[0])),
			C.MACH_RCV_MSG,
			0,
			C.mach_msg_size_t(len(excBuf)),
			d.excPort,
			C.MACH_MSG_TIMEOUT_NONE,
			C.MACH_PORT_NULL,
		)
		if kr != C.MACH_MSG_SUCCESS {
			log.Printf("[Debugger] mach_msg receive failed: %d", kr)
			continue
		}
		log.Println("[Exception Loop] Received exception message")

		excMsg := (*C.exc_msg_t)(unsafe.Pointer(&excBuf[0]))

		// log.Printf(
		// 	"[Exception Loop] exception=%d code=%d subcode=%d",
		// 	excMsg.exception,
		// 	excMsg.codes[0],
		// 	excMsg.codes[1],
		// )

		// Check if it's a breakpoint exception
		if excMsg.exception == C.EXC_BREAKPOINT || excMsg.exception == C.EXC_BAD_INSTRUCTION {
			log.Println("[Exception Loop] We got a breakpoint exception!")
			thread := C.exc_msg_thread(excMsg)
			if d.mainThread == 0 {
				d.mainThread = thread // Cache first thread seen
				log.Printf("[Debugger] Cached main thread: %d", thread)
			}

			// Check if we just completed a single-step
			if d.isSingleStepping {
				log.Println("[Exception Loop] Single-step completed, clearing state")
				C.disable_single_step(d.mainThread) // Ensure SS bit is cleared
				d.isSingleStepping = false

				// Re-install any breakpoint we temporarily removed for single-stepping
				if d.singleStepRemovedBP != 0 {
					log.Printf("[Exception Loop] Re-installing breakpoint at %#x", d.singleStepRemovedBP)
					brk := []byte{0x20, 0x00, 0x20, 0xd4}
					if err := d.writeWord(d.singleStepRemovedBP, brk); err != nil {
						log.Printf("[Exception Loop] Failed to re-install breakpoint: %v", err)
					}
					d.singleStepRemovedBP = 0
				}
			}

			// Dispatch to your existing handlers
			if d.DebugInfo == nil {
				log.Println("[Exception Loop] debuginfo not initialized, treating as initial breakpoint")
				d.initialBreakpointHit()
			} else {
				log.Println("[Exception Loop] debuginfo initialized, treating as regular breakpoint")
				d.breakpointHit(int(d.DebugInfo.GetTarget().PID))
			}

			// IMPORTANT: After your handler returns (Continue/step called),
			// send reply to resume the thread
			reply.Head.msgh_bits = C.make_reply_bits(excMsg.Head.msgh_bits)
			reply.Head.msgh_size = C.mach_msg_size_t(unsafe.Sizeof(reply))

			reply.Head.msgh_remote_port = excMsg.Head.msgh_remote_port
			reply.Head.msgh_local_port = C.MACH_PORT_NULL

			reply.Head.msgh_id = C.make_reply_id(excMsg.Head.msgh_id)

			reply.RetCode = C.KERN_SUCCESS

			replyKr := C.mach_msg(
				(*C.mach_msg_header_t)(unsafe.Pointer(&reply)),
				C.MACH_SEND_MSG,
				C.mach_msg_size_t(unsafe.Sizeof(reply)),
				0,
				C.MACH_PORT_NULL,
				C.MACH_MSG_TIMEOUT_NONE,
				C.MACH_PORT_NULL,
			)

			if replyKr != C.MACH_MSG_SUCCESS {
				log.Printf("[Debugger] Failed to send exception reply: %d", replyKr)
			} else {
				// Resume the stopped thread
				C.thread_resume(d.mainThread)
			}
		} else {
			// Unknown exception - pass to next handler

			reply.Head.msgh_bits = C.make_reply_bits(excMsg.Head.msgh_bits)
			reply.Head.msgh_size = C.mach_msg_size_t(unsafe.Sizeof(reply))

			reply.Head.msgh_remote_port = excMsg.Head.msgh_remote_port
			reply.Head.msgh_local_port = C.MACH_PORT_NULL

			reply.Head.msgh_id = excMsg.Head.msgh_id + 100

			reply.RetCode = 5 // KERN_FAILURE

			C.mach_msg(
				(*C.mach_msg_header_t)(unsafe.Pointer(&reply)),
				C.MACH_SEND_MSG,
				C.mach_msg_size_t(unsafe.Sizeof(reply)),
				0,
				C.MACH_PORT_NULL,
				C.MACH_MSG_TIMEOUT_NONE,
				C.MACH_PORT_NULL,
			)
		}
	}
}

func (d *darwinARM64Debugger) StartWithDebug(path string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Consecutive runs in the same hub reuse the debugger instance, so clear
	// all process-specific state before starting a new target.
	d.resetSessionState()

	// Drain any stale shutdown signal from a previous run.
	select {
	case <-d.EndDebugSession:
	default:
	}

	validatedPath, err := validateTargetPath(path)
	if err != nil {
		log.Printf("[Debugger] Rejected target path %q: %v", path, err)
		panic(err)
	}

	cmd := exec.Command(validatedPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = nil
	if err := cmd.Start(); err != nil {
		panic(err)
	}

	pid := cmd.Process.Pid
	log.Printf("[Debugger] Started target PID %d", pid)

	// Watch for process exit
	go func() {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(pid, &status, 0, nil)
		if err != nil {
			log.Printf("[Debugger] wait4 error: %v", err)
		}

		log.Printf("[Debugger] Target process exited: %v", status)

		// signal debugger shutdown
		select {
		case d.EndDebugSession <- true:
		default:
		}
	}()

	// Get task port
	var task C.mach_port_t
	kr := C.task_for_pid(C.get_mach_task_self(), C.int(cmd.Process.Pid), &task)
	if kr != C.KERN_SUCCESS {
		panic(fmt.Errorf("task_for_pid failed: %d", kr))
	}
	d.task = task

	// Set up exception port (your existing code - perfect)
	var excPort C.mach_port_t
	kr = C.mach_port_allocate(C.get_mach_task_self(), C.MACH_PORT_RIGHT_RECEIVE, &excPort)
	log.Println("mach_port_allocate:", kr)
	if kr != C.KERN_SUCCESS {
		panic(fmt.Errorf("mach_port_allocate failed: %d", kr))
	}
	d.excPort = excPort
	log.Printf("[Debugger] exception port = %d", d.excPort)

	kr = C.mach_port_insert_right(
		C.get_mach_task_self(),
		excPort,
		excPort,
		C.MACH_MSG_TYPE_MAKE_SEND,
	)
	log.Println("mach_port_insert_right:", kr)
	if kr != C.KERN_SUCCESS {
		panic(fmt.Errorf("mach_port_insert_right failed: %d", kr))
	}

	kr = C.set_debug_exception_ports(d.task, excPort)
	if kr != C.KERN_SUCCESS {
		panic(fmt.Errorf("task_set_exception_ports failed: %d", kr))
	}

	kr = C.set_thread_exception_ports(d.task, excPort)
	if kr != C.KERN_SUCCESS {
		panic(fmt.Errorf("thread_set_exception_ports failed: %d", kr))
	}

	// Get initial threads
	var firstThread C.thread_act_t
	kr = C.get_first_thread(d.task, &firstThread)
	if kr == C.KERN_SUCCESS {
		d.mainThread = firstThread
		log.Printf("[Debugger] Found main thread: %v", d.mainThread)

		// *** ADD THIS: SUSPEND THE THREAD ***
		kr = C.thread_suspend(firstThread)
		if kr != C.KERN_SUCCESS {
			panic(fmt.Errorf("thread_suspend failed: %d", kr))
		}
		log.Printf("[Debugger] *** MAIN THREAD SUSPENDED *** (SIGTRAP equivalent)")
	}

	dbInf, err := debuginfo.NewDebugInfo(validatedPath, cmd.Process.Pid)
	if err != nil {
		panic(err)
	}
	d.DebugInfo = dbInf
	d.initialBreakpointHit()

	// Check if we were stopped during initial breakpoint
	select {
	case <-d.EndDebugSession:
		log.Println("[Debugger] Debug session ended during initial breakpoint, cleaning up")
		return
	default:
		// Continue to debug loop
	}
	log.Println("[Debugger] Starting exception loop...")
	d.exceptionLoop()

	log.Println("[Debugger] Done")
}

func (d *darwinARM64Debugger) Continue(pid int) {
	log.Printf("[Continue] Starting Continue for PID %d", pid)

	regs, err := d.getRegs()
	if err != nil {
		panic(err)
	}

	pc := regs.Pc
	log.Printf("[Continue] PC = %#x", pc)

	orig, ok := d.Breakpoints[pc]
	if !ok {
		log.Printf("[Continue] No breakpoint at %#x, resuming normally", pc)
		C.thread_resume(d.mainThread)
		return
	}

	// 1️⃣ Restore original instruction
	if err := d.writeWord(pc, orig); err != nil {
		panic(err)
	}
	log.Printf("[Continue] Restored original instruction at %#x", pc)

	// 2️⃣ Install temporary breakpoint at next instruction
	next := pc + 4
	log.Printf("[Continue] Installing temporary step breakpoint at %#x", next)

	tmpOrig, err := d.readWord(next)
	if err != nil {
		panic(err)
	}

	brk := []byte{0x20, 0x00, 0x20, 0xd4}

	if err := d.writeWord(next, brk); err != nil {
		panic(err)
	}

	// store temporary breakpoint so we can restore it later
	d.tempBreakpoint = next
	d.tempOriginal = tmpOrig
	d.tempIsStepOver = false // Continue temp breakpoint - auto-resume

	log.Printf("[Continue] Temporary breakpoint installed")

	// 3️⃣ Resume thread
	C.thread_resume(d.mainThread)
}

func (d *darwinARM64Debugger) SingleStep(pid int) {
	log.Printf("[SingleStep] Starting single step for PID %d", pid)

	// Get current PC to check if we're at a breakpoint
	regs, err := d.getRegs()
	if err != nil {
		panic(err)
	}

	pc := regs.Pc
	log.Printf("[SingleStep] Current PC = %#x", pc)

	// If we're at a breakpoint, temporarily restore the original instruction
	d.singleStepRemovedBP = 0 // Reset
	if orig, ok := d.Breakpoints[pc]; ok {
		log.Printf("[SingleStep] At breakpoint, temporarily restoring original instruction")
		if err := d.writeWord(pc, orig); err != nil {
			panic(err)
		}
		d.singleStepRemovedBP = pc // Remember we need to re-install this
	}

	// Enable single-step mode using hardware debug registers
	kr := C.enable_single_step(d.mainThread)
	if kr != C.KERN_SUCCESS {
		panic(fmt.Errorf("enable_single_step failed: %d", kr))
	}

	// Mark that we're single-stepping (CPU clears SS bit when exception fires)
	d.isSingleStepping = true

	log.Printf("[SingleStep] Hardware single-step enabled, resuming thread")

	// Resume the thread - it will execute one instruction and trap
	C.thread_resume(d.mainThread)
}

func (d *darwinARM64Debugger) StepOver(pid int) {
	log.Printf("[StepOver] Starting step over for PID %d", pid)

	// Get current location
	regs, err := d.getRegs()
	if err != nil {
		panic(err)
	}

	pc := regs.Pc - d.slide
	filename, line, _ := d.DebugInfo.PCToLine(pc)

	log.Printf("[StepOver] Current location: %s:%d", filename, line)

	// Find the next source line
	nextLine := line + 1
	nextPC, _, err := d.DebugInfo.LineToPC(filename, nextLine)
	if err != nil {
		log.Printf("[StepOver] Failed to find next line %d: %v, using single-step instead", nextLine, err)
		d.SingleStep(pid)
		return
	}

	nextRuntimePC := nextPC + d.slide
	log.Printf("[StepOver] Next line %d is at PC %#x", nextLine, nextRuntimePC)

	// If we're at a breakpoint, restore the original instruction
	currentRuntimePC := regs.Pc
	if orig, ok := d.Breakpoints[currentRuntimePC]; ok {
		log.Printf("[StepOver] Restoring original instruction at %#x", currentRuntimePC)
		if err := d.writeWord(currentRuntimePC, orig); err != nil {
			panic(err)
		}
	}

	// Set temporary breakpoint at next line
	tmpOrig, err := d.readWord(nextRuntimePC)
	if err != nil {
		panic(err)
	}

	brk := []byte{0x20, 0x00, 0x20, 0xd4}
	if err := d.writeWord(nextRuntimePC, brk); err != nil {
		panic(err)
	}

	// Store temporary breakpoint info
	d.tempBreakpoint = nextRuntimePC
	d.tempOriginal = tmpOrig
	d.tempIsStepOver = true // StepOver temp breakpoint - stop at destination

	log.Printf("[StepOver] Temporary breakpoint installed at next line, resuming")

	// Resume execution
	C.thread_resume(d.mainThread)
}

func (d *darwinARM64Debugger) StopDebug() {
	if d.DebugInfo != nil {
		syscall.Kill(d.DebugInfo.GetTarget().PID, syscall.SIGKILL)
	}

	if d.excPort != 0 {
		kr := C.cleanup_exception_port(d.excPort)
		if kr != C.KERN_SUCCESS {
			log.Printf("cleanup_exception_port failed: %d", kr)
		}
		d.excPort = 0
	}

	if d.task != 0 {
		// Clear exception ports
		C.clear_debug_exception_ports(d.task)
		// Deallocate task port
		C.mach_port_deallocate(C.get_mach_task_self(), d.task)
		d.task = 0
	}
	select {
	case d.EndDebugSession <- true:
	default:
	}

	// Ensure the next StartWithDebug begins from a clean slate even if the
	// same debugger instance is reused by the hub.
	d.resetSessionState()
}

func (d *darwinARM64Debugger) SetBreakpoint(pid int, line int) error {
	if d.DebugInfo == nil {
		return fmt.Errorf("cannot set breakpoint: debug info not initialized")
	}
	if d.task == 0 {
		return fmt.Errorf("cannot set breakpoint: debugger task is not initialized")
	}
	if d.slide == 0 {
		if err := d.computeSlide(); err != nil {
			return fmt.Errorf("cannot set breakpoint: failed to compute slide: %w", err)
		}
	}

	filePC, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return err
	}

	// convert to runtime address
	runtimePC := (filePC + d.slide)
	log.Printf("Breakpoint stored at %#x", runtimePC)

	log.Printf(
		"[Debugger] breakpoint filePC=%#x slide=%#x runtimePC=%#x",
		filePC,
		d.slide,
		runtimePC,
	)

	if kr := C.probe_address_readable(d.task, C.mach_vm_address_t(runtimePC)); kr != C.KERN_SUCCESS {
		return fmt.Errorf(
			"computed runtime breakpoint address is not readable (kr=%d): filePC=%#x slide=%#x runtimePC=%#x",
			kr,
			filePC,
			d.slide,
			runtimePC,
		)
	}

	// read original instruction
	orig, err := d.readWord(runtimePC)
	if err != nil {
		return fmt.Errorf("failed to read instruction: %w", err)
	}

	// save original instruction
	d.Breakpoints[runtimePC] = orig

	// insert BRK
	brk := []byte{0x20, 0x00, 0x20, 0xd4}

	if err := d.writeWord(runtimePC, brk); err != nil {
		return fmt.Errorf(
			"failed to write breakpoint: %w (filePC=%#x slide=%#x runtimePC=%#x)",
			err,
			filePC,
			d.slide,
			runtimePC,
		)
	}

	log.Printf("[Debugger] Breakpoint inserted at %#x", runtimePC)

	return nil
}

func (d *darwinARM64Debugger) ClearBreakpoint(pid int, line int) error {
	filePC, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return err
	}

	runtimePC := (filePC + d.slide)

	orig := d.Breakpoints[runtimePC]

	if err := d.writeWord(runtimePC, orig); err != nil {
		return fmt.Errorf("failed to restore instruction: %v", err)
	}

	return nil
}

// initialBreakpointHit handles the initial SIGTRAP and allows setting breakpoints
func (d *darwinARM64Debugger) initialBreakpointHit() {
	if err := d.computeSlide(); err != nil {
		log.Printf("slide computation failed: %v", err)
	}
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
						} else {
							log.Printf("[Debugger] Breakpoint set at line %d while at initial breakpoint", int(line))
						}
					}
				}
			case "continue":
				log.Println("[Debugger] Continuing from initial SIGTRAP equivalent")
				C.thread_resume(d.mainThread)
				return
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
	regs, err := d.getRegs()
	if err != nil {
		log.Printf("[Debugger] Failed to get registers: %v", err)
		panic(err)
	}

	if d.tempBreakpoint != 0 && regs.Pc == d.tempBreakpoint {
		log.Printf("[Debugger] Temporary breakpoint hit")

		// restore original instruction
		if err := d.writeWord(d.tempBreakpoint, d.tempOriginal); err != nil {
			panic(err)
		}

		// reinstall the user's breakpoints
		brk := []byte{0x20, 0x00, 0x20, 0xd4}

		for addr := range d.Breakpoints {
			if err := d.writeWord(addr, brk); err != nil {
				panic(err)
			}
		}

		isStepOver := d.tempIsStepOver
		d.tempBreakpoint = 0
		d.tempIsStepOver = false

		if isStepOver {
			log.Printf("[Debugger] StepOver complete, stopping at destination")
			// Fall through to normal breakpoint handling (notify user)
		} else {
			log.Printf("[Debugger] Continue step-over breakpoint complete, resuming execution")
			// automatically continue execution
			C.thread_resume(d.mainThread)
			return
		}
	}

	// Get location information (rewind PC by 4 bytes for ARM64)
	pc := regs.Pc - d.slide
	filename, line, fn := d.DebugInfo.PCToLine(pc)

	function := "<unknown>"
	if fn != nil {
		function = fn.Name
	}

	event := BreakpointEvent{
		PID:      pid,
		Filename: filename,
		Line:     line,
		Function: function,
	}
	log.Printf("runtime PC: %#x", regs.Pc)
	log.Printf("slide: %#x", d.slide)
	log.Printf("lookup PC: %#x", (regs.Pc-4)-d.slide)

	// Send breakpoint hit event to hub
	log.Printf("[Debugger] Breakpoint hit at %s:%d in %s, waiting for command", filename, line, function)
	d.BreakpointHit <- event

	// Wait for commands from hub (loop until terminal command)
	for {
		select {
		case cmd := <-d.DebugCommand:
			log.Printf("[Debugger] Received command: %s", cmd.Type)
			switch cmd.Type {
			case "continue":
				d.Continue(pid)
				return // Terminal command - resume exception loop
			case "step":
				d.StepOver(pid)
				return // Terminal command - resume exception loop
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
				// Non-terminal command - loop again to wait for next command
			case "quit":
				d.StopDebug()
				return // Terminal command
			default:
				log.Printf("[Debugger] Unknown command: %s", cmd.Type)
				// Loop again to wait for next command
			}
		case <-d.EndDebugSession:
			log.Println("[Debugger] Debug session ending, stopping breakpoint handler")
			return
		}
	}
}
