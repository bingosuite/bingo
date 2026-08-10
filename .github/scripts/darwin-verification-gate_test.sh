#!/usr/bin/env bash
# Adversarial tests for the trusted Darwin verification gate policy.
#
# Every case runs the real policy script against a mocked `gh` CLI and a
# synthetic `pull_request_target` event, then asserts the exact statuses the
# gate published, the decision marker it recorded, and which API calls it did
# (and did not) make. The suite must pass under Bash 3.2 (macOS) and Bash 5.

set -uo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../.." && pwd)
gate="$script_dir/darwin-verification-gate.sh"
workflow="$repo_root/.github/workflows/darwin-verification-gate.yml"
policy_test_workflow="$repo_root/.github/workflows/darwin-verification-policy-test.yml"

tmpdir=$(mktemp -d)

# Injected into every attacker-controlled field of the synthetic event.
# A `gh` invocation carrying it means the gate consumed PR-authored text.
untrusted_token='PRTEXTPOISON9f3a'
trap 'rm -rf "$tmpdir"' EXIT

failures=0
cases=0

fail() {
  failures=$((failures + 1))
  printf 'FAIL: %s\n' "$1" >&2
}

pass() {
  printf 'ok   %s\n' "$1"
}

# ---------------------------------------------------------------------------
# Mock `gh`
# ---------------------------------------------------------------------------
# The mock answers every endpoint the gate uses. Anything else is a hard error,
# so a policy change that reaches for a new API surface fails the suite instead
# of silently succeeding. Reading previously published commit statuses is
# explicitly poisoned: that data is forgeable by any same-repository workflow,
# so the gate must never consult it.

mock_bin="$tmpdir/bin"
mkdir -p "$mock_bin"

cat > "$mock_bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -uo pipefail

printf '%s\n' "$*" >> "$MOCK_GH_LOG"

# `gh api --jq <expr>` filters the response client-side; the gate relies on it,
# so the mock has to honour it rather than returning raw JSON.
mock_jq_filter=''
mock_prev=''
for mock_arg in "$@"; do
  if [ "$mock_prev" = "--jq" ]; then
    mock_jq_filter=$mock_arg
  fi
  mock_prev=$mock_arg
done

emit() {
  if [ -n "$mock_jq_filter" ]; then
    jq -r "$mock_jq_filter"
  else
    cat
  fi
}

if [ "${1:-}" = "pr" ] && [ "${2:-}" = "edit" ]; then
  if [ "${MOCK_LABEL_REMOVE_STATUS:-0}" != "0" ]; then
    echo "mock: label removal denied" >&2
    exit 1
  fi
  exit 0
fi

if [ "${1:-}" != "api" ]; then
  echo "mock: unsupported gh invocation: $*" >&2
  exit 90
fi

case "$*" in
  *"/commits/"*"/statuses"*)
    echo "mock: the gate must never read previously published statuses" >&2
    exit 91
    ;;
esac

case "$*" in
  *--method\ POST*"/statuses/"*)
    state=""
    context=""
    description=""
    target=""
    prev=""
    for arg in "$@"; do
      case "$prev" in
        state=*) ;;
      esac
      case "$arg" in
        state=*) state=${arg#state=} ;;
        context=*) context=${arg#context=} ;;
        description=*) description=${arg#description=} ;;
        target_url=*) target=${arg#target_url=} ;;
      esac
      prev=$arg
    done
    if [ "${MOCK_STATUS_POST_STATUS:-0}" != "0" ]; then
      echo "mock: status POST denied" >&2
      exit 1
    fi
    printf '%s\n' "$state" >> "$MOCK_STATE_LOG"
    printf '%s|%s|%s\n' "$context" "$state" "$description" >> "$MOCK_STATUS_LOG"
    printf '%s\n' "$target" >> "$MOCK_TARGET_LOG"
    exit 0
    ;;
  *"/collaborators/"*"/permission"*)
    if [ "${MOCK_PERMISSION_STATUS:-0}" != "0" ]; then
      echo "mock: permission lookup failed" >&2
      exit 1
    fi
    printf '%s\n' "${MOCK_PERMISSION_JSON:-{\}}" | emit
    exit 0
    ;;
  *"/pulls/"*/*)
    echo "mock: mutable pull request sub-resource is banned: $*" >&2
    exit 92
    ;;
  *"/pulls/"*)
    if [ "${MOCK_LIVE_PR_STATUS:-0}" != "0" ]; then
      echo "mock: live pull request lookup failed" >&2
      exit 1
    fi
    printf '%s\n' "${MOCK_LIVE_PR_JSON:-{\}}" | emit
    exit 0
    ;;
  *"/issues/"*"/labels"*)
    if [ "${MOCK_LABEL_STATUS:-0}" != "0" ]; then
      echo "mock: label lookup failed" >&2
      exit 1
    fi
    printf '%s\n' "${MOCK_LABELS_JSON:-[]}" | emit
    exit 0
    ;;
  *"/compare/"*)
    if [ "${MOCK_COMPARE_STATUS:-0}" != "0" ]; then
      echo "mock: compare failed" >&2
      exit 1
    fi
    printf '%s\n' "${MOCK_COMPARE_JSON}" | emit
    exit 0
    ;;
  *"/git/commits/"*)
    all=$*
    ref=${all##*/git/commits/}
    ref=${ref%% *}
    if [ "$ref" = "$MOCK_MERGE_BASE_SHA" ]; then
      tree=$MOCK_BASE_TREE_SHA
    elif [ "$ref" = "$MOCK_HEAD_SHA" ]; then
      tree=$MOCK_HEAD_TREE_SHA
    else
      echo "mock: unexpected commit ref $ref" >&2
      exit 92
    fi
    jq -n --arg tree "$tree" '{tree: {sha: $tree}}' | emit
    exit 0
    ;;
  *"/git/trees/"*)
    all=$*
    ref=${all##*/git/trees/}
    ref=${ref%%\?*}
    ref=${ref%% *}
    if [ "$ref" = "$MOCK_BASE_TREE_SHA" ]; then
      entries=$MOCK_BASE_TREE_ENTRIES
      truncated=$MOCK_BASE_TREE_TRUNCATED
      status=${MOCK_BASE_TREE_STATUS:-0}
    elif [ "$ref" = "$MOCK_HEAD_TREE_SHA" ]; then
      entries=$MOCK_HEAD_TREE_ENTRIES
      truncated=$MOCK_HEAD_TREE_TRUNCATED
      status=${MOCK_HEAD_TREE_STATUS:-0}
    else
      echo "mock: unexpected tree ref $ref" >&2
      exit 93
    fi
    if [ "$status" != "0" ]; then
      echo "mock: tree read failed" >&2
      exit 1
    fi
    # Entries are either bare path strings (regular blobs with a synthetic
    # 40-hex SHA derived from their index) or objects that override any of
    # path/mode/type/sha, plus `drop_sha` to omit the field entirely.
    printf '%s' "$entries" | jq \
      --argjson truncated "$truncated" \
      '{
         truncated: $truncated,
         tree: (to_entries | map(
           (.key | tostring) as $i
           | (("0000000000000000000000000000000000000000" + $i) | .[-40:]) as $auto
           | (if (.value | type) == "string" then {path: .value} else .value end) as $spec
           | {
               path: $spec.path,
               mode: ($spec.mode // "100644"),
               type: ($spec.type // "blob"),
               sha: ($spec.sha // $auto)
             }
           | if ($spec.drop_sha // false) then del(.sha) else . end
         ))
       }' | emit
    exit 0
    ;;
  *"/git/blobs/"*)
    all=$*
    sha=${all##*/git/blobs/}
    sha=${sha%% *}
    alt=$(printf '%s' "$sha" | sed 's/^0*//')
    [ -n "$alt" ] || alt=0
    if [ "${MOCK_BLOB_STATUS:-0}" != "0" ]; then
      echo "mock: blob read failed" >&2
      exit 1
    fi
    b64=$(jq -r --arg sha "$sha" --arg alt "$alt" \
      '(.[$sha] // .[$alt] // "")' "$MOCK_BLOB_B64_FILE")
    if [ -z "$b64" ]; then
      content=$(jq -r --arg sha "$sha" --arg alt "$alt" \
        '(.[$sha] // .[$alt] // "package bingo\n")' "$MOCK_BLOB_CONTENTS_FILE")
      b64=$(printf '%s' "$content" | base64 | tr -d '\n')
    fi
    jq -n \
      --arg encoding "${MOCK_BLOB_ENCODING:-base64}" \
      --arg content "$b64" \
      '{encoding: $encoding, content: $content}' | emit
    exit 0
    ;;
esac

echo "mock: unexpected gh api call: $*" >&2
exit 94
MOCK
chmod +x "$mock_bin/gh"

# ---------------------------------------------------------------------------
# Case harness
# ---------------------------------------------------------------------------

case_id=0

# run_case <name> [key=value ...]
run_case() {
  local name=$1
  shift
  cases=$((cases + 1))
  case_id=$((case_id + 1))

  local action=synchronize
  local label_name=darwin-e2e-verified
  local has_label=false
  local labels_json=""
  local label_status=0
  local label_remove_status=0
  local status_post_status=0
  local compare_status=0
  local base_tree_status=0
  local head_tree_status=0
  local blob_status=0
  local blob_encoding=base64
  local base_tree_truncated=false
  local head_tree_truncated=false
  # Default fixture: one Darwin-sensitive addition, so a case only has to
  # describe the behaviour it is actually probing.
  local base_entries='[]'
  local head_entries='["internal/debugger/engine.go"]'
  local blob_contents='{}'
  local blob_b64='{}'
  local edited_changes='{}'
  local base_ref=main
  local actor=maintainer-jo
  local actor_type=User
  local perm_status=0
  local perm_permission=admin
  local perm_role=admin
  local perm_login=''
  local live_status=0
  local live_state=open
  local live_base_ref=main
  local live_base_sha=same
  local live_head_sha=same
  local live_head_repo=same
  local head_repo=contributor/fork

  local expect_exit=0
  local expect_states=''
  local expect_decision=''
  local expect_output=''
  local expect_missing_output=''
  local expect_description=''
  local gh_expect=''
  local gh_missing=''
  local expect_label_query=''
  local pr_number=17
  local head_tree_sha_override=''

  local kv key value
  for kv in "$@"; do
    key=${kv%%=*}
    value=${kv#*=}
    case "$key" in
      action) action=$value ;;
      label_name) label_name=$value ;;
      has_label) has_label=$value ;;
      labels_json) labels_json=$value ;;
      label_status) label_status=$value ;;
      label_remove_status) label_remove_status=$value ;;
      status_post_status) status_post_status=$value ;;
      compare_status) compare_status=$value ;;
      base_tree_status) base_tree_status=$value ;;
      head_tree_status) head_tree_status=$value ;;
      blob_status) blob_status=$value ;;
      blob_encoding) blob_encoding=$value ;;
      base_tree_truncated) base_tree_truncated=$value ;;
      head_tree_truncated) head_tree_truncated=$value ;;
      base_entries) base_entries=$value ;;
      head_entries) head_entries=$value ;;
      blob_contents) blob_contents=$value ;;
      blob_b64) blob_b64=$value ;;
      edited_changes) edited_changes=$value ;;
      base_ref) base_ref=$value ;;
      actor) actor=$value ;;
      actor_type) actor_type=$value ;;
      perm_status) perm_status=$value ;;
      perm_permission) perm_permission=$value ;;
      perm_role) perm_role=$value ;;
      perm_login) perm_login=$value ;;
      live_status) live_status=$value ;;
      live_state) live_state=$value ;;
      live_base_ref) live_base_ref=$value ;;
      live_base_sha) live_base_sha=$value ;;
      live_head_sha) live_head_sha=$value ;;
      live_head_repo) live_head_repo=$value ;;
      head_repo) head_repo=$value ;;
      expect_exit) expect_exit=$value ;;
      expect_states) expect_states=$value ;;
      expect_decision) expect_decision=$value ;;
      expect_output) expect_output=$value ;;
      expect_missing_output) expect_missing_output=$value ;;
      expect_description) expect_description=$value ;;
      gh_expect) gh_expect=$value ;;
      gh_missing) gh_missing=$value ;;
      expect_label_query) expect_label_query=$value ;;
      pr_number) pr_number=$value ;;
      head_tree_sha_override) head_tree_sha_override=$value ;;
      *)
        fail "$name: unknown harness key '$key'"
        return
        ;;
    esac
  done

  local work="$tmpdir/case-$case_id"
  mkdir -p "$work"
  printf '%s\n' "$name" > "$work/name"

  local head_sha base_sha merge_base_sha
  head_sha=$(printf 'ad%038x' "$case_id")
  base_sha=$(printf 'be%038x' "$case_id")
  merge_base_sha=$(printf 'cc%038x' "$case_id")

  local other_sha
  other_sha=$(printf 'fe%038x' "$case_id")

  local live_base_sha_value=$base_sha
  [ "$live_base_sha" = "other" ] && live_base_sha_value=$other_sha
  [ "$live_base_sha" != "same" ] && [ "$live_base_sha" != "other" ] && live_base_sha_value=$live_base_sha

  local live_head_sha_value=$head_sha
  [ "$live_head_sha" = "other" ] && live_head_sha_value=$other_sha
  [ "$live_head_sha" != "same" ] && [ "$live_head_sha" != "other" ] && live_head_sha_value=$live_head_sha

  local live_head_repo_value=$head_repo
  [ "$live_head_repo" != "same" ] && live_head_repo_value=$live_head_repo

  local perm_login_value=$actor
  [ -n "$perm_login" ] && perm_login_value=$perm_login

  if [ -z "$labels_json" ]; then
    if [ "$has_label" = "true" ]; then
      labels_json='[{"name":"darwin-e2e-verified"}]'
    else
      labels_json='[{"name":"needs-review"}]'
    fi
  fi

  local event="$work/event.json"
  jq -n \
    --arg head "$head_sha" \
    --arg base "$base_sha" \
    --arg base_ref "$base_ref" \
    --arg action "$action" \
    --arg label "$label_name" \
    --arg head_repo "$head_repo" \
    --arg actor "$actor" \
    --arg actor_type "$actor_type" \
    --argjson changes "$edited_changes" \
    --arg poison "$untrusted_token" \
    --argjson pr_number "$pr_number" \
    '{
      action: $action,
      sender: {login: $actor, type: $actor_type},
      label: {name: $label},
      changes: $changes,
      pull_request: {
        number: $pr_number,
        state: "open",
        title: ($poison + "-title"),
        body: ($poison + "-body"),
        head: {
          sha: $head,
          ref: ($poison + "-branch"),
          label: ($poison + "-label"),
          repo: {full_name: $head_repo}
        },
        base: {sha: $base, ref: $base_ref}
      }
    }' > "$event"

  local compare_json
  compare_json=$(jq -n --arg mb "$merge_base_sha" '{merge_base_commit: {sha: $mb}}')

  local live_pr_json
  live_pr_json=$(jq -n \
    --arg state "$live_state" \
    --arg base_ref "$live_base_ref" \
    --arg base_sha "$live_base_sha_value" \
    --arg head_sha "$live_head_sha_value" \
    --arg head_repo "$live_head_repo_value" \
    '{
      state: $state,
      base: {ref: $base_ref, sha: $base_sha},
      head: {sha: $head_sha, repo: {full_name: $head_repo}}
    }')

  local permission_json
  permission_json=$(jq -n \
    --arg permission "$perm_permission" \
    --arg role "$perm_role" \
    --arg login "$perm_login_value" \
    '{permission: $permission, role_name: $role, user: {login: $login}}')

  printf '%s' "$blob_contents" > "$work/blobs.json"
  printf '%s' "$blob_b64" > "$work/blobs-b64.json"

  local out="$work/output.txt"
  local decision="$work/decision"

  (
    PATH="$mock_bin:$PATH" \
    GITHUB_EVENT_PATH="$event" \
    GITHUB_REPOSITORY=bingosuite/bingo \
    GITHUB_SERVER_URL=https://github.com \
    GITHUB_RUN_ID=4242 \
    DARWIN_GATE_DECISION_FILE="$decision" \
    MOCK_GH_LOG="$work/gh.log" \
    MOCK_STATE_LOG="$work/states.log" \
    MOCK_STATUS_LOG="$work/statuses.log" \
    MOCK_TARGET_LOG="$work/targets.log" \
    MOCK_LABELS_JSON="$labels_json" \
    MOCK_LABEL_STATUS="$label_status" \
    MOCK_LABEL_REMOVE_STATUS="$label_remove_status" \
    MOCK_STATUS_POST_STATUS="$status_post_status" \
    MOCK_COMPARE_JSON="$compare_json" \
    MOCK_COMPARE_STATUS="$compare_status" \
    MOCK_MERGE_BASE_SHA="$merge_base_sha" \
    MOCK_HEAD_SHA="$head_sha" \
    MOCK_BASE_TREE_SHA=$(printf 'da%038x' $((case_id * 2))) \
    MOCK_HEAD_TREE_SHA="${head_tree_sha_override:-$(printf 'da%038x' $((case_id * 2 + 1)))}" \
    MOCK_BASE_TREE_ENTRIES="$base_entries" \
    MOCK_HEAD_TREE_ENTRIES="$head_entries" \
    MOCK_BASE_TREE_TRUNCATED="$base_tree_truncated" \
    MOCK_HEAD_TREE_TRUNCATED="$head_tree_truncated" \
    MOCK_BASE_TREE_STATUS="$base_tree_status" \
    MOCK_HEAD_TREE_STATUS="$head_tree_status" \
    MOCK_BLOB_CONTENTS_FILE="$work/blobs.json" \
    MOCK_BLOB_B64_FILE="$work/blobs-b64.json" \
    MOCK_BLOB_STATUS="$blob_status" \
    MOCK_BLOB_ENCODING="$blob_encoding" \
    MOCK_LIVE_PR_JSON="$live_pr_json" \
    MOCK_LIVE_PR_STATUS="$live_status" \
    MOCK_PERMISSION_JSON="$permission_json" \
    MOCK_PERMISSION_STATUS="$perm_status" \
    bash "$gate"
  ) > "$out" 2>&1
  local actual_exit=$?

  local ok=1

  if [ "$actual_exit" != "$expect_exit" ]; then
    fail "$name: expected exit $expect_exit, got $actual_exit"
    ok=0
  fi

  local actual_states=''
  if [ -f "$work/states.log" ]; then
    actual_states=$(paste -sd, - < "$work/states.log")
  fi
  if [ "$actual_states" != "$expect_states" ]; then
    fail "$name: expected states '$expect_states', got '$actual_states'"
    ok=0
  fi

  local actual_decision=''
  [ -f "$decision" ] && actual_decision=$(cat "$decision")
  if [ "$actual_decision" != "$expect_decision" ]; then
    fail "$name: expected decision '$expect_decision', got '$actual_decision'"
    ok=0
  fi

  if [ -n "$expect_output" ] && ! grep -Fq "$expect_output" "$out"; then
    fail "$name: expected output to contain '$expect_output'"
    ok=0
  fi

  if [ -n "$expect_missing_output" ] && grep -Fq "$expect_missing_output" "$out"; then
    fail "$name: output unexpectedly contains '$expect_missing_output'"
    ok=0
  fi

  touch "$work/statuses.log"
  if [ -n "$expect_description" ] &&
    ! grep -Fq "$expect_description" "$work/statuses.log"; then
    fail "$name: expected a status description containing '$expect_description'"
    ok=0
  fi

  touch "$work/gh.log"

  if [ -n "$gh_expect" ] && ! grep -Fq "$gh_expect" "$work/gh.log"; then
    fail "$name: expected gh call containing '$gh_expect'"
    ok=0
  fi

  if [ -n "$gh_missing" ] && grep -Fq "$gh_missing" "$work/gh.log"; then
    fail "$name: gh log unexpectedly contains '$gh_missing'"
    ok=0
  fi

  if [ -n "$expect_label_query" ]; then
    if grep -Fq "/issues/17/labels" "$work/gh.log"; then
      if [ "$expect_label_query" != "true" ]; then
        fail "$name: gate queried live labels but should not have"
        ok=0
      fi
    else
      if [ "$expect_label_query" = "true" ]; then
        fail "$name: gate did not query live labels"
        ok=0
      fi
    fi
  fi

  # Universal invariants, asserted for every single case.
  if grep -Eq '/commits/[0-9a-fA-F]+/statuses' "$work/gh.log"; then
    fail "$name: gate read forgeable prior commit statuses"
    ok=0
  fi

  # No `gh` invocation may carry pull-request-authored text. Titles, branch
  # names and bodies are attacker-controlled; only fixed paths, the numeric PR
  # id and hex SHAs may ever reach the API surface.
  if grep -Eq '(^| )(-f|-F|--jq|--field)?[^ ]*(<script|; *rm |\$\(|`)' "$work/gh.log"; then
    fail "$name: gate passed shell metacharacters to gh"
    ok=0
  fi
  # Every synthetic event carries this token in its title, body, head ref and
  # head label — all attacker-controlled. None may ever reach the API surface.
  if grep -Fq "$untrusted_token" "$work/gh.log"; then
    fail "$name: gate consumed untrusted pull request content"
    ok=0
  fi

  if [ -f "$work/targets.log" ] && [ -s "$work/targets.log" ]; then
    if grep -vq '^https://github.com/bingosuite/bingo/actions/runs/4242$' "$work/targets.log"; then
      fail "$name: status target_url did not point at this run"
      ok=0
    fi
  fi

  if [ -f "$work/statuses.log" ] && [ -s "$work/statuses.log" ]; then
    if grep -vq '^Darwin E2E verified|' "$work/statuses.log"; then
      fail "$name: gate published an unexpected status context"
      ok=0
    fi
  fi

  [ "$ok" = "1" ] && pass "$name"
}

# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

darwin_source='//go:build darwin && arm64

package debugger
'
plain_source='package hub

func Noop() {}
'
# Pre-1.17 syntax. `go build` still honors it, so the scanner must too.
legacy_source='// +build darwin

package debugger
'

# A cgo preamble makes a file platform-dependent with no explicit constraint.
cgo_source='package debugger

/*
#cgo darwin LDFLAGS: -framework CoreFoundation
*/
import "C"
'

# A comment that merely mentions cgo must not gate.
cgo_mention_source='package hub

// The debugger uses #cgo directives on darwin, but this file does not.
func Noop() {}
'

# A header long enough that a naive fixed-size prefix read would miss the
# constraint that follows it.
large_padding=''
i=0
while [ "$i" -lt 400 ]; do
  large_padding="$large_padding// filler line to push the build constraint far down the file
"
  i=$((i + 1))
done

bom=$(printf '\357\273\277')
bom_constraint_b64=$(printf '%s%s' "$bom" "$darwin_source" | base64 | tr -d '\n')
bom_plain_b64=$(printf '%s%s' "$bom" "$plain_source" | base64 | tr -d '\n')
bom_large_b64=$(printf '%s%s%s%s' "$bom" "$large_padding" "$darwin_source" "$plain_source" | base64 | tr -d '\n')
utf16_b64=$(printf '\377\376/\000/\000g\000o\000\072\000b\000u\000i\000l\000d\000 \000d\000a\000r\000w\000i\000n\000' | base64 | tr -d '\n')

# ---------------------------------------------------------------------------
# Scope: what the gate ignores
# ---------------------------------------------------------------------------

run_case "unrelated label events publish nothing" \
  action=labeled label_name=needs-review \
  expect_exit=0 expect_states='' expect_decision=ignored \
  expect_output="Ignoring unrelated 'needs-review' label event" \
  expect_label_query=false gh_missing='/statuses/'

run_case "unrelated unlabel events publish nothing" \
  action=unlabeled label_name=needs-review \
  expect_exit=0 expect_states='' expect_decision=ignored \
  expect_label_query=false gh_missing='/statuses/'

run_case "non-base pull request edits publish nothing" \
  action=edited edited_changes='{"title":{"from":"old"}}' \
  expect_exit=0 expect_states='' expect_decision=ignored \
  expect_output='did not change the base' \
  gh_missing='/statuses/'

run_case "unsupported event actions publish nothing" \
  action=assigned \
  expect_exit=2 expect_states='' expect_decision='' \
  gh_missing='/statuses/'

# ---------------------------------------------------------------------------
# Blocker 1 + 5: trigger scope, base binding, SHA-global statuses
# ---------------------------------------------------------------------------

# Both of these values reach a URL path. Neither is PR-authored, but a
# malformed one must be refused rather than pasted into a request.
# jq -e exits 4 when the filter selects nothing; any non-zero exit before a
# decision is recorded makes the workflow's always() fallback fail the head.
run_case "a non-numeric pull request number is refused before any API call" \
  pr_number='"17/../../evil"' \
  expect_exit=4 expect_states='' expect_decision='' \
  gh_missing='api'

run_case "a malformed tree SHA is refused before the tree is fetched" \
  head_entries='["internal/hub/hub.go"]' \
  head_tree_sha_override='not-a-sha' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='invalid tree SHA' \
  gh_missing='/git/trees/'

run_case "a pull request targeting another base is refused outright" \
  base_ref=release-1.x \
  head_entries='["internal/hub/hub.go"]' \
  expect_exit=1 expect_states='failure' expect_decision=failure \
  expect_output='only governs pull requests targeting' \
  gh_missing='/compare/'

run_case "a verified label on an alternate base cannot publish success" \
  action=labeled has_label=true base_ref=attacker-base \
  expect_exit=1 expect_states='failure' expect_decision=failure \
  expect_output='only governs pull requests targeting' \
  expect_label_query=false

run_case "retargeting the base during the run blocks success" \
  head_entries='["internal/hub/hub.go"]' \
  live_base_ref=release-1.x \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='moved while the Darwin gate was evaluating'

run_case "base branch movement during the run blocks success" \
  head_entries='["internal/hub/hub.go"]' \
  live_base_sha=other \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='moved while the Darwin gate was evaluating'

run_case "a force-pushed head blocks a stale success" \
  head_entries='["internal/hub/hub.go"]' \
  live_head_sha=other \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='moved while the Darwin gate was evaluating'

run_case "a closed pull request cannot receive a fresh success" \
  head_entries='["internal/hub/hub.go"]' \
  live_state=closed \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='moved while the Darwin gate was evaluating'

run_case "swapping the head repository blocks success" \
  head_entries='["internal/hub/hub.go"]' \
  live_head_repo=attacker/fork \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='moved while the Darwin gate was evaluating'

run_case "an unreadable live pull request fails closed" \
  head_entries='["internal/hub/hub.go"]' \
  live_status=1 \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='could not evaluate this head'

run_case "a delayed verified label cannot green a newer head" \
  action=labeled has_label=true live_head_sha=other \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='moved while the Darwin gate was evaluating'

run_case "a delayed verified label cannot green a retargeted pull request" \
  action=labeled has_label=true live_base_ref=release-1.x \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='moved while the Darwin gate was evaluating'

# ---------------------------------------------------------------------------
# Darwin scope detection
# ---------------------------------------------------------------------------

run_case "pure documentation changes pass without verification" \
  head_entries='["README.md","docs/ErrorHandling.md"]' \
  expect_exit=0 expect_states='pending,success' expect_decision=success \
  expect_output='No darwin-native code changed'

run_case "debugger changes require verification" \
  head_entries='["internal/debugger/engine.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='Darwin-native (CI-unexecutable) changes detected'

run_case "the verified label publishes success to the current head" \
  action=labeled has_label=true \
  expect_exit=0 expect_states='pending,success' expect_decision=success \
  expect_output='label added by maintainer-jo' \
  expect_label_query=true

run_case "integration test changes require verification" \
  head_entries='["test/integration/debugger_e2e_common_test.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "justfile changes require verification" \
  head_entries='["justfile"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "entitlements changes require verification" \
  head_entries='["entitlements.plist"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "module graph changes require verification" \
  head_entries='["go.mod"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "darwin file suffixes anywhere require verification" \
  head_entries='["cmd/wsmon/render_darwin.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "arm64 file suffixes anywhere require verification" \
  head_entries='["pkg/client/tuning_arm64.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "native sources require verification" \
  head_entries='["internal/somewhere/shim.c"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

# `go/build` compiles far more than this repository currently checks in, and
# only `.go` blobs are content-scanned. An extension Go accepts but the selector
# misses would ship darwin-only machine code past the gate: `shim_darwin.sx`
# carries an implicit darwin constraint and is never read by the blob scan.
for ext in sx S s f F for f90 swig swigcxx syso cc cpp cxx hh hpp hxx h m mm; do
  run_case "a .$ext native source requires verification" \
    head_entries="[\"internal/somewhere/shim.$ext\"]" \
    expect_exit=1 expect_states='pending,failure' expect_decision=failure

  run_case "a darwin-suffixed .$ext source requires verification" \
    head_entries="[\"cmd/wsmon/glue_darwin.$ext\"]" \
    expect_exit=1 expect_states='pending,failure' expect_decision=failure

  run_case "deleting a .$ext native source still requires verification" \
    base_entries="[\"internal/somewhere/shim.$ext\"]" head_entries='[]' \
    expect_exit=1 expect_states='pending,failure' expect_decision=failure
done

# The extension list must stay a suffix allow-list, not a substring match.
run_case "an extension that merely starts like a native one does not gate" \
  head_entries='["docs/notes.form"]' \
  expect_exit=0 expect_states='pending,success' expect_decision=success

run_case "a Go-adjacent extension does not gate" \
  head_entries='["testdata/fixture.golden"]' \
  expect_exit=0 expect_states='pending,success' expect_decision=success

run_case "a build constraint gates a convention-free Go file" \
  head_entries='["internal/hub/machfix.go"]' \
  blob_contents="$(jq -n --arg src "$darwin_source" '{"0":$src}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='internal/hub/machfix.go'

run_case "deleting a constrained Go file still gates" \
  base_entries='["internal/hub/machfix.go"]' head_entries='[]' \
  blob_contents="$(jq -n --arg src "$darwin_source" '{"0":$src}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "a legacy // +build constraint gates" \
  head_entries='["internal/hub/legacy.go"]' \
  blob_contents="$(jq -n --arg src "$legacy_source" '{"0":$src}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='internal/hub/legacy.go'

# A cgo preamble is platform-dependent with no explicit build constraint, so a
# `#cgo darwin` directive must gate on its own.
run_case "a cgo preamble gates a Go file with no build constraint" \
  head_entries='["internal/hub/bridge.go"]' \
  blob_contents="$(jq -n --arg src "$cgo_source" '{"0":$src}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='internal/hub/bridge.go'

run_case "deleting a cgo file still gates" \
  base_entries='["internal/hub/bridge.go"]' head_entries='[]' \
  blob_contents="$(jq -n --arg src "$cgo_source" '{"0":$src}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "merely mentioning cgo in prose does not gate" \
  head_entries='["internal/hub/notes.go"]' \
  blob_contents="$(jq -n --arg src "$cgo_mention_source" '{"0":$src}')" \
  expect_exit=0 expect_states='pending,success' expect_decision=success

run_case "an unconstrained Go file does not gate" \
  head_entries='["internal/hub/plain.go"]' \
  blob_contents="$(jq -n --arg src "$plain_source" '{"0":$src}')" \
  expect_exit=0 expect_states='pending,success' expect_decision=success

run_case "control characters in a path gate conservatively" \
  head_entries='["internal/hub/we\ni\rrd.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

# ---------------------------------------------------------------------------
# Blocker 2: UTF-8 byte-order marks
# ---------------------------------------------------------------------------

run_case "a UTF-8 BOM cannot hide a Darwin build constraint" \
  head_entries='["internal/hub/bomfix.go"]' \
  blob_b64="$(jq -n --arg b64 "$bom_constraint_b64" '{"0":$b64}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='internal/hub/bomfix.go'

run_case "deleting a BOM-prefixed constrained file still gates" \
  base_entries='["internal/hub/bomfix.go"]' head_entries='[]' \
  blob_b64="$(jq -n --arg b64 "$bom_constraint_b64" '{"0":$b64}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "a BOM behind a very large header still gates" \
  head_entries='["internal/hub/bomlarge.go"]' \
  blob_b64="$(jq -n --arg b64 "$bom_large_b64" '{"0":$b64}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

run_case "a UTF-8 BOM without a constraint does not gate" \
  head_entries='["internal/hub/bomplain.go"]' \
  blob_b64="$(jq -n --arg b64 "$bom_plain_b64" '{"0":$b64}')" \
  expect_exit=0 expect_states='pending,success' expect_decision=success

run_case "a non-UTF-8 byte-order mark gates conservatively" \
  head_entries='["internal/hub/utf16.go"]' \
  blob_b64="$(jq -n --arg b64 "$utf16_b64" '{"0":$b64}')" \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='non-UTF-8 byte-order mark'

run_case "an undecodable blob response fails closed" \
  head_entries='["internal/hub/plain.go"]' \
  blob_encoding=none \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='could not evaluate this head'

run_case "an unreadable blob fails closed" \
  head_entries='["internal/hub/plain.go"]' \
  blob_status=1 \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='could not evaluate this head'

# ---------------------------------------------------------------------------
# Blocker 3: symlinks, gitlinks and other non-regular tree entries
# ---------------------------------------------------------------------------

run_case "a changed Go symlink is gated without trusting its link text" \
  head_entries='[{"path":"internal/hub/link.go","mode":"120000","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='internal/hub/link.go' \
  gh_missing='/git/blobs/'

run_case "deleting a Go symlink is gated" \
  base_entries='[{"path":"internal/hub/link.go","mode":"120000","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]' \
  head_entries='[]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  gh_missing='/git/blobs/'

run_case "a submodule pointer change is gated" \
  head_entries='[{"path":"vendor/thirdparty","mode":"160000","type":"commit","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='vendor/thirdparty'

run_case "an unexpected file mode is gated" \
  head_entries='[{"path":"internal/hub/odd.go","mode":"100664","sha":"cccccccccccccccccccccccccccccccccccccccc"}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  gh_missing='/git/blobs/'

run_case "an executable Go file is still scanned normally" \
  head_entries='[{"path":"internal/hub/exec.go","mode":"100755","sha":"0000000000000000000000000000000000000000"}]' \
  blob_contents="$(jq -n --arg src "$plain_source" '{"0":$src}')" \
  expect_exit=0 expect_states='pending,success' expect_decision=success \
  gh_expect='/git/blobs/'

run_case "an unknown tree entry type fails closed" \
  head_entries='[{"path":"internal/hub/weird.go","type":"symlink","sha":"dddddddddddddddddddddddddddddddddddddddd"}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='truncated or malformed tree'

run_case "a malformed tree entry mode fails closed" \
  head_entries='[{"path":"internal/hub/weird.go","mode":"644"}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='truncated or malformed tree'

run_case "a tree entry without a SHA fails closed" \
  head_entries='[{"path":"internal/hub/weird.go","drop_sha":true}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='truncated or malformed tree'

run_case "a tree entry with an empty path fails closed" \
  head_entries='[{"path":""}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='truncated or malformed tree'

# ---------------------------------------------------------------------------
# Immutable diff integrity
# ---------------------------------------------------------------------------

run_case "a truncated head tree fails closed" \
  head_tree_truncated=true \
  head_entries='["README.md"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='truncated or malformed tree'

run_case "a truncated base tree cannot hide a Darwin deletion" \
  base_tree_truncated=true \
  base_entries='["internal/debugger/engine.go"]' head_entries='[]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='truncated or malformed tree'

run_case "an empty diff fails closed" \
  base_entries='["README.md"]' head_entries='["README.md"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='no changed files'

run_case "a compare API failure fails closed" \
  compare_status=1 \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='could not evaluate this head'

run_case "a base tree read failure fails closed" \
  base_tree_status=1 head_entries='["README.md"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='could not evaluate this head'

run_case "a head tree read failure fails closed" \
  head_tree_status=1 head_entries='["README.md"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='could not evaluate this head'

run_case "identical content with a different mode is a change" \
  base_entries='[{"path":"justfile","mode":"100644","sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}]' \
  head_entries='[{"path":"justfile","mode":"100755","sha":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

# ---------------------------------------------------------------------------
# Blocker 4: authorization is bound to a human actor, never to a status
# ---------------------------------------------------------------------------

run_case "write permission authorizes the verified label" \
  action=labeled has_label=true perm_permission=write perm_role=write \
  expect_exit=0 expect_states='pending,success' expect_decision=success \
  gh_expect='collaborators/maintainer-jo/permission'

run_case "maintain permission authorizes the verified label" \
  action=labeled has_label=true perm_permission=maintain perm_role=maintain \
  expect_exit=0 expect_states='pending,success' expect_decision=success

run_case "read permission cannot authorize the verified label" \
  action=labeled has_label=true perm_permission=read perm_role=triage \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='requires an authorized maintainer'

run_case "a non-collaborator cannot authorize the verified label" \
  action=labeled has_label=true perm_permission=none perm_role=none \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='requires an authorized maintainer'

run_case "bot actors cannot assert Darwin verification" \
  action=labeled has_label=true actor='github-actions[bot]' actor_type=Bot \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='requires an authorized maintainer' \
  gh_missing='collaborators/'

run_case "a User-typed login shaped like a bot is refused" \
  action=labeled has_label=true actor='sneaky[bot]' actor_type=User \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  gh_missing='collaborators/'

run_case "organization actors cannot assert Darwin verification" \
  action=labeled has_label=true actor=bingosuite actor_type=Organization \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  gh_missing='collaborators/'

run_case "path-like actor logins never reach the permission API" \
  action=labeled has_label=true actor='../../admin' actor_type=User \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  gh_missing='collaborators/'

run_case "an empty actor login is refused" \
  action=labeled has_label=true actor='' actor_type=User \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  gh_missing='collaborators/'

run_case "a permission API failure fails the label closed" \
  action=labeled has_label=true perm_status=1 \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='could not evaluate this head'

run_case "a permission response for another login is refused" \
  action=labeled has_label=true perm_login=someone-else \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='requires an authorized maintainer'

run_case "a forged head status cannot influence the decision" \
  action=labeled has_label=true \
  expect_exit=0 expect_states='pending,success' expect_decision=success \
  gh_missing='/statuses?per_page'

# ---------------------------------------------------------------------------
# Label lifecycle
# ---------------------------------------------------------------------------

run_case "an opened pull request is evaluated like any other head" \
  action=opened head_entries='["internal/debugger/engine.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='Darwin-native (CI-unexecutable) changes detected'

run_case "a reopened pull request is re-evaluated" \
  action=reopened head_entries='["README.md"]' \
  expect_exit=0 expect_states='pending,success' expect_decision=success \
  expect_output='No darwin-native code changed'

# Documented deliberately: with nothing Darwin-native in the head there is
# nothing to withdraw, so the scope check short-circuits to success before any
# label arm runs. Pinning it keeps AGENTS.md and the code from diverging.
run_case "an unlabel on a head with no darwin change still passes" \
  action=unlabeled has_label=false head_entries='["README.md"]' \
  expect_exit=0 expect_states='pending,success' expect_decision=success \
  expect_output='No darwin-native code changed'

run_case "removing the verified label always withdraws verification" \
  action=unlabeled has_label=false \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='withdrawn for this head' \
  expect_label_query=false

run_case "a re-added label cannot rescue a delayed unlabel event" \
  action=unlabeled has_label=true \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='withdrawn for this head' \
  expect_label_query=false

run_case "a removed label defeats a delayed label event" \
  action=labeled has_label=false \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='not currently present' \
  expect_label_query=true

run_case "a live label API failure fails closed" \
  action=labeled has_label=true label_status=1 \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='could not evaluate this head'

run_case "a label that merely contains the verified name is not a match" \
  action=labeled has_label=true \
  labels_json='[{"name":"darwin-e2e-verified-later"}]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_description='not currently present'

run_case "the verified label counts among several live labels" \
  action=labeled has_label=true \
  labels_json='[{"name":"needs-review"},{"name":"darwin-e2e-verified"},{"name":"area/debugger"}]' \
  expect_exit=0 expect_states='pending,success' expect_decision=success

run_case "synchronize clears a stale verified label" \
  head_entries='["internal/debugger/engine.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  gh_expect='pr edit'

run_case "a failed label cleanup still publishes failure" \
  label_remove_status=1 \
  head_entries='["internal/debugger/engine.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure \
  expect_output='could not remove'

run_case "a base-changing edit invalidates verification" \
  action=edited edited_changes='{"base":{"ref":{"from":"main"}}}' \
  head_entries='["internal/debugger/engine.go"]' \
  expect_exit=1 expect_states='pending,failure' expect_decision=failure

# ---------------------------------------------------------------------------
# Status publication integrity
# ---------------------------------------------------------------------------

run_case "a failed status POST records no decision" \
  status_post_status=1 \
  head_entries='["README.md"]' \
  expect_exit=1 expect_states='' expect_decision=''

# ---------------------------------------------------------------------------
# Status persistence across a sequence of events on one head
# ---------------------------------------------------------------------------

run_status_persistence_sequence() {
  local name="a verified head survives an unrelated label event"
  cases=$((cases + 1))

  local work="$tmpdir/sequence"
  mkdir -p "$work"

  local head_sha=deadbeef00000000000000000000000000000001
  local base_sha=ba5eba1100000000000000000000000000000002
  local merge_base=cafebabe00000000000000000000000000000003
  local base_tree=feedface00000000000000000000000000000004
  local head_tree=feedface00000000000000000000000000000005

  local compare_json
  compare_json=$(jq -n --arg mb "$merge_base" '{merge_base_commit: {sha: $mb}}')

  local live_pr_json
  live_pr_json=$(jq -n \
    --arg base_sha "$base_sha" --arg head_sha "$head_sha" \
    '{state:"open", base:{ref:"main", sha:$base_sha},
      head:{sha:$head_sha, repo:{full_name:"contributor/fork"}}}')

  local permission_json
  permission_json=$(jq -n '{permission:"admin", role_name:"admin", user:{login:"maintainer-jo"}}')

  printf '{}' > "$work/blobs.json"
  printf '{}' > "$work/blobs-b64.json"

  run_step() {
    local step=$1 action=$2 label=$3 has_label=$4
    local event="$work/event-$step.json"
    local labels='[{"name":"needs-review"}]'
    [ "$has_label" = "true" ] && labels='[{"name":"darwin-e2e-verified"}]'

    jq -n --arg head "$head_sha" --arg base "$base_sha" \
      --arg action "$action" --arg label "$label" \
      '{action: $action, sender: {login: "maintainer-jo", type: "User"},
        label: {name: $label}, changes: {},
        pull_request: {number: 17, state: "open",
          head: {sha: $head, repo: {full_name: "contributor/fork"}},
          base: {sha: $base, ref: "main"}}}' > "$event"

    (
      PATH="$mock_bin:$PATH" \
      GITHUB_EVENT_PATH="$event" \
      GITHUB_REPOSITORY=bingosuite/bingo \
      GITHUB_SERVER_URL=https://github.com \
      GITHUB_RUN_ID=4242 \
      DARWIN_GATE_DECISION_FILE="$work/decision-$step" \
      MOCK_GH_LOG="$work/gh.log" \
      MOCK_STATE_LOG="$work/states.log" \
      MOCK_STATUS_LOG="$work/statuses.log" \
      MOCK_TARGET_LOG="$work/targets.log" \
      MOCK_LABELS_JSON="$labels" \
      MOCK_COMPARE_JSON="$compare_json" \
      MOCK_MERGE_BASE_SHA="$merge_base" \
      MOCK_HEAD_SHA="$head_sha" \
      MOCK_BASE_TREE_SHA="$base_tree" \
      MOCK_HEAD_TREE_SHA="$head_tree" \
      MOCK_BASE_TREE_ENTRIES='[]' \
      MOCK_HEAD_TREE_ENTRIES='["internal/debugger/engine.go"]' \
      MOCK_BASE_TREE_TRUNCATED=false \
      MOCK_HEAD_TREE_TRUNCATED=false \
      MOCK_BLOB_CONTENTS_FILE="$work/blobs.json" \
      MOCK_BLOB_B64_FILE="$work/blobs-b64.json" \
      MOCK_LIVE_PR_JSON="$live_pr_json" \
      MOCK_PERMISSION_JSON="$permission_json" \
      bash "$gate"
    ) > "$work/out-$step.txt" 2>&1
    printf '%s' $?
  }

  local rc1 rc2 rc3
  rc1=$(run_step 1 synchronize darwin-e2e-verified false)
  rc2=$(run_step 2 labeled darwin-e2e-verified true)
  rc3=$(run_step 3 labeled needs-review true)

  local ok=1
  [ "$rc1" = "1" ] || { fail "$name: synchronize should fail closed (got $rc1)"; ok=0; }
  [ "$rc2" = "0" ] || { fail "$name: verified label should succeed (got $rc2)"; ok=0; }
  [ "$rc3" = "0" ] || { fail "$name: unrelated label should be ignored (got $rc3)"; ok=0; }

  local states
  states=$(paste -sd, - < "$work/states.log")
  if [ "$states" != "pending,failure,pending,success" ]; then
    fail "$name: expected 'pending,failure,pending,success', got '$states'"
    ok=0
  fi

  [ -f "$work/decision-3" ] && [ "$(cat "$work/decision-3")" = "ignored" ] || {
    fail "$name: unrelated label should record an ignored decision"
    ok=0
  }

  [ "$ok" = "1" ] && pass "$name"
}

run_status_persistence_sequence

# Commit statuses are SHA-global, so two pull requests can point at the same head
# commit. Only a generation that targets `main` may publish success for it.
#
# This drives the policy directly, so it exercises the script's own base-ref
# re-assertion — defense in depth for a misconfigured or removed trigger filter.
# In the deployed workflow the `branches: [main]` filter means an alternate-base
# run is never dispatched at all, which is strictly safer: it also posts nothing.
# What the workflow filter cannot do is un-publish a legitimate `main` success
# from a shared head SHA; that inherited-visibility residual is documented in
# AGENTS.md and is a property of commit statuses, not a gate decision.
run_shared_head_sha_sequence() {
  local name="only the main-targeting pull request may green a shared head SHA"
  cases=$((cases + 1))

  local work="$tmpdir/shared-head"
  mkdir -p "$work"

  local head_sha=5aaaaaaa00000000000000000000000000000001
  local base_sha=5bbbbbbb00000000000000000000000000000002
  local merge_base=5ccccccc00000000000000000000000000000003
  local base_tree=5ddddddd00000000000000000000000000000004
  local head_tree=5eeeeeee00000000000000000000000000000005

  local compare_json
  compare_json=$(jq -n --arg mb "$merge_base" '{merge_base_commit: {sha: $mb}}')

  local permission_json
  permission_json=$(jq -n \
    '{permission:"admin", role_name:"admin", user:{login:"maintainer-jo"}}')

  printf '{}' > "$work/blobs.json"
  printf '{}' > "$work/blobs-b64.json"

  run_shared_step() {
    local step=$1 pr_number=$2 base_ref=$3
    local event="$work/event-$step.json"

    jq -n --arg head "$head_sha" --arg base "$base_sha" \
      --arg base_ref "$base_ref" --argjson number "$pr_number" \
      '{action: "labeled", sender: {login: "maintainer-jo", type: "User"},
        label: {name: "darwin-e2e-verified"}, changes: {},
        pull_request: {number: $number, state: "open",
          head: {sha: $head, repo: {full_name: "contributor/fork"}},
          base: {sha: $base, ref: $base_ref}}}' > "$event"

    local live_pr_json
    live_pr_json=$(jq -n --arg base_ref "$base_ref" \
      --arg base_sha "$base_sha" --arg head_sha "$head_sha" \
      '{state:"open", base:{ref:$base_ref, sha:$base_sha},
        head:{sha:$head_sha, repo:{full_name:"contributor/fork"}}}')

    (
      PATH="$mock_bin:$PATH" \
      GITHUB_EVENT_PATH="$event" \
      GITHUB_REPOSITORY=bingosuite/bingo \
      GITHUB_SERVER_URL=https://github.com \
      GITHUB_RUN_ID=4242 \
      DARWIN_GATE_DECISION_FILE="$work/decision-$step" \
      MOCK_GH_LOG="$work/gh.log" \
      MOCK_STATE_LOG="$work/states.log" \
      MOCK_STATUS_LOG="$work/statuses.log" \
      MOCK_TARGET_LOG="$work/targets.log" \
      MOCK_LABELS_JSON='[{"name":"darwin-e2e-verified"}]' \
      MOCK_COMPARE_JSON="$compare_json" \
      MOCK_MERGE_BASE_SHA="$merge_base" \
      MOCK_HEAD_SHA="$head_sha" \
      MOCK_BASE_TREE_SHA="$base_tree" \
      MOCK_HEAD_TREE_SHA="$head_tree" \
      MOCK_BASE_TREE_ENTRIES='[]' \
      MOCK_HEAD_TREE_ENTRIES='["internal/debugger/engine.go"]' \
      MOCK_BASE_TREE_TRUNCATED=false \
      MOCK_HEAD_TREE_TRUNCATED=false \
      MOCK_BLOB_CONTENTS_FILE="$work/blobs.json" \
      MOCK_BLOB_B64_FILE="$work/blobs-b64.json" \
      MOCK_LIVE_PR_JSON="$live_pr_json" \
      MOCK_PERMISSION_JSON="$permission_json" \
      bash "$gate"
    ) > "$work/out-$step.txt" 2>&1
    printf '%s' $?
  }

  local rc_alt rc_main
  rc_alt=$(run_shared_step 1 21 release-1.x)
  rc_main=$(run_shared_step 2 17 main)

  local ok=1
  [ "$rc_alt" = "1" ] || {
    fail "$name: the alternate-base pull request should fail (got $rc_alt)"
    ok=0
  }
  [ "$rc_main" = "0" ] || {
    fail "$name: the main pull request should succeed (got $rc_main)"
    ok=0
  }

  local states
  states=$(paste -sd, - < "$work/states.log")
  if [ "$states" != "failure,pending,success" ]; then
    fail "$name: expected 'failure,pending,success', got '$states'"
    ok=0
  fi

  if ! grep -Fq "/statuses/$head_sha" "$work/gh.log"; then
    fail "$name: both generations should address the same head SHA"
    ok=0
  fi

  if grep -Fq "/git/trees/" "$work/out-1.txt"; then
    fail "$name: the alternate-base generation should not inspect the diff"
    ok=0
  fi

  [ "$ok" = "1" ] && pass "$name"
}

run_shared_head_sha_sequence

# ---------------------------------------------------------------------------
# Static contracts on the policy script and the workflows
# ---------------------------------------------------------------------------

check() {
  local name=$1
  shift
  cases=$((cases + 1))
  if "$@"; then
    pass "$name"
  else
    fail "$name"
  fi
}

trusted_workflow_uses_pull_request_target() {
  grep -q '^  pull_request_target:' "$workflow"
}

trusted_workflow_targets_main_only() {
  # Statuses are SHA-global and the policy is fetched by SHA, so the trusted
  # workflow must never run for a base branch an attacker can create.
  awk '
    /^  pull_request_target:/ {inside = 1; next}
    inside && /^  [^ ]/ {inside = 0}
    inside && /^    branches: \[main\]$/ {found = 1}
    END {exit found ? 0 : 1}
  ' "$workflow"
}

trusted_workflow_never_checks_out() {
  ! grep -q 'actions/checkout' "$workflow"
}

# `go/build` decides what actually compiles, and only `.go` blobs get a content
# scan, so every other extension it accepts must be caught by name. Shrinking
# this list silently reopens the "untagged cgo wrapper + shim_darwin.sx" bypass.
gate_covers_every_go_build_extension() {
  local exts ext
  exts=$(sed -n "s/^readonly darwin_native_exts='\(.*\)'$/\1/p" "$gate")
  [ -n "$exts" ] || return 1

  for ext in c cc cpp cxx m mm h hh hpp hxx f F for f90 s S sx swig swigcxx syso; do
    case "|$exts|" in
      *"|$ext|"*) ;;
      *) return 1 ;;
    esac
  done

  # Plain `.go` must NOT be in the bare-extension alternation: every Go file
  # would gate and the content scan would become dead code.
  case "|$exts|" in
    *'|go|'*) return 1 ;;
  esac

  # ... but a `_darwin`/`_arm64`-suffixed `.go` file carries an implicit
  # constraint and must still be caught by name.
  grep -q '_(darwin|arm64)(_\[\^/\.\]+)\*\\\\\.(go|\$darwin_native_exts)' "$gate"
}

trusted_workflow_runs_trusted_policy_only() {
  grep -q 'POLICY_SHA: \${{ github.workflow_sha }}' "$workflow" &&
    grep -q 'darwin-verification-gate.sh?ref=\$POLICY_SHA' "$workflow" &&
    ! grep -q 'github.event.pull_request.base.sha' "$workflow"
}

# The single highest-value guard for a privileged `pull_request_target` file:
# one `${{ github.event.pull_request.title }}` inside a `run:` block is shell
# injection with statuses:write and pull-requests:write. Untrusted values may
# only enter through `env:`, where they are variables and not code.
trusted_workflow_never_interpolates_into_run() {
  # Covers all four shell-bearing forms an expression could reach: a `run:`
  # block scalar (`|` or `>`), a single-line `run:`, and an `actions/github-script`
  # `script:` block. `env:`/`concurrency:` interpolation stays allowed — those
  # do not build a shell word.
  awk '
    /^ *(run|script): *[|>][-+0-9]* *$/ {
      inside = 1
      indent = match($0, /[^ ]/)
      next
    }
    inside {
      here = match($0, /[^ ]/)
      if ($0 !~ /^ *$/ && here <= indent) {inside = 0}
    }
    inside && /\$\{\{/ {bad = 1}
    # A single-line `run:`/`script:` value is a shell word on its own line.
    !inside && /^ *(run|script): *[^|>]/ && /\$\{\{/ {bad = 1}
    END {exit bad ? 1 : 0}
  ' "$workflow"
}

# A job-level `permissions:` block overrides the workflow-level one, so the
# top-level check alone is not sufficient.
trusted_workflow_has_no_scope_override() {
  [ "$(grep -c '^ *permissions:' "$workflow")" = "1" ] &&
    grep -q '^permissions:' "$workflow"
}

# Dropping `unlabeled` would silently make verification withdrawal a no-op;
# dropping `synchronize` would leave a new head SHA with no status at all.
trusted_subscribes_to_gated_actions() {
  local types action
  types=$(grep -E '^ *types: \[' "$workflow")
  [ -n "$types" ] || return 1
  for action in opened synchronize reopened edited labeled unlabeled; do
    case "$types" in
      *"$action"*) ;;
      *) return 1 ;;
    esac
  done
}

trusted_workflow_fails_closed_without_policy() {
  grep -q 'missing-policy' "$workflow" &&
    grep -q 'Trusted Darwin verification policy is unavailable' "$workflow"
}

trusted_workflow_never_cancels_in_progress() {
  grep -q 'cancel-in-progress: false' "$workflow" &&
    ! grep -q 'cancel-in-progress: true' "$workflow"
}

trusted_workflow_serializes_only_authoritative_events() {
  grep -q "format('unrelated-{0}', github.run_id)" "$workflow" &&
    grep -q "|| 'policy' }}" "$workflow"
}

trusted_workflow_has_a_decision_fallback() {
  grep -q 'if: always()' "$workflow" &&
    grep -q 'Darwin verification gate ended without a decision' "$workflow"
}

trusted_workflow_permissions_are_minimal() {
  awk '
    /^permissions:/ {inside = 1; next}
    inside && /^[^ ]/ {inside = 0}
    inside && /^  [a-z-]+:/ {
      line = $0
      sub(/^  /, "", line)
      if (line != "contents: read" &&
          line != "pull-requests: write" &&
          line != "statuses: write") {
        bad = 1
      }
    }
    END {exit bad ? 1 : 0}
  ' "$workflow"
}

trusted_workflow_declares_advisory_posture() {
  grep -qi 'advisory' "$workflow" &&
    grep -q 'github-actions\[bot\]' "$workflow"
}

trusted_workflow_has_no_merge_group_publisher() {
  ! grep -q 'merge_group' "$workflow"
}

policy_test_workflow_is_unprivileged() {
  grep -q '^  pull_request:' "$policy_test_workflow" &&
    ! grep -q 'pull_request_target' "$policy_test_workflow" &&
    ! grep -q 'statuses: write' "$policy_test_workflow" &&
    ! grep -q 'Darwin E2E verified' "$policy_test_workflow"
}

gate_binds_every_success_to_the_live_generation() {
  awk '
    /^[[:space:]]*#/ {next}
    /^[[:space:]]*$/ {next}
    {
      if ($0 ~ /post_status success/ && prev !~ /require_current_generation/) {
        bad = 1
      }
      prev = $0
    }
    END {exit bad ? 1 : 0}
  ' "$gate"
}

gate_never_reads_prior_statuses() {
  ! grep -q 'approval_ready' "$gate" &&
    ! grep -Eq '/commits/[^"]*/statuses' "$gate"
}

gate_binds_authorization_to_the_event_actor() {
  grep -q '\.sender\.login' "$gate" &&
    grep -q '\.sender\.type' "$gate" &&
    grep -q 'collaborators/\$actor_login/permission' "$gate" &&
    grep -q 'require_authorized_labeler' "$gate"
}

gate_authorizes_before_publishing_success() {
  # Per-site, not "the last one wins": a new success site added above the
  # authorization call must fail this check.
  local line last=0 authorized=0 sites=0
  while read -r line; do
    case "$line" in
      *require_authorized_labeler) authorized=1 ;;
      *"post_status success 'Darwin E2E verified"*)
        sites=$((sites + 1))
        [ "$authorized" = "1" ] || return 1
        ;;
    esac
    last=1
  done <<EOF
$(grep -E "require_authorized_labeler\$|post_status success 'Darwin E2E verified" "$gate")
EOF
  [ "$last" = "1" ] && [ "$sites" -ge 1 ]
}

gate_never_publishes_success_from_an_unlabel() {
  local arm withdraw
  arm=$(grep -n '^  labeled | unlabeled)' "$gate" | head -1 | cut -d: -f1)
  withdraw=$(grep -n "post_status failure 'Darwin verification was withdrawn" \
    "$gate" | head -1 | cut -d: -f1)
  [ -n "$arm" ] && [ -n "$withdraw" ] || return 1

  # The unlabeled branch must terminate the arm before any success can be
  # reached, so no `post_status success` may appear between them.
  awk -v a="$arm" -v b="$withdraw" \
    'NR > a && NR < b && /post_status success/ {bad = 1}
     END {exit bad ? 1 : 0}' "$gate" || return 1

  awk -v a="$withdraw" \
    'NR > a && /^      exit 1$/ {found = 1; exit}
     END {exit found ? 0 : 1}' "$gate"
}

# A force-push can change `/pulls/N/files` while the run is still publishing to
# the event's old head SHA, so the diff must come from immutable tree objects.
gate_uses_immutable_diff() {
  grep -q 'git/trees/' "$gate" &&
    grep -q 'recursive=1' "$gate" &&
    grep -q 'compare/' "$gate" &&
    ! grep -Eq 'pulls/[^"]*/files' "$gate"
}

# The "gate never consumes PR-authored text" invariant is only meaningful while
# the synthetic event actually carries the poison token in every field an
# attacker controls. An earlier revision gated that check behind a per-case
# variable no case ever set, making it silently vacuous.
harness_poisons_every_attacker_controlled_field() {
  local field
  for field in title body ref label; do
    grep -Eq "$field: \(\\\$poison \+" "$0" || return 1
  done
  grep -q '\--arg poison "\$untrusted_token"' "$0" &&
    grep -q 'grep -Fq "\$untrusted_token" "\$work/gh.log"' "$0"
}

gate_enforces_the_required_base_ref() {
  grep -q "required_base_ref='main'" "$gate" &&
    grep -q 'base_ref" != "\$required_base_ref' "$gate"
}

gate_validates_tree_documents() {
  grep -q 'validate_tree_document' "$gate" &&
    grep -q 'truncated' "$gate" &&
    grep -q '\^\[0-7\]{6}\$' "$gate"
}

gate_normalizes_byte_order_marks() {
  grep -q 'efbbbf' "$gate" &&
    grep -q 'tail -c +4' "$gate" &&
    grep -q 'fffe\|feff' "$gate"
}

gate_gates_non_regular_tree_entries() {
  grep -q 'irregular' "$gate" &&
    grep -q '100755' "$gate" &&
    grep -q '100644' "$gate"
}

gate_publishes_only_from_the_top_level_shell() {
  # `set -E` propagates the ERR trap into command-substitution subshells, so an
  # unguarded substitution below the guarded-region marker would publish a
  # status from the subshell and then again from the parent. Every substitution
  # must disarm the trap first. Full-line comments cannot execute, so they are
  # excluded from the scan.
  local region="$tmpdir/gate-region.sh"
  awk '
    /^# --- ERR-trap guarded region/ {inside = 1}
    inside && !/^[[:space:]]*#/ {print}
  ' "$gate" > "$region"

  [ -s "$region" ] || return 1

  # Count per line, not per matching line: `a=$(trap - ERR; x); b=$(y)` would
  # survive a line-granular filter because the guarded substitution deletes the
  # whole line from the scan.
  local total guarded
  total=$(grep -o '\$(' "$region" | wc -l | tr -d ' ')
  guarded=$(grep -o '\$(trap - ERR;' "$region" | wc -l | tr -d ' ')
  if [ "$total" != "$guarded" ]; then
    printf 'unguarded command substitution in the ERR-trap region (%s of %s):\n' \
      "$((total - guarded))" "$total" >&2
    grep -n '\$(' "$region" >&2
    return 1
  fi

  # Backticks, process substitution and explicit grouping subshells all inherit
  # the ERR trap under `set -E` exactly like `$( )` does. The grouping form is
  # anchored to command position — line start, after a separator, or after a
  # block keyword — so parentheses inside regexes and message strings, of which
  # this script has many, are not false positives.
  local banned='`|<\(|>\(|((^|[;&|{])[ \t]*|(^|[ \t;&|{])[ \t]*(then|else|do|elif|in)[ \t]+)\([ \t]*[A-Za-z_$]'
  if grep -Eq "$banned" "$region"; then
    printf 'banned subshell form in the ERR-trap region:\n' >&2
    grep -En "$banned" "$region" >&2
    return 1
  fi

  return 0
}

gate_records_a_decision_for_every_final_status() {
  awk '
    /^record_decision\(\)/ {seen_record = 1}
    /^post_status\(\)/ {inside = 1}
    inside && /record_decision/ {calls = 1}
    inside && /^}/ {inside = 0}
    END {exit (seen_record && calls) ? 0 : 1}
  ' "$gate"
}

check "trusted workflow uses pull_request_target" trusted_workflow_uses_pull_request_target
check "trusted workflow targets main only" trusted_workflow_targets_main_only
check "trusted workflow never checks out" trusted_workflow_never_checks_out
check "trusted workflow never interpolates into run" \
  trusted_workflow_never_interpolates_into_run
check "trusted workflow has no job scope override" \
  trusted_workflow_has_no_scope_override
check "trusted workflow subscribes to gated actions" \
  trusted_subscribes_to_gated_actions
check "gate uses immutable diff" gate_uses_immutable_diff
check "harness poisons every attacker controlled field" \
  harness_poisons_every_attacker_controlled_field
check "gate covers every go/build native extension" \
  gate_covers_every_go_build_extension
check "trusted workflow runs trusted policy only" trusted_workflow_runs_trusted_policy_only
check "trusted workflow fails closed without policy" trusted_workflow_fails_closed_without_policy
check "trusted workflow never cancels in progress" trusted_workflow_never_cancels_in_progress
check "trusted workflow serializes only authoritative events" trusted_workflow_serializes_only_authoritative_events
check "trusted workflow has a decision fallback" trusted_workflow_has_a_decision_fallback
check "trusted workflow permissions are minimal" trusted_workflow_permissions_are_minimal
check "trusted workflow declares advisory posture" trusted_workflow_declares_advisory_posture
check "trusted workflow has no merge_group publisher" trusted_workflow_has_no_merge_group_publisher
check "policy test workflow is unprivileged" policy_test_workflow_is_unprivileged
check "gate binds every success to the live generation" gate_binds_every_success_to_the_live_generation
check "gate never reads prior statuses" gate_never_reads_prior_statuses
check "gate binds authorization to the event actor" gate_binds_authorization_to_the_event_actor
check "gate authorizes before publishing success" gate_authorizes_before_publishing_success
check "gate never publishes success from an unlabel" gate_never_publishes_success_from_an_unlabel
check "gate enforces the required base ref" gate_enforces_the_required_base_ref
check "gate validates tree documents" gate_validates_tree_documents
check "gate normalizes byte order marks" gate_normalizes_byte_order_marks
check "gate gates non-regular tree entries" gate_gates_non_regular_tree_entries
check "gate publishes only from the top level shell" gate_publishes_only_from_the_top_level_shell
check "gate records a decision for every final status" gate_records_a_decision_for_every_final_status

printf '\n%d cases, %d failures\n' "$cases" "$failures"
[ "$failures" -eq 0 ]
