package ws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/bingosuite/bingo/config"
	"github.com/gorilla/websocket"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Server", func() {
	Describe("NewServer", func() {
		It("should create a new server", func() {
			http.DefaultServeMux = http.NewServeMux()
			server := NewServer("localhost:0", nil)
			Expect(server).NotTo(BeNil())
			Expect(server.addr).To(Equal("localhost:0"))
			Expect(server.hubs).To(BeEmpty())
		})
	})

	Describe("Serve", func() {
		It("should return error for invalid address", func() {
			http.DefaultServeMux = http.NewServeMux()
			server := NewServer("bad::addr", nil)
			err := server.Serve()
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("CreateHub", func() {
		It("should create a new hub", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 100,
				IdleTimeout: time.Minute,
			}

			server := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			hub1, err := server.CreateHub("session-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(hub1).NotTo(BeNil())
			Expect(hub1.sessionID).To(Equal("session-1"))

			hub2, err := server.CreateHub("session-2")
			Expect(err).NotTo(HaveOccurred())
			Expect(hub2).NotTo(BeNil())
			Expect(hub2).NotTo(Equal(hub1))
		})

		It("should return error if session already exists", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 100,
				IdleTimeout: time.Minute,
			}

			server := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			hub1, err := server.CreateHub("session-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(hub1).NotTo(BeNil())

			hub2, err := server.CreateHub("session-1")
			Expect(err).To(HaveOccurred())
			Expect(hub2).To(BeNil())
			Expect(err.Error()).To(ContainSubstring("session already exists"))
		})
	})

	Describe("GetHub", func() {
		It("should retrieve an existing hub", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 100,
				IdleTimeout: time.Minute,
			}

			server := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			hub1, err := server.CreateHub("session-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(hub1).NotTo(BeNil())

			hub2, err := server.GetHub("session-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(hub2).To(Equal(hub1))
		})

		It("should return error if session does not exist", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 100,
				IdleTimeout: time.Minute,
			}

			server := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			hub, err := server.GetHub("nonexistent")
			Expect(err).To(HaveOccurred())
			Expect(hub).To(BeNil())
			Expect(err.Error()).To(ContainSubstring("session not found"))
		})
	})

	Describe("MaxSessions", func() {
		It("should enforce max sessions limit", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 2,
				IdleTimeout: time.Minute,
			}

			server := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			hub1, err := server.CreateHub("session-1")
			Expect(err).NotTo(HaveOccurred())
			hub1.onShutdown = server.removeHub
			Expect(hub1).NotTo(BeNil())

			hub2, err := server.CreateHub("session-2")
			Expect(err).NotTo(HaveOccurred())
			hub2.onShutdown = server.removeHub
			Expect(hub2).NotTo(BeNil())

			hub3, err := server.CreateHub("session-3")
			Expect(err).To(HaveOccurred())
			Expect(hub3).To(BeNil())

			server.mu.RLock()
			hubCount := len(server.hubs)
			server.mu.RUnlock()
			Expect(hubCount).To(Equal(2))
		})
	})

	Describe("removeHub", func() {
		It("should remove hub from server", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 10,
				IdleTimeout: time.Minute,
			}

			server := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			hub, err := server.CreateHub("session-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(hub).NotTo(BeNil())

			server.removeHub("session-1")

			server.mu.RLock()
			hubCount := len(server.hubs)
			server.mu.RUnlock()
			Expect(hubCount).To(Equal(0))
		})
	})

	Describe("serveWebSocket", func() {
		It("should create session and ack when session is missing", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 10,
				IdleTimeout: time.Minute,
			}

			s := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			server := httptest.NewServer(http.HandlerFunc(s.getOrCreateSession))
			defer server.Close()

			dialer := websocket.Dialer{}
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/"
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())

			var msg Message
			_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			Expect(conn.ReadJSON(&msg)).To(Succeed())
			Expect(msg.Type).To(Equal(string(EventSessionStarted)))

			var started SessionStartedEvent
			Expect(json.Unmarshal(msg.Data, &started)).To(Succeed())
			Expect(started.SessionID).NotTo(BeEmpty())

			_ = conn.Close()

			Eventually(func() int {
				s.mu.RLock()
				defer s.mu.RUnlock()
				return len(s.hubs)
			}, 2*time.Second, 50*time.Millisecond).Should(BeNumerically(">=", 1))

			time.Sleep(50 * time.Millisecond)
		})

		It("should return on upgrade error", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 10,
				IdleTimeout: time.Minute,
			}

			s := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			server := httptest.NewServer(http.HandlerFunc(s.getOrCreateSession))
			defer server.Close()

			resp, err := http.Get(server.URL + "/ws/?session=session-1")
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()

			s.mu.RLock()
			hubCount := len(s.hubs)
			s.mu.RUnlock()
			Expect(hubCount).To(Equal(0))
		})

		It("should reject connection with provided session ID that does not exist", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 10,
				IdleTimeout: time.Minute,
			}

			s := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			server := httptest.NewServer(http.HandlerFunc(s.getOrCreateSession))
			defer server.Close()

			dialer := websocket.Dialer{}
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/?session=nonexistent"
			conn, resp, _ := dialer.Dial(wsURL, nil)
			if conn != nil {
				_ = conn.Close()
			}
			if resp != nil {
				_ = resp.Body.Close()
			}

			s.mu.RLock()
			hubCount := len(s.hubs)
			s.mu.RUnlock()
			Expect(hubCount).To(Equal(0))
		})

		It("should accept connection with provided session ID that exists", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 10,
				IdleTimeout: time.Minute,
			}

			s := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			hub, err := s.CreateHub("session-1")
			Expect(err).NotTo(HaveOccurred())
			hub.onShutdown = s.removeHub
			go hub.Run()

			server := httptest.NewServer(http.HandlerFunc(s.getOrCreateSession))
			defer server.Close()

			dialer := websocket.Dialer{}
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/?session=session-1"
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = conn.Close() }()

			Eventually(func() int {
				s.mu.RLock()
				defer s.mu.RUnlock()
				hub, ok := s.hubs["session-1"]
				if !ok || hub == nil {
					return 0
				}
				hub.mu.RLock()
				defer hub.mu.RUnlock()
				return len(hub.connections)
			}, 2*time.Second, 50*time.Millisecond).Should(BeNumerically(">=", 1))
		})
	})
})
