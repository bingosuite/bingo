package hub

import (
	"fmt"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/bingosuite/bingo/pkg/protocol"
)

// dispatchResult carries an optional confirmation event the hub should
// broadcast immediately. Most commands produce no event — the debugger emits
// one asynchronously on its Events channel.
type dispatchResult struct {
	event *protocol.Event
}

// dispatch translates cmd into a debugger call and returns the immediate
// confirmation event (if any) plus any error.
func dispatch(dbg debugger.Debugger, cmd protocol.Command) (dispatchResult, error) {
	switch cmd.Kind {

	case protocol.CmdLaunch:
		var p protocol.LaunchPayload
		if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{}, dbg.Launch(p.Program, p.Args, p.Env)

	case protocol.CmdAttach:
		var p protocol.AttachPayload
		if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{}, dbg.Attach(p.PID, p.BinaryPath)

	case protocol.CmdKill:
		return dispatchResult{}, dbg.Kill()

	case protocol.CmdSetBreakpoint:
		var p protocol.SetBreakpointPayload
		if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
			return dispatchResult{}, err
		}
		bp, err := dbg.SetBreakpoint(p.File, p.Line)
		if err != nil {
			return dispatchResult{}, err
		}
		evt, err := protocol.NewEvent(protocol.EventBreakpointSet, 0, protocol.BreakpointSetPayload{
			Breakpoint: bp,
		})
		if err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{event: &evt}, nil

	case protocol.CmdClearBreakpoint:
		var p protocol.ClearBreakpointPayload
		if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
			return dispatchResult{}, err
		}
		if err := dbg.ClearBreakpoint(p.ID); err != nil {
			return dispatchResult{}, err
		}
		evt, err := protocol.NewEvent(protocol.EventBreakpointCleared, 0, protocol.BreakpointClearedPayload{
			ID: p.ID,
		})
		if err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{event: &evt}, nil

	// Execution control: no immediate event. The debugger emits Stepped /
	// Continued asynchronously.
	case protocol.CmdContinue:
		return dispatchResult{}, dbg.Continue()
	case protocol.CmdStepOver:
		return dispatchResult{}, dbg.StepOver()
	case protocol.CmdStepInto:
		return dispatchResult{}, dbg.StepInto()
	case protocol.CmdStepOut:
		return dispatchResult{}, dbg.StepOut()

	case protocol.CmdLocals:
		var p protocol.LocalsPayloadCmd
		if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
			return dispatchResult{}, err
		}
		vars, err := dbg.Locals(p.FrameIndex)
		if err != nil {
			return dispatchResult{}, err
		}
		evt, err := protocol.NewEvent(protocol.EventLocals, 0, protocol.LocalsPayload{
			FrameIndex: p.FrameIndex,
			Variables:  vars,
		})
		if err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{event: &evt}, nil

	case protocol.CmdFrames:
		frames, err := dbg.StackFrames()
		if err != nil {
			return dispatchResult{}, err
		}
		evt, err := protocol.NewEvent(protocol.EventFrames, 0, protocol.FramesPayload{
			Frames: frames,
		})
		if err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{event: &evt}, nil

	case protocol.CmdGoroutines:
		goroutines, err := dbg.Goroutines()
		if err != nil {
			return dispatchResult{}, err
		}
		evt, err := protocol.NewEvent(protocol.EventGoroutines, 0, protocol.GoroutinesPayload{
			Goroutines: goroutines,
		})
		if err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{event: &evt}, nil

	default:
		return dispatchResult{}, fmt.Errorf("unknown command kind: %q", cmd.Kind)
	}
}
