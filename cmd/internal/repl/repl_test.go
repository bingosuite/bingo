package repl

import (
	"bytes"
	"context"
	"io"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chzyer/readline"
)

type testReader struct {
	results chan *readline.Result
	closed  chan struct{}
	once    sync.Once
	closes  atomic.Int32
	out     bytes.Buffer
}

func newTestReader(results ...*readline.Result) *testReader {
	ch := make(chan *readline.Result, len(results))
	for _, result := range results {
		ch <- result
	}
	return &testReader{
		results: ch,
		closed:  make(chan struct{}),
	}
}

func (r *testReader) Line() *readline.Result {
	select {
	case result := <-r.results:
		return result
	case <-r.closed:
		return &readline.Result{Error: io.EOF}
	}
}

func (r *testReader) Stdout() io.Writer { return &r.out }

func (r *testReader) close() {
	r.once.Do(func() {
		r.closes.Add(1)
		close(r.closed)
	})
}

func TestLoopContinuesAfterPartialInterrupt(t *testing.T) {
	reader := newTestReader(
		&readline.Result{Line: "locals 123", Error: readline.ErrInterrupt},
		&readline.Result{Line: "state"},
		&readline.Result{Error: readline.ErrInterrupt},
	)
	var commands [][]string

	Loop(context.Background(), reader, reader.close, make(chan struct{}), func(args []string) bool {
		commands = append(commands, args)
		return false
	})

	if len(commands) != 1 || len(commands[0]) != 1 || commands[0][0] != "state" {
		t.Fatalf("commands = %#v, want only state after canceled partial input", commands)
	}
	if got := reader.out.String(); got != "bye\n" {
		t.Fatalf("output = %q, want %q", got, "bye\n")
	}
}

func TestLoopCancellationClosesBlockedInput(t *testing.T) {
	reader := newTestReader()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Loop(ctx, reader, reader.close, make(chan struct{}), func([]string) bool {
			t.Error("dispatch called after cancellation")
			return false
		})
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop after cancellation")
	}
	if got := reader.closes.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
	if got := reader.out.String(); got != "" {
		t.Fatalf("output = %q, want no goodbye for cancellation", got)
	}
}

func TestLoopDoesNotDispatchLineReturnedDuringCancellation(t *testing.T) {
	reader := newTestReader(&readline.Result{Line: "continue"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	Loop(ctx, reader, reader.close, make(chan struct{}), func(args []string) bool {
		t.Fatalf("dispatched %v after cancellation", args)
		return false
	})

	if got := reader.out.String(); got != "" {
		t.Fatalf("output = %q, want silent cancellation", got)
	}
}

func TestEditorCloseUnblocksLine(t *testing.T) {
	input, inputWriter := io.Pipe()
	t.Cleanup(func() { _ = inputWriter.Close() })
	editor, err := NewEditor(&readline.Config{
		Stdin:              input,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
		FuncIsTerminal:     func() bool { return false },
		FuncMakeRaw:        func() error { return nil },
		FuncExitRaw:        func() error { return nil },
		FuncGetWidth:       func() int { return 80 },
		FuncOnWidthChanged: func(func()) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	lineDone := make(chan *readline.Result, 1)
	go func() { lineDone <- editor.Line() }()

	if err := editor.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-lineDone:
		if !result.CanBreak() {
			t.Fatalf("Line result = %+v, want a break after Close", result)
		}
	case <-time.After(time.Second):
		t.Fatal("Editor.Close did not unblock Line")
	}
}

func TestEditorCloseDoesNotConsumeBufferedNextLine(t *testing.T) {
	input, inputWriter := io.Pipe()
	t.Cleanup(func() { _ = inputWriter.Close() })
	editor, err := NewEditor(&readline.Config{
		Stdin:              input,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
		FuncIsTerminal:     func() bool { return false },
		FuncMakeRaw:        func() error { return nil },
		FuncExitRaw:        func() error { return nil },
		FuncGetWidth:       func() int { return 80 },
		FuncOnWidthChanged: func(func()) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	loopDone := make(chan struct{})
	go func() {
		Loop(context.Background(), editor, func() { _ = editor.Close() }, make(chan struct{}), func(args []string) bool {
			return len(args) == 1 && args[0] == "quit"
		})
		close(loopDone)
	}()
	if _, err := inputWriter.Write([]byte("quit\n\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("loop did not process quit")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- editor.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Editor.Close consumed buffered input and hung")
	}
}

func TestFrameIndexValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    int
		wantErr bool
	}{
		{name: "default", want: 0},
		{name: "explicit", args: []string{"3"}, want: 3},
		{name: "malformed", args: []string{"abc"}, wantErr: true},
		{name: "negative", args: []string{"-1"}, wantErr: true},
		{name: "reference overflow", args: []string{strconv.Itoa(math.MaxInt)}, wantErr: true},
		{name: "extra", args: []string{"1", "2"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FrameIndex(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("FrameIndex(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("FrameIndex(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}
