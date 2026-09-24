# bingo

**A multi-platform, visual concurrency debugger for Go.**

bingo is a standalone debugger that combines a standard [Debug Adapter
Protocol (DAP)](https://microsoft.github.io/debug-adapter-protocol/)
debugging experience with live Go concurrency telemetry. Drive a session from
VS Code, Neovim, or the terminal while the same process streams its goroutine
hierarchy, OS-thread state, source locations, and lifecycle changes to visual
or terminal observers.

In other words: you get normal breakpoints, stepping, and variable
inspection, *plus* a live view of every goroutine your program spawns — how
it was created, what it's doing now, and how it relates to the rest of the
program — updated as you debug.

[Get Bingo Latest](https://github.com/bingosuite/bingo/releases)

## What it does

- **Standard Go debugging** — launch, attach, restart, breakpoints, pause,
  continue, step over/in/out, stack frames, scoped locals, and
  evaluate/hover support.
- **Concurrency visibility** — a per-stop goroutine spawn tree with
  parent-child relationships, the current goroutine and OS thread, creation
  sites, and created/exited lifecycle deltas.
- **Shared sessions** — DAP clients (your editor) and WebSocket clients (a
  visual observer) can drive or watch the same debug session at the same
  time.
- **Editor integrations** — companions for VS Code and Neovim that
  discover, reuse, or start a compatible local server automatically.

The wire protocol is currently **1.4**, including structured variable trees,
bounded goroutine events, and exact per-envelope version enforcement.

## How it works

```
VS Code / Neovim / dapcli ─ DAP ──┐
                                  │
Concurrency view / wsmon ─ WS ────┼──> server ──> session hub ──> debugger
                                  │                         │
cli ───────────────────── WS ─────┘                         └──> Go tracee
```

The server creates one hub per managed session. DAP clients plug into the
same hub boundary as native WebSocket clients, so everyone shares event
fan-out, session state, breakpoint state, and run control. DAP provides the
standard IDE debug loop; the WebSocket protocol carries bingo's richer
concurrency stream. The native debugger uses `ptrace` on Linux and Mach
exception ports on macOS.

## Download

bingo is built from source today; native release artifacts (VS Code `.vsix`,
a Neovim companion archive, and terminal binaries) are published from the
[Releases page](https://github.com/bingosuite/bingo/releases) as they become
available for a given version. Check there first for a prebuilt package
matching your platform before building from source.

| Platform | Backend | Notes |
| --- | --- | --- |
| `linux/amd64` | `ptrace` | Requires native ptrace access |
| `darwin/arm64` | Mach exception ports | Requires the `bingonative` build tag and the debugger entitlement |

Other `GOOS`/`GOARCH` combinations aren't supported and won't build, since no
backend is registered for them.
