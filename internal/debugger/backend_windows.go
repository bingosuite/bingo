//go:build windows

package debugger

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func newBackend() Backend {
	return &windowsBackend{
		threads: make(map[uint32]windows.Handle),
	}
}

// windowsBackend implements Backend using the Windows Debug API.
//
// Windows debugging is event-driven: after CreateProcess or DebugActiveProcess,
// the debugger calls WaitForDebugEvent in a loop. Every debug event must be
// acknowledged with ContinueDebugEvent before the next Wait call will return.
//
// Thread handles are acquired from CREATE_THREAD_DEBUG_EVENT and
// CREATE_PROCESS_DEBUG_EVENT and stored in b.threads so that GetRegisters /
// SetRegisters can pass a valid handle to GetThreadContext / SetThreadContext.
type windowsBackend struct {
	pid      uint32
	hProc    windows.Handle
	threads  map[uint32]windows.Handle // tid → thread handle
	lastTID  uint32                    // TID of the most recent stop event
	stepping bool
}

// ── Win32 API lazy bindings ───────────────────────────────────────────────────

var (
	modKernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procWaitForDebugEvent  = modKernel32.NewProc("WaitForDebugEvent")
	procContinueDebugEvent = modKernel32.NewProc("ContinueDebugEvent")
	procReadProcessMemory  = modKernel32.NewProc("ReadProcessMemory")
	procWriteProcessMemory = modKernel32.NewProc("WriteProcessMemory")
	procGetThreadContext   = modKernel32.NewProc("GetThreadContext")
	procSetThreadContext   = modKernel32.NewProc("SetThreadContext")
	procDebugBreakProcess  = modKernel32.NewProc("DebugBreakProcess")
	procDebugActiveProcess = modKernel32.NewProc("DebugActiveProcess")
	procOpenProcess        = modKernel32.NewProc("OpenProcess")
)

// ── DEBUG_EVENT ───────────────────────────────────────────────────────────────

// debugEvent mirrors the Windows DEBUG_EVENT structure.
// The union payload (u) is sized for the largest member.
//
//	typedef struct _DEBUG_EVENT {
//	  DWORD dwDebugEventCode;
//	  DWORD dwProcessId;
//	  DWORD dwThreadId;
//	  union { ... } u;  // largest member is EXCEPTION_DEBUG_INFO (152 bytes)
//	} DEBUG_EVENT;
type debugEvent struct {
	DebugEventCode uint32
	ProcessId      uint32
	ThreadId       uint32
	_              [4]byte   // struct alignment padding to 8-byte boundary
	union          [152]byte // union payload — sized for EXCEPTION_DEBUG_INFO
}

// Debug event codes.
const (
	evException      = 1
	evCreateThread   = 2
	evCreateProcess  = 3
	evExitThread     = 4
	evExitProcess    = 5
	evLoadDll        = 6
	evUnloadDll      = 7
	evOutputDebugStr = 8
	evRipEvent       = 9
)

// ContinueDebugEvent status codes.
const (
	dbgContinue            = 0x00010002
	dbgExceptionNotHandled = 0x80010001
)

// Exception codes.
const (
	excBreakpoint = 0x80000003
	excSingleStep = 0x80000004
)

// CREATE_PROCESS_DEBUG_INFO layout (64-bit):
//
//	0:  hFile        (8)
//	8:  hProcess     (8)
//	16: hThread      (8)
//	24: lpBaseOfImage (8)
//	...
const (
	cpdiHProcess = 8
	cpdiHThread  = 16
)

// CREATE_THREAD_DEBUG_INFO layout (64-bit):
//
//	0: hThread         (8)
//	8: lpThreadLocalBase (8)
//	16: lpStartAddress  (8)
const ctdiHThread = 0

// EXCEPTION_DEBUG_INFO layout:
//
//	0: ExceptionRecord.ExceptionCode (4)
//	4: ExceptionRecord.ExceptionFlags (4)
//	8: ExceptionRecord.ExceptionRecord* (8)
//	16: ExceptionRecord.ExceptionAddress (8)
//	...
//	152: dwFirstChance (4)
const exdiExceptionCode = 0

// EXIT_PROCESS_DEBUG_INFO layout:
//
//	0: dwExitCode (4)
const epdiExitCode = 0

// ── Process lifecycle ─────────────────────────────────────────────────────────

func startTracedProcess(binaryPath string, args []string, env []string) (int, *exec.Cmd, error) {
	// DEBUG_ONLY_THIS_PROCESS: deliver debug events to us without inheriting
	// to child processes spawned by the debuggee.
	const DEBUG_ONLY_THIS_PROCESS = 0x00000002

	appPtr, err := syscall.UTF16PtrFromString(binaryPath)
	if err != nil {
		return 0, nil, fmt.Errorf("UTF16PtrFromString %q: %w", binaryPath, err)
	}

	var si windows.StartupInfo
	var pi windows.ProcessInformation
	si.Cb = uint32(unsafe.Sizeof(si))

	if err := windows.CreateProcess(
		appPtr, nil, nil, nil, false,
		DEBUG_ONLY_THIS_PROCESS,
		nil, nil, &si, &pi,
	); err != nil {
		return 0, nil, fmt.Errorf("CreateProcess: %w", err)
	}

	// The process and main-thread handles will be captured from the first
	// CREATE_PROCESS_DEBUG_EVENT in Wait(). We do not need pi.Process or
	// pi.Thread here; close them to avoid handle leaks.
	_ = windows.CloseHandle(pi.Thread)
	// Keep pi.Process open temporarily; it will be replaced by the handle
	// from the debug event (which carries full debug access rights).
	_ = windows.CloseHandle(pi.Process)

	return int(pi.ProcessId), nil, nil
}

func attachToProcess(pid int) error {
	r, _, e := procDebugActiveProcess.Call(uintptr(pid))
	if r == 0 {
		return fmt.Errorf("DebugActiveProcess pid %d: %w", pid, e)
	}
	return nil
}

func killProcess(pid int, cmd *exec.Cmd) error {
	if cmd != nil {
		return cmd.Process.Kill()
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return p.Kill()
}

// ── Backend implementation ────────────────────────────────────────────────────

func (b *windowsBackend) ContinueProcess() error {
	b.stepping = false
	r, _, e := procContinueDebugEvent.Call(
		uintptr(b.pid),
		uintptr(b.lastTID),
		uintptr(dbgContinue),
	)
	if r == 0 {
		return fmt.Errorf("ContinueDebugEvent pid=%d tid=%d: %w", b.pid, b.lastTID, e)
	}
	return nil
}

func (b *windowsBackend) SingleStep(tid int) error {
	b.stepping = true
	// Set the Trap Flag in the thread's EFLAGS register so the CPU delivers
	// an EXCEPTION_SINGLE_STEP after the next instruction.
	regs, err := b.GetRegisters(tid)
	if err != nil {
		return err
	}
	regs.EFlags |= 0x100 // TF bit
	if err := b.SetRegisters(tid, regs); err != nil {
		return err
	}
	// Resume the thread so it actually executes one instruction.
	r, _, e := procContinueDebugEvent.Call(
		uintptr(b.pid),
		uintptr(uint32(tid)),
		uintptr(dbgContinue),
	)
	if r == 0 {
		return fmt.Errorf("ContinueDebugEvent (singlestep) pid=%d tid=%d: %w", b.pid, tid, e)
	}
	return nil
}

func (b *windowsBackend) StopProcess() error {
	r, _, e := procDebugBreakProcess.Call(uintptr(b.hProc))
	if r == 0 {
		return fmt.Errorf("DebugBreakProcess: %w", e)
	}
	return nil
}

func (b *windowsBackend) ReadMemory(addr uint64, dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	var read uintptr
	r, _, e := procReadProcessMemory.Call(
		uintptr(b.hProc),
		uintptr(addr),
		uintptr(unsafe.Pointer(&dst[0])),
		uintptr(len(dst)),
		uintptr(unsafe.Pointer(&read)),
	)
	if r == 0 {
		return fmt.Errorf("ReadProcessMemory 0x%x: %w", addr, e)
	}
	if int(read) != len(dst) {
		return fmt.Errorf("ReadProcessMemory 0x%x: short read %d/%d", addr, read, len(dst))
	}
	return nil
}

func (b *windowsBackend) WriteMemory(addr uint64, src []byte) error {
	if len(src) == 0 {
		return nil
	}
	var written uintptr
	r, _, e := procWriteProcessMemory.Call(
		uintptr(b.hProc),
		uintptr(addr),
		uintptr(unsafe.Pointer(&src[0])),
		uintptr(len(src)),
		uintptr(unsafe.Pointer(&written)),
	)
	if r == 0 {
		return fmt.Errorf("WriteProcessMemory 0x%x: %w", addr, e)
	}
	return nil
}

func (b *windowsBackend) GetRegisters(tid int) (Registers, error) {
	h, ok := b.threads[uint32(tid)]
	if !ok {
		return Registers{}, fmt.Errorf("GetRegisters: unknown thread %d", tid)
	}
	return windowsGetRegisters(h)
}

func (b *windowsBackend) SetRegisters(tid int, reg Registers) error {
	h, ok := b.threads[uint32(tid)]
	if !ok {
		return fmt.Errorf("SetRegisters: unknown thread %d", tid)
	}
	return windowsSetRegisters(h, reg)
}

func (b *windowsBackend) Threads() ([]int, error) {
	if len(b.threads) == 0 {
		return nil, fmt.Errorf("Threads: no threads known (has Wait been called?)")
	}
	out := make([]int, 0, len(b.threads))
	for tid := range b.threads {
		out = append(out, int(tid))
	}
	return out, nil
}

// Wait blocks until the tracee produces a meaningful stop event.
//
// CREATE_PROCESS_DEBUG_EVENT, CREATE_THREAD_DEBUG_EVENT, EXIT_THREAD_DEBUG_EVENT,
// LOAD_DLL_DEBUG_EVENT, UNLOAD_DLL_DEBUG_EVENT, and OUTPUT_DEBUG_STRING_DEBUG_EVENT
// are handled internally: the thread is continued and Wait loops again.
//
// Only EXCEPTION_DEBUG_EVENT and EXIT_PROCESS_DEBUG_EVENT surface to the engine.
func (b *windowsBackend) Wait() (StopEvent, error) {
	for {
		var evt debugEvent
		r, _, e := procWaitForDebugEvent.Call(
			uintptr(unsafe.Pointer(&evt)),
			0xFFFFFFFF, // INFINITE timeout
		)
		if r == 0 {
			return StopEvent{}, fmt.Errorf("WaitForDebugEvent: %w", e)
		}

		tid := int(evt.ThreadId)
		b.lastTID = evt.ThreadId

		switch evt.DebugEventCode {

		case evCreateProcess:
			// Capture process and main-thread handles from the event.
			b.pid = evt.ProcessId
			b.hProc = windows.Handle(readPtr(evt.union[:], cpdiHProcess))
			hThread := windows.Handle(readPtr(evt.union[:], cpdiHThread))
			b.threads[evt.ThreadId] = hThread
			// Close the file handle (hFile) to avoid a handle leak.
			hFile := windows.Handle(readPtr(evt.union[:], 0))
			if hFile != 0 && hFile != windows.InvalidHandle {
				_ = windows.CloseHandle(hFile)
			}
			continueEvent(b.pid, evt.ThreadId)

		case evCreateThread:
			hThread := windows.Handle(readPtr(evt.union[:], ctdiHThread))
			b.threads[evt.ThreadId] = hThread
			continueEvent(b.pid, evt.ThreadId)

		case evExitThread:
			if h, ok := b.threads[evt.ThreadId]; ok {
				_ = windows.CloseHandle(h)
				delete(b.threads, evt.ThreadId)
			}
			continueEvent(b.pid, evt.ThreadId)

		case evExitProcess:
			code := *(*uint32)(unsafe.Pointer(&evt.union[epdiExitCode]))
			return StopEvent{
				Reason:   StopExited,
				TID:      tid,
				ExitCode: int(code),
			}, nil

		case evException:
			exCode := *(*uint32)(unsafe.Pointer(&evt.union[exdiExceptionCode]))
			regs, err := b.GetRegisters(tid)
			if err != nil {
				continueEvent(b.pid, evt.ThreadId)
				return StopEvent{}, err
			}

			switch exCode {
			case excBreakpoint:
				// On Windows the initial breakpoint on process attach/launch
				// is the loader's int3. archRewindPC handles the PC adjustment.
				return StopEvent{
					Reason: StopBreakpoint,
					TID:    tid,
					PC:     archRewindPC(regs.PC),
				}, nil

			case excSingleStep:
				return StopEvent{
					Reason: StopSingleStep,
					TID:    tid,
					PC:     regs.PC,
				}, nil

			default:
				// Other exception (access violation, etc.) — surface as signal.
				return StopEvent{
					Reason: StopSignal,
					TID:    tid,
					PC:     regs.PC,
					Signal: int(exCode),
				}, nil
			}

		default:
			// LOAD_DLL, UNLOAD_DLL, OUTPUT_DEBUG_STRING, RIP — continue silently.
			continueEvent(b.pid, evt.ThreadId)
		}
	}
}

// continueEvent sends ContinueDebugEvent for bookkeeping events.
func continueEvent(pid, tid uint32) {
	_, _, _ = procContinueDebugEvent.Call(
		uintptr(pid), uintptr(tid), uintptr(dbgContinue))
}

// readPtr reads a pointer-sized value from b at byte offset off.
func readPtr(b []byte, off int) uintptr {
	p := b[off:]
	if unsafe.Sizeof(uintptr(0)) == 8 {
		return uintptr(uint64(p[0]) | uint64(p[1])<<8 | uint64(p[2])<<16 | uint64(p[3])<<24 |
			uint64(p[4])<<32 | uint64(p[5])<<40 | uint64(p[6])<<48 | uint64(p[7])<<56)
	}
	return uintptr(uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16 | uint32(p[3])<<24)
}

var _ Backend = (*windowsBackend)(nil)

func (b *windowsBackend) setPID(pid int) { b.pid = uint32(pid) }
