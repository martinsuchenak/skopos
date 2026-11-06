package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A client on the 2025-11-25 protocol (mcp-remote, Claude Code) sends the
// per-request headers MCP-Protocol-Version, Mcp-Method and Mcp-Name with
// every call. paularlott/mcp 0.33 took any request carrying Mcp-Method for
// the 2026-07-25 "Modern" protocol and refused these with "Header mismatch:
// MCP-Protocol-Version header value \"2025-11-25\" does not match body value
// \"\"" — every tool call from those clients failed (skopos 0.21.1). These
// requests must keep working whatever the library version.
func TestLegacyClientHeadersAreAccepted(t *testing.T) {
	h := toolsE2E(t)
	for _, tc := range []struct {
		method, name string
		params       map[string]any
	}{
		{"tools/list", "", map[string]any{}},
		{"tools/call", "blackboard_read", map[string]any{"name": "blackboard_read", "arguments": map[string]any{"workspace_id": "ws-a"}}},
	} {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": tc.method, "params": tc.params})
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		req.Header.Set("Mcp-Method", tc.method)
		if tc.name != "" {
			req.Header.Set("Mcp-Name", tc.name)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		body := w.Body.String()
		if w.Code != http.StatusOK || strings.Contains(body, `"error"`) {
			t.Errorf("%s with 2025-11-25 client headers: status %d, body %s", tc.method, w.Code, body)
		}
	}
}
