package client

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServerURL(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		path      string
		query     url.Values
		websocket bool
		want      string
	}{
		{"local REST", "localhost:6060", "/api/sessions", nil, false, "http://localhost:6060/api/sessions"},
		{"local WebSocket", "127.0.0.1:6060", "/ws", url.Values{"create": {"1"}}, true, "ws://127.0.0.1:6060/ws?create=1"},
		{"loopback range", "127.0.0.2:6060", "/api/sessions", nil, false, "http://127.0.0.2:6060/api/sessions"},
		{"IPv6 loopback", "[::1]:6060", "/ws", nil, true, "ws://[::1]:6060/ws"},
		{"remote REST", "debug.example:6060", "/api/sessions", nil, false, "https://debug.example:6060/api/sessions"},
		{"remote WebSocket", "debug.example:6060", "/ws", url.Values{"session": {"a&b"}}, true, "wss://debug.example:6060/ws?session=a%26b"},
		{"remote IP", "192.0.2.1:6060", "/ws", nil, true, "wss://192.0.2.1:6060/ws"},
		{"explicit TLS", "https://debug.example", "/ws", nil, true, "wss://debug.example/ws"},
		{"explicit WSS", "wss://debug.example", "/api/sessions", nil, false, "https://debug.example/api/sessions"},
		{"explicit local TLS", "https://localhost:6060", "/api/sessions", nil, false, "https://localhost:6060/api/sessions"},
		{"explicit remote plaintext", "http://debug.example:6060", "/api/sessions", nil, false, "http://debug.example:6060/api/sessions"},
		{"explicit remote WS", "ws://debug.example:6060", "/ws", nil, true, "ws://debug.example:6060/ws"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := serverURL(tt.addr, tt.path, tt.query, tt.websocket)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("serverURL(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestServerURLRejectsInvalidAddresses(t *testing.T) {
	for _, addr := range []string{
		"", "debug.example", "debug.example:", ":6060", "debug.example:0", "debug.example:65536",
		"https://", "ftp://debug.example:6060", "https://user:pass@debug.example",
		"https://debug.example:", "https://debug.example/path", "https://debug.example?query=1",
		"https://debug.example?", "https://debug.example#fragment", "https://debug.example#",
	} {
		t.Run(addr, func(t *testing.T) {
			if got, err := serverURL(addr, "/ws", nil, true); err == nil {
				t.Errorf("serverURL(%q) = %q, want error", addr, got)
			}
		})
	}
}

func TestListSessionsRejectsRedirect(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()

	_, err := ListSessions(strings.TrimPrefix(server.URL, "http://"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("ListSessions redirect error = %v, want HTTP 302", err)
	}
	if redirected.Load() {
		t.Fatal("ListSessions followed a redirect")
	}
}
