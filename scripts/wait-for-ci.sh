#!/bin/sh
# Wait for the CI workflow's run on a commit and fail unless it succeeded.
#
# Usage: scripts/wait-for-ci.sh [commit]   (default: HEAD)
#
# Used by `task release`, so a tag only goes on a commit CI has passed,
# without rerunning the whole suite locally.
set -eu

sha=$(git rev-parse "${1:-HEAD}")

# The run appears a few seconds after the push.
run=""
for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
  run=$(gh run list --workflow ci.yml --commit "$sha" --limit 1 --json databaseId --jq '.[0].databaseId // empty')
  [ -n "$run" ] && break
  sleep 5
done
if [ -z "$run" ]; then
  echo "No CI run for $sha; push it first (git push origin HEAD)." >&2
  exit 1
fi

echo "Waiting for CI run $run on $sha..."
if ! gh run watch "$run" --exit-status --interval 15 >/dev/null; then
  echo "CI failed for $sha: $(gh run view "$run" --json url --jq .url)" >&2
  exit 1
fi
echo "CI passed for $sha."
