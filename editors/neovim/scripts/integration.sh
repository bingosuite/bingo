#!/usr/bin/env bash
set -euo pipefail

# Opt-in only: downloads a pinned nvim-dap archive into ignored project build
# storage. Neither the default Lua suite nor a user's plugin directories use it.
# Run: bash editors/neovim/scripts/integration.sh
# BINGO_TEST_BINARY may name an already-built native server (used unchanged).
# BINGO_NVIM_SMOKE_BINARY remains an alias; BINGO_TEST_BINARY takes precedence.
# BINGO_NVIM_SMOKE_DAP_ARCHIVE may name an offline copy of the exact archive.
# NVIM may name a specific Neovim executable instead of the one on PATH.
# A second real nvim-dap client must close on the server's terminated event;
# BINGO_NVIM_SMOKE_RETAIN_OBSERVER=0 selects the single-client control instead.
root="$(cd "$(dirname "$0")/../../.." && pwd)"
cd "$root"

revision=c9a0738e45f1bd41d792a126941348dce661cf9b
archive_sha256=1c0187f954cb577e0570ed8c63c58604a1e65868bb5621cdbc0efd4e11034659
platform="$(uname -s)/$(uname -m)"
case "$platform" in
  Darwin/arm64)
    if [[ "${GITHUB_ACTIONS:-}" == true && "${RUNNER_ENVIRONMENT:-}" != self-hosted ]]; then
      echo "SKIP: native Darwin execution requires a local or self-hosted Apple Silicon runner."
      exit 0
    fi
    if [[ "$(sysctl -in sysctl.proc_translated 2>/dev/null || true)" == 1 ]]; then
      echo "ERROR: native execution is required; Rosetta is unsupported." >&2
      exit 1
    fi
    goos=darwin
    goarch=arm64
    ;;
  Linux/x86_64) goos=linux; goarch=amd64 ;;
  *)
    echo "ERROR: supported native hosts are Darwin/arm64 and Linux/x86_64, got $platform." >&2
    exit 1
    ;;
esac
if [[ -n "${QEMU_CPU:-}" || -n "${QEMU_LD_PREFIX:-}" ]]; then
  echo "ERROR: native execution is required; QEMU is unsupported." >&2
  exit 1
fi
nvim_binary="${NVIM:-nvim}"
for tool in "$nvim_binary" go curl tar shasum; do
  command -v "$tool" >/dev/null || { echo "ERROR: missing $tool" >&2; exit 1; }
done

work="$root/build/neovim-integration/run-$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "$root/build/neovim-integration"
mkdir "$work"
mkdir -p "$work"/{work,home,config,data,state,cache,runtime,nvim-dap}
chmod 700 "$work/runtime"
echo "Native nvim-dap smoke artifacts: $work"
"$nvim_binary" --version > "$work/neovim-version.txt"
echo "Neovim=$nvim_binary $(sed -n '1p' "$work/neovim-version.txt")"

archive="${BINGO_NVIM_SMOKE_DAP_ARCHIVE:-$root/build/neovim-integration/nvim-dap-$revision.tar.gz}"
if [[ ! -f "$archive" ]]; then
  if [[ -n "${BINGO_NVIM_SMOKE_DAP_ARCHIVE:-}" ]]; then
    echo "ERROR: requested offline archive does not exist: $archive" >&2
    exit 1
  fi
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
    --connect-timeout 10 --max-time 120 \
    "https://codeload.github.com/mfussenegger/nvim-dap/tar.gz/$revision" \
    -o "$work/nvim-dap.tar.gz"
  archive="$work/nvim-dap.tar.gz"
fi
actual_sha256="$(shasum -a 256 "$archive" | cut -d ' ' -f 1)"
if [[ "$actual_sha256" != "$archive_sha256" ]]; then
  echo "ERROR: nvim-dap archive SHA256 mismatch: $actual_sha256" >&2
  exit 1
fi
tar -xzf "$archive" --strip-components=1 -C "$work/nvim-dap"
if [[ "$archive" == "$work/nvim-dap.tar.gz" ]]; then
  cp "$archive" "$root/build/neovim-integration/nvim-dap-$revision.tar.gz"
fi
echo "nvim-dap revision=$revision sha256=$actual_sha256"

export TMPDIR="$work/work" GOTMPDIR="$work/work"
export GOOS="$goos" GOARCH="$goarch"
binary="${BINGO_TEST_BINARY:-${BINGO_NVIM_SMOKE_BINARY:-}}"
if [[ -n "$binary" ]]; then
  [[ "$binary" == /* ]] || binary="$root/$binary"
  [[ -x "$binary" ]] || { echo "ERROR: native server is not executable: $binary" >&2; exit 1; }
else
  binary="$work/bingo"
  bash "$root/scripts/build-binary.sh" bingo "$binary"
fi
echo "Bingo server=$binary sha256=$(shasum -a 256 "$binary" | cut -d ' ' -f 1)"
go version -m "$binary" > "$work/server-build.txt"
mkdir "$work/source package"
cp editors/neovim/tests/fixtures/smoke/main.go "$work/source package/main.go"
{
  printf 'module bingo-neovim-smoke\n\n'
  awk '$1 == "go" { print; exit }' go.mod
} > "$work/source package/go.mod"
go vet -tags bingonative ./editors/neovim/tests/fixtures/smoke

export BINGO_NVIM_SMOKE_ROOT="$root" BINGO_NVIM_SMOKE_WORK="$work"
export BINGO_NVIM_SMOKE_SERVER="$binary"
export BINGO_NVIM_SMOKE_PID_FILE="$work/target.pid"
export BINGO_NVIM_SMOKE_CWD_FILE="$work/target.cwd"
export HOME="$work/home" XDG_CONFIG_HOME="$work/config" XDG_DATA_HOME="$work/data"
export XDG_STATE_HOME="$work/state" XDG_CACHE_HOME="$work/cache" XDG_RUNTIME_DIR="$work/runtime"
export NVIM_APPNAME=bingo-native-smoke
unset VIMINIT EXINIT LUA_PATH LUA_CPATH NVIM NVIM_LISTEN_ADDRESS

"$nvim_binary" --headless --noplugin -u NONE -i NONE -l "$root/editors/neovim/tests/integration.lua"
if [[ ! -f "$work/success" ]] || [[ "$(cat "$work/success")" != PASS ]]; then
  echo "ERROR: Neovim exited before the smoke completed; inspect $work." >&2
  exit 1
fi
