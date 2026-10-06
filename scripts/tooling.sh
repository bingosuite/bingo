#!/usr/bin/env bash

bingo_fail() {
  printf 'bingo: %s\n' "$*" >&2
  exit 1
}

bingo_require() {
  command -v "$1" >/dev/null 2>&1 || bingo_fail "$1 is required; install it and add it to PATH"
}

bingo_host() {
  case "$(uname -s)/$(uname -m)" in
    Darwin/arm64) bingo_host_os=darwin; bingo_host_arch=arm64 ;;
    Linux/x86_64) bingo_host_os=linux; bingo_host_arch=amd64 ;;
    *) bingo_fail "unsupported host; use native linux/amd64 or darwin/arm64" ;;
  esac
}

bingo_check_go() {
  bingo_require go
  local required actual major minor patch required_major required_minor required_patch
  required=$(sed -n 's/^go //p' "$repository_root/go.mod")
  [[ "$required" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] ||
    bingo_fail "cannot read the Go toolchain requirement from go.mod"
  required_major=${BASH_REMATCH[1]}
  required_minor=${BASH_REMATCH[2]}
  required_patch=${BASH_REMATCH[3]}
  actual=$(go env GOVERSION) || bingo_fail "cannot run the Go toolchain; need Go $required or newer"
  [[ "$actual" =~ ^go([0-9]+)\.([0-9]+)\.([0-9]+)$ ]] ||
    bingo_fail "unsupported Go toolchain '$actual'; need Go $required or newer"
  major=${BASH_REMATCH[1]}
  minor=${BASH_REMATCH[2]}
  patch=${BASH_REMATCH[3]}
  if (( major < required_major ||
        (major == required_major && minor < required_minor) ||
        (major == required_major && minor == required_minor && patch < required_patch) )); then
    bingo_fail "Go $required or newer is required; found $actual"
  fi
}

bingo_check_node() {
  bingo_require node
  local required actual
  required=$(cat "$repository_root/.nvmrc")
  actual=$(node --version) || bingo_fail "cannot run Node.js"
  [[ "$actual" == "v${required}."* ]] ||
    bingo_fail "Node.js ${required}.x is required for reproducible Darwin builds; found $actual"
}

bingo_check_darwin() {
  if [[ "$bingo_host_os" == darwin ]]; then
    bingo_require codesign
    bingo_require xcrun
    xcrun --find clang >/dev/null ||
      bingo_fail "Xcode Command Line Tools are required; run xcode-select --install"
  fi
}

bingo_validate_version() {
  local version=$1
  [[ "${#version}" -le 80 &&
     ("$version" == dev || "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$) ]] ||
    bingo_fail "version must be dev or vMAJOR.MINOR.PATCH with an optional prerelease suffix (at most 80 characters)"
}

bingo_validate_commit() {
  [[ "$1" == unknown || "$1" =~ ^[0-9a-f]{40}(-dirty)?$ ]] ||
    bingo_fail "commit must be a full lowercase Git SHA, optionally suffixed -dirty, or unknown"
}
