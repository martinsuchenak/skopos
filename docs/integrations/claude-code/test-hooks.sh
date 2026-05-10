#!/usr/bin/env bash
set -euo pipefail

HOOKS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/hooks.sh"
PASS=0; FAIL=0

check() {
  local desc="$1" code="$2"
  if [ "$code" -eq 0 ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc — exited $code"
    FAIL=$((FAIL + 1))
  fi
}

# These tests require a live skopos server on localhost:8080 to actually accept reports,
# but the hook must exit 0 even if the server is down (|| true).
# We test exit code only — visual confirmation via the dashboard.

echo '{"session_id":"test","tool_name":"Bash","tool_input":{}}' \
  | SKOPOS_SERVER_URL=http://localhost:9999 bash "$HOOKS" pre-tool
check "pre-tool exits 0 (server down)" $?

echo '{"session_id":"test","tool_name":"Read","tool_response":{}}' \
  | SKOPOS_SERVER_URL=http://localhost:9999 bash "$HOOKS" post-tool
check "post-tool exits 0 (server down)" $?

echo '{"session_id":"test","stop_hook_active":false}' \
  | SKOPOS_SERVER_URL=http://localhost:9999 bash "$HOOKS" stop
check "stop exits 0 (server down)" $?

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
