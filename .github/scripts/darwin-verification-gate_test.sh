#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
gate="$script_dir/darwin-verification-gate.sh"
trusted_workflow="$script_dir/../workflows/darwin-verification-gate.yml"
head_test_workflow="$script_dir/../workflows/darwin-verification-policy-test.yml"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

mkdir "$tmpdir/bin"
cat > "$tmpdir/bin/gh" <<'MOCK'
#!/usr/bin/env bash

set -euo pipefail

printf '%s\n' "$*" >> "$MOCK_GH_LOG"

if [ "$1 $2" = "pr edit" ]; then
  if [ "$MOCK_EDIT_STATUS" -ne 0 ]; then
    echo "HTTP 403: Resource not accessible by integration" >&2
    exit "$MOCK_EDIT_STATUS"
  fi
  exit 0
fi

if [ "$1" != "api" ]; then
  echo "Unexpected gh command: $*" >&2
  exit 2
fi

case "$*" in
  *"/statuses/"*)
    state=
    context=
    endpoint=
    for arg in "$@"; do
      case "$arg" in
        repos/*/statuses/*) endpoint=$arg ;;
        state=*) state=${arg#state=} ;;
        context=*) context=${arg#context=} ;;
      esac
    done
    printf '%s|%s|%s\n' "$endpoint" "$state" "$context" >> "$MOCK_STATUS_LOG"
    ;;
  *"/pulls/"*"/files?"*)
    if [ "$MOCK_DIFF_STATUS" -ne 0 ]; then
      echo "diff API failed" >&2
      exit "$MOCK_DIFF_STATUS"
    fi
    jq -n --arg filename "$MOCK_CHANGED_PATH" '[[{filename: $filename}]]'
    ;;
  *"/issues/"*"/labels?"*)
    if [ "$MOCK_LABEL_STATUS" -ne 0 ]; then
      echo "label API failed" >&2
      exit "$MOCK_LABEL_STATUS"
    fi
    if [ "$MOCK_HAS_LABEL" = "true" ]; then
      printf '%s\n' darwin-e2e-verified
    fi
    ;;
  *)
    echo "Unexpected gh api call: $*" >&2
    exit 2
    ;;
esac
MOCK
chmod +x "$tmpdir/bin/gh"

case_count=0

run_case() {
  local name=$1
  local action=$2
  local event_label=$3
  local head_repository=$4
  local changed_path=$5
  local has_label=$6
  local edit_status=$7
  local expected_exit=$8
  local expected_state=$9
  local expected_output=${10}
  local expected_label_query=${11}
  local diff_status=${12:-0}
  local label_status=${13:-0}
  local declared_changed_files=${14:-}
  local case_id
  local event_path
  local gh_log
  local status_log
  local head_sha
  local output
  local status=0

  case_count=$((case_count + 1))
  case_id=$case_count
  event_path="$tmpdir/event-$case_id.json"
  gh_log="$tmpdir/gh-$case_id.log"
  status_log="$tmpdir/status-$case_id.log"
  head_sha=$(printf '%040x' "$case_id")
  if [ -z "$declared_changed_files" ]; then
    declared_changed_files=$(printf '%s\n' "$changed_path" | wc -l | tr -d ' ')
  fi
  : > "$gh_log"
  : > "$status_log"

  jq -n \
    --arg action "$action" \
    --arg event_label "$event_label" \
    --arg head_repository "$head_repository" \
    --arg head_sha "$head_sha" \
    --argjson changed_files "$declared_changed_files" \
    '{
      action: $action,
      label: {name: $event_label},
      pull_request: {
        number: 17,
        changed_files: $changed_files,
        head: {sha: $head_sha, repo: {full_name: $head_repository}}
      }
    }' > "$event_path"

  output=$(
    PATH="$tmpdir/bin:$PATH" \
      GITHUB_EVENT_PATH="$event_path" \
      GITHUB_REPOSITORY=bingosuite/bingo \
      GITHUB_SERVER_URL=https://github.com \
      GITHUB_RUN_ID=123 \
      MOCK_GH_LOG="$gh_log" \
      MOCK_STATUS_LOG="$status_log" \
      MOCK_CHANGED_PATH="$changed_path" \
      MOCK_HAS_LABEL="$has_label" \
      MOCK_EDIT_STATUS="$edit_status" \
      MOCK_DIFF_STATUS="$diff_status" \
      MOCK_LABEL_STATUS="$label_status" \
      DARWIN_NATIVE_REGEX='^never-match$' \
      VERIFIED_LABEL=attacker-controlled \
      EVENT_ACTION=opened \
      HAS_VERIFIED_LABEL=true \
      LABEL_CLEANUP=cleared \
      CHANGED_FILES="$tmpdir/attacker-changed.txt" \
      IS_FORK=false \
      PR_NUMBER=999 \
      REPOSITORY=attacker/repository \
      GITHUB_WORKSPACE="$tmpdir/hostile-head" \
      bash "$gate" 2>&1
  ) || status=$?

  if [ "$status" -ne "$expected_exit" ]; then
    printf 'not ok - %s (status %s, expected %s)\n%s\n' \
      "$name" "$status" "$expected_exit" "$output" >&2
    exit 1
  fi

  if ! grep -Fq "$expected_output" <<< "$output"; then
    printf 'not ok - %s (missing output: %s)\n%s\n' \
      "$name" "$expected_output" "$output" >&2
    exit 1
  fi

  if grep -Eq 'attacker-controlled|attacker/repository| 999 ' "$gh_log"; then
    printf 'not ok - %s (hostile environment reached gh)\n' "$name" >&2
    exit 1
  fi

  if [ "$expected_label_query" = "true" ] &&
    ! grep -Fq "/issues/17/labels?" "$gh_log"; then
    printf 'not ok - %s (expected live label query)\n' "$name" >&2
    exit 1
  fi

  if [ "$expected_label_query" = "false" ] &&
    grep -Fq "/issues/17/labels?" "$gh_log"; then
    printf 'not ok - %s (unexpected live label query)\n' "$name" >&2
    exit 1
  fi

  if [ "$expected_state" = "none" ]; then
    if [ -s "$status_log" ]; then
      printf 'not ok - %s (unexpected head status)\n' "$name" >&2
      exit 1
    fi
  else
    expected_status="repos/bingosuite/bingo/statuses/$head_sha|$expected_state|Darwin E2E verified"
    if [ "$(tail -n 1 "$status_log")" != "$expected_status" ]; then
      printf 'not ok - %s (missing head-SHA status: %s)\n' \
        "$name" "$expected_status" >&2
      cat "$status_log" >&2
      exit 1
    fi
  fi

  printf 'ok - %s\n' "$name"
}

mkdir -p "$tmpdir/hostile-head/.github/workflows" "$tmpdir/hostile-head/.github/scripts"
printf '%s\n' 'jobs: {gate: {steps: [{run: "exit 0"}]}}' \
  > "$tmpdir/hostile-head/.github/workflows/darwin-verification-gate.yml"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' \
  > "$tmpdir/hostile-head/.github/scripts/darwin-verification-gate.sh"

run_case \
  "fork workflow exit 0 cannot bypass trusted synchronize" \
  synchronize "" contributor/fork internal/debugger/backend_darwin_arm64.go \
  true 1 1 failure "public-fork GITHUB_TOKEN could not remove" false
run_case \
  "same-repository synchronize fails current head" \
  synchronize "" bingosuite/bingo entitlements.plist \
  false 0 1 failure "re-verification required" false
run_case \
  "reopened pull request fails current head" \
  reopened "" contributor/fork entitlements.plist \
  true 1 1 failure "re-verification required" false
run_case \
  "verified label publishes success to current head" \
  labeled darwin-e2e-verified contributor/fork \
  test/integration/debugger_e2e_darwin_arm64_test.go \
  true 0 0 success "verified locally" true
run_case \
  "verified label removal publishes failure" \
  unlabeled darwin-e2e-verified contributor/fork internal/debugger/trap_arm64.go \
  false 0 1 failure "not currently present" true
run_case \
  "re-added verified label survives delayed unlabel event" \
  unlabeled darwin-e2e-verified contributor/fork internal/debugger/trap_arm64.go \
  true 0 0 success "verified locally" true
run_case \
  "unrelated label leaves prior status untouched" \
  labeled triage contributor/fork entitlements.plist \
  true 0 0 none "existing head-SHA status remains authoritative" false
run_case \
  "unrelated unlabel leaves prior status untouched" \
  unlabeled triage contributor/fork entitlements.plist \
  true 0 0 none "existing head-SHA status remains authoritative" false
run_case \
  "non-Darwin synchronize publishes success" \
  synchronize "" contributor/fork internal/debugger/engine.go \
  true 1 0 success "No darwin-native code changed" false
run_case \
  "opened Darwin change publishes failure" \
  opened "" contributor/fork entitlements.plist \
  false 0 1 failure "verification required" false
run_case \
  "near-match outside regex publishes success" \
  opened "" contributor/fork docs/backend_darwin_arm64.go \
  false 0 0 success "No darwin-native code changed" false
run_case \
  "diff API failure replaces pending with failure" \
  opened "" contributor/fork entitlements.plist \
  false 0 1 failure "diff API failed" false 1
run_case \
  "truncated PR files response fails closed" \
  opened "" contributor/fork docs/readme.md \
  false 0 1 failure "refusing an incomplete gate decision" false 0 0 3001
run_case \
  "newline filename cannot hide truncated response" \
  opened "" contributor/fork $'000-padding\nextra-line' \
  false 0 1 failure "returned 1 of 2 changed files" false 0 0 2

run_status_persistence_sequence() {
  local name=$1
  local first_action=$2
  local first_event_label=$3
  local first_has_label=$4
  local first_edit_status=$5
  local expected_state=$6
  local expected_first_exit=$7
  local case_id
  local head_sha
  local gh_log
  local status_log
  local first_event
  local unrelated_event
  local first_status=0
  local second_status=0

  case_count=$((case_count + 1))
  case_id=$case_count
  head_sha=$(printf '%040x' "$case_id")
  gh_log="$tmpdir/sequence-gh-$case_id.log"
  status_log="$tmpdir/sequence-status-$case_id.log"
  first_event="$tmpdir/sequence-first-$case_id.json"
  unrelated_event="$tmpdir/sequence-unrelated-$case_id.json"
  : > "$gh_log"
  : > "$status_log"

  jq -n \
    --arg action "$first_action" \
    --arg event_label "$first_event_label" \
    --arg head_sha "$head_sha" \
    '{
      action: $action,
      label: {name: $event_label},
      pull_request: {
        number: 17,
        changed_files: 1,
        head: {sha: $head_sha, repo: {full_name: "contributor/fork"}}
      }
    }' > "$first_event"
  jq -n \
    --arg head_sha "$head_sha" \
    '{
      action: "labeled",
      label: {name: "triage"},
      pull_request: {
        number: 17,
        changed_files: 1,
        head: {sha: $head_sha, repo: {full_name: "contributor/fork"}}
      }
    }' > "$unrelated_event"

  PATH="$tmpdir/bin:$PATH" \
    GITHUB_EVENT_PATH="$first_event" \
    GITHUB_REPOSITORY=bingosuite/bingo \
    MOCK_GH_LOG="$gh_log" \
    MOCK_STATUS_LOG="$status_log" \
    MOCK_CHANGED_PATH=entitlements.plist \
    MOCK_HAS_LABEL="$first_has_label" \
    MOCK_EDIT_STATUS="$first_edit_status" \
    MOCK_DIFF_STATUS=0 \
    MOCK_LABEL_STATUS=0 \
    bash "$gate" >/dev/null 2>&1 || first_status=$?

  PATH="$tmpdir/bin:$PATH" \
    GITHUB_EVENT_PATH="$unrelated_event" \
    GITHUB_REPOSITORY=bingosuite/bingo \
    MOCK_GH_LOG="$gh_log" \
    MOCK_STATUS_LOG="$status_log" \
    MOCK_CHANGED_PATH=entitlements.plist \
    MOCK_HAS_LABEL=true \
    MOCK_EDIT_STATUS=0 \
    MOCK_DIFF_STATUS=0 \
    MOCK_LABEL_STATUS=0 \
    bash "$gate" >/dev/null 2>&1 || second_status=$?

  if [ "$first_status" -ne "$expected_first_exit" ] ||
    [ "$second_status" -ne 0 ]; then
    printf 'not ok - %s (event statuses %s/%s)\n' \
      "$name" "$first_status" "$second_status" >&2
    exit 1
  fi

  expected_status="repos/bingosuite/bingo/statuses/$head_sha|$expected_state|Darwin E2E verified"
  if [ "$(wc -l < "$status_log" | tr -d ' ')" -ne 2 ] ||
    [ "$(tail -n 1 "$status_log")" != "$expected_status" ]; then
    printf 'not ok - %s (unrelated label changed the SHA-bound status)\n' \
      "$name" >&2
    cat "$status_log" >&2
    cat "$gh_log" >&2
    exit 1
  fi

  printf 'ok - %s\n' "$name"
}

run_status_persistence_sequence \
  "denied cleanup plus unrelated label preserves failure" \
  synchronize "" true 1 failure 1
run_status_persistence_sequence \
  "verified head plus unrelated label preserves success" \
  labeled darwin-e2e-verified true 0 success 0

case_count=$((case_count + 1))
if ! grep -Eq '^[[:space:]]+pull_request_target:' "$trusted_workflow" ||
  grep -Eq '^[[:space:]]+pull_request:' "$trusted_workflow"; then
  echo "not ok - trusted workflow is base-controlled pull_request_target only" >&2
  exit 1
fi
echo "ok - trusted workflow is base-controlled pull_request_target only"

case_count=$((case_count + 1))
if ! grep -Fq 'ref: ${{ github.event.pull_request.base.sha }}' "$trusted_workflow" ||
  grep -Fq 'github.event.pull_request.head.sha }}' "$trusted_workflow"; then
  echo "not ok - trusted workflow checks out only the base policy" >&2
  exit 1
fi
echo "ok - trusted workflow checks out only the base policy"

case_count=$((case_count + 1))
if ! grep -Eq '^[[:space:]]+pull_request:' "$head_test_workflow" ||
  ! grep -Fq 'bash .github/scripts/darwin-verification-gate_test.sh' "$head_test_workflow"; then
  echo "not ok - unprivileged pull_request workflow tests proposed HEAD policy" >&2
  exit 1
fi
echo "ok - unprivileged pull_request workflow tests proposed HEAD policy"

case_count=$((case_count + 1))
if grep -Fq 'Darwin E2E verified' "$head_test_workflow"; then
  echo "not ok - untrusted policy test can collide with required status context" >&2
  exit 1
fi
echo "ok - untrusted policy test has a distinct check name"

case_count=$((case_count + 1))
if ! grep -Fq 'statuses: write' "$trusted_workflow" ||
  grep -Eq 'statuses:[[:space:]]*write' "$head_test_workflow"; then
  echo "not ok - only the trusted workflow may publish commit statuses" >&2
  exit 1
fi
echo "ok - only the trusted workflow may publish commit statuses"

printf '1..%s\n' "$case_count"
