# BinGo

<div align="center">
    <img alt="bingo-logo" src="https://avatars.githubusercontent.com/u/247475762?s=400&u=f92f9e2a578d8651688fc67384c87b2d5ed30554&v=4" width="260" height="260" />
    <p><i><b>A multi-platform, visual concurrency debugger for GO.</b></i></p>
</div>

## Status

[![Go CI](https://github.com/bingosuite/bingo/actions/workflows/go.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/go.yml)
[![CodeQL](https://github.com/bingosuite/bingo/actions/workflows/codeql.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/codeql.yml)

## Overview

BinGo is a standalone visual concurrency debugger for Go that helps you:

- Visualize and understand goroutines, channels, and synchronization behavior
- Capture detailed runtime events and turn them into clear, interactive visualizations
- Use in a terminal UI or inside editors like VS Code or Vim
- Track goroutine lifecycles
- Inspect channels and mutexes
- Replay timelines of concurrent execution
- Detect deadlocks and goroutine leaks
- Debug tricky concurrency issues that traditional tools miss
- Extend with new frontends and integrations thanks to a modular, UI-agnostic core

## Supported Platforms

BinGo is currently built and tested on:

- `darwin/arm64` (Apple Silicon) — build with `-tags bingonative`
- `linux/amd64`

Builds on other GOOS/GOARCH combinations will fail with `undefined: newBackend` and similar errors from the [internal/debugger](internal/debugger/) package.

## Debug Adapter Protocol (DAP)

BinGo speaks the [Debug Adapter Protocol](https://microsoft.github.io/debug-adapter-protocol/)
alongside its native WebSocket protocol, so a standard IDE (VS Code, neovim) can
drive a debug session over a TCP socket while BinGo's own visual clients observe
— and optionally also drive — the **same** session in parallel.

Manual clients can start the server with a DAP listener:

```sh
just server                       # builds + runs with -addr :6060 -dap-addr :4711
# or, from a prebuilt binary:
bingo -addr :6060 -dap-addr :4711
```

`just server` starts both listeners with the defaults above; use `just server-ws`
for a WebSocket-only run (DAP disabled). The VS Code companion can instead
connect-or-start automatically.

### Server discovery and managed lifetime

Frontends can identify and reuse a compatible bingo process through
`GET /api/health` on the management/WebSocket listener:

```json
{
  "service": "bingo",
  "managementApiVersion": 1,
  "wireProtocolVersion": "1.2",
  "instanceId": "4dd4dfdd-7f55-41a5-bd95-c086ce6f3c2a",
  "dap": {
    "enabled": true,
    "address": "0.0.0.0:4711",
    "sessionEventVersion": 1
  },
  "managedIdleShutdown": {
    "enabled": false,
    "timeoutMs": 0
  },
  "sessionCount": 0
}
```

The response is non-cacheable. `managementApiVersion` versions this HTTP
contract independently of `wireProtocolVersion`; integrations should require
management API v1 and exact equality with the advertised bingo wire version.
Native WebSocket peers also enforce that equality on every command and event
envelope, including the initial welcome; a missing or mismatched `v` closes only
the incompatible connection. Graphical clients also require
`dap.sessionEventVersion: 1`, which guarantees the server emits
`bingo/session/v1` after managed session discovery. `instanceId` changes on
every process start. The DAP address is the actual bound listener address,
including the selected port when bingo was started with `-dap-addr ...:0`.

The intended process-owner flow is **connect or start**: health-check the known
management address, reuse a compatible bingo if present, otherwise start one
and let listener binding arbitrate concurrent startup attempts. Frontends do not
kill a shared bingo process directly.

Manual servers remain persistent by default. Process-managing integrations may
opt into server-owned idle shutdown:

```sh
bingo -addr 127.0.0.1:6060 -dap-addr 127.0.0.1:4711 -idle-timeout 30s
# equivalent development recipe:
just server darwin arm64 :6060 :4711 -idle-timeout 30s
```

The timeout is armed at startup and whenever the last managed session
disconnects. Any active session suppresses it, and a new session resets the full
grace period. Health polling and a DAP connection that has not yet created or
joined a session do not keep the process alive, so a process owner must allow
enough grace for its health check and DAP handshake. A zero or omitted timeout
disables idle shutdown. Positive values must be at least `1ms` and use whole
milliseconds so the enforced duration exactly matches `timeoutMs`.

### VS Code companion extension

Build and install the repository's companion extension:

```sh
just vscode-install
```

The capability-safe graphical concurrency runtime requires extension version **0.3.1 or
newer**. After installing or updating the VSIX, run **Developer: Reload Window**
once so the current extension host activates the new bundle.

It contributes debugger type `"bingo"` and connects VS Code's built-in Debug UI
directly to bingo. In its default `auto` mode it health-checks
`127.0.0.1:6060`, reuses a compatible server, or starts the matching bundled
server with DAP on `127.0.0.1:4711` and a 30-second server-owned idle grace.
Same-process requests coalesce; listener binding arbitrates concurrent extension
hosts. The detached child logs to persistent extension storage and the extension
never kills it. Keep Microsoft's Go extension installed for
`gopls`, navigation, formatting, and tests: the extensions coexist, and a bingo
debug configuration does **not** invoke or validate Delve (`dlv`) or take over
the Go extension's `"go"` debugger type. See
[editors/vscode/README.md](editors/vscode/README.md) for launch, session-join,
PID-attach, lifecycle fields, connect-only remote use, log paths, update, and
uninstall instructions.

Lifecycle configuration is explicit per launch: `serverMode`,
`managementHost`/`managementPort`, `dapHost`/`dapPort`,
`serverReadyTimeoutMs`, and `managedIdleTimeoutMs`. The defaults above use
`auto`; remote, forwarded, and custom endpoints must use
`"serverMode": "connectOnly"`, which neither probes nor spawns. Startup failures
name the endpoint and persistent log path in the **bingo Server** output channel.

Install the platform VSIX once (and update it when a newer version ships), then
select **bingo DAP: launch example (stop on entry)** from the repository's Run
and Debug dropdown, press F5, and choose one of the five
[progressive examples](examples/README.md). The only other root choice is
**bingo DAP: join running session**. The launch pre-task rebuilds all five
targets with debugger-friendly compiler flags; the installed extension health-checks
`127.0.0.1:6060`, reuses a compatible server or starts its bundled server, waits
for DAP on `127.0.0.1:4711`, and connects. No manual `just server` or separate
server-start/extension-host launch is required. The extension never kills the
shared server; after every client disconnects, a managed server exits after its
server-owned idle grace.

Contributor development of the extension source is deliberately separate from
normal target debugging. Run `just vscode-dev`, then launch VS Code explicitly
from a terminal with
`code --new-window --extensionDevelopmentPath="$PWD/editors/vscode" "$PWD"`.
Compatible manually-started servers remain supported and are reused.

The **Bingo Concurrency** Activity Bar view was introduced in 0.3.0; use 0.3.1
or newer so managed-server reuse requires the session-discovery capability.
Press F5, choose one of the five progressive examples, and the view
automatically follows the exact DAP-created session over WebSocket—no session
ID copy is needed. It visualizes the goroutine spawn tree, OS threads, current
goroutine, source locations, and bounded created/exited timeline while keeping
all run control in VS Code's Debug UI.

Other DAP clients can point at `127.0.0.1:4711`. The DAP client creates a
managed session on `launch`/`attach`; WebSocket observers join that same session
via `/ws?session=<id>` (the id is discoverable through `/api/sessions`, and the
adapter also prints it as a `console` output event). DAP covers the standard
debug loop (breakpoints, stepping, stack/variables, continue/pause); BinGo's
richer concurrency visualizations remain available to WebSocket clients on the
same session. See [AGENTS.md](AGENTS.md) → *DAP* for the architecture.

BinGo also ships an interactive DAP client, `cmd/dapcli`, that mirrors the
WebSocket CLI (`cmd/cli`) but drives a session over DAP:

```sh
just dapcli                       # create a session on launch (default -addr localhost:4711)
just dapcli -session <id>         # join an existing session as another client
```

Any number of `dapcli` and `cli` clients can drive and observe the **same**
session at once — start one, `launch` a target, then join from other terminals
with the announced session id.

## Concurrency telemetry (WebSocket)

Alongside the DAP debug loop, BinGo streams **concurrency telemetry** over its
native WebSocket protocol — a goroutine spawn hierarchy (parent/child linkage),
the live OS-thread set, and per-stop created/exited goroutine lifecycle deltas —
so UIs can build concurrency views on top of the data. The VS Code extension
joins automatically; `cmd/wsmon` remains the read-only terminal fallback:

```sh
go run ./cmd/wsmon -session <id>  # connects to -addr localhost:6060 by default
```

It never issues run-control commands, so it coexists with a DAP driver and other
WebSocket clients on the same session. For an end-to-end walkthrough — server, a
DAP driver (VS Code or `cmd/dapcli`), and the `wsmon` observer against one shared
session — see [docs/ConcurrencyTelemetry.md](docs/ConcurrencyTelemetry.md).

## Documentation

For detailed documentation, including client meeting minutes, existing solution comparision, project roadmap, installation instructions, usage guides, and API references, please read the [**Docs**](https://github.com/bingosuite/bingo/tree/main/docs).
