# bingo

<div align="center">
  <img alt="bingo logo" src="https://avatars.githubusercontent.com/u/247475762?s=400&u=f92f9e2a578d8651688fc67384c87b2d5ed30554&v=4" width="240" height="240" />
  <p><strong>A visual concurrency debugger for Go.</strong></p>
</div>

[![Go CI](https://github.com/bingosuite/bingo/actions/workflows/go.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/go.yml)
[![Debugger E2E](https://github.com/bingosuite/bingo/actions/workflows/debugger-e2e.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/debugger-e2e.yml)
[![VS Code extension](https://github.com/bingosuite/bingo-vscode/actions/workflows/vscode-extension.yml/badge.svg)](https://github.com/bingosuite/bingo-vscode/actions/workflows/vscode-extension.yml)
[![CodeQL](https://github.com/bingosuite/bingo/actions/workflows/codeql.yml/badge.svg)](https://github.com/bingosuite/bingo/actions/workflows/codeql.yml)

bingo is a standalone debugger that combines a standard
[Debug Adapter Protocol (DAP)](https://microsoft.github.io/debug-adapter-protocol/)
debugging experience with live Go concurrency telemetry. Drive a session from
VS Code, Neovim, or the terminal while the same process streams its goroutine
hierarchy, OS-thread state, source locations, and lifecycle changes to visual or
terminal observers.

**Start here:** [Setup](docs/SETUP.md) ·
[VS Code](https://github.com/bingosuite/bingo-vscode) ·
[Neovim](https://github.com/bingosuite/bingo-nvim) ·
[Examples](examples/README.md) ·
[Architecture](AGENTS.md) ·
[Roadmap](docs/ROADMAP.md)

## Current capabilities

- **Go debugging:** launch, attach, restart, source breakpoints, pause, continue,
  step over, step in, and step out.
- **Program inspection:** stack frames, lexically scoped locals, expandable typed
  values, and name-based evaluate/hover support.
- **Concurrency visibility:** per-stop goroutine snapshots with a spawn tree,
  parent-child relationships, current goroutine and thread, creation sites, and
  created/exited lifecycle deltas.
- **Shared sessions:** DAP and WebSocket clients can drive or observe the same
  tracee at the same time.
- **Managed tooling:** the VS Code and Neovim companions discover, reuse, or
  start a compatible local server and let the server shut itself down after an
  idle grace period.
- **Progressive examples:** five debugger-friendly programs grow from a simple
  loop to channels, worker pools, pipelines, and nested concurrent workflows.

The wire protocol is currently **1.4**, including structured variable trees,
bounded goroutine events, and exact per-envelope version enforcement.

## Quick start

bingo supports **Apple Silicon macOS and x86-64 Linux**. In VS Code, install the
matching platform VSIX from [bingo-vscode releases](https://github.com/bingosuite/bingo-vscode/releases)
or its successful CI artifacts, using **Extensions: Install from VSIX...**.
Editor source builds and installation instructions live in the
[VS Code repository](https://github.com/bingosuite/bingo-vscode); [bingo-nvim](https://github.com/bingosuite/bingo-nvim) owns the Neovim plugin.
This repository builds the server and terminal clients.
See [repository ownership and CI setup](docs/EDITOR_REPOSITORIES.md) for the
build handoff and required GitHub secrets.

Reload VS Code, open a saved Go file in a runnable `main` package, and run
**Bingo: Debug Go Package**. The command uses that file's directory, or the
workspace root when it contains Go source; use a launch configuration for a
different path.
Without a launch configuration, F5 can also select bingo to debug a Go package.
The server compiles the selected source directory with debugging information
and launches it; no `just`, pre-launch build task, separate server terminal, or
extension-development host is needed. In this checkout, **bingo: Debug example**
lets you choose one of the five progressive examples.

Use the native Debug UI for breakpoints, stepping, stacks, and variables.
**Bingo Concurrency** opens beside source and follows the same session's
goroutine tree, threads, and lifecycle. The Activity Bar view remains available.

**Go is needed on the server's PATH to debug source, not to run a prebuilt bingo
binary or launch a prebuilt target.** See the [setup guide](docs/SETUP.md) for
installation choices and binary-mode configurations. Prefer another frontend?
Follow the [Neovim setup](docs/SETUP.md#neovim) (`:BingoDebug [directory]`) or the
[terminal-only setup](docs/SETUP.md#terminal-only), then use the
[concurrency telemetry runbook](docs/ConcurrencyTelemetry.md) for a complete
DAP-driver/WebSocket-observer walkthrough.

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

The **Bingo Concurrency** editor panel and Activity Bar view follow the exact
DAP-created session over WebSocket without copying a session ID and provide:

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

`bingo -version` prints the build version, commit, platform, Go toolchain, and
independent wire protocol version without starting a listener.

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
source-launch capability (`sourceLaunchVersion: 1`),
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
just                       # list commands; never starts a server
just build                 # build for the current supported host
just test                  # run Go tests
just vet                   # run Go vet with the host's native build tag
just integration           # run non-native integration tests
just e2e-linux             # native Linux acceptance suite
just e2e-darwin            # signed native macOS acceptance suite
```

On macOS, use the `just` recipes or pass `-tags bingonative` to Go commands.
Plain `go test ./...` cannot compile the Darwin backend.

Build a local native release preview:

```sh
just release-package       # dev preview: native server, terminal clients and checksums
```

The [release workflow](.github/workflows/release.yml) builds both supported
platforms on native runners from an exact release tag. Manual runs keep assets
in Actions by default; an explicit option attaches them to an existing draft
without publishing it. The published-release trigger also uploads verified
assets. See [release preparation](docs/SETUP.md#release-preparation) for the
artifact names, checksum verification, and native-debug verification limits.

## Resources

| Resource | Use it for |
| --- | --- |
| [Setup guide](docs/SETUP.md) | Prerequisites, installation, frontend setup, platform notes, and troubleshooting |
| [VS Code extension](https://github.com/bingosuite/bingo-vscode) | Launch, attach, join, remote endpoints, logs, and extension development |
| [Neovim companion](https://github.com/bingosuite/bingo-nvim) | `nvim-dap` configuration, managed startup, and session commands |
| [Concurrency telemetry](docs/ConcurrencyTelemetry.md) | End-to-end DAP driver and WebSocket observer runbook |
| [Progressive examples](examples/README.md) | Suggested breakpoints and expected telemetry |
| [Architecture and contributor guide](AGENTS.md) | System design, invariants, code conventions, and test commands |
| [Error handling](docs/ErrorHandling.md) | Error propagation and logging conventions |
| [Roadmap](docs/ROADMAP.md) | Planned work and project direction |
| [Issues](https://github.com/bingosuite/bingo/issues) | Bug reports and tracked work |
| [Discussions](https://github.com/bingosuite/bingo/discussions) | Questions, ideas, and community conversation |
| [MIT license](LICENSE) | Project licensing terms |
