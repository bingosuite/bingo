// Package server provides the HTTP/WebSocket entry point for bingo. It manages
// debug sessions and routes WebSocket connections to the appropriate hub.
//
// Endpoints:
//
//	GET  /api/sessions        — list active sessions
//	GET  /ws?create           — create a new session and connect
//	GET  /ws?session={id}     — join an existing session
//
// Each session is backed by one Hub and one Debugger (created lazily on
// Launch/Attach). Sessions are automatically cleaned up when the last client
// disconnects.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

)

// Server is the single global bingo server. It owns the HTTP listener, the
// session store, and the lifecycle of all debug sessions.
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

// Start begins listening for connections. It blocks until the server is shut
// down or a fatal listener error occurs.
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

// Shutdown gracefully stops the server: closes the HTTP listener, waits for
// in-flight requests to drain, and cancels all session contexts.
func (s *Server) Shutdown(timeout time.Duration) {
	s.log.Info("shutting down server")
	s.cancel() // signal all sessions to stop

	ctx, done := context.WithTimeout(context.Background(), timeout)
	defer done()
	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.log.Error("http shutdown error", "err", err)
	}
}
