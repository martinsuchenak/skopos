package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/worker"
	mcplib "github.com/paularlott/mcp"
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

// In lean mode a tool is reached through execute_tool; it is refused there
// too, before the library validates its arguments (wrong ones must not
// describe the forbidden tool's schema).
func TestCappedKeyExecuteTool(t *testing.T) {
	SetLeanToolset(true)
	defer SetLeanToolset(false)
	h := toolsE2E(t)
	for _, args := range []map[string]any{
		{"item_id": "x", "workspace_id": "ws-a"},
		{"id": "x"},
	} {
		out := mcpPost(t, h, agentPrincipal(), map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
			"name":      "execute_tool",
			"arguments": map[string]any{"name": "inbox_claim", "parameters": args},
		}})
		if raw, _ := json.Marshal(out); !strings.Contains(string(raw), "capabilities do not allow inbox_claim") {
			t.Fatalf("execute_tool inbox_claim %v must be refused: %s", args, raw)
		}
	}
}

// The guard on the handler still refuses when the request reaches it.
func TestGuardToolRefusesCappedKey(t *testing.T) {
	called := false
	h := guardTool("inbox_claim", func(context.Context, *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
		called = true
		return nil, nil
	})
	ctx := auth.WithPrincipal(context.Background(), agentPrincipal())
	if _, err := h(ctx, nil); err == nil || called {
		t.Fatalf("guardTool must refuse inbox_claim for a capped key: err=%v called=%v", err, called)
	}
}

// A capped key's initialize answer carries instructions about its own
// tools only; an uncapped one gets the full text.
func TestCappedKeyInstructions(t *testing.T) {
	h := toolsE2E(t)
	init := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "1"},
	}}
	instructions := func(p *auth.Principal) string {
		out := mcpPost(t, h, p, init)
		result, _ := out["result"].(map[string]any)
		s, _ := result["instructions"].(string)
		return s
	}
	full := instructions(nil)
	agent := instructions(agentPrincipal())
	if !strings.Contains(full, "report_status") || !strings.Contains(full, "inbox_convert") {
		t.Fatalf("uncapped keys keep the full instructions: %s", full)
	}
	for _, want := range []string{"code_search", "blackboard_write", "plan_update_item", "skopos_context"} {
		if !strings.Contains(agent, want) {
			t.Errorf("agent instructions miss %s: %s", want, agent)
		}
	}
	for _, not := range []string{"report_status", "inbox_convert", "plan_create", "inbox_claim"} {
		if strings.Contains(agent, not) {
			t.Errorf("agent instructions mention %s: %s", not, agent)
		}
	}
}

// A capped key may not batch: a batch would slip past the per-request
// checks.
func TestCappedKeyBatchRefused(t *testing.T) {
	h := toolsE2E(t)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}]`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithPrincipal(req.Context(), agentPrincipal()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "batch requests are not accepted") {
		t.Fatalf("batch: %s", w.Body.String())
	}
}

func runPrincipal(stage string) *auth.Principal {
	p := agentPrincipal()
	p.KeyID, p.Name = "run:r1", "run r1"
	p.Run = &auth.RunIdentity{ID: "r1", WorkspaceID: "ws-a", ItemID: "i1", PlanID: "p1", Stage: stage, Runner: "claude", WorkerID: "w1"}
	return p
}

// A run's agent token sees and may call only its stage's tools: what the
// run preset's capabilities would also allow (creating or archiving plans,
// reading the inbox) is hidden and refused, and only an implement run may
// mark plan steps.
func TestRunPrincipalTools(t *testing.T) {
	h := toolsE2E(t)
	for _, stage := range []string{"plan", "implement", "review"} {
		got := map[string]bool{}
		for _, n := range listedTools(t, h, runPrincipal(stage)) {
			got[n] = true
		}
		for name := range runTools {
			if !got[name] {
				t.Errorf("%s run must see %s", stage, name)
			}
		}
		if got["plan_update_item"] != (stage == "implement") {
			t.Errorf("%s run: plan_update_item listed = %v", stage, got["plan_update_item"])
		}
		for _, hidden := range []string{"plan_create", "plan_add_item", "plan_archive", "inbox_list", "inbox_read", "inbox_questions", "blackboard_delete", "report_status"} {
			if got[hidden] {
				t.Errorf("%s run must not see %s", stage, hidden)
			}
		}
	}
	call := func(p *auth.Principal, tool string, args map[string]any) string {
		out := mcpPost(t, h, p, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
		raw, _ := json.Marshal(out)
		return string(raw)
	}
	if raw := call(runPrincipal("implement"), "plan_create", map[string]any{"workspace_id": "ws-a", "name": "x", "author_agent_id": "a"}); !strings.Contains(raw, "do not allow plan_create") {
		t.Fatalf("plan_create must be refused for a run: %s", raw)
	}
	if raw := call(runPrincipal("review"), "plan_update_item", map[string]any{"plan_id": "p1", "item_id": "x", "status": "done"}); !strings.Contains(raw, "do not allow plan_update_item") {
		t.Fatalf("a review run must not mark plan steps: %s", raw)
	}
	if raw := call(runPrincipal("review"), "blackboard_write", map[string]any{"workspace_id": "ws-a", "scope": "project", "entry_type": "finding", "title": "t", "content": "c", "author_agent_id": "a"}); strings.Contains(raw, `"isError":true`) || strings.Contains(raw, `"error"`) {
		t.Fatalf("a run records findings in its workspace: %s", raw)
	}
	for tool, args := range map[string]map[string]any{
		"blackboard_read": {"workspace_id": "ws-b"},
		"code_search":     {"workspace_id": "ws-b", "q": "x"},
		"skopos_context":  {"workspace_id": "ws-b"},
	} {
		if raw := call(runPrincipal("review"), tool, args); !strings.Contains(raw, "out of scope") {
			t.Fatalf("%s on another workspace must be out of scope: %s", tool, raw)
		}
	}
	// In lean mode the same refusal holds through execute_tool.
	SetLeanToolset(true)
	defer SetLeanToolset(false)
	lean := toolsE2E(t)
	out := mcpPost(t, lean, runPrincipal("implement"), map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
		"name": "execute_tool", "arguments": map[string]any{"name": "plan_archive", "parameters": map[string]any{"plan_id": "p1"}},
	}})
	if raw, _ := json.Marshal(out); !strings.Contains(string(raw), "do not allow plan_archive") {
		t.Fatalf("execute_tool plan_archive must be refused for a run: %s", raw)
	}
}

// The tools a run's token may call are exactly the ones the worker
// allowlists for the agent CLI, per stage.
func TestRunToolsMatchWorker(t *testing.T) {
	for _, stage := range []string{"plan", "implement", "review"} {
		want := map[string]bool{}
		for _, n := range worker.AgentToolNames(stage) {
			want[n] = true
		}
		got := map[string]bool{}
		for n := range runTools {
			got[n] = true
		}
		if stage == "implement" {
			for n := range runImplementTools {
				got[n] = true
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: server allows %v, the worker allowlists %v", stage, got, want)
		}
		for n := range got {
			if _, ok := toolNeeds[n]; !ok {
				t.Errorf("run tool %s has no toolNeeds entry", n)
			}
		}
	}
}

// A request whose params don't parse must not slip past the filter: a run
// (or any capped key) still gets only its own tools from tools/list, the
// capped instructions from initialize, and a refusal for a tools/call it
// cannot classify. A body that is no JSON-RPC object is refused.
func TestCappedKeyMalformedEnvelope(t *testing.T) {
	h := toolsE2E(t)
	post := func(body string) string {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req = req.WithContext(auth.WithPrincipal(req.Context(), runPrincipal("review")))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Body.String()
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"name":5}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":"x"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"arguments":7}}`,
	} {
		out := post(body)
		for _, hidden := range []string{"plan_create", "plan_archive", "inbox_list", "report_status", "blackboard_delete"} {
			if strings.Contains(out, `"`+hidden+`"`) {
				t.Errorf("%s leaked %s: %.200s", body, hidden, out)
			}
		}
	}
	init := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"name":5,"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	if strings.Contains(init, "inbox_claim") || strings.Contains(init, "plan_create") || !strings.Contains(init, "limited to the tools listed") {
		t.Errorf("initialize with odd params must still answer the capped instructions: %.300s", init)
	}
	if out := post(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":5}}`); !strings.Contains(out, "must name the tool") {
		t.Errorf("an unclassifiable tools/call must be refused: %.200s", out)
	}
	for _, body := range []string{`"tools/list"`, `7`, `{not json`} {
		if out := post(body); !strings.Contains(out, "not a JSON-RPC object") {
			t.Errorf("%s must be refused: %.200s", body, out)
		}
	}
}
