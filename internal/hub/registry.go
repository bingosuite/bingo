package hub

import "sync"

// registry is a thread-safe set of connected clients.
// broadcast is called from the hub's single event-loop goroutine; add and
// remove may be called from any goroutine, so we guard with a mutex.
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

// broadcast delivers msg to every registered client.
// The hub calls this from its single event-loop goroutine, but we hold an
// RLock so concurrent add/remove calls are safe.
func (r *registry) broadcast(msg []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for c := range r.clients {
		c.deliver(msg)
	}
}

// closeAll signals every registered client to disconnect by closing its send
// channel, then empties the registry. Uses c.closeSend() to prevent panics
// if deliver() already closed the channel due to buffer overflow.
func (r *registry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for c := range r.clients {
		c.closeSend()
		delete(r.clients, c)
	}
}

// snapshot returns a point-in-time copy of all registered clients.
func (r *registry) snapshot() []*Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Client, 0, len(r.clients))
	for c := range r.clients {
		out = append(out, c)
	}
	return out
}
