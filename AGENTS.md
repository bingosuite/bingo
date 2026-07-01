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
  Mach (`thread_get_state`, `mach_vm_read/write`, `task_threads`,
  `thread_set_state`) for state. `PT_ATTACHEXC` is NOT used — it routes signals
  through Mach exceptions, incompatible with our `wait4`-based loop.
- `task_for_pid` requires the `com.apple.security.cs.debugger` entitlement
  (the build embeds [entitlements.plist](entitlements.plist) via codesign).
- **Breakpoints are HARDWARE, not software.** The E2E target is an ad-hoc-signed
  Go binary; patching its `__TEXT` with a `BRK` invalidates the code signature,
  and on the next page-in AMFI SIGKILLs the tracee ("has no CMS blob? …
  Unrecoverable CT signature issue, bailing out"). Instead the backend arms the
  AArch64 debug registers (`DBGBVR`/`DBGBCR` via `ARM_DEBUG_STATE64`) — see
  `bingo_set_thread_hw_breakpoints`. No memory is patched, so the signature
  stays valid and there is no i-cache coherency problem. `WriteMemory`
  transparently maps the engine's breakpoint pokes onto debug registers: a write
  of the trap instruction → `armHWBreakpoint`; a write of the original bytes at a
  known bp → `disarmHWBreakpoint`; anything else → a genuine `rawWriteMemory`
  (never hit for breakpoints in normal operation).
- **HW breakpoints are per-thread; re-arm on every resume.** ARM debug registers
  live in each thread's context, so a thread created after the last arm (Go
  spins up new M's constantly under load) would not trap. `applyDebugState`
  re-writes the full breakpoint set into *every* current thread's debug
  registers on each `ContinueProcess` and each `resumeAfterSignal`. `Wait`
  resolves a SIGTRAP by scanning threads for one whose PC equals an armed
  address (`threadAtHWBreakpoint`) and returns `StopBreakpoint{TID, PC}`, so the
  engine never falls back to a (now nonexistent) in-memory trap scan.
- **The mid-run re-arm timer must NOT Mach-suspend the task.** Arming on resume
  covers threads that exist at resume time, but the target can create a new M
  and migrate the main goroutine onto it *between* two stops (no stop of its own
  is produced), so `rearmWhileRunning` re-applies the breakpoint set on a
  `rearmInterval` (10 ms) timer while `wait4` is blocked. It calls
  `applyDebugState` (a plain per-thread `thread_set_state`) and deliberately does
  **not** `task_suspend` the whole task. An earlier version suspended the task
  every tick to force threads off-core (so the CPU reloads their debug registers
  for certain); that armed reliably but froze the Go runtime — sysmon, the
  netpoller, the timer-lock holder — at a 10 ms cadence and would occasionally
  lose `main`'s `time.Sleep` wakeup, deadlocking the tracee with every M parked,
  correctly armed, and none reaching the breakpoint. Because arming-on-resume is
  the reliable path and the timer only needs to catch newly-created M's (which
  context-switch within microseconds on a yielding target and pick up the debug
  registers from a plain write), dropping the suspend removes the deadlock
  without reintroducing missed breakpoints. The timer is skipped while
  single-stepping.
- **Do NOT re-inject async signals — resume with signal 0.** This is the crux of
  correct Go-preemption support. Go async preemption is *thread-directed*: the
  runtime `pthread_kill`s `SIGURG` (16) at the exact M whose goroutine must reach
  a GC safe point. If you re-post it via `ptrace(PT_CONTINUE, pid, 1, SIGURG)`,
  XNU delivers it PROCESS-wide (`psignal`) to an arbitrary thread; the intended M
  never runs its handler, its `signalPending` stays set, `runtime.preemptM` never
  re-sends, and a stop-the-world hangs forever (the original 6/6 StepOver hang).
  `resumeAfterSignal` therefore resumes with signal 0 (`ptrace(request, pid, 1,
  0)`) for `SIGURG`/`SIGWINCH`, letting XNU deliver the still-pending
  thread-directed signal to its intended M. This matches Delve, whose
  `ptraceCont` always passes 0.
- `PT_STEP` is per-PROCESS on Darwin (despite the API taking a tid) and arms the
  task's *first* thread, not the tid you pass. So an arbitrary thread cannot be
  single-stepped; `needsTempBPStepOver()` returns true and the engine steps over
  a breakpoint with a transient next-line breakpoint + continue
  (`stepOverBreakpointViaTempBP`) instead of the restore→single-step→reinstall
  dance. For genuine machine single-steps (`SingleStep`) always pass `b.pid` and
  save the stepped thread port (`b.stepTID`) so `consumeStep` can label the stop.
- **Kill must be deterministic even from a wedged state.** A ptrace-stopped
  tracee ignores SIGKILL until resumed, and a Mach-suspended thread blocks
  termination. `killProcess` first drains every thread's suspend count
  (`bingo_resume_all_threads`), then delivers SIGKILL by every avenue
  (`PT_CONTINUE(SIGKILL)` → out-of-band `SIGKILL` → `PT_KILL` →
  `PT_DETACH(SIGKILL)`), then reaps with a 3 s bound so a truly stuck tracee can
  never wedge the engine loop (and hence `Kill`) past the harness's 5 s window.
- ASLR slide is computed in `TextSlide` by scanning the VM map for the first
  exec region with the 64-bit Mach-O magic. Do NOT use `TASK_DYLD_INFO` — its
  image array is unpopulated at the very first ptrace stop.

### Linux / amd64 ([backend_linux_amd64.go](internal/debugger/backend_linux_amd64.go))

- Pure ptrace. `startTracedProcess` enables `PTRACE_O_TRACEEXIT |
  PTRACE_O_TRACEEXEC`; clone-thread tracing is intentionally not enabled until
  the backend can resume Go runtime clone stops without parking the thread group.
- `Wait` uses `Wait4(-1, …, WALL)` to receive events for any thread.
  `PTRACE_EVENT_*` stops are absorbed (resumed and looped) and never surface
  to the engine.
- ptrace stops are per-thread. The backend records the last stopped TID and
  targets `ContinueProcess` / memory reads / memory writes at that TID, not
  blindly at the process PID. Non-main thread exits are absorbed inside `Wait`.
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
go test -tags linuxptrace ./test/integration -run TestLinuxAMD64DebuggerLaunchBreakpointSmoke
                                           # CI-only native linux/amd64 ptrace smoke test
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
