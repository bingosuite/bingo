#!/usr/bin/env bash

set -Eeuo pipefail

: "${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

readonly verified_label='darwin-e2e-verified'
readonly status_context='Darwin E2E verified'
readonly darwin_native_regex='^(internal/debugger/.*_darwin_.*|internal/debugger/trap_arm64\.go|test/integration/.*_darwin_.*_test\.go|entitlements\.plist)$'

event_action=$(jq -er '.action' "$GITHUB_EVENT_PATH")
pr_number=$(jq -er '.pull_request.number' "$GITHUB_EVENT_PATH")
head_sha=$(jq -er '.pull_request.head.sha | select(test("^[0-9a-fA-F]{40,64}$"))' "$GITHUB_EVENT_PATH")
head_repository=$(jq -er '.pull_request.head.repo.full_name // ""' "$GITHUB_EVENT_PATH")
event_label=$(jq -er '.label.name // ""' "$GITHUB_EVENT_PATH")
declared_changed_files=$(jq -er \
  '.pull_request.changed_files | numbers | select(. >= 0 and floor == .)' \
  "$GITHUB_EVENT_PATH")
run_url="${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/${GITHUB_RUN_ID:-0}"
final_status_posted=false
decision_file=${DARWIN_GATE_DECISION_FILE:-}

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
trap fail_closed ERR

case "$event_action" in
  opened | synchronize | reopened | labeled | unlabeled) ;;
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

post_status pending 'Evaluating Darwin verification policy.'

label_cleanup=not-run
if [ "$event_action" = "synchronize" ] || [ "$event_action" = "reopened" ]; then
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

files_json=$(mktemp)
trap 'rm -f "$files_json"' EXIT
gh api --paginate --slurp \
  "repos/$GITHUB_REPOSITORY/pulls/$pr_number/files?per_page=100" > "$files_json"
listed_changed_files=$(jq -er '[.[][]] | length' "$files_json")

if [ "$listed_changed_files" -ne "$declared_changed_files" ]; then
  echo "PR files API returned $listed_changed_files of $declared_changed_files changed files; refusing an incomplete gate decision." >&2
  false
fi

echo "Changed files in this PR:"
jq -r '.[][] | .filename | @json' "$files_json" | sed 's/^/  /'
echo

darwin_changed=$(jq -r --arg regex "$darwin_native_regex" \
  'any(.[][]; .filename | test($regex))' "$files_json")

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
  '.[][] | .filename | select(test($regex)) | @json' \
  "$files_json" | sed 's/^/  /'
echo

case "$event_action" in
  synchronize | reopened)
    if [ "$label_cleanup" = "failed" ]; then
      echo "::error title=Darwin E2E re-verification required::The stale '$verified_label' label could not be removed. Run 'just e2e-darwin' locally on Apple Silicon, remove or toggle the stale label, then re-add it to verify this head."
    else
      echo "::error title=Darwin E2E re-verification required::Run 'just e2e-darwin' locally on Apple Silicon, confirm it passes, then add the '$verified_label' label to verify this head."
    fi
    post_status failure 'New commits or reopening require Darwin re-verification.'
    trap - ERR
    exit 1
    ;;
  opened)
    post_status failure 'Darwin E2E verification is required for this head.'
    echo "::error title=Darwin E2E verification required::Run 'just e2e-darwin' locally on Apple Silicon, confirm it passes, then add the '$verified_label' label to this PR."
    trap - ERR
    exit 1
    ;;
  labeled | unlabeled)
    live_labels=$(gh api --paginate \
      "repos/$GITHUB_REPOSITORY/issues/$pr_number/labels?per_page=100" \
      --jq '.[].name')
    if grep -Fxq "$verified_label" <<< "$live_labels"; then
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
