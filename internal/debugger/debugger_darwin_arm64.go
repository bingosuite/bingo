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
	"sync"
	"syscall"
	"unsafe"

	"github.com/bingosuite/bingo/internal/debuginfo"
)

const (
	logPrefixDebugger      = "[Debugger]"
	logPrefixExceptionLoop = "[ExceptionLoop]"

	KERN_FAILURE_CODE = 5
)

var arm64BreakpointBytes = []byte{0x20, 0x00, 0x20, 0xd4}

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
	DebugInfo   debuginfo.DebugInfo
	Breakpoints map[uint64][]byte
	slide       uint64

	// stop is closed by StopDebug to signal all internal loops to exit.
	stop     chan struct{}
	stopOnce sync.Once

	// Communication with hub
	DebuggerEvents chan DebuggerEvent
	DebugCommand   chan DebugCommand

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

// resetSessionState clears all per-session debugger state so a debugger
// instance can be safely reused for a new target process.
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

// NewDebugger creates a Darwin ARM64 debugger implementation wired to the
// hub communication channels.
func NewDebugger(debuggerEvents chan DebuggerEvent, debugCommand chan DebugCommand) Debugger {
	return &darwinARM64Debugger{
		Breakpoints:    make(map[uint64][]byte),
		stop:           make(chan struct{}),
		DebuggerEvents: debuggerEvents,
		DebugCommand:   debugCommand,
	}
}

// sendEvent sends an event to the hub, aborting silently if the session is stopping.
func (d *darwinARM64Debugger) sendEvent(event DebuggerEvent) {
	select {
	case d.DebuggerEvents <- event:
	case <-d.stop:
	}
}

// computeSlide resolves and stores the target image ASLR slide used to
// translate file addresses to runtime addresses.
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
	log.Printf("%s ASLR slide = %#x", logPrefixDebugger, d.slide)
	return nil
}

// validateTargetPath returns a safe absolute executable path by normalizing the
// user input, restricting it to the current workspace, and validating file
// type and execute permissions.
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

// getRegs reads the current ARM64 register state from the tracked main thread.
func (d *darwinARM64Debugger) getRegs() (*ARM64ThreadState, error) {
	var st ARM64ThreadState
	count := C.mach_msg_type_number_t(C.ARM_THREAD_STATE64_COUNT)
	kr := C.get_arm64_thread_state(d.mainThread, (*C.arm_thread_state64_t)(unsafe.Pointer(&st)), &count)
	if kr != C.KERN_SUCCESS {
		return nil, fmt.Errorf("thread_get_state failed: %d", kr)
	}
	return &st, nil
}

// readWord reads one 32-bit instruction word from target process memory.
func (d *darwinARM64Debugger) readWord(addr uint64) ([]byte, error) {
	var word C.uint32_t
	kr := C.read_word(d.task, C.mach_vm_address_t(addr), &word)
	if kr != C.KERN_SUCCESS {
		return nil, fmt.Errorf("read_word failed: %d", kr)
	}
	v := uint32(word)
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}, nil
}

// writeWord writes one 32-bit instruction word into target process memory.
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

// exceptionLoop waits for Mach exception messages and converts breakpoint
// exceptions into debugger events while keeping thread-exception reply flow
// valid for the kernel.
func (d *darwinARM64Debugger) exceptionLoop() {
	runtime.LockOSThread() // Mach msg requires fixed thread
	defer runtime.UnlockOSThread()

	for {
		select {
		case <-d.stop:
			return
		default:
		}

		var excBuf [4096]byte
		var reply C.exc_msg_reply_t

		log.Printf("%s waiting for exception message", logPrefixExceptionLoop)

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
			log.Printf("%s mach_msg receive failed: %d", logPrefixExceptionLoop, kr)
			continue
		}
		log.Printf("%s received exception message", logPrefixExceptionLoop)

		excMsg := (*C.exc_msg_t)(unsafe.Pointer(&excBuf[0]))

		// Check if it's a breakpoint exception
		if excMsg.exception == C.EXC_BREAKPOINT || excMsg.exception == C.EXC_BAD_INSTRUCTION {
			log.Printf("%s breakpoint exception received", logPrefixExceptionLoop)
			thread := C.exc_msg_thread(excMsg)
			if d.mainThread == 0 {
				d.mainThread = thread
			}

			// Check if we just completed a single-step
			if d.isSingleStepping {
				C.disable_single_step(d.mainThread)
				d.isSingleStepping = false

				// Re-install any breakpoint we temporarily removed for single-stepping
				if d.singleStepRemovedBP != 0 {
					if err := d.writeWord(d.singleStepRemovedBP, arm64BreakpointBytes); err != nil {
						log.Printf("%s failed to re-install breakpoint: %v", logPrefixExceptionLoop, err)
					}
					d.singleStepRemovedBP = 0
				}
			}

			if d.DebugInfo == nil {
				log.Printf("%s handling initial breakpoint", logPrefixExceptionLoop)
				d.initialBreakpointHit()
			} else {
				d.breakpointHit(d.DebugInfo.GetTarget().PID)
			}

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
				log.Printf("%s failed to send exception reply: %d", logPrefixExceptionLoop, replyKr)
			}
			C.destroy_mach_message((*C.mach_msg_header_t)(unsafe.Pointer(&excBuf[0])))
			C.thread_resume(d.mainThread)
		} else {
			// Unknown exception - pass to next handler
			reply.Head.msgh_bits = C.make_reply_bits(excMsg.Head.msgh_bits)
			reply.Head.msgh_size = C.mach_msg_size_t(unsafe.Sizeof(reply))
			reply.Head.msgh_remote_port = excMsg.Head.msgh_remote_port
			reply.Head.msgh_local_port = C.MACH_PORT_NULL
			reply.Head.msgh_id = excMsg.Head.msgh_id + 100

			reply.RetCode = KERN_FAILURE_CODE

			C.mach_msg(
				(*C.mach_msg_header_t)(unsafe.Pointer(&reply)),
				C.MACH_SEND_MSG,
				C.mach_msg_size_t(unsafe.Sizeof(reply)),
				0,
				C.MACH_PORT_NULL,
				C.MACH_MSG_TIMEOUT_NONE,
				C.MACH_PORT_NULL,
			)
			C.destroy_mach_message((*C.mach_msg_header_t)(unsafe.Pointer(&excBuf[0])))
		}
	}
}

// StartWithDebug launches the target binary under debugger control, initializes
// task/exception ports, emits the initial stop event, and enters the exception
// handling loop.
func (d *darwinARM64Debugger) StartWithDebug(path string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// notifyEnd always sends a SessionEndedEvent before returning.
	notifyEnd := func(err error) { d.sendEvent(SessionEndedEvent{Err: err}) }

	d.resetSessionState()

	validatedPath, err := validateTargetPath(path)
	if err != nil {
		log.Printf("%s rejected target path %q: %v", logPrefixDebugger, path, err)
		notifyEnd(err)
		return
	}

	cmd := exec.Command(validatedPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = nil
	if err := cmd.Start(); err != nil {
		notifyEnd(err)
		return
	}

	pid := cmd.Process.Pid
	log.Printf("%s started target PID %d", logPrefixDebugger, pid)

	// Watch for process exit
	go func() {
		var status syscall.WaitStatus
		_, err := syscall.Wait4(pid, &status, 0, nil)
		if err != nil {
			log.Printf("%s wait4 error: %v", logPrefixDebugger, err)
		}

		log.Printf("%s target process exited: %v", logPrefixDebugger, status)

		// Signal debugger shutdown.
		d.stopOnce.Do(func() { close(d.stop) })
	}()

	// Get task port
	var task C.mach_port_t
	kr := C.task_for_pid(C.get_mach_task_self(), C.int(cmd.Process.Pid), &task)
	if kr != C.KERN_SUCCESS {
		notifyEnd(fmt.Errorf("task_for_pid failed: %d", kr))
		return
	}
	d.task = task

	// Set up exception port
	var excPort C.mach_port_t
	kr = C.mach_port_allocate(C.get_mach_task_self(), C.MACH_PORT_RIGHT_RECEIVE, &excPort)
	if kr != C.KERN_SUCCESS {
		notifyEnd(fmt.Errorf("mach_port_allocate failed: %d", kr))
		return
	}
	d.excPort = excPort
	log.Printf("%s exception port allocated: %d", logPrefixDebugger, d.excPort)

	kr = C.mach_port_insert_right(
		C.get_mach_task_self(),
		excPort,
		excPort,
		C.MACH_MSG_TYPE_MAKE_SEND,
	)
	if kr != C.KERN_SUCCESS {
		notifyEnd(fmt.Errorf("mach_port_insert_right failed: %d", kr))
		return
	}

	kr = C.set_debug_exception_ports(d.task, excPort)
	if kr != C.KERN_SUCCESS {
		notifyEnd(fmt.Errorf("task_set_exception_ports failed: %d", kr))
		return
	}

	kr = C.set_thread_exception_ports(d.task, excPort)
	if kr != C.KERN_SUCCESS {
		notifyEnd(fmt.Errorf("thread_set_exception_ports failed: %d", kr))
		return
	}

	// Get initial threads
	var firstThread C.thread_act_t
	kr = C.get_first_thread(d.task, &firstThread)
	if kr == C.KERN_SUCCESS {
		d.mainThread = firstThread
		kr = C.thread_suspend(firstThread)
		if kr != C.KERN_SUCCESS {
			notifyEnd(fmt.Errorf("thread_suspend failed: %d", kr))
			return
		}
	}

	dbInf, err := debuginfo.NewDebugInfo(validatedPath, cmd.Process.Pid)
	if err != nil {
		notifyEnd(err)
		return
	}
	d.DebugInfo = dbInf
	d.initialBreakpointHit()

	select {
	case <-d.stop:
		log.Printf("%s debug session ended during initial breakpoint", logPrefixDebugger)
		notifyEnd(nil)
		return
	default:
		// Continue to debug loop
	}

	d.exceptionLoop()
	notifyEnd(nil)
}

// Continue restores the current breakpointed instruction, places a temporary
// trap at the following instruction, and resumes execution.
func (d *darwinARM64Debugger) Continue(pid int) {
	log.Printf("%s continuing execution for PID %d", logPrefixDebugger, pid)

	regs, err := d.getRegs()
	if err != nil {
		panic(err)
	}

	pc := regs.Pc
	orig, ok := d.Breakpoints[pc]
	if !ok {
		log.Printf("%s no breakpoint at %#x, resuming normally", logPrefixDebugger, pc)
		C.thread_resume(d.mainThread)
		return
	}

	// Restore original instruction
	if err := d.writeWord(pc, orig); err != nil {
		panic(err)
	}

	// Install temporary breakpoint at next instruction
	next := pc + 4

	tmpOrig, err := d.readWord(next)
	if err != nil {
		panic(err)
	}

	if err := d.writeWord(next, arm64BreakpointBytes); err != nil {
		panic(err)
	}

	d.tempBreakpoint = next
	d.tempOriginal = tmpOrig
	d.tempIsStepOver = false

	C.thread_resume(d.mainThread)
}

// SingleStep executes exactly one instruction by enabling CPU single-step and
// temporarily removing any breakpoint at the current program counter.
func (d *darwinARM64Debugger) SingleStep(pid int) {
	log.Printf("%s single-stepping for PID %d", logPrefixDebugger, pid)

	// Get current PC to check if we're at a breakpoint
	regs, err := d.getRegs()
	if err != nil {
		panic(err)
	}

	pc := regs.Pc
	d.singleStepRemovedBP = 0
	if orig, ok := d.Breakpoints[pc]; ok {
		if err := d.writeWord(pc, orig); err != nil {
			panic(err)
		}
		d.singleStepRemovedBP = pc
	}

	kr := C.enable_single_step(d.mainThread)
	if kr != C.KERN_SUCCESS {
		panic(fmt.Errorf("enable_single_step failed: %d", kr))
	}

	d.isSingleStepping = true
	C.thread_resume(d.mainThread)
}

// StepOver advances to the next source line by setting a temporary breakpoint
// on the next line address, falling back to single-step when needed.
func (d *darwinARM64Debugger) StepOver(pid int) {
	log.Printf("%s step over for PID %d", logPrefixDebugger, pid)

	regs, err := d.getRegs()
	if err != nil {
		panic(err)
	}

	pc := regs.Pc - d.slide
	filename, line, _ := d.DebugInfo.PCToLine(pc)

	nextLine := line + 1
	nextPC, _, err := d.DebugInfo.LineToPC(filename, nextLine)
	if err != nil {
		d.SingleStep(pid)
		return
	}

	nextRuntimePC := nextPC + d.slide

	// If we're at a breakpoint, restore the original instruction
	currentRuntimePC := regs.Pc
	if orig, ok := d.Breakpoints[currentRuntimePC]; ok {
		log.Printf("%s restoring original instruction at %#x", logPrefixDebugger, currentRuntimePC)
		if err := d.writeWord(currentRuntimePC, orig); err != nil {
			panic(err)
		}
	}

	// Set temporary breakpoint at next line
	tmpOrig, err := d.readWord(nextRuntimePC)
	if err != nil {
		panic(err)
	}

	if err := d.writeWord(nextRuntimePC, arm64BreakpointBytes); err != nil {
		panic(err)
	}

	// Store temporary breakpoint info
	d.tempBreakpoint = nextRuntimePC
	d.tempOriginal = tmpOrig
	d.tempIsStepOver = true

	C.thread_resume(d.mainThread)
}

// StopDebug terminates the debugged process (if running), releases Mach ports,
// and notifies listeners that the debug session has ended.
func (d *darwinARM64Debugger) StopDebug() {
	log.Printf("%s stopping debug session", logPrefixDebugger)
	if d.DebugInfo != nil {
		syscall.Kill(d.DebugInfo.GetTarget().PID, syscall.SIGKILL)
	}

	if d.mainThread != 0 {
		if kr := C.release_thread_port(d.mainThread); kr != C.KERN_SUCCESS {
			log.Printf("%s release_thread_port failed: %d", logPrefixDebugger, kr)
		}
		d.mainThread = 0
	}

	if d.excPort != 0 {
		kr := C.cleanup_exception_port(d.excPort)
		if kr != C.KERN_SUCCESS {
			log.Printf("%s cleanup_exception_port failed: %d", logPrefixDebugger, kr)
		}
		d.excPort = 0
	}

	if d.task != 0 {
		C.clear_debug_exception_ports(d.task)
		C.mach_port_deallocate(C.get_mach_task_self(), d.task)
		d.task = 0
	}

	d.stopOnce.Do(func() { close(d.stop) })
}

// SetBreakpoint installs an ARM64 software breakpoint at the requested source
// line after resolving it to a runtime address with ASLR slide.
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

	runtimePC := filePC + d.slide

	if kr := C.probe_address_readable(d.task, C.mach_vm_address_t(runtimePC)); kr != C.KERN_SUCCESS {
		return fmt.Errorf("breakpoint address not readable (kr=%d): runtimePC=%#x", kr, runtimePC)
	}

	orig, err := d.readWord(runtimePC)
	if err != nil {
		return fmt.Errorf("failed to read instruction: %w", err)
	}

	d.Breakpoints[runtimePC] = orig

	// Insert breakpoint
	if err := d.writeWord(runtimePC, arm64BreakpointBytes); err != nil {
		return fmt.Errorf("failed to write breakpoint at %#x: %w", runtimePC, err)
	}

	return nil
}

// ClearBreakpoint restores the original instruction at the requested source
// line.
func (d *darwinARM64Debugger) ClearBreakpoint(pid int, line int) error {
	filePC, _, err := d.DebugInfo.LineToPC(d.DebugInfo.GetTarget().Path, line)
	if err != nil {
		return err
	}

	runtimePC := filePC + d.slide
	orig := d.Breakpoints[runtimePC]

	if err := d.writeWord(runtimePC, orig); err != nil {
		return fmt.Errorf("failed to restore instruction: %v", err)
	}

	return nil
}

// initialBreakpointHit publishes the initial stop event and processes commands
// allowed before normal execution resumes.
func (d *darwinARM64Debugger) initialBreakpointHit() {
	if err := d.computeSlide(); err != nil {
		log.Printf("%s slide computation failed: %v", logPrefixDebugger, err)
	}

	event := InitialBreakpointHitEvent{
		PID: d.DebugInfo.GetTarget().PID,
	}

	log.Printf("%s ready for commands", logPrefixDebugger)
	d.sendEvent(event)

	// Wait for commands from hub
	for {
		select {
		case cmd := <-d.DebugCommand:
			log.Printf("%s received command: %s", logPrefixDebugger, cmd.Type)

			switch cmd.Type {
			case "setBreakpoint":
				if data, ok := cmd.Data.(map[string]any); ok {
					if line, ok := data["line"].(int); ok {
						if err := d.SetBreakpoint(d.DebugInfo.GetTarget().PID, int(line)); err != nil {
							log.Printf("%s failed to set breakpoint at line %d: %v", logPrefixDebugger, int(line), err)
						} else {
							log.Printf("%s breakpoint set at line %d", logPrefixDebugger, int(line))
						}
					}
				}
			case "continue":
				log.Printf("%s continuing execution", logPrefixDebugger)
				C.thread_resume(d.mainThread)
				return
			case "stepOver":
				log.Printf("%s cannot stepover from initial breakpoint", logPrefixDebugger)
			case "singleStep":
				log.Printf("%s cannot single-step from initial breakpoint", logPrefixDebugger)
			case "quit":
				d.StopDebug()
				return
			default:
				log.Printf("%s unknown command: %s", logPrefixDebugger, cmd.Type)
			}
		case <-d.stop:
			log.Printf("%s debug session ended during initial breakpoint", logPrefixDebugger)
			return
		}
	}
}

// breakpointHit resolves the current PC to source location, publishes a
// breakpoint event, and processes runtime debugger commands.
func (d *darwinARM64Debugger) breakpointHit(pid int) {
	log.Printf("%s breakpoint hit for PID %d", logPrefixDebugger, pid)
	// Get register information to determine location
	regs, err := d.getRegs()
	if err != nil {
		log.Printf("%s failed to get registers: %v", logPrefixDebugger, err)
		panic(err)
	}

	if d.tempBreakpoint != 0 && regs.Pc == d.tempBreakpoint {
		if err := d.writeWord(d.tempBreakpoint, d.tempOriginal); err != nil {
			panic(err)
		}

		for addr := range d.Breakpoints {
			if err := d.writeWord(addr, arm64BreakpointBytes); err != nil {
				panic(err)
			}
		}

		isStepOver := d.tempIsStepOver
		d.tempBreakpoint = 0
		d.tempIsStepOver = false

		if !isStepOver {
			C.thread_resume(d.mainThread)
			return
		}
	}

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

	log.Printf("%s breakpoint at %s:%d in %s", logPrefixDebugger, filename, line, function)
	d.sendEvent(event)

	// Wait for commands from hub
	for {
		select {
		case cmd := <-d.DebugCommand:
			log.Printf("%s received command: %s", logPrefixDebugger, cmd.Type)
			switch cmd.Type {
			case "continue":
				d.Continue(pid)
				return
			case "stepOver":
				d.StepOver(pid)
				return
			case "singleStep":
				d.SingleStep(pid)
				return
			case "setBreakpoint":
				if data, ok := cmd.Data.(map[string]any); ok {
					if line, ok := data["line"].(int); ok {
						if err := d.SetBreakpoint(pid, line); err != nil {
							log.Printf("%s failed to set breakpoint at line %d: %v", logPrefixDebugger, line, err)
						}
					}
				}
			case "quit":
				d.StopDebug()
				return
			default:
				log.Printf("%s unknown command: %s", logPrefixDebugger, cmd.Type)
			}
		case <-d.stop:
			log.Printf("%s debug session ended", logPrefixDebugger)
			return
		}
	}
}
