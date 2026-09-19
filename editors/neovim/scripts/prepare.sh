#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo "Usage: bash editors/neovim/scripts/prepare.sh (no arguments)" >&2
  exit 1
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
if [[ ! -f "$root/go.mod" || ! -f "$root/scripts/build-binary.sh" ]]; then
  echo "ERROR: preparing bingo requires the full bingosuite/bingo checkout." >&2
  echo "For a standalone Neovim release, use its bundled bin/bingo or set server.binary." >&2
  exit 1
fi

exec bash "$root/scripts/build-binary.sh" bingo "$root/editors/neovim/bin/bingo"
