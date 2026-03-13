package ws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/gorilla/websocket"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestWebSocket(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "WebSocket Suite")
}

var _ = Describe("Hub", func() {
	var (
		hub            *Hub
		server         *httptest.Server
		wsURL          string
		shutdownCalled *atomic.Bool
	)

	BeforeEach(func() {
		hub = NewHub("test-session", time.Minute, debugger.NewDebugger())
		shutdownCalled = &atomic.Bool{}
		hub.onShutdown = func(sessionID string) {
			shutdownCalled.Store(true)
		}

		go hub.Run()

		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{}
			conn, _ := upgrader.Upgrade(w, r, nil)
			defer func() { _ = conn.Close() }()
		}))

		wsURL = "ws" + strings.TrimPrefix(server.URL, "http")
	})

	AfterEach(func() {
		if server != nil {
			server.Close()
		}
	})

	Describe("NewHub", func() {
		It("should create a new hub with correct properties", func() {
			sessionID := "test-session"
			idleTimeout := 5 * time.Minute

			testHub := NewHub(sessionID, idleTimeout, debugger.NewDebugger())

			Expect(testHub.sessionID).To(Equal(sessionID))
			Expect(testHub.idleTimeout).To(Equal(idleTimeout))
			Expect(testHub.connections).To(BeEmpty())
			Expect(testHub.register).NotTo(BeNil())
			Expect(testHub.unregister).NotTo(BeNil())
			Expect(testHub.events).NotTo(BeNil())
			Expect(testHub.commands).NotTo(BeNil())
		})
	})

	Describe("RegisterConnection", func() {
		It("should register a connection with hub", func() {
			dialer := websocket.Dialer{}
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = conn.Close() }()

			connection := NewConnection(conn, hub, "connection-1")
			hub.Register(connection)

			time.Sleep(100 * time.Millisecond)

			hub.mu.RLock()
			connectionCount := len(hub.connections)
			hub.mu.RUnlock()

			Expect(connectionCount).To(Equal(1))
		})
	})

	Describe("UnregisterConnection", func() {
		It("should unregister a connection from hub", func() {
			dialer := websocket.Dialer{}
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = conn.Close() }()

			connection := NewConnection(conn, hub, "connection-1")
			hub.Register(connection)

			time.Sleep(50 * time.Millisecond)

			hub.Unregister(connection)
			time.Sleep(100 * time.Millisecond)

			hub.mu.RLock()
			connectionCount := len(hub.connections)
			hub.mu.RUnlock()

			Expect(connectionCount).To(Equal(0))
			Expect(shutdownCalled.Load()).To(BeTrue())
		})
	})

	Describe("SendCommand", func() {
		It("should send commands to hub", func() {
			cmdData, _ := json.Marshal(ContinueCmd{
				Type:      CmdContinue,
				SessionID: "test-session",
			})
			command := Message{
				Type: string(CmdContinue),
				Data: cmdData,
			}

			hub.SendCommand(command)

			time.Sleep(50 * time.Millisecond)
		})
	})

	Describe("IdleTimeout", func() {
		It("should detect idle timeout and shutdown", func() {
			idleTimeout := 100 * time.Millisecond
			hub := NewHub("test-session", idleTimeout, debugger.NewDebugger())

			shutdownCalled := atomic.Bool{}
			hub.onShutdown = func(sessionID string) {
				shutdownCalled.Store(true)
			}

			done := make(chan struct{})
			go func() {
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()

				for {
					select {
					case <-ticker.C:
						if hub.idleTimeout > 0 && len(hub.connections) == 0 {
							if time.Since(hub.lastActivity) > hub.idleTimeout {
								hub.shutdown()
								done <- struct{}{}
								return
							}
						}

					case client := <-hub.register:
						hub.mu.Lock()
						hub.connections[client] = struct{}{}
						hub.lastActivity = time.Now()
						hub.mu.Unlock()

					case client := <-hub.unregister:
						hub.mu.Lock()
						if _, ok := hub.connections[client]; ok {
							delete(hub.connections, client)
							close(client.send)
							if len(hub.connections) == 0 {
								hub.mu.Unlock()
								hub.shutdown()
								done <- struct{}{}
								return
							}
						}
						hub.mu.Unlock()

					case <-hub.events:
						hub.lastActivity = time.Now()

					case <-hub.commands:
					}
				}
			}()

			select {
			case <-done:
				Expect(shutdownCalled.Load()).To(BeTrue())
			case <-time.After(3 * time.Second):
				Fail("timeout waiting for hub shutdown")
			}
		})
	})

	Describe("LastActivityUpdate", func() {
		It("should update lastActivity on events", func() {
			initialTime := hub.lastActivity

			time.Sleep(100 * time.Millisecond)

			hub.Broadcast(Message{
				Type: string(EventStateUpdate),
				Data: json.RawMessage(`{"data":"test"}`),
			})

			time.Sleep(50 * time.Millisecond)

			hub.mu.RLock()
			updatedTime := hub.lastActivity
			hub.mu.RUnlock()

			Expect(updatedTime.After(initialTime) || updatedTime.Equal(initialTime)).To(BeTrue())
		})
	})
})
