#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source "$repository_root/scripts/tooling.sh"
cd "$repository_root"
[[ "$#" == 1 ]] || bingo_fail "usage: bash scripts/preflight.sh {native|vscode|install|release}"
case "$1" in
  native|vscode|install|release) ;;
  *) bingo_fail "unknown preflight $1" ;;
esac
bingo_host
if [[ "$1" == install ]]; then
  bingo_require code
  code --version >/dev/null || bingo_fail "cannot run the VS Code CLI; install code in PATH before building"
fi
bingo_require go
if [[ "$1" != native ]]; then
  bingo_require npm
  bingo_require unzip
  bingo_require file
  bingo_check_node
  node_host=$(node -p 'process.platform + "/" + process.arch')
  expected_node_arch=$bingo_host_arch
  if [[ "$expected_node_arch" == amd64 ]]; then expected_node_arch=x64; fi
  [[ "$node_host" == "$bingo_host_os/$expected_node_arch" ]] ||
    bingo_fail "Node.js must run natively on $bingo_host_os/$expected_node_arch; found $node_host"
  if [[ "$bingo_host_os" == darwin ]]; then
    target=darwin-arm64
  else
    target=linux-x64
  fi
  [[ "${BINGO_VSCODE_TARGET:-$target}" == "$target" ]] ||
    bingo_fail "local install and release packaging must target this host ($target)"
fi
if [[ "$1" == release ]]; then
  bingo_require git
fi
bingo_check_go
bingo_check_darwin
printf 'Prerequisites ready: %s on %s/%s\n' "$1" "$bingo_host_os" "$bingo_host_arch"
