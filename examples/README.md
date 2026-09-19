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

In VS Code, install the bingo extension, set a suggested breakpoint, choose
**bingo: Debug example**, press F5, and select a level from the picker. The
server builds only the selected source directory with `-gcflags="all=-N -l"`;
no manual build, `just`, or pre-launch task is required for F5. Alternatively,
open a level's `main.go` and run **Bingo: Debug Go Package** without a launch
configuration. Execution runs to your breakpoints by default; add
`"stopOnEntry": true` only when you want the early entry stop.
The other root choices launch the spawntree demo from source or join an
existing bingo session. **Bingo Concurrency** automatically joins the selected
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
telemetry demo. Choose **bingo: Debug spawntree telemetry demo** in VS Code,
or build it separately with `just build-spawntree` for terminal clients and follow
[`docs/ConcurrencyTelemetry.md`](../docs/ConcurrencyTelemetry.md).
