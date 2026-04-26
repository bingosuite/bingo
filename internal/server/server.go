// Package server provides the HTTP/WebSocket entry point for bingo. See
// AGENTS.md for the endpoints and session lifecycle.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server owns the HTTP listener, the session store, and the lifecycle of all
// debug sessions.
type Server struct {
	httpServer *http.Server
	sessions   *sessionStore
	log        *slog.Logger
	ctx        context.Context
	cancel     context.CancelFunc
}

// New creates a Server that will listen on addr (e.g. ":6060").
func New(addr string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		sessions: newSessionStore(log.With("component", "sessions")),
		log:      log,
		ctx:      ctx,
		cancel:   cancel,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", s.handleListSessions)
	mux.HandleFunc("/ws", s.handleWS)

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return s
}

// Start blocks until shutdown or a fatal listener error.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp4", s.httpServer.Addr)
	if err != nil {
		return err
	}
	s.log.Info("bingo server listening", "addr", ln.Addr().String())

	err = s.httpServer.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown closes the HTTP listener, drains in-flight requests, and cancels
// all session contexts.
func (s *Server) Shutdown(timeout time.Duration) {
	s.log.Info("shutting down server")
	s.cancel()

	ctx, done := context.WithTimeout(context.Background(), timeout)
	defer done()
	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.log.Error("http shutdown error", "err", err)
	}
}
