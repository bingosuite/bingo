#!/usr/bin/env bash

set -Eeuo pipefail

: "${GITHUB_EVENT_PATH:?GITHUB_EVENT_PATH is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

readonly verified_label='darwin-e2e-verified'
readonly status_context='Darwin E2E verified'
readonly required_base_ref='main'

# The extension set is the full list `go/build` compiles, not just the ones this
# repository happens to use today: an untagged cgo wrapper plus a
# `shim_darwin.sx` is enough to ship darwin-only machine code, and Go applies the
# implicit `_darwin`/`_arm64` suffix constraint to every one of these. Only
# `.go` blobs are content-scanned for explicit constraints, so any other native
# extension must be caught by name or it is never inspected at all — which is
# why plain `.go` is absent from `darwin_native_exts` but present in the
# suffix-constrained alternation.
readonly darwin_native_exts='swigcxx|swig|f90|for|syso|cpp|cxx|hpp|hxx|cc|hh|sx|s|S|c|h|m|mm|f|F'
readonly darwin_native_regex="^(internal/debugger/.*|test/integration/.*|justfile|entitlements\\.plist|go\\.(mod|sum)|(.*/)?[^/]*_(darwin|arm64)(_[^/.]+)*\\.(go|$darwin_native_exts)|(.*/)?[^/]*\\.($darwin_native_exts))\$"

event_action=$(jq -er '.action' "$GITHUB_EVENT_PATH")
pr_number=$(jq -er '.pull_request.number' "$GITHUB_EVENT_PATH")
head_sha=$(jq -er '.pull_request.head.sha | select(test("^[0-9a-fA-F]{40,64}$"))' "$GITHUB_EVENT_PATH")
base_sha=$(jq -er '.pull_request.base.sha | select(test("^[0-9a-fA-F]{40,64}$"))' "$GITHUB_EVENT_PATH")
base_ref=$(jq -er '.pull_request.base.ref // ""' "$GITHUB_EVENT_PATH")
head_repository=$(jq -er '.pull_request.head.repo.full_name // ""' "$GITHUB_EVENT_PATH")
event_label=$(jq -er '.label.name // ""' "$GITHUB_EVENT_PATH")
actor_login=$(jq -er '.sender.login // ""' "$GITHUB_EVENT_PATH")
actor_type=$(jq -er '.sender.type // ""' "$GITHUB_EVENT_PATH")
base_changed=$(jq -r '(.changes.base // null) != null' "$GITHUB_EVENT_PATH")
run_url="${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY/actions/runs/${GITHUB_RUN_ID:-0}"
final_status_posted=false
decision_file=${DARWIN_GATE_DECISION_FILE:-}

# --- ERR-trap guarded region -------------------------------------------------
#
# Everything below either defines or runs code that executes with the
# `fail_closed` ERR trap installed. `set -E` propagates that trap into
# command-substitution subshells, where a failure would publish a status the
# parent then publishes again — and where `final_status_posted` cannot travel
# back out. Every `$(...)` from this marker onwards therefore disarms the trap
# so the top-level shell stays the only decision point. A contract test enforces
# that statically, because the duplicate-publication symptom only reproduces on
# bash >= 4.4 and not on the macOS system bash.

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

# Commit statuses are SHA-global: the same head SHA can belong to several pull
# requests, and a queued event can reach the runner long after the pull request
# it described was force-pushed, retargeted or closed. Publishing success is the
# only irreversible direction, so every success re-reads the live pull request
# and refuses unless the event's generation is still the current one.
require_current_generation() {
  local live_ok live_summary

  gh api "repos/$GITHUB_REPOSITORY/pulls/$pr_number" > "$live_pr_json"

  live_summary=$(trap - ERR; jq -r '
    "state=\(.state // "?")"
    + " base=\(.base.ref // "?")@\(.base.sha // "?")"
    + " head=\(.head.sha // "?")"
    + " headRepo=\(.head.repo.full_name // "?")"
  ' "$live_pr_json")

  live_ok=$(trap - ERR; jq -r \
    --arg base_ref "$required_base_ref" \
    --arg base_sha "$base_sha" \
    --arg head_sha "$head_sha" \
    --arg head_repository "$head_repository" '
      ((.state // "") == "open")
      and ((.base.ref // "") == $base_ref)
      and ((.base.sha // "") == $base_sha)
      and ((.head.sha // "") == $head_sha)
      and ((.head.repo.full_name // "") == $head_repository)
    ' "$live_pr_json")

  if [ "$live_ok" != "true" ]; then
    post_status failure 'Pull request moved while the Darwin gate was evaluating.'
    echo "::error title=Darwin E2E verification is stale::This run described base '$base_ref'@$base_sha with head $head_sha, but the live pull request is now $live_summary. Re-run the gate against the current head before asserting verification."
    trap - ERR
    exit 1
  fi

  echo "Live pull request generation confirmed: $live_summary"
}

deny_label() {
  post_status failure 'Darwin verification requires an authorized maintainer.'
  echo "::error title=Darwin E2E verification not authorized::$1. Only a repository collaborator with write, maintain or admin permission may assert Darwin verification by adding the '$verified_label' label."
  trap - ERR
  exit 1
}

# The "Darwin E2E verified" status is an Actions status and is therefore NOT
# cryptographically attributable: any workflow in this repository runs as the
# same `github-actions[bot]` identity and can write the same context. The gate
# consequently derives its authorization from the human who performed the label
# action — the trusted event actor, checked against the repository permission
# API — and never from a previously published status.
require_authorized_labeler() {
  local permission role_name api_login

  case "$actor_type" in
    User) ;;
    *)
      deny_label "the '${actor_type:-unknown}' actor '$actor_login' is not a human collaborator"
      ;;
  esac

  # Validated before interpolation into an API path, and restricted to the shape
  # GitHub actually allows for user logins.
  case "$actor_login" in
    '' | *'[bot]' | -* | *- | *[!A-Za-z0-9-]*)
      deny_label "the actor login '$actor_login' is not a plain GitHub user login"
      ;;
  esac

  gh api "repos/$GITHUB_REPOSITORY/collaborators/$actor_login/permission" \
    > "$permission_json"

  api_login=$(trap - ERR; jq -r '.user.login // ""' "$permission_json")
  permission=$(trap - ERR; jq -r '.permission // ""' "$permission_json")
  role_name=$(trap - ERR; jq -r '.role_name // ""' "$permission_json")

  if [ "$api_login" != "$actor_login" ]; then
    deny_label "the permission API answered for '$api_login' rather than '$actor_login'"
  fi

  # The legacy `permission` field folds maintain into write and triage into
  # read, so admin/write is exactly "write, maintain or admin".
  case "$permission" in
    admin | maintain | write) ;;
    *)
      deny_label "'$actor_login' holds '${permission:-unknown}' permission (role '${role_name:-unknown}'), which is below write"
      ;;
  esac

  echo "Authorized labeler: $actor_login (permission '$permission', role '${role_name:-unknown}')."
}

# A tree response is only usable when it is complete AND every entry has the
# shape the diff below assumes. Anything else is an ambiguous answer about what
# changed, so it fails closed rather than producing a partial decision.
validate_tree_document() {
  jq -e '
    (.truncated == false)
    and ((.tree | type) == "array")
    and (
      .tree
      | all(
          ((.path | type) == "string")
          and ((.path | length) > 0)
          and ((.mode | type) == "string")
          and (.mode | test("^[0-7]{6}$"))
          and ((.type | type) == "string")
          and ((.type == "blob") or (.type == "tree") or (.type == "commit"))
          and (
            (.type == "tree")
            or (
              ((.sha | type) == "string")
              and (.sha | test("^[0-9a-fA-F]{40,64}$"))
            )
          )
        )
    )
  ' "$1" >/dev/null
}

# Go honours a single leading UTF-8 byte-order mark, so an anchored build-tag
# scan must look past it; a file that starts with any other BOM cannot be read
# as Go source at all and its first line is hidden from this scan either way.
#
# Exit codes: 0 constraint found, 1 no constraint, 2 undecidable encoding,
# 3 the blob could not be inspected.
scan_blob_for_build_constraint() {
  local raw=$1
  local target=$raw
  local b0='' b1='' b2='' rest=''

  od -An -tx1 -N4 "$raw" > "$bom_probe" || return 3
  read -r b0 b1 b2 rest < "$bom_probe" || true

  case "$b0$b1" in
    fffe | feff)
      return 2
      ;;
  esac

  if [ "$b0$b1$b2" = "efbbbf" ]; then
    tail -c +4 "$raw" > "$blob_scan" || return 3
    target=$blob_scan
  fi

  if grep -Eq '^[[:space:]]*//[[:space:]]*(go:build|\+build)[[:space:]]' \
    "$target"; then
    return 0
  fi

  return 1
}

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

# The workflow trigger is already restricted to `main`, but statuses are
# SHA-global and this script is fetched by SHA, so it re-asserts the scope
# itself rather than trusting the trigger it was invoked from.
if [ "$base_ref" != "$required_base_ref" ]; then
  echo "This gate only governs pull requests targeting '$required_base_ref'; refusing to decide for base '$base_ref'." >&2
  false
fi

base_tree_json=$(trap - ERR; mktemp)
head_tree_json=$(trap - ERR; mktemp)
changed_paths_json=$(trap - ERR; mktemp)
go_blobs_tsv=$(trap - ERR; mktemp)
blob_json=$(trap - ERR; mktemp)
blob_raw=$(trap - ERR; mktemp)
blob_scan=$(trap - ERR; mktemp)
bom_probe=$(trap - ERR; mktemp)
live_pr_json=$(trap - ERR; mktemp)
permission_json=$(trap - ERR; mktemp)
trap 'rm -f "$base_tree_json" "$head_tree_json" "$changed_paths_json" "$go_blobs_tsv" "$blob_json" "$blob_raw" "$blob_scan" "$bom_probe" "$live_pr_json" "$permission_json"' EXIT

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

if ! validate_tree_document "$base_tree_json" ||
  ! validate_tree_document "$head_tree_json"; then
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
            type: $entry.type,
            regular: (
              $entry.type == "blob"
              and ($entry.mode == "100644" or $entry.mode == "100755")
            )
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

# A symlink, a submodule pointer or any other non-regular entry only exposes its
# link text to the tree scan, so a `.go` symlink can name a Darwin-constrained
# payload this gate would otherwise never read. Every such change is gated.
irregular_changed=$(trap - ERR; jq -r '
  any(.[];
    ((.before != null) and (.before.regular != true))
    or ((.after != null) and (.after.regular != true)))
' "$changed_paths_json")

if [ "$irregular_changed" = "true" ]; then
  echo "Changed entries that are not regular files (symlink, submodule or unexpected mode):"
  jq -r '
    .[]
    | select(
        ((.before != null) and (.before.regular != true))
        or ((.after != null) and (.after.regular != true))
      )
    | "\(.path | @json) before=\(.before.mode // "-")/\(.before.type // "-") after=\(.after.mode // "-")/\(.after.type // "-")"
  ' "$changed_paths_json" | sed 's/^/  /'
  echo
fi

darwin_changed=$(trap - ERR; jq -r --arg regex "$darwin_native_regex" \
  'any(.[].path; test($regex) or test("[[:cntrl:]]"))' \
  "$changed_paths_json")

if [ "$darwin_changed" = "false" ] && [ "$irregular_changed" = "true" ]; then
  darwin_changed=true
fi

constraint_path=
if [ "$darwin_changed" = "false" ]; then
  jq -r '
    .[]
    | select(.path | endswith(".go"))
    | .path as $path
    | (.before, .after)
    | select(. != null and .regular == true)
    | "\(.sha)\t\($path)"
  ' "$changed_paths_json" | sort -u > "$go_blobs_tsv"

  while IFS=$'\t' read -r blob_sha blob_path; do
    [ -n "$blob_sha" ] || continue

    gh api "repos/$GITHUB_REPOSITORY/git/blobs/$blob_sha" > "$blob_json"
    blob_decodable=$(trap - ERR; jq -r '
      ((.encoding // "") == "base64") and ((.content | type) == "string")
    ' "$blob_json")
    if [ "$blob_decodable" != "true" ]; then
      echo "Blob $blob_sha ($blob_path) was not returned as decodable content; refusing an incomplete gate decision." >&2
      false
    fi
    jq -r '.content' "$blob_json" | base64 -d > "$blob_raw"

    scan_status=0
    scan_blob_for_build_constraint "$blob_raw" || scan_status=$?
    case "$scan_status" in
      0)
        darwin_changed=true
        constraint_path=$blob_path
        break
        ;;
      1) ;;
      2)
        darwin_changed=true
        constraint_path=$blob_path
        echo "::warning title=Undecidable Go source encoding::$blob_path starts with a non-UTF-8 byte-order mark, so its build constraints cannot be read; gating conservatively."
        break
        ;;
      *)
        echo "Blob $blob_sha ($blob_path) could not be inspected for build constraints; refusing an incomplete gate decision." >&2
        false
        ;;
    esac
  done < "$go_blobs_tsv"
fi

if [ "$darwin_changed" = "false" ]; then
  require_current_generation
  post_status success 'No Darwin-native files changed.'
  echo "No darwin-native code changed; gate not required."
  exit 0
fi

if [ "$darwin_changed" != "true" ]; then
  echo "Unexpected Darwin path decision: $darwin_changed" >&2
  false
fi

echo "Darwin-native (CI-unexecutable) changes detected:"
jq -r --arg regex "$darwin_native_regex" \
  '.[].path | select(test($regex) or test("[[:cntrl:]]")) | @json' \
  "$changed_paths_json" | sed 's/^/  /'
if [ "$irregular_changed" = "true" ]; then
  echo "  (plus non-regular tree entries listed above)"
fi
if [ -n "$constraint_path" ]; then
  printf '  %s (explicit Go build constraint)\n' "$constraint_path"
fi
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
    # Only a real, authorized `labeled` action may assert verification. A label
    # removal is never restorative even when the label is live again by the time
    # this run reads it: the re-add produced its own `labeled` event, and that
    # event is the only one allowed to publish success.
    if [ "$event_action" = "unlabeled" ]; then
      post_status failure 'Darwin verification was withdrawn for this head.'
      echo "::error title=Darwin E2E verification withdrawn::Removing '$verified_label' invalidates verification for this head. Re-run the e2e-darwin recipe on Apple Silicon and add the label again; only a fresh, authorized label addition can restore this status."
      trap - ERR
      exit 1
    fi

    live_labels=$(trap - ERR; gh api --paginate \
      "repos/$GITHUB_REPOSITORY/issues/$pr_number/labels?per_page=100" \
      --jq '.[].name')
    if ! grep -Fxq "$verified_label" <<< "$live_labels"; then
      post_status failure 'Darwin verification label is not currently present.'
      echo "::error title=Darwin E2E verification required::The '$verified_label' label is not currently present on this PR."
      trap - ERR
      exit 1
    fi

    require_authorized_labeler
    require_current_generation
    post_status success 'Darwin E2E verified for this head SHA.'
    echo "'$verified_label' label added by $actor_login; darwin backend verified locally for $head_sha."
    exit 0
    ;;
esac
