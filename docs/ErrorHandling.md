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
if err := d.setBreakpoint(pid, line); err != nil {
    log.Printf("failed: %v", err)
    panic(err)
}

// Good — low-level function just wraps and returns; caller logs once
if err := d.setBreakpoint(pid, line); err != nil {
    return fmt.Errorf("setting initial breakpoint at line %d: %w", line, err)
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

## 7. Cross-goroutine error propagation: typed event channels

When an error occurs inside a goroutine, the idiomatic Go rule still applies — no panics, no global state. But the goroutine cannot `return` an error to its caller, so the error must travel on a channel.

In Bingo, **all debugger outcomes — normal events, clean exits, and failures — are delivered through the same `chan DebuggerEvent`**. This is preferable to a separate `chan error` because:

- There is a single channel to `select` on; no need to coordinate multiple channels.
- The hub's event loop handles every case uniformly with a type switch.
- `SessionEndedEvent` carries either a clean exit (`Err: nil`) or a failure (`Err: someErr`).

### How it works

`StartWithDebug` has **no return value**. It always sends a `SessionEndedEvent` before returning, so the hub always learns the outcome through the event channel regardless of whether the session ended cleanly or with an error.

```go
// debugger_linux_amd64.go

// notifyEnd is called on every exit path — errors and clean exits alike.
notifyEnd := func(err error) { d.sendEvent(SessionEndedEvent{Err: err}) }

// On failure:
if err := cmd.Start(); err != nil {
    notifyEnd(fmt.Errorf("starting target: %w", err))
    return
}

// On clean exit:
notifyEnd(nil)
```

```go
// hub.go

// StartWithDebug is launched in a goroutine. No error return to check.
go h.dbg.StartWithDebug(startDebugCmd.TargetPath)

// The hub's Run loop receives all outcomes uniformly:
case event := <-h.debuggerEvents:
    if done := h.handleDebuggerEvent(event); done {
        return
    }

// handleDebuggerEvent uses a type switch:
case debugger.SessionEndedEvent:
    if e.Err != nil {
        log.Printf("[Hub] Debug session %s ended with error: %v", h.sessionID, e.Err)
        h.broadcastError(e.Err)
    }
    h.shutdown()
    return true
```

### What goes in the interface vs. what stays internal

Only methods the hub actually calls belong on the `Debugger` interface. Internal helpers (`continueExec`, `singleStep`, `setBreakpoint`, etc.) stay as unexported methods on the concrete type, returning `error` normally through the synchronous call chain. Those errors eventually reach `mainDebugLoop` → `StartWithDebug` → `notifyEnd`, which puts them on the channel.

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
        Message:   "an unexpected error occurred during debugging. Check server logs for details.", // Don't leak internal details to clients
    }
    data, _ := json.Marshal(event)
    h.Broadcast(Message{Type: string(EventError), Data: data})
}
```

Do not send raw internal error chains or Go type information to the client. The `Message` field should be a short, user-facing string.

## Summary

| Situation                                        | Convention                                                                                                  |
| ------------------------------------------------ | ----------------------------------------------------------------------------------------------------------- |
| Operational / system-call failure                | Return `error` wrapped with `fmt.Errorf("context: %w", err)`                                                |
| Fatal startup failure in `main`                  | `log.Fatalf(...)`                                                                                           |
| Expected condition (e.g. process already exited) | Log and continue; don't propagate                                                                           |
| Error the caller must branch on                  | Define a sentinel `var Err... = errors.New(...)`                                                            |
| Programmer bug (should never happen)             | `panic` is acceptable                                                                                       |
| Logging                                          | Log once at the call site that handles the error, not at every layer                                        |
| Goroutine → owner error propagation              | Send a `SessionEndedEvent{Err: err}` on `chan DebuggerEvent`; no `return error` from the goroutine function |
| Methods not called by the hub                    | Keep unexported on the concrete type; return errors through the call chain normally                         |
| Server → WebSocket client error                  | Broadcast a typed `ErrorEvent`; keep internal details server-side                                           |
