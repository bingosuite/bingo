#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" != 2 && "$#" != 4 ]]; then
  printf 'Usage: bash scripts/build-binary.sh {bingo|cli|dapcli|wsmon} OUTPUT [GOOS GOARCH]\n' >&2
  exit 2
fi
command_name=$1
case "$command_name" in
  bingo|cli|dapcli|wsmon) ;;
  *) printf 'bingo: unknown binary %s\n' "$command_name" >&2; exit 2 ;;
esac
[[ -n "$2" ]] || { printf 'bingo: OUTPUT must not be empty\n' >&2; exit 2; }
case "$2" in
  /*) output=$2 ;;
  *) output="$PWD/$2" ;;
esac
repository_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
source "$repository_root/scripts/tooling.sh"
cd "$repository_root"
[[ "$output" != */ && "$output" != */. && "$output" != */.. && ! -d "$output" ]] ||
  bingo_fail "OUTPUT must name a file, not a directory"

bingo_host
target_os=${3:-$bingo_host_os}
target_arch=${4:-$bingo_host_arch}
case "$target_os/$target_arch" in
  linux/amd64) ;;
  darwin/arm64)
    [[ "$bingo_host_os/$bingo_host_arch" == darwin/arm64 ]] ||
      bingo_fail "darwin/arm64 builds require native Apple Silicon and Xcode Command Line Tools"
    ;;
  *) bingo_fail "unsupported build target $target_os/$target_arch" ;;
esac
reproducible=${BINGO_REPRODUCIBLE:-0}
[[ "$reproducible" == 0 || "$reproducible" == 1 ]] ||
  bingo_fail "BINGO_REPRODUCIBLE must be 0 or 1"
version=${BINGO_VERSION:-dev}
bingo_validate_version "$version"
commit=${BINGO_COMMIT:-unknown}
if [[ -z "${BINGO_COMMIT:-}" && -e "$repository_root/.git" ]]; then
  bingo_require git
  commit=$(git rev-parse --verify HEAD)
  if [[ -n "$(git status --porcelain --untracked-files=normal)" ]]; then
    commit="$commit-dirty"
  fi
fi
bingo_validate_commit "$commit"
bingo_check_go
if [[ "$target_os" == darwin ]]; then
  bingo_check_darwin
  if [[ "$reproducible" == 1 ]]; then
    bingo_check_node
  fi
fi

mkdir -p -- "$(dirname -- "$output")"
temporary=$(mktemp "${output}.XXXXXX")
trap 'rm -f -- "$temporary"' EXIT
ldflags="-buildid="
if [[ "$command_name" == bingo ]]; then
  ldflags="$ldflags -X main.buildVersion=$version -X main.buildCommit=$commit"
fi
arguments=(build -trimpath -buildvcs=false "-ldflags=$ldflags")
if [[ "$target_os" == darwin ]]; then
  arguments+=(-tags bingonative)
  cgo=1
else
  cgo=0
fi
CGO_ENABLED=$cgo GOOS=$target_os GOARCH=$target_arch \
  go "${arguments[@]}" -o "$temporary" "./cmd/$command_name"

if [[ "$target_os" == darwin ]]; then
  if [[ "$reproducible" == 1 ]]; then
    # dyld needs LC_UUID; normalize before signing, never remove it.
    node --input-type=module -e \
      'import { normalizeMachOUUID } from "./editors/vscode/scripts/normalize-mach-o-uuid.mjs"; normalizeMachOUUID(process.argv[1]);' \
      "$temporary"
  fi
  signing=(--sign - --force --timestamp=none --identifier "bingosuite.$command_name")
  if [[ "$command_name" == bingo ]]; then
    signing+=(--entitlements "$repository_root/entitlements.plist")
  fi
  codesign "${signing[@]}" "$temporary"
  codesign --verify --strict "$temporary"
fi
chmod 755 "$temporary"
mv -f -- "$temporary" "$output"
printf 'Built %s (%s/%s): %s\n' "$command_name" "$target_os" "$target_arch" "$output"
