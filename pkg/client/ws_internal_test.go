package client

import (
	"errors"
	"testing"

	"github.com/gorilla/websocket"
)

func TestNormalizeSendError(t *testing.T) {
	c := &wsClient{done: make(chan struct{})}
	if err := c.normalizeSendError(websocket.ErrCloseSent); !errors.Is(err, ErrClosed) {
		t.Fatalf("close-sent error = %v, want ErrClosed", err)
	}

	failure := errors.New("write failed")
	if err := c.normalizeSendError(failure); !errors.Is(err, failure) {
		t.Fatalf("open-client error = %v, want original failure", err)
	}

	close(c.done)
	if err := c.normalizeSendError(failure); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed-client error = %v, want ErrClosed", err)
	}
}
