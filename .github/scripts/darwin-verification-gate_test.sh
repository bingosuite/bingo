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
    # Only successfully published statuses are logged, so the log is an exact
    # record of what GitHub would show on the head SHA.
    for failing in ${MOCK_STATUS_FAIL_STATES:-}; do
      if [ "$failing" = "$state" ]; then
        echo "status API failed for state $state" >&2
        exit 1
      fi
    done
    printf '%s|%s|%s\n' "$endpoint" "$state" "$context" >> "$MOCK_STATUS_LOG"
    ;;
  *"/pulls/"*"/files?"*)
    if [ "$MOCK_DIFF_STATUS" -ne 0 ]; then
      echo "diff API failed" >&2
      exit "$MOCK_DIFF_STATUS"
    fi
    # `--slurp` yields one array per fetched page; the gate must flatten them.
    jq -n \
      --argjson filenames "$MOCK_CHANGED_PATHS_JSON" \
      --argjson pages "$MOCK_PAGES" '
        [$filenames[] | {filename: .}] as $files
        | ((($files | length) + $pages - 1) / $pages | floor) as $size
        | [range(0; $pages) | $files[(. * $size):((. + 1) * $size)]]
        | map(select(length > 0))
      '
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

fail_case() {
  printf 'not ok - %s\n' "$1" >&2
  shift
  if [ "$#" -gt 0 ]; then
    printf '%s\n' "$@" >&2
  fi
  exit 1
}

# run_case NAME key=value...
#
# Keys: action, label, head_repo, path, paths_json, pages, has_label,
# edit_status, diff_status, label_status, status_fail, declared_files,
# exit, state, states, output, label_query, decision, gh_contains, gh_missing.
run_case() {
  local name=$1
  shift

  local action=opened label='' head_repo=contributor/fork
  local path=entitlements.plist paths_json='' pages=1
  local has_label=false edit_status=0 diff_status=0 label_status=0
  local status_fail='' declared_files='' expect_exit=0 expect_state=none
  local expect_states=''
  local expect_output='' expect_label_query=any expect_decision=''
  local gh_contains='' gh_missing=''
  local kv key value

  for kv in "$@"; do
    key=${kv%%=*}
    value=${kv#*=}
    case "$key" in
      action) action=$value ;;
      label) label=$value ;;
      head_repo) head_repo=$value ;;
      path) path=$value ;;
      paths_json) paths_json=$value ;;
      pages) pages=$value ;;
      has_label) has_label=$value ;;
      edit_status) edit_status=$value ;;
      diff_status) diff_status=$value ;;
      label_status) label_status=$value ;;
      status_fail) status_fail=$value ;;
      declared_files) declared_files=$value ;;
      exit) expect_exit=$value ;;
      state) expect_state=$value ;;
      states) expect_states=$value ;;
      output) expect_output=$value ;;
      label_query) expect_label_query=$value ;;
      decision) expect_decision=$value ;;
      gh_contains) gh_contains=$value ;;
      gh_missing) gh_missing=$value ;;
      *) fail_case "$name" "unknown run_case key: $key" ;;
    esac
  done

  case_count=$((case_count + 1))
  local case_id=$case_count
  local event_path="$tmpdir/event-$case_id.json"
  local gh_log="$tmpdir/gh-$case_id.log"
  local status_log="$tmpdir/status-$case_id.log"
  local decision_file="$tmpdir/decision-$case_id.txt"
  local head_sha
  head_sha=$(printf '%040x' "$case_id")
  local output
  local status=0

  if [ -z "$paths_json" ]; then
    paths_json=$(jq -n --arg path "$path" '[$path]')
  fi
  if [ -z "$declared_files" ]; then
    declared_files=$(jq -n --argjson paths "$paths_json" '$paths | length')
  fi
  : > "$gh_log"
  : > "$status_log"
  rm -f "$decision_file"

  jq -n \
    --arg action "$action" \
    --arg event_label "$label" \
    --arg head_repository "$head_repo" \
    --arg head_sha "$head_sha" \
    --argjson changed_files "$declared_files" \
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
      DARWIN_GATE_DECISION_FILE="$decision_file" \
      MOCK_GH_LOG="$gh_log" \
      MOCK_STATUS_LOG="$status_log" \
      MOCK_CHANGED_PATHS_JSON="$paths_json" \
      MOCK_PAGES="$pages" \
      MOCK_HAS_LABEL="$has_label" \
      MOCK_EDIT_STATUS="$edit_status" \
      MOCK_DIFF_STATUS="$diff_status" \
      MOCK_LABEL_STATUS="$label_status" \
      MOCK_STATUS_FAIL_STATES="$status_fail" \
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

  if [ "$status" -ne "$expect_exit" ]; then
    fail_case "$name" "status $status, expected $expect_exit" "$output"
  fi

  if [ -n "$expect_output" ] && ! grep -Fq -- "$expect_output" <<< "$output"; then
    fail_case "$name" "missing output: $expect_output" "$output"
  fi

  if grep -Eq 'attacker-controlled|attacker/repository| 999 ' "$gh_log"; then
    fail_case "$name" "hostile environment reached gh" "$(cat "$gh_log")"
  fi

  if [ "$expect_label_query" = "true" ] &&
    ! grep -Fq "/issues/17/labels?" "$gh_log"; then
    fail_case "$name" "expected live label query" "$(cat "$gh_log")"
  fi

  if [ "$expect_label_query" = "false" ] &&
    grep -Fq "/issues/17/labels?" "$gh_log"; then
    fail_case "$name" "unexpected live label query" "$(cat "$gh_log")"
  fi

  if [ -n "$gh_contains" ] && ! grep -Fq -- "$gh_contains" "$gh_log"; then
    fail_case "$name" "expected gh call: $gh_contains" "$(cat "$gh_log")"
  fi

  if [ -n "$gh_missing" ] && grep -Fq -- "$gh_missing" "$gh_log"; then
    fail_case "$name" "unexpected gh call: $gh_missing" "$(cat "$gh_log")"
  fi

  if [ "$expect_state" = "none" ]; then
    if [ -s "$status_log" ]; then
      fail_case "$name" "unexpected head status" "$(cat "$status_log")"
    fi
  else
    local expected_status="repos/bingosuite/bingo/statuses/$head_sha|$expect_state|Darwin E2E verified"
    if [ "$(tail -n 1 "$status_log")" != "$expected_status" ]; then
      fail_case "$name" "missing head-SHA status: $expected_status" \
        "$(cat "$status_log")"
    fi
  fi

  # Assert the exact published sequence, not just the terminal status: the head
  # must move `pending` -> decision, and nothing may follow a decision.
  local want_states=$expect_states
  if [ -z "$want_states" ]; then
    if [ "$expect_state" = "none" ]; then
      want_states=''
    else
      want_states="pending,$expect_state"
    fi
  fi
  [ "$want_states" = "none" ] && want_states=''
  local actual_states
  actual_states=$(cut -d'|' -f2 "$status_log" | paste -sd, -)
  if [ "$actual_states" != "$want_states" ]; then
    fail_case "$name" "status sequence '$actual_states', expected '$want_states'" \
      "$(cat "$status_log")"
  fi

  local decision=''
  if [ -f "$decision_file" ]; then
    decision=$(tr -d '\n' < "$decision_file")
  fi
  if [ -n "$expect_decision" ]; then
    local want=$expect_decision
    [ "$want" = "none" ] && want=''
    if [ "$decision" != "$want" ]; then
      fail_case "$name" "decision '$decision', expected '$want'"
    fi
  fi

  printf 'ok - %s\n' "$name"
}

mkdir -p "$tmpdir/hostile-head/.github/workflows" "$tmpdir/hostile-head/.github/scripts"
printf '%s\n' 'jobs: {gate: {steps: [{run: "exit 0"}]}}' \
  > "$tmpdir/hostile-head/.github/workflows/darwin-verification-gate.yml"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' \
  > "$tmpdir/hostile-head/.github/scripts/darwin-verification-gate.sh"

# --- synchronize / reopened: always unverify the new head --------------------

run_case "fork workflow exit 0 cannot bypass trusted synchronize" \
  action=synchronize head_repo=contributor/fork \
  path=internal/debugger/backend_darwin_arm64.go has_label=true edit_status=1 \
  exit=1 state=failure label_query=false decision=failure \
  output="public-fork GITHUB_TOKEN could not remove"

run_case "same-repository synchronize fails current head" \
  action=synchronize head_repo=bingosuite/bingo path=entitlements.plist \
  exit=1 state=failure label_query=false decision=failure \
  gh_contains="pr edit 17" output="re-verification required"

run_case "same-repository cleanup denial warns without fork wording" \
  action=synchronize head_repo=bingosuite/bingo path=entitlements.plist \
  has_label=true edit_status=1 \
  exit=1 state=failure label_query=false decision=failure \
  output="The API could not remove"

run_case "successful cleanup omits the stale-label recovery hint" \
  action=synchronize head_repo=contributor/fork path=entitlements.plist \
  has_label=true edit_status=0 \
  exit=1 state=failure label_query=false decision=failure \
  gh_contains="--remove-label darwin-e2e-verified" \
  output="then add the 'darwin-e2e-verified' label to verify this head"

run_case "reopened pull request fails current head" \
  action=reopened head_repo=contributor/fork path=entitlements.plist \
  has_label=true edit_status=1 \
  exit=1 state=failure label_query=false decision=failure \
  output="re-verification required"

run_case "synchronize with a live verified label still fails closed" \
  action=synchronize head_repo=contributor/fork \
  path=internal/debugger/trap_arm64.go has_label=true edit_status=0 \
  exit=1 state=failure label_query=false decision=failure \
  output="re-verification required"

# --- opened ------------------------------------------------------------------

run_case "opened Darwin change publishes failure" \
  action=opened head_repo=contributor/fork path=entitlements.plist \
  exit=1 state=failure label_query=false decision=failure \
  gh_missing="pr edit" output="verification required"

run_case "opened Darwin change ignores a pre-existing label" \
  action=opened head_repo=contributor/fork path=entitlements.plist \
  has_label=true \
  exit=1 state=failure label_query=false decision=failure \
  output="verification required"

# --- verified label add / remove ---------------------------------------------

run_case "verified label publishes success to current head" \
  action=labeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=test/integration/debugger_e2e_darwin_arm64_test.go has_label=true \
  exit=0 state=success label_query=true decision=success \
  gh_missing="pr edit" output="verified locally"

run_case "verified label removal publishes failure" \
  action=unlabeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=internal/debugger/trap_arm64.go has_label=false \
  exit=1 state=failure label_query=true decision=failure \
  output="not currently present"

run_case "re-added verified label survives delayed unlabel event" \
  action=unlabeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=internal/debugger/trap_arm64.go has_label=true \
  exit=0 state=success label_query=true decision=success \
  output="verified locally"

run_case "removed verified label defeats a delayed label event" \
  action=labeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=internal/debugger/trap_arm64.go has_label=false \
  exit=1 state=failure label_query=true decision=failure \
  output="not currently present"

run_case "live label API failure fails closed" \
  action=labeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=entitlements.plist has_label=true label_status=1 \
  exit=1 state=failure decision=failure output="label API failed"

# --- unrelated labels --------------------------------------------------------

run_case "unrelated label leaves prior status untouched" \
  action=labeled label=triage head_repo=contributor/fork \
  path=entitlements.plist has_label=true \
  exit=0 state=none label_query=false decision=ignored \
  gh_missing="statuses/" output="existing head-SHA status remains authoritative"

run_case "unrelated unlabel leaves prior status untouched" \
  action=unlabeled label=triage head_repo=contributor/fork \
  path=entitlements.plist has_label=true \
  exit=0 state=none label_query=false decision=ignored \
  output="existing head-SHA status remains authoritative"

run_case "verified-label prefix does not count as the verified label" \
  action=labeled label=darwin-e2e-verified-2 head_repo=contributor/fork \
  path=entitlements.plist has_label=true \
  exit=0 state=none label_query=false decision=ignored \
  output="existing head-SHA status remains authoritative"

# --- path matching -----------------------------------------------------------

run_case "non-Darwin synchronize publishes success" \
  action=synchronize head_repo=contributor/fork path=internal/debugger/engine.go \
  has_label=true edit_status=1 \
  exit=0 state=success label_query=false decision=success \
  output="No darwin-native code changed"

run_case "near-match outside regex publishes success" \
  action=opened head_repo=contributor/fork path=docs/backend_darwin_arm64.go \
  exit=0 state=success label_query=false decision=success \
  output="No darwin-native code changed"

run_case "one Darwin file among many still gates" \
  action=opened head_repo=contributor/fork \
  paths_json='["README.md","internal/hub/hub.go","internal/debugger/backend_darwin_arm64.go","docs/x.md"]' \
  exit=1 state=failure decision=failure output="verification required"

# --- PR files API integrity --------------------------------------------------

run_case "paginated file list is flattened and gated" \
  action=opened head_repo=contributor/fork \
  paths_json='["a.md","b.md","c.md","entitlements.plist"]' pages=2 \
  exit=1 state=failure decision=failure output="verification required"

run_case "paginated non-Darwin file list publishes success" \
  action=opened head_repo=contributor/fork \
  paths_json='["a.md","b.md","c.md","d.md","e.md","f.md"]' pages=3 \
  exit=0 state=success decision=success output="No darwin-native code changed"

run_case "truncated PR files response fails closed" \
  action=opened head_repo=contributor/fork path=docs/readme.md \
  declared_files=3001 \
  exit=1 state=failure decision=failure \
  output="refusing an incomplete gate decision"

run_case "newline filename cannot hide truncated response" \
  action=opened head_repo=contributor/fork path=$'000-padding\nextra-line' \
  declared_files=2 \
  exit=1 state=failure decision=failure output="returned 1 of 2 changed files"

run_case "over-reported file list also fails closed" \
  action=opened head_repo=contributor/fork \
  paths_json='["a.md","b.md"]' declared_files=1 \
  exit=1 state=failure decision=failure \
  output="refusing an incomplete gate decision"

run_case "diff API failure replaces pending with failure" \
  action=opened head_repo=contributor/fork path=entitlements.plist \
  diff_status=1 \
  exit=1 state=failure decision=failure output="diff API failed"

# --- status API failures -----------------------------------------------------

# A pending POST that fails aborts before any evaluation; the ERR trap still
# publishes an explicit failure to the head.
run_case "pending status failure still fails the head closed" \
  action=opened head_repo=contributor/fork path=entitlements.plist \
  status_fail=pending \
  exit=1 state=failure states=failure decision=failure \
  output="status API failed for state pending"

# When even the fail-closed POST cannot be delivered, no decision is recorded and
# the workflow guard step takes over.
run_case "unpublishable failure records no decision" \
  action=opened head_repo=contributor/fork path=entitlements.plist \
  status_fail=failure \
  exit=1 state=pending states=pending decision=none \
  output="status API failed for state failure"

# A success POST that fails must degrade to failure, never to silence.
run_case "unpublishable success degrades to failure" \
  action=synchronize head_repo=contributor/fork path=internal/hub/hub.go \
  status_fail=success \
  exit=1 state=failure states=pending,failure decision=failure \
  output="status API failed for state success"

# --- unsupported actions -----------------------------------------------------

run_case "unsupported action refuses to decide" \
  action=edited head_repo=contributor/fork path=entitlements.plist \
  exit=2 state=none decision=none gh_missing="statuses/" \
  output="Unsupported pull_request_target action: edited"

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
    MOCK_CHANGED_PATHS_JSON='["entitlements.plist"]' \
    MOCK_PAGES=1 \
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
    MOCK_CHANGED_PATHS_JSON='["entitlements.plist"]' \
    MOCK_PAGES=1 \
    MOCK_HAS_LABEL=true \
    MOCK_EDIT_STATUS=0 \
    MOCK_DIFF_STATUS=0 \
    MOCK_LABEL_STATUS=0 \
    bash "$gate" >/dev/null 2>&1 || second_status=$?

  if [ "$first_status" -ne "$expected_first_exit" ] ||
    [ "$second_status" -ne 0 ]; then
    fail_case "$name" "event statuses $first_status/$second_status"
  fi

  expected_status="repos/bingosuite/bingo/statuses/$head_sha|$expected_state|Darwin E2E verified"
  if [ "$(wc -l < "$status_log" | tr -d ' ')" -ne 2 ] ||
    [ "$(tail -n 1 "$status_log")" != "$expected_status" ]; then
    fail_case "$name" "unrelated label changed the SHA-bound status" \
      "$(cat "$status_log")" "$(cat "$gh_log")"
  fi

  printf 'ok - %s\n' "$name"
}

run_status_persistence_sequence \
  "denied cleanup plus unrelated label preserves failure" \
  synchronize "" true 1 failure 1
run_status_persistence_sequence \
  "verified head plus unrelated label preserves success" \
  labeled darwin-e2e-verified true 0 success 0

# Workflow invariants are asserted through named predicates rather than an
# `eval`'d string so the checks stay statically analysable.
assert_workflow() {
  local name=$1
  shift

  case_count=$((case_count + 1))
  if ! "$@"; then
    fail_case "$name" "workflow invariant not satisfied"
  fi
  printf 'ok - %s\n' "$name"
}

trusted_is_base_controlled() {
  grep -Eq '^[[:space:]]+pull_request_target:' "$trusted_workflow" &&
    ! grep -Eq '^[[:space:]]+pull_request:' "$trusted_workflow"
}

# GitHub Actions '${{ }}' expressions are matched literally, not expanded.
# shellcheck disable=SC2016
trusted_checks_out_base_only() {
  grep -Fq 'ref: ${{ github.event.pull_request.base.sha }}' "$trusted_workflow" &&
    ! grep -Fq 'github.event.pull_request.head.sha }}' "$trusted_workflow"
}

trusted_subscribes_to_gated_actions() {
  grep -Fq 'types: [opened, synchronize, reopened, labeled, unlabeled]' \
    "$trusted_workflow"
}

# Cancelling a run between its pending and final status can strand the required
# head status on pending, so superseding must only ever drop still-queued runs.
trusted_never_cancels_in_progress() {
  ! grep -Eq 'cancel-in-progress:[[:space:]]*true' "$trusted_workflow" &&
    grep -Eq 'cancel-in-progress:[[:space:]]*false' "$trusted_workflow"
}

unrelated_labels_are_isolated() {
  grep -Fq "format('unrelated-{0}', github.run_id)" "$trusted_workflow" &&
    grep -Fq "github.event.label.name != 'darwin-e2e-verified'" "$trusted_workflow"
}

# GitHub Actions '${{ }}' expressions are matched literally, not expanded.
# shellcheck disable=SC2016
gating_events_share_one_group() {
  grep -Fq 'darwin-verification-${{ github.event.pull_request.number }}' \
    "$trusted_workflow"
}

trusted_fails_closed_without_decision() {
  grep -Fq 'if: always()' "$trusted_workflow" &&
    grep -Fq 'DARWIN_GATE_DECISION_FILE' "$trusted_workflow" &&
    grep -Fq 'Darwin verification gate ended without a decision.' "$trusted_workflow"
}

head_tests_run_proposed_policy() {
  grep -Eq '^[[:space:]]+pull_request:' "$head_test_workflow" &&
    grep -Fq 'bash .github/scripts/darwin-verification-gate_test.sh' \
      "$head_test_workflow"
}

head_tests_use_a_distinct_check_name() {
  ! grep -Fq 'Darwin E2E verified' "$head_test_workflow"
}

only_trusted_publishes_statuses() {
  grep -Fq 'statuses: write' "$trusted_workflow" &&
    ! grep -Eq 'statuses:[[:space:]]*write' "$head_test_workflow"
}

head_tests_cannot_write_pull_requests() {
  ! grep -Eq 'pull-requests:[[:space:]]*write' "$head_test_workflow"
}

# Bash >= 4.4 propagates an ERR trap into command-substitution subshells, so an
# unguarded $(...) publishes a duplicate status from the subshell before the
# parent publishes its own. Every substitution below the trap must disarm it.
# shellcheck disable=SC2016  # shell syntax is matched literally, not expanded.
command_substitutions_disarm_the_err_trap() {
  local body
  body=$(awk '/^trap fail_closed ERR$/ {found = 1; next} found' "$gate")
  [ -n "$body" ] || return 1

  local offenders
  offenders=$(printf '%s\n' "$body" | grep -F '$(' | grep -Fv '$(trap - ERR;' || true)
  if [ -n "$offenders" ]; then
    printf 'unguarded command substitution:\n%s\n' "$offenders" >&2
    return 1
  fi
}

gate_workflows_are_valid_yaml() {
  python3 -c 'import sys, yaml
for path in sys.argv[1:]:
    with open(path) as handle:
        yaml.safe_load(handle)' "$trusted_workflow" "$head_test_workflow"
}

assert_workflow "trusted workflow is base-controlled pull_request_target only" \
  trusted_is_base_controlled
assert_workflow "trusted workflow checks out only the base policy" \
  trusted_checks_out_base_only
assert_workflow "trusted workflow subscribes to every gated action" \
  trusted_subscribes_to_gated_actions
assert_workflow "trusted workflow never cancels a run in progress" \
  trusted_never_cancels_in_progress
assert_workflow "unrelated label events cannot supersede a gating run" \
  unrelated_labels_are_isolated
assert_workflow "gating events share one per-pull-request concurrency group" \
  gating_events_share_one_group
assert_workflow "trusted workflow fails closed without a published decision" \
  trusted_fails_closed_without_decision
assert_workflow "unprivileged pull_request workflow tests proposed HEAD policy" \
  head_tests_run_proposed_policy
assert_workflow "untrusted policy test has a distinct check name" \
  head_tests_use_a_distinct_check_name
assert_workflow "only the trusted workflow may publish commit statuses" \
  only_trusted_publishes_statuses
assert_workflow "untrusted policy test cannot write pull requests" \
  head_tests_cannot_write_pull_requests
assert_workflow "command substitutions cannot double-publish a status" \
  command_substitutions_disarm_the_err_trap

if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' 2>/dev/null; then
  assert_workflow "gate workflows are valid YAML" gate_workflows_are_valid_yaml
fi

printf '1..%s\n' "$case_count"
