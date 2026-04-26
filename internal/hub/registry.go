package hub

import "sync"

// registry is a thread-safe set of clients. broadcast is called from the
// hub's event-loop goroutine; add/remove may be called from any goroutine.
type registry struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
}

func newRegistry() *registry {
	return &registry{clients: make(map[*Client]struct{})}
}

func (r *registry) add(c *Client) {
	r.mu.Lock()
	r.clients[c] = struct{}{}
	r.mu.Unlock()
}

func (r *registry) remove(c *Client) {
	r.mu.Lock()
	delete(r.clients, c)
	r.mu.Unlock()
}

func (r *registry) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

func (r *registry) broadcast(msg []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for c := range r.clients {
		c.deliver(msg)
	}
}

// closeAll closes every client's send channel and empties the registry.
// Uses closeSend() (idempotent) to avoid panics when deliver() already
// closed the channel due to buffer overflow.
func (r *registry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for c := range r.clients {
		c.closeSend()
		delete(r.clients, c)
	}
}

func (r *registry) snapshot() []*Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Client, 0, len(r.clients))
	for c := range r.clients {
		out = append(out, c)
	}
	return out
}
