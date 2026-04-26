# AGENTS.md

Navigation guide for AI agents working on bingo. Human-readable but written
for agents — terse, link-heavy, biased toward "what you must know to not break
things." Keep this file up to date when touching architecture; it's the index
that replaces inline narrative comments.

## What bingo is

A standalone visual concurrency debugger for Go. Server (`cmd/bingo`) launches
or attaches to a target Go binary, drives it via OS-level ptrace/Mach calls,
and broadcasts events to one or more WebSocket clients. The reference CLI
client lives in `cmd/cli`.

Built and tested only on:

- `darwin/arm64` (Apple Silicon) — requires `-tags bingonative` and the
  `com.apple.security.cs.debugger` entitlement (or SIP off)
- `linux/amd64`

Other GOOS/GOARCH combos fail with `undefined: newBackend`.

## Layout

| Path | What lives here |
| --- | --- |
| [cmd/bingo](cmd/bingo/) | Server entry point — flag parsing, signal handler, calls into `internal/server`. |
| [cmd/cli](cmd/cli/) | Interactive readline client. |
| [cmd/target](cmd/target/) | Trivial target program for manual testing. |
| [cmd/githook](cmd/githook/) | Conventional-commits commitlint, wired via [lefthook.yml](lefthook.yml). |
| [pkg/protocol](pkg/protocol/) | Wire types: `Event`, `Command`, payload structs, `EventKind`, `CommandKind`, `SessionState`. Single source of truth. |
| [pkg/client](pkg/client/) | Reference Go client. WebSocket-backed. Public surface: `Client` interface + `Create` / `Join` / `ListSessions`. |
| [internal/server](internal/server/) | HTTP/WebSocket entry. `Server`, `sessionStore`, `/api/sessions` and `/ws` handlers. |
| [internal/hub](internal/hub/) | Per-session bridge between connected clients and one `Debugger`. |
| [internal/debugger](internal/debugger/) | The actual debugger. Engine + per-platform Backend. |
| [test/integration](test/integration/) | Ginkgo integration suite (currently a skeleton). |

## Architecture in one diagram

```
client(s)  ─── WebSocket ───>  internal/server ─── per-session ───>  internal/hub
                                  (sessionStore)                        │
                                                                  ┌─────┴──────┐
                                                                  │  Hub.Run   │
                                                                  │  loop      │
                                                                  └─────┬──────┘
                                                                        │ commands
                                                                        ▼
                                                                  internal/debugger
                                                                  (engine + Backend)
                                                                        │
                                                                        ▼ ptrace / Mach
                                                                   tracee process
```

Events flow upward; commands flow downward. The hub re-stamps every event with
its own monotonic seq before broadcast.

## Wire protocol — quick reference

Source of truth: [pkg/protocol/protocol.go](pkg/protocol/protocol.go),
[pkg/protocol/payload.go](pkg/protocol/payload.go).

Two envelope types: `Event` (server → client) and `Command` (client → server).
Both versioned; both carry `Kind` + raw-JSON `Payload`. Decode with
`DecodeEventPayload` / `DecodeCommandPayload` after switching on `Kind`.

### Suspend/resume protocol

The hub blocks after broadcasting any of these "suspending" events until a
"resuming" command arrives (or the 30-min safety timeout fires):

- Suspending events: `BreakpointHit`, `Panic`, `Stepped`
- Resuming commands: `Continue`, `StepOver`, `StepInto`, `StepOut`, `Kill`

While suspended, **non-resuming** commands (`SetBreakpoint`, `Locals`, …) are
still executed immediately — the process is paused, so it's safe.

When multiple clients race resume commands: **first writer wins**, the rest
are dropped (`resumeCh` has capacity 1; see [hub.go injectCommand](internal/hub/hub.go)).

### Session state machine

`SessionState` ∈ {`idle`, `running`, `suspended`, `exited`}.

```
            Launch / Attach            BreakpointHit / Panic / Stepped
   idle ────────────────────> running <────────────────────────────────── suspended
    ▲                            │                Continue / Step*
    │                            │
    │                            ▼
    └──────────────────────── exited
       process exit + handleDebuggerClosed
```

Managed sessions (created via the server) broadcast `EventSessionState` on
every transition and to newly connected clients (welcome message). Raw hubs
created via `hub.New(dbg, log)` (tests / single-session) do not.

### Synchronous vs fire-and-forget commands (client SDK)

In [pkg/client](pkg/client/), the `Client` interface splits methods by what
they wait for:

- **Synchronous** (`SetBreakpoint`, `ClearBreakpoint`, `Locals`, `StackFrames`,
  `Goroutines`): block until the matching confirmation event (or `EventError`
  for the same command kind) arrives. Implemented via `sendAndWait` in
  [pkg/client/ws.go](pkg/client/ws.go).
- **Fire-and-forget** (`Launch`, `Attach`, `Kill`, `Continue`, `Step*`):
  return as soon as the command is on the wire. Results arrive asynchronously
  on the `Events()` channel.

## Engine concurrency model — non-obvious invariants

Source: [internal/debugger/engine.go](internal/debugger/engine.go).

This is the most fragile code in the repo. Read this section before changing
anything in [internal/debugger/](internal/debugger/).

1. **All ptrace/Mach calls run on a single OS thread.** The engine event loop
   (`engine.loop`) calls `runtime.LockOSThread()` and never unlocks. ptrace
   on Linux is thread-specific — calling it from a different OS thread fails.
   Public `Debugger` methods (`Continue`, `SetBreakpoint`, …) submit a closure
   to `cmdCh`; the loop executes it. They don't make ptrace calls themselves.

2. **`waitLoop` is a one-shot, locked goroutine.** Every time the process is
   resumed, a fresh `waitLoop` goroutine is started. It calls `Backend.Wait()`
   exactly once (also `LockOSThread`'d) and sends the result to `stopCh`.
   Selects on `e.done` so a stale waitLoop exits cleanly when the engine has
   already shut down.

3. **Shutdown sequence.** When `StopExited` / `StopKilled` / `ErrProcessExited`
   arrives, the loop sets `stateExited`, calls `drainCmds` (answers queued
   commands with `ErrProcessExited` so blocked dispatchers unblock), then
   returns. The `defer` closes `done` (signals waitLoop to abandon pending
   sends) and then `events` (signals hub no more events coming).

4. **`Kill` is idempotent and races-safe.** It checks `done` first (fast
   path), then dispatches a closure that injects a synthetic `StopExited`
   into `stopCh`. The main loop sees that, exits cleanly. Multiple concurrent
   `Kill` callers share one teardown.

5. **`dispatch` is the only public-method pattern.** Send `engineCmd{fn,err}`
   on `cmdCh`, wait on `err`. If the loop has exited (`e.done` closed),
   return `ErrProcessExited` immediately so callers don't deadlock.

## Software-breakpoint step-over flow

When a thread stops at a software BP, the original instruction has been
overwritten with a trap (INT3 / BRK). To resume execution we must:

1. Restore the original bytes (`bps.removeFromTable` + `WriteMemory`).
2. Single-step that one instruction.
3. Reinstall the trap (`bps.reinstall`).
4. Then perform the user's intended action (`bpResumeAction`).

See `engine.resumeFromBreakpoint` and the `StopSingleStep` branch of
`engine.handleStop` in [internal/debugger/engine.go](internal/debugger/engine.go).

`bpResumeAction` values:

| Value | What it does |
| --- | --- |
| `bpResumeContinue` | Plain `ContinueProcess`. |
| `bpResumeStep` | Emit `EventStepped` (machine-instruction granularity). |
| `bpResumeSourceStep` | Set a temporary `<stepover-next>` BP at the next source line, then continue. |
| `bpResumeStepOut` | Set a temporary `<stepout-return>` BP at the saved return address, then continue. |

Internal sentinel BP files: `<stepover-next>`, `<stepout-return>`,
`<direct-addr>` (test helper). These get auto-cleared when hit and emit
`EventStepped`, not `EventBreakpointHit`.

If `bps.reinstall` ever fails after a single-step, **suspend instead of
resuming**. Running without the trap is a runaway process; reporting the
error lets the operator intervene.

## Architecture-specific traps

Per-arch in [trap_amd64.go](internal/debugger/trap_amd64.go) and
[trap_arm64.go](internal/debugger/trap_arm64.go):

| Arch | Instruction | PC after trap | `archRewindPC` |
| --- | --- | --- | --- |
| amd64 | `INT3` (1 byte, 0xCC) | RIP = bp+1 (advanced past INT3) | subtract 1 |
| arm64 | `BRK #0` (4 bytes) | PC = bp (at the BRK) | identity |

Be careful: spurious SIGTRAPs (Go runtime internal traps, libc assertions)
arrive as `StopBreakpoint` with no entry in our table. On ARM64, calling
`ContinueProcess` with PC unchanged re-executes the BRK — infinite loop. The
engine advances PC by `len(archTrapInstruction())` and resumes. See the
`bp == nil` branch in `handleStop`.

## Backend quirks

### Darwin / arm64 ([backend_darwin_arm64.go](internal/debugger/backend_darwin_arm64.go))

- Uses ptrace for control flow (`PT_CONTINUE`, `PT_STEP`, `PT_ATTACH`) and
  Mach (`thread_get_state`, `mach_vm_read/write`, `task_threads`) for state.
  `PT_ATTACHEXC` is NOT used — it routes signals through Mach exceptions,
  incompatible with our `wait4`-based loop.
- `task_for_pid` requires the `com.apple.security.cs.debugger` entitlement
  (the build embeds [entitlements.plist](entitlements.plist) via codesign).
- `task_threads` returns threads in **creation order**. `threads[0]` is
  often an idle Go runtime M parked in `pthread_cond_wait`, **not** the
  goroutine running user code. Use `findBreakpointThread` to locate the
  thread sitting at a BRK; for SingleStep, save the thread port we issued
  the step against (`b.stepTID`).
- `PT_STEP` is per-PROCESS on Darwin (despite the API taking a tid). Always
  pass `b.pid`, not the Mach thread port.
- `SIGURG` (Go preemption) and `SIGWINCH` must be re-delivered transparently
  during both step and continue, or scheduling breaks.
- ASLR slide is computed in `TextSlide` by scanning the VM map for the first
  exec region with the 64-bit Mach-O magic. Do NOT use `TASK_DYLD_INFO` — its
  image array is unpopulated at the very first ptrace stop.

### Linux / amd64 ([backend_linux_amd64.go](internal/debugger/backend_linux_amd64.go))

- Pure ptrace. `startTracedProcess` enables `PTRACE_O_TRACECLONE | TRACEEXIT
  | TRACEEXEC` so child threads are tracked and we get a stop just before
  `exit_group`.
- `Wait` uses `Wait4(-1, …, WALL)` to receive events for any thread.
  `PTRACE_EVENT_*` stops are absorbed (resumed and looped) and never surface
  to the engine.
- `g` pointer for goroutine inspection lives at `FS_BASE` on amd64.
- `SIGURG` re-delivery is mandatory here too.

## DWARF reader notes

[internal/debugger/dwarf.go](internal/debugger/dwarf.go).

- File matching is suffix-based (`fileMatches`) so users can supply short
  names like `main.go` against absolute paths embedded in DWARF.
- Slide is added when returning runtime addresses, subtracted when looking up
  by PC. Always go through `r.slide`; never raw-compare runtime PCs against
  DWARF addresses.
- `NextLinePC` powers source-level step-over. It returns the lowest is-stmt
  address with line > afterLine. After a step-over completes we **prefer the
  remembered destination** over re-querying `locationForPC` from the new PC,
  because the new PC can land on a DWARF entry with line==0.
- `LocalsForFrame` only handles `DW_OP_addr` (0x03) and `DW_OP_fbreg` (0x91).
  Register-allocated variables come back as `<optimized out>`. Values are
  read as 8 bytes and returned hex; type-aware formatting is a TODO.

## Hub seq stream — why one counter

The hub re-stamps every outbound event with its own atomic `seq` counter. The
engine has its own seq, and the hub also synthesises events (errors,
confirmations like `BreakpointSet`). If clients saw both streams interleaved,
they'd see two overlapping monotonic sequences and couldn't detect drops.
**Always go through `h.seq.Add(1)` before broadcasting.**

## Test layering

- `pkg/protocol`: pure wire round-trip tests, no fakes needed.
- `internal/debugger`: `fakeBackend` in [engine_test.go](internal/debugger/engine_test.go)
  replaces the OS. Tests seed mem/regs, push `StopEvent`s onto `stopCh`, and
  inspect recorded calls. `export_test.go` exposes a few internals
  (`ExportedForceSuspended`, `ExportedSetBreakpointAt`, …) so tests can
  bypass DWARF and the OS process model. Engine tests are tagged-agnostic —
  they avoid native code paths.
- `internal/hub`: `fakeDebugger` + `fakeWSConn` in [hub_test.go](internal/hub/hub_test.go).
  The fake conn uses a 256-deep `incoming` buffer so `WriteMessage` never
  blocks the hub event loop.
- `internal/server`: `httptest.Server` + real gorilla websocket client.
- `test/integration`: skeleton (placeholder Ginkgo suite).

Build/test commands:

```sh
just build [linux amd64 | darwin arm64]   # produces ./build/bingo/...
just test [PKG]                            # go test -v
just coverage [PKG]                        # writes test/coverage.out
just integration                           # ginkgo -r ./test/integration
```

On macOS, `go test ./...` without `-tags bingonative` will fail with
`undefined: newBackend`. Always use `go test -tags bingonative ./...` or run
through the justfile.

## Things that look weird but are intentional

- `process.kill` takes a `Backend` argument it doesn't use. Kept for symmetry
  with the engine's Kill path which also calls `bps.clearAll`.
- `CmdNone` is the empty string and gets `omitempty`'d off the wire. The
  protocol test pins this behaviour.
- Hub `New(dbg, log)` (without a session ID) is for tests / single-session
  setups: the debugger is pre-attached, no state events are broadcast,
  managed-session machinery is bypassed. Real sessions go through
  `NewSession`.
- The `// arbitrary instruction byte` style of one-line code-explainer
  comments has been removed throughout. If you're tempted to add one, make
  sure it's explaining a non-obvious WHY, not restating WHAT the code does.

## When you change something

- **Wire protocol** (`pkg/protocol`): bump `Version` for breaking changes,
  and update the round-trip table in `protocol_test.go`.
- **Suspend/resume sets**: update both `suspendingEvents` and
  `resumingCommands` in [hub.go](internal/hub/hub.go), and the matching
  hub_test cases.
- **New OS or arch**: add a new `backend_<goos>_<goarch>.go` and a matching
  `trap_<goarch>.go` if the trap differs. Update [README.md](README.md) and
  the build matrix in [.github/workflows/](.github/workflows/) and
  [justfile](justfile).
- **AGENTS.md drift**: if you introduce a new invariant or change one of the
  ones documented above, update this file in the same commit.
