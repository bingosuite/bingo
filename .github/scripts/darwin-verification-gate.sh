#!/usr/bin/env bash

set -euo pipefail

: "${EVENT_ACTION:?EVENT_ACTION is required}"
: "${CHANGED_FILES:?CHANGED_FILES is required}"

VERIFIED_LABEL="${VERIFIED_LABEL:-darwin-e2e-verified}"
HAS_VERIFIED_LABEL="${HAS_VERIFIED_LABEL:-}"
LABEL_CLEANUP="${LABEL_CLEANUP:-not-run}"

# These paths require runtime verification on real darwin/arm64 hardware.
readonly DARWIN_NATIVE_REGEX="${DARWIN_NATIVE_REGEX:-^(internal/debugger/.*_darwin_.*|internal/debugger/trap_arm64\.go|test/integration/.*_darwin_.*_test\.go|entitlements\.plist)$}"

if [ ! -r "$CHANGED_FILES" ]; then
  echo "Changed-files list is not readable: $CHANGED_FILES" >&2
  exit 2
fi

echo "Changed files in this PR:"
sed 's/^/  /' "$CHANGED_FILES"
echo

if ! grep -Eq "$DARWIN_NATIVE_REGEX" "$CHANGED_FILES"; then
  echo "No darwin-native code changed; gate not required."
  exit 0
fi

echo "Darwin-native (CI-unexecutable) files changed:"
grep -E "$DARWIN_NATIVE_REGEX" "$CHANGED_FILES" | sed 's/^/  /'
echo

if [ "$EVENT_ACTION" = "synchronize" ]; then
  if [ "$LABEL_CLEANUP" = "failed" ]; then
    echo "::error title=Darwin E2E re-verification required::New commits always invalidate prior Darwin verification. The stale '$VERIFIED_LABEL' label could not be removed. Run 'just e2e-darwin' locally on Apple Silicon, remove or toggle the stale label, then re-add it to re-run verification for this head."
  else
    echo "::error title=Darwin E2E re-verification required::New commits always invalidate prior Darwin verification. Run 'just e2e-darwin' locally on Apple Silicon, confirm it passes, then add the '$VERIFIED_LABEL' label to re-run verification for this head."
  fi
  exit 1
fi

case "$HAS_VERIFIED_LABEL" in
  true)
    echo "'$VERIFIED_LABEL' label present; darwin backend verified locally."
    ;;
  false)
    echo "::error title=Darwin E2E verification required::This PR changes darwin-native debugger code that cannot be executed on GitHub-hosted runners. Run 'just e2e-darwin' locally on Apple Silicon, confirm it passes, then add the '$VERIFIED_LABEL' label to this PR."
    exit 1
    ;;
  *)
    echo "HAS_VERIFIED_LABEL must be true or false for '$EVENT_ACTION' events" >&2
    exit 2
    ;;
esac
