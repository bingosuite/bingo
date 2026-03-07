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

func (d *darwinARM64Debugger) setRegs(st *ARM64ThreadState) error {
	count := C.mach_msg_type_number_t(C.ARM_THREAD_STATE64_COUNT)
	kr := C.set_arm64_thread_state(d.mainThread, (*C.arm_thread_state64_t)(unsafe.Pointer(st)), count)
	if kr != C.KERN_SUCCESS {
		return fmt.Errorf("thread_set_state failed: %d", kr)
	}
	return nil
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
	regs, err := d.getRegs()
	if err != nil {
		panic(err)
	}

	bpAddr := regs.Pc

	log.Printf("Continue lookup addr %#x", bpAddr)

	orig, ok := d.Breakpoints[bpAddr]
	if !ok {
		panic(fmt.Sprintf("breakpoint not found at %#x", bpAddr))
	}

	// restore instruction
	if err := d.writeWord(bpAddr, orig); err != nil {
		panic(err)
	}

	// rewind PC so the instruction executes
	regs.Pc = bpAddr

	// enable single-step
	regs.Cpsr |= 1 << 21

	if err := d.setRegs(regs); err != nil {
		panic(err)
	}

	C.thread_resume(d.mainThread)
}

// singleStepThread sets up single-step mode then resumes (your ptraceSingleStep replacement)
func (d *darwinARM64Debugger) singleStepThread() error {
	regs, err := d.getRegs()
	if err != nil {
		return err
	}

	// Enable single-step by setting SS bit in CPSR (bit 21)
	regs.Cpsr |= 1 << 21
	return d.setRegs(regs)
}

func (d *darwinARM64Debugger) SingleStep(pid int) {
	d.singleStepThread() // Sets SS bit, exception loop handles next stop
}

func (d *darwinARM64Debugger) StopDebug() {
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
}

func (d *darwinARM64Debugger) SetBreakpoint(pid int, line int) error {
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
		return fmt.Errorf("failed to write breakpoint: %w", err)
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

	if d.slide == 0 {
		if err := d.computeSlide(); err != nil {
			log.Printf("slide computation failed: %v", err)
		}
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

	// Check if this was a single-step trap
	if (regs.Cpsr & (1 << 21)) != 0 {
		// Disable single step
		regs.Cpsr &^= 1 << 21
		d.setRegs(regs)

		// Reinsert breakpoints
		for addr := range d.Breakpoints {
			runtime := addr + d.slide
			brk := []byte{0x20, 0x00, 0x20, 0xd4}
			d.writeWord(runtime, brk)
		}

		return
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
				d.SingleStep(pid)
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
