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
- Go on the server's `PATH` for source-package debugging;
- a native bingo server, prepared by the install hook below, bundled in a
  platform release, available as `bingo` on `PATH`, or configured explicitly.

## Install with lazy.nvim

Add this spec to your plugins. It installs the monorepo, exposes the Neovim
companion, and builds the native server at install/update time:

```lua
{
  "bingosuite/bingo",
  lazy = false,
  dependencies = { "mfussenegger/nvim-dap" },
  build = function(plugin)
    local result = vim.system({
      "bash", plugin.dir .. "/editors/neovim/scripts/prepare.sh",
    }, { text = true }):wait()
    if result.code ~= 0 then
      error("bingo preparation failed:\n" .. result.stderr .. result.stdout)
    end
  end,
  config = function(plugin)
    vim.opt.rtp:append(plugin.dir .. "/editors/neovim")
    vim.cmd.runtime("plugin/bingo.lua")
    require("bingo").setup()
  end,
}
```

Building the server requires the Go version in the repository's `go.mod`
(currently 1.25.5+) and, on macOS, Xcode Command Line Tools
(`xcode-select --install`). Neither `just` nor Node is required. The explicit
build hook runs `scripts/prepare.sh`; startup itself never builds or downloads a
server. The shared native builder checks prerequisites, signs with debugger
entitlements on Darwin, and replaces `editors/neovim/bin/bingo` only after a
successful build/signature check.

For a local monorepo checkout the same preparation is:

```sh
bash editors/neovim/scripts/prepare.sh
```

It takes no arguments and works from any directory when invoked by its full
path. The generated binary is platform-specific and ignored by Git.

Alternatively, extract the matching Neovim archive from
[Releases](https://github.com/bingosuite/bingo/releases) and point a local
plugin-manager `dir` at the directory containing `lua/`, `plugin/`, and
`bin/bingo`; call `require("bingo").setup()` with no build hook. A standalone
plugin without its bundled binary can use `PATH` or `server.binary` instead.
It cannot run the monorepo preparation script without the rest of the checkout.

## Debug a Go package

Open a saved `.go` file in a runnable `main` package, set a breakpoint with
`:DapToggleBreakpoint`, then run:

```vim
:BingoDebug
```

The server builds the current Go buffer's **directory** with
`go build -gcflags='all=-N -l'`, launches it, and runs to your breakpoint.
There is no executable-path prompt or manual target build. Outside a regular
Go buffer, the default is Neovim's current working directory. To select another
package:

```vim
:BingoDebug ./cmd/myapp
```

Use a package directory, not a `.go` file or a `./...` package pattern. Source
is read from disk; save edits first. The default `bingo: Debug Go package`
configuration in `:DapContinue` uses the same selection and
`stopOnEntry = false`. Source build failures are reported by the server; it owns
build cancellation, temporary binaries, and their cleanup with the session.
Restart reuses the built binary rather than rebuilding changed source.

Run `:checkhealth bingo` for binary resolution and actionable tool/prerequisite
diagnostics. It does not probe, spawn a server, or build a target.

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
API 1, DAP session-event version 1, and DAP source-launch version 1.
An older server on the management port is rejected with an upgrade diagnostic;
the plugin does not kill or replace a shared server. Update that process after
its other sessions finish. Auto mode requires distinct management
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

`setup()` adds four Go configurations to `dap.configurations.go`: debug a Go
package, launch a binary, attach to a PID, and join a managed session. Existing
user configurations, including bingo configurations with these names, are
preserved. Set `configurations = false` to register only the adapter.

## Binary launch, attach, and join

Use `:DapContinue` and the rest of your normal `nvim-dap` mappings, or start
through the companion commands:

```vim
:BingoLaunch /absolute/path/to/program
:BingoAttach 1234 /absolute/path/to/program
:BingoJoin session-id
:BingoSession
```

`BingoLaunch` still runs an already-compiled binary without building it, and
omitting its path prompts for one. Binary launch and process attach retain
their entry stop. Paths containing spaces work, including completion-escaped
paths. Lua callers can use `require("bingo").debug(directory)`,
`.launch(binary)`, `.attach(pid, optional_binary)`, and `.join(session_id)`.

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

## Custom launch configurations

Source and binary launches share the existing DAP adapter. For arguments, an
explicit target working directory, or a different entry-stop preference, add
your own configuration:

```lua
table.insert(require("dap").configurations.go, {
  name = "My service",
  type = "bingo",
  request = "launch",
  mode = "debug",
  program = "./cmd/service",
  cwd = vim.fn.getcwd(),
  args = { "--listen", "127.0.0.1:8080" },
  env = { "LOG_LEVEL=debug" },
  stopOnEntry = false,
})
```

`mode = "debug"` builds a Go package directory on the server; `mode = "exec"`
launches a binary. An omitted mode retains the old binary-launch semantics.
`cwd` is the target's working directory and the base for a relative `program`.
In local `auto` mode, source paths and explicit `cwd` are validated and made
absolute against Neovim's cwd before transport; a source launch without `cwd`
uses its package directory. A binary launch without `cwd` keeps the legacy
server working directory. Attach/join do not accept launch-only `mode` or `cwd`.

The pinned nvim-dap's initialization warning timer covers the delayed launch
response too. Source launches give that existing timer 130 seconds to accommodate
the server's two-minute build deadline and handshake, avoiding a false
"adapter didn't respond" warning during a cold build. Binary launch/attach keep
the ten-second setting; there is no added client-side request timeout.

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

In `connectOnly`, source `program` and `cwd` are **server-local** paths. They
are not checked against this editor's filesystem or rewritten, and the server
needs Go available on its own `PATH`. Use an explicit server path in
`:BingoDebug` or a custom configuration when the editor and server directories
differ; no source synchronization or path mapping is implied.

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
coalescing and spawn/log ownership, source/cwd preparation, real Ex path escaping,
cancelled prompts, native preparation delegation, and adapter/session lifecycle.
Shared helpers restore globals and loaded/preloaded modules even after failures. Stress cases
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
on a test-only runtimepath. Unless `BINGO_TEST_BINARY` is supplied, it builds the
current server through the shared native builder (with debugger entitlements on
Darwin). A supplied server is used unchanged; its hash/build metadata and
the Neovim version are saved with the logs. `BINGO_NVIM_SMOKE_DAP_ARCHIVE` can
name an offline copy of the exact pinned archive.

The smoke opens an isolated source package whose directory contains spaces and
calls `bingo.debug()` with no arguments. The **server** builds the target;
there is no precompiled target or Lua compiler. It observes the custom session
announcement, runs directly to a source breakpoint without an entry-stop
resume, verifies the package working directory, and reads the real
`known:int=42` local through stack/scopes/variables requests. A second real
nvim-dap client joins the same suspended target before a **single** terminate intent.
That observer must receive exactly one server `terminated` event; both client
registries must empty, the target and its server-owned build directory must
disappear, and the server must exit through its own idle grace. The observer is
load-bearing: pinned nvim-dap
closes the initiating client on the terminate response and would otherwise
mask a missing server terminal event. `BINGO_NVIM_SMOKE_RETAIN_OBSERVER=0`
selects a complementary single-client control, not the CI acceptance path.

All configuration, runtimepath, ports, processes, and logs are isolated under
ignored `build/neovim-integration/run-*` directories; no user configuration is
read or modified. Success never signals the server. Failure cleanup targets
only the test-owned server and fixture process after verifying its executable
is inside this run's private server TMPDIR. Runs retain diagnostics for inspection.
Requirements are Neovim 0.11.7+, the repository's Go toolchain, curl, tar, and a **native**
linux/amd64 or local/self-hosted darwin/arm64 host. Linux CI executes this
layer; hosted macOS explicitly skips native Mach execution while still
running the default Lua/real-TCP suite. Emulation is not a native-kernel proof.
