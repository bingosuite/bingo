# bingo Debugger for VS Code

The `bingosuite.bingo` companion connects VS Code's Debug UI directly to bingo.
It owns debugger type `bingo`; it never registers `go`, invokes or validates
Delve (`dlv`), or calls the Microsoft Go extension. Keep `golang.go` installed
for gopls, navigation, formatting, and tests.

Platform-specific packages are available for:

- `darwin-arm64` (Apple Silicon), with the bundled server codesigned for native
  debugging;
- `linux-x64`, with the bundled linux/amd64 server.

## Install, update, and uninstall

From the repository root on a supported host:

```sh
just vscode-install
```

This builds, verifies, and installs `dist/bingo-<platform>.vsix`. The graphical
concurrency view debuted in 0.3.0, 0.3.1 added capability-safe managed-server
reuse, and 0.3.2 added wire 1.3's honest unknown stopped-goroutine rendering;
**0.4.0** adds wire 1.4's bounded goroutine event contract, which is what keeps
the concurrency view alive on highly concurrent targets. **0.4.1** adds
bounded-family error classification, truthful omission states, and manual
recovery after reconnect exhaustion. **0.4.2** rejects colliding management and
DAP listeners before managed startup. **0.5.0** adds call stacks, frame locals,
expandable variables, and source navigation to the goroutine inspector.
**0.6.0** keeps native Run and Debug alongside a reusable Bingo editor panel,
adds bounded creation-source previews, and bounds aggregate inspector work.
**0.7.0** adds configuration-free Go-package debugging, server-owned automatic
builds, cancellable startup, and source-launch capability checks. **0.7.1**
updates the WebSocket runtime and packaging dependencies for security fixes.
Use a matching 0.7.1 companion and server for the source quick start.
Rerun the command to update, then run
**Developer: Reload Window** once so the active extension host loads the new
bundle. Package without installing with `just vscode-package`. Uninstall with:

```sh
code --uninstall-extension bingosuite.bingo
```

Generated binaries and VSIX files are ignored. Packaging rebuilds the native
binary and VSIX twice and requires both SHA-256 hashes to match.

## Debug a Go package without configuration

Install the matching platform VSIX, open a saved Go file in a runnable `main`
package, set a breakpoint, then run **Bingo: Debug Go Package** from the Command
Palette or the Go editor's debug button. No `launch.json`, manual binary build,
`just`, or separate server command is needed. With no launch configuration,
press F5 and choose **bingo Debugger** for the same path.

The active Go file selects its **directory**, not just that file, so sibling
source files are included. With no active Go file, Bingo can use a selected or
single workspace root only if it contains Go source. A multi-root workspace
without a selected folder, an untitled Go buffer, or a root without Go files
gets an actionable message instead of an arbitrary guessed package. Open a file
under `cmd/your-app/` when the module root is not the package you want to run.

The server runs `go build` with `-gcflags="all=-N -l"` and owns the temporary
binary. **Go must be installed and on the server's PATH**; a bundled debugger is
not a bundled Go toolchain. Build progress and compiler errors come through DAP
in the native Debug Console. Execution runs to your breakpoints by default;
`"stopOnEntry": true` remains available for an explicit early stop.

In this repository, **bingo: Debug example** offers the five progressive source
packages. **bingo: Debug spawntree telemetry demo** and **bingo: Join running
session** are the other Run and Debug choices. F5 builds only the selected
package. The optional workspace build tasks remain for terminal binary clients.

## Connect or start

In the default `auto` mode the extension:

1. checks `http://127.0.0.1:6060/api/health`;
2. reuses a compatible manual or managed bingo server;
3. if and only if the endpoint refuses the connection, starts its bundled
   server with management on `127.0.0.1:6060`, DAP on
   `127.0.0.1:4711`, and a 30-second server-owned idle grace;
4. waits up to five seconds for compatible health, then connects VS Code to DAP;
5. receives the DAP adapter's `bingo/session/v1` event and automatically joins
   that exact managed session over WebSocket in **Bingo Concurrency**.

Compatible health must advertise both `dap.sessionEventVersion: 1` and
`dap.sourceLaunchVersion: 1`, in addition to exact wire version 1.4 and matching
management/DAP endpoints. An older 1.4 server without source-build support is
incompatible and is never reused or replaced while it occupies the endpoint.

Concurrent VS Code extension hosts may both try to start. Listener binding
chooses the winner; a child that loses the race is harmless because both hosts
reuse the compatible winner. Requests in one extension host for the same
endpoint share one readiness operation. Cancel from the **Preparing bingo
debugger** notification or VS Code's startup cancellation. Cancelling one
request does not affect another waiting request; cancelling the last stops
pending startup work and prevents a late binary/log lookup from spawning a
server. If a server has already started, it remains server-owned and exits
under its idle policy rather than being killed.

Auto mode requires distinct management and DAP endpoints and rejects an
identical host/port pair before probing or spawning. `connectOnly` remains
permissive for custom endpoint arrangements.

The child is detached and logs to persistent extension storage. Run
**Bingo: Show Server Output**, or choose **Show Logs** on a startup error, to
open the **bingo Server** output channel and find the absolute server log path. The
extension never kills a server, including one it spawned. Closing VS Code only
disconnects its client; bingo's `-idle-timeout` owns managed shutdown so DAP and
WebSocket clients can share the process safely.

If the management port answers with non-bingo HTTP or an incompatible bingo,
F5 fails without spawning over it. Update that server or configure a different
unused management/DAP port pair. Managed spawn/readiness errors include the
persistent log path; failures before a spawn have no new server log.

## Bingo Concurrency

By default, Bingo opens an editor panel **beside your source**, leaving the
native **Run and Debug** sidebar available for Watch, Breakpoints, Call Stack,
Variables, and the normal debug controls. Debug Console remains VS Code's native
panel. These native views cannot all be embedded through supported extension
APIs, so Bingo complements them rather than replacing or fighting their focus.
The Bingo Activity Bar icon still opens the same observer as a manual sidebar.
Its title-bar Fit action fits only that sidebar and never opens or focuses the
editor panel. **Bingo: Fit Concurrency Tree in Editor** explicitly opens/fits
the editor; each webview's own Fit button stays local to that surface.
The extension host owns the WebSocket and validated model, so hiding or recreating
the webview does not lose the latest snapshot. Multiple debug sessions appear
in the selector; the status bar shows active goroutine/thread counts.

- Pan, zoom, fit, search, and keyboard arrows navigate the deterministic spawn
  tree. Parent links, current state, status, and thread badges remain stable
  across updates, including missing-parent and cyclic runtime data. Search runs
  against the full validated snapshot before the 500-node rendering cap, then
  fits a bounded match/ancestor layout.
- Selecting any goroutine prominently shows its parent goid, the recorded
  **Created** location (the `go` statement), and the **Entry** location (the start
  function, which can be a compiler-generated wrapper). **Spawned here** displays
  nearby local source with the recorded creation line highlighted and an explicit
  **Open creation source** action. This works independently of stack inspection,
  including for a goroutine that is not stopped.
- The debugger inspector shows runtime metadata,
  clickable current/start/creation locations, the stopped goroutine's DAP call
  stack, frame selection, locals, and lazy variable expansion. Stack and locals
  are currently available only for the goroutine that is actually stopped;
  selecting another node explains that limit and offers a shortcut back to the
  stopped goroutine rather than showing misleading data.
- Source links in goroutine metadata and stack frames open the exact file and
  line in the editor. Thread cards and a bounded created/exited timeline provide
  physical and lifecycle context.
- Snapshots arrive at entry, breakpoints, and pauses—not per step. The view sends
  one read-only `GoroutineSnapshot` request after joining and only sends another
  when **Refresh** is explicit. Stack, scope, and variable reads use standard
  read-only DAP requests, including after step stops where the graph intentionally
  keeps its last snapshot. The view never sends run-control commands.
- **Bingo: Copy Concurrency Snapshot** copies validated JSON. **Select
  Concurrency Session**, **Refresh**, and **Fit** are available from the
  Command Palette. **Open Concurrency Beside Source** and the status item open
  or reuse the editor panel; the Activity Bar icon and VS Code's generated
  `bingo.concurrency.focus` command remain manual sidebar access.

`bingo.concurrency.autoReveal` defaults to `true`. A new session opens the editor
panel once without taking focus away from source. Later stops and restarts do
not create panels or steal focus. Closing the panel keeps it closed for that
session; reopen it explicitly from the command or status item. Disable auto-reveal
for manual-only access. Neither surface changes global debug settings, moves
native views, or terminates the shared server when closed.
A synthetic degraded snapshot explicitly means
that rich Go runtime concurrency metadata was unavailable—commonly at the early
entry stop—not that DAP stack frames and locals are necessarily unavailable.
Connection, degraded, empty, sequence-gap, and error states remain visible.
Rendering uses a strict nonce CSP, local bundles only, DOM `textContent` for
tracee strings, VS Code theme/high-contrast colors, labelled controls, and
keyboard selection.
Treeitems expose hierarchy level, parent context, sibling position, selection,
and synchronized keyboard focus to assistive technology.

### Local creation source and inspector bounds

Automatic source reads require a trusted workspace and a canonical local path
inside one of its folders. Traversal and symlink escapes, non-regular files,
missing/unreadable files, invalid UTF-8, and files above **256 KiB / 10,000 lines**
produce an explicit unavailable state. Previews show at most **nine lines**, each
at most **300 UTF-16 code units** including a clipping marker. Reads are serialized;
rapid selection changes retain only the latest target. A **16-entry** cache belongs
to one session/snapshot and is cleared by Refresh.

The preview reads the file on disk, not unsaved edits. It is **not verified
against the compiled binary**: edited source can move the recorded line, so the
highlight does not prove that the displayed statement created this goroutine.
Source links use native file navigation, never commands or URLs from target data.
Messages identify the rendered document, revision, session, selection, and a
metadata location kind/frame ID; the host chooses the authoritative path.
Replaced documents and stale selections cannot navigate or inspect a newer stop.

Variable rendering has a **1,000-node / 20-level** ceiling and expands each shared
reference subtree only once, with explicit shared/circular/limit indications.
The read-only inspector caps each frame generation at **2,000 variable nodes,
256 KiB of UTF-8 variable text, 256 references, and 128 requests**. Individual
responses retain at most 200 frames, 32 scopes, or 500 variables, with independent
response shape/text/depth limits. At most four requests remain in flight, with
five-second UI deadlines; timed-out wire requests retain their slot until they
settle. Resume invalidates inspection immediately, even before WebSocket state
catches up, and stale scopes cannot fan out more variable requests.
Inspection remains read-only DAP; unused WebSocket Frames/Locals broadcasts are
not consumed, and neither the graph nor its source preview drives execution.

### Large targets and the 1.4 telemetry contract

A highly concurrent target can hold far more goroutines than fit in one
telemetry frame. Wire protocol 1.4 makes that a bounded, honest contract rather
than a failure:

- The debugger packs `GoroutineSnapshot` and `Goroutines` to a **2 MiB** budget
  scoped to those two events only, keeping the current goroutine, its ancestors,
  and the current thread first so the tree you are looking at stays intact.
- When anything is left out, the payload carries the server's **original**
  totals. The view then shows `shown of total` rather than presenting a
  truncated set as the whole picture, and notes server-side omissions separately
  from the view's own filter and 500-node render cap.
- If the debugger's runtime scan itself hit its ceiling, the totals are a lower
  bound and are marked with a trailing `+`.
- The WebSocket transport limit sits deliberately **above** the decoder limit, so
  a frame that breaks the contract is reported as a protocol error instead of
  looking like a dropped connection. Protocol errors are terminal: the observer
  stops instead of reconnecting into the identical failure. Ordinary connection
  drops still reconnect as before, and an oversized event that the contract does
  not cover — a very large `Locals`/`Frames`/`Evaluate` broadcast, for
  instance — stays recoverable rather than ending the view; so is a payload that
  merely exceeds those caps on an event the contract does not bound, such as an
  `Error` echoing a long Watch expression. **Refresh** is the manual recovery for
  every terminal state — a protocol error or an exhausted reconnect ladder — and
  redials rather than asking a socket that is gone.
- An empty tree or thread list is attributed to the cause the evidence supports:
  a filter that matched nothing, elements the event omitted, or a runtime the
  debugger could not read. Each collection's shortfall is reported once, beside
  its own data.
- Events the view does not consume (`Output`, `Locals`, `Frames`, `Goroutines`,
  `Evaluate`, breakpoint confirmations, `Restarted`) have their envelope
  validated but their body skipped, so another client's large data request can
  never take the view down.

## Configurations

### Build and debug a package

For a repeatable target, save this in `.vscode/launch.json`'s `configurations`:

```json
{
  "name": "bingo: Debug Go Package",
  "type": "bingo",
  "request": "launch",
  "mode": "debug",
  "program": "${workspaceFolder}/cmd/my-app"
}
```

`program` must name a server-local directory, not a `.go` file, import path, or
`go test` target. Optional `cwd` sets the target's working directory and the base
for a relative `program`. Without `cwd`, source mode uses the resolved package
directory. Paths containing spaces are passed as paths, not shell commands.
`args`, `env`, and `stopOnEntry` work in both launch modes. `env` is an array of
`KEY=value` strings; the server applies overrides to the build and target.

### Launch a binary

```json
{
  "name": "bingo: Launch binary",
  "type": "bingo",
  "request": "launch",
  "mode": "exec",
  "program": "${workspaceFolder}/build/target/target",
  "args": [],
  "env": ["BINGO_MODE=debug"],
  "stopOnEntry": true
}
```

Existing launch configurations with a `program` but **no `mode` still launch a
binary**. Bingo does not silently reinterpret them as source. In exec mode an
omitted or empty `cwd` retains the server's working directory.

### Join a managed session

```json
{
  "name": "bingo: Join session",
  "type": "bingo",
  "request": "attach",
  "session": "replace-with-session-id"
}
```

Joining does not relaunch, reattach, or automatically resume the shared session.

### Attach to an OS process

```json
{
  "name": "bingo: Attach to process",
  "type": "bingo",
  "request": "attach",
  "pid": 1234,
  "binaryPath": "/absolute/path/to/the/binary",
  "stopOnEntry": true
}
```

`binaryPath` is optional, but bingo needs its DWARF data for source breakpoints,
stack frames, and locals.

## Lifecycle fields

| Field | Default | Meaning |
| --- | --- | --- |
| `serverMode` | `"auto"` | Health-check and reuse/start locally. Use `"connectOnly"` for remote or custom endpoints. |
| `managementHost` | `"127.0.0.1"` | Management/health host. Auto mode requires this exact IPv4 loopback. |
| `managementPort` | `6060` | Management, REST, and WebSocket port. |
| `dapHost` | `"127.0.0.1"` | DAP connect host. Auto mode requires this exact IPv4 loopback. |
| `dapPort` | `4711` | DAP connect/listen port. |
| `serverReadyTimeoutMs` | `5000` | Bounded wait for compatible health. |
| `managedIdleTimeoutMs` | `30000` | Idle grace passed only to a server the extension starts. |

The explicit IPv4 defaults match bingo's `tcp4` DAP listener and avoid older
Node runtimes resolving `localhost` to IPv6 first.

### Remote and custom endpoints

Autostart is local-only. For SSH, dev containers, Codespaces, port forwarding,
or any custom host, explicitly select connect-only mode:

```json
{
  "name": "bingo: Remote Go Package",
  "type": "bingo",
  "request": "launch",
  "mode": "debug",
  "program": "/workspace/cmd/my-app",
  "serverMode": "connectOnly",
  "managementHost": "debug.internal",
  "managementPort": 16060,
  "dapHost": "debug.internal",
  "dapPort": 14711
}
```

Connect-only mode does not probe management health, inspect a bundled binary, or
spawn. Start and secure the reachable bingo server through that environment's
normal process manager. The concurrency observer uses the same configured
management host/port for its WebSocket connection. Both `program` and `cwd`
are **server-local**, not paths on the VS Code client. Source mode requires a
server with source-launch support even though connect-only mode deliberately
skips compatibility probes.

## Manual server option

Autostart is not mandatory. A compatible server already listening on the
configured endpoints is reused:

```sh
just server
# persistent until interrupted; add -idle-timeout 30s for server-owned cleanup
```

## Contributor extension development

```sh
just vscode-dev       # stage the native server and extension for a development host
just vscode-check     # clean install, lint, typecheck, tests, bundle/list smoke
just vscode-package   # native reproducible package + content verification
npm --prefix editors/vscode run test:integration # isolated Electron + fake DAP + displayed DOM
VSCODE_TEST_VERSION=1.85.2 npm --prefix editors/vscode run test:integration
VSCODE_TEST_VERSION=1.137.0 npm --prefix editors/vscode run test:integration
npm --prefix editors/vscode run e2e:packaged     # server-built source packages + native DAP + WS + DOM
```

`just vscode-dev` restores the exact npm lockfile with lifecycle scripts
disabled, stages the source extension's native binary, and builds its bundle.
It does not pre-build the examples: the server builds the selected source
package when debugging starts. A failed native build or signing step preserves
the previously prepared binary and target marker.
Contributor tooling is not added to the root Run and Debug dropdown. To exercise
the staged source extension, launch its Extension
Development Host explicitly from a terminal:

```sh
code --new-window --extensionDevelopmentPath="$PWD/editors/vscode" "$PWD"
```

Inside that window, select the normal example configuration. Ordinary target
debugging instead uses the installed VSIX and asks the server to build the
selected source package, so F5 does not rebuild or codesign the extension-local
server.

Electron tests run in isolated profiles, not the user's installed editor. The
default runner is pinned to 1.107.1; the compatibility matrix additionally pins
the supported 1.85.2 floor and 1.137.0. The host bundle targets Node 18 for the
floor's extension host. CI and local overrides use `VSCODE_TEST_VERSION`; the
Electron suite asserts `vscode.version` equals the runner's requested version,
including its default, before exercising the UI. Fake DAP tests exercise the real
editor/webview lifecycle; native packaged tests exercise the real debugger with
a lightweight DOM renderer.
These are complementary layers, not a claim that the native target ran inside
Electron.

The packaged E2E reserves unique loopback management/DAP ports, proves
compatible-instance reuse without a competing spawn, and drives levels 1–5
through the same `goPackageConfiguration` helper as **Debug Go Package**. The
packaged server builds each source directory in `mode: "debug"` with its package
`cwd`; neither the harness nor CI prebuilds `build/examples` targets. Discovery
and `initialized` share a 130-second deadline to allow the server's two-minute
build budget. The test explicitly enables `stopOnEntry` to preserve entry
inspection, then checks real breakpoints, locals, a nested level-5 tree,
displayed creation source and variable expansion, select/filter/copy/refresh,
single-terminate behavior with the observer still connected, and server-owned
idle exit. It signals only its exact captured server PID, and only on failure.

## Troubleshooting

- **Unsupported platform:** auto mode packages only linux/x64 and darwin/arm64.
  Use a matching package or `connectOnly`.
- **Endpoint occupied/incompatible:** inspect the process on the reported
  management port. The extension will not replace it.
- **Startup timeout/child exit:** open **bingo Server** and inspect the persistent
  log path printed there.
- **Remote endpoint rejected:** set `"serverMode": "connectOnly"`.
- **Server remains after VS Code closes:** expected while another session is
  active or during the idle grace. The extension never sends a kill signal.
- **Concurrency view is empty:** stop at entry, a breakpoint, or Pause, then use
  **Bingo: Refresh Concurrency Snapshot**. Stripped/pre-runtime targets can
  degrade to a synthetic single goroutine. `cmd/wsmon` remains available as a
  terminal observer.
