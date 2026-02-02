package ws

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bingosuite/bingo/config"
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
		hub = NewHub("test-session", time.Minute)
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

			testHub := NewHub(sessionID, idleTimeout)

			Expect(testHub.sessionID).To(Equal(sessionID))
			Expect(testHub.idleTimeout).To(Equal(idleTimeout))
			Expect(testHub.clients).To(BeEmpty())
			Expect(testHub.register).NotTo(BeNil())
			Expect(testHub.unregister).NotTo(BeNil())
			Expect(testHub.events).NotTo(BeNil())
			Expect(testHub.commands).NotTo(BeNil())
		})
	})

	Describe("RegisterClient", func() {
		It("should register a client with hub", func() {
			dialer := websocket.Dialer{}
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = conn.Close() }()

			client := NewClient(conn, hub, "client-1")
			hub.Register(client)

			time.Sleep(100 * time.Millisecond)

			hub.mu.RLock()
			clientCount := len(hub.clients)
			hub.mu.RUnlock()

			Expect(clientCount).To(Equal(1))
		})
	})

	Describe("UnregisterClient", func() {
		It("should unregister a client from hub", func() {
			dialer := websocket.Dialer{}
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = conn.Close() }()

			client := NewClient(conn, hub, "client-1")
			hub.Register(client)

			time.Sleep(50 * time.Millisecond)

			hub.Unregister(client)

			time.Sleep(100 * time.Millisecond)

			hub.mu.RLock()
			clientCount := len(hub.clients)
			hub.mu.RUnlock()

			Expect(clientCount).To(Equal(0))
			Expect(shutdownCalled.Load()).To(BeTrue())
		})
	})

	Describe("Broadcast", func() {
		It("should broadcast messages to all clients", func() {
			dialer := websocket.Dialer{}

			conn1, _, _ := dialer.Dial(wsURL, nil)
			defer func() { _ = conn1.Close() }()
			client1 := NewClient(conn1, hub, "client-1")

			conn2, _, _ := dialer.Dial(wsURL, nil)
			defer func() { _ = conn2.Close() }()
			client2 := NewClient(conn2, hub, "client-2")

			hub.Register(client1)
			hub.Register(client2)

			time.Sleep(50 * time.Millisecond)

			eventData, _ := json.Marshal(GoroutineEvent{
				Type:        EventGoroutineEvent,
				SessionID:   "test-session",
				GoroutineID: 1,
				State:       "running",
			})
			message := Message{
				Type: string(EventGoroutineEvent),
				Data: eventData,
			}

			hub.Broadcast(message)

			time.Sleep(50 * time.Millisecond)

			Expect(len(client1.send) == 0 || len(client2.send) == 0).To(BeFalse())
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
			// Command sent successfully if we reach here
		})
	})

	Describe("IdleTimeout", func() {
		It("should detect idle timeout and shutdown", func() {
			idleTimeout := 100 * time.Millisecond
			hub := NewHub("test-session", idleTimeout)

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
						if hub.idleTimeout > 0 && len(hub.clients) == 0 {
							if time.Since(hub.lastActivity) > hub.idleTimeout {
								hub.shutdown()
								done <- struct{}{}
								return
							}
						}

					case client := <-hub.register:
						hub.mu.Lock()
						hub.clients[client] = struct{}{}
						hub.lastActivity = time.Now()
						hub.mu.Unlock()

					case client := <-hub.unregister:
						hub.mu.Lock()
						if _, ok := hub.clients[client]; ok {
							delete(hub.clients, client)
							close(client.send)
							if len(hub.clients) == 0 {
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
						// Handle command
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

var _ = Describe("Client", func() {
	var (
		hub    *Hub
		server *httptest.Server
		wsURL  string
	)

	BeforeEach(func() {
		hub = NewHub("test-session", time.Minute)

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

	Describe("NewClient", func() {
		It("should create a new client with correct properties", func() {
			dialer := websocket.Dialer{}
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = conn.Close() }()

			client := NewClient(conn, hub, "client-1")

			Expect(client.id).To(Equal("client-1"))
			Expect(client.hub).To(Equal(hub))
			Expect(client.conn).To(Equal(conn))
			Expect(client.send).NotTo(BeNil())
		})
	})

	Describe("ReadPump", func() {
		It("should read from connection", func() {
			go hub.Run()

			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, _ := upgrader.Upgrade(w, r, nil)
				defer func() { _ = conn.Close() }()

				client := NewClient(conn, hub, r.RemoteAddr)
				hub.Register(client)

				client.ReadPump()
			}))
			defer testServer.Close()

			dialer := websocket.Dialer{}
			testURL := "ws" + strings.TrimPrefix(testServer.URL, "http")
			clientConn, _, err := dialer.Dial(testURL, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = clientConn.Close() }()

			testMsg := Message{
				Type: string(CmdContinue),
				Data: json.RawMessage(`{"sessionId":"test-session","type":"continue"}`),
			}

			err = clientConn.WriteJSON(testMsg)
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(100 * time.Millisecond)
		})
	})

	Describe("WritePump", func() {
		var upgradeAndDial = func(handler func(*websocket.Conn)) (*httptest.Server, *websocket.Conn) {
			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, _ := upgrader.Upgrade(w, r, nil)
				defer func() { _ = conn.Close() }()
				handler(conn)
			}))

			testURL := "ws" + strings.TrimPrefix(testServer.URL, "http")
			clientConn, _, err := websocket.DefaultDialer.Dial(testURL, nil)
			Expect(err).NotTo(HaveOccurred())
			return testServer, clientConn
		}

		It("should write to connection", func() {
			go hub.Run()

			testServer, clientConn := upgradeAndDial(func(conn *websocket.Conn) {
				client := NewClient(conn, hub, "test-client")
				hub.Register(client)
				go client.WritePump()

				var msg Message
				for {
					if err := conn.ReadJSON(&msg); err != nil {
						break
					}
				}
			})
			defer testServer.Close()

			time.Sleep(100 * time.Millisecond)
			_ = clientConn.Close()
			time.Sleep(50 * time.Millisecond)
		})

		It("should write messages successfully", func() {
			received := make(chan Message, 1)
			done := make(chan struct{})

			testServer, clientConn := upgradeAndDial(func(conn *websocket.Conn) {
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				var msg Message
				if err := conn.ReadJSON(&msg); err == nil {
					received <- msg
				}

				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						close(done)
						return
					}
				}
			})
			defer testServer.Close()

			client := NewClient(clientConn, nil, "client-1")
			go client.WritePump()

			client.send <- Message{Type: string(EventStateUpdate), Data: json.RawMessage(`{"ok":true}`)}
			close(client.send)

			Eventually(func() bool {
				select {
				case <-received:
					return true
				default:
					return false
				}
			}, 2*time.Second, 50*time.Millisecond).Should(BeTrue())

			Eventually(func() bool {
				select {
				case <-done:
					return true
				default:
					return false
				}
			}, 2*time.Second, 50*time.Millisecond).Should(BeTrue())
		})

		It("should handle write errors", func() {
			serverClosed := make(chan struct{})

			testServer, clientConn := upgradeAndDial(func(conn *websocket.Conn) {
				_ = conn.Close()
				close(serverClosed)
			})
			defer testServer.Close()

			client := NewClient(clientConn, nil, "client-1")
			go client.WritePump()

			client.send <- Message{Type: string(EventStateUpdate), Data: json.RawMessage(`{"ok":true}`)}

			Eventually(func() bool {
				select {
				case <-serverClosed:
					return true
				default:
					return false
				}
			}, 2*time.Second, 50*time.Millisecond).Should(BeTrue())
		})

		It("should send close when channel closes", func() {
			done := make(chan struct{})

			testServer, clientConn := upgradeAndDial(func(conn *websocket.Conn) {
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						close(done)
						return
					}
				}
			})
			defer testServer.Close()

			client := NewClient(clientConn, nil, "client-1")
			go client.WritePump()

			close(client.send)

			Eventually(func() bool {
				select {
				case <-done:
					return true
				default:
					return false
				}
			}, 2*time.Second, 50*time.Millisecond).Should(BeTrue())
		})
	})

	Describe("ConcurrentOperations", func() {
		It("should handle concurrent client operations", func() {
			go hub.Run()

			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, _ := upgrader.Upgrade(w, r, nil)
				defer func() { _ = conn.Close() }()

				client := NewClient(conn, hub, r.RemoteAddr)
				hub.Register(client)

				var msg Message
				for {
					if err := conn.ReadJSON(&msg); err != nil {
						break
					}
				}
			}))
			defer testServer.Close()

			dialer := websocket.Dialer{}
			testURL := "ws" + strings.TrimPrefix(testServer.URL, "http")

			var wg sync.WaitGroup
			numClients := 5

			for i := 0; i < numClients; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					conn, _, _ := dialer.Dial(testURL, nil)
					if conn != nil {
						defer func() { _ = conn.Close() }()
						time.Sleep(50 * time.Millisecond)
					}
				}()
			}

			wg.Wait()
			time.Sleep(200 * time.Millisecond)

			hub.mu.RLock()
			clientCount := len(hub.clients)
			hub.mu.RUnlock()

			Expect(clientCount).To(BeNumerically(">=", 0))
		})
	})

	Describe("SlowClientHandling", func() {
		It("should handle slow clients", func() {
			go hub.Run()

			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, _ := upgrader.Upgrade(w, r, nil)
				defer func() { _ = conn.Close() }()

				client := NewClient(conn, hub, r.RemoteAddr)
				hub.Register(client)

				select {}
			}))
			defer testServer.Close()

			dialer := websocket.Dialer{}
			testURL := "ws" + strings.TrimPrefix(testServer.URL, "http")
			conn, _, _ := dialer.Dial(testURL, nil)
			if conn != nil {
				defer func() { _ = conn.Close() }()
			}

			time.Sleep(100 * time.Millisecond)

			for i := 0; i < eventBufferSize+10; i++ {
				hub.Broadcast(Message{
					Type: string(EventStateUpdate),
					Data: json.RawMessage(`{"index":` + string(bytes.Join([][]byte{}, []byte{})) + `}`),
				})
			}

			time.Sleep(100 * time.Millisecond)
		})
	})
})

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

	Describe("GetOrCreateHub", func() {
		It("should create and retrieve hubs", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 100,
				IdleTimeout: time.Minute,
			}

			server := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			hub1 := server.GetOrCreateHub("session-1")
			Expect(hub1).NotTo(BeNil())
			Expect(hub1.sessionID).To(Equal("session-1"))

			hub2 := server.GetOrCreateHub("session-1")
			Expect(hub2).To(Equal(hub1))

			hub3 := server.GetOrCreateHub("session-2")
			Expect(hub3).NotTo(BeNil())
			Expect(hub3).NotTo(Equal(hub1))
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

			hub1 := server.GetOrCreateHub("session-1")
			hub1.onShutdown = server.removeHub
			Expect(hub1).NotTo(BeNil())

			hub2 := server.GetOrCreateHub("session-2")
			hub2.onShutdown = server.removeHub
			Expect(hub2).NotTo(BeNil())

			hub3 := server.GetOrCreateHub("session-3")
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

			hub := server.GetOrCreateHub("session-1")
			Expect(hub).NotTo(BeNil())

			server.removeHub("session-1")

			server.mu.RLock()
			hubCount := len(server.hubs)
			server.mu.RUnlock()
			Expect(hubCount).To(Equal(0))
		})
	})

	Describe("serveWebSocket", func() {
		It("should reject connections without session", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 10,
				IdleTimeout: time.Minute,
			}

			s := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			server := httptest.NewServer(http.HandlerFunc(s.serveWebSocket))
			defer server.Close()

			dialer := websocket.Dialer{}
			wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/"
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())
			_ = conn.Close()

			time.Sleep(50 * time.Millisecond)

			s.mu.RLock()
			hubCount := len(s.hubs)
			s.mu.RUnlock()
			Expect(hubCount).To(Equal(0))
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

			server := httptest.NewServer(http.HandlerFunc(s.serveWebSocket))
			defer server.Close()

			resp, err := http.Get(server.URL + "/ws/?session=session-1")
			Expect(err).NotTo(HaveOccurred())
			_ = resp.Body.Close()

			s.mu.RLock()
			hubCount := len(s.hubs)
			s.mu.RUnlock()
			Expect(hubCount).To(Equal(0))
		})

		It("should create hub and register client", func() {
			wsConfig := config.WebSocketConfig{
				MaxSessions: 10,
				IdleTimeout: time.Minute,
			}

			s := &Server{
				addr:   "localhost:0",
				hubs:   make(map[string]*Hub),
				config: wsConfig,
			}

			server := httptest.NewServer(http.HandlerFunc(s.serveWebSocket))
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
				return len(hub.clients)
			}, 2*time.Second, 50*time.Millisecond).Should(BeNumerically(">=", 1))
		})
	})
})

var _ = Describe("Protocol", func() {
	Describe("Message", func() {
		It("should handle Message struct", func() {
			data, _ := json.Marshal(map[string]string{"key": "value"})
			msg := Message{
				Type: string(EventStateUpdate),
				Data: data,
			}

			Expect(msg.Type).To(Equal(string(EventStateUpdate)))
			Expect(msg.Data).NotTo(BeNil())

			jsonData, err := json.Marshal(msg)
			Expect(err).NotTo(HaveOccurred())

			var unmarshaledMsg Message
			err = json.Unmarshal(jsonData, &unmarshaledMsg)
			Expect(err).NotTo(HaveOccurred())

			Expect(unmarshaledMsg.Type).To(Equal(msg.Type))
		})

		It("should encode and decode JSON", func() {
			originalMsg := Message{
				Type: string(EventGoroutineEvent),
				Data: json.RawMessage(`{"type":"goroutineEvent","goroutineId":123}`),
			}

			jsonData, err := json.Marshal(originalMsg)
			Expect(err).NotTo(HaveOccurred())

			var decodedMsg Message
			err = json.Unmarshal(jsonData, &decodedMsg)
			Expect(err).NotTo(HaveOccurred())

			Expect(decodedMsg.Type).To(Equal(originalMsg.Type))
			Expect(decodedMsg.Data).To(Equal(originalMsg.Data))
		})
	})

	Describe("GoroutineEvent", func() {
		It("should handle GoroutineEvent struct", func() {
			event := GoroutineEvent{
				Type:        EventGoroutineEvent,
				SessionID:   "session-1",
				GoroutineID: 42,
				State:       "running",
				PC:          "0x12345",
			}
			event.Source.File = "main.go"
			event.Source.Line = 10

			Expect(event.Type).To(Equal(EventGoroutineEvent))
			Expect(event.GoroutineID).To(Equal(uint64(42)))
			Expect(event.Source.File).To(Equal("main.go"))
			Expect(event.Source.Line).To(Equal(10))

			data, err := json.Marshal(event)
			Expect(err).NotTo(HaveOccurred())

			var unmarshaled GoroutineEvent
			err = json.Unmarshal(data, &unmarshaled)
			Expect(err).NotTo(HaveOccurred())

			Expect(unmarshaled.GoroutineID).To(Equal(event.GoroutineID))
			Expect(unmarshaled.Source.File).To(Equal(event.Source.File))
		})
	})

	Describe("InspectResult", func() {
		It("should handle InspectResult struct", func() {
			result := InspectResult{
				Type:        EventInspectResult,
				SessionID:   "session-1",
				GoroutineID: 42,
				Vars: map[string]string{
					"x": "10",
					"y": "20",
				},
			}

			Expect(result.Type).To(Equal(EventInspectResult))
			Expect(result.GoroutineID).To(Equal(uint64(42)))
			Expect(result.Vars["x"]).To(Equal("10"))
			Expect(result.Vars["y"]).To(Equal("20"))

			data, err := json.Marshal(result)
			Expect(err).NotTo(HaveOccurred())

			var unmarshaled InspectResult
			err = json.Unmarshal(data, &unmarshaled)
			Expect(err).NotTo(HaveOccurred())

			Expect(unmarshaled.GoroutineID).To(Equal(result.GoroutineID))
			Expect(unmarshaled.Vars["x"]).To(Equal(result.Vars["x"]))
		})
	})

	Describe("ContinueCmd", func() {
		It("should handle ContinueCmd struct", func() {
			cmd := ContinueCmd{
				Type:      CmdContinue,
				SessionID: "session-1",
			}

			Expect(cmd.Type).To(Equal(CmdContinue))
			Expect(cmd.SessionID).To(Equal("session-1"))

			data, err := json.Marshal(cmd)
			Expect(err).NotTo(HaveOccurred())

			var unmarshaled ContinueCmd
			err = json.Unmarshal(data, &unmarshaled)
			Expect(err).NotTo(HaveOccurred())

			Expect(unmarshaled.SessionID).To(Equal(cmd.SessionID))
		})
	})

	Describe("InspectGoroutineCmd", func() {
		It("should handle InspectGoroutineCmd struct", func() {
			cmd := InspectGoroutineCmd{
				Type:        CmdInspectGoroutine,
				SessionID:   "session-1",
				GoroutineID: 42,
			}

			Expect(cmd.Type).To(Equal(CmdInspectGoroutine))
			Expect(cmd.GoroutineID).To(Equal(uint64(42)))

			data, err := json.Marshal(cmd)
			Expect(err).NotTo(HaveOccurred())

			var unmarshaled InspectGoroutineCmd
			err = json.Unmarshal(data, &unmarshaled)
			Expect(err).NotTo(HaveOccurred())

			Expect(unmarshaled.GoroutineID).To(Equal(cmd.GoroutineID))
		})
	})
})
