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
//
// Breakpoint commands are deliberately absent: their ids cross the hub's
// logical/physical translation boundary, so Hub.dispatchCommand handles them
// (see breakpoints.go). Reaching them here would mean that routing was
// bypassed, so the default case reports an error rather than handing a
// client-supplied id straight to the engine.
//
//nolint:gocognit,gocyclo // Protocol command routing is intentionally centralized here.
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

	// Pause is fire-and-forget: it arms an async interrupt and returns. The
	// debugger emits EventPaused once the SIGSTOP lands (no immediate event).
	case protocol.CmdPause:
		return dispatchResult{}, dbg.Pause()

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

	case protocol.CmdEvaluate:
		var p protocol.EvaluatePayloadCmd
		if err := protocol.DecodeCommandPayload(cmd, &p); err != nil {
			return dispatchResult{}, err
		}
		result, err := dbg.Evaluate(p.FrameIndex, p.Name)
		if err != nil {
			return dispatchResult{}, err
		}
		evt, err := protocol.NewEvent(protocol.EventEvaluate, 0, protocol.EvaluatePayload{
			Result: result,
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

	case protocol.CmdGoroutineSnapshot:
		snap, err := dbg.GoroutineSnapshot()
		if err != nil {
			return dispatchResult{}, err
		}
		evt, err := protocol.NewEvent(protocol.EventGoroutineSnapshot, 0, snap)
		if err != nil {
			return dispatchResult{}, err
		}
		return dispatchResult{event: &evt}, nil

	default:
		return dispatchResult{}, fmt.Errorf("unknown command kind: %q", cmd.Kind)
	}
}
