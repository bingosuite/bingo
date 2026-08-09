package repl

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/chzyer/readline"
)

// Editor makes readline cancellation safe for bingo's long-lived event loops.
type Editor struct {
	*readline.Instance

	input     *readyInput
	closeOnce sync.Once
	closeErr  error
}

type readyInput struct {
	io.ReadCloser
	ready chan struct{}
	once  sync.Once
}

func (i *readyInput) Read(p []byte) (int, error) {
	i.once.Do(func() { close(i.ready) })
	return i.ReadCloser.Read(p)
}

// NewEditor retains readline's inner cancelable input so Close can unblock it.
func NewEditor(config *readline.Config) (*Editor, error) {
	source := config.Stdin
	if source == nil {
		source = readline.Stdin
	}
	input := &readyInput{
		ReadCloser: readline.NewCancelableStdin(source),
		ready:      make(chan struct{}),
	}
	config.Stdin = input

	instance, err := readline.NewEx(config)
	if err != nil {
		_ = input.Close()
		return nil, err
	}
	return &Editor{Instance: instance, input: input}, nil
}

func (e *Editor) Close() error {
	e.closeOnce.Do(func() {
		_ = e.input.Close()
		select {
		case <-e.input.ready:
		default:
			// readline adds its terminal WaitGroup entry inside the goroutine.
			// A canceled read is the only monotonic proof that Add has run.
			e.Terminal.KickRead()
			<-e.input.ready
		}
		e.closeErr = e.Instance.Close()
	})
	return e.closeErr
}

// Reader is the readline surface shared by bingo's interactive clients.
type Reader interface {
	Line() *readline.Result
	Stdout() io.Writer
}

// Loop reads and dispatches commands until the user exits, the context is
// canceled, or the remote endpoint disconnects.
func Loop(
	ctx context.Context,
	reader Reader,
	closeInput func(),
	disconnected <-chan struct{},
	dispatch func([]string) bool,
) {
	stopClose := context.AfterFunc(ctx, closeInput)
	defer stopClose()

	for {
		result := reader.Line()
		if ctx.Err() != nil || channelClosed(disconnected) {
			return
		}
		if result.CanContinue() {
			continue
		}
		if result.CanBreak() {
			if ctx.Err() == nil && !channelClosed(disconnected) {
				_, _ = fmt.Fprintln(reader.Stdout(), "bye")
			}
			return
		}

		line := strings.TrimSpace(result.Line)
		if line == "" {
			continue
		}
		if dispatch(strings.Fields(line)) {
			_, _ = fmt.Fprintln(reader.Stdout(), "bye")
			return
		}
	}
}

// FrameIndex parses the optional argument accepted by `locals [frame]`.
func FrameIndex(args []string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	if len(args) > 1 {
		return 0, fmt.Errorf("usage: locals [frame]")
	}

	frame, err := strconv.Atoi(args[0])
	if err != nil || frame < 0 || frame == math.MaxInt {
		return 0, fmt.Errorf("invalid frame index: %s", args[0])
	}
	return frame, nil
}

// PrintAsync writes one refresh-safe line through readline's output writer.
func PrintAsync(out io.Writer, message string) {
	_, _ = fmt.Fprintf(out, "  %s\n", message)
}

// PrintOutput renders asynchronous debuggee output consistently across clients.
func PrintOutput(out io.Writer, category, content string) {
	if category == "" {
		category = "output"
	}
	content = strings.TrimRight(content, "\r\n")
	if content == "" {
		_, _ = fmt.Fprintf(out, "  [%s]\n", category)
		return
	}
	_, _ = fmt.Fprintf(out, "  [%s] %s\n", category, content)
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
