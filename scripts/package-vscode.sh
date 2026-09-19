#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source "$repository_root/scripts/tooling.sh"
[[ "$#" == 0 || ("$#" == 1 && "$1" == install) ]] ||
  bingo_fail "usage: bash scripts/package-vscode.sh [install]"
mode=${1:-vscode}
bash "$repository_root/scripts/preflight.sh" "$mode"
cd "$repository_root"

npm --prefix editors/vscode ci --ignore-scripts
# vsce runs vscode:prepublish, so package already builds both bundles once.
npm --prefix editors/vscode run package
npm --prefix editors/vscode run package:verify

if [[ "$mode" == install ]]; then
  bingo_host
  if [[ "$bingo_host_os" == darwin ]]; then
    target=darwin-arm64
  else
    target=linux-x64
  fi
  code --install-extension "$repository_root/dist/bingo-$target.vsix" --force
fi
