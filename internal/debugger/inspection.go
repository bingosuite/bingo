package debugger

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type goroutineInspector interface {
	StackFramesForGoroutine(int) ([]protocol.Frame, error)
	LocalsForGoroutine(int, int) ([]protocol.Variable, error)
	EvaluateForGoroutine(int, int, string) (protocol.Variable, error)
}

type inspectionEndedError struct {
	stop StopEvent
}

func (e *inspectionEndedError) Error() string { return "process exited during goroutine inspection" }
func (e *inspectionEndedError) Unwrap() error { return ErrSessionInvalidated }

// These helpers preserve legacy Debugger implementations without silently
// serving the stopped thread for an unsupported selected goroutine.
func StackFramesForGoroutine(d Debugger, goid int) ([]protocol.Frame, error) {
	if goid == 0 {
		return d.StackFrames()
	}
	if inspector, ok := d.(goroutineInspector); ok {
		return inspector.StackFramesForGoroutine(goid)
	}
	return nil, fmt.Errorf("debugger does not support selected-goroutine inspection")
}

func LocalsForGoroutine(d Debugger, goid, frame int) ([]protocol.Variable, error) {
	if goid == 0 {
		return d.Locals(frame)
	}
	if inspector, ok := d.(goroutineInspector); ok {
		return inspector.LocalsForGoroutine(goid, frame)
	}
	return nil, fmt.Errorf("debugger does not support selected-goroutine inspection")
}

func EvaluateForGoroutine(d Debugger, goid, frame int, name string) (protocol.Variable, error) {
	if goid == 0 {
		return d.Evaluate(frame, name)
	}
	if inspector, ok := d.(goroutineInspector); ok {
		return inspector.EvaluateForGoroutine(goid, frame, name)
	}
	return protocol.Variable{}, fmt.Errorf("debugger does not support selected-goroutine inspection")
}

func (e *engine) StackFramesForGoroutine(goid int) ([]protocol.Frame, error) {
	var frames []protocol.Frame
	err := e.dispatch(func() error {
		regs, err := e.goroutineRegisters(goid)
		if err != nil {
			return err
		}
		frames = e.dw.FramesForStack(e.walkStack(regs))
		return nil
	})
	return frames, err
}

func (e *engine) LocalsForGoroutine(goid, frame int) ([]protocol.Variable, error) {
	var vars []protocol.Variable
	err := e.dispatch(func() error {
		regs, err := e.goroutineRegisters(goid)
		if err != nil {
			return err
		}
		pc, base, err := e.frameLocationFromRegisters(frame, regs)
		if err != nil {
			return err
		}
		vars, err = e.dw.LocalsForFrame(e.backend, pc, base)
		return err
	})
	return vars, err
}

func (e *engine) EvaluateForGoroutine(goid, frame int, name string) (protocol.Variable, error) {
	var result protocol.Variable
	err := e.dispatch(func() error {
		regs, err := e.goroutineRegisters(goid)
		if err != nil {
			return err
		}
		pc, base, err := e.frameLocationFromRegisters(frame, regs)
		if err != nil {
			return err
		}
		result, err = e.dw.EvaluateName(e.backend, pc, base, name)
		return err
	})
	return result, err
}

// stoppedInspectionThreads must prove the whole runtime cannot schedule or
// copy a parked stack. A sampled status, or a second identical read, is not that
// proof: Linux siblings can resume a goroutine between the two reads.
type stoppedInspectionThreads interface {
	inspectionThreads() ([]int, error)
}

func (e *engine) goroutineRegisters(goid int) (Registers, error) {
	if err := e.requireSuspended(); err != nil {
		return Registers{}, err
	}
	if e.wait != nil {
		return Registers{}, fmt.Errorf("goroutine inspection requires acknowledged waiter retirement")
	}
	if goid <= 0 || uint64(goid) > 1<<53-1 {
		return Registers{}, fmt.Errorf("invalid goroutine id %d", goid)
	}
	l, ok := e.getGoLayout()
	if !ok {
		return Registers{}, fmt.Errorf("goroutine inspection requires Go runtime DWARF")
	}
	tid, err := e.activeTID()
	if err != nil {
		return Registers{}, err
	}
	tids := []int{tid}
	worldStopped := false
	if backend, ok := e.backend.(stoppedInspectionThreads); ok {
		tids, err = backend.inspectionThreads()
		if err != nil {
			if errors.Is(err, ErrSessionInvalidated) {
				e.inspectionFailure = err
				var ended *inspectionEndedError
				if errors.As(err, &ended) {
					e.inspectionOutcome = &stopResult{evt: ended.stop}
				} else {
					e.inspectionOutcome = &stopResult{err: err}
				}
			}
			return Registers{}, err
		}
		worldStopped = true
	}
	gptr, err := e.findInspectionGoroutine(l, goid)
	if err != nil {
		return Registers{}, err
	}
	header, ok := e.readGoroutineHeader(l, gptr)
	lo, hi, boundsOK := e.readGoroutineStackBounds(l, gptr)
	if !ok || !boundsOK || header.goid != int64(goid) || !header.included() || lo >= hi {
		return Registers{}, fmt.Errorf("goroutine %d has inaccessible or inconsistent runtime context", goid)
	}
	for _, thread := range tids {
		regs, err := e.backend.GetRegisters(thread)
		if err != nil {
			return Registers{}, fmt.Errorf("goroutine inspection registers: %w", err)
		}
		if stackContainsSP(lo, hi, regs.SP) {
			if stopped, ok := e.backend.(interface {
				inspectionStop(int) (StopEvent, bool)
			}); ok && thread != tid {
				if stop, known := stopped.inspectionStop(thread); known && stop.Reason == StopBreakpoint {
					pc := archRewindPC(regs.PC)
					owned, err := e.attachedBreakpointOwned(pc)
					if err != nil {
						return Registers{}, fmt.Errorf("goroutine %d pending breakpoint: %w", goid, err)
					}
					if owned {
						regs.PC = pc
					}
				}
			}
			return e.checkedInspectionStack(goid, regs, lo, hi)
		}
	}
	if !worldStopped {
		return Registers{}, fmt.Errorf("goroutine %d cannot be inspected safely: this backend stops only the reporting thread; parked and sibling stacks may still change", goid)
	}
	var regs Registers
	switch header.status {
	case 1, 4, 9:
		pc, pcOK := e.dw.structMemberOffset("runtime.gobuf", "pc")
		sp, spOK := e.dw.structMemberOffset("runtime.gobuf", "sp")
		bp, bpOK := e.dw.structMemberOffset("runtime.gobuf", "bp")
		if !pcOK || !spOK || !bpOK {
			return Registers{}, fmt.Errorf("goroutine %d saved gobuf layout unavailable", goid)
		}
		regs, err = e.readInspectionRegisters(gptr+uint64(l.gSched), pc, sp, bp)
	case 3:
		pc, pcOK := e.dw.structMemberOffset("runtime.g", "syscallpc")
		sp, spOK := e.dw.structMemberOffset("runtime.g", "syscallsp")
		bp, bpOK := e.dw.structMemberOffset("runtime.g", "syscallbp")
		if !pcOK || !spOK || !bpOK {
			return Registers{}, fmt.Errorf("goroutine %d syscall context layout unavailable", goid)
		}
		regs, err = e.readInspectionRegisters(gptr, pc, sp, bp)
	default:
		return Registers{}, fmt.Errorf("goroutine %d is %s without a stopped user-stack context (scheduler or signal stack)", goid, goStatusString(header.status))
	}
	if err != nil {
		return Registers{}, fmt.Errorf("goroutine %d saved context: %w", goid, err)
	}
	return e.checkedInspectionStack(goid, regs, lo, hi)
}

func (e *engine) findInspectionGoroutine(l *goLayout, goid int) (uint64, error) {
	ptr, length, ok := e.allgsMetadata()
	if !ok || ptr == 0 {
		return 0, fmt.Errorf("goroutine table unavailable")
	}
	end := min(length, uint64(2*maxGoroutineScan))
	for i := uint64(0); i < end; i++ {
		addr, ok := allgsEntryAddress(ptr, i)
		if !ok {
			return 0, fmt.Errorf("goroutine table address overflow")
		}
		gptr, ok := e.readU64(addr)
		if !ok {
			return 0, fmt.Errorf("goroutine table slot %d unreadable", i)
		}
		if gptr == 0 {
			continue
		}
		header, ok := e.readGoroutineHeader(l, gptr)
		if !ok {
			return 0, fmt.Errorf("goroutine table entry %d unreadable", i)
		}
		if header.goid == int64(goid) && header.included() {
			return gptr, nil
		}
	}
	if length > end {
		return 0, fmt.Errorf("goroutine %d not found within the %d-slot inspection bound", goid, end)
	}
	return 0, fmt.Errorf("goroutine %d not found or exited", goid)
}

func (e *engine) readInspectionRegisters(base uint64, pc, sp, bp int64) (Registers, error) {
	var regs Registers
	for _, field := range []struct {
		offset int64
		value  *uint64
	}{{pc, &regs.PC}, {sp, &regs.SP}, {bp, &regs.BP}} {
		var buf [8]byte
		if err := e.backend.ReadMemory(base+uint64(field.offset), buf[:]); err != nil {
			return Registers{}, err
		}
		*field.value = binary.LittleEndian.Uint64(buf[:])
	}
	return regs, nil
}

func (e *engine) checkedInspectionStack(goid int, regs Registers, lo, hi uint64) (Registers, error) {
	if regs.PC == 0 || !stackContainsSP(lo, hi, regs.SP) {
		return Registers{}, fmt.Errorf("goroutine %d has invalid saved PC/SP", goid)
	}
	bp := regs.BP
	minBP := regs.SP
	// Go's arm64 frame pointer names the saved FP slot just below SP.
	if runtime.GOARCH == "arm64" && minBP >= 8 {
		minBP -= 8
	}
	for depth := 0; bp != 0; depth++ {
		if depth >= maxStackDepth || bp < minBP || bp < lo || bp > hi || hi-bp < 16 {
			return Registers{}, fmt.Errorf("goroutine %d frame chain exceeds its stack bounds or depth limit: depth=%d bp=%#x sp=%#x stack=[%#x,%#x)", goid, depth, bp, regs.SP, lo, hi)
		}
		var frame [16]byte
		if err := e.backend.ReadMemory(bp, frame[:]); err != nil {
			return Registers{}, fmt.Errorf("goroutine %d frame chain: %w", goid, err)
		}
		next := binary.LittleEndian.Uint64(frame[:8])
		if next == 0 || binary.LittleEndian.Uint64(frame[8:]) == 0 {
			break
		}
		if next <= bp {
			return Registers{}, fmt.Errorf("goroutine %d has a cyclic or reversed frame chain", goid)
		}
		bp = next
	}
	return regs, nil
}
