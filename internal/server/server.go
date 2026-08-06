// Package server provides the HTTP/WebSocket entry point for bingo. See
// AGENTS.md for the endpoints and session lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bingosuite/bingo/internal/dap"
	"github.com/google/uuid"
)

const idleShutdownGrace = 10 * time.Second

var (
	ErrServerClosed      = errors.New("server is shutting down")
	ErrServerStarted     = errors.New("server already started")
	ErrDAPAlreadyStarted = errors.New("DAP server already started")
)

// Server owns the HTTP listener, the session store, and the lifecycle of all
// debug sessions.
type Server struct {
	httpServer *http.Server
	sessions   *sessionStore
	log        *slog.Logger
	ctx        context.Context
	cancel     context.CancelFunc

	instanceID  string
	idleTimeout time.Duration

	lifecycleMu sync.RWMutex
	started     bool
	shutting    bool
	admissions  uint64
	httpAddress string
	dapStarted  bool
	dapServer   *dap.Server
	dapAddress  string

	monitorOnce  sync.Once
	sessionOps   sync.WaitGroup
	shutdownOnce sync.Once
	shutdownDone chan struct{}

	// The hook lets tests hold the narrow timer-fire-to-shutdown-commit window
	// open without making production timing assumptions.
	idleCommitHook func()
}

// New creates a Server that will listen on addr (e.g. ":6060").
func New(addr string, log *slog.Logger) *Server {
	return newServer(addr, 0, log)
}

// NewWithIdleTimeout creates a Server that exits after its managed-session
// count remains zero for idleTimeout. A zero timeout preserves persistent
// server behavior.
func NewWithIdleTimeout(addr string, idleTimeout time.Duration, log *slog.Logger) (*Server, error) {
	if err := ValidateIdleTimeout(idleTimeout); err != nil {
		return nil, err
	}
	return newServer(addr, idleTimeout, log), nil
}

// ValidateIdleTimeout ensures the timer and timeoutMs health field can represent
// the configured duration identically.
func ValidateIdleTimeout(idleTimeout time.Duration) error {
	switch {
	case idleTimeout < 0:
		return fmt.Errorf("idle timeout must not be negative: %s", idleTimeout)
	case idleTimeout > 0 && idleTimeout < time.Millisecond:
		return fmt.Errorf("idle timeout must be zero or at least 1ms: %s", idleTimeout)
	case idleTimeout%time.Millisecond != 0:
		return fmt.Errorf("idle timeout must use whole milliseconds: %s", idleTimeout)
	default:
		return nil
	}
}

func newServer(addr string, idleTimeout time.Duration, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())

	s := &Server{
		sessions:     newSessionStore(log.With("component", "sessions")),
		log:          log,
		ctx:          ctx,
		cancel:       cancel,
		instanceID:   uuid.NewString(),
		idleTimeout:  idleTimeout,
		shutdownDone: make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
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
	s.lifecycleMu.Lock()
	if s.shutting {
		s.lifecycleMu.Unlock()
		return nil
	}
	if s.started {
		s.lifecycleMu.Unlock()
		return ErrServerStarted
	}
	s.started = true
	s.lifecycleMu.Unlock()

	var listenConfig net.ListenConfig
	ln, err := listenConfig.Listen(s.ctx, "tcp4", s.httpServer.Addr)
	if err != nil {
		s.Shutdown(idleShutdownGrace)
		return err
	}

	s.lifecycleMu.Lock()
	if s.shutting {
		s.lifecycleMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.httpAddress = ln.Addr().String()
	s.lifecycleMu.Unlock()

	s.log.Info("bingo server listening", "addr", ln.Addr().String())
	s.startIdleMonitor()

	err = s.httpServer.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	s.Shutdown(idleShutdownGrace)
	return err
}

// Shutdown closes the HTTP listener, drains in-flight requests, and cancels
// all session contexts.
func (s *Server) Shutdown(timeout time.Duration) {
	s.shutdownOnce.Do(func() {
		s.shutdown(timeout)
		close(s.shutdownDone)
	})
	<-s.shutdownDone
}

// Done closes after the server has finished closing listeners and draining all
// managed sessions.
func (s *Server) Done() <-chan struct{} {
	return s.shutdownDone
}

func (s *Server) shutdown(timeout time.Duration) {
	s.lifecycleMu.Lock()
	s.shutting = true
	ds := s.dapServer
	s.dapServer = nil
	s.dapAddress = ""
	s.lifecycleMu.Unlock()

	s.log.Info("shutting down server")

	if ds != nil {
		if err := ds.Close(); err != nil {
			s.log.Error("dap shutdown error", "err", err)
		}
	}

	ctx, done := context.WithTimeout(context.Background(), timeout)
	defer done()
	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.log.Error("http shutdown error", "err", err)
	}
	s.sessionOps.Wait()
	s.cancel()
	if !s.sessions.waitEmpty(ctx) {
		s.log.Error("session shutdown timed out", "remaining", s.sessions.count())
	}
}

func (s *Server) acceptingSessions() bool {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	return !s.shutting
}

func (s *Server) beginSessionOperation() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.shutting {
		return false
	}
	s.admissions++
	s.sessionOps.Add(1)
	return true
}

func (s *Server) idleSnapshot() (int, uint64, uint64, <-chan struct{}) {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	count, generation, changed := s.sessions.snapshot()
	return count, generation, s.admissions, changed
}

func (s *Server) commitIdleShutdown(generation, admissions uint64) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.shutting || s.admissions != admissions {
		return false
	}
	count, currentGeneration, _ := s.sessions.snapshot()
	if count != 0 || currentGeneration != generation {
		return false
	}
	s.shutting = true
	return true
}

func (s *Server) startIdleMonitor() {
	if s.idleTimeout <= 0 {
		return
	}
	s.monitorOnce.Do(func() {
		go s.monitorIdle()
	})
}

func (s *Server) monitorIdle() {
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	var timerC <-chan time.Time
	count, armedGeneration, armedAdmissions, changed := s.idleSnapshot()
	if count == 0 {
		resetTimer(timer, s.idleTimeout)
		timerC = timer.C
	}
	defer stopTimer(timer)

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-changed:
			count, generation, admissions, nextChanged := s.idleSnapshot()
			changed = nextChanged
			if count > 0 {
				stopTimer(timer)
				timerC = nil
				continue
			}
			resetTimer(timer, s.idleTimeout)
			timerC = timer.C
			armedGeneration = generation
			armedAdmissions = admissions
		case <-timerC:
			timerC = nil
			if s.idleCommitHook != nil {
				s.idleCommitHook()
			}
			if s.commitIdleShutdown(armedGeneration, armedAdmissions) {
				s.log.Info("managed server idle timeout elapsed", "timeout", s.idleTimeout)
				s.Shutdown(idleShutdownGrace)
				return
			}
			count, generation, admissions, nextChanged := s.idleSnapshot()
			changed = nextChanged
			if count == 0 {
				resetTimer(timer, s.idleTimeout)
				timerC = timer.C
				armedGeneration = generation
				armedAdmissions = admissions
			}
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, timeout time.Duration) {
	stopTimer(timer)
	timer.Reset(timeout)
}
