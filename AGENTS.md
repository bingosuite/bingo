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
| [editors/vscode](editors/vscode/) | Locally packaged TypeScript companion extension. Owns VS Code debugger type `bingo`, enables Go breakpoints, and connects the built-in Debug UI to an already-running bingo DAP TCP listener. |
| [cmd/target](cmd/target/) | Trivial target program for manual testing. |
| [examples/spawntree](examples/spawntree/) | Concurrency demo target: a deterministic main → supervisor → worker×N goroutine spawn tree for exercising the telemetry stream (see [docs/ConcurrencyTelemetry.md](docs/ConcurrencyTelemetry.md)). |
| [cmd/githook](cmd/githook/) | Conventional-commits commitlint, wired via [lefthook.yml](lefthook.yml). |
| [pkg/protocol](pkg/protocol/) | Wire types: `Event`, `Command`, payload structs, `EventKind`, `CommandKind`, `SessionState`. Single source of truth. |
| [pkg/client](pkg/client/) | Reference Go client. WebSocket-backed. Public surface: `Client` interface + `Create` / `Join` / `ListSessions`. |
| [internal/server](internal/server/) | HTTP/WebSocket entry. `Server`, `sessionStore`, `/api/sessions` and `/ws` handlers. |
| [internal/hub](internal/hub/) | Per-session bridge between connected clients and one `Debugger`. |
| [internal/dap](internal/dap/) | Debug Adapter Protocol translator. A `Handler` implements `hub.WSConn`, so a DAP/IDE client plugs into a hub session as just another client (ZERO hub changes). |
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
through the protocol. Routing `Kill` via `cmdCh` fixes that; the suspended wait
loop `return`s after executing a `Kill` (like `Restart`), since the process it
was waiting to resume no longer exists. `Kill` with no active debugger is a
benign no-op success (nothing to terminate).

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
actually resumes the process. If the resume is **rejected** — e.g. a transient
backend error while reinstalling a software breakpoint leaves the engine
`stateSuspended` — the hub broadcasts the `EventError` but **stays in the wait
loop** (it checks that the session left `suspended` before returning). Bailing
out on a failed resume would strand the client: the process is still suspended,
but a retry resume lands in `resumeCh`, which only the wait loop drains, so the
session could never be resumed again.

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
  to the engine — except the **main thread's** `PTRACE_EVENT_EXIT`, which is the
  one exit the engine needs. Because `PTRACE_O_TRACEEXIT` stops the leader
  *before* it dies and the engine tears down on the resulting `StopExited`, the
  real status never resurfaces as a later `wait4` `Exited()`; so `Wait` reads it
  at that stop via `PTRACE_GETEVENTMSG` (a `wait(2)`-encoded status) and reports
  the true `ExitCode`, or `StopKilled` on signal death. Returning a hardcoded 0
  here dropped every tracee's exit code (#94).
- ptrace stops are per-thread. The backend records the last stopped TID and
  targets `ContinueProcess` / memory writes at that TID, not
  blindly at the process PID. Non-main thread exits are absorbed inside `Wait`.
- `ReadMemory` uses **`process_vm_readv(2)`** as the fast path, falling back to
  `PTRACE_PEEKDATA` only when it is unavailable or short-reads. `process_vm_readv`
  bulk-copies the whole buffer in one syscall and — unlike ptrace ops — is NOT
  thread-bound, so it runs directly off the caller and skips the `execPtrace`
  tracer-thread handoff entirely (mirrors Delve). This is load-bearing for the
  goroutine snapshot: it issues dozens of small reads per stop across every live
  goroutine, and the old word-at-a-time-PEEKDATA-through-execPtrace path made a
  snapshot-on-every-breakpoint so slow it pushed the `churn` e2e past its
  target's 180s watchdog. The fallback keeps the original error semantics for
  genuinely-unmapped addresses. (Darwin was never affected — it already
  bulk-reads via `mach_vm_read`.)
- Single-step vs breakpoint disambiguation uses **both** `stepping` and
  `stepTID` (the exact TID `SingleStep` was issued against). Only a `cause==0`
  SIGTRAP on `stepTID` is the step's completion; the same stop on any other
  thread is that thread hitting an INT3 and is reported as a breakpoint. This
  matters because `Wait4(-1, …)` can return a sibling thread's concurrent
  breakpoint (or SIGURG) while a step is in flight — keying off `stepping`
  alone would misclassify it and corrupt the engine's step-over state machine.
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
  FP rule) and walking the saved-FP chain (`fp = *fp`). Missing/uncovered CFI
  degrades gracefully to the old `FP + 16` heuristic (`cfaFallbackFromFP`) rather
  than erroring the stop. The reader is arch-generic (maps SP/FP DWARF register
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

**Version 1.1** (`pkg/protocol`): reshaped `Goroutine` (added `ParentID`,
`StartLoc`, `CreatedLoc`, `ThreadID`, `Current`; renamed `GoLoc`→`CreatedLoc`),
new `Thread`, new `GoroutineSnapshotPayload`, new `EventGoroutineSnapshot` +
`CmdGoroutineSnapshot`.

**Streaming cadence (the load-bearing invariant).** `EventGoroutineSnapshot` is
auto-emitted on exactly the suspends that can change the concurrency picture —
**breakpoint hit, pause, and the launch/attach entry stop** — and on demand via
`CmdGoroutineSnapshot`. It is **NOT** emitted per step: `emitStepped` stays cheap
(embeds a synthetic single goroutine, no `allgs` scan) to protect the fragile
single-step/step-over path from extra per-step memory reads. `emitBreakpointHit`
/ `emitPaused` build the snapshot **once**, embed its current goroutine in the
stop event, then stream the same snapshot — one build, no double scan, no double
delta pass.

**Not a suspending event.** It follows a suspending event (or answers a query)
and never gates the hub. DAP `translateEvent` **deliberately ignores** it:
translating it would corrupt the FIFO that correlates a DAP `threads` request to
`EventGoroutines` (snapshots are unsolicited, with no matching request). DAP
clients get goroutine data from the `threads` request (`EventGoroutines`, which
now returns the rich list); the snapshot stream is WebSocket-only.

**Reference observer + runbook.** [cmd/wsmon](cmd/wsmon/) is the reference
WebSocket telemetry consumer: it `-session`-joins read-only and live-renders the
spawn tree / thread set / lifecycle deltas from this stream (a UI-agnostic view
of exactly the data a spawn-hierarchy visualization needs). The end-to-end
DAP-drives + WS-observes walkthrough — server, VS Code (or `cmd/dapcli`) driver,
and `wsmon` against the [examples/spawntree](examples/spawntree/) target — is in
[docs/ConcurrencyTelemetry.md](docs/ConcurrencyTelemetry.md).

**Lifecycle deltas.** `engine.prevGoids` (loop-thread-only, no lock, like
`manualStopPending`) remembers the previous live goid set; `diffGoids` returns
created/exited and adopts the new set. First snapshot returns nil deltas (a fresh
session must not report every goroutine as "created"). A **degraded** snapshot
(runtime unreadable — e.g. the pre-init entry stop) does **not** touch
`prevGoids`: an empty read must not look like every goroutine exited.

**Graceful fallback.** Every read is best-effort. `resolveGoLayout` marks the
layout invalid if any required `g`/`gobuf`/`stack`/`m` offset is missing, and any
unreadable address degrades the whole snapshot to the legacy single synthetic
goroutine (`ID:1, Status:"waiting"`, current PC) rather than erroring the stop.
This preserves behavior for stripped binaries / attach-without-DWARF and keeps
the `fakeBackend` engine unit tests green.

**Current goroutine = SP-containment** (platform-independent): the stopped
thread's SP within `[g.stack.lo, g.stack.hi)`. The current *thread* is the M
whose `curg` goid equals the current goid. A non-current goroutine's
`CurrentLoc` uses `gobuf.pc` (where it resumes); the current one uses the live
PC. Status strings are hardcoded (stable across Go versions); wait-reason
strings are read dynamically from `runtime.waitReasonStrings`. Goroutines with
goid<=0 or status `_Gdead` (scan bit stripped) are filtered out — their exit
surfaces in the next Exited delta.

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

**VS Code companion — transport only, separate from Go tooling.**
[editors/vscode](editors/vscode/) packages as extension ID
`bingosuite.bingo`, registers a
`DebugAdapterDescriptorFactory` for debugger type `bingo` and returns
`DebugAdapterServer(dapPort, dapHost)` (defaults `127.0.0.1:4711`). It never
registers type `go`, launches or validates `dlv`, or calls into Microsoft's Go
extension. Keep `golang.go` installed for gopls/navigation/formatting/tests; a
`"type": "bingo"` launch is owned entirely by the companion and this DAP server.
The explicit IPv4 default matches `internal/dap/server.go`'s `tcp4` listener;
do not change it to `localhost`, which older VS Code/Node runtimes can resolve
to `::1` without falling back to IPv4.
The extension validates launch (`program`), existing-session join (`session`),
and OS-process attach (`pid`, optional `binaryPath`) before connecting.
`dapHost`/`dapPort` are client-owned endpoint fields that remain in VS Code's
raw launch/attach arguments; Go's JSON decoder ignores those unknown fields, so
they never enter the bingo command payload. Do not add them to the wire protocol
or `launchConfig`.

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
  loop; returns `io.EOF` on Close. **cmdOut-priority:** a non-blocking check of
  `cmdOut` precedes the `{cmdOut | done}` select, so a `Kill` enqueued right
  before `Close` (disconnect-terminate) is still handed off before EOF.
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
  `awaitingWelcome`): `suspended`→`suspended=true` + `stopped reason=pause` (tid
  defaults to 1 — the engine inspects the currently-stopped goroutine regardless
  of the DAP threadId); `exited`→`terminated`; idle/running→nothing. For the
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
(correlated via `evalQ`, NOT a stop — see below); `EventSessionState`→ignored on
the launch/attach path, but consumed **once** as the initial state on the join
path (see *Joining an existing session*); `EventGoroutineSnapshot`→**deliberately
ignored** (WebSocket-only concurrency stream with no DAP equivalent; translating
it would corrupt the `threads`→`EventGoroutines` FIFO — see the goroutine
snapshot section).

`EventContinued` → DAP `continued` **only for out-of-band resumes**. The Handler
increments `pendingContinues` before enqueuing its OWN continue and decrements it
on the matching `EventContinued` (suppressing it — the IDE already implied
continuation via the continue/step response). A continue driven by a *different*
client on the session arrives with the counter at 0 and IS surfaced as
`continued`, so the IDE learns the tracee is running again. This is exactly why
the prerequisite PR made the engine emit `EventContinued` on resume.

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

**setBreakpoints is replace-all** (`breakpoints.go`): diff the requested lines
for a source against `bpByFile` — clear removed, set new, keep unchanged — and
respond once every slot in the request resolves, in request order. Clearing the
breakpoint the process is currently parked on re-arms it through the engine's
step-off path (see the clearbp spec), so the e2e continue-to-exit uses a
no-breakpoint target, not a clear-then-continue.

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
configuration in milliseconds, and the current session count. Management API
compatibility and WebSocket wire compatibility are separate checks: changing
one does not implicitly version the other. A DAP bind to `:0` MUST publish the
actual listener address, never the unresolved configured address. Health
polling has no lifecycle effect. Positive idle durations below `1ms` or with a
fractional millisecond are rejected so the timer and integer `timeoutMs` field
always describe the exact same interval.

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

**Idle generation and shutdown invariants.** `sessionStore` broadcasts changes
by closing and replacing a channel under its map mutex; snapshots return
`count`, a monotonically increasing generation, and the current channel after
releasing the lock. The generation prevents a create/remove burst from looking
like uninterrupted zero-session time, and close-broadcast lets the idle monitor
and shutdown waiter observe the same transition without stealing it from each
other. Never block on the change channel while holding the store lock.

The server also increments an admission generation under `lifecycleMu` before
every WS or DAP create/join operation. When an idle timer fires, it takes that
same lock, rechecks both the store and admission generations, and marks
`shutting` before releasing the lock. An operation admitted after the timer's
snapshot therefore invalidates the expiry; an operation arriving after the
commit is rejected. Do not hold `lifecycleMu` over hub/client work: the
generation increment + WaitGroup admission happen under the lock, while the
operation runs after release.

The idle monitor starts only after HTTP binds, owns one reset/drain-safe timer,
and exits on server cancellation. It may call `Shutdown` itself, so Shutdown
must not wait for the monitor goroutine. Shutdown is idempotent: mark admission
closed; close DAP and gracefully stop HTTP; wait for any WebSocket
create/join operation that crossed the admission gate; cancel all hub contexts;
then wait event-driven for the session store to empty. DAP Close already waits
for its handlers, including clients that connected but never created a session.
This ordering prevents a late client add from escaping registry teardown while
still allowing every established session to close gracefully.

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
Done, and completed shutdown can win while Start is still unwinding without
parking its eventual result send. The HTTP bind uses `net.ListenConfig` with the
server context so cancellation reaches listener setup. Fatal bind/Serve errors
must run the same one-time Shutdown before Start returns the original error;
this closes a DAP listener that may have started first and guarantees Done.
Concurrent duplicate Start calls return `ErrServerStarted` without tearing down
the first caller.

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
  translators) and [internal/dap/handler_test.go](internal/dap/handler_test.go)
  (full handshake over loopback TCP + a fake `Session`/`Provider` + a command
  recorder: handshake→breakpoint, own-continue-suppressed, out-of-band-continue-
  surfaced, setBreakpoints diff/FIFO, stackTrace/variables correlation,
  step→stopped=step, disconnect-terminates). Run with the normal
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
  for endpoint/request validation plus manifest tests that pin debugger ownership,
  Go breakpoint support, endpoint defaults, and launch/join/PID snippets. The
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
  (a cleared breakpoint stops firing), `kill` (Kill terminates a
  freely-running tracee), `exit` (EventProcessExited reports the tracee's real
  exit code), `attach` (attach by PID to an already-running tracee — one the
  debugger did not launch — then breakpoint it), `concurrency` (the
  goroutine/thread snapshot data foundation — drives a known spawn-tree target
  and asserts parent linkage, start/created locations, the thread set with a
  single current thread, and created/exited lifecycle deltas across stops), and
  `restart` (hub-level
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

  **Platform scoping — both containers run the full set.** The darwin container
  wires the same specs as linux: `basic`, `stepping`, `breakpoints`, `churn`,
  `kill`, `exit`, `attach`, `concurrency`, `pause`, `inspect`, `restart`,
  `fullstack`, and `dap`,
  plus the
  darwin-only `hygiene` (Mach exception port-right leak regression). This was NOT
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
just build-spawntree                       # rebuilds the VS Code demo with -N -l
just vscode-check                          # lint, typecheck, test, bundle, package-list smoke
just vscode-package                        # verifies reproducibility, writes ignored dist/bingo.vsix
just vscode-install                        # explicitly installs/updates bingosuite.bingo
just e2e-linux                             # native linux/amd64 ptrace E2E (all labels)
just e2e-darwin                            # native darwin/arm64 Mach-exception E2E (codesigned; all labels)
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
  and update the round-trip table in `protocol_test.go`. Currently **1.2** (the
  reshaped `Variable` — added `Kind` + `Children` for the type-aware expandable
  tree — and the new `CmdEvaluate`/`EventEvaluate` name-only evaluate command/
  event with `EvaluatePayloadCmd`/`EvaluatePayload`). 1.1 had reshaped
  `Goroutine` and added `Thread`/`GoroutineSnapshotPayload` +
  `EventGoroutineSnapshot`/`CmdGoroutineSnapshot`.
- **Management API** (`internal/server`): `ManagementAPIVersion` is independent
  of the wire protocol. Bump it for incompatible `/api/health` or management
  semantics, keep the response structs/tags and README example aligned, and
  preserve no-cache + GET-only behavior.
- **Goroutine snapshot layout**: the reader resolves runtime struct offsets from
  DWARF **by name** (`goroutines.go`), never hardcoded. If you add a field, add
  it to `goLayout`/`resolveGoLayout`; a missing *required* offset invalidates the
  layout and falls back to the synthetic goroutine — keep new fields optional
  unless they're truly required. Preserve the streaming-cadence invariant
  (snapshot on breakpoint/pause/entry, never per-step) and the degraded-snapshot
  rule (don't touch `prevGoids` on an unreadable read).
- **Suspend/resume sets**: update both `suspendingEvents` and
  `resumingCommands` in [hub.go](internal/hub/hub.go), and the matching
  hub_test cases.
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
