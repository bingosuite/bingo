# bingo for Neovim

The Neovim companion registers bingo as a TCP adapter for
[`nvim-dap`](https://github.com/mfussenegger/nvim-dap). `nvim-dap` owns the
standard debug UI and drives launch, attach, breakpoints, stepping, stack frames,
variables, and evaluate requests. bingo remains a separate server and never
invokes Delve.

## Requirements

- Neovim 0.11.7 or newer;
- `mfussenegger/nvim-dap`;
- Apple Silicon macOS or x86-64 Linux;
- a bingo server prepared with `just neovim-prepare`, available as `bingo` on
  `PATH`, or configured explicitly.

## Install from this repository

Prepare the platform-native server:

```sh
just neovim-prepare
```

Add `editors/neovim` to Neovim's runtime path with your plugin manager. For a
local checkout with `lazy.nvim`:

```lua
{
  dir = "/absolute/path/to/bingo/editors/neovim",
  dependencies = { "mfussenegger/nvim-dap" },
  config = function()
    require("bingo").setup()
  end,
}
```

The generated `editors/neovim/bin/bingo` is platform-specific and ignored by
Git. If the plugin directory is installed separately, put a compatible `bingo`
binary on `PATH` or pass `server.binary`.

## Managed connect-or-start

The default `auto` mode mirrors the VS Code companion:

1. probe `http://127.0.0.1:6060/api/health`;
2. reuse a compatible bingo server with DAP on `127.0.0.1:4711`;
3. start the prepared or configured binary only when the management connection
   is refused;
4. wait up to five seconds for compatible health;
5. connect `nvim-dap` to the shared DAP listener.

The spawned server is detached and receives a 30-second server-owned idle grace.
The plugin never kills it. Logs default to
`stdpath("state") .. "/bingo/server.log"`.

Compatibility requires the exact Go wire version (currently 1.4), management
API 1, and DAP session-event version 1. Auto mode requires distinct management
and DAP endpoints, both on `127.0.0.1`. Invalid launch/attach/join arguments
are rejected before transport or spawn. Remote DNS and IPv6 hosts are accepted
in `connectOnly`; header delimiters and malformed IPv6 addresses are rejected.

Health reads support Content-Length, chunked responses with trailers, and
EOF-delimited responses. A framed response completes without waiting for the
peer to close. Headers/trailers are bounded to 8 KiB and the entire response to
64 KiB; conflicting framing, malformed chunks, truncated EOF and buffered
trailing bytes fail the probe. The wall-clock deadline never resets on incoming
bytes. Disposal cancels pending probes and timers, releases owned libuv handles,
and prevents stale callbacks from starting a server or completing a new attempt.

```lua
require("bingo").setup({
  server = {
    mode = "auto",
    binary = "/absolute/path/to/bingo", -- optional
    management_host = "127.0.0.1",
    management_port = 6060,
    dap_host = "127.0.0.1",
    dap_port = 4711,
    ready_timeout_ms = 5000,
    idle_timeout_ms = 30000,
    log_path = nil,
  },
  configurations = true,
  notify_session = true,
})
```

`setup()` adds three Go configurations to `dap.configurations.go`: launch a
binary, attach to a PID, and join a managed session. Set `configurations = false`
to register only the adapter.

## Drive a session

Use `:DapContinue` and the rest of your normal `nvim-dap` mappings, or start
through the companion commands:

```vim
:BingoLaunch /absolute/path/to/program
:BingoAttach 1234 /absolute/path/to/program
:BingoJoin session-id
:BingoSession
```

`BingoAttach` accepts an optional binary path, but DWARF-backed source
breakpoints, stack frames, and locals require it. Joining does not relaunch,
reattach, or automatically resume the shared session.

The adapter's `bingo/session/v1` event is validated and retained. `:BingoSession`
shows the active ID, `require("bingo").session_id()` returns it, and the plugin
emits `User BingoSession` with `event.data.session_id` for other Neovim plugins.
Only registered, live bingo sessions may announce an ID. Duplicate announcements
are idempotent; malformed or conflicting ones are reported and ignored.
Termination, disconnect, and transport closure clear session ownership. Repeated
setup restores owned close hooks, and callbacks from the old setup cannot
repopulate the new session registry.

## Remote and custom endpoints

Autostart is loopback-only. Connect to an externally managed or forwarded server
without probing or spawning:

```lua
require("bingo").setup({
  server = {
    mode = "connectOnly",
    dap_host = "debug.internal",
    dap_port = 14711,
  },
})
```

Each `nvim-dap` configuration may override the same camel-case lifecycle fields
as VS Code: `serverMode`, `managementHost`, `managementPort`, `dapHost`,
`dapPort`, `serverReadyTimeoutMs`, and `managedIdleTimeoutMs`.

## Concurrency telemetry

DAP drives the debug loop. bingo's richer goroutine spawn hierarchy remains on
the WebSocket stream, so observe the announced session from another terminal:

```sh
go run ./cmd/wsmon -session <session-id>
```

The observer is read-only and can coexist with Neovim and other DAP or WebSocket
clients on the same session.

## Development

```sh
just neovim-check
```

The default suite parses every Lua file and requires no installed `nvim-dap`,
debugger binary, user configuration, or external service. Its focused modules
cover Go-source-driven contract drift, configuration/request boundaries,
HTTP framing, deterministic libuv faults/deadlines/cancellation, manager
coalescing and spawn/log ownership, and adapter/session lifecycle. Shared helpers
restore globals and loaded/preloaded modules even after failures. Stress cases
exercise hundreds of coalesced callbacks and repeated sessions; real loopback
TCP cases use ephemeral ports and close only their own sockets/timers.

[Neovim CI](../../.github/workflows/neovim-extension.yml) runs that same suite on
native linux/amd64 and darwin/arm64 with checksum-pinned Neovim 0.11.7, read-only
repository permissions, and isolated XDG directories. The default suite does
not claim to exercise nvim-dap itself or native debugger operations.

### Real nvim-dap and native debugger smoke

```sh
bash editors/neovim/scripts/integration.sh
# Optional: select a minimum Neovim and an already-built, signed native server.
NVIM=/absolute/path/to/nvim BINGO_TEST_BINARY=/absolute/path/to/bingo \
  bash editors/neovim/scripts/integration.sh
```

This explicitly invoked layer downloads nvim-dap revision
`c9a0738e45f1bd41d792a126941348dce661cf9b`, verifies its pinned SHA-256, and puts it
on a test-only runtimepath. It builds a small unoptimized target and, unless
`BINGO_TEST_BINARY` is supplied, the current server (with debugger entitlements
on Darwin). A supplied server is used unchanged; its hash/build metadata and
the Neovim version are saved with the logs. `BINGO_NVIM_SMOKE_DAP_ARCHIVE` can
name an offline copy of the exact pinned archive.

The smoke launches through the real companion/nvim-dap adapter, observes the
custom session announcement, hits a source breakpoint, and reads the real
`known:int=42` local through stack/scopes/variables requests. Entry has no
resolved goroutine, so it uses nvim-dap's real `Session:request` with thread
zero to continue rather than inventing a thread ID. A second real nvim-dap
client joins the same suspended target before a **single** terminate intent.
That observer must receive exactly one server `terminated` event; both client
registries must empty, the target must disappear, and the server must exit
through its own idle grace. The observer is load-bearing: pinned nvim-dap
closes the initiating client on the terminate response and would otherwise
mask a missing server terminal event. `BINGO_NVIM_SMOKE_RETAIN_OBSERVER=0`
selects a complementary single-client control, not the CI acceptance path.

All configuration, runtimepath, ports, processes, and logs are isolated under
ignored `build/neovim-integration/run-*` directories; no user configuration is
read or modified. Success never signals the server. Failure cleanup targets
only the test-owned server and the fixture PID after verifying its unique
executable path. Runs retain diagnostics for inspection. Requirements are
Neovim 0.11.7+, the repository's Go toolchain, curl, tar, and a **native**
linux/amd64 or local/self-hosted darwin/arm64 host. Linux CI executes this
layer; hosted macOS explicitly skips native Mach execution while still
running the default Lua/real-TCP suite. Emulation is not a native-kernel proof.

Run `:checkhealth bingo` inside Neovim to verify the required Neovim and
`nvim-dap` dependencies are available.
