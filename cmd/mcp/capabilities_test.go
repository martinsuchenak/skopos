package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

func mcpPost(t *testing.T, h http.Handler, p *auth.Principal, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode (%d): %v — %s", w.Code, err, w.Body.String())
	}
	return out
}

func listedTools(t *testing.T, h http.Handler, p *auth.Principal) []string {
	t.Helper()
	out := mcpPost(t, h, p, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	sort.Strings(names)
	return names
}

func agentPrincipal() *auth.Principal {
	p := &auth.Principal{KeyID: "agent", Name: "agent", Workspaces: map[string]struct{}{"ws-a": {}}, Capabilities: map[string]struct{}{}}
	for _, c := range auth.Presets["agent"] {
		p.Capabilities[c] = struct{}{}
	}
	return p
}

// Every registered tool names what it needs from a capped key; a new tool
// without an entry would be refused to every capped key.
func TestEveryToolHasCapabilityNeeds(t *testing.T) {
	for _, name := range listedTools(t, toolsE2E(t), nil) {
		if _, ok := toolNeeds[name]; !ok {
			t.Errorf("tool %s has no entry in toolNeeds (cmd/mcp/capabilities.go)", name)
		}
	}
}

// A capped key's tools/list shows only what it may call, and a call outside
// its capabilities is refused, while an allowed one works.
func TestCappedKeyTools(t *testing.T) {
	h := toolsE2E(t)
	all := listedTools(t, h, nil)
	agent := listedTools(t, h, agentPrincipal())
	if len(agent) == 0 || len(agent) >= len(all) {
		t.Fatalf("agent sees %d of %d tools", len(agent), len(all))
	}
	has := map[string]bool{}
	for _, n := range agent {
		has[n] = true
	}
	for _, want := range []string{"code_search", "blackboard_read", "blackboard_write", "plan_read", "plan_update_item", "skopos_context"} {
		if !has[want] {
			t.Errorf("agent must see %s (has %v)", want, agent)
		}
	}
	for _, hidden := range []string{"inbox_convert", "inbox_claim", "report_status", "skopos_workspaces"} {
		if has[hidden] {
			t.Errorf("agent must not see %s", hidden)
		}
	}

	call := func(tool string, args map[string]any) map[string]any {
		return mcpPost(t, h, agentPrincipal(), map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	}
	refused := call("report_status", map[string]any{"workspace_id": "ws-a", "status": "working", "agent_type": "x"})
	if raw, _ := json.Marshal(refused); !strings.Contains(string(raw), "capabilities do not allow report_status") {
		t.Fatalf("report_status must be refused: %s", raw)
	}
	allowed := call("blackboard_write", map[string]any{"workspace_id": "ws-a", "scope": "project", "entry_type": "finding", "title": "t", "content": "c", "author_agent_id": "a"})
	if raw, _ := json.Marshal(allowed); allowed["error"] != nil || strings.Contains(string(raw), `"isError":true`) {
		t.Fatalf("blackboard_write must work for the agent: %s", raw)
	}
}

// In lean mode a tool is reached through execute_tool; the guard on the
// tool's handler refuses it there too.
func TestCappedKeyExecuteTool(t *testing.T) {
	SetLeanToolset(true)
	defer SetLeanToolset(false)
	h := toolsE2E(t)
	out := mcpPost(t, h, agentPrincipal(), map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
		"name":      "execute_tool",
		"arguments": map[string]any{"name": "inbox_claim", "arguments": map[string]any{"id": "x", "workspace_id": "ws-a"}},
	}})
	if raw, _ := json.Marshal(out); !strings.Contains(string(raw), "capabilities do not allow inbox_claim") {
		t.Fatalf("execute_tool inbox_claim must be refused by the guard: %s", raw)
	}
}
