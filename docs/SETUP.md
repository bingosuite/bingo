# Setup

bingo supports **native x86-64 Linux (`linux/amd64`)** and **Apple Silicon macOS
(`darwin/arm64`)**. Choose an editor or the terminal; running a debugger should
not require learning the contributor build pipeline.

## Choose an installation

When a [release](https://github.com/bingosuite/bingo/releases) provides the
native artifacts below, prefer the matching prebuilt package. This guide does
not assume artifacts have already been published, and bingo is not installed
from a Marketplace listing. The source paths below work from this checkout.

| What you want | Prebuilt package, when released | Source fallback |
| --- | --- | --- |
| VS Code | Platform `.vsix`, including the server | `bash scripts/package-vscode.sh install` |
| Neovim | Platform `bingo-neovim_*.tar.gz`, including `bin/bingo` | Plugin-manager prepare hook or `bash editors/neovim/scripts/prepare.sh` |
| Terminal | Platform `bingo_*.tar.gz`, containing server and three clients | `bash scripts/build-binary.sh bingo build/bin/bingo` |

**Prebuilt debugger and client binaries do not need Go, Node, npm, or `just`.**
Debugging a **source package** does need a Go compiler on the **server's PATH**.
Launching an already-built target does not. `just` is optional shorthand for
source/contributor commands, never an editor startup dependency.

To use a source fallback:

```sh
git clone https://github.com/bingosuite/bingo.git
cd bingo
```

Source builds require Go **1.25.5 or newer** (the release toolchain is pinned by
`go.mod`). VSIX source builds additionally require **Node.js 22.x** (pinned by
`editors/vscode/.nvmrc`), npm, `unzip`, and `file`. The install command also needs
the `code` CLI; packaging alone does not. On macOS, native server builds need
Xcode Command Line Tools and `codesign`; install the tools with
`xcode-select --install` if absent. Preflight reports missing requirements
before dependency restoration or compilation.

## VS Code

Requires VS Code **1.85 or newer**.

1. Install the matching release VSIX with **Extensions: Install from VSIX...**,
   or run this once from the source checkout:

   ```sh
   bash scripts/package-vscode.sh install
   # equivalent optional shorthand: just vscode-install
   ```

2. Run **Developer: Reload Window** after an install/update.
3. Open a saved Go file in a runnable `main` package and run
   **Bingo: Debug Go Package**. It uses that file's directory, or the workspace
   root when it contains Go source; it does not open a directory picker.
   With no launch configuration, F5 can also select bingo. Use an explicit
   `launch.json` configuration for a different package path.
4. Set breakpoints and use the native Debug UI. The Bingo Concurrency editor
   opens beside source; the Activity Bar offers the same read-only model.

The server builds that package with `-gcflags="all=-N -l"` and owns the temporary
binary for the session's lifetime. The extension starts or reuses a compatible
server automatically. There is no pre-launch `just` task and no manual binary
path for source debugging.

In this repository, **bingo: Debug example** selects one of the five progressive
examples; **bingo: Debug spawntree telemetry demo** opens the long-running
concurrency demo. Both launch source directories. See the
[examples guide](../examples/README.md) for useful breakpoints.

The source install path restores the exact npm lockfile with lifecycle scripts
disabled, packages the native server and extension **once**, verifies the exact
VSIX contents/architecture/executable mode/signature, then invokes
`code --install-extension`. It deliberately does not run the full lint/test
suite or double packaging gate. To build the same artifact without changing
your VS Code profile:

```sh
bash scripts/package-vscode.sh
# equivalent: just vscode-local-package
```

Microsoft's Go extension remains useful for `gopls`, formatting, and tests.
bingo owns debugger type `"bingo"` and never invokes Delve. The
[extension guide](../editors/vscode/README.md) covers attach/join, remote
endpoints, logs, and contributor extension-host development.

## Neovim

Requires **Neovim 0.11.7 or newer** and
[`nvim-dap`](https://github.com/mfussenegger/nvim-dap).

Use the matching prebuilt companion archive as a local plugin/runtime directory,
keeping its `lua/`, `plugin/`, and `bin/` directories together. It needs no prepare
hook. For a source checkout, the plugin-manager build hook is:

```sh
bash editors/neovim/scripts/prepare.sh
```

The hook builds/signs only the server and needs neither Node nor `just`.
The [Neovim guide](../editors/neovim/README.md) includes the full monorepo
`lazy.nvim` configuration and the local/prebuilt runtime-path setup. After
`require("bingo").setup()`, open a Go file and run:

```vim
:BingoDebug
" Or explicitly choose the package:
:BingoDebug /absolute/path/to/project/cmd/app
```

The default is the active Go file's package directory, or the editor's working
directory. The server builds and launches the package; use normal `nvim-dap`
breakpoints, stepping, stacks, and variables. `:BingoLaunch` remains the
prebuilt-binary path, and `:BingoAttach`, `:BingoJoin`, and `:BingoSession` retain
their existing roles. `:checkhealth bingo` diagnoses setup problems.

Neovim drives DAP; use `bingo-wsmon -session <session-id>` for concurrency
telemetry rather than expecting a graphical observer in the plugin.

## Source launch versus binary launch

The explicit DAP launch mode chooses who builds the target:

| Launch arguments | Behavior |
| --- | --- |
| `"mode": "debug"` | `program` is a Go package **directory**; the server compiles it, then launches the temporary binary |
| `"mode": "exec"` or omitted `mode` | `program` is an existing executable; no compiler runs |
| Optional `cwd` | Target working directory; source mode defaults to its package directory, binary mode preserves the server working directory when omitted |

For a saved VS Code configuration, the minimal source launch is:

```json
{
  "type": "bingo",
  "request": "launch",
  "name": "Debug Go package",
  "mode": "debug",
  "program": "${workspaceFolder}/cmd/app"
}
```

Set `cwd` explicitly when the program reads assets relative to a different
directory. In remote/connect-only sessions, source paths, compiler, and working
directory are on the **server**, not copied from the editor. Compilation errors
are reported as launch failures. Cold compilation can take longer than server
readiness; those are separate operations.

Restart reuses the built session binary. **Start a new debug session to rebuild
source edits.** Existing mode-less binary configurations do not silently turn
into source builds.

Managed editor startup requires the server's `dap.sourceLaunchVersion: 1`
health capability as well as the exact wire version (**1.4**). Build/release,
extension-package, management API, and wire versions are separate identities.
An older or incompatible shared server is never killed/replaced automatically.

## Terminal only

The terminal archive contains `bin/bingo`, `bin/bingo-cli`, `bin/bingo-dapcli`,
and `bin/bingo-wsmon`. Keep them in the extracted directory or copy the desired
executables to a directory on your `PATH`. Check the server identity first:

```sh
./bin/bingo -version
./bin/bingo -addr 127.0.0.1:6060 -dap-addr 127.0.0.1:4711
```

Keep the server running. In another terminal, run `./bin/bingo-dapcli` and launch
an existing debugger-friendly target:

```text
launch /absolute/path/to/target
break main.go:20
c
```

The client prints the managed session ID. A third terminal can observe it with
`./bin/bingo-wsmon -session <session-id>`. `bingo-cli` is the equivalent native
WebSocket driver.

From source, the convenience path is:

```sh
just build-examples
just server                 # management/WebSocket + DAP; no unrelated target build
# In another terminal:
just dapcli
# At its prompt: launch ./build/examples/level3-worker-pool
```

Without `just`, build the server through
`bash scripts/build-binary.sh bingo build/bin/bingo`; run clients with
`go run ./cmd/dapcli`, `go run ./cmd/cli`, or `go run ./cmd/wsmon`.
Prebuilt example targets are optional: the editors' source-launch path builds
only the selected package. The
[concurrency runbook](ConcurrencyTelemetry.md) walks through DAP driving and
WebSocket observation of the same session.

## Platform and endpoint notes

macOS uses the `bingonative` tag and debugger entitlement. The build helper
applies both; released macOS binaries are **ad-hoc signed, not notarized**.
Host security policy may prevent running downloaded, unnotarized code or
acquiring a task port. Use the source-build path when appropriate; do not disable
Gatekeeper or SIP globally to install bingo.

Linux uses native `ptrace`. Attaching can be limited by kernel or container
policy; prefer the smallest process/container permission needed instead of
weakening system-wide policy. Emulated Linux on Apple Silicon is not a native
debugger test environment.

Default endpoints are loopback-only: management/WebSocket **127.0.0.1:6060**,
DAP **127.0.0.1:4711**. The protocols do not authenticate non-loopback clients.
Managed servers exit after their idle grace; manual servers stay running unless
started with `-idle-timeout`. If an endpoint is occupied or incompatible, inspect
the server logs and active sessions, deliberately stop a server you own, or use
another pair of distinct endpoints. Frontends never kill a shared server for you.

## Contributor checks

These are separate from ordinary installation/startup:

```sh
just                        # list commands without starting anything
just build
just test
just vet
just vscode-check           # lint, typecheck, unit/DOM tests, bundle
just vscode-package         # full checks + two-build binary/VSIX reproducibility
just neovim-check
```

On macOS, direct Go test/vet commands need `-tags bingonative`. The native
debugger acceptance suites are `just e2e-linux` and `just e2e-darwin`; Darwin
execution requires a suitable local/self-hosted Apple Silicon machine.
Contributor source-extension work uses `just vscode-dev`, then the explicit
Extension Development Host command in the
[extension guide](../editors/vscode/README.md). It does not rebuild the examples.

## Release preparation

The [Release workflow](../.github/workflows/release.yml) accepts an existing
`vMAJOR.MINOR.PATCH` tag (optional prerelease suffix, at most 80 characters).
Both native builders check out the immutable triggering commit. The tag must
resolve to that same commit before building; a separately selected tag cannot
run its code with a different branch's Actions cache authority. Moved tags and
dirty checkouts are rejected.

For a tag at the selected branch's tip, use **Actions -> Release -> Run
workflow** with that tag. For an older tag, dispatch from the tag itself
(replace `vX.Y.Z` with the release tag):

```sh
gh workflow run release.yml --repo bingosuite/bingo --ref vX.Y.Z -f tag=vX.Y.Z
```

The default produces downloadable Actions artifacts only. To stage a release,
first create a **draft release** for that tag and select `upload_to_draft`; this attaches
verified assets but never publishes the draft. With the CLI, add
`-F upload_to_draft=true` to the command above. Publishing an existing release
also triggers verified asset builds/uploads. Neither path invokes `code` or
modifies an editor profile.

Each platform contributes these assets, with `VERSION` including the leading
`v` and `PLATFORM` equal to `linux_amd64` or `darwin_arm64`:

| Asset | Contents |
| --- | --- |
| `bingo_VERSION_PLATFORM.tar.gz` | Server, three named terminal clients, MIT license, install instructions, build metadata |
| `bingo-neovim_VERSION_PLATFORM.tar.gz` | Lua companion, native `bin/bingo`, README, MIT license, install instructions, metadata |
| `bingo-VERSION-linux-x64.vsix` or `bingo-VERSION-darwin-arm64.vsix` | Matching platform VS Code extension and the same server |
| `bingo_VERSION_PLATFORM.json` | Exact commit, suite version, platform, Go/wire/extension versions, signing information |
| `bingo_VERSION_PLATFORM_SHA256SUMS.txt` | SHA-256 of the other four assets, sorted by basename |

Download only the artifact(s) you want plus your platform's checksum file into
one directory. Before installing or extracting them, run:

```sh
shasum -a 256 -c --ignore-missing bingo_VERSION_PLATFORM_SHA256SUMS.txt
# Linux may also use: sha256sum -c --ignore-missing bingo_VERSION_PLATFORM_SHA256SUMS.txt
```

Proceed only when the command succeeds **and each chosen artifact's exact
filename is reported `OK`**. No verified files is not success, and a `FAILED`
result means the download must not be used.

Checksum names are relative to that download directory, and checksum filenames
are platform-specific so the two release jobs never overwrite one another.
Checksums detect corruption; they are not an independent signing identity.

For a local, non-publishing preview, run `just release-package` and inspect
`dist/release/`. It explicitly labels modified source as `dev`/`-dirty`. For a
tagged build use `just release-package vX.Y.Z` from its clean, matching checkout.
Both run full extension checks and the existing two-build VSIX/binary
reproducibility gate. Terminal clients are also built twice and compared.
Archive contents, executable permissions, metadata, and checksum sets are
verified before files are promoted from staging.

CI uses the Go version in `go.mod` and Node version in `.nvmrc`. Builds run on
native Ubuntu x86-64 and `macos-15` arm64, never emulation. The workflow checks
Go and extension source and smoke-tests server health/idle exit; that smoke is
**not native debugger acceptance**. Maintain the existing Linux native E2E and
local Darwin E2E/review procedure in [AGENTS.md](../AGENTS.md) before release.
Only the final upload job has write permission; it rechecks the release tag,
draft/published state, and exact per-platform checksum sets.

## Troubleshooting

- **Missing `code` CLI:** enable **Shell Command: Install 'code' command in
  PATH**, or use the package-only command and **Install from VSIX...**.
- **Wrong Node version:** select Node 22 for packaging; an installed VSIX does
  not use your system Node.
- **Source launch fails:** check that the chosen directory is a runnable Go
  package, its module dependencies resolve, and Go is on the server's PATH.
- **Source changes do not appear after Restart:** start a fresh session to
  rebuild; Restart intentionally reuses the current binary.
- **Old server at a managed endpoint:** inspect **bingo Server** in VS Code or
  `:checkhealth bingo` in Neovim. Choose a free endpoint pair or deliberately
  upgrade your own server after its sessions end.

For bugs, open an [issue](https://github.com/bingosuite/bingo/issues).
For setup questions, use
[GitHub Discussions](https://github.com/bingosuite/bingo/discussions).
