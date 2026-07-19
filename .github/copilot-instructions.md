# Copilot instructions

The full guidance for AI agents working on **bingo** lives in
[`AGENTS.md`](../AGENTS.md) at the repo root — read it first. It's the single
source of truth; this file only exists so GitHub Copilot picks up the pointer.

Non-negotiables (see `AGENTS.md` for the rest and the *why*):

- **Comments explain why, not what.** No decorative or code-restating
  one-liners.
- **Conventional Commits are enforced** (`feat|fix|docs|style|refactor|perf|test|chore|wip`)
  by the commit-msg hook.
- **Return errors, don't `panic`** in server/hub/debugger control paths.
- **Supported platforms are linux/amd64 and darwin/arm64 only.** On macOS,
  build and test with `-tags bingonative` (or use the `just` recipes).
- **`internal/debugger/` is the most fragile code** — read the concurrency and
  breakpoint sections of `AGENTS.md` before touching it.
- **Keep `AGENTS.md` in sync** in the same commit when you change an invariant.
