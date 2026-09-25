#!/usr/bin/env bash
set -euo pipefail
repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source "$repository_root/scripts/tooling.sh"
cd "$repository_root"
[[ "$#" == 1 ]] || bingo_fail "usage: bash scripts/preflight.sh {native|release}"
case "$1" in native|release) ;; *) bingo_fail "unknown preflight $1" ;; esac
bingo_host
bingo_check_go
bingo_check_darwin
if [[ "$1" == release ]]; then
  bingo_require git
  if [[ "$bingo_host_os" == darwin ]]; then bingo_check_node; fi
fi
printf 'Prerequisites ready: %s on %s/%s\n' "$1" "$bingo_host_os" "$bingo_host_arch"
