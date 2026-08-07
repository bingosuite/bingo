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

	lifecycleMu      sync.RWMutex
	started          bool
	shutting         bool
	httpAddress      string
	dapStarted       bool
	dapStarting      bool
	dapStartDone     chan struct{}
	dapServer        *dap.Server
	dapAddress       string
	activeAdmissions int
	admissionChanged chan struct{}
	idleExpired      bool

	monitorOnce  sync.Once
	sessionOps   sync.WaitGroup
	shutdownOnce sync.Once
	shutdownDone chan struct{}

	listen   func(context.Context, string, string) (net.Listener, error)
	dapServe func(context.Context, *dap.Server, string) (net.Addr, error)

	// These hooks expose admission and timer ordering without making tests rely
	// on scheduler timing. Production leaves both nil.
	admissionHook func()
	idleTimerHook func()
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
		sessions:         newSessionStore(log.With("component", "sessions")),
		log:              log,
		ctx:              ctx,
		cancel:           cancel,
		instanceID:       uuid.NewString(),
		idleTimeout:      idleTimeout,
		shutdownDone:     make(chan struct{}),
		admissionChanged: make(chan struct{}),
	}
	var listenConfig net.ListenConfig
	s.listen = listenConfig.Listen
	s.dapServe = func(ctx context.Context, ds *dap.Server, addr string) (net.Addr, error) {
		return ds.ServeContext(ctx, addr)
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

	ln, err := s.listen(s.ctx, "tcp4", s.httpServer.Addr)
	if err != nil {
		if s.lifecycleStopping() && errors.Is(err, context.Canceled) {
			return nil
		}
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
	dapStartDone := s.dapStartDone
	s.dapServer = nil
	s.dapAddress = ""
	s.lifecycleMu.Unlock()

	s.log.Info("shutting down server")
	s.cancel()

	if dapStartDone != nil {
		<-dapStartDone
	}

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
	if !s.sessions.waitEmpty(ctx) {
		s.log.Error("session shutdown timed out", "remaining", s.sessions.count())
	}
}

func (s *Server) beginSessionOperation() bool {
	s.lifecycleMu.Lock()
	if s.shutting || s.idleExpired {
		s.lifecycleMu.Unlock()
		return false
	}
	s.activeAdmissions++
	s.sessionOps.Add(1)
	hook := s.admissionHook
	s.lifecycleMu.Unlock()
	if hook != nil {
		hook()
	}
	return true
}

func (s *Server) endSessionOperation() {
	s.lifecycleMu.Lock()
	s.activeAdmissions--
	close(s.admissionChanged)
	s.admissionChanged = make(chan struct{})
	s.lifecycleMu.Unlock()
	s.sessionOps.Done()
}

func (s *Server) admissionSnapshot() (int, <-chan struct{}) {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	return s.activeAdmissions, s.admissionChanged
}

type idleDecision struct {
	committed        bool
	expired          bool
	count            int
	generation       uint64
	sessionChanged   <-chan struct{}
	admissionChanged <-chan struct{}
}

func (s *Server) commitIdleShutdown(generation uint64) idleDecision {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	count, currentGeneration, sessionChanged := s.sessions.snapshot()
	decision := idleDecision{
		count:            count,
		generation:       currentGeneration,
		sessionChanged:   sessionChanged,
		admissionChanged: s.admissionChanged,
	}
	if s.shutting {
		return decision
	}
	if count != 0 {
		s.idleExpired = false
		return decision
	}
	if currentGeneration != generation {
		s.idleExpired = false
		return decision
	}
	s.idleExpired = true
	decision.expired = true
	if s.activeAdmissions != 0 {
		return decision
	}
	s.shutting = true
	decision.committed = true
	return decision
}

func (s *Server) clearIdleExpiry() {
	s.lifecycleMu.Lock()
	s.idleExpired = false
	s.lifecycleMu.Unlock()
}

func (s *Server) lifecycleStopping() bool {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	return s.shutting
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
	sessionCount, armedGeneration, sessionChanged := s.sessions.snapshot()
	_, admissionChanged := s.admissionSnapshot()
	deadlineExpired := false
	if sessionCount == 0 {
		resetTimer(timer, s.idleTimeout)
		timerC = timer.C
	}
	defer stopTimer(timer)

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-sessionChanged:
			count, generation, nextChanged := s.sessions.snapshot()
			sessionChanged = nextChanged
			sessionCount = count
			if sessionCount > 0 {
				stopTimer(timer)
				timerC = nil
				deadlineExpired = false
				s.clearIdleExpiry()
				continue
			}
			if deadlineExpired && generation == armedGeneration {
				continue
			}
			resetTimer(timer, s.idleTimeout)
			timerC = timer.C
			deadlineExpired = false
			armedGeneration = generation
			s.clearIdleExpiry()
		case <-admissionChanged:
			if !deadlineExpired {
				_, admissionChanged = s.admissionSnapshot()
				continue
			}
			decision := s.commitIdleShutdown(armedGeneration)
			sessionChanged = decision.sessionChanged
			admissionChanged = decision.admissionChanged
			if decision.committed {
				s.log.Info("managed server idle timeout elapsed", "timeout", s.idleTimeout)
				s.Shutdown(idleShutdownGrace)
				return
			}
			sessionCount = decision.count
			if sessionCount > 0 {
				stopTimer(timer)
				timerC = nil
				deadlineExpired = false
				continue
			}
			if !decision.expired {
				resetTimer(timer, s.idleTimeout)
				timerC = timer.C
				deadlineExpired = false
				armedGeneration = decision.generation
			}
		case <-timerC:
			timerC = nil
			decision := s.commitIdleShutdown(armedGeneration)
			sessionChanged = decision.sessionChanged
			admissionChanged = decision.admissionChanged
			deadlineExpired = decision.expired
			if s.idleTimerHook != nil {
				s.idleTimerHook()
			}
			if decision.committed {
				s.log.Info("managed server idle timeout elapsed", "timeout", s.idleTimeout)
				s.Shutdown(idleShutdownGrace)
				return
			}
			sessionCount = decision.count
			if sessionCount > 0 {
				deadlineExpired = false
				continue
			}
			if !decision.expired {
				resetTimer(timer, s.idleTimeout)
				timerC = timer.C
				armedGeneration = decision.generation
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
