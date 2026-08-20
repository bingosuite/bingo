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

The headless-Neovim check parses every Lua file and runs the dependency-free
configuration, adapter-registration, health-contract, HTTP, and session-event
tests. Run `:checkhealth bingo` inside Neovim to verify the required Neovim and
`nvim-dap` dependencies are available.
