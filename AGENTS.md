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
or attaches to a target Go binary, drives it via OS-level primitives (ptrace on
linux, Mach exception ports on darwin), and broadcasts events to one or more
WebSocket clients. The reference CLI client lives in `cmd/cli`.

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
  `go tool goimports -w` on staged `*.go` files. Match the surrounding style otherwise.
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
| [cmd/dapcli](cmd/dapcli/) | Interactive readline client that drives a session over DAP (mirrors `cmd/cli`'s UX). Talks to the server's `-dap-addr` listener; can create a session or `-session` join an existing one. |
| [cmd/wsmon](cmd/wsmon/) | Read-only terminal telemetry observer. `-session`-joins a running session over WebSocket and live-renders the goroutine spawn tree + OS threads + created/exited lifecycle deltas from the `EventGoroutineSnapshot` stream. Never drives execution — the WS-observes half of the DAP-drives/WS-observes demo. |
| [editors/vscode](editors/vscode/) | Platform-packaged TypeScript companion extension. Owns debugger type `bingo`, manages the shared server, and hosts the read-only Bingo Concurrency Activity Bar WebSocket observer. |
| [cmd/target](cmd/target/) | Trivial target program for manual testing. |
| [examples/level1-loop](examples/level1-loop/) … [examples/level5-workflow](examples/level5-workflow/) | Progressive debugger targets, selected by the root VS Code launch picker and built together with `just build-examples` (see [examples/README.md](examples/README.md)). |
| [examples/spawntree](examples/spawntree/) | Concurrency demo target: a deterministic main → supervisor → worker×N goroutine spawn tree for exercising the telemetry stream (see [docs/ConcurrencyTelemetry.md](docs/ConcurrencyTelemetry.md)). |
| [cmd/githook](cmd/githook/) | Conventional-commits commitlint, wired via [lefthook.yml](lefthook.yml). |
| [pkg/protocol](pkg/protocol/) | Wire types: `Event`, `Command`, payload structs, `EventKind`, `CommandKind`, `SessionState`. Single source of truth. |
| [pkg/client](pkg/client/) | Reference Go client. WebSocket-backed. Public surface: `Client` interface + `Create` / `Join` / `ListSessions`. |
| [internal/server](internal/server/) | HTTP/WebSocket entry. `Server`, `sessionStore`, `/api/sessions` and `/ws` handlers. |
| [internal/hub](internal/hub/) | Per-session bridge between connected clients and one `Debugger`. |
| [internal/dap](internal/dap/) | Debug Adapter Protocol translator. A `Handler` implements `hub.WSConn`, so a DAP/IDE client plugs into a hub session as just another client (ZERO hub changes). |
| [internal/dapclient](internal/dapclient/) | Lightweight DAP decoder shared by in-repo clients so namespaced bingo events coexist with standard go-dap messages. |
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

**Exact-version ingress is mandatory.** Every Go WebSocket peer validates each
inbound envelope with `protocol.ValidateVersion`, including the client's initial
welcome event and every later event. The hub validates commands in the client
read pump **before** `injectCommand`, so an incompatible `Continue` / `Step*`
cannot enter `resumeCh`. A missing/empty version is incompatible. On mismatch,
terminate only that connection with WebSocket close code 1002 and a reason that
names the expected and received versions; do not broadcast `EventError` or
increment the shared hub sequence for a peer-local compatibility failure.
`/api/health` remains a discovery preflight, not a substitute for per-envelope
validation. Exact enforcement of the existing contract does not itself require
a `Version` bump.

### Suspend/resume protocol

The hub blocks after broadcasting any of these "suspending" events until a
"resuming" command arrives (or the 30-min safety timeout fires):

- Suspending events: `BreakpointHit`, `Panic`, `Stepped`, `Paused`
- Resuming commands: `Continue`, `StepOver`, `StepInto`, `StepOut`

While suspended, **non-resuming** commands (`SetBreakpoint`, `Locals`, …) are
still executed immediately — the process is paused, so it's safe.

The 30-minute safety timeout synthesizes a versioned `CmdContinue` and uses the
same `executeCommand` path as a client resume. Success therefore performs the
normal `running` state transition and event ordering. A rejected auto-continue
broadcasts `EventError`, remains in the suspended wait loop, and re-arms a full
30-minute interval so client retries stay serviceable without a hot retry loop.

Ordinary-command admission is lossless while the originating client remains
connected. When bounded `cmdCh` is full, `injectCommand` backpressures that
client's read pump for up to five seconds while also selecting on hub shutdown
and the client's disconnect signal. If capacity never frees, the hub removes
and closes that client rather than silently dropping the command. This is
load-bearing for DAP's id-less confirmation FIFOs: a logged drop would leave a
pending request forever and shift every later confirmation, while a synthetic
`EventError` for an unadmitted tail could resolve the wrong FIFO head. The
lossy, capacity-one `resumeCh` is the deliberate exception described below.

A successful `Continue` emits a **non-suspending** `EventContinued` from the
engine (`engine.Continue` → `emitContinued`) before the process runs free. It is
not in the suspending set and does not gate the hub — it's a fire-and-forget
notification so a client that did *not* issue the resume (another WebSocket
observer, or a DAP adapter mapping it to a `continued` event) learns the tracee
is running again instead of waiting for the next stop. Steps do **not** emit it:
they self-complete into a suspending `Stepped`, which DAP maps to `stopped`
reason=step, not `continued`.

`Pause` and `Kill` are the odd ones out: both must act **while the process is
running**, not only while suspended, so neither is a resuming command. They ride
the ordinary `cmdCh` (like `SetBreakpoint`), which both Run's main loop and the
suspended wait loop drain. `Pause` is a suspending *request* whose suspend is
reported asynchronously via the `Paused` event once the interrupt surfaces (see
[Pause — async interrupt](#pause--async-interrupt)). `Kill` was previously
misrouted through `resumeCh`, which is only drained while suspended — so a `Kill`
against a runaway target (tight loop, no breakpoints) could never terminate it
through the protocol. Routing `Kill` via `cmdCh` fixes that. `Kill` with no active
debugger is a benign no-op success (nothing to terminate).

Stale resumes: a resuming command sent **while the process is still running**
(an erroneous or racing client) lands in `resumeCh` but is not drained by Run's
main loop. To stop it from satisfying a *future* suspend — auto-continuing past
a fresh `BreakpointHit`/`Stepped` before the client can inspect — `handleEvent`
**drains `resumeCh` before broadcasting the suspending event**. Draining *before*
(not after) the broadcast is load-bearing: the broadcast is the starting gun, so
any *legitimate* resume the client sends in response necessarily lands in
`resumeCh` after the drain and is caught by the wait loop, while any resume
already buffered when the drain runs is necessarily stale (a legitimate resume
can only follow the client observing the suspend). Draining *after* the broadcast
raced a zero-latency in-process client, which could put its legitimate resume in
`resumeCh` before the drain ran and have it silently eaten — wedging the session
(and flaking the hub tests under randomized load).

When multiple clients race resume commands: **first writer wins**, the rest
are dropped (`resumeCh` has capacity 1; see [hub.go injectCommand](internal/hub/hub.go)).

Rejected resumes: a resuming command only ends the suspend if the debugger
actually resumes the process. If the resume is **rejected synchronously** —
e.g. the dispatch returns an error and leaves the engine `stateSuspended` — the
hub broadcasts the `EventError` but **stays in the wait loop** (it checks that
the session left `suspended` before returning). Bailing out on a failed resume
would strand the client: the process is still suspended, but a retry resume
lands in `resumeCh`, which only the wait loop drains, so the session could never
be resumed again.

Resumes that are **accepted and only fail later** are a different problem, and
the wait loop cannot catch them: `Continue`/`Step*` off a software breakpoint
return `nil` as soon as the step-over single-step is armed, so the hub has
already transitioned to `running` and left the wait loop by the time the
failure happens on the engine loop. Those asynchronous halts are reported with a
suspending `EventPaused` — see [step-over flow](#software-breakpoint-step-over-flow).

**Leaving the wait loop is decided by observed state, never by command kind.**
Both branches that execute a command while suspended — `resumeCh` and `cmdCh` —
return only `if h.State() != protocol.StateSuspended`. The `cmdCh` branch used to
return unconditionally for `CmdRestart`/`CmdKill` on the premise that the process
it was waiting on no longer existed. That premise only holds when the command
*succeeded*: a `Restart` rejected before it touches the debugger (raw hub with no
factory, no prior `Launch` — an Attach-based session, malformed `RestartPayload`)
and a `Kill` the debugger refuses both broadcast an `EventError` and leave the
original process suspended. Returning there stranded the session exactly like a
rejected resume — every later `Continue`/`Step*` landed in `resumeCh`, which
Run's outer loop never drains — leaving a live, frozen tracee that only a
successful `Kill`/`Restart` or a full session teardown could recover (and on a
raw hub `Restart` can never succeed, so only teardown could). The state check is
exhaustive: successful Restart → `running` (returns), failed relaunch → `idle`
(returns, old debugger discarded), rejected Restart / failed `Kill` → still
`suspended` (stays, retryable). A **successful** `Kill` also leaves the state
`suspended` — `executeCommand` has no state case for it — so the loop stays
parked until the debugger reports its own teardown as `EventProcessExited` or a
closed `Events` channel, which the wait loop's events branch already handles
(engine shutdown always produces one; see
[Engine concurrency model](#engine-concurrency-model--non-obvious-invariants)).
Waiting for that signal is strictly more accurate than inferring death from a
command kind.

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

- **Synchronous** (`Restart`, `SetBreakpoint`, `ClearBreakpoint`, `Locals`,
  `Evaluate`, `StackFrames`, `Goroutines`): block until the matching
  confirmation event (or `EventError` for the same command kind) arrives.
  Implemented via `sendAndWait` in [pkg/client/ws.go](pkg/client/ws.go).
- **Fire-and-forget** (`Launch`, `Attach`, `Kill`, `Continue`, `Step*`, `Pause`,
  `RequestGoroutineSnapshot`): return as soon as the command is on the wire.
  Results arrive asynchronously on the `Events()` channel.

`RequestGoroutineSnapshot` is fire-and-forget **for a correctness reason, not
convenience**: `EventGoroutineSnapshot` is the only dual-purpose frame in the
protocol — the server also pushes it unsolicited on every entry/breakpoint/pause
stop — so it can never be correlated to a request by kind. A synchronous
`GoroutineSnapshot()` has no sound placement in the reply-debt model below, in
either direction. *Retiring* a timed-out snapshot request as debt lets that debt
silently swallow the next automatic push, permanently losing that stop's
lifecycle deltas. *Discarding* it instead re-opens #144 for this kind: a stale
reply — or any automatic push — then satisfies a later same-kind call. Even
before any timeout, a plain in-flight call is satisfied by whichever snapshot
lands first, which is usually a stop's automatic push. All three follow from one
root: a frame that can exist with **no request** cannot be matched to one.

So the fix is to remove the waiter, not to place it better. The request registers
**no pending entry and no reply debt**, and every snapshot — requested or
automatic — is delivered exactly once on `Events()`, where a client treats them
uniformly as telemetry. It must stay out of the correlated set: never route it
through `sendAndWait`. See issue #187.

Two consequences a caller must handle, both inherent to an id-less broadcast
protocol rather than to this change:

- **Delivery is best-effort.** The answer rides the shared `Events()` buffer,
  which `readPump` drops from when the consumer stops draining, and a rejected
  request answers with `EventError`(`CmdGoroutineSnapshot`) instead of a
  snapshot. Anything that waits for a snapshot must handle both — and must not
  treat that error as *its* answer: `EventError` is broadcast and carries no
  requester, so it may be another client's rejection with a valid snapshot still
  to come. Correlating it by kind is the same mistake as correlating the
  snapshot itself. `cmd/wsmon`'s `-once` shows the intended shape: the rejection
  is advisory, a later snapshot wins, and a `-timeout` deadline (not the error)
  is what bounds the wait.
- **The answer is broadcast.** A query is fanned out to every client on the
  session like any other event, and it carries empty `Created`/`Exited`. An
  observer that renders "the latest snapshot's deltas" (as `cmd/wsmon`'s
  lifecycle panel does) therefore blanks that panel until the next automatic
  stop; an append-only timeline (the VS Code view) is unaffected. Making a query
  addressable to its requester needs wire-level correlation and is deliberately
  out of scope here — a candidate for a future protocol revision.

`syncMu` serializes synchronous commands, but a timeout does **not** cancel the
command already sent to the server. The pending queue therefore retains a
timed-out request as retired reply debt until its matching confirmation or
matching `EventError` arrives. `routeToPending` always claims and removes the
oldest matching entry under `pendingMu` before notifying a live waiter; retired
matches are consumed without notification. This prevents a late response from
one of this client's timed-out commands from satisfying a newer same-kind call,
and prevents back-to-back matching frames from blocking the read pump on a
waiter's one-element channel. If a debt never resolves, a newer same-kind call
must time out rather than accept an ambiguous reply; subsequent same-kind calls
continue timing out while that reply stream remains one response short. Keep the
WebSocket open: disconnecting the last client tears down the hub/debuggee, while
unrelated async events remain valid.

Every command `sendAndWait` serves is a **genuine confirmation** — one that
cannot exist without a request — so the timeout path above is uniform, with no
per-kind exemption. Do not add one. `EventGoroutineSnapshot` is the only frame
that breaks that property, and while a synchronous snapshot query existed it
forced exactly such a carve-out (discard instead of retire). That carve-out
preserved the telemetry stream but could not create correlation: with a waiter
present, a genuinely late query reply or an unrelated automatic push could still
satisfy a later in-flight call. Removing the waiter removed the ambiguity — the
snapshot is now event-driven end to end and out of this model entirely.

This is deliberately bounded by the id-less broadcast protocol. The queue
preserves this client's ordered command stream; it cannot distinguish an
unsolicited same-kind event or confirmation caused by another driving client.
The single-driver caveat still applies until the wire protocol gains correlation
IDs.

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
   tracer thread. On Darwin there is no ptrace: the Mach calls run on the
   engine-loop thread itself (Mach ports are task-wide). Mirrors Delve's
   `execPtraceFunc`.

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
3. Reinstall the trap (`bps.reinstall`) if the breakpoint is still enabled.
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
`EventStepped`, not `EventBreakpointHit`. Linux can deliver a sibling's
already-queued hit after the trap has been restored, when amd64 RIP is still one
byte past the old INT3. The engine therefore retains each successfully restored
sentinel address **and its restored instruction bytes** for the session lifetime:
per-thread wait statuses expose no point at which every queued sibling hit has
drained. A later unmatched stop is rewound only when current memory matches one
of those restored histories and the architecture decoder proves no live trap
instruction starts at, or spans into, that address. A genuine `INT3`/`ICEBP`/
`CD 03` or ARM `BRK` is advanced instead; address history alone would re-execute
that trap forever, and on amd64 a `CD 03` whose second byte is the remembered
address would otherwise resume mid-instruction. Read/register/continue failures
on the stale-recovery path use `haltOnError` (`Error` then `Paused`).

Clearing a breakpoint is final even when the tracee is parked on it or already
single-stepping off it. A successful parked clear restores the bytes and
invalidates matching `lastBP` / `lastBPTID`; the live PC was rewound when the
trap stop arrived, so the next resume can continue directly from the original
instruction. During an in-flight step-off the entry is intentionally absent
from the table and its original bytes are already restored: `ClearBreakpoint`
matches the retained entry by ID, records its restored bytes, marks it disabled,
and succeeds. Completion then skips `reinstall` but still performs the saved
`bpResumeAction`. Recording the bytes keeps an already-queued same-address
sibling hit classifiable after owner death: it is rewound and resumed silently,
never reported or reinstalled. Failed restoration leaves the entry enabled and
all pending state intact. `clearAll` applies the same
invalidation/cancellation rules for Kill.

The non-nil `steppingOverBP` also reserves its address for the entire in-flight
step, even after a Clear disables the retained entry. Public `SetBreakpoint`
rejects that same resolved address before `breakpointTable.set` or any backend
memory access, while different addresses remain settable. Do not put the
temporarily disarmed entry back in `byAddr` to reserve it: `atAddr` classifies
real `StopBreakpoint` events and must describe only installed traps. The
reservation ends when step-off ownership clears `steppingOverBP`.

If `bps.reinstall` ever fails after a single-step, **suspend instead of
resuming**. Running without the trap is a runaway process; reporting the
error lets the operator intervene.

**A `StopBreakpoint` that arrives while `steppingOverBP` is set means the
step-over will never complete, so `handleStop` reconciles it there.** Only the
stepped thread's own `StopSingleStep` puts a lifted trap back, and neither
backend surfaces a foreign breakpoint while that thread can still produce one
(linux holds it in the wait-side queue, darwin re-faults it). So the only way
this combination occurs is that the stepped thread died mid-step — at which
point the entry is out of `byID`/`byAddr`, and the sole remaining reference to
it is the one-slot `steppingOverBP` that the next resume overwrites. An enabled
entry is reinstalled **before** `bps.atAddr`, not after, so a sibling stopped at
the same address resolves to the real breakpoint instead of taking the
spurious-SIGTRAP path. A concurrently-cleared entry is different: its original
bytes are already authoritative and `enabled=false`, so the backstop ends the
step without reinstalling it. Clearing is final on every completion path.
Placement is load-bearing and pinned by mutation: moving the reconcile below
the lookup fails the same-address spec, removing it fails both
(`engine_test.go` → "a breakpoint stop that arrives while a step-over is in
flight"); `engine_breakpoint_clear_test.go` separately pins the disabled path.
This mirrors the reconcile the `StopSignal` branch has always done; before it,
the two branches disagreed and only the signal half was safe.

On **linux** that death is now caught earlier and more precisely, by the
`StopStepThreadExited` boundary (park-queue rule 5): the backend refuses to
release the held stop at all until the engine has reinstalled, so the sibling
never reaches `handleStop` with `steppingOverBP` still set. With nothing parked
the backend holds the dying owner itself as the write anchor and reports the
boundary immediately, so the reinstall no longer waits for an unrelated thread to
stop. The reconcile above
is therefore the **cross-platform backstop** — it is what covers darwin, which
has no wait-side queue and no boundary event — and a no-op on linux, where
`steppingOverBP` is already nil by the time the held stop drains. Keep both:
the boundary is the ordered handoff, the reconcile is the invariant that a
breakpoint stop never proceeds while a lifted trap is outstanding.

The engine's `StopStepThreadExited` handler therefore has a strict order:
resolve the retained breakpoint entry, *then* acknowledge with
`completeStepThreadExit`, which is what releases the anchor. Resolution means
reinstalling an enabled entry through the anchor, or preserving the restored
bytes of an entry cleared mid-step; the cleared path still acknowledges, because
it has no write obligation but must open the gate. Both operations can fail and
halt suspended rather than tearing the session down — a failed reinstall keeps
`steppingOverBP` set and the anchor held, a failed release keeps the anchor held
and the gate closed, and in both cases `ContinueProcess`/`SingleStep` refuse
while `stepExitPending` so Kill/Restart is the only recovery.

**Every asynchronous halt in `handleStop` must be reported with a *suspending*
event, not a bare `EventError`.** These failures happen after the resume that
led to them already returned `nil` and emitted `EventContinued`, so the hub has
transitioned to `running` and left its suspend wait loop. `EventError` is not
suspending, so on its own it leaves the hub believing the tracee runs while it
is halted — and since `resumeCh` is drained *only* inside that wait loop, every
later `Continue`/`Step*` sits unread and the session is stranded for good
(issue #183). The rule at each such site is:

1. emit the detailed `EventError` (it carries the real cause),
2. ensure `setState(stateSuspended)`,
3. emit the suspending `EventPaused`.

`haltOnError(cmd, cause, stop)` does all three in the right order — use it
rather than open-coding the pair. Order matters: `Continued → Error → Paused`.
`Paused` must be last because the hub drains `resumeCh` *before* broadcasting a
suspending event, so a legitimate retry sent in response to `Paused`
necessarily lands after that drain.

`emitHaltedOnError` emits `EventPaused` with a **synthetic** goroutine and a
pure-DWARF location, and issues **no backend calls at all** — not
`emitPaused`/`goroutineSnapshot`, and not even a stack walk. The backend on this
path is by definition already failing, so any read could delay or prevent the
one event that restores liveness; clients can ask for frames or a snapshot once
the session is responsive again.

Delivery of that `Paused` is guaranteed by a **reserved buffer slot**. The
events channel is `eventBufSize + eventBufReserve` deep; ordinary `emit` refuses
to use the last slot, and only `emitReserved` — used solely by the halt path —
may. Without it a saturated buffer silently dropped the halt and recreated the
strand. `haltOnError` therefore emits the cause as an ordinary event (logging it
instead when no ordinary slot is free, since losing detail is far cheaper than
losing the suspend) and the `Paused` through `emitReserved`.

**One reserved slot suffices**, because a second halt can never be queued behind
the first: emitting a halt leaves the engine `stateSuspended` with the tracee
stopped, a stopped tracee cannot produce the next stop, and the only route back
to running is a resume — which the hub sends exclusively from inside the suspend
wait loop it enters *after receiving* that event. The same argument covers the
ordinary suspending events (`BreakpointHit`/`Stepped`/`Paused`/`Panic`): none can
still be sitting in the buffer when a halt is handled. The length check needs no
lock: the loop thread is the only writer, so `len` can only fall as the hub
drains, and observing room guarantees the send succeeds.

`EventPaused` is the right kind: `EventStepped` is treated as the entry stop by
the DAP restart path and can auto-continue, `EventBreakpointHit` would claim a
trap that may not be installed, and an unsolicited `EventBreakpointCleared`
would corrupt DAP's id-less FIFO correlation. A failed-reinstall breakpoint
stays **out** of the table: re-adding it would advertise an armed breakpoint
that can never fire.

What a halt promises is **control**, not a pristine tracee: the operator regains
the ability to inspect, retry, or kill. It does not guarantee the tracee's byte
state is consistent, and at the `populateBreakpointStop` site it specifically is
not — with no located stop the PC was never rewound off the trap, so on amd64
RIP sits one byte past the `INT3` and a plain `Continue` would execute from
mid-instruction. **`Kill`/`Restart` is the safe recovery there, not `Continue`**;
that site's `EventError` says so explicitly. Elsewhere a failed or partially
applied breakpoint write can leave an untracked trap behind. Reconciling tracee
state after such a write is a separate problem — do not add disabled-breakpoint
protocol/state here.

Note also that this rule covers the *reporting* of halts, not every internal
resume: the spurious-trap advance still ignores `ContinueProcess`'s error. If it
fails, the engine records `stateRunning` for a tracee that never resumed and no
event is emitted at all — the same class of liveness loss from the other
direction. It has no confirmed backend reachability, so **do not treat this
class as globally closed.** Every resume that can consume or requeue delayed
SIGURG does check the error: post-breakpoint Continue/StepOver/StepOut,
ordinary-signal auto-resume, and stale-Pause suppression all report
`EventError` followed by suspending `EventPaused`.

The sites are `populateBreakpointStop` and retired-sentinel verification/
recovery failure (`StopBreakpoint`),
`populateStopPC` failure and `bps.reinstall` failure (`StopSingleStep`), the
`bpResumeStepOut` return-breakpoint `set` failure (which is also the one site
that must add the otherwise-missing `setState(stateSuspended)` — it is reached
with the engine still `stateRunning`), and the in-flight reinstall and manual
`populateStopPC` failures (`StopSignal`). The `bpResumeSourceStep` fallback is
already safe: it emits a suspending `EventStepped`.

## Foreign-thread stop parking during a single-step (linux)

Source: the platform-neutral `classifyUserStop` + `stepQueue` in
[waitpark.go](internal/debugger/waitpark.go), embedded in `linuxBackend` and
driven from `Wait` in
[backend_linux_amd64.go](internal/debugger/backend_linux_amd64.go). Normal stop
parking and delivery live entirely in `Wait`. The sole engine handshake is the
abnormal stepped-thread-death boundary: the backend cannot reinstall the
breakpoint entry the engine owns, so it returns an internal
`StopStepThreadExited` before releasing the real held stop.

`stepQueue` owns the step bookkeeping (`stepping`/`stepTID`, via
`beginStep`/`endStep`) *and* the held stops, because the two are one state
machine: the release gate requires both "no hardware step outstanding" and "no
dead-owner reconciliation pending". It is embedded, so every pre-existing
`b.stepping` / `b.stepTID` use site is unchanged. It carries no build tag and no
backend dependency so the ordering/gating rules are unit-testable — and
mutation-checkable — on any host.

**The invariant: while a single-step is outstanding, `Wait` does not return a
user-visible stop belonging to any other thread.** The sibling stays
ptrace-stopped exactly where it is and is delivered from a FIFO queue on a later
`Wait`, once the step has completed and the engine has reinstalled the trap it
stepped off.

Why it is needed (issue #199, linux only): `Wait` uses `Wait4(-1, …, WALL)`, so
a sibling thread's `SIGTRAP` can surface in the middle of the
restore→single-step→reinstall sequence, when the stepped-over trap bytes are out
of the tracee and its entry is out of `bps` (see
[step-over flow](#software-breakpoint-step-over-flow)). Handing that to the
engine destroys the step-over state machine two ways: a **distinct** sibling
breakpoint overwrites `lastBP` and the next resume overwrites the one-slot
`steppingOverBP`, permanently losing the original entry with its trap disarmed
(so its `ClearBreakpoint` id fails); a **same-address** sibling finds no entry,
takes the spurious-SIGTRAP path, advances PC and calls `ContinueProcess`, which
clears `stepping`/`stepTID` so the real step completion is misclassified. Darwin
is immune by construction — its receive loop already loops until the trap belongs
to the stepping thread — which is why this is a linux-backend fix and darwin has
no counterpart.

**Why it must be in the backend and not the engine.** The `Backend` interface has
exactly one TID-explicit resume, `SingleStep(tid)`. `ContinueProcess`,
`WriteMemory` (POKEDATA) and the `ReadMemory` PEEKDATA fallback are all TID-less
and target `traceTID()` == `lastStopTID` == the TID of the stop `Wait` most
recently **returned**. The engine is written against the implicit invariant
*"the stop I am handling is the thread I will act on and resume"*, and it has no
way to restore that pairing for an event it chose to replay later: `recordStop`
is backend-private, and by the time a deferred event were replayed the exact
step completion would already have overwritten `lastStopTID`. Parking inside
`Wait` preserves the invariant instead of trying to reconstruct it — a parked
event is simply not a stop yet, and `recordStop` runs at **delivery**, never at
park time. The death boundary also stays backend-owned: it borrows the oldest
held stop's still-stopped TID as the write anchor without dequeuing that stop,
then the engine acknowledges only after reinstalling its breakpoint.

Rules are enforced by `Wait`, except for the one serialized engine
acknowledgement in rule 5 (see the locking note below):

1. **Park only foreign user-visible stops.** `classifyUserStop` parks a
   `StopBreakpoint`/`StopSignal` only when a step is in flight (`stepping` **and**
   a non-zero `stepTID`) and the stopping TID is not `stepTID`. Everything the
   loop already absorbs inline stays inline: clone events, non-main thread
   exits, a new thread's initial `SIGSTOP`, `SIGURG`, `SIGCONT`. (`exec` is the
   one `PTRACE_EVENT` that is *not* absorbed — see rule 9.)
2. **Never park the stepped thread's own stop.** Its trap is the step completing
   and its signal is the step's outcome; both must reach the engine or the trap
   is never reinstalled and nothing can ever drain. A `stepping` flag with a zero
   `stepTID` is treated as no step at all for the same reason — parking against a
   completion we cannot recognise would block forever.
3. **Dequeue only when no step is outstanding.** `stepQueue.releasable` refuses
   to pop while `stepping`; `drainParked` (which wraps it) runs at the top of
   each `Wait` iteration, before blocking in `wait4`, and returns nothing while
   `stepping`. This is what guarantees a same-address sibling is delivered only
   after the reinstall, so it resolves to a real breakpoint rather than the
   spurious-trap path.
4. **Current-stop state is installed on delivery only.** Never at park time —
   moving the resume
   target to a thread the engine is not working on is precisely the corruption
   being fixed. The same rule covers the stop's **signal**, which is why the
   queue holds whole `StopEvent`s: the signal is a field of the parked event,
   not current backend state. `drainParked` moves it into the per-TID pending
   signal map only when the event is released, immediately before publishing
   `lastStopTID`; recording it at park time would let another stop consume it
   against the wrong thread (issue #206).
5. **A dead stepped thread closes through an internal reconciliation boundary.**
   `interruptStepIfStepped` clears hardware ownership (`stepping`/`stepTID`) when
   the exact stepped TID exits, because its completion can never arrive, but it
   keeps the parked-stop gate closed. `Wait` then returns `StopStepThreadExited`
   anchored to a thread that is genuinely ptrace-stopped, **without dequeuing any
   held stop**. The engine reinstalls `steppingOverBP`, then calls
   `completeStepThreadExit`; only the following `Wait` may deliver the real stop.
   If reinstall fails, the original entry remains owned by `steppingOverBP`,
   `Error → Paused` is emitted, and Continue/Step are rejected; Kill/Restart is
   the safe recovery. This prevents a sibling from replacing `lastBP` while the
   original logical breakpoint is still out of the table.

   **The anchor is whichever stopped thread is available, and the dying owner
   itself is one of them.** The boundary needs a TID the engine can `POKEDATA`
   into, and ptrace can only write through a thread that is *actually stopped*.
   Two supply that:

   - **The step owner, held.** When it dies through `PTRACE_EVENT_EXIT` and
     nothing is parked, `planAbsorb` returns `holdStepOwner` and `applyAbsorb`
     **skips the resume entirely**, keeping it at that stop. `PTRACE_EVENT_EXIT`
     fires in `do_exit` *before* `exit_mm`, so its address space is still mapped
     and a write through it reaches the breakpoint. It is released exactly once,
     by `completeStepThreadExit`, and only after the engine has reinstalled.
   - **The oldest parked stop**, which lends its TID without being dequeued.
     A parked sibling is **never resumed** on the boundary's behalf: the engine
     has not been told about that stop, so running its thread would be the very
     corruption the queue exists to prevent.

   The held owner is preferred when both exist. A **reaped** owner
   (`ws.Exited()`/`ws.Signaled()`) cannot be held — it is already gone — so that
   shape keeps the original lazy fallback: `stepExitPending` stays set, `Wait`
   keeps blocking in `wait4`, and the first foreign stop to arrive parks (the
   park condition includes `stepExitPending`) and supplies the anchor. Whole
   process death still routes through the main thread and tears down as usual.

   `completeStepThreadExit` returns an error because that release is a real
   ptrace op. `ESRCH`/`ENOENT` is benign — the anchor was mid-exit and simply
   finished dying, which is what the release asked for. Any other failure leaves
   the hold in place **and the gate closed**, and the engine halts suspended
   (   `Error → Paused`, no new `waitLoop`) rather than resuming past a thread
   nothing will ever deliver. `ContinueProcess`/`SingleStep` already refuse while
   `stepExitPending`, so both halt paths make Kill/Restart the only recovery.
   No path leaks a held owner: it is discharged on a successful acknowledgement,
   dropped by `purge` (and therefore by `abortStep`) where the thread it names is
   gone or belongs to a replaced image, and otherwise discharged by the engine's
   explicit discard on a session-invalidating failure (rule 11).
6. **Teardown purges inside `Wait`.** `stepQueue.purge` runs where the process is
   confirmed gone — the leader's real exit, its signal death, and `ECHILD` — and
   drops the held anchor along with the parked stops. It deliberately does NOT
   run at the leader's `PTRACE_EVENT_EXIT` any more: that stop no longer proves
   death (rule 10). There is deliberately **no engine-callable purge**: the queue
   is not part of the engine's state model.

   **A parked TID is not guaranteed to still be alive**, and nothing in the queue
   invalidates an entry when its thread dies. `TASK_TRACED` carries
   `TASK_WAKEKILL`, so a group-wide kill pulls a parked thread straight out of
   its stop with no ptrace op from us. Measured on Linux 6.10: a worker parked at
   a signal-delivery stop, never touched by the tracer, moved on its own to
   `PTRACE_EVENT_EXIT` the moment the group was `SIGKILL`ed — and then held
   there, because *that* stop does wait for a continue. (That is also why
   `reapAfterKill` must continue every stopped thread it finds: not because the
   original parked stop survived the kill, but because the kill left the thread
   at `EVENT_EXIT`.)

   So both paths that use a parked TID must tolerate its death, and they do: the
   anchor's write can fail, which halts suspended with a `kill or restart`
   diagnosis rather than corrupting anything, and the release goes through
   `continueIfTraceeExists`, which treats `ESRCH` as success. Do not add a
   liveness assumption on the anchor or drain paths, and do not remove that
   `ESRCH` tolerance.
7. **Absorbing a stop on the stepped thread must re-arm its step.** Every site
   that handles a stop inline and resumes the thread names its branch to
   `stepQueue.planAbsorb` (via `absorbStop`/`applyAbsorb`) instead of reaching
   for a ptrace primitive: the stepped thread gets `PTRACE_SINGLESTEP`,
   everything else `PTRACE_CONT`. The kernel delivers one stop per resume, so an
   absorbed event on the stepped thread has **consumed** the pending step. A plain continue there cancels it while `stepping`/`stepTID`
   stay latched — no completion can ever arrive, so rule 3 holds the gate shut
   forever, every later foreign stop parks, `Wait` never returns and the tracee
   freezes. This generalises what the `SIGURG` branch always did; do not add an
   absorb site that calls `continueIfTraceeExists` directly. Two consequences
   worth knowing: the re-armed step resumes *after* the absorbed event, so the
   stepped instruction can land one instruction further than the engine asked.
   The reinstall address is unaffected (it is the saved `sob.addr`, not the
   stopped PC), so a step-over is exact; but a machine-granularity
   `bpResumeStep` whose instruction is itself the `clone` reports an
   `EventStepped` PC one instruction beyond single-step semantics. That is a
   real, very narrow inaccuracy rather than a purely cosmetic one — accepted
   because the alternative it replaces is a hard    freeze. A signal-delivery stop happens before the interrupted instruction, so
   re-arming there executes the promised instruction exactly. `PTRACE_SINGLESTEP`
   always uses signal 0: injecting a signal would enter the handler frame before
   executing the instruction the engine is stepping. SIGURG is instead retained
   per TID through the re-step, requeued to that exact TID, and forwarded only
   from the resulting fresh signal-delivery stop; see
   [Linux signal forwarding](#linux-signal-forwarding).

   The decision is pure and table-tested, but a branch choosing to consult it is
   not, and that gap was real rather than theoretical: with a live tracee as the
   only way into the wait loop, rewriting the `CLONE`, `SIGURG`, `SIGCONT` or
   non-main `SIGSTOP` branch back into a bare `continueIfTraceeExists` — the exact
   freeze above —
   passed the entire unit suite, as did dropping the thread-exit gate release and
   both `abortStep` guards. `Wait` therefore takes its three kernel calls through
   the nil-able `waitFn`/`contFn`/`stepFn` seams so tests can script wait statuses
   and record the primitive each branch used
   (`TestLinuxWaitResumesTheSteppedThreadWithASingleStep` and its converse). Keep
   every new branch reachable that way. The non-main `SIGSTOP` branch needs a
   stepped-thread case even though it reads as a brand-new thread's initial
   group-stop: it keys off `tid != pid`, not on the thread being new, so any
   non-main `SIGSTOP` reaches it, including one on the thread under an active
   step. Its `absorbNewThread` routing is what makes that harmless, and only a
   stepped-thread case holds it there.

   The same rule covers the signal a held stop carries. `Wait` copies it at the
   park site (`Signal: int(sig)`) and the engine only ever sees the replayed
   copy, so a dropped signal is invisible until a real tracee misroutes one.
   Seeding the queue with a `StopEvent` the test built cannot catch that — the
   value under test comes from the test. `TestLinuxWaitPreservesTheSignalOfAHeldStop`
   drives `Wait` so the number comes off a scripted wait status through the
   production copy, and asserts the exact signal rather than merely a nonzero
   one. **There are two such copies, and each needs its own gate**: the park
   site, and the ordinary inline `return` that hands a stop straight to the
   engine. Gating only the park site left the inline one free — zeroing it
   passed the whole suite, including the E2E, which counts stops by reason and
   accepts the `signal 0` rendering. That copy is what the engine compares
   against `PauseSignal()` to tell a Pause interrupt from an ordinary signal, so
   `TestLinuxWaitDeliversTheSignalItStoppedOn` pins it by exact value on both
   paths through it (no step in flight, and G6's stepped-thread-own-signal).
   One residual: swapping an `absorbKind`
   *label* between two branches that share a `planAbsorb` row is behaviour-
   preserving and not detected — the decisions are pinned, the names are not.
8. **An unknown `PTRACE_EVENT` on the stepped thread cannot re-arm and must fail
   the wait instead.** With only TRACEEXIT/TRACEEXEC/TRACECLONE enabled it is
   unreachable in practice, and if it were reached there is nothing to reason
   about: re-arming assumes a stop shape we do not understand, and continuing
   resumes a tracee whose software breakpoint is still absent from memory. It
   calls `stepQueue.abortStep` — lift the gate, drop the held stops *and* any
   held anchor — and returns an error from `Wait`. The engine turns that into an
   `EventError` and tears the session down (`engine.go`'s `stopCh` error branch).
   This case stays **TID-keyed**: the same event on any other thread is absorbed
   with a plain continue.
9. **`PTRACE_EVENT_EXEC` is fatal unconditionally.** It is deliberately NOT an
   absorb decision and has no `planAbsorb` row: `execve` replaces the process
   image for **every** thread, so every breakpoint address, every saved
   instruction byte and every tracked thread id describes memory that no longer
   exists. There is nothing to reconcile against whether or not a step was in
   flight and whichever thread it is reported on, so `Wait` calls `abortStep` and
   errors immediately, **without resuming the exec stop**. Two facts the error
   message must respect: after `execve` the kernel reports this stop under the
   **thread-group leader's pid**, and the execing thread's former tid is only
   retrievable via `PTRACE_GETEVENTMSG` — so the reported pid must never be
   described as the stepped TID. Both are now *in* the message.

   It is also the **retraction point for a stashed leader exit** (rule 10).

   **bingo's own startup is out of reach by construction**, which is what makes
   an unconditional rule safe. The launch path consumes its own `execve`
   `SIGTRAP` in `startTracedProcess`'s private `Wait4` *before* calling
   `PtraceSetOptions`, so the image bingo itself launched never produces an
   `EVENT_EXEC` and never reaches `Wait`; `Restart` builds a brand-new debugger
   and repeats that sequence; and the attach path (`attachToProcess`) currently
   installs **no ptrace options at all**, so it cannot generate the event either.
   Keep that ordering if you touch either path.

   That guarantee is about the execve *bingo performed*, not about "nothing
   execs early". A tracee whose first image immediately execs a second one — a
   **binfmt interpreter that re-execs**, e.g. Rosetta under emulated
   linux/amd64 Docker, or some qemu-user setups — really does produce a
   post-startup `EVENT_EXEC`, and it now kills the session where it used to be
   silently continued. That is the intended verdict rather than a regression:
   the entry stop, the DWARF slide and every address derived from them describe
   the pre-exec image, so the old behaviour worked by coincidence. It is also
   why the linux E2E suite cannot run under emulated Docker at all — a
   restriction that already existed for other reasons (these specs require a
   native kernel), but which now surfaces immediately instead of subtly.

   Making this tolerant is NOT cheap and must not be done with a blind
   "ignore the first exec": that would re-admit exactly the corruption the rule
   prevents for a genuine mid-session exec. It needs a verified handshake inside
   `startTracedProcess` that identifies and re-validates the final image before
   any DWARF load or breakpoint install.
10. **A stop under the LEADER's id is evidence of a terminal, never proof of
    one.** `de_thread()` retires the old leader when a **non-leader** calls
    `execve()`: the execing thread takes over the leader's pid and the retired
    task's exit is delivered as `PTRACE_EVENT_EXIT` under `tid == pid` while the
    process runs on. A leader that merely `pthread_exit()`s is the same shape.
    Verified against Linux 6.10 — the observed sequence is

        (LEADER-ID) EVENT_EXIT  msg -> "exited, code 0"   <- process ALIVE
        siblings    EVENT_EXIT + real exits
        (LEADER-ID) EVENT_EXEC  msg = execing thread's FORMER tid
        (LEADER-ID) EVENT_EXIT + real exit                 <- the actual death

    and the retired leader's status is byte-for-byte an ordinary clean exit, so
    **no status heuristic can work**. Returning a terminal there reports a dead
    process to the client while the tracee is alive under the same pid.

    So `Wait` **stashes** the `GETEVENTMSG` status (which must still be read
    before resuming — that stop is the only moment it can be read at all, #94),
    resumes the leader, and keeps waiting. It **commits** the terminal only on
    genuine group death — the leader's real `Exited()`/`Signaled()`, or `ECHILD`
    — and it **retracts** the stash on `EVENT_EXEC`, which is positive proof of
    retirement. The stash lives on `linuxBackend` (it is not queue state) and,
    like `parked`, is touched only inside `Wait`, so it needs no lock.

    **An observed `wait4` status beats the stash**, and the direction is
    load-bearing rather than arbitrary. `GETEVENTMSG` at `EVENT_EXIT` yields that
    *task's* `do_exit` value, whereas the leader's real wait status carries the
    **group** exit code (`wait_task_zombie` reports `group_exit_code` once
    `SIGNAL_GROUP_EXIT` is set). They diverge exactly in the scenario this rule
    exists for: measured on Linux 6.10, a `main` that calls `pthread_exit(0)`
    stashes `exited, code 0` while the process is alive, and a worker that later
    calls `exit(3)` — or `abort()`s — is reaped as code 3 / signal 6. Preferring
    the stash would report a clean `0` for both, which is the very class of
    misreport #94 exists to prevent. The stash is therefore the fallback for the
    one commit point with no observed status at all, `ECHILD`.

    Consequence to know: `EventProcessExited` for an ordinary exit is now emitted
    at real group death rather than at the leader's `EVENT_EXIT`. The exit *code*
    is unchanged. If a stop is parked when the group dies, the leader's zombie is
    not reapable until the engine drains it, so the terminal is reported after
    that drain — later, but never lost, because `Wait` drains before blocking and
    the step machinery always resolves through the owner's death boundary.
    Trading a false terminal for a **lost** one would be strictly worse than the
    bug this fixes, so that whole ordering — leader exit stashed, dead step owner
    reconciled, held sibling delivered, terminal finally committed with the
    authoritative code — is pinned end to end by
    `TestLinuxWaitStillCommitsTheTerminalWithStopsHeld`.
11. **The fatal branches must not abandon the stops they own.** Rules 8 and 9
    give up on the tracee, and `abortStep` drops the queue's *record* of the
    threads it was holding — but those threads are still ptrace-stopped, as is the thread
    whose stop triggered the failure. Leaving them is not merely untidy:
    `tracerThread.close()` ends the locked OS thread, and the kernel then
    detaches every tracee and **resumes** it, straight into software breakpoints
    still written in its text.

    Both branches therefore wrap `ErrSessionInvalidated`, and the engine's
    `stopCh` error branch discards the tracee explicitly before returning:
    `endThreadStep`, `bps.clearAll` (restore the original bytes **first** — that
    is what makes the release safe), then `proc.kill(backend, running:false)`.
    For a launched tracee `reapAfterKill` loops `Wait4(-1, WALL)` and continues
    every thread it finds stopped, which discharges the parked stops and the
    trigger stop; for an attached one the detach is safe because the traps are
    already gone. `running` is false because the `waitLoop` that produced the
    failure has already delivered its result, so `killProcess` is the sole
    reaper (#111). Only these two branches carry the marker: ordinary `wait4`
    failures deliberately keep their pre-existing behaviour, which still leaks a
    detach-and-resume, and closing that generic class belongs with the
    wait-ownership work (#205/#217).

    The exec case inverts one step. `ErrImageReplaced` (which wraps
    `ErrSessionInvalidated`) suppresses the restore, because writing saved bytes
    back would poke instructions from the OLD image at OLD addresses into the new
    one. Nothing of ours is in that image to remove.

    What this does **not** claim: a fully clean release of an ATTACHED tracee.
    `killProcess` detaches the leader and the kernel releases the remaining
    threads when the tracer thread closes, but a thread parked at a software
    breakpoint still has its PC one trap-width past the trap on amd64 and
    `abortStep` has already discarded the metadata needed to rewind it, and
    `bps.clearAll` ignores individual restore failures. A launched tracee is
    killed so none of that matters; an attached one may resume mid-instruction.
    Fixing that needs per-TID quiescing, checked restoration and PC repair, which
    is out of scope here — do not describe attached release as safe.

**Explicit limits — this is NOT an atomic stop-the-world step-over.** Sibling
threads keep running and keep trapping during a step; the fix only guarantees
that their stops are *reported later*, after the trap is back:

- The trap really is absent from tracee memory for the duration of the step, so
  a sibling executing that address in the window legitimately does not trap.
  Exact per-cycle sibling hit counts are inherently racy — never assert them.
- A user `StepOver`/`StepInto` may still surface as a sibling's
  `EventBreakpointHit` rather than `EventStepped`: the step completes, the trap
  is reinstalled, and the queued sibling is then delivered as the next stop.
  Callers already tolerate this; the `churn` spec always has.
- Queue depth is bounded by the live thread count — a ptrace-stopped thread
  cannot stop again until it is resumed (G7) — so no cap is needed.
- A table-resident user `ClearBreakpoint` of an address a parked stop refers to
still surfaces that stop as a generic SIGTRAP. An in-flight clear is different:
its retained entry records the restored bytes before it is disabled, so an
owner-death boundary can release a same-address sibling without resurrecting
the breakpoint or resuming mid-instruction. One-shot engine sentinels retain
the same kind of restored-byte history. In both cases a delayed hit is rewound
only when current bytes match the engine's successful restore and no live
architecture trap spans the address. The focused engine tests pin the exact
`SetRegisters(tid, rewoundPC)` operation, a genuine live trap at the same
address, and x86's cross-boundary `CD 03`; the plain native overlap spec requires
`LinuxRetiredInternalBreakpointCount > 0`; an armed-ness probe alone cannot
distinguish safe recovery from the mid-instruction path.
- **A stepped thread that dies never releases a held stop directly.** The
  internal boundary is returned first and records the anchor TID as `traceTID`,
  so the engine's reinstall writes through a genuinely stopped thread while every
  actual `StopEvent` remains FIFO-owned by `Wait`. After acknowledgement, the
  top-of-loop drain delivers them before blocking in `wait4`; main-process exit
  still purges anything not yet delivered. When the anchor is the dying owner
  itself, that acknowledgement is also the only thing that resumes it — and a
  parked sibling is never resumed at all. The queue, scripted-`Wait`, engine,
  and native raw-`SYS_exit` tests separately pin the gate, exact-TID anchor,
  breakpoint ownership, and real-kernel path for both anchor shapes.
- **Kill ownership is unchanged, and no second reaper is added.** Killing a
  *suspended* tracee still reaps through `reapAfterKill`, which loops
  `Wait4(-1, WALL)` and resumes whatever it finds ptrace-stopped: a parked thread
  — or a held anchor — is ptrace-stopped like any other and is indistinguishable
  to that loop, so the queue costs it iterations, nothing more. Killing a
  *running* tracee still
  leaves reaping to the in-flight `waitLoop` (#111 forbids a second `wait4`
  reaper here, and #205 is why a process-global one is dangerous in a shared
  server). The one honest cost: because the drain runs before `wait4`, that
  final `waitLoop` can return a held stop instead of blocking, so it absorbs
  fewer thread deaths than it would have, leaving a few more unreaped statuses
  until the tracer process exits. Bounded by the live thread count, the same P3
  class as #217's unreaped leader, and it neither hangs nor wedges teardown —
  `declareStepOverlapKillSpec` kills mid-step with stops held and requires both
  `Kill` and the event-stream close to complete. Do not "fix" this with a purge
  on kill: `purge` is `Wait`-owned (rule 6) and draining before blocking is what
  makes a same-address sibling resolve after the reinstall (rule 3).
- **The `kill` label itself never exercises the queue.** `declareKillRunningSpec`
  sets no breakpoints — it launches, `Continue`s, and `Kill`s — so the engine
  never single-steps, `beginStep` is never called, nothing is ever parked, and
  `resumeFor` returns `resumeContinue` for every absorbed stop, which is the
  pre-change `continueIfTraceeExists` verbatim. A failure in that label is
  therefore not attributable to the queue. The one seen (native run
  `31344255645`) hung in `startTracedProcess`'s `Wait4(pid)` waiting for a *new*
  tracee's execve stop, and is the pre-existing #205 hazard: `engine.Kill`
  injects a synthetic `StopExited`, so the loop can reach `stateExited` while the
  real `waitLoop` is still blocked in the process-global `Wait4(-1, WALL)`. That
  orphan absorbs the SIGKILLed threads' deaths, loops back, blocks again with no
  statuses left, and can then collect the *next* iteration's child exec stop
  before discovering `done` is closed and exiting — which is why no second
  `wait4` frame survives in the timeout dump. Fixing it belongs with #205/#217,
  not here.

**No lock on the queue, and none is needed.** `parked` is touched only inside `Wait`.
Successive `Wait` calls run on different one-shot `waitLoop` goroutines that the
engine starts with `go e.waitLoop()` **after** consuming the previous `Wait`'s
result from `stopCh`, so no two ever overlap; that channel-and-goroutine-start
chain is also what orders the `stepping`/`stepTID` writes the engine makes in
between (`SingleStep` sets them, `ContinueProcess` clears them) and the
`completeStepThreadExit` acknowledgement against the next `Wait`'s reads.
Only the cumulative diagnostic counters are atomic because native tests read
them concurrently; do not add a queue mutex. This no-lock rule applies only to
`stepQueue`: pending signals cross from `Wait` to the engine loop and therefore
have their own mutex.

Regression gates: the classifier table, the queue-mechanics tests **and** the
resume-decision tests covering rules 7–11 in
[waitpark_test.go](internal/debugger/waitpark_test.go) — all host-agnostic, so
they run and can be mutation-checked on macOS — plus the backend-specific tests
in [backend_linux_amd64_test.go](internal/debugger/backend_linux_amd64_test.go)
that pin *when `lastStopTID` moves* (not on park, only on delivery, and `Wait`
drains before blocking) and drive `Wait` itself over scripted wait statuses so
every branch's effect on the step is executed rather than merely decided —
including the unconditional exec refusal, the held-owner anchor, its exactly-once
release, the benign-`ESRCH` release, the gate-stays-closed release failure, the
deferred/committed/retracted leader exit and the session-invalidated marker —
plus the cross-layer engine-over-real-queue specs in
[engine_stepqueue_test.go](internal/debugger/engine_stepqueue_test.go) that pin
reinstall-before-release for both anchor shapes, plus the engine specs that pin
rule 11 (a session-invalidating wait failure restores the tracee's instructions;
an ordinary one does not), plus the linux-only `overlap`
E2E label in
[debugger_e2e_linux_amd64_test.go](test/integration/debugger_e2e_linux_amd64_test.go):
step-over overlap, machine-step (`StepInto`) overlap, a foreign ordinary-signal
storm, `Pause` racing an in-flight step, kill with stops held, a raw
`SYS_exit` that kills the exact stepped thread with a sibling parked, the
same raw `SYS_exit` with **nothing** parked so the dying owner anchors itself,
and a **non-leader `execve`** whose `de_thread()` leader retirement must surface
as an image-replacement error rather than a fabricated process exit.
Those assert
only invariants the fix guarantees — both logical breakpoints remain tracked at
every observable stop, every hit belongs to a known id, no error or unexpected
exit, both ids still clearable, threads still making progress at the end of the
run — plus non-vacuity: the park counters, the held-owner counter, the signal
number, and (reported, not asserted) the rule-7 re-arm counter. The `churn` label
runs five rounds in CI.

**Non-vacuity is asserted, not assumed.** The overlap those specs provoke is
inherently racy, so a run that never actually parked a stop proves nothing about
the rule. `LinuxParkedStopCount`
([park_diag_linux_amd64.go](internal/debugger/park_diag_linux_amd64.go)) exposes
the backend's cumulative park counter through the public `Debugger` surface —
the same test-hook shape as darwin's `DarwinTaskPortSendRefs` — and the
step-over and foreign-signal specs **fail** if it never advanced. Without that
gate a build with the parking rule deleted still passes every other assertion in
those specs. The counter is cumulative and `atomic` because the test reads it
from its own goroutine; draining and purging describe what is still held, not
what was ever held, so neither rolls it back.

`LinuxParkedSignalCount` splits out the held **signal** stops. The wait loop
absorbs SIGURG, SIGCONT and a new thread's initial SIGSTOP before
`classifyUserStop`, so a signal stop that reaches the queue is an
externally-directed interrupt. The ordinary-signal overlap target exposes one
locked signal TID and a separate locked thread stopped on a breakpoint at a raw
`nanosleep` syscall. The harness starts a machine step over that syscall, then
directs SIGUSR1 to the known foreign TID. The step owner blocks runtime
preemption's SIGURG, and the target gates its SIGCONT storm until this transaction
finishes, so the blocking syscall keeps the step open until the foreign signal
is provably parked. After forwarding, the signaled locked thread must hit its
own gated breakpoint under the same TID, proving the backend resumed the thread
that actually stopped rather than merely reporting the signal. The Pause overlap
spec uses the same counter for its directed main-thread SIGSTOP. In both cases,
a held signal is direct evidence that the interrupt arrived *while a single-step
was outstanding*.

**The foreign-signal spec also gates the signal *number*, not just its arrival.**
A held stop carries its signal in the `StopEvent` the wait loop builds at park
time (rule 4), so a build that dropped that field would still deliver the stop,
still advance every counter, and merely report `signal 0`. The spec therefore
records the numbers the engine reported and requires `SIGUSR1` to be among them
and `0` not to be — with a held **signal** stop asserted separately, since that
is the population whose number travels through the queue. `SIGUSR1` is chosen
because ptrace intercepts and forwards it to the exact stopped TID, whose handler
changes only an atomic counter in the target; its exact value is asserted, so do
not swap it.

**`LinuxStepRearmCount` is the native evidence for rule 7.** The failure that
rule prevents is a freeze, which a test can otherwise only observe as a timeout —
indistinguishable from a slow runner. The counter records how often an absorbed
stop landed on the stepped thread and re-armed its step, so a positive value
proves the rule fired against a real kernel. The foreign-signal target storms
`SIGCONT` (absorbed, and harmless because nothing there is ever group-stopped)
specifically to make that landing happen; the spec reports the count rather than
asserting a threshold, because which thread a process-directed signal lands on is
racy per cycle.

**`LinuxStepThreadExitCount` is the native evidence for rule 5.** The dedicated
C/assembly target places a breakpoint directly on `SYS_exit`, installs a hot
sibling breakpoint while that thread is suspended, then resumes the removed
instruction. The count proves the exact step owner died, and re-setting/clearing
the original ID proves reconciliation restored logical ownership before the
sibling surfaced.

That spec asserts `parked + held > 0`, **not** `parked > 0`, and the distinction
is load-bearing rather than defensive. Which thread anchors the boundary is racy:
if a sibling has already parked it lends its TID, otherwise the dying owner is
held. Since the owner-held path resolves the whole transaction in microseconds,
a fast runner routinely completes it before any sibling parks — that is what the
original `parked > 0` gate started failing on once holding was introduced, and it
was the gate that was wrong, not the run. Both anchors are correct, so the honest
non-vacuity claim is that an anchor existed at all.

**`LinuxHeldStepOwnerCount` is the native evidence for the other anchor shape,
and the reason there is a second `SYS_exit` target.** The spec above arms a hot
sibling line *before* resuming, so a sibling stop is essentially always parked by
the time the owner dies — which means it passes identically whether or not the
held-owner rule exists. `declareStepOwnerHoldSpec` therefore arms **only** the
raw `SYS_exit`, so the queue is empty at the death and the dying owner must
anchor its own reconciliation. Its target spawns one doomed thread at a time on a
long interval, which does two things: it keeps a second trap from being pending
when the step begins (that would supply the very sibling anchor the spec exists
to avoid), and it guarantees a **later** thread reaches the same logical
breakpoint afterwards. That later hit, carrying the original breakpoint id, is
what proves the trap was really written back through the held thread — it is
unreachable if the hold or the reinstall is dropped. The parked count is
deliberately not asserted there: an empty queue is the premise.

**A held interrupt is usually surfaced, not suppressed — but only usually.** It
surfaces when it drains while `manualStopPending` is still set, which is the
common case for a source-level step-over: the queue is released as soon as the
machine step finishes, and the engine then runs on to the next-line breakpoint,
so the interrupt is delivered well before the `EventStepped` that would clear
the flag. It is suppressed when a self-stop completes the step first — the
documented pending-interrupt race. Both are legitimate, so `EventPaused` is
asserted across a whole run and never per cycle.

**Sizing rule for these specs — they race their own target's watchdog.** Every
E2E target self-terminates with `go func() { time.Sleep(180 * time.Second);
os.Exit(0) }()`. A `-race` cycle costs ~0.8–1.05s and hosted runners vary by
~20% (run 31330091994 was uniformly that much slower than 31329090064), so an
iteration count tuned on a fast runner can exceed the watchdog on a slow one and
surface as a **spurious `ProcessExited`, not a real regression** — that is what
failed `churn` at iteration 199/200 there. Keep ≥~35% headroom at the slow rate
when changing `BINGO_E2E_OVERLAP_ITERS` (default 90) or
`BINGO_E2E_CHURN_ITERS` (200 by default, pinned to 120 for the five-round CI
loop); prefer more rounds over a longer single round, since rounds reset the
watchdog while iterations spend it.

`BINGO_E2E_OVERLAP_PAUSE_ITERS` (default 70) is sized from the opposite
direction: it is bounded above by the watchdog like the rest, but bounded
*below* by statistical power. Only ~10–22% of cycles land the interrupt inside
the machine-step window (measured 9/40, 5/40 and 4/40 across three native runs),
so the held-interrupt assertion is the spec's own flake risk — at 40 cycles it
would fail spuriously ~0.9^40 ≈ 1.5% of the time. 70 takes that to ~0.06% and
still costs only ~88s at the slowest per-cycle rate yet observed. Lowering it
re-introduces the flake; raising it much further runs into the watchdog.

## Linux signal forwarding

Source: [pending_signal.go](internal/debugger/pending_signal.go) plus the
delivery and resume wiring in
[backend_linux_amd64.go](internal/debugger/backend_linux_amd64.go).

Linux ptrace reports an ordinary or fatal signal as a signal-delivery stop. The
engine emits one `EventOutput` and resumes; resuming with signal `0` suppresses
delivery, so a synchronous fault immediately refaults and fatal signals never
terminate (issue #206). The backend owns the fix because only it knows both the
stopped TID and the signal argument to the next ptrace resume:

1. A surfaced `StopSignal` is recorded in `currentByTID[tid]` **before**
   `lastStopTID` publishes that TID. If a distinct signal for that TID arrives
   while an older current signal is retained across a failed continue or a
   signal-zero step, the older signal moves to `backlogByTID[tid]` instead of
   being overwritten; matching standard signals coalesce. The newest delivered
   stop stays current because it is the signal the exact next `PTRACE_CONT` must
   inject, while the displaced signals are requeued to that TID in capture order.
   Pending state is never process-wide: another stop may move `lastStopTID`, but
   cannot transfer or clear a different TID's signals.
2. Only `PTRACE_CONT` injects a pending signal. `PTRACE_SINGLESTEP` always uses
   signal zero and leaves all pending state intact: injecting any signal while
   stepping enters its signal frame and reports a trace trap before the
   instruction the engine promised to step has executed. This state is normally
   consumed by the automatic continue from `StopSignal`; it remains observable
   only when that resume fails and the engine suspends through `haltOnError`, in
   which case later steps must not lose or prematurely inject it. The next
   exact-TID continue forwards the actual non-zero value.
3. SIGURG on the exact `stepTID` additionally needs to survive an absorbed stop.
   The #202 `applyAbsorb` path saves it in `delayedByTID[tid]`, re-arms the
   real instruction step with signal zero, and retains the delayed value through
   further steps. The next continue uses exact-TID `tgkill(SIGURG)` followed by
   `PTRACE_CONT(..., 0)`; only the resulting fresh signal-delivery stop resumes
   with `PTRACE_CONT(..., SIGURG)`. A simultaneous current signal has priority
   while the displaced-signal backlog and delayed SIGURG are requeued, matching
   instances coalesce, and the first delayed signal wins until it has been
   requeued.
4. The parked-stop FIFO from #202 retains the signal inside the `StopEvent`.
   Parking never touches pending state. `drainParked` performs the same
   signal-before-TID delivery handoff as a live `Wait` result.
5. Pause's main-thread `SIGSTOP` is deliberately excluded from current state,
   preserving manual Pause and leftover-interrupt suppression even when the same
   TID has delayed SIGURG. Clone `SIGSTOP`, `SIGCONT`, ptrace events and spurious
   breakpoint traps remain internal Wait-loop cases with signal zero. A raw
   non-main `SIGSTOP` is not proof that its TID is a new clone: when it belongs
   to the active `stepTID`, Wait preserves that owner's pending state while
   re-arming `PTRACE_SINGLESTEP(..., 0)`.
6. Resume takes a per-TID batch transactionally. Successfully requeued backlog
   entries are not restored if a later requeue fails; the unsent suffix and
   current signal are. A failed public exact-TID `PTRACE_CONT` restores the
   current signal even for `ESRCH`, while already-requeued entries stay owned by
   the kernel. Internal continue retains the established conservative rollback:
   on ptrace failure it restores the whole captured batch because retirement has
   not been established. A stopped TID cannot deliver the first requeue before
   retry and matching standard signals coalesce; if `ESRCH` meant the TID escaped
   that stop, its fresh delivery goes through `set`, which coalesces it against
   the restored backlog. Internal single-steps leave every pending collection
   intact. Conclusive retirement paths — non-main `Exited`, `Signaled`, and
   `PTRACE_EVENT_EXIT` branches plus held-owner release — clear their TID
   **before** any resume. The branch treating a non-main `SIGSTOP` as a clone
   initial stop also clears stale reused-TID state unless that TID is the active
   step owner; the ambiguous owner case must retain its signal across the
   signal-zero re-arm. Exec, process exit, Kill/detach, and tracer shutdown purge
   all state to prevent TID reuse. The maps are mutex-protected because `Wait`
   publishes from its goroutine while the engine consumes them; unlike
   `stepQueue`, this state crosses goroutines.

The host-agnostic map tests, linux backend raw ptrace-tuple tests, the `signals`
E2E label (one SIGSEGV/SIGABRT output followed by signal death, one handled
thread-directed ordinary signal with post-delivery sibling progress, and Pause
suppression), the handler-returning stepping-thread SIGURG discriminator in
[sigurg_step_e2e_linux_amd64_test.go](internal/debugger/sigurg_step_e2e_linux_amd64_test.go),
and the foreign-signal `overlap` spec are the regression gates. The focused
discriminator pins two one-byte instruction advances, exact
`tgkill → PTRACE_CONT(0) → fresh stop → PTRACE_CONT(SIGURG)` tuples, handler
return, and post-handler exit. The overlap spec must observe a signal-specific
parked-stop count increase; signal outputs plus generic parked traps are not
proof that a signal was held during a step. Backend mutation gates additionally
pin failed-continue → signal-zero step → distinct same-TID signal, requiring the
older signal to be requeued to that TID and the fresh signal to be the exact
`PTRACE_CONT` argument; an ambiguous non-main `SIGSTOP` on the active step owner
must likewise preserve the retained signal through its signal-zero re-arm and
inject it on the next exact-TID continue. Direct cleanup coverage pins all three
non-leader death branches.

Deferred-signal transactions necessarily recreate delivery with `tgkill`.
They preserve every distinct signal number, its target TID, and the backlog's
requeue-attempt order, but not the original `siginfo_t`; once multiple standard
signals are kernel-pending, Linux chooses their delivery order, and signals that
arrive concurrently during requeue or the re-step may interleave. Preserving
that provenance and delivery ordering requires the broader wait-ownership work,
not a process-global pending-signal scalar in this layer.

**Composition constraint for #205:** a future process-wide wait broker may own
the raw `wait4`, but it must route the complete `(TID, signal)` status to the
owning session/backend and preserve FIFO order. Pending signal state stays
per-session and per-TID; it must not move into one process-global broker slot or
be installed before the owning backend actually delivers the stop.

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

Pure Mach exception-port model (issue #92) — **no ptrace, no wait4 for stop
detection**. Launch is `posix_spawn` with `POSIX_SPAWN_START_SUSPENDED`; stops
are detected by a `mach_msg` receive loop.

- **Exception port, not wait4.** `acquirePorts` registers a task-level Mach
  exception port masking **only `EXC_MASK_BREAKPOINT`** and folds it, a dead-name
  notification port, and a private control port into one port **set**. `Wait`
  blocks in `bingo_mach_recv` on that set. A software `BRK` and a hardware
  single-step both raise `EXC_BREAKPOINT` (msg id 2401, `exc=6`, `code0=1` —
  indistinguishable by code, so step-vs-breakpoint is disambiguated by the
  engine's `stepping`/step-over bookkeeping). Process exit arrives as the
  dead-name notification (`BINGO_MSG_DEATH`) → `reap` via `wait4`.
- **Native BSD signals + async preemption ON.** Because only breakpoints are
  masked, Unix signals are left to native, thread-directed delivery. Go's
  async-preemption `SIGURG` (a `pthread_kill` to a specific M) therefore reaches
  the exact M the runtime targeted, so `GODEBUG=asyncpreemptoff` is **not** set
  (`asyncPreemptOffDefault = false`) — this is the whole point of #92. Tradeoff:
  the debugger no longer surfaces Unix signals as *stops* on darwin (a crash
  becomes a process exit, not a signal stop). bingo's engine only needs
  breakpoint/step/exit and no e2e asserts on signal reporting.
- **Instruction-cache coherency is mandatory on every write.**
  `bingo_write_memory` wraps `mach_vm_write` with `task_suspend`/`task_resume`
  (quiesce + pipeline drain) **and** a `mach_vm_machine_attribute(MATTR_CACHE,
  MATTR_VAL_CACHE_FLUSH)` on the patched range. The attribute call is NOT a no-op
  on Apple Silicon (it returns `KERN_SUCCESS`; an earlier comment claimed
  otherwise). Without it, a freshly-written trap (`<stepover-next>`,
  `<stepout-return>`) that is re-executed within microseconds can hit a stale L1
  I-cache line and be silently skipped — the root cause of the ~2.5% StepOut/
  step-over wedge that #92 fixes. A trap re-hit a full loop-iteration later never
  flaked (the line had been evicted), which is why the bug was intermittent.
- **Deferred exception reply.** With only `EXC_MASK_BREAKPOINT` masked and BSD
  signals native, replying `KERN_SUCCESS` to a breakpoint exception *immediately*
  lets XNU build a `_sigtramp` frame for a pending signal before the engine reads
  registers. So `Wait` receives with `do_reply=0`, stashes the reply header by
  tid (`stashReply`), and keeps the faulting thread un-acked (frozen at its real
  PC). The reply is flushed only immediately before that thread is resumed.
  **Invariant: every resume path MUST flush its reply or `Wait` hangs forever** —
  `ContinueProcess`/`killProcess` call `flushAllReplies`; `SingleStep` /
  `singleStepThread` / the `advanceStepOver` re-arm call `flushReply(tid)`. At
  most one pending reply per thread.
- **Exception messages carry send rights that MUST be balanced.** Every
  `exception_raise` (msgh_id 2401) the kernel delivers inserts a send right to
  BOTH the faulting thread (`desc[0]`) and the task (`desc[1]`) into our IPC
  space; Mach coalesces each onto our existing port name and bumps that name's
  user-ref count. Left unbalanced this is a per-stop leak: the task is a single
  cached name whose uref then grows without bound until `KERN_UREFS_OVERFLOW`,
  after which the kernel can no longer copy out the exception and `Wait` wedges
  (also one leaked dead name per exited thread under churn). So `bingo_mach_recv`
  **deallocates the task right unconditionally** (we hold the cached `taskPort`,
  so its uref stays ≥1) and hands the thread right back; the caller balances that:
  the main `Wait` path folds it into the retained per-thread set
  (`adoptExcThreadPort` — release if already tracked, else adopt, mirroring
  `Threads()`), and the `bingo_stop_the_world` drain path deallocates each
  absorbed straggler's thread right directly (it re-faults on the next resume).
  Reconcile runs on EVERY received exception (including intermediate step-over
  traps), not just returned stops, or the retire loop leaks. The regression gate
  is the `hygiene`-labelled darwin E2E spec.
- **Stop-the-world = individual thread suspends + drain.** On a real breakpoint,
  `stopTheWorld` (`bingo_stop_the_world`) suspends every running thread and
  **drains any already-queued exceptions** (reply to each). `thread_suspend` does
  NOT flush a thread's already-queued Mach exception message, so a sibling
  thread's breakpoint that was queued before it got suspended would otherwise
  resurface mid-single-step and be misread as the step's completion. Draining
  closes most of that race (Delve does the same).
- **Single-step ignores traps that aren't the stepping thread's.** The drain
  above is best-effort: a non-blocking receive cannot catch a sibling exception
  the kernel has not finished *delivering* yet, so one can still surface during
  the single-step window under heavy thread churn. `Wait` therefore only lets a
  trap whose faulting thread == the stepping thread (`isStepThread` for the
  atomic step-over, `consumeStep(tid)` for the ablation step) drive step
  completion; a straggler sibling breakpoint stays parked (already suspended,
  reply stashed) and re-faults on the next resume. Without this guard the
  straggler is misread as a bogus `StopSingleStep`, leaving the real single-step
  armed and wedging the next resume — the residual `churn` step-over hang. This
  is the "loop until the trap belongs to the stepping thread" invariant (mirrors
  Delve's `singleStep`); it is the definitive fix, with the drain as an
  optimization that keeps the parked-straggler path rare.
- **Resting-state invariant:** when the engine is `stateSuspended`, every live
  thread has Mach `suspend_count >= 1`. `ContinueProcess` = normalizing
  `thread_resume` of ALL threads to 0. `singleStepThread(tid)` = resume ONLY
  `tid` (others stay held): with the world stopped, sysmon is frozen, so no NEW
  `SIGURG` is generated in the step window. `task_threads` returns threads in
  **creation order**, so `threads[0]` is frequently an idle runtime M, not the
  user thread — step primitives target `curTID` (the thread that faulted), never
  `threads[0]`.
- **`task_for_pid` requires the `com.apple.security.cs.debugger` entitlement**
  (the build embeds [entitlements.plist](entitlements.plist) via codesign). The
  task port is acquired **once** past the launch race and cached (`taskPort`);
  re-calling `task_for_pid` per op intermittently wedges in the kernel.
- **Kill is wait4-based, no `cmd.Wait`.** `killProcess` flushes replies, resumes
  every thread, `SIGKILL`s, and reaps via `wait4` — it does not block the engine
  loop in `cmd.Wait`, so kill-while-running no longer deadlocks (the old
  wait4/ptrace failure). `launched` distinguishes a spawned tracee (SIGKILL) from
  an attached one (detach).
- **ASLR slide** is computed in `TextSlide` by scanning the VM map for the first
  exec region with the 64-bit Mach-O magic. Do NOT use `TASK_DYLD_INFO` — its
  image array is unpopulated at the very first stop.

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
  to the engine — with two exceptions. The **main thread's**
  `PTRACE_EVENT_EXIT` is the one exit the engine needs, and
  `PTRACE_EVENT_EXEC` is fatal and fails the wait outright (park-queue rule 9).
  `PTRACE_O_TRACEEXIT` stops the leader
  *before* it dies, and that stop is the only moment its `wait(2)`-encoded status
  can be read, so `Wait` reads it there via `PTRACE_GETEVENTMSG` — returning a
  hardcoded 0 instead dropped every tracee's exit code (#94). It does **not**
  report a terminal there: that stop is also what `de_thread()` produces when a
  non-leader execs, so the status is stashed and the terminal is committed only
  once the group is genuinely gone (rule 10). Where a real `wait4` status is
  available it wins, because the leader's wait status carries the GROUP exit code
  while the `EVENT_EXIT` payload is only that task's `do_exit` value; the stash is
  the fallback for `ECHILD`, where there is no observed status at all.
- ptrace stops are per-thread. The backend records the last stopped TID and
  targets `ContinueProcess` / memory writes at that TID, not blindly at the
  process PID. `lastStopTID` is `atomic.Int64`: `Wait` records stops from the
  one-shot `waitLoop` goroutine, while running engine commands can concurrently
  read it through `traceTID` (`Kill` → breakpoint cleanup, or WebSocket
  Set/ClearBreakpoint). The atomic is only a defined TID snapshot, not a
  stop-state guarantee; the ptrace call still returns its normal error if that
  TID is not stopped. Ordinary suspended operations remain ordered by the
  `waitLoop` → `stopCh` → engine-loop handoff. Regression coverage is
  `TestLinuxBackend*StopTID*`, run under `-race` in the unit-test workflow.
  Non-main thread exits are absorbed inside `Wait`.
- User-visible signal stops carry per-TID pending delivery state.
  `ContinueProcess` injects only the signal owned by the exact resumed TID;
  `SingleStep` always uses signal zero and preserves that state for a later
  continue. A parked stop does not install pending state until delivery. Pause
  `SIGSTOP` is excluded and all teardown paths purge it. See
  [Linux signal forwarding](#linux-signal-forwarding).
- `ReadMemory` uses **`process_vm_readv(2)`** as the fast path, falling back to
  `PTRACE_PEEKDATA` only when it is unavailable or short-reads. `process_vm_readv`
  bulk-copies the whole buffer in one syscall and — unlike ptrace ops — is NOT
  thread-bound, so it runs directly off the caller and skips the `execPtrace`
  tracer-thread handoff entirely (mirrors Delve). This is load-bearing for the
  goroutine snapshot: it issues dozens of small reads per stop across every live
  goroutine, and the old word-at-a-time-PEEKDATA-through-execPtrace path made a
  snapshot-on-every-breakpoint so slow it pushed the `churn` e2e into its
  then-fixed 180s target watchdog. The harness now derives that watchdog from
  `BINGO_E2E_CHURN_ITERS` (2s per iteration plus 60s) before compiling the
  target, so the tuning knob scales both test work and target lifetime. The
  fallback keeps the original error semantics for genuinely-unmapped addresses.
  (Darwin was never affected — it already bulk-reads via `mach_vm_read`.)
- Single-step vs breakpoint disambiguation uses **both** `stepping` and
  `stepTID` (the exact TID `SingleStep` was issued against). Only a `cause==0`
  SIGTRAP on `stepTID` is the step's completion; the same stop on any other
  thread is that thread hitting an INT3 and is reported as a breakpoint. This
  matters because `Wait4(-1, …)` can return a sibling thread's concurrent
  breakpoint (or SIGURG) while a step is in flight — keying off `stepping`
  alone would misclassify it and corrupt the engine's step-over state machine.
  Correct classification is necessary but not sufficient: a correctly-labelled
  foreign breakpoint handed to the engine mid-step is still corrupting, so `Wait`
  also **parks** it until the step completes — see
  [foreign-thread stop parking](#foreign-thread-stop-parking-during-a-single-step-linux).
- **One live tracee per process.** `Wait4(-1, …, WALL)` is scoped to the whole
  tracer *process*, not to one tracee, so two `Debugger`s running at once in the
  same binary steal each other's stops — observed as `PTRACE_GETREGS` `ESRCH`
  for a foreign tid and both backends wedging in `Kill`. Nothing filters by pid,
  and adding a filter would break the deliberate any-thread wait. Tests must
  therefore own exactly one tracee for its whole lifetime; an e2e spec that
  needs a differently-configured target launches it in a spec of its own, after
  the previous `DeferCleanup` has killed the last one.
- `g` pointer for goroutine inspection lives at `FS_BASE` on amd64.
- `killProcess` never reaps the zombie itself while the engine's `waitLoop` is
  in flight (a *running* tracee). That waitLoop is blocked in `Wait4(-1, WALL)`
  and is the **sole** legitimate reaper: it absorbs every thread's SIGKILL death
  and surfaces `StopKilled`. A second reaper in `killProcess` deadlocks Kill two
  ways (#111): it races the waitLoop for the same stops, and — because a Go
  tracee is always multi-threaded — `Wait4(pid)` targets only the thread-group
  leader, whose zombie stays unreapable until every sibling is reaped, which
  `killProcess` cannot do. So `killProcess` only reaps when the tracee was
  **suspended** at a stop (no waitLoop), via `reapAfterKill`, which loops
  `Wait4(-1, WALL)` (any thread, never the leader's pid), resuming any
  ptrace-stopped thread so the pending process-wide SIGKILL kills it, until
  `ECHILD`. The engine passes `running` (state == running) down through
  `process.kill` to drive this. (Darwin has no such race — its `waitLoop` blocks
  on a Mach port, not `wait4`, so its `killProcess` `Wait4(pid)` is always the
  sole reaper and ignores `running`.) Regression guard: the `kill` e2e loops the
  launch→run→Kill cycle (`BINGO_E2E_KILL_ITERS`).
- `SIGURG` re-delivery is mandatory here too. A sibling SIGURG is re-delivered
  immediately on that sibling's continue. A SIGURG on `stepTID` consumes the
  outstanding step, so `applyAbsorb` reissues the instruction step with zero
  and saves SIGURG per TID; the next continue requeues it to that TID and forwards
  it only from the fresh signal-delivery stop. Never use
  `PTRACE_SINGLESTEP(SIGURG)` or inject delayed SIGURG directly from the
  single-step SIGTRAP stop.

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
- `locationForPC` must **bracket** the PC (`prev.Address <= pc < next.Address`,
  `prev` a non-end-sequence row), not merely pick the greatest address `<= pc`.
  Go emits CUs with `DW_AT_ranges` (no contiguous low/high pc), so `cuContainsPC`
  can't pre-filter them and every CU is scanned; without the bracket check a CU
  whose whole line program lies below `pc` would falsely "match" on its trailing
  end-sequence row and return a bogus file:line. The function name comes from the
  independent `funcIndex` (binary search over subprograms) and is always right;
  only the file:line needs the bracket. This matters for the goroutine snapshot,
  which resolves creation-site file:line for many off-CPU PCs.
- Runtime-introspection helpers (`runtimeVarAddr`, `structMemberOffset`,
  `runtimeArrayInfo`) resolve a package var's address, a struct field's byte
  offset, and an array's base/count/stride from DWARF **by name** (cached). They
  back the goroutine/thread snapshot reader (`goroutines.go`), which never
  hardcodes runtime layout — offsets shift between Go versions. See the
  concurrency snapshot section below.
- `LocalsForFrame` (and `EvaluateName`) only evaluate the `DW_OP_addr` (0x03)
  and `DW_OP_fbreg` (0x91) location expressions to an address; register-allocated
  variables come back as `<optimized out>`. Given an address + a resolved
  `dwarf.Type`, the **type-aware formatter** in
  [values.go](internal/debugger/values.go) (`formatTyped`) reads the correct
  number of bytes per type and renders a **bounded, eager `protocol.Variable`
  tree** — nested structs/slices/arrays and one pointer deref appear inline as
  `Children`, so both WebSocket and DAP can expand values without a path/
  expression parser (deferred to a later PR). Supported kinds: signed/unsigned
  ints of every width, bool, float32/64, complex64/128, `string` (reads the
  {ptr,len} header then bounded bytes), pointers (hex + one deref child), structs
  (fields), arrays and slices (slice = {ptr,len,cap} header → elements), plus
  best-effort one-line summaries for map/chan/func/interface (a pointer/word hex;
  no children). Type classification unwraps `TypedefType`/`QualType` and detects
  Go's higher-level kinds by typedef **name** (`map[`, `chan `, `interface {`)
  before unwrapping, because their underlying representation is a plain pointer/
  struct that would otherwise misclassify; a visited-typedef guard breaks the
  `interface {}` / `error` self-reference.
- **Bounds & fallback (never error the stop):** `maxValueDepth=4`,
  `maxChildren=100` (overflow appended as a synthetic `… N more` node),
  pointer-deref depth `1`, `maxStringBytes≈256`, and a **visited-address cycle
  guard** (pointer targets) so self-referential structures can't infinite-loop.
  Those are *per-path* caps; on top of them a **shared global ceiling**
  (`maxTotalNodes=10000`, `maxTotalBytes=256 KiB`) is threaded through `formatCtx`
  and debited once per `formatNode` and by every read. Without it a
  collection-of-collections stays bounded only by the *product* of the width caps
  (`[][][][]int` ≈ `maxChildren⁴` ≈ 10⁸ nodes, each its own `ReadMemory`), which
  would wedge the single-threaded engine loop for a single `Locals`/`Evaluate`/
  `variables` request. When either budget is exhausted the walk stops expanding
  and appends **one** truncation node (`Value: "<truncated: inspection budget
  exhausted>"`) — a per-path degradation, never an error on the stop.
  Reads go through the backend's bulk `ReadMemory` (linux `process_vm_readv`,
  darwin `mach_vm_read`) sized per type — never per-byte. Any unreadable address
  degrades that one node to `<unreadable: …>`; an unknown/absent type degrades to
  an 8-byte hex or `<optimized out>` — the surrounding tree and the stop are
  unaffected. The pure leaf-formatters (`formatInt/Uint/Float/Bool/Complex`) take
  raw bytes and are unit-tested without DWARF or a backend; the global ceiling is
  pinned by `TestFormatTypedBudget` (a pathological nested aggregate truncates to
  ~`maxTotalNodes`).
- `subprogramVars` is a depth-aware walk of the full containing subprogram,
  not a flat scan to the first null DIE. Both `DW_TAG_lexical_block` and
  `DW_TAG_inlined_subroutine` define variable scope. The containing subprogram
  must have a concrete matching range (Go's no-range subprograms are abstract
  inline definitions); nested-scope containment compares the **unslid** stopped
  PC against every range from `dwarf.Data.Ranges`
  (linux lexical blocks use range lists and ranges can be discontiguous);
  missing or unreadable ranges are conservatively active rather than making
  variables disappear. Definitely inactive subtrees are pruned with
  `Reader.SkipChildren`, and same-name variables are deduplicated so the
  deepest active declaration wins for both Locals and EvaluateName. Concrete
  inline DIEs whose names only exist through `DW_AT_abstract_origin` remain
  unsupported and are skipped — do not resolve abstract origins or location
  lists without extending the documented expression model deliberately. A
  concrete ranged subprogram with `Children=false` has no locals; never seed a
  child walk for it, or the next sibling DIE leaks into the frame. Malformed
  child-DIE read errors propagate instead of becoming a truncated success.
- `EvaluateName` resolves a **single variable name only** (no dotted paths /
  indexing / arithmetic): a local or parameter in the subprogram containing the
  frame PC first, then a package-level global via `globalVar` (matches the exact
  DWARF name or a package-qualified `pkg.name` suffix, `DW_OP_addr` only). Backs
  the WebSocket `CmdEvaluate`/`EventEvaluate` and the DAP `evaluate` request.
- **`DW_OP_fbreg` locals resolve against the CFA, not a fixed FP offset** — this
  is why [frame.go](internal/debugger/frame.go) exists. Go compiles every
  function's `DW_AT_frame_base` to `DW_OP_call_frame_cfa`, so a local at
  `DW_OP_fbreg -104` lives at `CFA - 104`, and the CFA (the caller's SP at the
  call) is **per-function**: on **arm64** it is `frame-pointer + framesize`
  (x29 points at the saved FP/LR pair at the *bottom* of the frame, locals sit
  *above* — so it is emphatically NOT `FP + 16`), and on **amd64** the body rule
  is `FP + 16`. The only thing that encodes framesize is the `.debug_frame` Call
  Frame Information, which `debug/dwarf` does **not** parse — so `frame.go` is a
  minimal CFI reader that evaluates just the CFA column (`parseFrameTable` →
  `runCFAProgram`, which tracks only `def_cfa*` + `advance_loc*` +
  `remember`/`restore_state` and parses every other opcode solely to skip its
  operands without desyncing; unknown opcode → stop). `engine.frameLocation`
  calls `dwarfReader.cfa(pc, sp, fp)` per frame, **chaining outward by
  `SP_{i+1} = CFA_i`** (a callee's CFA is its caller's SP; Go passes args in
  registers so arm64 leaves SP unmoved and amd64's retaddr push is already in the
  FP rule) and walking the saved-FP chain (`fp = *fp`). `walkStack` preserves
  saved return PCs, but every non-top frame uses `returnPC-1` for function/line/
  scope and CFI lookup (underflow guarded): the saved PC is after CALL/BL and can
  equal an exclusive lexical/FDE boundary, while one byte back lies within the
  call instruction on both supported architectures. The top frame's live PC is
  never adjusted. Missing/uncovered CFI degrades gracefully to the old `FP + 16`
  heuristic (`cfaFallbackFromFP`) rather than erroring the stop. The reader is
  arch-generic (maps SP/FP DWARF register
  numbers by `runtime.GOARCH` — arm64 SP=31/FP=29, amd64 RSP=7/RBP=6) and
  wrapped in `recover()` so malformed CFI can never crash the engine. Go's DWARF
  sections are zlib-compressed (`__zdebug_frame`/`.zdebug_frame` with a 12-byte
  `"ZLIB"` header, or ELF `SHF_COMPRESSED`); `loadDWARFData` inflates the frame
  section itself (`inflateZlib`) since `debug/dwarf` only decompresses the
  sections *it* consumes.

## Goroutine / thread snapshot — the concurrency data foundation

Source: [internal/debugger/goroutines.go](internal/debugger/goroutines.go)
(reader) + the emit sites in [engine.go](internal/debugger/engine.go).

**What it is / why.** The data foundation for concurrency visualization (a
goroutine-spawn-hierarchy tree, a live-thread view, lifecycle tracking). It
reads the Go runtime's `runtime.allgs` (every goroutine) and `runtime.allm`
(every OS thread) straight from tracee memory via DWARF-resolved struct offsets,
and streams a `GoroutineSnapshotPayload` carrying: the goroutine set with
`ParentID` spawn linkage, each goroutine's `StartLoc` (startpc) and `CreatedLoc`
(the `go` statement, gopc), the thread set, the current goroutine, and
**created/exited goid deltas** since the previous snapshot.

**Version 1.3** (`pkg/protocol`): reserves goroutine ID 0 with status `unknown`
for a synthetic unresolved stopped goroutine. Consumers must not attribute that
stop to a real goid; DAP omits its optional `stopped.threadId`.

**Version 1.1** reshaped `Goroutine` (added `ParentID`,
`StartLoc`, `CreatedLoc`, `ThreadID`, `Current`; renamed `GoLoc`→`CreatedLoc`),
new `Thread`, new `GoroutineSnapshotPayload`, new `EventGoroutineSnapshot` +
`CmdGoroutineSnapshot`.

**Size contract — DEFINED BUT NOT YET IN FORCE (issue #194).** These two events
are the only ones carrying an unbounded runtime collection, and on a concurrent
target they outgrow what a consumer can accept. `pkg/protocol` now defines the
contract and the pure packers that implement it — `MaxGoroutineEventBytes`
(2 MiB, scoped to `EventGoroutineSnapshot` + `EventGoroutines` only),
`MaxSnapshotGoroutines`, `MaxSnapshotThreads`, `MinThreadsRetained`,
`MaxGoroutineStringLength`, `MaxLifecycleDeltaIDs`, `SnapshotTotals`, and
`PackSnapshot`/`PackGoroutines` in
[goroutinepack.go](pkg/protocol/goroutinepack.go).

**Nothing calls them yet, and that is deliberate.** `Version` stays **1.3**,
`Totals` is never populated, and no client enforces the budget. A version bump or
consumer enforcement landing ahead of the producer would be worse than the bug:
the server would advertise a contract it still violates, and a conforming client
would reject perfectly valid output — permanently, if it treated the violation as
deterministic. Bump the version, wire both producers, and activate consumer
enforcement **in one change**; a dormant contract is safe to merge, a half-active
one is not. There is deliberately **no** generic hub cap, no client read cap, no
`Location` truncation, no chunking, and no compression.

What the packers guarantee once wired: exact byte accounting from a real marshal
of the real `Event` at `Seq = math.MaxUint64` (the hub re-stamps seq, so anything
narrower under-counts), with each element charged its standalone marshal length —
exact because JSON arrays are additive. Deterministic selection: current
goroutine, its whole ancestor chain, then ascending goid; current thread, then
ascending MID. Those anchors are required (the anchor *count* is checked before
any is placed, so a spawn chain longer than the element cap degrades rather than
overflowing it); `Created`/`Exited` are never trimmed; an oversized non-anchor is
skipped and packing continues; strings are capped in **UTF-16 code units** to
match a JavaScript consumer exactly and an offending element is dropped whole,
never truncated. `Current` is zero or delivered — a degraded result names no
goroutine it did not send. `SnapshotTotals` reports the ORIGINAL counts and, per
collection, whether that scan was clipped, so a consumer can distinguish "this
event is partial" from "the debugger's own walk stopped early".

**Streaming cadence (the load-bearing invariant).** `EventGoroutineSnapshot` is
auto-emitted on exactly the suspends that can change the concurrency picture —
**breakpoint hit, pause, and the launch/attach entry stop** — and on demand via
`CmdGoroutineSnapshot`. It is **NOT** emitted per step: `emitStepped` stays cheap
(walks frames for the stopped TID but performs no `runtime.allgs`, `runtime.allm`,
or current-g metadata reads, and reports synthetic ID 0) to protect the fragile
single-step/step-over path from per-step runtime inspection. `emitBreakpointHit`
/ `emitPaused` build the snapshot **once**, embed its current goroutine in the
stop event, then stream the same snapshot — one build, no double scan, no double
delta pass. Because the event is dual-purpose (push *and* query answer), the
SDK's request is fire-and-forget — `Client.RequestGoroutineSnapshot`, never a
`sendAndWait` — and only the automatic ones carry lifecycle deltas. The one
`EventPaused` that carries **no** snapshot is the internal
async halt (`emitHaltedOnError`, see [step-over flow](#software-breakpoint-step-over-flow)):
it is emitted on a backend error path, where a snapshot would push dozens of
further reads through the backend that just failed. Observers keep their last
good model; the halt is still reported, and `CmdGoroutineSnapshot` can refresh
on demand.

**Not a suspending event.** It follows a suspending event (or answers a query)
and never gates the hub. DAP `translateEvent` **deliberately ignores** it:
translating it would corrupt the FIFO that correlates a DAP `threads` request to
`EventGoroutines` (snapshots are unsolicited, with no matching request). DAP
clients get goroutine data from the `threads` request (`EventGoroutines`, which
now returns the rich list); the snapshot stream is WebSocket-only.

**Observers + runbook.** The VS Code extension's Bingo Concurrency view is the
graphical consumer; it joins from the DAP discovery event without user input.
[cmd/wsmon](cmd/wsmon/) is the terminal fallback and joins with `-session`.
Both are read-only and render the spawn tree / thread set / lifecycle deltas
from this stream (a UI-agnostic view of exactly the data a spawn-hierarchy
visualization needs). The end-to-end
DAP-drives + WS-observes walkthrough — server, VS Code (or `cmd/dapcli`) driver,
and either observer against the [examples/spawntree](examples/spawntree/)
target — is in [docs/ConcurrencyTelemetry.md](docs/ConcurrencyTelemetry.md).

**Lifecycle deltas.** `engine.prevGoids` (loop-thread-only, no lock, like
`manualStopPending`) remembers the previous live goid set; `diffGoids` returns
created/exited and adopts the new set. First snapshot returns nil deltas (a fresh
session must not report every goroutine as "created"). A **degraded** snapshot
(runtime unreadable — e.g. the pre-init entry stop) does **not** touch
`prevGoids`: an empty read must not look like every goroutine exited. This holds
for both automatic and on-demand snapshots: neither kind may clear or adopt a
baseline from an incomplete walk.

A current goroutine anchored from beyond the capped rich scan is also excluded
from this baseline: it is present for stop identity, but switching between two
still-live tail goroutines must not fabricate created/exited deltas.

**Automatic snapshots alone own the baseline.** `prevGoids` is the *only*
lifecycle memory, so whoever advances it decides what the next automatic
snapshot can report. The on-demand `CmdGoroutineSnapshot` path is therefore a
pure observation: `engine.GoroutineSnapshot` calls `goroutineSnapshotQuery`
(`buildSnapshot(false)`), which reports the same live picture with **empty
Created/Exited** and leaves `prevGoids` untouched; only `goroutineSnapshot`
(`buildSnapshot(true)`, used by `emitBreakpointHit` / `emitPaused` /
`emitGoroutineSnapshot` at the entry stop) diffs and adopts. `snapshotFrom` is
the single seam where the two differ. Before this split a manual refresh
consumed the pending deltas — the following automatic snapshot diffed against
the query, so goroutines created and exited in between appeared in neither
(issue #187). Do not add a delta-bearing query path back: the wire payload is
unchanged, and a client that wants deltas must read the automatic stream.

The regression gate is the `concurrency`-labelled `declareBaselineOwnershipSpec`
E2E, and it is deliberately shaped: a query at an ordinary breakpoint stop sees
exactly the live set that stop's automatic snapshot already adopted, so adopting
again would be a no-op and prove nothing. The spec instead **steps over a `go`
statement** — steps are the one suspend that carries no automatic snapshot — so
the query observes a goroutine the baseline has never seen, then asserts the
NEXT automatic snapshot still reports it as created. It fails if the query is
made lifecycle-tracking or the automatic path non-tracking; the unit tests
around `snapshotFrom` pin the seam's semantics but cannot catch a rewiring.

**Graceful fallback.** Every read is best-effort. `resolveGoLayout` marks the
layout invalid if any required `g`/`gobuf`/`stack`/`m` offset is missing, and any
unreadable `allgs` header/slot, required `g` status/goid/stack bound, stack-only
tail entry, or rooted exact-M/allm link degrades the whole snapshot to one
synthetic unknown goroutine (`ID:0, Status:"unknown"`, current PC) rather than
returning a partial live set, changing `prevGoids`, or erroring the stop. The
arm64 X28 chain is different: it is a speculative ABI hint, not a runtime root,
so an unreadable or invalid candidate (`g`, `g.m`, `m.g0`, or `m.gsignal`) is a
complete miss and the reader falls through without discarding the rich set.
Both stack bounds are required for every included live goroutine: without them
SP containment cannot rule that goroutine in or out as the stopped one.
Intentional nil/dead/freelist entries remain filters, and optional metadata
remains best-effort. This distinction is load-bearing on Linux: ptrace stops
only the reporting thread, so sibling runtime mutations can race the walk; only
backends that stop the world make the reads race-free. If every required read
succeeds but the bounded current lookup misses, `Current` remains 0 and the stop
event embeds the same ID-0 unknown — `currentGoroutineFrom` must never borrow the
first real goroutine. This preserves honest behavior for stripped binaries,
attach-without-DWARF, pre-runtime-init entry stops, zeroed/unreadable registers,
and scheduler-stack stops whose `m.curg` cannot be resolved.

**Coherent metadata reads — the half degradation can't cover.** Degradation
answers reads that *fail*. It cannot answer reads that all *succeed* while
being mutually inconsistent, which is the other shape of the same Linux race.
`allgsMetadata` (`goroutines.go`) prevents that shape instead of detecting it,
and its read **order** is the whole mechanism:

- `runtime.allgadd` publishes the array pointer **before** the length. Reading
  the pointer first can therefore pair a stale array with its larger
  successor's length and walk past the end of the old allocation. The word
  after a heap object is mapped, so that overrun **succeeds** — the walk
  reports `Complete: true`, and an unrelated heap object is accepted as a
  goroutine, fabricating a `Created` and then an `Exited` for a goroutine that
  never existed.
- Read the **length first, the pointer second**. This is sound only because
  `runtime.allgs` is **append-only and never shrinks**, so a length observed
  before a pointer always fits inside whichever array that pointer names. Do
  not "simplify" this ordering, and do not assume a wider single read fixes it:
  `process_vm_readv` takes no lock and is not atomic against the writer's
  separate word stores.
- Prefer `runtime.allglen` + `runtime.allgptr`, the runtime's own atomics,
  which carry the documented contract ("allgptr is updated before allglen.
  Readers should read allglen before allgptr"). The raw `runtime.allgs` header
  is a fallback for images lacking those symbols and uses the same
  length-before-pointer order, but its three words are ordinary
  compiler-scheduled stores, so it is best-effort where the mirror is
  guaranteed.

Worst case under the correct order is missing a goroutine created during the
read, which the runtime explicitly sanctions for lock-free readers. See #235.

**Current goroutine discovery is independent and bounded.** SP containment
(`g.stack.lo <= stopped SP < g.stack.hi`) identifies ordinary user-stack stops
inside the `maxGoroutineScan=8192` rich `runtime.allgs` prefix. If that misses:

1. On arm64, X28 provides the stopped `*g`. A user `g` is validated by SP
   containment; if X28 names g0/gsignal instead, the reader follows
   `g.m → m.curg`, verifies `curg.m == m`, and marks that live positive-goid
   goroutine current without requiring its stack to contain the scheduler SP.
   Any unreadable or invalid link in this speculative chain is a miss, not
   snapshot degradation.
2. On linux/amd64, `FS_BASE` is not a stable `*g` address under external
   linking. Linux ptrace TIDs and `runtime.m.procid` are the same kernel TID, so
   the reader searches for the exact stopped M, then resolves `m.curg` as above.
   The ordinary `allm` window remains `maxThreadScan=2048`; one additional
   targeted-only 2048-M continuation reads only `procid`/`alllink` until the
   match, for a strict maximum of 4096 inspected M nodes. Darwin never compares
   its Mach thread port to `m.procid` (a pthread).
3. A final stack-only pass examines at most one additional
   `maxGoroutineScan` `allgs` range, reading pointer + stack bounds until a
   match. Together with the rich prefix, no more than 16,384 allgs slots are
   inspected.

A current goroutine already in the rich prefix is replaced in place; a
beyond-prefix match is fully decoded once and prepended as an anchor excluded
from lifecycle deltas. `readThreads` likewise keeps its rich prefix capped and
may append exactly one current-M anchor found by a bounded goid-only
continuation, so thread/current identity stays coherent without unbounded
latency. On Linux, a concurrent runtime mutation or M transition can make a
required exact-M read inconsistent because only the reporting TID is stopped;
that case deliberately degrades instead of reporting a potentially wrong goid.
If the exact M lies beyond the 4096-node bound and SP containment cannot identify
a user stack (for example a g0 stop), identity remains unknown rather than
guessed.
A non-current goroutine's `CurrentLoc` uses `gobuf.pc` (where it resumes); the
current one uses the live PC. Status strings are hardcoded (stable across Go
versions); wait-reason strings are read dynamically from
`runtime.waitReasonStrings`. Goroutines with goid<=0 or status `_Gdead` (scan
bit stripped) are filtered out — their exit surfaces in the next Exited delta.

**Note on `go f(args)` wrappers.** A goroutine started with arguments gets a
compiler-generated `<caller>.gowrapN` closure as its startpc, so `StartLoc`
resolves to the wrapper, not `f`. Argument-less `go f()` points startpc straight
at `f`. `CreatedLoc` (the `go` statement site) and `ParentID` are unaffected —
they're the robust identifiers for a spawn tree.

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

## Hub debugger ownership — shutdown is a linearization point

`internal/hub.Hub` guards both `dbg` and `closing` with `dbgMu`. A factory-created
debugger is not owned by the hub until `installDebugger` accepts it under that
lock. Shutdown takes the same lock, marks ownership closed, and removes the
currently installed debugger atomically; it calls `Kill` only after releasing
the lock. If shutdown wins, installation is rejected and the command path must
discard the candidate debugger itself, then return without changing session
state or broadcasting. If installation wins, shutdown removes and tears down
that debugger. This covers both the initial Launch/Attach factory gap and
Restart's longer constructor/relaunch gap, so no live tracee can appear after
the hub and session have already exited.

Never hold `dbgMu` across a `Debugger` method or socket I/O. Run-loop reads take
a short snapshot via `currentDebugger`; teardown paths detach ownership under
the lock and perform the idempotent `Kill` outside it. State transitions also
check `closing` under `dbgMu`, preventing a command already in flight from
resurrecting a session after registry teardown begins.

**A candidate must be disposed of on EVERY exit, not just the shutdown race.**
Being caller-owned cuts both ways: while a candidate sits outside `h.dbg`,
shutdown's snapshot cannot see it and `Run` never selects on its events, so if
the command path abandons it *nothing else ever will*. `debugger.New` is not a
cheap value — `newEngine` starts `go e.loop()` immediately, that loop
`runtime.LockOSThread()`s, and its `defer` is the only thing that closes
`done`/`events` and calls `closeTracer()`; on linux `newBackend` also spins up a
tracer thread at construction. **Only `Kill` drives that loop to exit**, so a
dropped candidate permanently strands a goroutine plus a locked OS thread per
attempt (issue #188: repeated failed Restarts accumulated them). `handleRestart`
therefore registers a deferred `discardDebugger` the moment the candidate
exists and clears it at the single ownership-transfer point — immediately after
`installDebugger` accepts it — so relaunch failure, install rejection, and any
future early return all dispose exactly once, and a successful restart's now
hub-owned debugger is never killed by the caller. Disposal targets the captured
candidate by identity, so it can never kill a newer or currently installed
debugger.

`discardDebugger` is a `Hub` method because the hub is the last owner of a
discarded debugger: a failing `Kill` is logged there (the owning top level, per
[docs/ErrorHandling.md](docs/ErrorHandling.md)) rather than returned, so cleanup
never replaces the original launch error the client is told about.

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
- `h.bps *breakpointIDs` — the logical-id table (see
  [Breakpoint identity](#breakpoint-identity--hub-owned-logical-ids)). Restart
  `reset`s it, then walks the `sortedRestartTargets()` snapshot (the
  `installedLogical()` set, sorted for determinism) taken *before* the reset,
  re-`SetBreakpoint`s each retained
  location on the new `Debugger` (which re-resolves `file:line` through DWARF
  against the new process image — addresses aren't reused directly since a
  relaunch can shift the load address), `bind`s the logical id to the fresh
  physical id, and reports the **same logical ids** in `RestartedPayload`.
  Locations that no longer resolve lose their mapping and are reported in
  `Discarded`.

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
resume after executing a non-resuming command, and it leaves only when the
command moved the session out of `suspended` — `h.State() != StateSuspended`,
the same guard the `resumeCh` branch uses. A **successful** `CmdRestart` hits
that (it transitions to `running`, or to `idle` if the relaunch failed), so
Run's outer loop naturally picks up the new debugger's events channel (`h.dbg`
is reassigned inside `handleRestart` before the confirmation event is
broadcast). A `CmdRestart` **rejected** before it touches the debugger, or a
failed `CmdKill`, leaves the original process suspended and the loop keeps
waiting — keying the exit off the command kind instead wedged those sessions,
see [Suspend/resume protocol](#suspendresume-protocol).

`EventRestarted` is a confirmation event (like `BreakpointSet`), not a
suspending one — the new process's suspended state (if any, e.g. break-on-
entry) is reported the normal way via `EventStepped`/`EventBreakpointHit` once
the relaunched process actually reaches that point.

**Failed relaunch**: the old debugger is already dead by then, so the session
falls back to `idle` with no debugger installed and broadcasts an `EventError`
wrapping the cause as `restart: relaunch failed: …`. `lastLaunch` and
`restartBreakpoints` are deliberately **kept**, so a retry Restart (or a fresh
Launch) still works. The replacement that failed to launch is killed by
`handleRestart` itself — see the candidate-disposal rule in
[Hub debugger ownership](#hub-debugger-ownership--shutdown-is-a-linearization-point).

**A failed breakpoint reinstall is NOT a failed relaunch** — do not extend the
disposal rule to it. By the time the reinstall loop runs, `installDebugger` has
already accepted the replacement, so it is hub-owned and the tracee is running;
a `SetBreakpoint` error is collected as a `DiscardedBreakpoint` and reported in
`EventRestarted` (mirroring Delve), and the session stays `running`. Killing the
replacement there would terminate a healthy process over one unresolvable
`file:line`. Only locations that *did* resolve are carried into
`restartBreakpoints`. Both directions are pinned by the `Restart relaunch
failure` and `Restart breakpoint reinstall failure` specs in
[hub_test.go](internal/hub/hub_test.go).

## Breakpoint identity — hub-owned logical ids

Source: [internal/hub/breakpoints.go](internal/hub/breakpoints.go).

**Client-visible breakpoint ids are hub-owned logical ids, never raw engine
ids.** `breakpointTable.nextID` (`internal/debugger/breakpoint.go`) restarts at
1 in every engine, so a physical id only means anything to the engine that
issued it — and Restart replaces that engine. A command can be *generated*
against one engine and *injected* after it has been replaced: the DAP adapter
marshals a `ClearBreakpoint` and its hub read pump is descheduled between
`Handler.ReadMessage` and `injectCommand`
([internal/hub/client.go](internal/hub/client.go)) while another client
completes a Restart. The stale physical id then names a *different* breakpoint
in the fresh process and disarms the wrong trap (issue #200).

An epoch stamped at `injectCommand` is **provably insufficient** — provenance is
bound when the Handler generates the bytes, but injection happens after the
Restart and would read the new epoch. So the hub owns identity instead:

- `breakpointIDs.next` is monotonic for the **whole hub lifetime**. `reset()`
  (a fresh `Launch`/`Attach`, and the engine swap inside `Restart`) drops the
  mappings but deliberately does **not**
  rewind the high-water mark; re-minting is exactly what would let a delayed
  command from a previous target alias a breakpoint in the new one.
- `byLogical` maps logical → `{physicalID, loc}`; `byPhysical` is the reverse
  lookup engine-generated events need. `dropPhysical` only deletes a reverse
  entry while it still points at the given logical id, so re-homing on Restart
  cannot clobber another breakpoint's lookup.
- `CmdSetBreakpoint` installs, then mints a logical id and broadcasts
  `EventBreakpointSet` with it (`Hub.setBreakpoint`).
- `CmdClearBreakpoint` (`Hub.clearBreakpoint`) rejects an unknown logical id
  **without touching the debugger** — that is the load-bearing half of the fix —
  translates a known one to the active physical id, and only `untrack`s after
  the debugger confirms. **No optimistic deletion:** a rejected clear leaves the
  trap armed (`breakpointTable.clear` keeps its entry when the memory write
  fails), so the client must keep being able to name it.
- `Hub.localizeBreakpointIDs` rewrites the physical id in an engine-generated
  `EventBreakpointHit` to the logical id before broadcast. It runs on both
  broadcast paths in `handleEvent` (the initial event and the suspended
  wait-loop's `nextEvt`). The step-over/step-out sentinels (`<stepover-next>`,
  `<stepout-return>`) report `EventStepped` and carry no breakpoint id, so they
  are unaffected; the test-only `<direct-addr>` sentinel does emit
  `EventBreakpointHit` and is translated like any other.
- A hit can **race its own clear**, and a cleared breakpoint can be **resurrected
  by the engine**. The engine emits into a buffered channel and
  `engine.ClearBreakpoint` has no suspend guard, so `Run`'s `select` may execute
  a queued clear before draining a hit that was generated first; separately,
  clearing the breakpoint the tracee is *parked on* is re-armed under the **same
  physical id** by the step-off path (`resumeFromBreakpoint` replays the
  `e.lastBP` it stashed at hit time rather than re-reading the table, and
  `breakpointTable.reinstall` re-adds the entry under its original id). `untrack`
  therefore `retire`s the pair into a **bidirectional tombstone**:
  `retiredLogical` (physical→logical) lets `logicalFor` report a late or
  resurrected hit under the id the client held instead of a number it was never
  told about, and `retiredPhysical` (logical→physical) lets `physicalForClear`
  keep honouring a `CmdClearBreakpoint` for that id. Without the second
  direction a resurrected trap is reported under a logical id that
  `CmdClearBreakpoint` then rejects (it is no longer in `byLogical`) *and*
  `CmdSetBreakpoint` cannot replace (the engine still holds the address, so it
  fails `errBreakpointExists`) — a phantom breakpoint that is permanently
  neither removable nor settable. A tombstone-clear is deliberately **not**
  consumed (`untrack` no-ops on an already-retired id) because the tracee may
  still be parked there and resurrect it again.
  The record is per-engine and bounded
  by `retiredCap`; only a very recent retirement can still be in flight. It is
  safe precisely because `breakpointTable.set` never reuses a physical id within
  one engine, and because `reset()` purges it on every Launch/Attach/Restart — a
  tombstone that outlived its engine would reintroduce exactly the #200 alias.
- A physical id that is neither live nor retired is **adopted** rather than
  passed through (`logicalFor`): passing it through could collide with a logical
  id naming a different breakpoint. This keeps raw `hub.New` hubs (tests /
  single-session, where a debugger may be driven directly) coherent. An adopted
  mapping is **not** `installed`, so `installedLogical` excludes it from
  `sortedRestartTargets` — the hub never armed it and must not arm it on the
  replacement process.
- A fresh out-of-band `Launch`/`Attach` (a *second* client relaunching a session
  another client is driving) invalidates every logical id: the mappings are
  reset while the high-water mark advances, so an id minted against the previous
  target can never name a breakpoint in the new one. A client still holding old
  ids gets a clean `EventError` ("breakpoint N not found") on its next
  `CmdClearBreakpoint` instead of silently disarming a stranger's breakpoint —
  the pre-fix behaviour, where a raw physical id aliased whatever the fresh
  engine had compacted into that slot. It is therefore a strict improvement, not
  a regression, but it is **not** transparent: a DAP adapter whose `bpLine`s
  still record the pre-Launch ids believes lines are armed that the new process
  does not have, and only discovers it when a later removal fails. `CmdRestart`
  is the supported way to relaunch while *preserving* ids, and it is what the
  DAP `restart` request maps to; a client that drives `CmdLaunch` directly on a
  shared session owns the reconciliation. An explicit invalidation event is
  deliberately out of scope here — it is a new protocol surface, and #200 is
  about not corrupting the ids that *do* survive.

Every protocol surface carrying a breakpoint id is covered:
`BreakpointSetPayload.Breakpoint.ID`, `BreakpointClearedPayload.ID`,
`BreakpointHitPayload.Breakpoint.ID`, `RestartedPayload.Breakpoints[].ID`, and
the inbound `ClearBreakpointPayload.ID`. Because breakpoint ids cross this
translation boundary, `CmdSetBreakpoint`/`CmdClearBreakpoint` are **absent from
the generic `dispatch`** in [dispatcher.go](internal/hub/dispatcher.go) —
`Hub.dispatchCommand` routes them to the hub methods, and reaching `dispatch`
with one means routing was bypassed, so it errors rather than handing a
client-supplied id to the engine.

The wire shape is unchanged (ids were already opaque ints); only ownership and
lifetime changed, so `protocol.Version` stays at 1.2. On the DAP side this makes
the positional `setQ`/`clearQ` FIFOs correlate confirmations that match the
actual physical effect, and `reconcileRestartBreakpointsLocked` a self-consistent
re-identification.

Regression coverage:
[internal/dap/hublogicalbp_test.go](internal/dap/hublogicalbp_test.go) drives a
**real** `hub.Hub` + **real** `dap.Handler` with a gating `hub.WSConn`
interposed at the `ReadMessage` → read-pump boundary; it holds a generated
clear, restarts via a second client, releases it, and asserts the correct
physical breakpoint was disarmed (plus an in-order negative control). The held
request runs on its own goroutine and **must** be joined before the test
returns — it reports through `t`, which panics the package once the test has
completed. [internal/hub/breakpoints_test.go](internal/hub/breakpoints_test.go)
covers the tombstone from both ends: a step-off-resurrected breakpoint stays
clearable under the id the client holds, and a retired id never reaches a
replacement engine.

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
3. `StopProcess()` triggers the backend's platform interrupt. On **linux** it
   sends `PauseSignal()` = `SIGSTOP` (a real signal). On **darwin** it sends
   nothing to the tracee — it posts an empty Mach message to a private control
   port that is in `Wait`'s receive set (`bingo_send_ctrl`), because a
   `task_suspend`/`thread_suspend` cannot wake a thread blocked in `mach_msg`.
   Either way the interrupt surfaces from `Backend.Wait()` as
   `StopEvent{Reason: StopSignal, Signal: PauseSignal()}`, so detection lives
   entirely in the engine's `handleStop` `StopSignal` branch. `PauseSignal()`
   returns `SIGSTOP` on linux and `SIGUSR2` on darwin, but on darwin that value
   is only a **sentinel** the engine matches against — no `SIGUSR2` is ever
   sent.
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

**Darwin: a control-port wake + `stopTheWorld`, no signal at all.** darwin
`StopProcess()`
([backend_darwin_arm64.go](internal/debugger/backend_darwin_arm64.go)) does
`bingo_send_ctrl(ctrlPort)` — a non-blocking `mach_msg` send to a private port
folded into `Wait`'s receive set. `Wait` sees the wake as `BINGO_MSG_PAUSE`,
runs `stopTheWorld` (suspend every thread + drain queued exceptions), and
returns `StopEvent{StopSignal, PauseSignal()}` with `PauseSignal()` = `SIGUSR2`
as a pure engine sentinel — no signal is delivered to the tracee. A Mach send is
used rather than `task_suspend`/`thread_suspend` directly because those cannot
wake a thread blocked in `mach_msg`; the send both wakes `Wait` and lets it do
the suspend on the serialized loop. Because no signal is injected and no whole-
group stop is created, **resume-after-pause is a plain `ContinueProcess`** —
identical to resuming from a breakpoint, no special handling. With async
preemption RE-ENABLED (#92) the tracee does generate `SIGURG`, but those are
delivered natively to the correct M and never intercepted, so they do not
interfere with the pause round-trip. This mirrors Delve's *intent* (an on-demand
halt surfaced to the receive loop); bingo's partial-stop model only needs the
reporting path suspended, so it does not replicate Delve's per-thread
`thread_suspend` + atomic halt-flag machinery.

**Resume-after-pause is a plain Continue.** On linux bingo never *injects* the
interrupt signal (`Continue` does `PtraceCont(tid, 0)` with signal 0, which
suppresses the pending SIGSTOP); on darwin no signal was ever sent. Either way
resuming is identical to resuming from a breakpoint. The `pause`-labelled E2E
spec (`declarePauseSpec`,
[debugger_e2e_common_test.go](test/integration/debugger_e2e_common_test.go),
wired into **both** the linux and darwin containers) runs the
Continue→Pause→Paused round-trip several times; if the first resume hung, the
second Pause's wake would never surface and the spec would time out.

`StopProcess()` is idempotent: linux guards `pid == 0` and treats `ESRCH`
(process already gone) as a no-op success; darwin tolerates `MACH_SEND_TIMED_OUT`
(the receive loop is momentarily not blocked) as success. Delve's manual-stop is
heavier (Linux: `sys.Kill(pid, SIGTRAP)` + a `trapWaitInternal` halt-flag state
machine; Darwin: Mach `thread_suspend` on every thread + an atomic halt flag)
because it lands *every* thread at a consistent stop point; bingo's model stops
the world in `Wait` and reports the pause, which is all the engine needs.

## DAP — Debug Adapter Protocol alongside WebSocket

Source: [internal/dap/](internal/dap/). Wired via
[internal/server/dap.go](internal/server/dap.go) + the `-dap-addr` flag in
[cmd/bingo/main.go](cmd/bingo/main.go).

**What it is / why.** DAP is the IDE-facing debugger wire protocol (VS Code,
neovim). bingo speaks it *alongside* the native WebSocket protocol so a standard
editor can DRIVE a session (breakpoints, stepping, stack/vars, continue/pause)
while any number of WebSocket clients OBSERVE — and can also drive — the SAME
session in parallel. DAP is the least-common-denominator debug loop; bingo's
richer concurrency visualizations stay on the WebSocket side. The two coexist on
one hub session (this is the whole point — an IDE gets a working debugger, the
bingo UI gets its bonus features, both against the same tracee).

**VS Code companion — managed transport, separate from Go tooling.**
[editors/vscode](editors/vscode/) packages as extension ID
`bingosuite.bingo`, registers a
`DebugAdapterDescriptorFactory` for debugger type `bingo`. The async factory
first ensures a compatible server, then returns
`DebugAdapterServer(dapPort, dapHost)` (defaults `127.0.0.1:4711`). It never
registers type `go`, launches or validates `dlv`, or calls into Microsoft's Go
extension. Keep `golang.go` installed for gopls/navigation/formatting/tests; a
`"type": "bingo"` launch is owned entirely by the companion and this DAP server.
The explicit IPv4 default matches `internal/dap/server.go`'s `tcp4` listener;
do not change it to `localhost`, which older VS Code/Node runtimes can resolve
to `::1` without falling back to IPv4.
The extension validates launch (`program`), existing-session join (`session`),
and OS-process attach (`pid`, optional `binaryPath`) before connecting.
`serverMode`/management/DAP/timing fields are client-owned and remain in VS
Code's raw launch/attach arguments; Go's JSON decoder ignores those unknown
fields, so they never enter the bingo command payload. Do not add them to the
wire protocol or `launchConfig`.

**VS Code connect-or-start invariants.** Default `serverMode:"auto"` is local
only: management `127.0.0.1:6060`, DAP `127.0.0.1:4711`, readiness 5s, managed
idle grace 30s. It health-checks before spawning and requires
`service:"bingo"`, management API 1, the exact wire version, enabled DAP,
`dap.sessionEventVersion:1`, and
the expected DAP port/host (wildcard advertised hosts retain the configured
connect host). Only connection refusal permits spawning; a non-bingo or
incompatible occupant fails safely. `connectOnly` bypasses management and spawn
for remote/custom workflows. Health reads have both response abort/error
handling and an independent wall-clock deadline; readiness probes receive only
the remaining overall budget, so a slow-drip HTTP peer cannot hold F5 open. The
poller probes immediately after spawn, uses the normal 100ms cadence outside
the last 50ms, then a 10ms cadence while every request can retain at least a
25ms wall-clock budget (covered by a real localhost test). When another useful
probe no longer fits it waits out the absolute deadline rather than issuing a
sub-millisecond request or spinning.

One extension host coalesces in-flight ensures by normalized endpoint. Across
hosts, listener binding arbitrates races: a child that loses is success if the
compatible winner becomes healthy before the deadline. The bundled child is
spawned with argv (never a shell), detached/unref'd with ignored stdin and
stdout/stderr inherited from a persistent extension-storage log file. The
extension NEVER kills a server, including one it spawned; disposal only aborts
bounded probes. Cancellation is rechecked after every awaited binary/log
prerequisite and immediately before spawn, because a child started after
deactivation cannot be reclaimed without violating the never-kill contract.
Server-owned `-idle-timeout` is the sole teardown owner.

VSIXes are platform-specific: `linux-x64` contains only linux/amd64 bingo;
`darwin-arm64` contains only a `bingonative` arm64 binary codesigned with
[entitlements.plist](entitlements.plist). Runtime resolves only
`bin/bingo` + `bin/target.json` inside the installed/development extension,
checks the target, and repairs executable mode if extraction lost it.
Packaging rebuilds/signs twice and requires identical binary and VSIX hashes;
tests drift-check service/API/wire constants against Go source and inspect exact
archive contents (one native binary plus the two bundles and original icon),
target metadata, architecture, mode, and entitlements.
The extension package version is the installed-runtime upgrade boundary:
material shipped behavior changes must bump both `package.json` and the lockfile
or VS Code can retain an older bundle under the same identity. The manifest test
and package verifier pin the current version (**0.3.1**) in source and VSIX
metadata.
The root Run and Debug dropdown exposes exactly two `"type":"bingo"` choices:
launch one of five progressive examples through a `pickString`, and join a
running session. Normal F5 uses the installed VSIX and rebuilds the five targets
with `just build-examples`; contributor source-extension development runs
`just vscode-dev` and launches an Extension Development Host explicitly from
the CLI with
`code --new-window --extensionDevelopmentPath="$PWD/editors/vscode" "$PWD"`,
outside the root launch configurations. The recipe restores the exact npm
lockfile with lifecycle scripts disabled before building. The macOS packaging
job uses the supported `macos-15` arm64 image, asserts `uname -m`, and runs the
real packaged-server smoke. Both package matrix legs run the pinned Electron
activation/view/custom-event acknowledgement test; linux additionally runs the
real packaged DAP→WebSocket graphical-model E2E. Darwin native-debug execution
requires local/self-hosted Apple Silicon, where the same E2E covers all five
examples. Both tests observe server-owned idle exit on success. On failure the
smoke may SIGKILL only the detached process group it created; the packaged E2E
may signal only its exact captured server PID. Cleanup is test-only and must
never enter extension production code.
Apple's external linker can vary `LC_UUID` even for identical cgo inputs, but
current dyld rejects binaries with `-no_uuid`; `normalize-mach-o-uuid.mjs`
therefore derives a stable UUID from the unsigned Mach-O with its UUID and
linker signature zeroed, writes it back, then codesigns. Preserve that
normalize-before-sign order and the repeated two-build gate.

**Architecture — a translator at the `hub.WSConn` seam, ZERO hub changes.**
`dap.Handler` implements `hub.WSConn` and is registered via
`Session.AddClient(handler, log)`, so from the hub's perspective the DAP client
is just another client: it reuses ALL hub fan-out / broadcast / seq-restamping /
slow-client eviction. There is deliberately no DAP-awareness anywhere in
`internal/hub` or `internal/debugger`.

- `WriteMessage(mt, data)` (called on the hub write-pump goroutine): the hub
  hands it a marshalled bingo `Event`; the Handler `UnmarshalEvent`s and
  translates to DAP. Always returns nil so the hub never treats a slow/broken
  DAP socket as a failed writer — the socket lifecycle is owned by `Serve`/`Close`.
- `ReadMessage()` (called on the hub read-pump goroutine): blocks on an internal
  `cmdOut chan []byte` of marshalled bingo `Command`s produced by the DAP read
  loop; returns `io.EOF` on Close. **cmdOut-priority:** it probes `cmdOut` before
  the `{cmdOut | done}` select and, if `done` wins, probes `cmdOut` once more
  before returning EOF. The final drain is load-bearing: disconnect enqueues
  `Kill` before writing its response (which can call `Close` on failure) and
  before its explicit `Close`, but both channels can become ready between the
  first probe and the blocking select. Without the re-probe, random select
  choice can drop the queued kill while another observer keeps the session
  alive. The hub may backpressure this read pump while its ordinary-command
  queue is full; `cmdOut` stays bounded, and Handler `Close` unblocks both
  `ReadMessage` and a DAP producer waiting to enqueue.
- `Serve()` (its own goroutine, one per connection): the DAP read loop —
  `godap.ReadProtocolMessage` → `dispatchRequest`. Runs the handshake state
  machine, enqueues bingo commands, and answers non-hub requests directly.

**Three goroutines touch a Handler** — `Serve` (socket reads), the hub read pump
(`ReadMessage`), the hub write pump (`WriteMessage`→`translateEvent`). `mu`
guards coordination state; `writeMu` serialises socket writes + the DAP `seq`.
**Rule: never hold `mu` across a socket write or a `cmdOut` enqueue** (release
`mu`, then take `writeMu`). go-dap's `WriteProtocolMessage` does not set `Seq`,
so `send` stamps it via reflection (`setSeqField` walks anonymous-embedded
structs for the int `Seq`).

**Managed-session discovery event.** Immediately after `startSession` has
successfully attached the DAP handler to a created or joined managed session,
the adapter emits exactly one custom DAP event named `bingo/session/v1` with
body `{"version":1,"sessionId":"…"}` and preserves the console announcement.
It emits neither event on create/join failure. `sessionAnnounced` is claimed
under `mu`, but the event write occurs after unlocking, preserving the
no-lock-across-socket-write invariant. Go DAP clients must read through
`internal/dapclient`, which recognizes this namespaced event before delegating
standard messages to go-dap; `cmd/dapcli` and the native E2E client share that
path. The VS
Code extension subscribes at
activation and keys observers by `DebugSession.id`, never
`activeDebugSession`.

**Concurrency webview ownership/security.** The extension host, not the
webview, owns one bounded WebSocket observer/model per live DAP session. It
reuses the normalized management endpoint, validates protocol 1.3 envelopes,
payload/string/count limits and seq gaps, and reconnects only while the DAP
session lives. It sends `CmdGoroutineSnapshot` once after every successful
WebSocket join and thereafter only for explicit Refresh—never run control.
The recreatable webview receives validated view models through a
ready/rendered-ack protocol. Every document has a generation token; async
`postMessage` completions may mutate delivery state only while their captured
view and generation are current, even when an old and new render share a
revision. Rendered acknowledgements echo both generation and revision, so a
destroyed document cannot acknowledge its replacement. A fresh `ready` resets
any in-flight revision
because a hidden non-retained webview may have discarded its prior delivery;
otherwise the host can wait forever for an acknowledgement from a dead document.
Preserve the strict nonce CSP, `dist`-only `localResourceRoots`,
DOM/textContent rendering, deterministic capped cycle-safe tree, bounded
lifecycle history, and multi-session selector. Filtering searches the full
validated snapshot (up to the scanner's 8,193-entry rich-prefix-plus-anchor
bound) before applying
the 500-node rendering cap, re-lays out each match with at most four ancestors,
and resets fit so a deep or previously capped match cannot remain invisible.
Empty results keep Fit/zoom callbacks safe even without an SVG scene. SVG
treeitems carry `aria-level`, sibling position/size, selection, and parent
context; arrow navigation moves DOM focus with selection.

### Handshake (Delve-style, VS Code-compatible)

1. `initialize` → `Capabilities` (ConfigurationDone/Terminate/Restart +
   TerminateDebuggee + `SupportsEvaluateForHovers`). NO `initialized` event yet.
   Only capabilities bingo actually implements are advertised — `evaluate`
   (name-only, see below) backs the hover cap.
2. `launch`/`attach` → `startSession` (`CreateSession` + `AddClient(self)`)
   **then** enqueue `CmdLaunch`/`CmdAttach`; set `launching=true`. Registering as
   a client BEFORE enqueuing the launch is what guarantees we receive the entry
   stop. An `EventError(Launch/Attach)` during `launching` → error the start
   request + `terminated` (`failStart`).
3. The entry stop is an **`EventStepped`** (engine's `Launch`/`Attach` both call
   `emitStoppedAtCurrentPC`). While `launching`, `onStop` fires the `initialized`
   event (breakpoints can now resolve against the loaded image), flips
   `launching→false`, `suspended=true`, and withholds the launch response and any
   `stopped`.
4. `setBreakpoints` (suspended at entry) → diff/FIFO (below).
5. `configurationDone` → respond, send the DELAYED launch/attach response, then
   if `stopOnEntry` send `stopped reason=entry`, else enqueue `CmdContinue`
   (`pendingContinues++`). Restart reuses a `restarting` flag so the post-restart
   entry `EventStepped` is treated as a fresh entry.

`restartReqSeq` separately gates only an unanswered DAP restart. A second
request before `EventRestarted`/`EventError` is rejected without enqueueing
another destructive `CmdRestart`; once the response is sent, a new restart is
allowed even if the prior process's entry stop has not arrived yet. In that
race, `onStop` suppresses the superseded entry while a newer `restartReqSeq` is
pending and preserves `restarting` for the replacement process. This relies on
the hub/client FIFO ordering each restart's `EventRestarted` before its own
entry `EventStepped`.

### Joining an existing session (no relaunch)

A DAP `attach` carrying a `session` argument and **no** `pid` means "join an
already-running bingo session as an ADDITIONAL client", not "attach to an OS
process". `onAttach` routes this to `onJoin` (`requests.go`). This is what backs
`cmd/dapcli -session <id>` and lets many DAP + WebSocket clients share one
session.

- `onJoin` registers as a hub client (`startSession(cfg.Session)` → `AddClient`)
  but enqueues **no** `CmdLaunch`/`CmdAttach` — the session is already running
  under other clients, so it must not disturb the shared run state.
- The join flags (`joining`, `awaitingWelcome`, `attached`, `startCmd="attach"`)
  are set BEFORE `startSession`, because the hub delivers its welcome
  `EventSessionState` the instant `AddClient` runs and `onSessionState` must see
  `awaitingWelcome=true` to translate it.
- `initialized` fires immediately (the target image is already loaded, so
  breakpoints resolve right away — there is no entry stop to wait for).
- `onSessionState` (`events.go`) consumes that welcome **once** (gated on
  `awaitingWelcome`): `suspended`→`suspended=true` + `stopped reason=pause`
  (`threadId` omitted until a stop identifies a positive goid);
  `exited`→`terminated`; idle/running→nothing. For the
  normal launch/attach path `awaitingWelcome` is never set, so it is a no-op
  there (the entry stop drives the initial state instead).
- `onConfigurationDone`'s `joining` branch responds to the attach but does NOT
  `pendingContinues++`, resume, or fabricate an entry stop.

### Event → DAP mapping

`EventStepped`(entry)=handshake signal; `EventStepped`(step)→`stopped`
reason=step; `EventBreakpointHit`→`stopped` reason=breakpoint;
`EventPanic`→reason=exception; `EventPaused`→reason=pause;
`EventProcessExited`→`exited`(code)+`terminated`; `EventOutput`→`output`;
`EventRestarted`→delayed `restart` response; `EventEvaluate`→`evaluate` response
(correlated via `evalQ`, NOT a stop — see below); `EventSessionState`→sends
nothing on the launch/attach path (only recorded as the session's lifecycle
state, which gates resume-rejection resync), but consumed **once** as the initial
state on the join path (see *Joining an existing session*);
`EventGoroutineSnapshot`→**deliberately
ignored** (WebSocket-only concurrency stream with no DAP equivalent; translating
it would corrupt the `threads`→`EventGoroutines` FIFO — see the goroutine
snapshot section).

Suspending events carry the runtime goid when it is known. An ID-0 synthetic
unknown omits DAP's optional `stopped.threadId`; never clamp an unresolved stop
to 1, because that identifies the unrelated real g1. `threads` responses may
still assign a synthetic entry a transport-only positive handle because DAP
requires every listed `Thread.id` to be non-zero. VS Code interprets an omitted
`stopped.threadId` as a request to fetch every returned thread's stack. To keep
the cheap synthetic step honest without amplifying one step into thousands of
identical `CmdFrames`, the first `threads` response after an unknown stop is
collapsed to exactly one entry: the current goroutine if that explicit
Goroutines query resolves it, otherwise a transport-only
`stopped goroutine (unknown)`. A resolved query restores normal full responses
for later requests. `stackTrace` returns the stopped stack only for that current
handle or a request that preserves the omitted/non-positive stop id, and empty
frames for every other positive thread id; bingo cannot unwind arbitrary
goroutines yet. `cmd/dapcli` therefore retains an omitted stop id as zero
instead of carrying a stale positive id into `stackTrace`.

`EventContinued` → DAP `continued` **only for out-of-band resumes**. The Handler
increments `pendingContinues` before enqueuing its OWN continue and decrements it
on the matching `EventContinued` (suppressing it — the IDE already implied
continuation via the continue/step response). A continue driven by a *different*
client on the session arrives with the counter at 0 and IS surfaced as
`continued`, so the IDE learns the tracee is running again. This is exactly why
the prerequisite PR made the engine emit `EventContinued` on resume.

**Rejected resumes must resynchronize the adapter (`failResume`/`failRestart`).**
`onContinue`/`onStep`/`onRestart` clear `suspended` *optimistically* and answer
their request `success` — DAP has no way to retract a successful `continue`/
`next` response. But the hub only transitions to `running` on success: a rejected
`Continue`/`StepOver`/`StepInto`/`StepOut`/`Restart` arrives as `EventError` with
the engine still `stateSuspended` (see [Suspend/resume
protocol](#suspendresume-protocol) → Rejected resumes). So `onError` MUST restore
`suspended = true` for every one of those command kinds — all three step kinds
handled explicitly, never left to the generic console `default`. Otherwise
`threads`/`stackTrace`/`variables`/`evaluate` take their not-suspended branch
forever, answering synthetically without reaching the hub, and the IDE shows a
running program that can never stop again (the routine trigger is `stepOut` at
the outermost frame, which `engine.stepOut` rejects without resuming or emitting
any stop).

A rejected Continue/Step also emits ONE `stopped` (reason `exception`, the
rejection text in `Text`, `AllThreadsStopped`, current thread) — the only DAP
message that can walk a client back after a successful resume response. Restart
does NOT: its delayed error response already reports the failure, so whatever
state the client was in remains current. Restart's restore is the **captured
pre-request view** (`restartWasSuspended`, taken in `onRestart` before the
optimistic clear), never an unconditional suspend: DAP permits restart while the
tracee is RUNNING, and the hub rejects a restart on an attach-created session
(no prior `Launch`) without touching that still-running process — asserting a
suspension there desynchronizes the adapter the *opposite* way, forwarding
inspection requests for a process that never stopped. The capture is consumed by
`failRestart` and cleared on success (`onRestarted`); an overlap rejection
returns before touching it, so the in-flight request keeps its own.

No resync happens once the session is **idle or exited** (`sessionEndedLocked`,
fed by `EventSessionState` on every connection plus `EventProcessExited`). The
hub's relaunch-failure path kills the old process, broadcasts the error, *then*
transitions the managed session to idle, and answers every later command with
"no active debugger" — reporting a stop there would leave the client stopped on
a process that no longer exists. Outside a joiner's welcome, `onSessionState`
only records that state (and drops a stale `suspended` on idle/exited); it sends
nothing, since process death is already reported by `EventProcessExited`.

Further invariants: the `pendingContinues` debt of a rejected Continue
is settled exactly once (its `EventContinued` never arrives); no second `stopped`
is emitted while the handler is already `suspended` (a real stop won the race) or
while the handshake still owes an initial state report — `launching` (the entry
stop) or a joiner's `awaitingWelcome` (the hub's welcome `EventSessionState`,
which another client's rejection can race between `AddClient` and delivery); and
recovery does NOT route through `onStop` — the process never moved, so the
current suspension's `varCache` stays valid and the `launching`/`restarting`
latches are untouched.

### Request → command + the FIFO-correlation limitation

`continue`→Continue; `next/stepIn/stepOut`→StepOver/Into/Out; `pause`→Pause;
`threads`→Goroutines; `stackTrace`→Frames; `scopes`→synthetic single "Locals"
scope whose `variablesReference` IS the frame id; `variables`→Locals(frameIndex)
for a frame-root ref, or a synchronous cache hit for a child ref (see below);
`evaluate`→Evaluate(frameIndex, name); `disconnect`/`terminate`→Kill;
`restart`→Restart. Data requests (threads/stackTrace/variables/evaluate) are only
enqueued while the Handler believes it is `suspended`; otherwise they return an
empty (best-effort) result rather than blocking.

`variablesReference = frameIndex+1`, `frameID = frameIndex+1` (both reversible
via `frameIndexFromRef`, both non-zero since DAP reserves 0). threads =
goroutines with id `max(id,1)`; empty list → synthetic `{1,"main"}`.

**Structured (expandable) variables — eager tree + `varCache`.** bingo now
computes a **bounded typed subtree** per local (children inline; see the DWARF
reader notes). `EventLocals`/`EventEvaluate` carry it. `buildVarTree`
(`translate.go`) walks that subtree and, for every node with children, allocates
a fresh `variablesReference` from `varRefBase` (`1<<16`) upward via `allocVarRef`
and caches the node's DAP children under it in `varCache` (`ref→[]godap.Variable`).
Child refs start above `varRefBase` so they never collide with a frame-root ref
(a scope's reference == `frameIndex+1`, bounded by the max stack depth), letting
`onVariables` tell them apart by magnitude: a ref in `varCache` is served
synchronously (no round-trip); anything else is a frame-root ref that enqueues
`CmdLocals`. The cache reflects ONE memory snapshot, so `resetVarsLocked` clears
`varCache`/`nextVarRef` on every stop (`onStop`) — a child ref from a prior
suspension is stale and must not expand.

**`evaluate` is name-only.** `onEvaluate` sends `CmdEvaluate{FrameIndex, Name}`
(Expression = the bare variable name; the `context` field — watch/hover/repl — is
handled uniformly/ignored). `onEvaluated` correlates the `EventEvaluate` via a new
`evalQ` FIFO, returns the value string + type, and — if the result has children —
allocates a child ref (same `varCache` machinery) so the client can expand it. A
general expression evaluator (arithmetic/dotted-path/indexing) is a LATER PR; this
is the eager-tree foundation that deliberately avoids needing a path parser.

**FIFO correlation — the key limitation.** bingo's confirmation events
(`EventBreakpointSet/Cleared`, `EventFrames`, `EventGoroutines`, `EventLocals`,
`EventEvaluate`) carry **no request/correlation id**. The Handler correlates each
incoming confirmation to the oldest outstanding DAP request of that kind via
per-kind FIFO queues (`setQ`/`clearQ`/`threadsQ`/`framesQ`/`localsQ`/`evalQ`),
relying on the hub's in-order event stream. **This is valid only while the DAP
client is the sole driver of breakpoints/data-requests on the session.** A
WebSocket client concurrently setting breakpoints or requesting frames/locals/
evaluate on the same session could misalign the FIFOs. Observers (read-only) are
always safe; concurrent *drivers* of those specific request kinds are the
documented caveat. Fixing it properly needs correlation ids in the bingo protocol
— deferred. Resume/step/breakpoint-hit events are broadcast to all clients and
need no correlation, so multi-driver continue/step is fine; only the id-less
confirmation requests are affected.

The hub's reliable ordinary-command admission is a prerequisite for these
FIFOs: it either executes each admitted command in order or closes the whole DAP
connection on sustained overload. Never replace overload eviction with an
uncorrelated bingo error event — the adapter cannot identify a dropped tail
slot, so such an event could consume the wrong FIFO head.

**setBreakpoints is a replace-all TRANSACTION** (`breakpoints.go`): a request
records the source's latest-wins intent and is answered only once every operation
it owns has been confirmed. `bpByFile` is `file → line → *bpLine`, and `bpLine`
deliberately keeps three things apart, because pipelined requests make them
legitimately disagree:

- `installedID` — an installed FACT. Written **only** by a confirmed
  `EventBreakpointSet` and dropped **only** by a confirmed
  `EventBreakpointCleared`. Never on intent.
- `desired` — the newest request's intent for that line (latest wins).
- `pending` — the single LIVE `bpOp` for that line. One live op per line at a
  time is what keeps the id-less `setQ`/`clearQ` FIFOs unambiguous.

An operation is **abandoned** when a restart discards its line: it stops being
that line's `pending` op (so the line can converge again immediately instead of
latching behind a removal that already happened), but it keeps its place in the
FIFO, because the debugger will still answer it and that answer must pop the head
it reserved or every later confirmation correlates to the wrong request.
`liveLineLocked` is the gate — it matches the popped op against the line's
current `pending`, so an abandoned (or superseded, or forgotten) op's answer does
exactly one thing: pop. It never settles waiters, fails owners, or writes state
that now belongs to a re-identified line.

`advanceLineLocked` is the convergence loop: it issues the one command the
installed/desired gap calls for, or settles everyone parked on the line when the
two already agree. Every confirmation re-enters it, which yields the invariants:

- A **removal is owned by the request that caused it** (`openClears` +
  `clearOwners`), so a response never precedes its own clears. A remove-all no
  longer reports success while the `ClearBreakpoint` is still in flight.
- A **rejected clear retains the mapping** and fails the originating request
  (`clearFailure` → an error `setBreakpoints` response). Forgetting the id would
  leave a breakpoint the client can never remove — `breakpointTable.clear` keeps
  the entry when the memory write fails, so the trap really is still armed. The
  failure path deliberately does not re-enter the loop; a new request retries.
  That no-retry rule is justified **only** while the line is still armed: with
  nothing armed there is nothing to retry and nothing to report, so the removal
  completes successfully and the line resumes converging rather than stalling.
- A **pending Set superseded before it confirms** is cancelled on confirm: the id
  is recorded as a fact (the trap IS armed) but `desired` stays false, so the
  next advance clears it immediately. It is never committed as desired state.
- An **overlapping request wanting an already-pending line attaches** to that op
  (another `setWaiter`) instead of issuing a duplicate `CmdSetBreakpoint`, which
  the engine would reject with `errBreakpointExists` — and whose failure handling
  used to delete the entry holding the live id.
- **Exactly one response per request** (`bpRequest.ready()` claims the right to
  answer; slots and obligations settle on different goroutines), and requests are
  answered in `reqSeq` order so a later request's view lands last.
- Sources are independent; the id-less FIFO limitation is unchanged.

Breakpoint commands are staged in `outbox` and drained by `flushCommands`
(serialised by `flushMu`) rather than enqueued directly: both the DAP read loop
and the hub write pump produce them, and the FIFOs only correlate if the wire
order matches the order slots were reserved under `mu`.

Clearing the breakpoint the process is currently parked on re-arms it through the
engine's step-off path (see the clearbp spec), so the e2e continue-to-exit uses a
no-breakpoint target, not a clear-then-continue.

`bpLine` keeps the stable DAP id (`dapID`) separate from the debugger's internal
id (`installedID`). After `EventRestarted`, reconcile its exact source-path/line
keys from `RestartedPayload` before replying: retained entries adopt the fresh
debugger id, while discarded entries are removed and emit a `breakpoint` changed
event with their prior DAP id and `verified:false`. A line with an operation in
flight is re-identified **too** — correlation rides the queued `*bpOp`, not
`installedID`, so adopting the fresh id cannot desynchronise the FIFOs, whereas
keeping the pre-restart id would strand the line on an identifier the new engine
never issued (the already-marshalled clear fails, retain-on-failure reissues that
dead id forever, and the real new breakpoint becomes unremovable). Only a line
with `installedID == 0` is skipped: it owns no identity yet, so a payload entry
for it describes another driver's breakpoint. A **discarded** line goes further:
it abandons its in-flight operation (see above) — the command was addressed to a
breakpoint the relaunched process no longer has, so its answer can only be stale,
and leaving it live would latch the line behind a removal that already happened.
Do not reset `setQ`/`clearQ`; those FIFOs still own any in-flight confirmations,
abandoned ones included.

**This transaction depends on reliable ordinary-command delivery** (#160 / #162).
There are deliberately no adapter-side timeouts — inventing one would mask
command loss and could fire against a merely slow debugger. A silently dropped
`SetBreakpoint`/`ClearBreakpoint` therefore produces no confirmation, which now
latches that line's `pending` forever: no further operation can be issued for it
and the owning request is never answered. The adapter side of the contract is
covered here (it blocks rather than drops, and wire order matches slot-reservation
order); the hub side is `injectCommand`'s admission.

### Server wiring + multi-client discovery

`internal/server` implements `dap.Provider` (`dapProvider`): `CreateSession` →
`sessions.create` (an ordinary managed hub, identical to `/ws?create`), so a
DAP-created session auto-cleans on disconnect and is joinable by WebSocket
observers. `Server.StartDAP(addr)` opens the DAP TCP accept loop; `Shutdown`
closes it. The DAP client emits a `console` output naming the session id;
observers join `/ws?session=<id>` (also discoverable via `/api/sessions`).

**Management discovery contract.** `GET /api/health` is the stable,
non-cacheable process-discovery endpoint frontends use for connect-or-start. Its
explicit JSON response carries `service:"bingo"`, `managementApiVersion:1`, the
independent `wireProtocolVersion` from `pkg/protocol.Version`, a per-process
UUID `instanceId`, the enabled/resolved DAP listener address, managed-idle
configuration in milliseconds, and the current session count. The DAP object
also advertises `sessionEventVersion:1`; graphical clients require it before
reusing a managed server because older API-v1/wire-v1.2 servers do not emit the
`bingo/session/v1` discovery event. Management API
compatibility and WebSocket wire compatibility are separate checks: changing
one does not implicitly version the other. A DAP bind to `:0` MUST publish the
actual listener address, never the unresolved configured address. Health
polling has no lifecycle effect. Positive idle durations below `1ms` or with a
fractional millisecond are rejected so the timer and integer `timeoutMs` field
always describe the exact same interval.

**Listener safety.** The CLI and `just server` defaults bind management,
WebSocket, and DAP endpoints to explicit IPv4 loopback. The protocols do not
authenticate clients, and WebSocket clients without an `Origin` header are
valid, so never restore wildcard defaults. Operators may explicitly choose a
non-loopback address for trusted-network or externally authenticated setups.

**Connect-or-start and ownership.** A frontend health-checks the known
management address and reuses a compatible process; otherwise it starts bingo
and lets listener binding arbitrate concurrent startup attempts. Frontends
NEVER directly kill a potentially shared server. `-idle-timeout` is the opt-in
process-owner mechanism; omitted/zero keeps manually started servers persistent.
The server arms the grace at startup and whenever its managed-session count
reaches zero, cancels it while any session exists, and shuts itself down only
when the count stayed zero for the whole grace. A bare DAP TCP connection does
not count: DAP creates a session only on launch/attach, so process owners must
allow enough startup grace for health-check + connect + handshake.
The VS Code companion is the first implementation of this contract; future UI
clients can follow it, but no bingo UI process manager is shipped here.

**Idle admission and shutdown invariants.** `sessionStore` broadcasts changes
by closing and replacing a channel under its map mutex; snapshots return
`count`, a monotonically increasing generation, and the current channel after
releasing the lock. The generation prevents a create/remove burst from looking
like uninterrupted zero-session time, and close-broadcast lets the idle monitor
and shutdown waiter observe the same transition without stealing it from each
other. Never block on the change channel while holding the store lock.

Every WS and DAP create/join increments `activeAdmissions` under `lifecycleMu`
before doing store, hub, or socket work; its deferred completion decrements the
count and close-broadcasts `admissionChanged`. Timer expiry takes the same lock
and may mark `shutting` only when the armed zero-session generation is still
current, the fresh store count is zero, and no admission is active. Lock order
is lifecycle then a store read snapshot; neither lock is held across hub or
socket I/O.

An admission already active when the deadline expires gets to finish, but the
expired gate rejects further operations so repeated failed joins cannot keep
the process alive. The original deadline is not reset while that operation is
in flight: success produces a live session and disarms shutdown; failure with
the same zero-session generation commits shutdown immediately on completion.
If the store generation changed because a session became active and later
returned to zero, the monitor starts a fresh full grace period.

The idle monitor starts only after HTTP binds, owns one reset/drain-safe timer,
and exits on server cancellation. It may call `Shutdown` itself, so Shutdown
must not wait for the monitor goroutine. Shutdown is idempotent: mark admission
closed and cancel the server context; join any in-flight DAP listener startup;
close DAP and gracefully stop HTTP; wait for every admitted WS/DAP create/join;
then wait event-driven for the canceled session store to empty. DAP Close
already waits for its handlers, including clients that connected but never
created a session. This ordering prevents a late client add from escaping
registry teardown while still allowing every established session to close.

`StartDAP` reserves its at-most-once start under `lifecycleMu`, releases the
lock before context-aware listener binding, and publishes the server/address
only if shutdown has not won. Shutdown cancellation unblocks the bind and waits
for that start attempt before closing Done, so no DAP listener can appear after
finalization. A genuine bind failure remains retryable only while the server is
not shutting down.

Hub admission has a second, session-local gate: `registry.add`, `remove`, and
`closeAll` serialize on the registry mutex. Removing the final client marks the
registry closed in the same critical section, before `removeClient` schedules
hub shutdown; closeAll also marks it closed before removing clients.
`Hub.AddClient` returns `ErrHubClosed` and closes the supplied connection when
admission is closed. WebSocket and DAP callers must propagate/handle that error.
This is what closes the race where a join resolves a session just before its
final client triggers one-shot hub teardown. Do not hold the registry lock
across socket writes.

`http.Server.Shutdown` closes the listener before session draining completes,
and Start can also be delayed in listener setup. `cmd/bingo` therefore runs
Start in a goroutine with a one-result buffered channel and selects it against
`Server.Done()`: Start errors return immediately, a clean Start return waits for
Done, and a Done-first result still waits for Start to unwind and consumes its
result. The HTTP bind uses `net.ListenConfig` with the server context so
Shutdown always unblocks listener setup; lifecycle-owned cancellation is clean,
while genuine bind/Serve errors run the same one-time Shutdown and remain the
Start result. This closes a DAP listener that may have started first and
guarantees Done. Concurrent duplicate Start calls return `ErrServerStarted`
without tearing down the first caller.

**One-driver vs many-driver.** DAP assumes a single driver; bingo does not
enforce it. WebSocket clients CAN also drive (the hub's `resumeCh` is
first-writer-wins). The recommended posture is DAP-drives + WebSocket-observes,
but multi-driver resume/step works because those events fan out to everyone; the
only fragility is the id-less confirmation FIFO above.

**Interactive DAP client (`cmd/dapcli`).** A readline REPL mirroring `cmd/cli`'s
command set/UX but speaking DAP over TCP (`just dapcli`, default `-addr
localhost:4711`). Without `-session` it creates a session on the first
`launch`/`attach` (DAP has no standalone "create session" request) and captures
the announced id from the `console` output for `state`/discovery; with `-session
<id>` it JOINS via the `onJoin` path above. `launch`/`attach` default
`stopOnEntry:true` so the interactive user always gets control and the tracee
stays alive for others to join. It buffers `break`s set before launch and flushes
them on `initialized`. Any mix of `dapcli` and `cli` clients can drive/observe
one session concurrently.

**Option Y (rejected).** The alternative was teaching the hub about DAP directly
(a second protocol path through `internal/hub`). Rejected: it would fork the most
fragile fan-out/suspend-gate code for a second protocol. The `hub.WSConn`
translator keeps DAP entirely outside the hub — a strictly additive package.

### Tests

- Unit: [internal/dap/translate_test.go](internal/dap/translate_test.go) (pure
  translators), [internal/dap/handler_test.go](internal/dap/handler_test.go)
  (full handshake over loopback TCP + a fake `Session`/`Provider` + a command
  recorder: handshake→breakpoint, own-continue-suppressed, out-of-band-continue-
  surfaced, setBreakpoints diff/FIFO, stackTrace/variables correlation,
  step→stopped=step, disconnect-terminates), and
  [internal/dap/breakpoints_test.go](internal/dap/breakpoints_test.go) (the
  breakpoint transaction: a rejected clear retains the id and fails its request,
  partial clear failure, a superseded pending set clears exactly once, an
  overlapping pending set issues one command, set→remove→re-add leaves one live
  breakpoint, cross-source independence, restart re-identification with an
  in-flight operation, a discarded line abandoning its operation (later set,
  already-satisfied removal, late stale success), no-drop/in-order command
  delivery, exactly-once responses). Run with the normal
  `go test -tags bingonative ./internal/dap/...`.
- E2E: label `dap` in [test/integration](test/integration/) — a real go-dap
  client over TCP through the WHOLE stack (client → TCP →
  `internal/dap.Handler` → hub → debugger → tracee), see
  [debugger_e2e_dap_test.go](test/integration/debugger_e2e_dap_test.go).
  `declareDAPSpec` (single driver: launch→bp→inspect threads/stack/vars→resume
  re-hits bp), `declareDAPExitSpec` (clean exit → `exited`+`terminated`),
  `declareDAPMultiClientSpec` (**1 DAP driver + N WebSocket observers on one
  session all witness the same breakpoint hit** — the coexistence proof;
  `BINGO_E2E_DAP_OBSERVERS`, default 3), and `declareDAPJoinSpec` (**a SECOND DAP
  client joins an already-suspended session by id — the `onJoin` path — inspects
  it, then DRIVES a continue that the original DAP driver and a WebSocket observer
  both witness out of band** — the many-DAP-drivers-per-session proof). All four
  run in BOTH the linux and darwin containers (the translator is platform-
  agnostic; only the backend differs). CI: the `dap` label runs in the
  `fullstack-*` jobs of
  [debugger-e2e.yml](.github/workflows/debugger-e2e.yml).
- VS Code: [editors/vscode](editors/vscode/) uses strict TypeScript unit tests
  for endpoint/request validation, telemetry codecs/observer/reconnect/session
  registry, tree normalization/layout/lifecycle, a real lightweight DOM renderer,
  webview security/messages, and manifest/workspace/package contracts; a pinned
  `@vscode/test-electron` run activates the real extension/view and acknowledges
  a fake adapter's namespaced custom event, while `packagedE2E.ts` drives the
  actual native packaged server, DAP, WebSocket observer, and graphical model.
  The
  dedicated [vscode-extension.yml](.github/workflows/vscode-extension.yml)
  workflow lints, typechecks, tests, bundles, and builds the local VSIX without
  changing the Go CI jobs.

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
  inspect recorded calls. It also supports **fault injection**
  (`failWriteAt` / `failReadAt` / `failRegisters` / `failThreads`, guarded by
  `faultMu`) so the asynchronous `handleStop` error paths can be driven
  deterministically; arm the fault *before* pushing the stop that reaches it.
  [engine_halt_test.go](internal/debugger/engine_halt_test.go) uses this to pin
  the suspending-halt invariant at every site, and pairs a **real `Hub` with a
  real engine** over the fake backend — the hub's own `fakeDebugger` cannot
  exercise asynchronous stop handling, so that spec lives here rather than in
  `internal/hub`. `export_test.go` exposes a few internals
  (`ExportedForceSuspended`, `ExportedSetBreakpointAt`, …) so tests can
  bypass DWARF and the OS process model. Note `ExportedForceRunning` only flips
  the state field — it starts no `waitLoop`, so a stop pushed after it is never
  consumed; reach `running` through a real `Continue` when a stop must be
  delivered. Engine tests are tagged-agnostic — they avoid native code paths.
- `internal/hub`: `fakeDebugger` + `fakeWSConn` in [hub_test.go](internal/hub/hub_test.go).
  The fake conn uses a 256-deep `incoming` buffer so `WriteMessage` never
  blocks the hub event loop.
- `internal/server`: `httptest.Server` + real gorilla websocket client.
- `test/integration`: Ginkgo suite. A trivial placeholder spec runs by default;
  the real content is the **debugger E2E acceptance tests** — Ginkgo specs gated
  behind the `e2e` build tag that launch a real target and drive the ACTUAL
  native backend (ptrace on linux/amd64, a pure-Mach exception-port model on
  darwin/arm64), NOT the `fakeBackend`. These need a real kernel, so they only
  run on native runners (they can't run under emulation or with fakes). Split
  into:
  `debugger_e2e_common_test.go` (harness + target sources + shared spec bodies),
  `debugger_e2e_linux_amd64_test.go`, `debugger_e2e_darwin_arm64_test.go`, and
  `debugger_e2e_fullstack_test.go` (plus `debugger_e2e_dap_test.go`). Ginkgo
  labels: `basic`
  (continue+step-over correctness), `churn` (multi-thread robustness),
  `pause` (async-interrupt / manual-stop round-trip), `stepping`
  (StepInto crosses into a callee, StepOut returns to the caller), `inspect`
  (StackFrames chain + Locals + Goroutines at a breakpoint), `breakpoints`
  (a cleared breakpoint stops firing and an in-flight step-off reserves its
  temporarily table-less address), `kill` (Kill terminates a
  freely-running tracee), `exit` (EventProcessExited reports the tracee's real
  exit code), `signals` (linux-only: fatal and ordinary signal forwarding plus
  the shared Pause-suppression control), `overlap` (linux-only: a foreign thread's breakpoint stop
  surfacing while another thread single-steps off a software breakpoint —
  issue #199), `attach` (attach by PID to an already-running tracee — one the
  debugger did not launch — then breakpoint it), `concurrency` (the
  goroutine/thread snapshot data foundation — drives a known spawn-tree target
  and asserts parent linkage, start/created locations, the thread set with a
  single current thread, and created/exited lifecycle deltas across stops),
  `current-goroutine` (a small native stop/frame/snapshot/thread coherence
  control plus an opt-in over-cap proof selected by
  `BINGO_E2E_CURRENT_GOROUTINES`; the 20,000-goroutine proof is deliberately not
  part of every suite because the existing 8,192-entry rich decode is costly
  under `-race`; both native workflow jobs permanently run the small control),
  and `restart` (hub-level
  kill+relaunch reinstalls
  breakpoints and reruns from the top), all driving `debugger.Debugger`
  in-process (except `restart`/`fullstack`/`dap`, which go through the stack); plus
  `fullstack`, which drives operations through the ENTIRE stack (pkg/client →
  WebSocket → internal/server → internal/hub → debugger → tracee) to catch
  transport/hub wiring regressions the backend-only specs can't (seq re-stamping
  of real events, the suspend/resume gate on a genuine BreakpointHit, synchronous
  SetBreakpoint confirmation routing); plus `dap`, which drives a real Debug
  Adapter Protocol client over TCP through the SAME whole stack (go-dap client →
  TCP → internal/dap.Handler → hub → debugger → tracee) and, in the multi-client
  spec, attaches N WebSocket observers to the DAP-driven session to prove DAP +
  WebSocket coexist, and in the join spec attaches a SECOND DAP client to an
  existing session to prove many DAP drivers coexist (see the DAP section above).
  The `pause` spec
  (`declarePauseSpec`) is
  wired into **both** the linux and darwin containers — detection is
  platform-agnostic in the engine and each backend's `StopProcess()` surfaces the
  interrupt to `Wait` (a real SIGSTOP on linux, a control-port wake on darwin).
  The `hygiene` spec (`declarePortHygieneSpec`) is **darwin-only** — it asserts
  the Mach exception path does not leak task/thread send rights across many
  breakpoint stops (reads the `debugger.DarwinTaskPortSendRefs` hook), a check
  meaningless on the ptrace backend.

  **Platform scoping — both containers run the shared set.** The darwin container
  wires the same shared specs as linux: `basic`, `stepping`, `breakpoints`, `churn`,
  `kill`, `exit`, `attach`, `concurrency`, `current-goroutine`, `pause`, `inspect`, `restart`,
  `fullstack`, and `dap`. Linux additionally runs `signals` and `overlap`;
  darwin additionally runs the
  `hygiene` Mach exception port-right leak regression. This was NOT
  always so:
  under the old darwin wait4/ptrace model the step-off-an-armed-trap specs
  (`basic`, `stepping`, `breakpoints`, `churn`) and `kill` (kill-while-running)
  were LINUX-ONLY, because single-stepping off a software breakpoint could be
  diverted by a mid-step BSD signal and killing a freely-running tracee
  deadlocked the engine loop. The Mach-exception rearchitecture (**#92**) closed
  all of those gaps, so they now run on darwin too. The three things that made it
  reliable, with async preemption RE-ENABLED (`asyncPreemptOffDefault = false`):

  1. **Per-thread exception delivery + native signals.** Masking only
     `EXC_MASK_BREAKPOINT` and leaving BSD signals native means Go's
     thread-directed `SIGURG` reaches the exact M, so a single-step is no longer
     diverted into the runtime signal trampoline. (Under wait4 the only resume
     was process-directed, which misdirected SIGURG — the #89 root cause that had
     forced `asyncpreemptoff`.) During a single-step only the stepped thread is
     resumed (others stay Mach-suspended), so sysmon is frozen and no new
     preemption is injected in the step window.
  2. **Target-side I-cache flush on every breakpoint write.**
     `mach_vm_machine_attribute(MATTR_CACHE, MATTR_VAL_CACHE_FLUSH)` in
     `bingo_write_memory` makes a freshly-installed trap (`<stepover-next>`,
     `<stepout-return>`) visible the instant it is re-executed. Without it a
     trap re-hit within microseconds could fetch a stale L1 I-cache line and be
     skipped — the ~2.5% StepOut/step-over wedge (see Backend quirks → Darwin).
  3. **wait4-based kill, no `cmd.Wait`.** darwin `killProcess` resumes every
     thread, `SIGKILL`s, and reaps via `wait4` without blocking the engine loop,
     so kill-while-running tears down cleanly instead of deadlocking.

  Each label is its own CI job. CI:
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
  check requires a maintainer to run the **trusted base branch's**
  `e2e-darwin` recipe against the PR head on Apple Silicon, review any PR changes
  to that recipe, and add the `darwin-e2e-verified` label. The conservative
  coverage floor includes all `internal/debugger/` and `test/integration/`
  changes, `justfile`, entitlements, the module graph, repo-wide platform suffixes
  and native sources, plus explicit Go build constraints. The "Darwin
  E2E verified" **commit status is posted explicitly to the PR head SHA** and
  Darwin-native heads fail until the label is present.

  **Advisory, not authoritative — read this before relying on the status.**
  This repository's default Actions permissions are write, and *every* same-repo
  workflow runs under the single `github-actions[bot]` identity. Any workflow a
  PR author adds can therefore write the same `Darwin E2E verified` context on
  any SHA, and branch protection cannot distinguish "which workflow" produced an
  Actions status — the app binding is per-app, not per-workflow. So this status
  is a **review aid and audit trail, not a cryptographically enforceable gate**,
  and it is deliberately not a required check. Do not describe it as tamper-proof
  and do not "fix" it by disabling the merge queue: queue state is orthogonal to
  the shared-identity problem. Real enforcement needs either a **distinct trusted
  GitHub App** publishing its own status context (which branch protection *can*
  bind to), an org-required workflow (unavailable here), or native
  required-review/CODEOWNERS if the human review itself is the enforcement. The
  hardening below closes every bypass found in review that does not require an
  attacker to add a status-writing workflow; it cannot close that one, and no
  audit can prove the list exhaustive.

  `pull_request_target` is a requirement here, not a preference: a
  fork-triggered `pull_request` run gets a read-only `GITHUB_TOKEN` (it can
  neither publish the head status nor clear a stale label) and it executes
  workflow YAML taken from the PR head, so a fork could simply rewrite the gate
  to pass. Only a base-controlled `pull_request_target` run has both a write
  token and base-controlled policy. The usual argument against
  `pull_request_target` — a privileged token combined with untrusted code — is
  eliminated by never checking out or executing head content.

  **Trigger scope and policy source are both pinned to trusted refs.** The
  trigger is filtered to `branches: [main]`, and the policy script is fetched at
  **`${{ github.workflow_sha }}`** — the SHA of the commit the running workflow
  file came from — *not* at `github.event.pull_request.base.sha`. Without both,
  a same-repo writer could push an unprotected branch carrying a malicious
  `.github/scripts/darwin-verification-gate.sh`, open a PR against it, and have
  that attacker-authored policy executed with `pull-requests: write` +
  `statuses: write`. Never reintroduce an event-derived policy ref, and never
  widen the branch filter.

  The `pull_request_target` workflow and its own job run against the
  base SHA, so that job is deliberately NOT the merge gate. The trusted workflow
  performs **no checkout at all**: it reads the single policy script from the
  trusted workflow SHA through the contents API, so no working tree exists that
  could hold PR head or merge content, and it reads PR metadata/labels through
  GitHub APIs. Do
  not add `actions/checkout`, or any step that executes head code, consumes head
  artifacts, or builds a shell command from PR content, to this privileged
  workflow. Changed paths are computed from the immutable event base/head SHAs:
  the compare API resolves their merge base, then recursive Git-tree snapshots
  are structurally diffed by path/mode/type/blob SHA. Never return to the live
  PR-files endpoint — a force-push can change that response while the run still
  publishes to the event's old head SHA. A missing/empty diff or a Git-tree
  `truncated` response fails closed; **both** trees are independently schema-
  validated before the diff (array shape, non-empty string `path`, six-octal
  `mode`, `type` ∈ blob/tree/commit, hex `sha` on non-tree entries) and any
  malformed document fails closed; path matching stays inside JSON (Git permits
  newlines in filenames). The selector is deliberately conservative: repo-wide
  implicit `_darwin*`/`_arm64*` source suffixes, every changed native source
  that `go/build` compiles (C, C++, Obj-C, Fortran, assembly incl. `.sx`, SWIG,
  `.syso`), control-character paths, and any changed
  Go blob containing an explicit build-constraint line all require verification.
  This deliberately over-gates Linux/cross-platform constraints rather than
  guessing an incomplete tag universe.

  **PR-authored text is never consumed.** The gate reads only structural fields
  from the event (numbers, SHAs, refs, the sender login, the label name) and
  never the title, body, head branch name, or head label — all of which an
  attacker sets freely and any of which could otherwise reach a shell word or an
  API path. The contract suite injects a poison token into every one of those
  fields on **every** synthetic event and fails the case if the token appears in
  any `gh` invocation. A separate static contract asserts the harness still
  injects it and still asserts on it, because an earlier revision gated that
  check behind a per-case variable no case ever set — making it silently vacuous.

  **Only `.go` blobs are content-scanned, so everything else must be caught by
  name.** The native-source extension list covers every extension `go/build`
  compiles — `c cc cpp cxx m h hh hpp hxx f F for f90 s S sx swig swigcxx syso`
  — plus `.mm`, which Go itself does not accept but which is conventional for
  Objective-C++ and therefore gated anyway. A contract test asserts the list
  stays complete. Shrinking it reopens a real bypass:
  an untagged cgo wrapper plus a darwin-only `shim_darwin.sx` ships machine code
  the gate never looked at. Plain `.go` is deliberately **absent** from that
  bare-extension list (every Go change would gate and the constraint scan would
  become dead code) but **present** in the `_darwin`/`_arm64` suffix
  alternation, where the filename itself is the constraint.

  **A `.go` blob also gates on a cgo preamble**, not just on `//go:build` /
  `// +build`. A `#cgo darwin LDFLAGS: …` directive — bare inside the `/* … */`
  block before `import "C"`, or as a `// #cgo …` line comment — makes the file
  platform-dependent with no explicit constraint anywhere in it. A bare `#cgo`
  at the start of a line is not valid Go outside such a comment, so the anchored
  match cannot false-positive; prose that merely mentions `#cgo` mid-line does
  not gate. Both behaviours are pinned by cases.
  `edited` base-retarget events invalidate
  verification; unrelated PR edits preserve the current SHA-bound status.

  **Non-regular tree entries are always gated.** A changed `.go` *symlink*
  (mode `120000`) stores only its link text, so scanning its blob can never see
  the Darwin constraint in the file it resolves to; a gitlink/submodule
  (`type: commit`) has no blob at all. Every changed entry whose mode is not
  `100644`/`100755` or whose type is not `blob` — on **either** side, so both
  additions and deletions count — forces verification and is itemized in the
  log. Only regular blobs are content-scanned.

  **A UTF-8 BOM does not hide a build constraint.** Go 1.25 strips a single
  leading BOM before parsing, so `\xEF\xBB\xBF//go:build darwin` is a *live*
  constraint that an anchored `grep` on the raw bytes would miss. The blob
  scanner reads the first four bytes with `od`, drops exactly one leading UTF-8
  BOM before matching, and treats a UTF-16 BOM (`fffe`/`feff`) as
  "gate, don't guess". Blob responses that are not decodable base64, or whose
  encoding/content fields are missing or wrongly typed, fail closed — never
  "assume no constraint". This applies to added *and* deleted blobs.

  A separate unprivileged
  [.github/workflows/darwin-verification-policy-test.yml](.github/workflows/darwin-verification-policy-test.yml)
  runs the proposed HEAD script/tests on `pull_request` with a read-only token
  and a distinct check name. It is review feedback only: a fork can modify that
  workflow, so it never publishes the required status.

  On `synchronize`, `reopened`, or base-changing `edited`, the trusted policy
  best-effort removes `darwin-e2e-verified` and posts failure to the new/current
  head regardless of label cleanup success.

  **Only an authorized human `labeled` event may assert verification.** Success
  requires, in order: the event action is exactly `labeled` for exactly
  `darwin-e2e-verified`; the label is still present in live PR metadata; the
  event **sender** is a `User` (not a bot/app) whose login passes a shape check
  before it is interpolated into an API path; and
  `GET /repos/{owner}/{repo}/collaborators/{login}/permission` reports
  `admin`, `maintain`, or `write` for that exact login. The gate **never** reads
  prior commit statuses as a readiness signal — the previous `approval_ready`
  handshake was removed because any same-repo workflow can seed the status it
  was reading, which made it a self-signed approval. An `unlabeled` event for
  the verified label **always** publishes failure for a head that needs
  verification, and never consults live label state: a remove/re-add race must
  be settled by a *fresh authorized `labeled` event*, not by an unlabel run
  noticing the label came back. (A head with **no** Darwin-native change is
  short-circuited to `success` by the scope check before any label arm runs —
  there is nothing to withdraw, and the same live-generation binding still
  applies. Both behaviors are pinned by cases in the contract suite so the
  distinction cannot drift.) Unrelated label
  events post nothing, preserving either a legitimate success
  or a stale-cleanup failure already bound to the same head SHA.

  **Every success is bound to the live PR generation.** Commit statuses are
  SHA-global: two PRs can share one head commit, and a status published for one
  is visible to the other. So immediately before **each** `post_status success`
  the gate re-reads live PR metadata and requires that the PR is still `open`,
  its base ref is still exactly `main`, its live base SHA and head SHA still
  equal the event's, and the head repo is unchanged. A retarget mid-run, a
  force-push (including head ABA), a close, or a delayed run from an older event
  therefore fails closed instead of greening a commit whose context has moved.
  The policy also re-asserts `base.ref == main` itself rather than trusting the
  trigger filter — defense in depth for a future misconfiguration, and the only
  thing that would refuse an alternate-base run before it posted `pending`.
  Under the deployed `branches: [main]` filter that path is unreachable, because
  such a run is never dispatched at all.

  **Residual, by design: this status is main-only and is inherited, not
  scoped.** A commit status belongs to a SHA, not to a pull request, so a
  success legitimately published for a `main` PR is also *visible* on any other
  PR that shares that head commit — including one targeting a different base,
  whose diff against a different merge base may contain Darwin changes that were
  never verified. No PR-scoped decision can revoke a SHA-scoped signal, and the
  `branches: [main]` filter means the gate never runs (and so never re-evaluates
  or overwrites) on those PRs. Therefore: read `Darwin E2E verified` as an
  assertion about a **head commit relative to `main`**, and do not consume it as
  verification on any other base. Cross-base assurance needs a PR-scoped
  mechanism (a trusted App check, or human review), not this status.

  Relevant runs
  post `pending` before evaluation, serialize per PR, and post failure on
  API/decision errors so an old success cannot survive a reopened/error window.
  A status counts as published only when the API accepted it, so a failed POST
  never records a decision.
  If the trusted policy cannot be fetched, the trusted workflow posts failure
  inline.
  Keep permissions limited to API reads, PR-label cleanup, and
  head commit statuses; never expose secrets or execute untrusted code through
  `pull_request_target`.

  **Never cancel a gate run.** Because every relevant run publishes `pending`
  before it can decide, cancelling one mid-flight can strand the required head
  status on `pending` forever — a killed runner is not a reliable place to
  recover. So `cancel-in-progress` is **`false`**, which limits superseding to
  runs that are still queued and have therefore published nothing. Do not
  reintroduce `cancel-in-progress: true`.

  **Concurrency grouping is part of the policy.** Only events that are
  authoritative for the current head (`opened`, `synchronize`, `reopened`,
  base-changing `edited`, and `darwin-e2e-verified` label add/remove) share the
  serialized per-PR `policy` group. Unrelated labels and non-base edits get a
  per-run group so they can neither queue behind nor displace a queued gating
  run. A verified-label event that replaces a queued `synchronize` still cannot
  pass: the live-generation check re-reads the PR, so a head that moved after
  the label was applied fails closed.

  **A run must publish a decision or fail closed.** The gate records its
  terminal outcome (`success`, `failure`, or `ignored`) to
  `DARWIN_GATE_DECISION_FILE`, and a final `if: always()` workflow step — which
  also runs on cancellation — posts an explicit failure when that marker is
  absent. This converts an interrupted run into a failing head status instead of
  a permanently pending one. An unrecognized event action exits without a
  decision on purpose, so that fallback fails the head closed.

  **Only the top-level shell may publish.** `set -E` propagates the policy's ERR
  trap into command-substitution subshells, so an unguarded `$(...)` that fails
  publishes a status from the subshell and then again from the parent (the
  subshell's "already posted" flag cannot propagate back out). Every `$(...)`
  below the trap installation disarms it with `$(trap - ERR; …)`, and a contract
  test enforces that statically — the runtime symptom only reproduces on the
  runner's bash 5, not on macOS's bash 3.2.

  **Merge-queue posture:** the active default-branch merge queue evaluates
  required checks on synthetic merge-group SHAs, while this trusted workflow
  posts only to PR head SHAs. Do not require `Darwin E2E verified`: a missing
  group status would time out every queue entry, and an Actions-only
  `merge_group` publisher is not a safe fix because merge-group content includes
  PR-authored workflow YAML that runs with a base-repo write token under the
  same `github-actions[bot]` identity and can forge or overwrite the context.
  **Disabling the merge queue does not make this status authentic** — the
  shared-identity problem above is independent of it. Enforcement requires a
  distinct trusted GitHub App (or another status source branch protection can
  bind separately), or replacing the status with native required review /
  CODEOWNERS. Current branch protection/rulesets do not require the context, so
  the gate is explicitly advisory.

  The policy's adversarial contract suite lives in
  [.github/scripts/darwin-verification-gate_test.sh](.github/scripts/darwin-verification-gate_test.sh)
  and must stay runnable on both macOS bash 3.2 and Linux bash 5 with no new
  tooling. It mocks `gh` end to end (compare/tree/blob/PR/permission/status
  APIs) and poisons the commit-status *read* endpoint so any regression that
  reintroduces status-as-readiness fails loudly.

Build/test commands:

```sh
just build [linux amd64 | darwin arm64]   # produces ./build/bingo/...
just test [PKG]                            # go test -v
just coverage [PKG]                        # writes test/coverage.out
just integration                           # ginkgo -r ./test/integration (no e2e tag)
just build-examples                        # build five progressive targets with -N -l
just build-spawntree                       # build the dedicated telemetry demo with -N -l
just vscode-prepare                        # stage the current native server inside the extension
just vscode-dev                            # stage source extension + native server + examples for CLI-launched Extension Host
just vscode-check                          # lint, typecheck, test, bundle, package-list smoke
just vscode-package                        # writes verified dist/bingo-<platform>.vsix
just vscode-install                        # explicitly installs/updates bingosuite.bingo
npm --prefix editors/vscode run test:integration # pinned Electron activation/view/custom-event test
npm --prefix editors/vscode run e2e:packaged     # real native packaged DAP + graphical telemetry path
just e2e-linux                             # native linux/amd64 ptrace E2E (all labels)
just e2e-darwin                            # native darwin/arm64 Mach-exception E2E (codesigned; all labels)
# Filter to one label, e.g. only the correctness gate (package path must come
# before the -ginkgo.* flag so `go test` doesn't mistake it for the package):
go test -tags e2e -race ./test/integration -ginkgo.label-filter=basic
# Linux signal delivery and its Pause suppression control:
go test -tags e2e -race ./test/integration -ginkgo.label-filter=signals
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
- `RestartPayload.Args`/`Env` deliberately **omit** `omitempty` (unlike
  `LaunchPayload`). The hub's `handleRestart` gates the override on nil-ness
  (`if override.Args != nil`), so a non-nil empty slice must survive the wire
  as `[]` to mean "clear", distinct from `null`/absent meaning "reuse the
  original Launch value". `omitempty` would collapse both to `{}`. The protocol
  test pins the nil-vs-empty round trip. See issue #102.
- Hub `New(dbg, log)` (without a session ID) is for tests / single-session
  setups: the debugger is pre-attached, no state events are broadcast,
  managed-session machinery is bypassed. Real sessions go through
  `NewSession`.
- The `// arbitrary instruction byte` style of one-line code-explainer
  comments has been removed throughout. If you're tempted to add one, make
  sure it's explaining a non-obvious WHY, not restating WHAT the code does.

## When you change something

- **Wire protocol** (`pkg/protocol`): bump `Version` for breaking changes,
  and update the round-trip table in `protocol_test.go`. Currently **1.3**
  (goroutine ID 0/status `unknown` is the synthetic unresolved stop identity).
  1.2 had the reshaped
  `Variable` — added `Kind` + `Children` for the type-aware expandable
  tree — and the new `CmdEvaluate`/`EventEvaluate` name-only evaluate command/
  event with `EvaluatePayloadCmd`/`EvaluatePayload`. 1.1 had reshaped
  `Goroutine` and added `Thread`/`GoroutineSnapshotPayload` +
  `EventGoroutineSnapshot`/`CmdGoroutineSnapshot`.
- **Management API** (`internal/server`): `ManagementAPIVersion` is independent
  of the wire protocol. Bump it for incompatible `/api/health` or management
  semantics, keep the response structs/tags and README example aligned, and
  preserve no-cache + GET-only behavior.
- **DAP session discovery**: when the `bingo/session/v1` body contract changes,
  bump `protocol.DAPSessionEventVersion`, the `dap.sessionEventVersion` health
  capability, the extension compatibility check, and the shared
  `internal/dapclient` decoder/tests together.
- **Goroutine snapshot layout**: the reader resolves runtime struct offsets from
  DWARF **by name** (`goroutines.go`), never hardcoded. If you add a field, add
  it to `goLayout`/`resolveGoLayout`; a missing *required* offset invalidates the
  layout and falls back to the synthetic goroutine — keep new fields optional
  unless they're truly required. Preserve the streaming-cadence invariant
  (snapshot on breakpoint/pause/entry, never per-step), the degraded-snapshot
  rule (don't touch `prevGoids` on an unreadable read), and the
  automatic-snapshots-own-the-baseline rule (a `CmdGoroutineSnapshot` query
  reports no deltas and never advances `prevGoids`), and the
  length-before-pointer order in `allgsMetadata` (see *Coherent metadata
  reads*) — reordering it silently reintroduces fabricated goroutines that no
  completeness check can catch.
- **Breakpoint ids**: client-visible ids are the hub's session-stable logical
  ids (see
  [Breakpoint identity](#breakpoint-identity--hub-owned-logical-ids)). A new
  protocol surface carrying a breakpoint id must be translated in
  [internal/hub/breakpoints.go](internal/hub/breakpoints.go) — outbound
  physical→logical, inbound logical→physical — and an unknown inbound id must be
  rejected without reaching the debugger. Never rewind
  `breakpointIDs.next`, never delete a mapping before the debugger confirms the
  removal, and never leak a raw engine id to clients.
- **Suspend/resume sets**: update both `suspendingEvents` and
  `resumingCommands` in [hub.go](internal/hub/hub.go), and the matching
  hub_test cases.
- **New `handleStop` error path**: any new place where `handleStop` gives up on
  a resume must report it with `haltOnError` (detailed `EventError` + suspending
  `EventPaused`) with the engine in `stateSuspended`. A bare `EventError` is
  non-suspending and strands the session permanently — see
  [step-over flow](#software-breakpoint-step-over-flow).
- **New stop classification in the linux `Wait` loop**: decide deliberately
  whether the new case is user-visible. Anything the engine would act on must go
  through `classifyUserStop` so it is parked when it belongs to a thread other
  than the one being single-stepped, and its `recordStop` must happen only when
  the event is actually returned. See
  [foreign-thread stop parking](#foreign-thread-stop-parking-during-a-single-step-linux).
- **`PtraceSetOptions` on either process path**: the unconditional
  `PTRACE_EVENT_EXEC` failure (park-queue rule 9) is safe only because bingo's
  own `execve` is consumed *before* `TRACEEXEC` is enabled, and because
  `attachToProcess` enables no options at all. Preserve that ordering in
  `startTracedProcess`. Enabling `TRACEEXEC` on attach would not retroactively
  produce an event for an exec that already happened, but it WOULD make any
  later exec by the attached process fatal — decide that deliberately, and note
  the converse hazard it fixes: with no options at all, an attached process's
  exec currently arrives as a plain `SIGTRAP` that `Wait` can misread as a
  breakpoint.
- **New `EventKind`/`CommandKind`**: if it should reach an IDE, add its
  translation to [internal/dap](internal/dap/) (`translateEvent`/event handlers
  for events, `dispatchRequest` for the reverse). Events with no DAP equivalent
  are safely ignored in `translateEvent`, but decide deliberately — a new
  *suspending* event especially must map to a `stopped` reason or the IDE won't
  realise the tracee halted.
- **VS Code debug configuration** (`editors/vscode`): keep debugger ownership on
  type `bingo` and transport through `DebugAdapterServer`; never route bingo
  launches back through `type: go`, `debugServer`, or Delve. Keep the manifest
  schema, pure configuration tests, `.vscode/launch.json`, and extension README
  aligned when launch/attach arguments change.
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
