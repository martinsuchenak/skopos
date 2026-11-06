#!/bin/sh
# Print the CHANGELOG.md section for a version, without its heading.
#
# Usage: scripts/changelog-section.sh <version> [changelog-file]
#   version: "v0.0.21" or "0.0.21"
#
# Exits 1 if the version has no section or the section is empty. Used by
# `task release` (to refuse tagging an undocumented version) and by the
# release workflow (to publish the section as the GitHub release notes).
set -eu

if [ $# -lt 1 ]; then
  echo "usage: $0 <version> [changelog-file]" >&2
  exit 2
fi

version="${1#v}"
file="${2:-CHANGELOG.md}"

section=$(awk -v v="$version" '
  /^## \[/ {
    if (found) exit
    if (index($0, "## [" v "]") == 1) { found = 1; next }
  }
  /^\[[^]]+\]: / { if (found) exit }
  found { print }
' "$file")

# Trim leading and trailing blank lines.
section=$(printf '%s\n' "$section" | sed -e '/./,$!d' | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}')

if [ -z "$section" ]; then
  echo "error: no changelog entry for $version in $file (add a '## [$version] — YYYY-MM-DD' section)" >&2
  exit 1
fi

printf '%s\n' "$section"
