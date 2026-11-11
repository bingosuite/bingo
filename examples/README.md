# Progressive debugger examples

These five small targets introduce bingo one concept at a time. They are finite,
self-contained, and built without compiler optimization so source stepping and
locals remain easy to follow.

| Level | Concepts | Expected application goroutines | Suggested breakpoint | Inspect in DAP and WS telemetry |
| --- | --- | --- | --- | --- |
| [`level1-loop`](level1-loop/) | Sequential loop, locals, continue, step-over | `main` only | `main.go:8`, the running-total update | `step` and `total`; a single application goroutine |
| [`level2-channel`](level2-channel/) | First spawn, send/receive, close/range | `main` → producer | `main.go:14`, the channel send | producer creation and exit; channel value in each goroutine |
| [`level3-worker-pool`](level3-worker-pool/) | Three siblings, jobs/results channels, `WaitGroup` | `main` → worker ×3 | `worker` at `main.go:20` | sibling workers, current worker/job, workers exiting after `jobs` closes |
| [`level4-pipeline`](level4-pipeline/) | Owned channel closure, multi-stage pipeline, `select`, explicit cancellation | `main` → generate, square, retain | `retainEven` at `main.go:43` | stage fan-out, values moving between stages, all stages exiting after the third retained value cancels the context |
| [`level5-workflow`](level5-workflow/) | Nested workflows, parallel stages, deterministic error cancellation, shared mutex-protected ledger | `main` → workflow ×3 → stage ×3 | `inventoryStage` at `main.go:83` or `processOrder` at `main.go:137` | parent/child hierarchy, overlapping workflow lifetimes, canceled stages for `order-202`, shared ledger state |

## Build, run, and debug

Build every target with debugger-friendly compiler flags:

```sh
just build-examples
./build/examples/level1-loop
go run -race ./examples/level5-workflow
go test -race ./examples/...
```

In VS Code, install the bingo extension, open a target's `main.go`, set a suggested
breakpoint, choose **bingo: Debug active Go package**, and press F5. The
server builds only the selected source directory with `-gcflags="all=-N -l"`;
no manual build, `just`, or pre-launch task is required for F5. Alternatively,
open a level's `main.go` and run **Bingo: Debug Go Package** without a launch
configuration. Execution runs to your breakpoints by default; add
`"stopOnEntry": true` only when you want the early entry stop.
The other root choice joins an existing bingo session. New examples need no
launch configuration changes. **Bingo Concurrency** automatically joins the selected
DAP session and makes the increasing hierarchy visible from level 1's single
application goroutine through level 5's nested workflows and stages.

For a terminal-only DAP session, start `just server`, run `just dapcli`, and
launch a selected binary:

```text
launch ./build/examples/level3-worker-pool
break examples/level3-worker-pool/main.go:20
c
```

The graphical view requires no session-id copy. For a terminal-only observer,
join the reported session with `go run ./cmd/wsmon -session <id>`. Concurrent
worker and workflow status lines can interleave differently between runs, but
every level's final summary is sorted or otherwise deterministic.

[`spawntree`](spawntree/) remains the dedicated advanced hierarchy and lifecycle
telemetry demo. Open its `main.go` and use the same active-package launch in VS Code,
or build it separately with `just build-spawntree` for terminal clients and follow
[`docs/ConcurrencyTelemetry.md`](../docs/ConcurrencyTelemetry.md).

## Channel contents

Open [`channel-contents/main.go`](channel-contents/main.go), set a breakpoint on
the `fmt.Printf("inspect jobs: ...")` line, and run **Bingo: Debug Go Package**.
Expand `jobs` in Variables, a name-only Watch, or the Bingo inspector. It is a
closed channel with `len:3`, `cap:3`, and typed `[0]` / `[1]` / `[2]` values
`first`, `second`, `third` in receive order, despite its wrapped buffer.
Continue to confirm the program still receives those same values: inspection
never removes them. Unbuffered channels have no stored buffer to inspect;
blocked sender values are not reported as buffered contents.

## Nested goroutine graph

Open [`nested-goroutines/main.go`](nested-goroutines/main.go), set a breakpoint
on `fmt.Printf("round %d: all %d nested goroutines are ready\n", round, nodes)`,
and run **Bingo: Debug Go Package** from the Command Palette or Go editor button.
The server builds the example for you; F5 also selects the active Go package.

At that breakpoint, all 14 spawned goroutines are alive: `main` has two
children, each child has two children, and each grandchild has two children.
The application tree has four levels including `main` (plus any runtime
goroutines shown by the graph). Select a branch to inspect its `path` local,
such as `left/right/left`, and its creation site.

Continue to release the entire tree and build a fresh one. The breakpoint hits
three times, letting you compare parent links and created/exited lifecycle
deltas between rounds. Without a debugger, each round stays alive for two
seconds before exiting cleanly:

```sh
go run -race ./examples/nested-goroutines
```
