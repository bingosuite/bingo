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
concurrency view debuted in 0.3.0 and 0.3.1 added capability-safe managed-server
reuse; **0.4.0** speaks wire protocol **1.3**, whose bounded goroutine event
contract is what keeps the concurrency view alive on highly concurrent targets.
0.4.0 is the minimum supported version. Rerun the command to update, then run
**Developer: Reload Window** once so the active extension host loads the new
bundle. Package without installing with `just vscode-package`. Uninstall with:

```sh
code --uninstall-extension bingosuite.bingo
```

Generated binaries and VSIX files are ignored. Packaging rebuilds the native
binary and VSIX twice and requires both SHA-256 hashes to match.

## F5: connect or start

Install the matching platform VSIX once, select
**bingo DAP: launch example (stop on entry)** (or
**bingo DAP: join running session**) from the repository's Run and Debug
dropdown, press F5, and choose one of the five progressive targets. There is no
separate server-start or extension-host choice. In the default `auto` mode the extension:

1. checks `http://127.0.0.1:6060/api/health`;
2. reuses a compatible manual or managed bingo server;
3. if and only if the endpoint refuses the connection, starts its bundled
   server with management on `127.0.0.1:6060`, DAP on
   `127.0.0.1:4711`, and a 30-second server-owned idle grace;
4. waits up to five seconds for compatible health, then connects VS Code to DAP;
5. receives the DAP adapter's `bingo/session/v1` event and automatically joins
   that exact managed session over WebSocket in **Bingo Concurrency**.

Compatible health must advertise `dap.sessionEventVersion: 1`; an older server
that lacks managed-session discovery is reported as incompatible and is never
reused or replaced while it occupies the configured endpoint.

Concurrent VS Code extension hosts may both try to start. Listener binding
chooses the winner; a child that loses the race is harmless because both hosts
reuse the compatible winner. Requests in one extension host for the same
endpoint share one readiness operation.

The child is detached and logs to persistent extension storage. Open the
**bingo Server** output channel to see the absolute server log path. The
extension never kills a server, including one it spawned. Closing VS Code only
disconnects its client; bingo's `-idle-timeout` owns managed shutdown so DAP and
WebSocket clients can share the process safely.

If the management port answers with non-bingo HTTP or an incompatible bingo,
F5 fails without spawning over it. Errors include the endpoints and server log
path.

## Bingo Concurrency

The Bingo Activity Bar icon opens a session-aware graphical observer. The
extension host owns the WebSocket and validated model, so hiding or recreating
the webview does not lose the latest snapshot. Multiple debug sessions appear
in the selector; the status bar shows active goroutine/thread counts.

- Pan, zoom, fit, search, and keyboard arrows navigate the deterministic spawn
  tree. Parent links, current state, status, and thread badges remain stable
  across updates, including missing-parent and cyclic runtime data. Search runs
  against the full validated snapshot before the 500-node rendering cap, then
  fits a bounded match/ancestor layout.
- Selecting a goroutine shows wait reason, thread, and current/start/creation
  source locations. Thread cards and a bounded created/exited timeline provide
  physical and lifecycle context.
- Snapshots arrive at entry, breakpoints, and pauses—not per step. The view sends
  one read-only `GoroutineSnapshot` request after joining and only sends another
  when **Refresh** is explicit. It never sends run-control commands.
- **Bingo: Copy Concurrency Snapshot** copies validated JSON. **Select
  Concurrency Session**, **Refresh**, and **Fit** are available from the
  Command Palette; the Activity Bar icon and status item focus the view through
  VS Code's generated `bingo.concurrency.focus` command.

`bingo.concurrency.autoReveal` defaults to `true` and reveals the first session
once. Disable it to keep the view in the background. Connection, degraded,
empty, sequence-gap, and error states remain visible. Rendering uses a strict
nonce CSP, local bundles only, DOM `textContent` for tracee strings, VS Code
theme/high-contrast colors, labelled controls, and keyboard selection.
Treeitems expose hierarchy level, parent context, sibling position, selection,
and synchronized keyboard focus to assistive technology.

### Large targets and the 1.3 telemetry contract

A highly concurrent target can hold far more goroutines than fit in one
telemetry frame. Wire protocol 1.3 makes that a bounded, honest contract rather
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
  instance — stays recoverable rather than ending the view.
- Events the view does not consume (`Output`, `Locals`, `Frames`, `Goroutines`,
  `Evaluate`, breakpoint confirmations, `Restarted`) have their envelope
  validated but their body skipped, so another client's large data request can
  never take the view down.

## Configurations

### Launch a binary

```json
{
  "name": "bingo: Launch binary",
  "type": "bingo",
  "request": "launch",
  "program": "${workspaceFolder}/build/target/target",
  "args": [],
  "env": ["BINGO_MODE=debug"],
  "stopOnEntry": true,
  "serverMode": "auto",
  "managementHost": "127.0.0.1",
  "managementPort": 6060,
  "dapHost": "127.0.0.1",
  "dapPort": 4711,
  "serverReadyTimeoutMs": 5000,
  "managedIdleTimeoutMs": 30000
}
```

`env` is an array of `KEY=value` strings.

### Join a managed session

```json
{
  "name": "bingo: Join session",
  "type": "bingo",
  "request": "attach",
  "session": "replace-with-session-id",
  "serverMode": "auto",
  "managementHost": "127.0.0.1",
  "managementPort": 6060,
  "dapHost": "127.0.0.1",
  "dapPort": 4711
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
  "stopOnEntry": true,
  "serverMode": "auto",
  "managementHost": "127.0.0.1",
  "managementPort": 6060,
  "dapHost": "127.0.0.1",
  "dapPort": 4711
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
  "type": "bingo",
  "request": "launch",
  "program": "/workspace/build/target",
  "serverMode": "connectOnly",
  "dapHost": "debug.internal",
  "dapPort": 14711
}
```

Connect-only mode does not probe management health, inspect a bundled binary, or
spawn. Start and secure the reachable bingo server through that environment's
normal process manager. The concurrency observer uses the same configured
management host/port for its WebSocket connection.

## Manual server option

Autostart is not mandatory. A compatible server already listening on the
configured endpoints is reused:

```sh
just server
# persistent until interrupted; add -idle-timeout 30s for server-owned cleanup
```

## Contributor extension development

```sh
just vscode-dev       # build extension, native bundled server, and examples
just vscode-check     # clean install, lint, typecheck, tests, bundle/list smoke
just vscode-package   # native reproducible package + content verification
npm --prefix editors/vscode run test:integration # isolated Electron view/event acknowledgement
npm --prefix editors/vscode run e2e:packaged     # actual packaged server + DAP + WS graphical model
```

`just vscode-dev` restores the exact npm lockfile with lifecycle scripts
disabled, stages the source extension's native binary, builds its bundle, and
rebuilds the progressive examples. It does not add contributor tooling to the root Run and
Debug dropdown. To exercise the staged source extension, launch its Extension
Development Host explicitly from a terminal:

```sh
code --new-window --extensionDevelopmentPath="$PWD/editors/vscode" "$PWD"
```

Inside that window, select the normal example configuration. Ordinary target
debugging instead uses the installed VSIX and runs only
**bingo: build examples**, so F5 does not rebuild or codesign the
extension-local server.

The packaged E2E reserves unique loopback management/DAP ports, proves
compatible-instance reuse without a competing spawn, drives levels 1–5 (with a
nested level-5 tree), exercises select/filter/copy/refresh, and waits for the
managed server to exit by its idle policy. It signals only its exact captured
server PID, and only on failure.

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
