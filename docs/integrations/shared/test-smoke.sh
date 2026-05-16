#!/usr/bin/env bash
# End-to-end smoke test for Skopos integrations.
# Requires: skopos server running on localhost:8080 (MCP on :9000)
# Usage: SKOPOS_API_KEY=your-key bash docs/integrations/shared/test-smoke.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/skopos-session.sh"

BASE_URL="${SKOPOS_SERVER_URL:-http://localhost:8080}"
API_KEY="${SKOPOS_API_KEY:-}"
SESSION_ID="smoke-test-$(date +%s)"
PASS=0; FAIL=0

check() {
  local desc="$1" code="$2"
  if [ "$code" -eq 0 ]; then
    echo "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $desc"
    FAIL=$((FAIL + 1))
  fi
}

echo "==> Smoke test against $BASE_URL"

# 1. CLI report
echo "--- Layer 1: CLI"
skopos report \
  --server-url "$BASE_URL" \
  ${API_KEY:+--api-key "$API_KEY"} \
  --agent-id "smoke-test-agent" \
  --agent-type "claude-code" \
  --workspace "$PWD" \
  --session-id "$SESSION_ID" \
  --status running \
  --message "smoke test via CLI"
check "CLI report accepted" $?

# 2. REST report
echo "--- Layer 2: REST"
HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
  -X POST "$BASE_URL/api/reports" \
  -H "Content-Type: application/json" \
  ${API_KEY:+-H "X-API-Key: $API_KEY"} \
  -d "{\"agent_id\":\"smoke-rest\",\"agent_type\":\"claude-code\",\"workspace\":\"$PWD\",\"session_id\":\"$SESSION_ID\",\"status\":\"running\",\"message\":\"smoke test via REST\"}")
[ "$HTTP_STATUS" = "200" ] || [ "$HTTP_STATUS" = "201" ]
check "REST report accepted (HTTP $HTTP_STATUS)" $?

# 3. Session appears in API
echo "--- Verification"
SESSIONS=$(curl -s "$BASE_URL/api/sessions")
echo "$SESSIONS" | grep -q "$SESSION_ID"
check "Session visible in GET /api/sessions" $?

echo ""
echo "--- Plans"
PLAN_HTTP=$(curl -s -o /tmp/skopos-plan.json -w "%{http_code}" \
  -X POST "$BASE_URL/api/plans" \
  -H "Content-Type: application/json" \
  ${API_KEY:+-H "X-API-Key: $API_KEY"} \
  -d "{\"name\":\"Smoke test plan\",\"author_agent_id\":\"smoke-test-agent\"}")
[ "$PLAN_HTTP" = "201" ]
check "Plan created (HTTP $PLAN_HTTP)" $?

PLAN_ID=$(jq -r '.id' /tmp/skopos-plan.json)

ITEM_HTTP=$(curl -s -o /tmp/skopos-item.json -w "%{http_code}" \
  -X POST "$BASE_URL/api/plans/$PLAN_ID/items" \
  -H "Content-Type: application/json" \
  ${API_KEY:+-H "X-API-Key: $API_KEY"} \
  -d "{\"title\":\"Test item\"}")
[ "$ITEM_HTTP" = "201" ]
check "Plan item added (HTTP $ITEM_HTTP)" $?

ITEM_ID=$(jq -r '.id' /tmp/skopos-item.json)

PATCH_HTTP=$(curl -s -o /dev/null -w "%{http_code}" \
  -X PATCH "$BASE_URL/api/plans/$PLAN_ID/items/$ITEM_ID" \
  -H "Content-Type: application/json" \
  ${API_KEY:+-H "X-API-Key: $API_KEY"} \
  -d '{"status":"done"}')
[ "$PATCH_HTTP" = "204" ]
check "Plan item updated to done (HTTP $PATCH_HTTP)" $?

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
