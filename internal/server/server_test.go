package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/bingosuite/bingo/pkg/protocol"
)

func TestServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Server Suite")
}


func toWS(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

func recvState(conn *websocket.Conn) (protocol.SessionStatePayload, error) {
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return protocol.SessionStatePayload{}, err
	}
	var evt protocol.Event
	if err := json.Unmarshal(msg, &evt); err != nil {
		return protocol.SessionStatePayload{}, err
	}
	var p protocol.SessionStatePayload
	return p, protocol.DecodeEventPayload(evt, &p)
}


var _ = Describe("Server", func() {

	var (
		srv *Server
		ts  *httptest.Server
	)

	BeforeEach(func() {
		srv = New(":0", nil)
		ts = httptest.NewServer(srv.httpServer.Handler)
	})

	AfterEach(func() {
		srv.cancel()
		ts.Close()
		time.Sleep(50 * time.Millisecond)
	})


	Describe("GET /api/sessions", func() {
		It("returns an empty JSON array when no sessions exist", func() {
			resp, err := http.Get(ts.URL + "/api/sessions")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(resp.Header.Get("Content-Type")).To(Equal("application/json"))

			var sessions []SessionInfo
			Expect(json.NewDecoder(resp.Body).Decode(&sessions)).To(Succeed())
			Expect(sessions).To(BeEmpty())
		})

		It("rejects non-GET requests", func() {
			resp, err := http.Post(ts.URL+"/api/sessions", "", nil)
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusMethodNotAllowed))
		})

		It("includes sessions created via WebSocket", func() {
			conn, _, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			_, _ = recvState(conn)

			resp, err := http.Get(ts.URL + "/api/sessions")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()

			var sessions []SessionInfo
			Expect(json.NewDecoder(resp.Body).Decode(&sessions)).To(Succeed())
			Expect(sessions).To(HaveLen(1))
			Expect(sessions[0].State).To(Equal(protocol.StateIdle))
			Expect(sessions[0].Clients).To(Equal(1))
		})
	})


	Describe("WebSocket endpoint", func() {

		It("returns 400 when neither ?create nor ?session is specified", func() {
			resp, err := http.Get(ts.URL + "/ws")
			Expect(err).NotTo(HaveOccurred())
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
		})

		Context("?create", func() {
			It("upgrades to WebSocket and sends an idle welcome state", func() {
				conn, resp, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
				Expect(err).NotTo(HaveOccurred())
				defer conn.Close()
				Expect(resp.StatusCode).To(Equal(http.StatusSwitchingProtocols))

				p, err := recvState(conn)
				Expect(err).NotTo(HaveOccurred())
				Expect(p.State).To(Equal(protocol.StateIdle))
				Expect(p.Clients).To(Equal(1))
				Expect(p.SessionID).NotTo(BeEmpty())
			})

			It("generates a valid UUID session ID", func() {
				conn, _, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
				Expect(err).NotTo(HaveOccurred())
				defer conn.Close()

				p, err := recvState(conn)
				Expect(err).NotTo(HaveOccurred())
				Expect(p.SessionID).To(MatchRegexp(
					`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`))
			})

			It("creates distinct sessions for each request", func() {
				conn1, _, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
				Expect(err).NotTo(HaveOccurred())
				defer conn1.Close()
				p1, _ := recvState(conn1)

				conn2, _, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
				Expect(err).NotTo(HaveOccurred())
				defer conn2.Close()
				p2, _ := recvState(conn2)

				Expect(p1.SessionID).NotTo(Equal(p2.SessionID))
				Expect(srv.sessions.count()).To(Equal(2))
			})
		})

		Context("?session={id}", func() {
			It("joins an existing session with correct client count", func() {
				conn1, _, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
				Expect(err).NotTo(HaveOccurred())
				defer conn1.Close()
				p1, _ := recvState(conn1)

				conn2, _, err := websocket.DefaultDialer.Dial(
					toWS(ts, "/ws?session="+p1.SessionID), nil)
				Expect(err).NotTo(HaveOccurred())
				defer conn2.Close()

				p2, err := recvState(conn2)
				Expect(err).NotTo(HaveOccurred())
				Expect(p2.SessionID).To(Equal(p1.SessionID))
				Expect(p2.Clients).To(Equal(2))
			})

			It("closes the connection when the session does not exist", func() {
				conn, _, err := websocket.DefaultDialer.Dial(
					toWS(ts, "/ws?session=does-not-exist"), nil)
				Expect(err).NotTo(HaveOccurred())

				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				_, _, err = conn.ReadMessage()
				Expect(err).To(HaveOccurred())
				conn.Close()
			})
		})
	})


	Describe("session lifecycle", func() {
		It("removes the session when the sole client disconnects", func() {
			conn, _, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
			Expect(err).NotTo(HaveOccurred())

			Eventually(srv.sessions.count, "2s", "50ms").Should(Equal(1))

			conn.Close()

			Eventually(srv.sessions.count, "2s", "50ms").Should(Equal(0))
		})

		It("keeps the session alive while at least one client remains", func() {
			conn1, _, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
			Expect(err).NotTo(HaveOccurred())
			defer conn1.Close()

			p, _ := recvState(conn1)

			conn2, _, err := websocket.DefaultDialer.Dial(
				toWS(ts, "/ws?session="+p.SessionID), nil)
			Expect(err).NotTo(HaveOccurred())

			conn2.Close()
			time.Sleep(100 * time.Millisecond)

			Expect(srv.sessions.count()).To(Equal(1))
			Expect(srv.sessions.get(p.SessionID)).NotTo(BeNil())
		})

		It("cleans up all sessions on context cancellation", func() {
			conn, _, err := websocket.DefaultDialer.Dial(toWS(ts, "/ws?create"), nil)
			Expect(err).NotTo(HaveOccurred())
			defer conn.Close()

			Eventually(srv.sessions.count, "2s", "50ms").Should(Equal(1))

			srv.cancel()

			Eventually(srv.sessions.count, "2s", "50ms").Should(Equal(0))
		})
	})


	Describe("Start and Shutdown", func() {
		It("starts on a random port and shuts down cleanly", func() {
			s := New("127.0.0.1:0", nil)

			errCh := make(chan error, 1)
			go func() { errCh <- s.Start() }()

			time.Sleep(50 * time.Millisecond)

			s.Shutdown(time.Second)

			Eventually(errCh, "2s").Should(Receive(BeNil()))
		})
	})
})
