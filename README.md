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
bingo -addr :6060 -dap-addr :6061
```

Point your editor's debug adapter at `127.0.0.1:6061`. The DAP client creates a
managed session on `launch`/`attach`; WebSocket observers join that same session
via `/ws?session=<id>` (the id is discoverable through `/api/sessions`, and the
adapter also prints it as a `console` output event). DAP covers the standard
debug loop (breakpoints, stepping, stack/variables, continue/pause); BinGo's
richer concurrency visualizations remain available to WebSocket clients on the
same session. See [AGENTS.md](AGENTS.md) → *DAP* for the architecture.

## Documentation

For detailed documentation, including client meeting minutes, existing solution comparision, project roadmap, installation instructions, usage guides, and API references, please read the [**Docs**](https://github.com/bingosuite/bingo/tree/main/docs).
