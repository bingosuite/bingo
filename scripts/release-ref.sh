#!/usr/bin/env bash
set -euo pipefail

repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source "$repository_root/scripts/tooling.sh"
[[ "$#" == 1 ]] || bingo_fail "usage: bash scripts/release-ref.sh vMAJOR.MINOR.PATCH"
bingo_validate_version "$1"
[[ "$1" != dev ]] || bingo_fail "a release requires an existing version tag, not dev"
printf 'tag=%s\n' "$1"
