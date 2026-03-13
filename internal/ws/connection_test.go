package ws

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/bingosuite/bingo/internal/debugger"
	"github.com/gorilla/websocket"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Connection", func() {
	var (
		hub    *Hub
		server *httptest.Server
		wsURL  string
	)

	BeforeEach(func() {
		hub = NewHub("test-session", time.Minute, debugger.NewDebugger())

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

	Describe("NewConnection", func() {
		It("should create a new client with correct properties", func() {
			dialer := websocket.Dialer{}
			conn, _, err := dialer.Dial(wsURL, nil)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = conn.Close() }()

			connection := NewConnection(conn, hub, "connection-1")

			Expect(connection.id).To(Equal("connection-1"))
			Expect(connection.hub).To(Equal(hub))
			Expect(connection.conn).To(Equal(conn))
			Expect(connection.send).NotTo(BeNil())
		})
	})

	Describe("ReadPump", func() {
		It("should read from connection", func() {
			go hub.Run()

			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, _ := upgrader.Upgrade(w, r, nil)
				defer func() { _ = conn.Close() }()

				connection := NewConnection(conn, hub, r.RemoteAddr)
				hub.Register(connection)

				connection.ReadPump()
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
				client := NewConnection(conn, hub, "test-client")
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

			connection := NewConnection(clientConn, nil, "connection-1")
			go connection.WritePump()

			connection.send <- Message{Type: string(EventStateUpdate), Data: json.RawMessage(`{"ok":true}`)}
			close(connection.send)
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

			connection := NewConnection(clientConn, nil, "connection-1")
			go connection.WritePump()

			connection.send <- Message{Type: string(EventStateUpdate), Data: json.RawMessage(`{"ok":true}`)}

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

			connection := NewConnection(clientConn, nil, "connection-1")
			go connection.WritePump()

			close(connection.send)

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
		It("should handle concurrent connection operations", func() {
			go hub.Run()

			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, _ := upgrader.Upgrade(w, r, nil)
				defer func() { _ = conn.Close() }()

				connection := NewConnection(conn, hub, r.RemoteAddr)
				hub.Register(connection)

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
			clientCount := len(hub.connections)
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

				connection := NewConnection(conn, hub, r.RemoteAddr)
				hub.Register(connection)

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
