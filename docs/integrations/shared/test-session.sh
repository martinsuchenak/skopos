#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/skopos-session.sh"

PASS=0; FAIL=0

check() {
  local desc="$1" got="$2" want="$3"
  if [ "$got" = "$want" ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc — got '$got', want '$want'"
    FAIL=$((FAIL + 1))
  fi
}

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

# Test 1: env var takes priority
export SKOPOS_SESSION_ID="env-session"
check "env var" "$(skopos_session_id "$TMPDIR")" "env-session"
unset SKOPOS_SESSION_ID

# Test 2: file fallback
echo "file-session" > "$TMPDIR/.skopos-session"
check "file fallback" "$(skopos_session_id "$TMPDIR")" "file-session"
rm "$TMPDIR/.skopos-session"

# Test 3: hash generation is stable (same dir+date returns same ID twice)
id1=$(skopos_session_id "$TMPDIR")
id2=$(skopos_session_id "$TMPDIR")
check "hash stability" "$id1" "$id2"

# Test 4: hash is 12 chars
check "hash length" "${#id1}" "12"

# Test 5: hash is persisted to .skopos-session file
check "file written after hash" "$(cat "$TMPDIR/.skopos-session")" "$id1"

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
