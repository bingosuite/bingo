#!/usr/bin/env bash

set -Eeuo pipefail

: "${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

readonly verified_label='darwin-e2e-verified'
readonly status_context='Darwin E2E verified'
readonly darwin_native_regex='^(internal/debugger/.*|test/integration/.*|justfile|entitlements\.plist|go\.(mod|sum)|(.*/)?[^/]*_(darwin|arm64)(_[^/.]+)*\.(go|s|S|c|cc|cpp|cxx|h|hh|hpp|hxx|m|mm|syso)|(.*/)?[^/]*\.(c|cc|cpp|cxx|h|hh|hpp|hxx|m|mm|s|S|syso))$'

event_action=$(jq -er '.action' "$GITHUB_EVENT_PATH")
pr_number=$(jq -er '.pull_request.number' "$GITHUB_EVENT_PATH")
head_sha=$(jq -er '.pull_request.head.sha | select(test("^[0-9a-fA-F]{40,64}$"))' "$GITHUB_EVENT_PATH")
base_sha=$(jq -er '.pull_request.base.sha | select(test("^[0-9a-fA-F]{40,64}$"))' "$GITHUB_EVENT_PATH")
head_repository=$(jq -er '.pull_request.head.repo.full_name // ""' "$GITHUB_EVENT_PATH")
event_label=$(jq -er '.label.name // ""' "$GITHUB_EVENT_PATH")
base_changed=$(jq -r '(.changes.base // null) != null' "$GITHUB_EVENT_PATH")
run_url="${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/${GITHUB_RUN_ID:-0}"
final_status_posted=false
decision_file=${DARWIN_GATE_DECISION_FILE:-}
approval_ready=false

# The workflow treats an absent decision as "interrupted after posting pending"
# and fails the head closed, so only terminal outcomes may be recorded here.
record_decision() {
  [ -n "$decision_file" ] || return 0
  printf '%s\n' "$1" > "$decision_file"
}

post_status() {
  local state=$1
  local description=$2

  # A decision is only real once GitHub accepted it, and `fail_closed` runs with
  # `set +e`, so the POST result must be checked explicitly rather than relying
  # on `set -e` to abort here.
  if ! gh api --method POST "repos/$GITHUB_REPOSITORY/statuses/$head_sha" \
    -f state="$state" \
    -f context="$status_context" \
    -f description="$description" \
    -f target_url="$run_url" >/dev/null; then
    return 1
  fi

  if [ "$state" != "pending" ]; then
    final_status_posted=true
    record_decision "$state"
  fi
}

fail_closed() {
  local exit_code=$?

  trap - ERR
  if [ "$final_status_posted" != "true" ]; then
    set +e
    post_status failure 'Darwin verification gate could not evaluate this head.'
  fi
  exit "$exit_code"
}
# `set -E` propagates this trap into command-substitution subshells, where a
# failure would post a status the parent then posts again (and where
# `final_status_posted` cannot propagate back out). Every `$(...)` below the
# trap therefore disarms it so the parent shell stays the only decision point.
trap fail_closed ERR

case "$event_action" in
  opened | synchronize | reopened | edited | labeled | unlabeled) ;;
  *)
    echo "Unsupported pull_request_target action: $event_action" >&2
    exit 2
    ;;
esac

if { [ "$event_action" = "labeled" ] || [ "$event_action" = "unlabeled" ]; } &&
  [ "$event_label" != "$verified_label" ]; then
  echo "Ignoring unrelated '$event_label' label event; the existing head-SHA status remains authoritative."
  record_decision ignored
  exit 0
fi

if [ "$event_action" = "edited" ] && [ "$base_changed" != "true" ]; then
  echo "Ignoring pull request edit that did not change the base; the existing head-SHA status remains authoritative."
  record_decision ignored
  exit 0
fi

if [ "$event_action" = "labeled" ] && [ "$event_label" = "$verified_label" ]; then
  prior_statuses_json=$(trap - ERR; mktemp)
  trap 'rm -f "$prior_statuses_json"' EXIT
  gh api --paginate --slurp \
    "repos/$GITHUB_REPOSITORY/commits/$head_sha/statuses?per_page=100" \
    > "$prior_statuses_json"
  approval_ready=$(trap - ERR; jq -r \
    --arg context "$status_context" \
    --arg run_prefix "${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/" '
      [
        .[][]
        | select(.context == $context)
      ][0] as $latest
      | (
          $latest.state == "failure"
          and $latest.creator.login == "github-actions[bot]"
          and (($latest.target_url // "") | startswith($run_prefix))
        )
    ' "$prior_statuses_json")
  rm -f "$prior_statuses_json"
fi

post_status pending 'Evaluating Darwin verification policy.'

label_cleanup=not-run
if [ "$event_action" = "synchronize" ] ||
  [ "$event_action" = "reopened" ] ||
  [ "$event_action" = "edited" ]; then
  if output=$(trap - ERR; gh pr edit "$pr_number" --repo "$GITHUB_REPOSITORY" \
    --remove-label "$verified_label" 2>&1); then
    label_cleanup=cleared
    echo "Cleared '$verified_label' if present; this head requires re-verification."
  else
    exit_code=$?
    label_cleanup=failed
    if [ "$head_repository" != "$GITHUB_REPOSITORY" ]; then
      echo "::warning title=Stale Darwin verification label not removed::The public-fork GITHUB_TOKEN could not remove '$verified_label' (exit $exit_code). The head status will fail closed; remove or toggle the stale label, then re-add it after local verification."
    else
      echo "::warning title=Stale Darwin verification label not removed::The API could not remove '$verified_label' (exit $exit_code). The head status will fail closed; remove or toggle the stale label, then re-add it after local verification."
    fi
    printf '%s\n' "$output" >&2
  fi
fi

base_tree_json=$(trap - ERR; mktemp)
head_tree_json=$(trap - ERR; mktemp)
changed_paths_json=$(trap - ERR; mktemp)
trap 'rm -f "$base_tree_json" "$head_tree_json" "$changed_paths_json"' EXIT

merge_base_sha=$(trap - ERR; gh api \
  "repos/$GITHUB_REPOSITORY/compare/$base_sha...$head_sha" \
  --jq '.merge_base_commit.sha')
case "$merge_base_sha" in
  '' | *[!0-9a-fA-F]*)
    echo "Compare API returned an invalid merge-base SHA." >&2
    false
    ;;
esac

merge_base_tree_sha=$(trap - ERR; gh api \
  "repos/$GITHUB_REPOSITORY/git/commits/$merge_base_sha" --jq '.tree.sha')
head_tree_sha=$(trap - ERR; gh api \
  "repos/$GITHUB_REPOSITORY/git/commits/$head_sha" --jq '.tree.sha')

gh api "repos/$GITHUB_REPOSITORY/git/trees/$merge_base_tree_sha?recursive=1" \
  > "$base_tree_json"
gh api "repos/$GITHUB_REPOSITORY/git/trees/$head_tree_sha?recursive=1" \
  > "$head_tree_json"

if ! jq -e '.truncated == false and (.tree | type == "array")' \
  "$base_tree_json" >/dev/null ||
  ! jq -e '.truncated == false and (.tree | type == "array")' \
    "$head_tree_json" >/dev/null; then
  echo "Git tree API returned a truncated or malformed tree; refusing an incomplete gate decision." >&2
  false
fi

jq -n \
  --slurpfile base "$base_tree_json" \
  --slurpfile head "$head_tree_json" '
    def entries($doc):
      reduce ($doc[0].tree[] | select(.type != "tree")) as $entry
        ({};
          .[$entry.path] = {
            mode: $entry.mode,
            sha: $entry.sha,
            type: $entry.type
          });
    entries($base) as $before
    | entries($head) as $after
    | (($before | keys_unsorted) + ($after | keys_unsorted) | unique)
    | map(select($before[.] != $after[.]) | {
        path: .,
        before: $before[.],
        after: $after[.]
      })
  ' > "$changed_paths_json"

listed_changed_files=$(trap - ERR; jq -er 'length' "$changed_paths_json")
if [ "$listed_changed_files" -eq 0 ]; then
  echo "Immutable base/head comparison returned no changed files; refusing an empty gate decision." >&2
  false
fi

echo "Changed files in this PR:"
jq -r '.[].path | @json' "$changed_paths_json" | sed 's/^/  /'
echo

darwin_changed=$(trap - ERR; jq -r --arg regex "$darwin_native_regex" \
  'any(.[].path; test($regex) or test("[[:cntrl:]]"))' \
  "$changed_paths_json")

if [ "$darwin_changed" = "false" ]; then
  blob_shas=$(trap - ERR; jq -r '
    .[]
    | select(.path | endswith(".go"))
    | .before.sha, .after.sha
    | select(type == "string")
  ' "$changed_paths_json" | sort -u)
  while IFS= read -r blob_sha; do
    [ -n "$blob_sha" ] || continue
    blob_file=$(trap - ERR; mktemp)
    gh api "repos/$GITHUB_REPOSITORY/git/blobs/$blob_sha" \
      --jq '.content' | base64 -d > "$blob_file"
    if grep -Eq '^[[:space:]]*//[[:space:]]*(go:build|\+build)[[:space:]]' \
      "$blob_file"; then
      darwin_changed=true
      rm -f "$blob_file"
      break
    fi
    rm -f "$blob_file"
  done <<< "$blob_shas"
fi

if [ "$darwin_changed" = "false" ]; then
  post_status success 'No Darwin-native files changed.'
  echo "No darwin-native code changed; gate not required."
  exit 0
fi

if [ "$darwin_changed" != "true" ]; then
  echo "Unexpected Darwin path decision: $darwin_changed" >&2
  false
fi

echo "Darwin-native (CI-unexecutable) files changed:"
jq -r --arg regex "$darwin_native_regex" \
  '.[].path | select(test($regex) or test("[[:cntrl:]]")) | @json' \
  "$changed_paths_json" | sed 's/^/  /'
echo

case "$event_action" in
  synchronize | reopened | edited)
    if [ "$label_cleanup" = "failed" ]; then
      echo "::error title=Darwin E2E re-verification required::The stale '$verified_label' label could not be removed. Using the trusted base branch's justfile, run the e2e-darwin recipe against this head on Apple Silicon; then remove or toggle the stale label and re-add it."
    else
      echo "::error title=Darwin E2E re-verification required::Using the trusted base branch's justfile, run the e2e-darwin recipe against this head on Apple Silicon, review any PR changes to the recipe, confirm it passes, then add the '$verified_label' label."
    fi
    post_status failure 'New commits or reopening require Darwin re-verification.'
    trap - ERR
    exit 1
    ;;
  opened)
    post_status failure 'Darwin E2E verification is required for this head.'
    echo "::error title=Darwin E2E verification required::Using the trusted base branch's justfile, run the e2e-darwin recipe against this head on Apple Silicon, review any PR changes to the recipe, confirm it passes, then add the '$verified_label' label."
    trap - ERR
    exit 1
    ;;
  labeled | unlabeled)
    live_labels=$(trap - ERR; gh api --paginate \
      "repos/$GITHUB_REPOSITORY/issues/$pr_number/labels?per_page=100" \
      --jq '.[].name')
    if grep -Fxq "$verified_label" <<< "$live_labels"; then
      if [ "$event_action" = "labeled" ] && [ "$approval_ready" != "true" ]; then
        post_status failure 'Darwin verification must follow a head evaluation.'
        echo "::error title=Darwin E2E verification not ready::This head has not completed a trusted failing gate run yet. Wait for that run, then toggle '$verified_label' again."
        trap - ERR
        exit 1
      fi
      post_status success 'Darwin E2E verified for this head SHA.'
      echo "'$verified_label' label present; darwin backend verified locally for $head_sha."
      exit 0
    fi

    post_status failure 'Darwin verification label is not currently present.'
    echo "::error title=Darwin E2E verification required::The '$verified_label' label is not currently present on this PR."
    trap - ERR
    exit 1
    ;;
esac
