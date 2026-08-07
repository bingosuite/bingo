package server

import (
	"fmt"

	"github.com/bingosuite/bingo/internal/dap"
)

// dapProvider adapts the server's sessionStore to dap.Provider so the DAP
// handler can create fresh managed sessions (on launch/attach) or join an
// existing one by id. The sessions it hands out are ordinary managed hubs —
// identical to those created over /ws?create — so WebSocket observers can join
// the very same session a DAP client is driving.
type dapProvider struct {
	srv *Server
}

func (p dapProvider) CreateSession() (dap.Session, error) {
	if !p.srv.beginSessionOperation() {
		return nil, ErrServerClosed
	}
	defer p.srv.endSessionOperation()
	sess := p.srv.sessions.create(p.srv.ctx)
	return sess.hub, nil
}

func (p dapProvider) GetSession(id string) (dap.Session, bool) {
	if !p.srv.beginSessionOperation() {
		return nil, false
	}
	defer p.srv.endSessionOperation()
	sess := p.srv.sessions.get(id)
	if sess == nil {
		return nil, false
	}
	return sess.hub, true
}

// StartDAP opens a DAP TCP listener on addr and serves it until Shutdown. It
// returns immediately once listening; connections are handled in the
// background. Safe to call at most once.
func (s *Server) StartDAP(addr string) error {
	s.lifecycleMu.Lock()
	if s.shutting {
		s.lifecycleMu.Unlock()
		return ErrServerClosed
	}
	if s.dapStarted || s.dapStarting {
		s.lifecycleMu.Unlock()
		return ErrDAPAlreadyStarted
	}
	s.dapStarting = true
	startDone := make(chan struct{})
	s.dapStartDone = startDone
	serve := s.dapServe
	s.lifecycleMu.Unlock()

	ds := dap.NewServer(dapProvider{srv: s}, s.log.With("component", "dap"))
	bound, err := serve(s.ctx, ds, addr)

	s.lifecycleMu.Lock()
	s.dapStarting = false
	if err != nil {
		stopping := s.shutting || s.ctx.Err() != nil
		if stopping {
			s.lifecycleMu.Unlock()
			_ = ds.Close()
			close(startDone)
			return ErrServerClosed
		}
		close(startDone)
		s.lifecycleMu.Unlock()
		return fmt.Errorf("listen for DAP: %w", err)
	}
	if s.shutting {
		s.lifecycleMu.Unlock()
		_ = ds.Close()
		close(startDone)
		return ErrServerClosed
	}
	s.dapStarted = true
	s.dapServer = ds
	s.dapAddress = bound.String()
	close(startDone)
	s.lifecycleMu.Unlock()
	return nil
}
