# bingo

<div align="center">
  <img alt="bingo logo" src="https://avatars.githubusercontent.com/u/247475762?s=400&u=f92f9e2a578d8651688fc67384c87b2d5ed30554&v=4" width="240" height="240" />
  <p><strong>A visual concurrency debugger for Go.</strong></p>
</div>

[![Go CI](https://github.com/bingosuite/bingo/actions/workflows/go.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/go.yml)
[![Debugger E2E](https://github.com/bingosuite/bingo/actions/workflows/debugger-e2e.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/debugger-e2e.yml)
[![VS Code extension](https://github.com/bingosuite/bingo/actions/workflows/vscode-extension.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/vscode-extension.yml)
[![CodeQL](https://github.com/bingosuite/bingo/actions/workflows/codeql.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/codeql.yml)

bingo is a standalone debugger that combines a standard
[Debug Adapter Protocol (DAP)](https://microsoft.github.io/debug-adapter-protocol/)
debugging experience with live Go concurrency telemetry. Drive a session from
VS Code, Neovim, or the terminal while the same process streams its goroutine
hierarchy, OS-thread state, source locations, and lifecycle changes to visual or
terminal observers.

## Current capabilities

- **Go debugging:** launch, attach, restart, source breakpoints, pause, continue,
  step over, step in, and step out.
- **Program inspection:** stack frames, lexically scoped locals, expandable typed
  values, and name-based evaluate/hover support.
- **Concurrency visibility:** per-stop goroutine snapshots with a spawn tree, parent-child
  relationships, current goroutine and thread, creation sites, and created/exited
  lifecycle deltas.
- **Shared sessions:** DAP and WebSocket clients can drive or observe the same
  tracee at the same time.
- **Managed tooling:** the VS Code and Neovim companions discover, reuse, or
  start a compatible local server and let the server shut itself down after an
  idle grace period.
- **Progressive examples:** five debugger-friendly programs grow from a simple
  loop to channels, worker pools, pipelines, and nested concurrent workflows.

The wire protocol is currently **1.2**, including structured variable trees and
the evaluate command. Recent debugger work also hardened breakpoint ownership,
restart reconciliation, exact protocol-version enforcement, Linux signal
forwarding, and concurrent single-step behavior.

## Getting started

### Requirements

| Requirement | Supported version |
| --- | --- |
| Platform | Apple Silicon macOS (`darwin/arm64`) or x86-64 Linux (`linux/amd64`) |
| Go toolchain | 1.25.5 |
| Task runner | [`just`](https://github.com/casey/just) |
| VS Code | 1.85+ |
| Extension build | Node.js 22.x, npm, and the `code` CLI |
| Neovim | 0.11.7+ with [`nvim-dap`](https://github.com/mfussenegger/nvim-dap) |

On macOS, the build recipes enable the `bingonative` tag and ad hoc sign native
binaries with the debugger entitlement.

### VS Code (recommended)

Clone the repository and build/install the platform-specific companion
extension:

```sh
git clone https://github.com/bingosuite/bingo.git
cd bingo
just vscode-install
```

Then:

1. Run **Developer: Reload Window** in VS Code.
2. Open **Run and Debug** and select **bingo DAP: launch example (stop on entry)**.
3. Press F5 and choose one of the five progressive examples.
4. Use VS Code's Debug UI for breakpoints, stepping, stack frames, and variables.
5. The **Bingo Concurrency** editor opens automatically for the session. The
   compact Activity Bar view remains available, and selecting a goroutine can
   open its recorded spawn site beside the visualization.

The launch task rebuilds the selected examples with optimizations and inlining
disabled. No separate server process or extension-development host is required.
Keep the Go extension installed for `gopls`, formatting, navigation, and tests;
bingo owns only debugger type `"bingo"` and does not invoke Delve.

See the [VS Code extension guide](editors/vscode/README.md) for custom launch
configurations, PID attach, joining a session, remote `connectOnly` mode, server
logs, and extension development.

### Neovim

Prepare the native server and add `editors/neovim` to Neovim's runtime path:

```sh
just neovim-prepare
```

```lua
{
  dir = "/absolute/path/to/bingo/editors/neovim",
  dependencies = { "mfussenegger/nvim-dap" },
  config = function()
    require("bingo").setup()
  end,
}
```

The companion registers a function-form `nvim-dap` adapter with the same
managed `auto` and remote `connectOnly` modes as VS Code. Use `:BingoLaunch`,
`:BingoAttach`, or `:BingoJoin`, then drive breakpoints, stepping, stacks, and
variables through normal `nvim-dap` commands. `:BingoSession` shows the managed
session ID for a read-only `wsmon` telemetry observer. See the
[Neovim guide](editors/neovim/README.md) for setup and configuration.

### Terminal-only workflow

Build the examples and start both the WebSocket management listener and DAP
listener:

```sh
just build-examples
just server
```

In a second terminal, start the interactive DAP client:

```sh
just dapcli
```

Launch and debug an example from its prompt:

```text
launch ./build/examples/level3-worker-pool
break examples/level3-worker-pool/main.go:20
c
```

The client announces the managed session ID. A third terminal can join it with
the read-only telemetry monitor:

```sh
go run ./cmd/wsmon -session <session-id>
```

Use `just dapcli -session <session-id>` or `just cli -session <session-id>` to
join as another driver. For the complete DAP-drives/WebSocket-observes walkthrough,
see [Concurrency telemetry](docs/ConcurrencyTelemetry.md).

## How it works

```text
VS Code / Neovim / dapcli ─ DAP ──┐
                                  │
Concurrency view / wsmon ─ WS ────┼──> server ──> session hub ──> debugger
                                  │                         │
cli ───────────────────── WS ─────┘                         └──> Go tracee
```

The server creates one hub per managed session. DAP clients plug into the same
hub boundary as native WebSocket clients, so all clients share event fan-out,
session state, breakpoint state, and run control. DAP provides the standard IDE
debug loop; the WebSocket protocol carries bingo's richer concurrency stream.

The native debugger uses `ptrace` on Linux and Mach exception ports on macOS.
Every managed session is isolated, and every outbound event receives one
monotonic hub sequence number.

## VS Code concurrency view

The repository currently packages extension version **0.4.0**. Its primary
concurrency surface is a full editor tab, with a compact Activity Bar view for
quick inspection. It automatically follows the exact DAP-created session over
WebSocket without copying a session ID and provides:

- a deterministic, bounded goroutine spawn tree;
- current goroutine and OS-thread state;
- start, current, and creation source locations;
- created/exited lifecycle history;
- filtering across the full validated snapshot;
- keyboard-accessible navigation, fit/zoom controls, and snapshot export.

Run control stays in VS Code's Debug UI. The extension-side observer is read-only
and requests a fresh concurrency snapshot only after joining or when explicitly
refreshed.

## Server and protocol

`just server` builds bingo and starts the default loopback listeners:

| Interface | Default | Purpose |
| --- | --- | --- |
| Management + WebSocket | `127.0.0.1:6060` | Health, managed sessions, native clients, telemetry |
| DAP | `127.0.0.1:4711` | IDE and DAP client debugging |

Equivalent binary invocation:

```sh
bingo -addr 127.0.0.1:6060 -dap-addr 127.0.0.1:4711
```

Use `just server-ws` for a WebSocket-only server. Manual servers are persistent
by default; process-managing integrations can opt into server-owned cleanup:

```sh
bingo \
  -addr 127.0.0.1:6060 \
  -dap-addr 127.0.0.1:4711 \
  -idle-timeout 30s
```

Frontends discover compatibility through `GET /api/health`. The response
separately advertises management API version 1, the exact wire protocol version,
the process instance ID, resolved DAP listener, DAP session-event version,
managed idle policy, and session count. Native peers also validate the wire
version on every envelope; an incompatible peer is disconnected without
affecting the shared session.

Both default listeners bind to IPv4 loopback. Non-loopback binds are
unauthenticated and should only be exposed on a trusted network or behind an
authenticated transport.

## Progressive examples

| Example | Focus |
| --- | --- |
| [`level1-loop`](examples/level1-loop/) | Sequential stepping and locals |
| [`level2-channel`](examples/level2-channel/) | Goroutine creation and channel flow |
| [`level3-worker-pool`](examples/level3-worker-pool/) | Sibling workers and lifecycle changes |
| [`level4-pipeline`](examples/level4-pipeline/) | Pipeline stages, `select`, and cancellation |
| [`level5-workflow`](examples/level5-workflow/) | Nested concurrency, errors, and shared state |

See the [examples guide](examples/README.md) for suggested breakpoints and what
to inspect at each level. The separate
[`spawntree`](examples/spawntree/) target is the focused hierarchy and lifecycle
telemetry demo.

## Supported platforms

| Platform | Backend | Notes |
| --- | --- | --- |
| `darwin/arm64` | Mach exception ports | Requires `-tags bingonative` and the debugger entitlement |
| `linux/amd64` | `ptrace` | Requires native ptrace access |

Other GOOS/GOARCH combinations are not supported and fail to build because no
backend is registered.

## Development

```sh
just build                 # build for the current supported host
just test                  # run Go tests
just integration           # run non-native integration tests
just vscode-check          # lint, typecheck, test, and bundle the extension
just neovim-check          # parse and test the Neovim companion
just e2e-linux             # native Linux acceptance suite
just e2e-darwin            # signed native macOS acceptance suite
```

On macOS, use the `just` recipes or pass `-tags bingonative` to Go commands.
Plain `go test ./...` cannot compile the Darwin backend.

## Documentation

| Guide | Contents |
| --- | --- |
| [VS Code extension](editors/vscode/README.md) | Install, configure, attach, join, troubleshoot, and develop |
| [Neovim companion](editors/neovim/README.md) | Configure `nvim-dap`, managed startup, launch, attach, and join |
| [Concurrency telemetry](docs/ConcurrencyTelemetry.md) | End-to-end DAP driver and WebSocket observer runbook |
| [Progressive examples](examples/README.md) | Example concepts, breakpoints, and expected telemetry |
| [Error handling](docs/ErrorHandling.md) | Error propagation and logging conventions |
| [Roadmap](docs/ROADMAP.md) | Planned work |
| [AGENTS.md](AGENTS.md) | Architecture, invariants, testing, and contributor guidance |
