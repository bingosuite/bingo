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
- For the VS Code driver: the **Go extension** (the repo already sets
  `go.buildTags: bingonative` in `.vscode/settings.json`). The DAP driver uses
  VS Code's `debugServer` mode to connect straight to bingo's DAP port — the Go
  extension is only needed so VS Code recognises the `"type": "go"` config; it
  does **not** launch its own debugger. If the extension gets in the way, use the
  `cmd/dapcli` driver in step 3b instead — it proves the exact same coexistence.

## 1. Build the server and the demo target

```sh
just build                        # server → ./build/bingo/bingo_<os>_<arch> (codesigned on darwin)
go build -o build/spawntree ./examples/spawntree
```

`examples/spawntree` is a deterministic **main → supervisor → worker×3** spawn
tree that churns a fresh worker pool each round, so consecutive snapshots show
workers appearing in the `created` delta and leaving in `exited`. The intended
breakpoint is the `fmt.Printf` inside `worker` (`examples/spawntree/main.go:27`).

## 2. Start the server with DAP enabled

```sh
./build/bingo/bingo_darwin_arm64 -addr :6060 -dap-addr :4711 -v
# linux: ./build/bingo/bingo_linux_amd64 -addr :6060 -dap-addr :4711 -v
```

- `:6060` — WebSocket + REST (`/ws`, `/api/sessions`). `cmd/wsmon` connects here.
- `:4711` — DAP. VS Code / `cmd/dapcli` connect here.

Leave it running.

## 3a. Drive with VS Code (DAP)

1. Open this repo in VS Code.
2. Run the **“bingo DAP: launch spawntree (stop on entry)”** configuration
   (`.vscode/launch.json`). VS Code connects to `:4711`, bingo launches
   `build/spawntree`, and stops at entry.
3. Set a breakpoint on `examples/spawntree/main.go:27` and **Continue** — the
   tracee stops there with several worker goroutines alive.
4. Grab the **session id** so the observer can join: it is printed on the server
   console (`bingo session <id> ready …`) and in the bingo DAP `console` output,
   and is listed by `curl -s localhost:6060/api/sessions`.

> A second VS Code window can *join* the same session (observe/drive over DAP)
> with the **“bingo DAP: join running session”** config, which sends a DAP
> `attach` carrying only the `session` id (no `pid`) — bingo's join path.

## 3b. Drive with cmd/dapcli (DAP, no IDE)

Equivalent driver in a terminal — use this if VS Code isn't set up:

```sh
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
