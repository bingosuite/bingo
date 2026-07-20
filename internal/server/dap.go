package server

import (
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
	sess := p.srv.sessions.create(p.srv.ctx)
	return sess.hub, nil
}

func (p dapProvider) GetSession(id string) (dap.Session, bool) {
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
	ds := dap.NewServer(dapProvider{srv: s}, s.log.With("component", "dap"))
	if _, err := ds.Serve(addr); err != nil {
		return err
	}
	s.dapServer = ds
	return nil
}
