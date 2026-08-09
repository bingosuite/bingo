#!/usr/bin/env bash

set -euo pipefail

: "${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

readonly verified_label='darwin-e2e-verified'
readonly darwin_native_regex='^(internal/debugger/.*_darwin_.*|internal/debugger/trap_arm64\.go|test/integration/.*_darwin_.*_test\.go|entitlements\.plist)$'

event_action=$(jq -er '.action' "$GITHUB_EVENT_PATH")
pr_number=$(jq -er '.pull_request.number' "$GITHUB_EVENT_PATH")
head_repository=$(jq -er '.pull_request.head.repo.full_name // ""' "$GITHUB_EVENT_PATH")
event_label=$(jq -er '.label.name // ""' "$GITHUB_EVENT_PATH")

case "$event_action" in
  opened | synchronize | reopened | labeled | unlabeled) ;;
  *)
    echo "Unsupported pull_request action: $event_action" >&2
    exit 2
    ;;
esac

label_cleanup=not-run
if [ "$event_action" = "synchronize" ] || [ "$event_action" = "reopened" ]; then
  if output=$(gh pr edit "$pr_number" --repo "$GITHUB_REPOSITORY" \
    --remove-label "$verified_label" 2>&1); then
    label_cleanup=cleared
    echo "Cleared '$verified_label' if present; this head requires re-verification."
  else
    exit_code=$?
    label_cleanup=failed
    if [ "$head_repository" != "$GITHUB_REPOSITORY" ]; then
      echo "::warning title=Stale Darwin verification label not removed::The public-fork GITHUB_TOKEN is read-only, so '$verified_label' could not be removed (exit $exit_code). The gate will fail closed; remove or toggle the stale label, then re-add it to verify this head."
    else
      echo "::warning title=Stale Darwin verification label not removed::The API failed to remove '$verified_label' (exit $exit_code). The gate will fail closed; remove or toggle the stale label, then re-add it to verify this head."
    fi
    printf '%s\n' "$output" >&2
  fi
fi

changed_files=$(mktemp)
trap 'rm -f "$changed_files"' EXIT
gh pr diff "$pr_number" --repo "$GITHUB_REPOSITORY" --name-only > "$changed_files"

echo "Changed files in this PR:"
sed 's/^/  /' "$changed_files"
echo

if ! grep -Eq "$darwin_native_regex" "$changed_files"; then
  echo "No darwin-native code changed; gate not required."
  exit 0
fi

echo "Darwin-native (CI-unexecutable) files changed:"
grep -E "$darwin_native_regex" "$changed_files" | sed 's/^/  /'
echo

if [ "$event_action" = "synchronize" ] || [ "$event_action" = "reopened" ]; then
  if [ "$label_cleanup" = "failed" ]; then
    echo "::error title=Darwin E2E re-verification required::New commits and reopened pull requests invalidate prior Darwin verification. The stale '$verified_label' label could not be removed. Run 'just e2e-darwin' locally on Apple Silicon, remove or toggle the stale label, then re-add it to re-run verification for this head."
  else
    echo "::error title=Darwin E2E re-verification required::New commits and reopened pull requests invalidate prior Darwin verification. Run 'just e2e-darwin' locally on Apple Silicon, confirm it passes, then add the '$verified_label' label to re-run verification for this head."
  fi
  exit 1
fi

has_label=$(gh pr view "$pr_number" --repo "$GITHUB_REPOSITORY" --json labels \
  -q "[.labels[].name] | any(. == \"$verified_label\")")

if { [ "$event_action" = "labeled" ] || [ "$event_action" = "unlabeled" ]; } &&
  [ "$event_label" != "$verified_label" ]; then
  echo "::error title=Darwin E2E verification unchanged::The '$event_label' label event cannot verify this head. Remove or toggle '$verified_label', then re-add it after local verification."
  exit 1
fi

case "$has_label" in
  true)
    echo "'$verified_label' label present; darwin backend verified locally."
    ;;
  false)
    echo "::error title=Darwin E2E verification required::This PR changes darwin-native debugger code that cannot be executed on GitHub-hosted runners. Run 'just e2e-darwin' locally on Apple Silicon, confirm it passes, then add the '$verified_label' label to this PR."
    exit 1
    ;;
  *)
    echo "Unexpected label query result: $has_label" >&2
    exit 2
    ;;
esac
