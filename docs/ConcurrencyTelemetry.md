# Concurrency telemetry — end-to-end runbook

bingo speaks two protocols against **one** debug session at the same time:

- **DAP** (`-dap-addr`) — an IDE (VS Code) or `cmd/dapcli` *drives* the session:
  breakpoints, stepping, continue/pause, stack/variables. This is the
  least-common-denominator debug loop.
- **WebSocket** (`-addr`) — any number of clients *observe* (and can also drive)
  the same session. The richer concurrency telemetry — the goroutine spawn tree,
  the OS-thread set, and created/exited lifecycle deltas — streams here as
  `EventGoroutineSnapshot`.

This runbook wires both together A–Z: a server, a **DAP driver** (VS Code or
`cmd/dapcli`), and a terminal **WS observer** (`cmd/wsmon`) that live-renders the
telemetry while the driver steps through a goroutine spawn tree.

```
             drives (DAP :4711)                observes (WS :6060)
  VS Code  ─────────────────────►  bingo server  ◄─────────────────  cmd/wsmon
  / dapcli                          (one session)                    (spawn tree,
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

  Reload the VS Code window after installation. The companion owns debugger type
  `"bingo"` and connects directly to bingo's DAP listener; it neither invokes
  nor validates `dlv`, and it does not replace the Go extension's `"go"` type.
  To update, rerun `just vscode-install`; uninstall with
  `code --uninstall-extension bingosuite.bingo`.

## 1. Demo target

`examples/spawntree` is a deterministic **main → supervisor → worker×3** spawn
tree that churns a fresh worker pool each round, so consecutive snapshots show
workers appearing in the `created` delta and leaving in `exited`. The intended
breakpoint is the `fmt.Printf` inside `worker` (`examples/spawntree/main.go:27`).
There is no separate manual target build step: the normal workspace launch
configuration runs **bingo: build spawntree** before F5. It uses the installed
VSIX's bundled server and does not rebuild or codesign extension sources.
Contributor source-extension work is a separate command-line path: run
`just vscode-dev`, then
`code --new-window --extensionDevelopmentPath="$PWD/editors/vscode" "$PWD"`.
It is intentionally absent from the root Run and Debug dropdown.

## 2. Drive with VS Code (DAP, automatic server)

1. Open this repo in VS Code.
2. Select **“bingo DAP: launch spawntree (stop on entry)”**. The only other root
   choice is **“bingo DAP: join running session”**.
3. Press F5. VS Code runs **“bingo: build spawntree”**. The companion
   health-checks `127.0.0.1:6060`,
   reuses a compatible server or starts its detached bundled server, waits up
   to `serverReadyTimeoutMs` (five seconds by default) for compatible readiness,
   then connects to DAP at `127.0.0.1:4711`. bingo launches the rebuilt
   `build/spawntree` and stops at entry.
4. Set a breakpoint on `examples/spawntree/main.go:27` and **Continue** — the
   tracee stops there with several worker goroutines alive.
5. Grab the **session id** so the observer can join: it is printed in the bingo
   DAP `console` output and is listed by
   `curl -s 127.0.0.1:6060/api/sessions`.

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
just server
just dapcli            # or: go run -tags bingonative ./cmd/dapcli -addr localhost:4711
# in the REPL:
launch ./build/spawntree      # creates a session, stops on entry
break examples/spawntree/main.go:27
c                             # continue → hits the breakpoint
```

`dapcli` prints the session id it created (also on the server console). Continue
again to churn rounds; each stop pushes a fresh telemetry snapshot to observers.

## 4. Observe the telemetry with cmd/wsmon

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
- Snapshots stream on **breakpoint / pause / entry**, not per single-step (steps
  stay cheap). Use the driver's breakpoints/continue to advance between frames.
- If the tracee is a stripped binary or stopped before runtime init, the snapshot
  degrades to a single synthetic goroutine; `wsmon` renders whatever is present.
