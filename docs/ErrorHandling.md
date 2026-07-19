# Error Handling Conventions

How Bingo surfaces, wraps, logs, and propagates errors. Written for the current
architecture (see [AGENTS.md](../AGENTS.md)): a debugger `engine`, a per-session
`hub`, the `pkg/protocol` wire types, and `pkg/client`. Keep this in sync with
the code — it is the reference the codebase is expected to follow.

## TL;DR

- Return errors; don't panic (panic is only for programmer bugs).
- Wrap with `fmt.Errorf("doing X: %w", err)` so the chain stays inspectable.
- Log **once**, at the top of the call chain that decides what to do — not at
  every layer.
- Cross-goroutine errors travel on a channel, not via panics or globals. In
  Bingo every debugger outcome — stops, exits, and failures — rides the single
  `Debugger.Events()` channel as a typed `protocol.Event`.
- The server → client boundary carries errors as a typed `EventError`, reusing
  the normal broadcast path.

## 1. Return errors; don't panic

Functions return errors to their callers. `panic` is reserved for situations
that represent a programming bug (an invariant that should be impossible by
design) — never for operational or system-call failures.

```go
// Bad
func (e *engine) resume() {
    if err := e.backend.ContinueProcess(); err != nil {
        panic(err)
    }
}

// Good
func (e *engine) resume() error {
    if err := e.backend.ContinueProcess(); err != nil {
        return fmt.Errorf("resuming process: %w", err)
    }
    return nil
}
```

The only `panic` in non-test code is
[`protocol.MustEvent`](../pkg/protocol/encoding.go), a test-only helper whose
doc comment says so.

## 2. Wrap errors with context using `%w`

When returning an error from a call that may fail, wrap it with a short
description of what the code was trying to do, using `%w` so callers can use
`errors.Is` / `errors.As`.

```go
// Bad — no context, hard to trace
return err

// Good — contextual, inspectable chain
return fmt.Errorf("set breakpoint at %s:%d: %w", file, line, err)
```

Keep the message lowercase and concise. Avoid `"failed to"` prefixes — an error
being returned already implies failure.

## 3. Log once, at the top level

Low-level functions wrap and return. The owner of the call chain logs. In Bingo
the "top level" is one of:

- the **engine loop** ([`engine.loop`](../internal/debugger/engine.go)), which
  turns a failed stop into an `EventError`;
- the **hub** ([`Hub.executeCommand`](../internal/hub/hub.go)), which logs a
  failed command and broadcasts an `EventError`;
- an **HTTP handler** ([`internal/server`](../internal/server/handler.go));
- **`main`** ([`cmd/bingo`](../cmd/bingo/main.go)).

Logging at every layer produces duplicate noise. A helper that already returns
its error to one of those owners should not also log it.

Bingo logs with `log/slog` (structured), not `log.Printf`:

```go
// low-level: wrap + return, no logging
if _, err := e.dw.PCForFileLine(file, line); err != nil {
    return fmt.Errorf("resolve %s:%d: %w", file, line, err)
}

// top level (hub): log once, with structured fields
h.log.Warn("command failed", "kind", cmd.Kind, "err", err)
```

## 4. Fatal startup failures exit; they don't panic

In `main`, an unrecoverable startup error is logged and the process exits with
status 1 — no stack trace. With slog that is `log.Error(...)` followed by
`os.Exit(1)` (the `slog` equivalent of `log.Fatalf`):

```go
if err := srv.Start(); err != nil {
    log.Error("server error", "err", err)
    os.Exit(1)
}
```

## 5. Don't treat expected conditions as errors

Some failures are normal operating conditions and should not propagate. A
`PtraceDetach` failing because the tracee already exited, or a WebSocket read
returning a normal-close code, is expected. Handle it at the point of detection
and continue.

```go
// internal/hub/client.go — a normal close is not an error worth logging loudly
if err := c.conn.ReadMessage(); err != nil {
    if !isNormalClose(err) {
        c.log.Warn("read error", "err", err)
    }
    return
}
```

Deliberately-ignored errors on cleanup paths use `_ =` (e.g. best-effort
`SetWriteDeadline`, `Close` during shutdown) so the intent is explicit.

## 6. Sentinel errors for conditions callers branch on

If a caller must distinguish a specific failure mode, define a package-level
sentinel rather than string-matching. The debugger package exports several in
[`debugger.go`](../internal/debugger/debugger.go):

```go
var (
    ErrProcessExited  = errors.New("debugger: process exited")
    ErrNotSuspended   = errors.New("debugger: process is not suspended")
    ErrAlreadyRunning = errors.New("debugger: process already running")
    ErrNoProcess      = errors.New("debugger: no process")
)

// Caller branches with errors.Is:
if errors.Is(err, debugger.ErrProcessExited) { /* tear down */ }
```

The engine uses this internally: a `Wait` that returns `ErrProcessExited` is
turned into an `EventProcessExited`, while any other error becomes an
`EventError` (see [`engine.loop`](../internal/debugger/engine.go)). The
`dispatch` helper returns `ErrProcessExited` when the loop has already exited so
blocked callers unblock instead of deadlocking.

## 7. Cross-goroutine error propagation: one typed event channel

A goroutine cannot `return` an error to its caller, so the error must travel on
a channel. Bingo does **not** use a separate `chan error`. Instead, **all
debugger outcomes — stops, clean exits, and failures — are delivered through the
single `chan protocol.Event`** exposed by `Debugger.Events()`.

Why one channel rather than a side `chan error`:

- There is a single thing to `select` on; no multi-channel coordination.
- The hub handles every outcome uniformly by switching on `event.Kind`.
- A terminal outcome is just another event: `EventProcessExited` for a clean
  exit, `EventError` for a failure.

### How it works

The debugger runs a serialized event loop plus a one-shot `waitLoop` goroutine
(see AGENTS.md → engine concurrency model). `waitLoop` calls `Backend.Wait()`
once and sends the result — value **or error** — to `stopCh`. The loop is the
only place that decides what a failure means:

```go
// internal/debugger/engine.go
case result := <-e.stopCh:
    if result.err != nil {
        if errors.Is(result.err, ErrProcessExited) {
            e.emitProcessExited(0)      // normal termination
        } else {
            e.emitError(protocol.CmdNone, result.err) // failure → typed event
        }
        e.drainCmds()
        return
    }
    e.handleStop(result.evt)
```

Because the loop owns the transition, an error raised deep in the stop-handling
state machine (e.g. a breakpoint that can't be reinstalled) is wrapped, returned
up the synchronous call chain to the loop, and emitted as one `EventError` — it
is never logged three times on the way up.

The hub is the reader. It re-stamps each event with its own seq and reacts:

```go
// internal/hub/hub.go — Run loop
case evt, ok := <-h.eventsCh():
    if !ok {
        // channel closed = debugger gone; managed sessions go idle
        h.handleDebuggerClosed()
        continue
    }
    h.handleEvent(ctx, evt)
```

### Best-effort emit is deliberate

`engine.emit` does a non-blocking send and drops on a full buffer
(`eventBufSize`), rather than blocking. This is intentional: the emit runs on
the serialized loop, and blocking it while a reader is gone (e.g. a cancelled
session tearing down) would deadlock the loop against its own `Kill`. The buffer
is sized so the continuously-draining hub never fills it in practice, and the
terminal exit path is additionally backstopped by the events channel closing
when the loop returns. A marshal failure inside `emit` is logged, not silently
dropped.

### Synchronous vs. fire-and-forget at the client boundary

`pkg/client` splits methods by what they wait for. Synchronous methods
(`SetBreakpoint`, `Locals`, `StackFrames`, `Goroutines`, `ClearBreakpoint`)
block on their confirmation event **or** an `EventError` for the same command
kind — see [`sendAndWait` / `routeToPending`](../pkg/client/ws.go). An
`EventError` whose `Command` matches is turned back into a Go `error` for the
caller:

```go
case evt := <-ch:
    if evt.Kind == protocol.EventError {
        var ep protocol.ErrorPayload
        _ = protocol.DecodeEventPayload(evt, &ep)
        return protocol.Event{}, fmt.Errorf("server: %s", ep.Message)
    }
```

Fire-and-forget methods (`Launch`, `Attach`, `Kill`, `Continue`, `Step*`) return
once the command is on the wire; their results — including asynchronous
`EventError`s with `Command == CmdNone` — arrive on `Events()` and are printed
by the CLI's event loop.

## 8. Propagating errors to clients: typed `EventError`

Errors cross the WebSocket as a typed JSON event, reusing the same protocol and
broadcast machinery as every other event:

```go
// pkg/protocol — EventError + ErrorPayload
const EventError EventKind = "Error"

type ErrorPayload struct {
    Command CommandKind `json:"command,omitempty"` // which command failed (CmdNone for async)
    Message string      `json:"message"`
}

// internal/hub/hub.go
func (h *Hub) broadcastError(kind protocol.CommandKind, err error) {
    evt, e := protocol.NewEvent(protocol.EventError, h.seq.Add(1), protocol.ErrorPayload{
        Command: kind,
        Message: err.Error(),
    })
    if e != nil {
        h.log.Error("failed to marshal error event", "err", e)
        return
    }
    h.broadcast(evt)
}
```

### On error message content

Bingo is a **local developer tool**: the client is the operator debugging their
own process on their own machine. The actual error text (`err.Error()`) is
therefore surfaced to the client on purpose — a debugger that hides "no DWARF
info: was a binary path provided?" behind "an error occurred" is useless.

This is the opposite of a public-facing web service, where internal error
detail should be kept server-side and clients get a generic message. If Bingo
ever grows a remote/multi-tenant mode, revisit this: keep detail in the
server-side `slog` output and map to short client-facing messages there. The
`ErrorPayload.Message` split from server logs already gives you the seam.

## Summary

| Situation                                        | Convention                                                                                   |
| ------------------------------------------------ | -------------------------------------------------------------------------------------------- |
| Operational / system-call failure                | Return `error` wrapped with `fmt.Errorf("context: %w", err)`                                  |
| Fatal startup failure in `main`                  | `log.Error(...)` + `os.Exit(1)`                                                              |
| Expected condition (process gone, normal close)  | Handle at the detection point; don't propagate. Cleanup no-ops use `_ =`                      |
| Error the caller must branch on                  | Package-level sentinel `var Err... = errors.New(...)`, checked with `errors.Is`               |
| Programmer bug (impossible by design)            | `panic` is acceptable                                                                         |
| Logging                                          | Log once, at the owning top level (engine loop / hub / handler / `main`), via `slog`          |
| Goroutine → owner error propagation              | Emit a typed `protocol.Event` (`EventError` / `EventProcessExited`) on `Debugger.Events()`    |
| Methods not on the `Debugger` interface          | Keep unexported on `engine`; return errors up the synchronous chain to the loop               |
| Server → WebSocket client error                  | Broadcast a typed `EventError`; message text is intentionally surfaced (local tool)           |
