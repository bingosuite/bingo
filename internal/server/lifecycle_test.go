package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/pkg/protocol"
)

const testIdleTimeout = 150 * time.Millisecond

type runningServer struct {
	server *Server
	url    string
	errCh  chan error
}

func startServer(t *testing.T, idleTimeout time.Duration) runningServer {
	t.Helper()
	g := NewWithT(t)
	srv, err := NewWithIdleTimeout("127.0.0.1:0", idleTimeout, nil)
	g.Expect(err).NotTo(HaveOccurred())
	return startConstructedServer(t, srv)
}

func startConstructedServer(t *testing.T, srv *Server) runningServer {
	t.Helper()
	g := NewWithT(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	var address string
	g.Eventually(func() string {
		srv.lifecycleMu.RLock()
		defer srv.lifecycleMu.RUnlock()
		address = srv.httpAddress
		return address
	}, time.Second).ShouldNot(BeEmpty())

	return runningServer{
		server: srv,
		url:    "http://" + address,
		errCh:  errCh,
	}
}

func (r runningServer) stop(t *testing.T) {
	t.Helper()
	r.server.Shutdown(time.Second)
	g := NewWithT(t)
	g.Eventually(r.errCh, time.Second).Should(Receive(BeNil()))
}

func dialCreate(t *testing.T, baseURL string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/ws?create"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	return conn
}

func getHealth(t *testing.T, handler http.Handler) (*httptest.ResponseRecorder, HealthResponse) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	var health HealthResponse
	NewWithT(t).Expect(json.NewDecoder(response.Body).Decode(&health)).To(Succeed())
	return response, health
}

func TestHealthDiscovery(t *testing.T) {
	g := NewWithT(t)
	srv := New(":0", nil)

	response, health := getHealth(t, srv.httpServer.Handler)
	g.Expect(response.Code).To(Equal(http.StatusOK))
	g.Expect(response.Header().Get("Content-Type")).To(Equal("application/json"))
	g.Expect(response.Header().Get("Cache-Control")).To(Equal("no-store"))
	g.Expect(health.Service).To(Equal("bingo"))
	g.Expect(health.ManagementAPIVersion).To(Equal(ManagementAPIVersion))
	g.Expect(health.WireProtocolVersion).To(Equal(protocol.Version))
	g.Expect(uuid.Validate(health.InstanceID)).To(Succeed())
	g.Expect(health.DAP).To(Equal(DAPHealth{}))
	g.Expect(health.ManagedIdleShutdown).To(Equal(ManagedIdleShutdownHealth{}))
	g.Expect(health.SessionCount).To(Equal(0))

	rawResponse := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rawResponse, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var fields map[string]json.RawMessage
	g.Expect(json.NewDecoder(rawResponse.Body).Decode(&fields)).To(Succeed())
	g.Expect(fields).To(HaveLen(7))
	for _, field := range []string{
		"service",
		"managementApiVersion",
		"wireProtocolVersion",
		"instanceId",
		"dap",
		"managedIdleShutdown",
		"sessionCount",
	} {
		g.Expect(fields).To(HaveKey(field))
	}

	_, repeated := getHealth(t, srv.httpServer.Handler)
	g.Expect(repeated.InstanceID).To(Equal(health.InstanceID))
	g.Expect(New(":0", nil).instanceID).NotTo(Equal(health.InstanceID))
}

func TestHealthRejectsNonGET(t *testing.T) {
	g := NewWithT(t)
	srv := New(":0", nil)
	request := httptest.NewRequest(http.MethodPost, "/api/health", nil)
	response := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(response, request)

	g.Expect(response.Code).To(Equal(http.StatusMethodNotAllowed))
	g.Expect(response.Header().Get("Allow")).To(Equal(http.MethodGet))
	g.Expect(response.Header().Get("Cache-Control")).To(Equal("no-store"))
}

func TestHealthReportsResolvedDAPAddress(t *testing.T) {
	g := NewWithT(t)
	srv, err := NewWithIdleTimeout(":0", 30*time.Second, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(srv.StartDAP("127.0.0.1:0")).To(Succeed())
	t.Cleanup(func() {
		srv.Shutdown(time.Second)
	})

	_, health := getHealth(t, srv.httpServer.Handler)
	g.Expect(health.DAP.Enabled).To(BeTrue())
	g.Expect(health.DAP.Address).To(MatchRegexp(`^127\.0\.0\.1:\d+$`))
	g.Expect(health.DAP.Address).NotTo(HaveSuffix(":0"))
	g.Expect(health.ManagedIdleShutdown).To(Equal(ManagedIdleShutdownHealth{
		Enabled:   true,
		TimeoutMS: 30000,
	}))
	g.Expect(srv.StartDAP("127.0.0.1:0")).To(MatchError(ErrDAPAlreadyStarted))
}

func TestHealthReportsSessionCount(t *testing.T) {
	g := NewWithT(t)
	srv := New(":0", nil)
	srv.sessions.create(srv.ctx)

	_, health := getHealth(t, srv.httpServer.Handler)
	g.Expect(health.SessionCount).To(Equal(1))

	srv.Shutdown(time.Second)
	g.Expect(srv.sessions.count()).To(Equal(0))
}

func TestIdleTimeoutValidation(t *testing.T) {
	g := NewWithT(t)
	tests := []struct {
		timeout time.Duration
		message string
	}{
		{timeout: -time.Second, message: "idle timeout must not be negative: -1s"},
		{timeout: time.Nanosecond, message: "idle timeout must be zero or at least 1ms: 1ns"},
		{timeout: 1500 * time.Microsecond, message: "idle timeout must use whole milliseconds: 1.5ms"},
	}
	for _, test := range tests {
		srv, err := NewWithIdleTimeout(":0", test.timeout, nil)
		g.Expect(err).To(MatchError(test.message))
		g.Expect(srv).To(BeNil())
	}

	srv, err := NewWithIdleTimeout(":0", time.Millisecond, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(srv.idleTimeout).To(Equal(time.Millisecond))
}

func TestIdleShutdownDisabled(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, 0)

	g.Consistently(running.errCh, testIdleTimeout).ShouldNot(Receive())
	running.stop(t)
}

func TestIdleShutdownExpiresAtStartup(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, testIdleTimeout)

	g.Eventually(running.errCh, time.Second).Should(Receive(BeNil()))
	g.Expect(running.server.sessions.count()).To(Equal(0))
}

func TestHealthAndBareDAPConnectionDoNotSuppressIdleShutdown(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, testIdleTimeout)
	g.Expect(running.server.StartDAP("127.0.0.1:0")).To(Succeed())

	running.server.lifecycleMu.RLock()
	dapAddress := running.server.dapAddress
	running.server.lifecycleMu.RUnlock()
	conn, err := net.Dial("tcp4", dapAddress)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = conn.Close() }()

	response, err := http.Get(running.url + "/api/health")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(response.Body.Close()).To(Succeed())

	g.Eventually(running.errCh, time.Second).Should(Receive(BeNil()))
	g.Expect(conn.SetReadDeadline(time.Now().Add(time.Second))).To(Succeed())
	_, err = conn.Read(make([]byte, 1))
	g.Expect(err).To(HaveOccurred())
}

func TestDAPCreatedSessionSuppressesIdleShutdown(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, testIdleTimeout)
	_, err := (dapProvider{srv: running.server}).CreateSession()
	g.Expect(err).NotTo(HaveOccurred())

	g.Eventually(running.server.sessions.count, time.Second).Should(Equal(1))
	g.Consistently(running.errCh, 2*testIdleTimeout).ShouldNot(Receive())
	running.stop(t)
}

func TestIdleExpiryYieldsToWebSocketAdmission(t *testing.T) {
	g := NewWithT(t)
	srv, err := NewWithIdleTimeout("127.0.0.1:0", testIdleTimeout, nil)
	g.Expect(err).NotTo(HaveOccurred())

	ready := make(chan struct{})
	proceed := make(chan struct{})
	var hookOnce sync.Once
	srv.idleCommitHook = func() {
		hookOnce.Do(func() {
			close(ready)
			<-proceed
		})
	}
	running := startConstructedServer(t, srv)
	g.Eventually(ready, time.Second).Should(BeClosed())

	conn := dialCreate(t, running.url)
	defer func() { _ = conn.Close() }()
	g.Eventually(srv.sessions.count, time.Second).Should(Equal(1))
	close(proceed)

	g.Consistently(running.errCh, 2*testIdleTimeout).ShouldNot(Receive())
	g.Expect(conn.Close()).To(Succeed())
	g.Eventually(running.errCh, time.Second).Should(Receive(BeNil()))
}

func TestIdleExpiryYieldsToDAPAdmission(t *testing.T) {
	g := NewWithT(t)
	srv, err := NewWithIdleTimeout("127.0.0.1:0", testIdleTimeout, nil)
	g.Expect(err).NotTo(HaveOccurred())

	ready := make(chan struct{})
	proceed := make(chan struct{})
	var hookOnce sync.Once
	srv.idleCommitHook = func() {
		hookOnce.Do(func() {
			close(ready)
			<-proceed
		})
	}
	running := startConstructedServer(t, srv)
	g.Eventually(ready, time.Second).Should(BeClosed())

	_, err = (dapProvider{srv: srv}).CreateSession()
	g.Expect(err).NotTo(HaveOccurred())
	g.Eventually(srv.sessions.count, time.Second).Should(Equal(1))
	close(proceed)

	g.Consistently(running.errCh, 2*testIdleTimeout).ShouldNot(Receive())
	running.stop(t)
}

func TestActiveSessionSuppressesIdleShutdown(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, testIdleTimeout)
	conn := dialCreate(t, running.url)
	defer func() { _ = conn.Close() }()

	g.Eventually(running.server.sessions.count, time.Second).Should(Equal(1))
	g.Consistently(running.errCh, 2*testIdleTimeout).ShouldNot(Receive())

	g.Expect(conn.Close()).To(Succeed())
	g.Eventually(running.errCh, time.Second).Should(Receive(BeNil()))
}

func TestNewSessionRearmsIdleShutdown(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, testIdleTimeout)
	first := dialCreate(t, running.url)
	g.Eventually(running.server.sessions.count, time.Second).Should(Equal(1))

	g.Expect(first.Close()).To(Succeed())
	g.Eventually(running.server.sessions.count, time.Second).Should(Equal(0))
	g.Consistently(running.errCh, testIdleTimeout/3).ShouldNot(Receive())

	second := dialCreate(t, running.url)
	defer func() { _ = second.Close() }()
	g.Eventually(running.server.sessions.count, time.Second).Should(Equal(1))
	g.Consistently(running.errCh, 2*testIdleTimeout).ShouldNot(Receive())

	g.Expect(second.Close()).To(Succeed())
	g.Eventually(running.errCh, time.Second).Should(Receive(BeNil()))
}

func TestMultipleSessionsSuppressIdleShutdown(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, testIdleTimeout)
	first := dialCreate(t, running.url)
	second := dialCreate(t, running.url)
	defer func() { _ = first.Close() }()
	defer func() { _ = second.Close() }()
	g.Eventually(running.server.sessions.count, time.Second).Should(Equal(2))

	g.Expect(first.Close()).To(Succeed())
	g.Eventually(running.server.sessions.count, time.Second).Should(Equal(1))
	g.Consistently(running.errCh, 2*testIdleTimeout).ShouldNot(Receive())

	g.Expect(second.Close()).To(Succeed())
	g.Eventually(running.errCh, time.Second).Should(Receive(BeNil()))
}

func TestSessionStoreBroadcastSupportsMultipleWaiters(t *testing.T) {
	g := NewWithT(t)
	store := newSessionStore(slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	store.create(ctx)
	g.Expect(store.count()).To(Equal(1))

	waiters := make(chan bool, 2)
	for range 2 {
		go func() {
			waitCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			waiters <- store.waitEmpty(waitCtx)
		}()
	}

	cancel()
	g.Eventually(waiters, time.Second).Should(Receive(BeTrue()))
	g.Eventually(waiters, time.Second).Should(Receive(BeTrue()))
}

func TestShutdownIsConcurrentAndIdempotent(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, 0)
	g.Expect(running.server.StartDAP("127.0.0.1:0")).To(Succeed())
	running.server.lifecycleMu.RLock()
	dapAddress := running.server.dapAddress
	running.server.lifecycleMu.RUnlock()
	dapConn, err := net.Dial("tcp4", dapAddress)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = dapConn.Close() }()

	conn := dialCreate(t, running.url)
	defer func() { _ = conn.Close() }()
	g.Eventually(running.server.sessions.count, time.Second).Should(Equal(1))

	var callers sync.WaitGroup
	callers.Add(8)
	for range 8 {
		go func() {
			defer callers.Done()
			running.server.Shutdown(time.Second)
		}()
	}
	done := make(chan struct{})
	go func() {
		callers.Wait()
		close(done)
	}()

	g.Eventually(done, 2*time.Second).Should(BeClosed())
	g.Eventually(running.errCh, time.Second).Should(Receive(BeNil()))
	g.Expect(running.server.sessions.count()).To(Equal(0))
	running.server.Shutdown(time.Second)
	g.Expect(running.server.StartDAP("127.0.0.1:0")).To(MatchError(ErrServerClosed))
}

func TestStartIsAtMostOnce(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, 0)
	g.Expect(running.server.Start()).To(MatchError(ErrServerStarted))
	running.stop(t)

	g.Expect(running.server.Start()).To(Succeed())
}

func TestStartBindFailureFinalizesDAPAndDone(t *testing.T) {
	g := NewWithT(t)
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = occupied.Close() }()

	srv := New(occupied.Addr().String(), nil)
	g.Expect(srv.StartDAP("127.0.0.1:0")).To(Succeed())
	srv.lifecycleMu.RLock()
	dapAddress := srv.dapAddress
	srv.lifecycleMu.RUnlock()

	startResult := make(chan error, 1)
	go func() {
		startResult <- srv.Start()
	}()
	var startErr error
	g.Eventually(startResult, time.Second).Should(Receive(&startErr))
	g.Expect(startErr).To(HaveOccurred())
	g.Expect(errors.Is(startErr, ErrServerStarted)).To(BeFalse())
	var netErr *net.OpError
	g.Expect(errors.As(startErr, &netErr)).To(BeTrue())
	g.Expect(netErr.Op).To(Equal("listen"))
	g.Expect(srv.Done()).To(BeClosed())

	conn, err := net.DialTimeout("tcp4", dapAddress, 100*time.Millisecond)
	if conn != nil {
		_ = conn.Close()
	}
	g.Expect(err).To(HaveOccurred())
}

func TestShutdownBeforeStartDoesNotResurrectServer(t *testing.T) {
	g := NewWithT(t)
	srv := New("127.0.0.1:0", nil)
	srv.Shutdown(time.Second)

	g.Expect(srv.Start()).To(Succeed())
	g.Expect(srv.StartDAP("127.0.0.1:0")).To(MatchError(ErrServerClosed))

	response := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/ws?create", nil))
	g.Expect(response.Code).To(Equal(http.StatusServiceUnavailable))
}

func TestWebSocketJoinRejectsClosedHub(t *testing.T) {
	g := NewWithT(t)
	srv := New(":0", nil)
	sessionCtx, cancelSession := context.WithCancel(context.Background())
	sess := srv.sessions.create(sessionCtx)
	cancelSession()
	g.Eventually(sess.hub.Done(), time.Second).Should(BeClosed())
	g.Eventually(srv.sessions.count, time.Second).Should(Equal(0))

	srv.sessions.mu.Lock()
	srv.sessions.sessions[sess.id] = sess
	srv.sessions.notifyLocked()
	srv.sessions.mu.Unlock()

	httpServer := httptest.NewServer(srv.httpServer.Handler)
	defer httpServer.Close()
	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws?session="+sess.id,
		nil,
	)
	g.Expect(err).NotTo(HaveOccurred())
	defer func() { _ = conn.Close() }()

	g.Expect(conn.SetReadDeadline(time.Now().Add(time.Second))).To(Succeed())
	_, _, err = conn.ReadMessage()
	g.Expect(err).To(HaveOccurred())
	g.Expect(sess.hub.ClientCount()).To(Equal(0))

	srv.sessions.remove(sess.id)
	srv.Shutdown(time.Second)
}

func TestDAPBindFailureIsRetryable(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, 0)
	defer running.stop(t)

	g.Expect(running.server.StartDAP(running.server.httpAddress)).To(HaveOccurred())
	g.Expect(running.server.StartDAP("127.0.0.1:0")).To(Succeed())
	g.Expect(running.server.StartDAP("127.0.0.1:0")).To(MatchError(ErrDAPAlreadyStarted))
}

func TestDurationMillisecondsIsExact(t *testing.T) {
	g := NewWithT(t)
	g.Expect(durationMilliseconds(0)).To(Equal(int64(0)))
	g.Expect(durationMilliseconds(time.Millisecond)).To(Equal(int64(1)))
	g.Expect(durationMilliseconds(150 * time.Millisecond)).To(Equal(int64(150)))
}
