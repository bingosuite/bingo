# Setup

bingo is currently built and installed from source. This guide takes a fresh
checkout to a working VS Code, Neovim, or terminal debugging setup.

## Prerequisites

### Required

| Requirement | Supported version or platform |
| --- | --- |
| Operating system | Apple Silicon macOS (`darwin/arm64`) or x86-64 Linux (`linux/amd64`) |
| [Go](https://go.dev/dl/) | 1.25.5 |
| [just](https://github.com/casey/just#installation) | Current stable release |
| [Git](https://git-scm.com/downloads) | Any recent release |

Confirm the tools are available:

```sh
go version
just --version
git --version
```

Other operating systems and architectures are not supported. They fail to build
because bingo intentionally provides native debugger backends only for the two
platforms above.

### Frontend-specific

Choose the dependencies for the frontend you plan to use:

| Frontend | Additional requirements |
| --- | --- |
| VS Code | VS Code 1.85+, Node.js 22.x, npm, and the `code` CLI |
| Neovim | Neovim 0.11.7+ and [`nvim-dap`](https://github.com/mfussenegger/nvim-dap) |
| Terminal | No additional dependencies |

The Microsoft Go extension remains useful in VS Code for `gopls`, formatting,
navigation, and tests. bingo owns debugger type `"bingo"` and does not invoke
Delve.

## Clone and build

```sh
git clone https://github.com/bingosuite/bingo.git
cd bingo
just build
```

The binary is written to:

```text
build/bingo/bingo_<goos>_<goarch>
```

`just build` selects the current supported host automatically. On macOS it also
enables the `bingonative` build tag and signs the binary with the debugger
entitlement.

## Choose a frontend

### VS Code

VS Code provides the most complete experience: the standard Debug UI drives the
session while the Bingo Concurrency view follows goroutines and OS threads.

1. Ensure Node.js 22, npm, and the `code` CLI are on `PATH`.
2. Build, verify, and install the platform-specific extension:

   ```sh
   just vscode-install
   ```

3. Run **Developer: Reload Window** in VS Code.
4. Open this repository, select **bingo DAP: launch example (stop on entry)** in
   **Run and Debug**, press F5, and choose an example.

The launch task builds debugger-friendly example binaries automatically. The
extension discovers or starts the local bingo server, so no separate server
terminal is needed.

See the [VS Code extension guide](../editors/vscode/README.md) for custom launch
configurations, PID attach, joining an existing session, remote endpoints,
server logs, and extension development.

### Neovim

Prepare the platform-native server:

```sh
just neovim-prepare
```

Add `editors/neovim` to Neovim's runtime path and configure the companion. For a
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

Run `:BingoLaunch`, `:BingoAttach`, or `:BingoJoin`, then use normal `nvim-dap`
commands for breakpoints, stepping, stacks, and variables. Run
`:checkhealth bingo` if the adapter or dependencies are not detected.

See the [Neovim companion guide](../editors/neovim/README.md) for the complete
configuration, managed-server behavior, remote endpoints, and session commands.

### Terminal only

Build the examples and start the management/WebSocket and DAP listeners:

```sh
just build-examples
just server
```

Keep the server running. In a second terminal, start the DAP client:

```sh
just dapcli
```

Launch an example and set a breakpoint from its prompt:

```text
launch ./build/examples/level3-worker-pool
break examples/level3-worker-pool/main.go:20
c
```

The client prints the managed session ID. Observe its concurrency telemetry from
a third terminal:

```sh
go run ./cmd/wsmon -session <session-id>
```

The [concurrency telemetry runbook](ConcurrencyTelemetry.md) covers the complete
DAP-driver/WebSocket-observer workflow. The
[progressive examples guide](../examples/README.md) lists useful breakpoints and
expected behavior.

## Platform notes

### macOS

- Only Apple Silicon (`darwin/arm64`) is supported.
- The native backend uses cgo, Mach APIs, and `codesign`. Install the Xcode
  command-line tools if those are unavailable:

  ```sh
  xcode-select --install
  ```

- Prefer the `just` recipes. They apply the required `bingonative` tag and sign
  native binaries with `entitlements.plist`.
- If you invoke Go tests directly, include the native tag:

  ```sh
  go test -tags bingonative ./...
  ```

Plain `go test ./...` cannot compile the Darwin backend.

### Linux

- Only native x86-64 (`linux/amd64`) is supported.
- The backend requires Linux `ptrace`. Launching a child process normally works
  under standard host policies, while attaching to an existing process can be
  restricted by the kernel or container runtime.
- Containers must permit ptrace system calls and typically need
  `CAP_SYS_PTRACE`. Exact configuration depends on the runtime; running bingo
  directly on the host is the simplest setup.

## Verify the checkout

Build the debugger and example targets:

```sh
just build
just build-examples
```

Contributors can also run the platform-aware Go and integration suites:

```sh
just test
just integration
```

Frontend-specific checks are available when their dependencies are installed:

```sh
just vscode-check
just neovim-check
```

## Troubleshooting

- **Unsupported platform:** compare `go env GOOS GOARCH` with the two supported
  targets above.
- **VS Code cannot run `just vscode-install`:** confirm Node.js 22, npm, and
  `code` are on `PATH`. In VS Code, the command palette action
  **Shell Command: Install 'code' command in PATH** installs the CLI where
  supported.
- **A local endpoint is occupied:** bingo uses `127.0.0.1:6060` for management
  and WebSocket traffic and `127.0.0.1:4711` for DAP. Stop the conflicting
  service or configure distinct endpoints.
- **VS Code reports an incompatible server:** inspect the **bingo Server**
  output channel. The extension will not replace an unknown or incompatible
  process listening on the configured management port.
- **Linux attach is denied:** inspect the host's ptrace and container security
  policy. Avoid weakening a system-wide policy when a narrower process or
  container permission is available.
- **macOS native debugging fails:** rebuild through `just` so the native tag and
  entitlement are applied. The
  [VS Code troubleshooting guide](../editors/vscode/README.md#troubleshooting)
  includes extension-specific diagnostics.

For bugs, open an [issue](https://github.com/bingosuite/bingo/issues). For setup
questions and ideas, use
[GitHub Discussions](https://github.com/bingosuite/bingo/discussions).
