# Editor repositories and build handoff

- [bingo](https://github.com/bingosuite/bingo) owns the server, terminal clients,
  protocol fixtures, and native binary builds.
- [bingo-vscode](https://github.com/bingosuite/bingo-vscode) owns the VS Code
  extension, its tests, platform VSIX packaging, and editor CI.
- [bingo-nvim](https://github.com/bingosuite/bingo-nvim) owns the Neovim plugin,
  its tests, platform plugin archives, and editor CI.

The editor histories are extracted with `git subtree split` from `editors/vscode`
and `editors/neovim`. Author information, messages, and relevant ancestry are
preserved; commit IDs change because the trees are rooted at the plugin directory.
The core repository retains its original history. Shared packaging/signing code
that builds the server stays in core.

## Build and release behavior

Every main push runs `Build packages` on native Linux amd64 and Darwin arm64.
Both jobs run Go vet/tests, compare two builds of every native binary, verify
archive contents and checksums, and smoke the packaged server's health/idle exit.
Each uploads `release-linux-x64` or `release-darwin-arm64` with a 90-day retention.
No GitHub release is published automatically.

The pre-existing `Release` workflow still accepts an existing version tag through
manual dispatch, optionally attaches assets to an existing draft, and builds and
uploads assets when a release is published. Its artifacts contain only the core
server and terminal clients after the split. The tag must match the selected
workflow revision. Manual example:

```sh
gh workflow run release.yml --repo bingosuite/bingo --ref vX.Y.Z -f tag=vX.Y.Z
```

After a successful main package build or published-release build, the separate
`Notify editor repositories` workflow sends `bingo-build` repository dispatches
to both editors. The payload contains the originating repository, run ID and
commit SHA. Notification runs on the default branch, with no checkout and no
execution of producer code. It does not run for pull requests or failed builds.

Each editor also runs on its own pushes, PRs, and manual dispatches. These resolve
the newest successful eligible main/release package run once, then pass that same
run ID and SHA to every job. A build-triggered run uses the triggering build,
even if a newer build finishes meanwhile. Consumers independently validate the
upstream workflow, successful status, source repository, branch/event and SHA.
They check artifact and file checksums plus metadata before staging the server.
Expired or missing artifacts fail explicitly instead of silently changing builds.

Editor compatibility tests read Go constants, generated wire fixtures, and source
examples from the exact core commit under `.bingo/source`. Editor packaging copies
the downloaded binary unchanged: it never recompiles, normalizes or re-signs it.
Package artifacts include a provenance JSON file with both the selected build and
binary hash. VS Code retains its two-package reproducibility and archive checks;
Neovim emits a checked archive with fixed timestamps and executable modes.
Native debugger acceptance runs on Linux. Hosted Darwin covers signatures,
packaging and the existing editor checks; native Mach acceptance remains local
or self-hosted.

## One-time GitHub setup

1. Create public `bingosuite/bingo-vscode` and `bingosuite/bingo-nvim` repositories
   without generated initial files. Push each extracted history and its migration
   commit as `main`. Keep the existing `bingosuite.bingo` extension identity.
2. Create a fine-grained personal access token with resource owner `bingosuite`,
   selecting **only those two editor repositories**, with repository
   **Contents: Read and write**. GitHub requires that permission for
   [repository dispatch](https://docs.github.com/en/rest/repos/repos#create-a-repository-dispatch-event).
   Complete organization approval if required, and set an appropriate expiry.
3. Add that value as the Actions secret `EDITOR_DISPATCH_TOKEN` on
   `bingosuite/bingo`. Do not put the token in source, shell history, or chat.
   The editor test jobs do not receive it.
   Create a separate fine-grained token selecting only bingosuite/bingo with
   **Actions: Read-only** and save it as `BINGO_BUILD_READ_TOKEN` in both editors
   (or an organization secret restricted to those two repositories).
   GitHub requires upstream Actions read permission even for public artifact
   downloads. Public metadata lookup uses the ordinary GITHUB_TOKEN.
   Fork PRs do not receive secrets: source/unit checks can run, but packaging
   fails explicitly until a maintainer runs the reviewed revision from a
   repository branch. No privileged pull_request_target checkout is used.
4. Merge the core migration and wait for its first successful `Build packages`
   run. Both editor workflows must exist on their default branches to receive
   repository dispatches. If the editor repositories were seeded before the first
   core package exists, rerun their initial failed builds or manually dispatch them
   after core packaging finishes.
5. Verify both editor runs name the same upstream run/SHA and produce both native
   platform packages. Check that the packaged server hash matches the core asset.

The new handoff format starts at the migration commit. Older tags keep their
historical suite packaging and are not valid inputs to the new consumers.
For a failed dispatch after fixing credentials, rerun the notification workflow;
this does not rebuild or change the chosen Bingo binary.

## Manual editor releases

Download artifacts from a successful editor workflow and attach them to a release
in that editor's repository. Include its build-provenance file. Do not rebuild a
package after testing it. VS Code runtime changes still require a manifest and
lockfile version bump before a release. Publishing to the VS Code Marketplace is
not configured by this migration. Neovim's source repository remains usable with
a separately installed server on PATH or an explicit `server.binary` setting.
