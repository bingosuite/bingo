# AGENTS.md

Navigation guide for AI agents working on bingo. Human-readable but written
for agents — terse, link-heavy, biased toward "what you must know to not break
things." Keep this file up to date when touching architecture; it's the index
that replaces inline narrative comments.

**This file is the single source of truth for agent guidance.** Tool-specific
entry points ([CLAUDE.md](CLAUDE.md),
[.github/copilot-instructions.md](.github/copilot-instructions.md)) are thin
pointers back here — put new guidance in this file, not in them, so nothing
drifts.

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

## Conventions for AI agents

Rules for making changes. These encode decisions already litigated in the repo;
follow them so reviews stay about substance, not style.

### Comments

- Explain **why**, not **what**. A comment must add context the code can't:
  an invariant, a non-obvious constraint, a hazard, a reference. Never restate
  what the next line literally does.
- Do **not** add decorative or narrating one-liners (the
  `// arbitrary instruction byte` / `// loop over items` style). They were
  deliberately purged from this codebase; don't reintroduce them.
- Prefer a short doc comment on the function/type over inline noise. If a block
  genuinely needs explaining, one paragraph above it beats five scattered tags.
- When you remove or move non-obvious logic, move its *why*-comment with it.

### Code style

- `gofmt` / `goimports` are mandatory — the lefthook pre-commit hook runs
  `goimports -w` on staged `*.go` files. Match the surrounding style otherwise.
- Make surgical, focused changes. Don't opportunistically reformat or refactor
  unrelated code in the same commit.
- Return errors; don't `panic` in server/hub/debugger control paths. Panics
  crash the whole server (see issues #29, #60). `panic` is acceptable only in
  clearly test-only or truly-unreachable-by-construction spots.

### Commits

- Conventional Commits are **enforced** by the commit-msg hook
  ([cmd/githook](cmd/githook/), wired via [lefthook.yml](lefthook.yml)).
  Format: `<type>(<scope>): <description>`.
- Allowed types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`,
  `chore`, `wip`. Non-conforming messages are rejected.

### Build, test, verify

- Always verify before declaring done: `go vet` + the relevant tests.
- On macOS the darwin backend needs `-tags bingonative`; plain
  `go test ./...` fails with `undefined: newBackend`. Use the justfile
  (`just build`, `just test`) or pass the tag explicitly. Full command list is
  in the build/test commands block near the end of this file.
- Only run linters/builds/tests that already exist; don't introduce new
  tooling for a change unless the task is specifically about that.

### Platform scope

- Supported platforms are **linux/amd64** and **darwin/arm64** only. Do not add
  backends, build tags, or CI matrix entries for other GOOS/GOARCH (see #61).

### Keep docs in sync

- If you change an architectural invariant documented here, update AGENTS.md in
  the **same commit** (see [When you change something](#when-you-change-something)).
- Keep the tool pointer files ([CLAUDE.md](CLAUDE.md),
  [.github/copilot-instructions.md](.github/copilot-instructions.md)) as thin
  redirects — never fork guidance into them.

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
| [test/integration](test/integration/) | Ginkgo suite. Placeholder specs + the platform-split debugger E2E acceptance tests (`e2e` build tag). |

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

- Suspending events: `BreakpointHit`, `Panic`, `Stepped`, `Paused`
- Resuming commands: `Continue`, `StepOver`, `StepInto`, `StepOut`

While suspended, **non-resuming** commands (`SetBreakpoint`, `Locals`, …) are
still executed immediately — the process is paused, so it's safe.

`Pause` and `Kill` are the odd ones out: both must act **while the process is
running**, not only while suspended, so neither is a resuming command. They ride
the ordinary `cmdCh` (like `SetBreakpoint`), which both Run's main loop and the
suspended wait loop drain. `Pause` is a suspending *request* whose suspend is
reported asynchronously via the `Paused` event once the interrupt surfaces (see
[Pause — async interrupt](#pause--async-interrupt)). `Kill` was previously
misrouted through `resumeCh`, which is only drained while suspended — so a `Kill`
against a runaway target (tight loop, no breakpoints) could never terminate it
through the protocol. Routing `Kill` via `cmdCh` fixes that; the suspended wait
loop `return`s after executing a `Kill` (like `Restart`), since the process it
was waiting to resume no longer exists. `Kill` with no active debugger is a
benign no-op success (nothing to terminate).

Stale resumes: a resuming command sent **while the process is still running**
(an erroneous or racing client) lands in `resumeCh` but is not drained by Run's
main loop. To stop it from satisfying a *future* suspend — auto-continuing past
a fresh `BreakpointHit`/`Stepped` before the client can inspect — the wait loop
**drains `resumeCh` on entry**, immediately after broadcasting the suspending
event. Any resume buffered at that instant is necessarily stale: a legitimate
resume can only follow the client observing the suspend, which hasn't been
delivered over the wire yet.

When multiple clients race resume commands: **first writer wins**, the rest
are dropped (`resumeCh` has capacity 1; see [hub.go injectCommand](internal/hub/hub.go)).

### Session state machine

`SessionState` ∈ {`idle`, `running`, `suspended`, `exited`}.

```
            Launch / Attach       BreakpointHit / Panic / Stepped / Paused
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
   (`engine.loop`) calls `runtime.LockOSThread()` and never unlocks. Public
   `Debugger` methods (`Continue`, `SetBreakpoint`, …) submit a closure to
   `cmdCh`; the loop executes it. They don't make ptrace calls themselves.
   ptrace is thread-bound on Linux, so the linux backend goes further: it owns
   a **dedicated tracer thread** (`tracerThread`) and funnels *every* ptrace
   control op through `execPtrace`, because they must issue from the exact
   thread that forked/attached the tracee. `wait4` is the one exception — legal
   from any thread of the tracer process, so `Wait` runs it directly off the
   tracer thread. On Darwin the ptrace/Mach calls run on the engine-loop thread
   itself (Mach ports are task-wide). Mirrors Delve's `execPtraceFunc`.

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

On a matched software-BP stop the engine calls `rewindToBreakpoint` to write
the rewound PC back into the tracee's **live** register (not just the local
`StopEvent`). On amd64 the CPU leaves RIP one byte past the `INT3`, so every
resume path — plain continue and the restore→single-step→reinstall step-over
dance — would otherwise start mid-instruction and corrupt the tracee (this
manifested as a hung `StepOver`). No-op on arm64/Darwin, whose rewind is
identity.

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
  goroutine running user code. Darwin `Wait` returns raw SIGTRAP stops without
  reading Mach thread/register state; the engine resolves the thread sitting at
  a BRK on its serialized event loop. For SingleStep, save the thread port we
  issued the step against (`b.stepTID`).
- `PT_STEP` is per-PROCESS on Darwin (despite the API taking a tid). Always
  pass `b.pid`, not the Mach thread port.
- `SIGURG` (Go preemption) and `SIGWINCH` must be re-delivered transparently
  during both step and continue, or scheduling breaks.
- ASLR slide is computed in `TextSlide` by scanning the VM map for the first
  exec region with the 64-bit Mach-O magic. Do NOT use `TASK_DYLD_INFO` — its
  image array is unpopulated at the very first ptrace stop.

### Linux / amd64 ([backend_linux_amd64.go](internal/debugger/backend_linux_amd64.go))

- Pure ptrace, funnelled through one dedicated tracer thread
  (`tracerThread` / `execPtrace`) because ptrace is thread-bound: the initial
  fork/exec, attach, and every control op (`CONT` / `SINGLESTEP` /
  `GET`·`SETREGS` / `PEEK`·`POKEDATA` / `SETOPTIONS`) must originate from the
  one thread that became the tracer. `wait4` runs off that thread (valid from
  any tracer thread). Splitting the wait from the control ops was the original
  step-over hang: cross-thread `PTRACE_SINGLESTEP` failed with `ESRCH`.
- `startTracedProcess` enables `PTRACE_O_TRACEEXIT | PTRACE_O_TRACEEXEC |
  PTRACE_O_TRACECLONE`. Clone tracing is set at the single-threaded execve stop
  so every later Go-runtime worker thread inherits it; without it a goroutine
  migrated (e.g. by `time.Sleep`) onto an untraced clone thread would deliver
  its breakpoint `SIGTRAP` to the Go runtime ("fatal: trace trap") instead of
  the tracer. Each new thread's initial `SIGSTOP` is resumed **individually** —
  never a group-continue, which would let a thread parked at a breakpoint run
  away (the "parking the thread group" hazard).
- `Wait` uses `Wait4(-1, …, WALL)` to receive events for any thread.
  `PTRACE_EVENT_*` stops are absorbed (resumed and looped) and never surface
  to the engine.
- ptrace stops are per-thread. The backend records the last stopped TID and
  targets `ContinueProcess` / memory reads / memory writes at that TID, not
  blindly at the process PID. Non-main thread exits are absorbed inside `Wait`.
- Single-step vs breakpoint disambiguation uses **both** `stepping` and
  `stepTID` (the exact TID `SingleStep` was issued against). Only a `cause==0`
  SIGTRAP on `stepTID` is the step's completion; the same stop on any other
  thread is that thread hitting an INT3 and is reported as a breakpoint. This
  matters because `Wait4(-1, …)` can return a sibling thread's concurrent
  breakpoint (or SIGURG) while a step is in flight — keying off `stepping`
  alone would misclassify it and corrupt the engine's step-over state machine.
- `g` pointer for goroutine inspection lives at `FS_BASE` on amd64.
- `SIGURG` re-delivery is mandatory here too — but only the `stepTID` thread is
  re-single-stepped on SIGURG; a SIGURG on any other thread is re-delivered and
  that thread continued.

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

## Logging — one injected logger per component

`server`, `hub`, `client`, and the debugger `engine` each hold a `*slog.Logger`
field (`s.log`, `h.log`, `c.log`, `e.log`), threaded down from
[cmd/bingo/main.go](cmd/bingo/main.go) through `server.New` → `sessionStore` →
`hub.NewSession` → `debugger.New`/`NewWithBackend`. `sessionStore.create` scopes
it with `.With("session", id)` before handing it to both the hub and the
debugger, so every log line for a session — regardless of which layer emitted
it — is correlated by that field. **Never call the package-level `slog.Debug`
/ `slog.Info` / `slog.Warn` / `slog.Error` from inside these components** —
that bypasses the configured level/handler and the session correlation,
producing duplicate-looking, uncorrelated log lines (this was the root cause
of issue #32). Constructors accept a nil logger and fall back to
`slog.Default()` (tests rely on this).

## Hub seq stream — why one counter

The hub re-stamps every outbound event with its own atomic `seq` counter. The
engine has its own seq, and the hub also synthesises events (errors,
confirmations like `BreakpointSet`). If clients saw both streams interleaved,
they'd see two overlapping monotonic sequences and couldn't detect drops.
**Always go through `h.seq.Add(1)` before broadcasting.**

## Restart — hub-level, not engine-level

`CmdRestart` (`internal/hub/hub.go` → `handleRestart`) kills the current
process and relaunches it, reinstalling previously-set breakpoints. It is
implemented entirely in the hub, **not** as a new `Debugger`/engine method,
because of the engine's one-way shutdown invariant (see
[Engine concurrency model](#engine-concurrency-model--non-obvious-invariants)):
once `stateExited` is reached, `loop()` permanently closes `done` and
`events`. Reviving a dead engine in place would need an epoch/generation
counter on `stopResult` to stop a stale `waitLoop` result from the killed
process being misread as belonging to the new one — too risky in the most
fragile package in the repo. Instead, Restart calls `Kill()` on the old
`Debugger`, discards it, and creates a fresh one via the hub's existing
`newDebugger` factory (the same one `Launch`/`Attach` already use for managed
sessions), then relaunches and re-sets breakpoints on the new instance. This
mirrors Delve's `Debugger.Restart` (`service/debugger/debugger.go`): kill/
detach, relaunch, reinstall logical breakpoints, collect `DiscardedBreakpoint`
for ones that fail to resolve (bingo: `protocol.DiscardedBreakpoint`).

Bookkeeping needed across the kill+relaunch, since the old engine's state is
gone once killed:

- `h.lastLaunch *protocol.LaunchPayload` — the program/args/env from the most
  recent successful `Launch`. Restart refuses to run if this is nil (no prior
  Launch, or the session was started via `Attach` — mirrors Delve's
  `canRestart`: there's no "same binary" to relaunch for an attached process).
  Set on `CmdLaunch` success, cleared on `CmdAttach` success.
- `h.restartBreakpoints map[int]protocol.Location` — id → location for every
  breakpoint currently believed installed. Updated on `CmdSetBreakpoint` /
  `CmdClearBreakpoint` success, reset on `CmdLaunch`/`CmdAttach`. Restart
  reinstalls these (sorted by id for determinism) via `SetBreakpoint` on the
  new `Debugger`, which re-resolves each `file:line` through DWARF against the
  new process image — addresses aren't reused directly since a relaunch can
  shift the load address.

**Routing quirk**: `CmdRestart` intentionally does **not** go through
`resumeCh` like `CmdContinue`/`CmdStep*`. `resumeCh` is only ever drained
inside `handleEvent`'s suspend-wait loop — Run's outer `select` never reads
it — so a resuming command sent while the hub *isn't* currently suspended
(the common case: restarting a running or idle session) would sit unread in
the buffered channel indefinitely. `CmdKill` used to share this hazard (it was
a resuming command); it is now routed via `cmdCh` for exactly this reason — see
[Suspend/resume protocol](#suspendresume-protocol). `CmdRestart` is likewise
routed through the ordinary `cmdCh` (like `SetBreakpoint`), which both Run's
outer loop and the suspend-wait loop's `case cc := <-h.cmdCh:` branch drain.
The one special case: that branch normally loops back to keep waiting for a
resume after executing a non-resuming command, but for `CmdRestart` **and**
`CmdKill` it `return`s instead — the suspended process it was waiting on no
longer exists (replaced or terminated), so there's nothing left to resume, and
returning lets Run's outer loop naturally pick up the new/closed debugger's
events channel (`h.dbg` is reassigned inside `handleRestart` before the
confirmation event is broadcast).

`EventRestarted` is a confirmation event (like `BreakpointSet`), not a
suspending one — the new process's suspended state (if any, e.g. break-on-
entry) is reported the normal way via `EventStepped`/`EventBreakpointHit` once
the relaunched process actually reaches that point.

## Pause — async interrupt

`Pause` forcibly halts a **running** tracee and suspends it, like an on-demand
breakpoint. It's the one suspend that is *asynchronous*: breakpoints and steps
are self-stops (the tracee runs into a trap it was set up to hit), whereas Pause
interrupts from the outside at an arbitrary instruction.

Flow (detection is platform-agnostic, in the shared engine — the only
per-platform piece is *which* signal `StopProcess()` sends, abstracted behind
`Backend.PauseSignal()`):

1. Client sends `CmdPause` while running. The hub routes it via `cmdCh`
   (**not** `resumeCh` — Pause is not a resuming command; see
   [Suspend/resume protocol](#suspendresume-protocol)) to `engine.Pause()`.
2. `engine.Pause()` (in [engine.go](internal/debugger/engine.go)) `dispatch`es a
   closure that, if state != `stateRunning`, returns `ErrNotRunning`; otherwise
   sets `manualStopPending = true` and calls `backend.StopProcess()`, then
   returns immediately. It does **not** change state — the suspend happens when
   the stop surfaces. Fire-and-forget from the client's view; `EventPaused`
   arrives asynchronously.
3. `StopProcess()` sends the backend's interrupt signal (`PauseSignal()`:
   `SIGSTOP` on linux, `SIGUSR2` on darwin). A deliberate signal surfaces from
   `Backend.Wait()` as `StopEvent{Reason: StopSignal, Signal: PauseSignal()}`
   (a ptrace signal-delivery-stop) on **both** backends, so detection lives
   entirely in the engine's `handleStop` `StopSignal` branch.
4. `handleStop` `StopSignal` branch: if `manualStopPending` (and
   `stop.Signal == e.backend.PauseSignal()`), it clears the flag, defensively
   reinstalls any in-flight step-over BP (mirrors the existing StopSignal
   reinstall), `populateStopPC`s, `setState(stateSuspended)`, and
   `emitPaused(stop)` — returning **without** continuing. Genuine other signals
   keep the original emit-output-then-auto-resume behavior.

**Loop-thread-only flag, no sync.** `manualStopPending` is a plain `bool` with
no mutex because both writers/readers — `Pause()`'s dispatched closure and
`handleStop` — run on the single engine loop thread (see
[Engine concurrency model](#engine-concurrency-model--non-obvious-invariants)).
Don't add locking; don't touch it from another goroutine.

**Pending-interrupt race.** If a real breakpoint/step stop wins the race after
`Pause()` set the flag but before the interrupt signal is dequeued, the process
suspends for *that* self-stop and the signal stays queued in the tracee. To stop
it being misread as a bogus Pause on the next resume, `manualStopPending` is
cleared on **every** self-stop suspend (`emitBreakpointHit` / `emitStepped`).
Then when the leftover signal later surfaces with the flag clear, the
`StopSignal` branch silently suppresses it (continue, no `EventPaused`, no
spurious signal output). A focused engine unit test pins this ordering.

**Linux: SIGSTOP is directed at the main thread.** `StopProcess()` on
[backend_linux_amd64.go](internal/debugger/backend_linux_amd64.go) uses
`tgkill(pid, pid, SIGSTOP)` rather than a process-directed `kill`. A
process-directed SIGSTOP can be dequeued by any thread, and linux `Wait()`
deliberately **swallows** a non-main-thread SIGSTOP as a clone group-stop
(`sig == SIGSTOP && tid != b.pid` → continue), so a multithreaded Pause could
be lost. Targeting the main thread (`tid == pid`) makes it fall through to the
`StopSignal` return every time. `PauseSignal()` returns `SIGSTOP`.

**Darwin: a *catchable* signal (SIGUSR2), not SIGSTOP.** darwin `StopProcess()`
([backend_darwin_arm64.go](internal/debugger/backend_darwin_arm64.go)) sends
`kill(pid, SIGUSR2)` and `PauseSignal()` returns `SIGUSR2`. SIGSTOP does **not**
work on darwin: XNU delivers a SIGSTOP to a ptraced tracee as a *job-control*
stop that Go's `syscall.WaitStatus` classifies as neither `Stopped()` nor
`Exited()` (`StopSignal() == -1`), so `Wait()`'s `if !ws.Stopped() { continue }`
skips it and re-blocks in `wait4` forever — verified empirically on real Apple
Silicon. A **catchable** signal instead produces an ordinary ptrace
signal-delivery stop that `wait4` *does* report, so it falls through darwin
`Wait()` to `StopEvent{StopSignal, sig}` (SIGTRAP is breakpoint/step and
SIGURG/SIGWINCH are re-delivered, so those are special-cased; SIGUSR2 is not).
SIGUSR2 is chosen because it is catchable, never terminal-generated, and unused
by the Go runtime. Bingo never re-injects it (`Continue` resumes with signal 0),
so the tracee never actually receives it — resume is a plain Continue, and the
async-preempt SIGURG hazard doesn't apply (the darwin backend also forces
`GODEBUG=asyncpreemptoff=1`, so the tracee has no SIGURG anyway). This mirrors
Delve's *intent* (an on-demand halt surfaced to the wait loop) while fitting
bingo's `wait4` architecture — it does **not** use Mach `thread_suspend` (Delve
needs that only because it detects stops via a Mach exception port, not `wait4`).

**Resume-after-pause is a plain Continue.** bingo never *injects* the interrupt
signal (`Continue` does `PtraceCont(tid, 0)` / `PT_CONTINUE` with signal 0,
which suppresses the pending signal), so only the reporting thread entered
signal-delivery-stop and no whole-group stop was created. Resuming is therefore
identical to resuming from a breakpoint — no special handling. The
`pause`-labelled E2E spec (`declarePauseSpec`,
[debugger_e2e_common_test.go](test/integration/debugger_e2e_common_test.go),
wired into **both** the linux and darwin containers) runs the
Continue→Pause→Paused round-trip several times; if the first resume hung, the
second Pause's signal would never surface and the spec would time out.

`StopProcess()` itself is the hardened idempotent primitive from the Restart
groundwork: a `pid == 0` guard and `ESRCH` (process already gone) treated as a
no-op success. Delve's manual-stop is heavier (Linux: `sys.Kill(pid, SIGTRAP)` +
a `trapWaitInternal` halt-flag state machine; Darwin: Mach `thread_suspend` on
every thread + an atomic halt flag) because it lands *every* thread at a
consistent stop point; bingo's partial-stop model only needs the one reporting
thread suspended, so a single interrupt signal suffices on both platforms.

## Error handling

Conventions for wrapping, logging, and propagating errors live in
[docs/ErrorHandling.md](docs/ErrorHandling.md). The short version: return
wrapped errors (`fmt.Errorf("context: %w", err)`), never panic outside
programmer-bug territory, and log **once** at the owning top level (engine loop
/ hub / HTTP handler / `main`) via `slog`. Cross-goroutine errors do not use a
side `chan error` — every debugger outcome, failures included, rides the single
`Debugger.Events()` channel as a typed `protocol.Event` (`EventError` /
`EventProcessExited`) and is broadcast to clients as an `EventError`.

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
- `test/integration`: Ginkgo suite. A trivial placeholder spec runs by default;
  the real content is the **debugger E2E acceptance tests** — Ginkgo specs gated
  behind the `e2e` build tag that launch a real target and drive the ACTUAL
  native backend (ptrace on linux/amd64, ptrace+Mach on darwin/arm64), NOT the
  `fakeBackend`. These need a real kernel, so they only run on native runners
  (they can't run under emulation or with fakes). Split into:
  `debugger_e2e_common_test.go` (harness + target sources + shared spec bodies),
  `debugger_e2e_linux_amd64_test.go`, `debugger_e2e_darwin_arm64_test.go`, and
  `debugger_e2e_fullstack_test.go`. Ginkgo labels: `basic`
  (continue+step-over correctness), `churn` (multi-thread robustness), and
  `pause` (async-interrupt / manual-stop round-trip), all driving
  `debugger.Debugger` in-process; plus `fullstack`, which drives the
  same operations through the ENTIRE stack (pkg/client → WebSocket →
  internal/server → internal/hub → debugger → tracee) to catch transport/hub
  wiring regressions the backend-only specs can't (seq re-stamping of real
  events, the suspend/resume gate on a genuine BreakpointHit, synchronous
  SetBreakpoint confirmation routing). The `pause` spec (`declarePauseSpec`) is
  wired into **both** the linux and darwin containers — detection is
  platform-agnostic in the engine and each backend's `PauseSignal()` surfaces
  the interrupt through `wait4`. Each label is its own CI job. CI:
  [.github/workflows/debugger-e2e.yml](.github/workflows/debugger-e2e.yml).
  The linux jobs run fully on hosted runners. The darwin jobs compile and
  codesign the E2E binary on hosted macOS runners (the only CI check that the
  darwin backend even builds — the unit-test job on ubuntu compiles only the
  linux backend), but EXECUTION is gated to self-hosted runners via
  `if: runner.environment == 'self-hosted'`. macOS 14 (Sonoma) blocks
  `task_for_pid` on GitHub-hosted runners even with the debugger entitlement
  (the call hangs in the kernel), so the Mach backend can't attach there; hosted
  runners print a SKIPPED note and go green. Run darwin E2E locally via
  `just e2e-darwin`.
- **Darwin verification gate**
  ([.github/workflows/darwin-verification-gate.yml](.github/workflows/darwin-verification-gate.yml)):
  because the darwin backend can't be executed in CI, this human-in-the-loop
  check requires a maintainer to run `just e2e-darwin` locally and add the
  `darwin-e2e-verified` PR label whenever a PR touches darwin-native code whose
  runtime behaviour only runs on real Apple Silicon — matched by regex over
  `internal/debugger/*_darwin_*`, `internal/debugger/trap_arm64.go`,
  `test/integration/*_darwin_*_test.go`, and `entitlements.plist`. The "Darwin
  E2E verified" check fails until the label is present; it re-runs on
  `labeled`/`unlabeled` so adding the label flips it green without a new push,
  and is a green no-op for PRs that don't touch those paths. On `synchronize`
  (new commits pushed) the workflow removes the `darwin-e2e-verified` label
  itself before evaluating the gate — a verification only covers the commits
  it was run against, so it must not silently carry forward onto new,
  unverified commits — then re-checks the label live via `gh pr view` rather
  than the stale `github.event.pull_request.labels` payload, since that
  payload predates the removal. This needs `pull-requests: write` permission
  (not just `read`). Mark it a required status check in branch protection to
  actually block merges.

Build/test commands:

```sh
just build [linux amd64 | darwin arm64]   # produces ./build/bingo/...
just test [PKG]                            # go test -v
just coverage [PKG]                        # writes test/coverage.out
just integration                           # ginkgo -r ./test/integration (no e2e tag)
just e2e-linux                             # native linux/amd64 ptrace E2E (basic + churn + pause + fullstack)
just e2e-darwin                            # native darwin/arm64 ptrace+Mach E2E (codesigned)
# Filter to one label, e.g. only the correctness gate (package path must come
# before the -ginkgo.* flag so `go test` doesn't mistake it for the package):
go test -tags e2e -race ./test/integration -ginkgo.label-filter=basic
# The full-stack spec exercises client → WebSocket → hub → debugger → tracee:
go test -tags e2e -race ./test/integration -ginkgo.label-filter=fullstack
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
- **Error handling**: follow [docs/ErrorHandling.md](docs/ErrorHandling.md).
  New cross-goroutine failure paths surface as a typed event on
  `Debugger.Events()`, not a side channel; update that doc if you change a
  convention.
