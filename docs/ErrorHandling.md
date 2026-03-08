# Error Handling Conventions

This document describes the error handling conventions to follow across the Bingo codebase.

## 1. Return errors; don't panic

Functions should return errors to their callers rather than panicking. `panic` is reserved for situations that represent a programming bug (e.g. a nil pointer dereference that should never happen by design) — not for operational or system-call failures.

```go
// Bad
func (d *debugger) Continue(pid int) {
    if err := unix.PtraceCont(pid, 0); err != nil {
        panic(err)
    }
}

// Good
func (d *debugger) Continue(pid int) error {
    if err := unix.PtraceCont(pid, 0); err != nil {
        return fmt.Errorf("resuming pid %d: %w", pid, err)
    }
    return nil
}
```

## 2. Wrap errors with context using `%w`

When returning an error from a call that may fail, always wrap it with a short description of what the code was trying to do. Use `fmt.Errorf` with the `%w` verb so callers can inspect the chain with `errors.Is` / `errors.As`.

```go
// Bad — no context, hard to trace
return err

// Good — contextual message, inspectable chain
return fmt.Errorf("setting breakpoint at line %d: %w", line, err)
```

Keep the message lowercase and concise. Avoid redundant prefixes like `"failed to"` — the fact that an error is being returned already implies failure.

## 3. Log once at the top level

Low-level functions should wrap and return errors. The top-level caller (e.g. the hub goroutine, an HTTP handler, `main`) is responsible for logging and deciding what to do. Logging at every level produces duplicate noise in the output.

```go
// Bad — logs and panics at every layer
if err := d.SetBreakpoint(pid, line); err != nil {
    log.Printf("failed: %v", err)
    panic(err)
}

// Good — low-level function just returns; caller logs once
if err := d.StartWithDebug(path); err != nil {
    log.Printf("[Hub] Debug session for %q failed: %v", path, err)
    // signal shutdown, clean up, etc.
}
```

## 4. `log.Fatalf` for fatal startup failures

In `main`, use `log.Fatalf` rather than `log.Printf` + `panic` for unrecoverable startup errors. It prints the message and exits cleanly with status 1, without producing a stack trace.

```go
// Bad
log.Printf("server error: %v", err)
panic(err)

// Good
log.Fatalf("server error: %v", err)
```

## 5. Don't treat expected conditions as errors

Some failures are normal operating conditions. For example, a `PtraceDetach` call failing because the process already exited is not an error worth propagating. Log it at the point of detection, then continue or clean up gracefully.

```go
if err := unix.PtraceDetach(pid); err != nil {
    // Process may have already exited — not fatal
    log.Printf("[Debugger] Detach failed (process may have exited): %v", err)
}
```

## 6. Sentinel errors for conditions callers need to branch on

If a caller needs to distinguish a specific failure mode, define a package-level sentinel error rather than comparing strings.

```go
var ErrProcessExited = errors.New("target process has exited")

// Caller can then check:
if errors.Is(err, debugger.ErrProcessExited) { ... }
```

## 7. Cross-goroutine error propagation: `chan error`

When a goroutine needs to signal its outcome back to an owner (e.g. the debugger back to the hub), use a `chan error` where `nil` means a clean exit and a non-nil value carries the failure. This is consistent with how `context`, `errgroup`, and the standard library communicate goroutine results.

In this codebase, `endDebugSession chan error` serves this role:

```go
// Debugger: clean exit (target process finished normally)
select {
case d.EndDebugSession <- nil:
default:
}

// Debugger: failure (e.g. ptrace error in the debug loop)
// The hub goroutine that launched StartWithDebug sends the returned error:
go func() {
    if err := h.dbg.StartWithDebug(path); err != nil {
        select {
        case h.endDebugSession <- err:
        default:
        }
    }
}()

// Hub: receive and act on the result
case err := <-h.endDebugSession:
    if err != nil {
        log.Printf("[Hub] Debug session error: %v", err)
        h.broadcastError(err)
    }
    h.shutdown()
```

For cases where multiple goroutines need to be coordinated, [`golang.org/x/sync/errgroup`](https://pkg.go.dev/golang.org/x/sync/errgroup) is the idiomatic tool — it collects the first non-nil error from a group and cancels the others via a `context.Context`.

## 8. Propagating errors to WebSocket clients: typed error events

Since the client boundary is a WebSocket, errors should travel as typed JSON events using the same protocol as other events (`breakpointHit`, `stateUpdate`, etc.). This reuses the existing broadcast machinery and keeps internal Go error details server-side — clients only receive a plain human-readable message.

```go
// protocol.go
const EventError EventType = "error"

type ErrorEvent struct {
    Type      EventType `json:"type"`
    SessionID string    `json:"sessionId"`
    Message   string    `json:"message"`
}

// hub.go
func (h *Hub) broadcastError(err error) {
    event := ErrorEvent{
        Type:      EventError,
        SessionID: h.sessionID,
        Message:   err.Error(),
    }
    data, _ := json.Marshal(event)
    h.Broadcast(Message{Type: string(EventError), Data: data})
}
```

Do not send raw internal error chains or Go type information to the client. The `Message` field should be a short, user-facing string.

## Summary

| Situation | Convention |
|---|---|
| Operational / system-call failure | Return `error` wrapped with `fmt.Errorf("context: %w", err)` |
| Fatal startup failure in `main` | `log.Fatalf(...)` |
| Expected condition (e.g. process already exited) | Log and continue; don't propagate |
| Error the caller must branch on | Define a sentinel `var Err... = errors.New(...)` |
| Programmer bug (should never happen) | `panic` is acceptable |
| Logging | Log once at the call site that handles the error, not at every layer |
| Goroutine → owner error propagation | `chan error`: send `nil` for clean exit, non-nil for failure |
| Server → WebSocket client error | Broadcast a typed `ErrorEvent`; keep internal details server-side |
