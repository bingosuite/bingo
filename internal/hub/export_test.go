// Exposes internal synchronization points to hub_test. Compiled only during tests.
package hub

import "github.com/bingosuite/bingo/pkg/protocol"

func (h *Hub) ExportedSetEventTransitionHook(hook func(protocol.EventKind, protocol.SessionState)) {
	h.eventTransitionHook = hook
}
