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
  *"/commits/"*"/statuses?"*)
    jq -n \
      --arg state "$MOCK_PRIOR_STATUS_STATE" \
      --arg creator "$MOCK_PRIOR_STATUS_CREATOR" \
      --arg target "$MOCK_PRIOR_STATUS_TARGET" '
        if $state == "" then
          [[]]
        else
          [[{
            context: "Darwin E2E verified",
            state: $state,
            creator: {login: $creator},
            target_url: $target
          }]]
        end
      '
    ;;
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
  *"/compare/"*)
    if [ "$MOCK_DIFF_STATUS" -ne 0 ]; then
      echo "diff API failed" >&2
      exit "$MOCK_DIFF_STATUS"
    fi
    printf '%s\n' "$MOCK_MERGE_BASE_SHA"
    ;;
  *"/git/commits/"*)
    case "$*" in
      *"/git/commits/$MOCK_MERGE_BASE_SHA"*)
        printf '%s\n' "$MOCK_BASE_TREE_SHA"
        ;;
      *"/git/commits/$MOCK_HEAD_SHA"*)
        printf '%s\n' "$MOCK_HEAD_TREE_SHA"
        ;;
      *)
        echo "Unexpected commit lookup: $*" >&2
        exit 2
        ;;
    esac
    ;;
  *"/git/trees/"*)
    paths='[]'
    truncated=false
    case "$*" in
      *"/git/trees/$MOCK_BASE_TREE_SHA"*)
        paths=$MOCK_BASE_PATHS_JSON
        truncated=$MOCK_BASE_TREE_TRUNCATED
        ;;
      *"/git/trees/$MOCK_HEAD_TREE_SHA"*)
        paths=$MOCK_CHANGED_PATHS_JSON
        truncated=$MOCK_TREE_TRUNCATED
        ;;
      *)
        echo "Unexpected tree lookup: $*" >&2
        exit 2
        ;;
    esac
    jq -n \
      --argjson paths "$paths" \
      --argjson truncated "$truncated" '
        {
          truncated: $truncated,
          tree: [
            range(0; $paths | length) as $index
            | {
                mode: "100644",
                path: $paths[$index],
                sha: ($index | tostring),
                type: "blob"
              }
          ]
        }
      '
    ;;
  *"/git/blobs/"*)
    path=
    for arg in "$@"; do
      case "$arg" in
        repos/*/git/blobs/*) path=$arg ;;
      esac
    done
    sha=${path##*/}
    content=$(jq -r --arg sha "$sha" '.[$sha] // "package bingo\n"' \
      "$MOCK_BLOB_CONTENTS_FILE")
    printf '%s' "$content" | base64
    ;;
  *"/issues/"*"/labels?"*)
    if [ "$MOCK_LABEL_STATUS" -ne 0 ]; then
      echo "label API failed" >&2
      exit "$MOCK_LABEL_STATUS"
    fi
    # Anything other than true/false is emitted verbatim as the live label set,
    # so a case can pin that only an exact match counts as verification.
    case "$MOCK_HAS_LABEL" in
      true) printf '%s\n' darwin-e2e-verified ;;
      false) ;;
      *) printf '%s\n' "$MOCK_HAS_LABEL" ;;
    esac
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
# Keys: action, label, head_repo, path, paths_json, base_paths_json,
# base_changed, tree_truncated, blob_contents_json, prior_status,
# prior_creator, prior_target, has_label, edit_status, diff_status, label_status,
# status_fail, exit, state, states, output, label_query, decision, gh_contains,
# gh_missing.
run_case() {
  local name=$1
  shift

  local action=opened label='' head_repo=contributor/fork
  local path=entitlements.plist paths_json='' base_paths_json='[]'
  local base_changed=false base_tree_truncated=false tree_truncated=false
  local blob_contents_json='{}'
  local prior_status=failure prior_creator='github-actions[bot]'
  local prior_target='https://github.com/bingosuite/bingo/actions/runs/122'
  local has_label=false edit_status=0 diff_status=0 label_status=0
  local status_fail='' expect_exit=0 expect_state=none
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
      base_paths_json) base_paths_json=$value ;;
      base_changed) base_changed=$value ;;
      base_tree_truncated) base_tree_truncated=$value ;;
      tree_truncated) tree_truncated=$value ;;
      blob_contents_json) blob_contents_json=$value ;;
      prior_status) prior_status=$value ;;
      prior_creator) prior_creator=$value ;;
      prior_target) prior_target=$value ;;
      has_label) has_label=$value ;;
      edit_status) edit_status=$value ;;
      diff_status) diff_status=$value ;;
      label_status) label_status=$value ;;
      status_fail) status_fail=$value ;;
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
  local blob_contents_file="$tmpdir/blobs-$case_id.json"
  local head_sha
  head_sha=$(printf '%040x' "$case_id")
  local base_sha
  base_sha=$(printf 'f%039x' "$case_id")
  local merge_base_sha
  merge_base_sha=$(printf 'e%039x' "$case_id")
  local base_tree_sha
  base_tree_sha=$(printf 'c%039x' "$case_id")
  local head_tree_sha
  head_tree_sha=$(printf 'd%039x' "$case_id")
  local output
  local status=0

  if [ -z "$paths_json" ]; then
    paths_json=$(jq -n --arg path "$path" '[$path]')
  fi
  : > "$gh_log"
  : > "$status_log"
  printf '%s\n' "$blob_contents_json" > "$blob_contents_file"
  rm -f "$decision_file"

  jq -n \
    --arg action "$action" \
    --arg event_label "$label" \
    --arg head_repository "$head_repo" \
    --arg head_sha "$head_sha" \
    --arg base_sha "$base_sha" \
    --argjson base_changed "$base_changed" \
    --argjson changed_files "$(jq -n --argjson paths "$paths_json" '$paths | length')" \
    '{
      action: $action,
      label: {name: $event_label},
      changes: (
        if $base_changed
        then {base: {ref: {from: "previous-base"}}}
        else {}
        end
      ),
      pull_request: {
        number: 17,
        changed_files: $changed_files,
        base: {sha: $base_sha},
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
      MOCK_BASE_PATHS_JSON="$base_paths_json" \
      MOCK_BASE_TREE_TRUNCATED="$base_tree_truncated" \
      MOCK_TREE_TRUNCATED="$tree_truncated" \
      MOCK_MERGE_BASE_SHA="$merge_base_sha" \
      MOCK_BASE_TREE_SHA="$base_tree_sha" \
      MOCK_HEAD_TREE_SHA="$head_tree_sha" \
      MOCK_HEAD_SHA="$head_sha" \
      MOCK_BLOB_CONTENTS_FILE="$blob_contents_file" \
      MOCK_PRIOR_STATUS_STATE="$prior_status" \
      MOCK_PRIOR_STATUS_CREATOR="$prior_creator" \
      MOCK_PRIOR_STATUS_TARGET="$prior_target" \
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

  if grep -Fq "/pulls/17/files" "$gh_log"; then
    fail_case "$name" "gate consulted the live pull request file list" \
      "$(cat "$gh_log")"
  fi

  if grep -Fq '|pending|' "$status_log" &&
    ! grep -Fq "/compare/$base_sha...$head_sha" "$gh_log"; then
    fail_case "$name" "gate did not bind its diff to event base/head SHAs" \
      "$(cat "$gh_log")"
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
  output="then add the 'darwin-e2e-verified' label"

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

run_case "verified label cannot supersede an unevaluated head" \
  action=labeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=entitlements.plist has_label=true prior_status='' \
  exit=1 state=failure label_query=true decision=failure \
  output="has not completed a trusted failing gate run"

run_case "untrusted prior failure cannot authorize a verified label" \
  action=labeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=entitlements.plist has_label=true prior_creator=octocat \
  exit=1 state=failure label_query=true decision=failure \
  output="has not completed a trusted failing gate run"

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

# The live-label read is the only path that can publish a green status, so a
# label that merely contains the verified name must not satisfy it.
run_case "a live label that only contains the verified name is not a match" \
  action=labeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=entitlements.plist has_label=darwin-e2e-verified-2 \
  exit=1 state=failure label_query=true decision=failure \
  output="not currently present"

run_case "the verified label counts among several live labels" \
  action=labeled label=darwin-e2e-verified head_repo=contributor/fork \
  path=entitlements.plist has_label='triage
darwin-e2e-verified
needs-review' \
  exit=0 state=success label_query=true decision=success \
  output="verified locally"

# --- pull request edits ------------------------------------------------------

run_case "non-base pull request edit preserves the current status" \
  action=edited base_changed=false head_repo=contributor/fork \
  path=entitlements.plist \
  exit=0 state=none decision=ignored gh_missing="statuses/" \
  output="did not change the base"

run_case "base retarget invalidates Darwin verification" \
  action=edited base_changed=true head_repo=contributor/fork \
  path=entitlements.plist has_label=true edit_status=1 \
  exit=1 state=failure decision=failure label_query=false \
  output="re-verification required"

# --- path matching -----------------------------------------------------------

run_case "non-Darwin synchronize publishes success" \
  action=synchronize head_repo=contributor/fork path=internal/hub/hub.go \
  has_label=true edit_status=1 \
  exit=0 state=success label_query=false decision=success \
  output="No darwin-native code changed"

run_case "near-match outside regex publishes success" \
  action=opened head_repo=contributor/fork path=docs/backend-darwin-arm64.go \
  exit=0 state=success label_query=false decision=success \
  output="No darwin-native code changed"

run_case "darwin GOOS suffix without arch still gates" \
  action=opened head_repo=contributor/fork \
  path=internal/debugger/backend_darwin.go \
  exit=1 state=failure decision=failure output="verification required"

run_case "darwin suffix in a new package still gates" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/attach_darwin.go \
  exit=1 state=failure decision=failure output="verification required"

run_case "arm64 assembly in a new package still gates" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/attach_arm64.s \
  exit=1 state=failure decision=failure output="verification required"

run_case "arm64 test suffix still gates" \
  action=opened head_repo=contributor/fork \
  path=internal/debugger/trap_arm64_test.go \
  exit=1 state=failure decision=failure output="verification required"

run_case "native source without platform suffix gates conservatively" \
  action=opened head_repo=contributor/fork \
  path=internal/debugger/machtrap.s \
  exit=1 state=failure decision=failure output="verification required"

darwin_constraint_blob=$(jq -n \
  '{"0":"//go:build darwin && arm64 && bingonative\n\npackage debugger\n"}')
run_case "darwin build constraint gates convention-free Go file" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/machfix.go \
  blob_contents_json="$darwin_constraint_blob" \
  exit=1 state=failure decision=failure output="verification required"

negated_constraint_blob=$(jq -n \
  '{"0":"//go:build !linux && !windows\n\npackage debugger\n"}')
run_case "negated platform constraint is evaluated as Darwin-only" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/negated.go \
  blob_contents_json="$negated_constraint_blob" \
  exit=1 state=failure decision=failure output="verification required"

legacy_constraint_blob=$(jq -n \
  '{"0":"// +build darwin,arm64\n\npackage debugger\n"}')
run_case "legacy Darwin build constraint still gates" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/legacy.go \
  blob_contents_json="$legacy_constraint_blob" \
  exit=1 state=failure decision=failure output="verification required"

cross_platform_constraint_blob=$(jq -n \
  '{"0":"//go:build (linux && amd64) || (darwin && arm64)\n\npackage debugger\n"}')
run_case "cross-platform build constraint gates conservatively" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/crossplatform.go \
  blob_contents_json="$cross_platform_constraint_blob" \
  exit=1 state=failure decision=failure output="verification required"

large_padding=$(awk 'BEGIN {
  for (i = 0; i < 4000; i++) {
    print "// padding padding padding padding padding"
  }
}')
large_header=$(printf '//go:build darwin && arm64\n%s\npackage debugger\n' \
  "$large_padding")
large_header_file="$tmpdir/large-header.go"
printf '%s' "$large_header" > "$large_header_file"
large_constraint_blob=$(jq -n --rawfile content "$large_header_file" \
  '{"0":$content}')
run_case "large pre-package header cannot bypass build constraint detection" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/largeheader.go \
  blob_contents_json="$large_constraint_blob" \
  exit=1 state=failure decision=failure output="verification required"

decoy_constraint_blob=$(jq -n \
  '{"0":"/*\n//go:build ignore\n*/\npackage debugger\n"}')
run_case "constraint-like comment gates conservatively rather than false-green" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/decoy.go \
  blob_contents_json="$decoy_constraint_blob" \
  exit=1 state=failure decision=failure output="verification required"

run_case "existing Darwin probe constraint is gated" \
  action=opened head_repo=contributor/fork \
  path=internal/debugger/gcpreempt_probe_test.go \
  blob_contents_json="$darwin_constraint_blob" \
  exit=1 state=failure decision=failure output="verification required"

run_case "ordinary Go change without Darwin constraint stays ungated" \
  action=opened head_repo=contributor/fork \
  path=internal/hub/hub.go \
  exit=0 state=success decision=success output="No darwin-native code changed"

run_case "runtime-dispatched debugger code always gates" \
  action=opened head_repo=contributor/fork \
  path=internal/debugger/dwarf.go \
  exit=1 state=failure decision=failure output="verification required"

run_case "integration suite entry point always gates" \
  action=opened head_repo=contributor/fork \
  path=test/integration/integration_suite_test.go \
  exit=1 state=failure decision=failure output="verification required"

run_case "Darwin verification recipe always gates" \
  action=opened head_repo=contributor/fork path=justfile \
  exit=1 state=failure decision=failure output="verification required"

run_case "module graph changes always gate" \
  action=opened head_repo=contributor/fork path=go.mod \
  exit=1 state=failure decision=failure output="verification required"

run_case "one Darwin file among many still gates" \
  action=opened head_repo=contributor/fork \
  paths_json='["README.md","internal/hub/hub.go","internal/debugger/backend_darwin_arm64.go","docs/x.md"]' \
  exit=1 state=failure decision=failure output="verification required"

# --- immutable tree diff integrity -------------------------------------------

many_paths_json=$(jq -n \
  '[range(0; 150) | "docs/generated-\(.).md"] + ["entitlements.plist"]')
run_case "more than 100 changed files are evaluated immutably" \
  action=opened head_repo=contributor/fork \
  paths_json="$many_paths_json" \
  exit=1 state=failure decision=failure output="verification required"

run_case "multi-file non-Darwin tree diff publishes success" \
  action=opened head_repo=contributor/fork \
  paths_json='["a.md","b.md","c.md","d.md","e.md","f.md"]' \
  exit=0 state=success decision=success output="No darwin-native code changed"

run_case "truncated Git tree response fails closed" \
  action=opened head_repo=contributor/fork path=docs/readme.md \
  tree_truncated=true \
  exit=1 state=failure decision=failure \
  output="truncated or malformed tree"

run_case "truncated base tree cannot hide a Darwin deletion" \
  action=opened head_repo=contributor/fork \
  base_paths_json='["entitlements.plist","README.md"]' \
  paths_json='["README.md"]' base_tree_truncated=true \
  exit=1 state=failure decision=failure \
  output="truncated or malformed tree"

run_case "control character filename fails closed" \
  action=opened head_repo=contributor/fork \
  path=$'000-padding\nentitlements.plist' \
  exit=1 state=failure decision=failure output="verification required"

run_case "empty immutable diff fails closed" \
  action=opened head_repo=contributor/fork paths_json='[]' \
  exit=1 state=failure decision=failure \
  output="refusing an empty gate decision"

run_case "deleting a Darwin file still requires verification" \
  action=opened head_repo=contributor/fork \
  base_paths_json='["entitlements.plist"]' paths_json='[]' \
  exit=1 state=failure decision=failure output="verification required"

run_case "deleting a convention-free Darwin constrained file is gated" \
  action=opened head_repo=contributor/fork \
  base_paths_json='["internal/hub/machfix.go"]' paths_json='[]' \
  blob_contents_json="$darwin_constraint_blob" \
  exit=1 state=failure decision=failure output="verification required"

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
  action=closed head_repo=contributor/fork path=entitlements.plist \
  exit=2 state=none decision=none gh_missing="statuses/" \
  output="Unsupported pull_request_target action: closed"

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
  local base_sha
  local merge_base_sha
  local base_tree_sha
  local head_tree_sha

  case_count=$((case_count + 1))
  case_id=$case_count
  head_sha=$(printf '%040x' "$case_id")
  base_sha=$(printf 'f%039x' "$case_id")
  merge_base_sha=$(printf 'e%039x' "$case_id")
  base_tree_sha=$(printf 'c%039x' "$case_id")
  head_tree_sha=$(printf 'd%039x' "$case_id")
  gh_log="$tmpdir/sequence-gh-$case_id.log"
  status_log="$tmpdir/sequence-status-$case_id.log"
  first_event="$tmpdir/sequence-first-$case_id.json"
  unrelated_event="$tmpdir/sequence-unrelated-$case_id.json"
  : > "$gh_log"
  : > "$status_log"
  printf '%s\n' '{}' > "$tmpdir/blobs-sequence-$case_id.json"

  jq -n \
    --arg action "$first_action" \
    --arg event_label "$first_event_label" \
    --arg head_sha "$head_sha" \
    --arg base_sha "$base_sha" \
    '{
      action: $action,
      label: {name: $event_label},
      pull_request: {
        number: 17,
        changed_files: 1,
        base: {sha: $base_sha},
        head: {sha: $head_sha, repo: {full_name: "contributor/fork"}}
      }
    }' > "$first_event"
  jq -n \
    --arg head_sha "$head_sha" \
    --arg base_sha "$base_sha" \
    '{
      action: "labeled",
      label: {name: "triage"},
      pull_request: {
        number: 17,
        changed_files: 1,
        base: {sha: $base_sha},
        head: {sha: $head_sha, repo: {full_name: "contributor/fork"}}
      }
    }' > "$unrelated_event"

  PATH="$tmpdir/bin:$PATH" \
    GITHUB_EVENT_PATH="$first_event" \
    GITHUB_REPOSITORY=bingosuite/bingo \
    MOCK_GH_LOG="$gh_log" \
    MOCK_STATUS_LOG="$status_log" \
    MOCK_CHANGED_PATHS_JSON='["entitlements.plist"]' \
    MOCK_BASE_PATHS_JSON='[]' \
    MOCK_BASE_TREE_TRUNCATED=false \
    MOCK_TREE_TRUNCATED=false \
    MOCK_MERGE_BASE_SHA="$merge_base_sha" \
    MOCK_BASE_TREE_SHA="$base_tree_sha" \
    MOCK_HEAD_TREE_SHA="$head_tree_sha" \
    MOCK_HEAD_SHA="$head_sha" \
    MOCK_BLOB_CONTENTS_FILE="$tmpdir/blobs-sequence-$case_id.json" \
    MOCK_PRIOR_STATUS_STATE=failure \
    MOCK_PRIOR_STATUS_CREATOR='github-actions[bot]' \
    MOCK_PRIOR_STATUS_TARGET='https://github.com/bingosuite/bingo/actions/runs/122' \
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
    MOCK_BASE_PATHS_JSON='[]' \
    MOCK_BASE_TREE_TRUNCATED=false \
    MOCK_TREE_TRUNCATED=false \
    MOCK_MERGE_BASE_SHA="$merge_base_sha" \
    MOCK_BASE_TREE_SHA="$base_tree_sha" \
    MOCK_HEAD_TREE_SHA="$head_tree_sha" \
    MOCK_HEAD_SHA="$head_sha" \
    MOCK_BLOB_CONTENTS_FILE="$tmpdir/blobs-sequence-$case_id.json" \
    MOCK_PRIOR_STATUS_STATE=failure \
    MOCK_PRIOR_STATUS_CREATOR='github-actions[bot]' \
    MOCK_PRIOR_STATUS_TARGET='https://github.com/bingosuite/bingo/actions/runs/122' \
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

# The privileged job must never materialise a working tree, and the only code it
# runs must be read from the base SHA, which a pull request cannot influence.
# GitHub Actions '${{ }}' expressions are matched literally, not expanded.
# shellcheck disable=SC2016
trusted_runs_base_policy_only() {
  ! grep -Eq 'uses:[[:space:]]*actions/checkout' "$trusted_workflow" &&
    grep -Fq 'BASE_SHA: ${{ github.event.pull_request.base.sha }}' \
      "$trusted_workflow" &&
    grep -Fq 'contents/.github/scripts/darwin-verification-gate.sh?ref=$BASE_SHA' \
      "$trusted_workflow" &&
    ! grep -Fq 'github.event.pull_request.head.sha }}' "$trusted_workflow" &&
    ! grep -Fq 'github.event.pull_request.head.ref' "$trusted_workflow"
}

trusted_subscribes_to_gated_actions() {
  grep -Fq 'types: [opened, synchronize, reopened, edited, labeled, unlabeled]' \
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
    grep -Fq "github.event.label.name != 'darwin-e2e-verified'" "$trusted_workflow" &&
    grep -Fq "github.event.action == 'edited' && github.event.changes.base == null" \
      "$trusted_workflow"
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

  # Backticks, process substitution and explicit subshells inherit the ERR trap
  # exactly like '$( )' does, but carry no '$(' for the scan above to catch.
  # Ban them outright so the guarded form stays the only way to spawn a subshell.
  offenders=$(printf '%s\n' "$body" | grep -E '`|<\(|>\(' || true)
  if [ -n "$offenders" ]; then
    printf 'subshell form that cannot be guarded:\n%s\n' "$offenders" >&2
    return 1
  fi
}

gate_uses_immutable_diff() {
  grep -Fq 'compare/$base_sha...$head_sha' "$gate" &&
    grep -Fq 'git/trees/$merge_base_tree_sha?recursive=1' "$gate" &&
    grep -Fq 'git/trees/$head_tree_sha?recursive=1' "$gate" &&
    ! grep -Fq '/pulls/$pr_number/files' "$gate"
}

gate_workflows_are_valid_yaml() {
  python3 -c 'import sys, yaml
for path in sys.argv[1:]:
    with open(path) as handle:
        yaml.safe_load(handle)' "$trusted_workflow" "$head_test_workflow"
}

assert_workflow "trusted workflow is base-controlled pull_request_target only" \
  trusted_is_base_controlled
assert_workflow "trusted workflow runs only base-SHA policy and never checks out" \
  trusted_runs_base_policy_only
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
assert_workflow "gate diff is bound to immutable event SHAs" \
  gate_uses_immutable_diff

if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' 2>/dev/null; then
  assert_workflow "gate workflows are valid YAML" gate_workflows_are_valid_yaml
fi

printf '1..%s\n' "$case_count"
