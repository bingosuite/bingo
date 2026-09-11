package dap

import (
	godap "github.com/google/go-dap"

	"github.com/bingosuite/bingo/pkg/protocol"
)

// finishTermination claims one terminal lifecycle, regardless of whether the
// engine reported a real exit or the hub observed its event channel closing.
// Only a real ProcessExited supplies an exit code; detach is not process death.
func (h *Handler) finishTermination(exitCode *int) {
	h.mu.Lock()
	messages := h.terminationMessagesLocked(exitCode)
	h.mu.Unlock()
	h.send(messages...)
}

func (h *Handler) terminationMessagesLocked(exitCode *int) []godap.Message {
	if h.terminated {
		return nil
	}
	h.terminated = true
	h.terminating = false
	h.suspended = false
	h.stopThreadUnknown = false
	h.launching = false
	h.restarting = false
	h.awaitingWelcome = false
	h.sessionState = protocol.StateExited
	startSeq, startCmd := h.startReqSeq, h.startCmd
	restartSeq := h.restartReqSeq
	h.startReqSeq = 0
	h.restartReqSeq = 0
	var messages []godap.Message
	if startSeq != 0 {
		messages = append(messages, h.errorResponse(startSeq, startCmd, "debug session ended before configuration completed"))
	}
	if restartSeq != 0 {
		messages = append(messages, h.errorResponse(restartSeq, "restart", "debug session ended during restart"))
	}
	if exitCode != nil {
		messages = append(messages, &godap.ExitedEvent{
			Event: h.event("exited"), Body: godap.ExitedEventBody{ExitCode: *exitCode},
		})
	}
	messages = append(messages, &godap.TerminatedEvent{Event: h.event("terminated")})
	// Keep a real exited/terminated pair together even if the IDE concurrently
	// acknowledges termination by sending disconnect.
	return messages
}
