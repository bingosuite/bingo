package dap

import (
	"log/slog"
	"net"
	"sync"
)

// Server accepts DAP connections on a TCP listener and hands each one to a
// Handler bound to the given Provider. One goroutine per connection.
type Server struct {
	provider Provider
	log      *slog.Logger

	mu       sync.Mutex
	listener net.Listener
	closed   bool
	// handlers tracks live connection handlers so Close can force them shut.
	// A handler that connected but never started a session has no hub to tear
	// it down, so its Serve goroutine would otherwise block forever in
	// ReadProtocolMessage and wedge wg.Wait().
	handlers map[*Handler]struct{}
	wg       sync.WaitGroup
}

// NewServer creates a DAP server over provider. It does not listen until Serve.
func NewServer(provider Provider, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{provider: provider, log: log, handlers: make(map[*Handler]struct{})}
}

// Serve binds addr (host:port) and accepts connections until Close. It returns
// the bound address so callers can log it or, in tests, dial an ephemeral port.
// The accept loop runs in the background; Serve returns once listening.
func (s *Server) Serve(addr string) (net.Addr, error) {
	// tcp4 to match the rest of bingo's local-only listeners and keep the
	// address predictable for IDE launch configs.
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	s.wg.Add(1)
	go s.acceptLoop(ln)

	s.log.Info("dap server listening", "addr", ln.Addr().String())
	return ln.Addr(), nil
}

func (s *Server) acceptLoop(ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			s.log.Warn("dap accept error", "err", err)
			return
		}
		h := NewHandler(conn, s.provider, s.log)
		if !s.register(h) {
			// Server closed between Accept and registration; drop the conn so
			// its Serve goroutine never starts (and never joins wg).
			_ = h.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.unregister(h)
			h.Serve()
		}()
	}
}

// register adds h to the live set unless the server is already closing, in
// which case the caller must drop the connection. Registration and the closed
// check share s.mu with Close so no handler can slip in after Close snapshots
// the set to force-close it.
func (s *Server) register(h *Handler) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.handlers[h] = struct{}{}
	return true
}

func (s *Server) unregister(h *Handler) {
	s.mu.Lock()
	delete(s.handlers, h)
	s.mu.Unlock()
}

// Close stops accepting new connections, closes the listener, and force-closes
// every live handler so their Serve read loops unblock — including a client
// that connected but never started a session (which has no hub to tear it
// down). It then waits for the accept loop and all connection goroutines.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.listener
	handlers := make([]*Handler, 0, len(s.handlers))
	for h := range s.handlers {
		handlers = append(handlers, h)
	}
	s.mu.Unlock()

	var err error
	if ln != nil {
		err = ln.Close()
	}
	for _, h := range handlers {
		_ = h.Close()
	}
	s.wg.Wait()
	return err
}
