#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
gate="$script_dir/darwin-verification-gate.sh"
workflow="$script_dir/../workflows/darwin-verification-gate.yml"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

mkdir "$tmpdir/bin"
cat > "$tmpdir/bin/gh" <<'MOCK'
#!/usr/bin/env bash

set -euo pipefail

printf '%s\n' "$*" >> "$MOCK_GH_LOG"

case "$1 $2" in
  "pr edit")
    if [ "$MOCK_EDIT_STATUS" -ne 0 ]; then
      echo "HTTP 403: Resource not accessible by integration" >&2
      exit "$MOCK_EDIT_STATUS"
    fi
    ;;
  "pr diff")
    printf '%s\n' "$MOCK_CHANGED_PATH"
    ;;
  "pr view")
    printf '%s\n' "$MOCK_HAS_LABEL"
    ;;
  *)
    echo "Unexpected gh command: $*" >&2
    exit 2
    ;;
esac
MOCK
chmod +x "$tmpdir/bin/gh"

case_count=0

while IFS='|' read -r name action event_label head_repository changed_path has_label edit_status expected_status expected_output expect_view; do
  case_count=$((case_count + 1))
  event_path="$tmpdir/event-$case_count.json"
  log_path="$tmpdir/gh-$case_count.log"
  jq -n \
    --arg action "$action" \
    --arg event_label "$event_label" \
    --arg head_repository "$head_repository" \
    '{action: $action, label: {name: $event_label}, pull_request: {number: 17, head: {repo: {full_name: $head_repository}}}}' \
    > "$event_path"

  status=0
  output=$(
    PATH="$tmpdir/bin:$PATH" \
      GITHUB_EVENT_PATH="$event_path" \
      GITHUB_REPOSITORY=bingosuite/bingo \
      MOCK_GH_LOG="$log_path" \
      MOCK_CHANGED_PATH="$changed_path" \
      MOCK_HAS_LABEL="$has_label" \
      MOCK_EDIT_STATUS="$edit_status" \
      DARWIN_NATIVE_REGEX='^never-match$' \
      VERIFIED_LABEL=attacker-controlled \
      EVENT_ACTION=opened \
      HAS_VERIFIED_LABEL=true \
      LABEL_CLEANUP=cleared \
      CHANGED_FILES="$tmpdir/attacker-changed.txt" \
      IS_FORK=false \
      PR_NUMBER=999 \
      REPOSITORY=attacker/repository \
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

  if ! grep -Fq -- '--repo bingosuite/bingo' "$log_path"; then
    printf 'not ok - %s (trusted repository was not used)\n' "$name" >&2
    exit 1
  fi

  if grep -Fq 'attacker-controlled' "$log_path"; then
    printf 'not ok - %s (hostile label override reached gh)\n' "$name" >&2
    exit 1
  fi

  if grep -Fq 'attacker/repository' "$log_path"; then
    printf 'not ok - %s (hostile repository override reached gh)\n' "$name" >&2
    exit 1
  fi

  if grep -Fq ' 999 ' "$log_path"; then
    printf 'not ok - %s (hostile PR number override reached gh)\n' "$name" >&2
    exit 1
  fi

  if [ "$action" = "synchronize" ] || [ "$action" = "reopened" ]; then
    if ! grep -Fq 'pr edit 17 --repo bingosuite/bingo --remove-label darwin-e2e-verified' "$log_path"; then
      printf 'not ok - %s (trusted cleanup inputs were not used)\n' "$name" >&2
      exit 1
    fi
  elif grep -Fq 'pr edit' "$log_path"; then
    printf 'not ok - %s (hostile action override triggered cleanup)\n' "$name" >&2
    exit 1
  fi

  if [ "$expect_view" = "true" ] && ! grep -Fq 'pr view' "$log_path"; then
    printf 'not ok - %s (expected live label query)\n' "$name" >&2
    exit 1
  fi

  if [ "$expect_view" = "false" ] && grep -Fq 'pr view' "$log_path"; then
    printf 'not ok - %s (unexpected live label query)\n' "$name" >&2
    exit 1
  fi

  printf 'ok - %s\n' "$name"
done <<'CASES'
hostile policy overrides cannot bypass fork synchronize|synchronize||contributor/fork|internal/debugger/backend_darwin_arm64.go|true|1|1|public-fork GITHUB_TOKEN is read-only|false
same-repository synchronize invalidates prior verification|synchronize||bingosuite/bingo|entitlements.plist|false|0|1|New commits and reopened pull requests invalidate prior Darwin verification|false
reopened fork fails closed with stale verification|reopened||contributor/fork|entitlements.plist|true|1|1|remove or toggle the stale label|false
verified label addition passes with live label|labeled|darwin-e2e-verified|contributor/fork|test/integration/debugger_e2e_darwin_arm64_test.go|true|0|0|label present; darwin backend verified locally|true
verified label removal fails without live label|unlabeled|darwin-e2e-verified|contributor/fork|internal/debugger/trap_arm64.go|false|0|1|Darwin E2E verification required|true
unrelated label cannot revive stale verification|labeled|triage|contributor/fork|entitlements.plist|true|0|1|label event cannot verify this head|true
unrelated unlabel cannot revive stale verification|unlabeled|triage|contributor/fork|entitlements.plist|true|0|1|label event cannot verify this head|true
non-Darwin synchronize bypasses gate after cleanup attempt|synchronize||contributor/fork|internal/debugger/engine.go|true|1|0|No darwin-native code changed; gate not required|false
near-match outside preserved regex bypasses gate|opened||contributor/fork|docs/backend_darwin_arm64.go|false|0|0|No darwin-native code changed; gate not required|false
CASES

case_count=$((case_count + 1))
if ! grep -Fq 'ref: ${{ github.event.pull_request.base.sha }}' "$workflow"; then
  echo "not ok - workflow executes the gate from the trusted base SHA" >&2
  exit 1
fi
echo "ok - workflow executes the gate from the trusted base SHA"

case_count=$((case_count + 1))
if grep -Eq '^[[:space:]]+(DARWIN_NATIVE_REGEX|VERIFIED_LABEL|EVENT_ACTION|HAS_VERIFIED_LABEL|LABEL_CLEANUP|PR_NUMBER|REPOSITORY):' "$workflow"; then
  echo "not ok - workflow supplies policy or decision inputs to the trusted gate" >&2
  exit 1
fi
echo "ok - workflow supplies no policy or decision inputs to the trusted gate"

case_count=$((case_count + 1))
if ! grep -Fq 'verification policy unavailable' "$workflow"; then
  echo "not ok - workflow fails closed when the trusted policy is unavailable" >&2
  exit 1
fi
echo "ok - workflow fails closed when the trusted policy is unavailable"

printf '1..%s\n' "$case_count"
