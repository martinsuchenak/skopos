package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/martinsuchenak/skopos/internal/auth"
	mcplib "github.com/paularlott/mcp"
)

// need is one area a tool requires, read or write.
type need struct {
	area  string
	write bool
}

// toolNeeds names what each tool requires from a capped key (key
// capabilities, tmp/agent-mcp-and-indexing — design.md §1). A tool missing
// here is refused to every capped key; TestEveryToolHasCapabilityNeeds keeps
// the map complete.
var toolNeeds = map[string][]need{
	"blackboard_read":   {{auth.AreaBlackboard, false}},
	"blackboard_write":  {{auth.AreaBlackboard, true}},
	"blackboard_delete": {{auth.AreaBlackboard, true}},

	"code_search":       {{auth.AreaCode, false}},
	"code_find":         {{auth.AreaCode, false}},
	"code_symbol":       {{auth.AreaCode, false}},
	"code_outline":      {{auth.AreaCode, false}},
	"code_callers":      {{auth.AreaCode, false}},
	"code_callees":      {{auth.AreaCode, false}},
	"code_call_tree":    {{auth.AreaCode, false}},
	"code_impact":       {{auth.AreaCode, false}},
	"code_dependencies": {{auth.AreaCode, false}},
	"code_cycles":       {{auth.AreaCode, false}},
	"code_dead":         {{auth.AreaCode, false}},
	"code_branch_diff":  {{auth.AreaCode, false}},
	"code_index_status": {{auth.AreaCode, false}},

	"inbox_list":     {{auth.AreaInbox, false}},
	"inbox_read":     {{auth.AreaInbox, false}},
	"inbox_create":   {{auth.AreaInbox, true}},
	"inbox_update":   {{auth.AreaInbox, true}},
	"inbox_claim":    {{auth.AreaInbox, true}},
	"inbox_complete": {{auth.AreaInbox, true}},
	"inbox_convert":  {{auth.AreaInbox, true}, {auth.AreaPlans, false}},
	"inbox_discard":  {{auth.AreaInbox, true}},
	"inbox_reopen":   {{auth.AreaInbox, true}},
	"inbox_restore":  {{auth.AreaInbox, true}},

	"plan_read":                   {{auth.AreaPlans, false}},
	"plan_create":                 {{auth.AreaPlans, true}},
	"plan_add_item":               {{auth.AreaPlans, true}},
	"plan_update_item":            {{auth.AreaPlans, true}},
	"plan_archive":                {{auth.AreaPlans, true}},
	"plan_add_dependency":         {{auth.AreaPlans, true}},
	"plan_remove_dependency":      {{auth.AreaPlans, true}},
	"plan_add_plan_dependency":    {{auth.AreaPlans, true}},
	"plan_remove_plan_dependency": {{auth.AreaPlans, true}},

	"report_status": {{auth.AreaStatus, true}},

	// skopos_context returns sessions, blackboard entries, plans and inbox
	// items: it needs a read of each.
	"skopos_context": {{auth.AreaStatus, false}, {auth.AreaBlackboard, false}, {auth.AreaPlans, false}, {auth.AreaInbox, false}},

	"skopos_workspaces":  {{auth.AreaWorkspaces, false}},
	"workspace_pipeline": {{auth.AreaWorkspaces, false}},
}

// toolAllowed reports whether the caller may use tool. Uncapped callers
// (root, internal, keys without capabilities) may use every tool.
func toolAllowed(ctx context.Context, tool string) bool {
	p := auth.PrincipalFromContext(ctx)
	if !p.Capped() {
		return true
	}
	needs, ok := toolNeeds[tool]
	if !ok {
		return false
	}
	for _, n := range needs {
		if !p.Allows(n.area, n.write) {
			return false
		}
	}
	return true
}

// guardTool wraps a tool's handler with the capability check, so a call
// through tools/call and one through lean mode's execute_tool are refused
// alike.
func guardTool(name string, handler mcplib.ToolHandler) mcplib.ToolHandler {
	return func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
		if !toolAllowed(ctx, name) {
			return nil, mcplib.NewToolErrorInvalidParams("this key's capabilities do not allow " + name)
		}
		return handler(ctx, req)
	}
}

// filterToolsList applies a capped key's capabilities at the protocol level:
// tools/list hides the tools it may not call, and tools/call of one is
// refused before the library validates its arguments (guardTool still
// covers lean mode's execute_tool). Everything else passes through.
func filterToolsList(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !auth.PrincipalFromContext(r.Context()).Capped() {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading request body", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var envelope struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			next.ServeHTTP(w, r)
			return
		}
		if envelope.Method == "tools/call" {
			name := envelope.Params.Name
			if name != mcplib.ToolSearchName && name != mcplib.ExecuteToolName && !toolAllowed(r.Context(), name) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": envelope.ID,
					"error": map[string]any{"code": -32602, "message": "this key's capabilities do not allow " + name},
				})
				return
			}
		}
		if envelope.Method != "tools/list" && envelope.Method != "initialize" {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		out := rec.Body.Bytes()
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			rewrite := filterToolsJSON
			if envelope.Method == "initialize" {
				rewrite = cappedInitializeJSON
			}
			if filtered, ok := rewrite(r.Context(), out); ok {
				out = filtered
			}
		}
		for k, v := range rec.Header() {
			if k == "Content-Length" {
				continue
			}
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(out)
	})
}

// filterToolsJSON drops the tools the caller may not use from a tools/list
// JSON-RPC response.
func filterToolsJSON(ctx context.Context, raw []byte) ([]byte, bool) {
	var resp map[string]any
	if json.Unmarshal(raw, &resp) != nil {
		return nil, false
	}
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if result == nil || tools == nil {
		return nil, false
	}
	kept := make([]any, 0, len(tools))
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		name, _ := tool["name"].(string)
		// The lean meta-tools stay: tool_search finds, execute_tool's
		// target is checked by guardTool.
		if name == mcplib.ToolSearchName || name == mcplib.ExecuteToolName || toolAllowed(ctx, name) {
			kept = append(kept, t)
		}
	}
	result["tools"] = kept
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, false
	}
	return out, true
}

// cappedInitializeJSON replaces the server instructions in a capped key's
// initialize answer: the full text tells agents to report status, create
// plans and work the inbox, which such a key may not do.
func cappedInitializeJSON(ctx context.Context, raw []byte) ([]byte, bool) {
	var resp map[string]any
	if json.Unmarshal(raw, &resp) != nil {
		return nil, false
	}
	result, _ := resp["result"].(map[string]any)
	if result == nil {
		return nil, false
	}
	result["instructions"] = cappedInstructions(ctx)
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, false
	}
	return out, true
}

// cappedInstructions describes only what the caller's key may use.
func cappedInstructions(ctx context.Context) string {
	can := func(tool string) bool { return toolAllowed(ctx, tool) }
	var b strings.Builder
	b.WriteString("You are connected to **skopos**, shared memory and a code index for AI agents, with a key limited to the tools listed in tools/list. Pass `workspace_id` to every call.\n")
	if can("skopos_context") {
		b.WriteString("\n- Start with `skopos_context` (`workspace_id`, `branch`): recorded knowledge, active plans and sessions in one call.")
	}
	if can("blackboard_read") {
		b.WriteString("\n- `blackboard_read` (`workspace_id`, `branch`): decisions, findings, bugs and debt other agents and people recorded.")
	}
	if can("blackboard_write") {
		b.WriteString("\n- `blackboard_write`: record a decision and why, a bug you found, a non-obvious constraint — scope `branch` (with `branch_name`) or `project`; type decision, finding, warning, bug or debt; always set `author_agent_id`. Be selective.")
	}
	if can("code_search") {
		b.WriteString("\n- Code index (pass `branch`): `code_search` for names, signatures and text (`semantic: true` for meaning), `code_symbol` for exact definitions, `code_callers` / `code_callees` / `code_impact` for the call graph, `code_outline` for a file's definitions, `code_index_status` for which branches are indexed. The index can lag the newest commits; confirm in the files.")
	}
	if can("plan_read") {
		b.WriteString("\n- `plan_read`: a plan and its items (pass `item_id` for one item).")
	}
	if can("plan_update_item") {
		b.WriteString("\n- `plan_update_item`: mark an item `in_progress` or `done` as you work on it.")
	}
	if can("inbox_read") {
		b.WriteString("\n- `inbox_list` / `inbox_read`: captured work items, read only.")
	}
	if can("report_status") {
		b.WriteString("\n- `report_status`: checkpoint what you are doing (never `stuck` or `orphaned`).")
	}
	return b.String()
}
