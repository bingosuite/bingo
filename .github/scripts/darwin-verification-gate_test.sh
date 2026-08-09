#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
gate="$script_dir/darwin-verification-gate.sh"
workflow="$script_dir/../workflows/darwin-verification-gate.yml"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

case_count=0

while IFS='|' read -r name action changed_path has_label cleanup expected_status expected_output; do
  case_count=$((case_count + 1))
  changed_files="$tmpdir/changed-$case_count.txt"
  printf '%s\n' "$changed_path" > "$changed_files"

  status=0
  output=$(
    EVENT_ACTION="$action" \
      CHANGED_FILES="$changed_files" \
      HAS_VERIFIED_LABEL="$has_label" \
      LABEL_CLEANUP="$cleanup" \
      bash "$gate" 2>&1
  ) || status=$?

  if [ "$status" -ne "$expected_status" ]; then
    printf 'not ok - %s (status %s, expected %s)\n%s\n' \
      "$name" "$status" "$expected_status" "$output" >&2
    exit 1
  fi

  if ! grep -Fq "$expected_output" <<< "$output"; then
    printf 'not ok - %s (missing output: %s)\n%s\n' \
      "$name" "$expected_output" "$output" >&2
    exit 1
  fi

  printf 'ok - %s\n' "$name"
done <<'CASES'
fork removal denied ignores stale live label|synchronize|internal/debugger/backend_darwin_arm64.go|true|failed|1|remove or toggle the stale label
same-repository synchronize invalidates prior verification|synchronize|entitlements.plist|false|cleared|1|New commits always invalidate prior Darwin verification
same-repository relabel verifies the same head|labeled|entitlements.plist|true|not-run|0|label present; darwin backend verified locally
labeled Darwin change passes with live label|labeled|test/integration/debugger_e2e_darwin_arm64_test.go|true|not-run|0|label present; darwin backend verified locally
unlabeled Darwin change fails without live label|unlabeled|internal/debugger/trap_arm64.go|false|not-run|1|Darwin E2E verification required
non-Darwin change bypasses gate|synchronize|internal/debugger/engine.go|true|failed|0|No darwin-native code changed; gate not required
near-match outside preserved regex bypasses gate|opened|docs/backend_darwin_arm64.go|false|not-run|0|No darwin-native code changed; gate not required
CASES

case_count=$((case_count + 1))
if ! grep -Fq 'ref: ${{ github.event.pull_request.base.sha }}' "$workflow"; then
  echo "not ok - workflow executes the gate from the trusted base SHA" >&2
  exit 1
fi
echo "ok - workflow executes the gate from the trusted base SHA"

case_count=$((case_count + 1))
expected_regex='^(internal/debugger/.*_darwin_.*|internal/debugger/trap_arm64\.go|test/integration/.*_darwin_.*_test\.go|entitlements\.plist)$'
if ! grep -Fq "DARWIN_NATIVE_REGEX: '$expected_regex'" "$workflow"; then
  echo "not ok - workflow bootstrap preserves the Darwin path regex" >&2
  exit 1
fi
echo "ok - workflow bootstrap preserves the Darwin path regex"

printf '1..%s\n' "$case_count"
