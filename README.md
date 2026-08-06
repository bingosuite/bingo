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

Start the server with a DAP listener:

```sh
just server                       # builds + runs with -addr :6060 -dap-addr :4711
# or, from a prebuilt binary:
bingo -addr :6060 -dap-addr :4711
```

`just server` starts both listeners with the defaults above; use `just server-ws`
for a WebSocket-only run (DAP disabled).

### VS Code companion extension

Build and install the repository's companion extension:

```sh
just vscode-package
code --install-extension dist/bingo.vsix --force
```

It contributes debugger type `"bingo"` and connects VS Code's built-in Debug UI
directly to `localhost:4711`. Keep Microsoft's Go extension installed for
`gopls`, navigation, formatting, and tests: the extensions coexist, and a bingo
debug configuration does **not** invoke or validate Delve (`dlv`) or take over
the Go extension's `"go"` debugger type. See
[editors/vscode/README.md](editors/vscode/README.md) for launch, session-join,
PID-attach, update, and uninstall instructions.

After installing, run `just server`, select a `"type": "bingo"` configuration
from `.vscode/launch.json`, and press F5.

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
so any UI can build a concurrency view on top of the data. `cmd/wsmon` is a
read-only terminal observer that joins a session and live-renders these streams:

```sh
go run ./cmd/wsmon -session <id>  # connects to -addr localhost:6060 by default
```

It never issues run-control commands, so it coexists with a DAP driver and other
WebSocket clients on the same session. For an end-to-end walkthrough — server, a
DAP driver (VS Code or `cmd/dapcli`), and the `wsmon` observer against one shared
session — see [docs/ConcurrencyTelemetry.md](docs/ConcurrencyTelemetry.md).

## Documentation

For detailed documentation, including client meeting minutes, existing solution comparision, project roadmap, installation instructions, usage guides, and API references, please read the [**Docs**](https://github.com/bingosuite/bingo/tree/main/docs).
