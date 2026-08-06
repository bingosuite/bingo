# bingo Debugger for VS Code

This companion extension connects VS Code's built-in Debug UI directly to an
already-running [bingo](https://github.com/bingosuite/bingo) Debug Adapter
Protocol listener. Its extension ID is `bingosuite.bingo`; it owns debugger type
`bingo`, does not launch or validate Delve (`dlv`), and does not replace the
Microsoft Go extension's debugger type.

Keep the Microsoft Go extension installed for `gopls`, navigation, formatting,
and test integration. The two extensions coexist: Go language tooling comes from
`golang.go`, while a launch configuration with `"type": "bingo"` uses this
extension and the bingo server.

## Build and install

From the repository root:

```sh
just vscode-install
```

Reload the VS Code window after installation. To update, rebuild the VSIX and
reinstall it by rerunning `just vscode-install`. To package without installing,
run `just vscode-package`; it writes `dist/bingo.vsix`.

To uninstall:

```sh
code --uninstall-extension bingosuite.bingo
```

Generated dependencies, bundles, and VSIX files are ignored by Git.
The package recipe builds twice with normalized ZIP metadata and fails if the
two VSIX hashes differ.

## Use

Start bingo's WebSocket and DAP listeners:

```sh
just server
```

Then select a `bingo` configuration in **Run and Debug** and press F5. This
repository's `.vscode/launch.json` includes a launch and a session-join example.
The launch example first runs the workspace's `just build-spawntree` task, then
connects to the default DAP endpoint at `127.0.0.1:4711`.

### Launch a binary

```json
{
  "name": "bingo: Launch binary",
  "type": "bingo",
  "request": "launch",
  "program": "${workspaceFolder}/build/target/target",
  "args": [],
  "env": ["BINGO_MODE=debug"],
  "stopOnEntry": true,
  "dapHost": "127.0.0.1",
  "dapPort": 4711
}
```

`env` follows bingo's current DAP contract: an array of `KEY=value` strings.

### Join an existing bingo session

```json
{
  "name": "bingo: Join session",
  "type": "bingo",
  "request": "attach",
  "session": "replace-with-session-id",
  "dapHost": "127.0.0.1",
  "dapPort": 4711
}
```

Joining does not relaunch, reattach, or automatically resume the shared session.

### Attach to an OS process

```json
{
  "name": "bingo: Attach to process",
  "type": "bingo",
  "request": "attach",
  "pid": 1234,
  "binaryPath": "/absolute/path/to/the/binary",
  "stopOnEntry": true,
  "dapHost": "127.0.0.1",
  "dapPort": 4711
}
```

`binaryPath` is optional for attaching, but bingo needs its DWARF data for
source breakpoints, stack frames, and locals.

## Endpoint and remote environments

`dapHost` and `dapPort` tell the extension host where bingo is listening. The
extension only connects; it never starts the server. The default host is the
explicit IPv4 loopback because bingo currently opens its DAP listener with
`tcp4`; `localhost` can resolve to IPv6 first in older VS Code runtimes. For a
remote extension host (SSH, a dev container, or Codespaces), use an address
reachable from that host and bind bingo's `-dap-addr` accordingly.

## Development

```sh
just vscode-check
just vscode-package
```

The check runs ESLint, strict TypeScript type checking, unit and manifest tests,
the esbuild bundle, and a VSIX file-list smoke check.
