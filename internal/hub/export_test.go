// Exposes internal symbols to hub_test. Compiled only during `go test`.
package hub

import "time"

// ExportedSetSuspendTimeout must be called before Run starts.
func ExportedSetSuspendTimeout(h *Hub, timeout time.Duration) {
	h.suspendTimeout = timeout
}
