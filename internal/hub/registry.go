package hub

import "sync"

// registry is a thread-safe set of clients. broadcast is called from the
// hub's event-loop goroutine; add/remove may be called from any goroutine.
type registry struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
	closed  bool
}

func newRegistry() *registry {
	return &registry{clients: make(map[*Client]struct{})}
}

func (r *registry) add(c *Client) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	r.clients[c] = struct{}{}
	return true
}

func (r *registry) remove(c *Client) (bool, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[c]; !ok {
		return false, len(r.clients)
	}
	delete(r.clients, c)
	remaining := len(r.clients)
	if remaining == 0 {
		r.closed = true
	}
	return true, remaining
}

func (r *registry) count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

func (r *registry) snapshot() []*Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	clients := make([]*Client, 0, len(r.clients))
	for c := range r.clients {
		clients = append(clients, c)
	}
	return clients
}

// closeAll closes every client's send channel and empties the registry.
// Uses closeSend() (idempotent) to avoid panics when deliver() already
// closed the channel due to buffer overflow.
func (r *registry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for c := range r.clients {
		c.closeSend()
		delete(r.clients, c)
	}
}
