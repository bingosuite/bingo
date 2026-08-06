package server

import (
	"context"
	"encoding/json"
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
	srv, err := NewWithIdleTimeout(":0", -time.Second, nil)
	g.Expect(err).To(MatchError("idle timeout must not be negative: -1s"))
	g.Expect(srv).To(BeNil())
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

func TestDAPBindFailureIsRetryable(t *testing.T) {
	g := NewWithT(t)
	running := startServer(t, 0)
	defer running.stop(t)

	g.Expect(running.server.StartDAP(running.server.httpAddress)).To(HaveOccurred())
	g.Expect(running.server.StartDAP("127.0.0.1:0")).To(Succeed())
	g.Expect(running.server.StartDAP("127.0.0.1:0")).To(MatchError(ErrDAPAlreadyStarted))
}

func TestDurationMillisecondsRoundsUp(t *testing.T) {
	g := NewWithT(t)
	g.Expect(durationMilliseconds(0)).To(Equal(int64(0)))
	g.Expect(durationMilliseconds(time.Nanosecond)).To(Equal(int64(1)))
	g.Expect(durationMilliseconds(1500 * time.Microsecond)).To(Equal(int64(2)))
}
