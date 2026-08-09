package debugger

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/bingosuite/bingo/pkg/protocol"
)

type emitLogRecord struct {
	level   slog.Level
	message string
}

type emitLogCapture struct {
	mu      sync.Mutex
	records []emitLogRecord
}

func (h *emitLogCapture) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *emitLogCapture) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, emitLogRecord{
		level:   record.Level,
		message: record.Message,
	})
	return nil
}

func (h *emitLogCapture) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *emitLogCapture) WithGroup(string) slog.Handler {
	return h
}

func (h *emitLogCapture) snapshot() []emitLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]emitLogRecord(nil), h.records...)
}

func TestEngineEmitUsesInjectedLogger(t *testing.T) {
	previousDefault := slog.Default()
	globalCapture := &emitLogCapture{}
	slog.SetDefault(slog.New(globalCapture))
	t.Cleanup(func() {
		slog.SetDefault(previousDefault)
	})

	injectedCapture := &emitLogCapture{}
	e := &engine{
		events: make(chan protocol.Event, 1),
		log:    slog.New(injectedCapture),
	}

	e.emit(protocol.EventOutput, make(chan struct{}))
	e.emit(protocol.EventOutput, protocol.OutputPayload{Content: "first"})
	e.emit(protocol.EventOutput, protocol.OutputPayload{Content: "second"})

	want := []emitLogRecord{
		{level: slog.LevelError, message: "engine.emit: marshal event failed"},
		{level: slog.LevelWarn, message: "engine.emit: events buffer full — dropping"},
	}
	got := injectedCapture.snapshot()
	if len(got) != len(want) {
		t.Fatalf("injected logger received %d records, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("injected record %d = %#v, want %#v", i, got[i], want[i])
		}
	}

	if got := globalCapture.snapshot(); len(got) != 0 {
		t.Errorf("global logger received %d records, want none: %#v", len(got), got)
	}
}
