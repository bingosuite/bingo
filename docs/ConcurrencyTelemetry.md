# Concurrency telemetry — end-to-end runbook

bingo speaks two protocols against **one** debug session at the same time:

- **DAP** (`-dap-addr`) — an IDE (VS Code) or `cmd/dapcli` *drives* the session:
  breakpoints, stepping, continue/pause, stack/variables. This is the
  least-common-denominator debug loop.
- **WebSocket** (`-addr`) — any number of clients *observe* (and can also drive)
  the same session. The richer concurrency telemetry — the goroutine spawn tree,
  the OS-thread set, and created/exited lifecycle deltas — streams here as
  `EventGoroutineSnapshot`.

The VS Code 0.4.0 extension wires both together automatically: DAP drives while
the **Bingo Concurrency** Activity Bar view observes the exact session over
WebSocket. `cmd/wsmon` remains the terminal observer for non-VS Code workflows.

```
             drives (DAP :4711)                observes (WS :6060)
  VS Code  ─────────────────────►  bingo server  ◄─────────────────  Concurrency view
  / dapcli                          (one session)                    / cmd/wsmon
                                          │                            threads,
                                          ▼                            lifecycle)
                                    spawntree tracee
```

See [AGENTS.md](../AGENTS.md) → *DAP* and *Goroutine / thread snapshot* for the
architecture behind this.

## 0. Prerequisites

- macOS **darwin/arm64** (needs `-tags bingonative` + the codesigned server
  entitlement) or **linux/amd64**. The `just` recipes handle the tag/codesign.
- For Go language tooling in VS Code: Microsoft's **Go extension**. The repo
  keeps its `go.buildTags: bingonative` settings for gopls, navigation,
  formatting, and tests.
- For the VS Code debugger: bingo's separate companion extension. Build and
  install the matching platform VSIX once:

  ```sh
  just vscode-install
  ```

  Automatic graphical telemetry requires **bingosuite.bingo 0.4.0 or newer**. Run
  **Developer: Reload Window** once after installation or update. The companion
  owns debugger type `"bingo"` and connects directly to bingo's DAP listener;
  it neither invokes nor validates `dlv`, and it does not replace the Go
  extension's `"go"` type. To update, rerun `just vscode-install`; uninstall with
  `code --uninstall-extension bingosuite.bingo`.

## 1. Demo targets

The [progressive example suite](../examples/README.md) is available from the
normal workspace launch picker. `level5-workflow` gives the richest hierarchy:
**main → workflow×3 → stage×3**, including a deterministic canceled workflow.
The intended telemetry breakpoint is the result send in `inventoryStage`
(`examples/level5-workflow/main.go:83`). The workspace launch runs
**bingo: build examples** before F5 and uses the installed VSIX's bundled
server; it does not rebuild or codesign extension sources.

`examples/spawntree` remains the dedicated long-running lifecycle demo. It
churns a deterministic **main → supervisor → worker×3** tree so consecutive
snapshots show workers appearing in `created` and leaving in `exited`. Build it
with `just build-spawntree` and drive it with `cmd/dapcli` as shown below.
Contributor source-extension work is a separate command-line path: run
`just vscode-dev`, then
`code --new-window --extensionDevelopmentPath="$PWD/editors/vscode" "$PWD"`.
It is intentionally absent from the root Run and Debug dropdown.

## 2. Drive with VS Code (DAP, automatic server)

1. Open this repo in VS Code.
2. Select **“bingo DAP: launch example (stop on entry)”**. The only other root
   choice is **“bingo DAP: join running session”**.
3. Press F5, choose **level5-workflow**, and let VS Code run
   **“bingo: build examples”**. The companion
   health-checks `127.0.0.1:6060`,
   reuses a compatible server or starts its detached bundled server, waits up
   to `serverReadyTimeoutMs` (five seconds by default) for compatible readiness,
   then connects to DAP at `127.0.0.1:4711`. bingo launches
   `build/examples/level5-workflow` and stops at entry.
4. Set a breakpoint on `examples/level5-workflow/main.go:83` and **Continue** —
   the tracee stops with several workflow and stage goroutines alive.
5. Open **Bingo Concurrency** in the Activity Bar. The DAP adapter publishes the
   versioned `bingo/session/v1` custom event after session attachment, and the
   extension joins it automatically. No session-id copy is required.
6. Search/select nodes, inspect current/start/creation locations and threads,
   or use the title-bar Refresh/Fit actions. The first graphical session
   auto-reveals unless `bingo.concurrency.autoReveal` is disabled.

No manual `just server` is required. The extension never kills the shared
process. The default managed server exits only after its 30-second idle grace
with no sessions. Open **bingo Server** for the persistent child log path. If
another extension host starts concurrently, listener binding selects one server
and both hosts reuse it.

The lifecycle fields are `serverMode`, management host/port, DAP host/port,
`serverReadyTimeoutMs`, and `managedIdleTimeoutMs`; the checked-in launch file
pins their local defaults. Remote or forwarded endpoints must explicitly use
`"serverMode": "connectOnly"`, which bypasses health and spawn. If F5 reports an
occupied/incompatible endpoint or startup timeout, inspect the endpoint and log
path in **bingo Server** rather than killing a potentially shared process.

> A second VS Code window can *join* the same session (observe/drive over DAP)
> with the **“bingo DAP: join running session”** config, which sends a DAP
> `attach` carrying only the `session` id (no `pid`) — bingo's join path.
> A separate `"request": "attach"` configuration with numeric `pid` and optional
> `binaryPath` attaches to an OS process instead; the extension README has the
> full shape.

## 3. Drive with cmd/dapcli (DAP, no IDE)

Equivalent driver in a terminal requires a manual server:

```sh
just build-spawntree
just server
just dapcli            # or: go run -tags bingonative ./cmd/dapcli -addr localhost:4711
# in the REPL:
launch ./build/spawntree      # creates a session, stops on entry
break examples/spawntree/main.go:27
c                             # continue → hits the breakpoint
```

`dapcli` prints the session id it created (also on the server console). Continue
again to churn rounds; each stop pushes a fresh telemetry snapshot to observers.

## 4. Terminal fallback with cmd/wsmon

In another terminal, join the **same session id** read-only and watch it update:

```sh
go run ./cmd/wsmon -addr localhost:6060 -session <session-id>
# one-shot (print a single snapshot and exit): add -once
```

`wsmon` redraws in place on every `EventGoroutineSnapshot`, showing:

- the **goroutine spawn tree** built from `ParentID` (the `go` statement that
  spawned each goroutine), with the current goroutine marked;
- the **OS thread set** (runtime M's) and which goroutine each is running;
- the **created / exited** goid deltas since the previous snapshot;
- the **last stop event** (breakpoint / pause / step / exit) and session state.

Each time you Continue in the driver and hit the breakpoint again, `wsmon`
repaints with the new round's workers — the plumbing, end to end.

## Notes

- `wsmon` is a pure observer: it never sends run-control commands, so it cannot
  disturb the DAP driver. Many `wsmon` + DAP clients can share one session.
- The graphical observer is also read-only. It sends exactly one on-demand
  snapshot request after joining and on explicit Refresh only.
- Snapshots stream on **breakpoint / pause / entry**, not per single-step (steps
  stay cheap). Use the driver's breakpoints/continue to advance between frames.
- If the tracee is a stripped binary or stopped before runtime init, the snapshot
  degrades to a single synthetic goroutine; both observers render the degraded
  state rather than failing the debug session.

## Bounded goroutine events (protocol 1.3)

`EventGoroutineSnapshot` and `EventGoroutines` are the only two events that carry
an unbounded runtime collection, so they are the only two with a size contract
(`protocol.MaxGoroutineEventBytes`, 2 MiB, measured on the real marshalled
`Event`). There is no generic message cap, no `Location` truncation, and no
chunking or compression — everything else on the wire is unchanged.

What a consumer sees:

- **Deterministic content.** The current goroutine comes first, then its
  ancestors nearest-first, then the rest by ascending goid; the current thread
  leads the thread set, and a floor of 32 threads is packed before goroutines
  compete for the remaining budget. Two clients observing the same stop receive
  the same bytes.
- **Anchors and deltas are never dropped.** The current goroutine, the current
  thread, and the created/exited lifecycle deltas always survive. If not even the
  anchors fit, the event degrades to empty collections rather than failing; a
  degraded result that still overflows (only possible if the deltas alone do)
  reports `Oversized` rather than pretending to conform.
- **Deltas are not packed elements.** Because they are never trimmed,
  `created`/`exited` can legitimately exceed the element caps — the debugger's
  scan reaches 8192. A consumer must not apply its element cap to them; the byte
  contract is their bound.
- **`totals` is the honesty channel.** It appears *only* when elements were left
  off the wire or the debugger's runtime scan was clipped, carrying the
  **original** counts. Its presence alone means "this is not everything".
  `totals.clipped` means the scan stopped early, so `totals.goroutines` is a
  lower bound — render it as such (VS Code appends `+`).

A consumer should therefore treat the goroutine list as a bounded, ordered view
and read `totals` for the truth about scale — not assume the list is complete.
Element-count caps (`MaxSnapshotGoroutines` 5000, `MaxSnapshotThreads` 2048)
apply even when the bytes would allow more.

Clients that enforce their own limits should keep their transport ceiling
strictly **above** their decoder ceiling. A frame that breaks the contract is
then delivered and rejected as a deterministic protocol error — which must not be
retried — instead of being killed inside the WebSocket layer, where it is
indistinguishable from a flaky link and drives a pointless reconnect loop.

Two limits that are easy to get wrong:

- **Scope the fatal treatment to these two kinds.** Every other event is
  deliberately unbounded — `EventLocals`/`EventFrames`/`EventEvaluate` are
  broadcast to all clients and are limited only by the debugger's inspection
  budget, so a large variable expansion can legitimately exceed the goroutine
  budget. Treat those as transient, or an unrelated Variables-pane action will
  permanently kill an observer.
- **Do not apply the element caps to `created`/`exited`.** They are never
  trimmed, so they can exceed `MaxSnapshotGoroutines`; the debugger's scan
  reaches 8192.
