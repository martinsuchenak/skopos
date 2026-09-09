package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/codeindex"
	"github.com/martinsuchenak/skopos/internal/codeindex/parse"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/status"
	_ "modernc.org/sqlite"
)

// toolsE2E drives real JSON-RPC tools/call requests through the MCP handler,
// exercising the dependency tools (add/remove, item/plan) end to end — the
// layer where registration-only tests previously provided no coverage.
func toolsE2E(t *testing.T) http.Handler {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := sqlDB.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("fk pragma: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return NewMCPHandler(
		status.NewService(status.NewStorage(sqlDB)),
		blackboard.NewService(blackboard.NewStorage(sqlDB)),
		plans.NewService(plans.NewStorage(sqlDB)),
		codeIndexServiceForTest(t, sqlDB),
	)
}

type rpcResult struct {
	Result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func callTool(t *testing.T, h http.Handler, sessionID string, id int, tool string, args map[string]any) rpcResult {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": args,
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var res struct {
		rpcResult
		ID int `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("%s: decode response (status %d): %v — body %s", tool, w.Code, err, w.Body.String())
	}
	if res.Error != nil {
		t.Fatalf("%s: json-rpc error %d: %s", tool, res.Error.Code, res.Error.Message)
	}
	return res.rpcResult
}

func callToolExpectError(t *testing.T, h http.Handler, sessionID string, id int, tool string, args map[string]any) struct {
	Code    int
	Message string
} {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": tool, "arguments": args},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var res struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&res); err != nil {
		t.Fatalf("%s: decode error response: %v — body %s", tool, err, w.Body.String())
	}
	if res.Error == nil {
		t.Fatalf("%s: expected a JSON-RPC error, got success: %s", tool, w.Body.String())
	}
	return struct {
		Code    int
		Message string
	}{res.Error.Code, res.Error.Message}
}

func codeIndexServiceForTest(t *testing.T, sqlDB *sql.DB) *codeindex.Service {
	store, err := codeindex.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("index store: %v", err)
	}
	t.Cleanup(store.Close)
	return codeindex.NewService(store)
}

func initialize(t *testing.T, h http.Handler) string {
	t.Helper()
	raw := []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: status %d body %s", w.Code, w.Body.String())
	}
	return w.Header().Get("Mcp-Session-Id")
}

// expectToolError asserts the tool call fails with the given JSON-RPC error
// code (-32602 invalid params for client mistakes, -32603 internal).
func expectToolError(t *testing.T, h http.Handler, sessionID string, id int, tool string, args map[string]any, wantCode int) {
	t.Helper()
	res := callToolExpectError(t, h, sessionID, id, tool, args)
	if res.Code != wantCode {
		t.Fatalf("%s: expected error code %d, got %d (%s)", tool, wantCode, res.Code, res.Message)
	}
}

func callText(t *testing.T, h http.Handler, sessionID string, id int, tool string, args map[string]any) string {
	t.Helper()
	res := callTool(t, h, sessionID, id, tool, args)
	if res.Result.IsError {
		t.Fatalf("%s: tool reported error: %s", tool, res.Result.Content)
	}
	if len(res.Result.Content) == 0 {
		t.Fatalf("%s: no content in result", tool)
	}
	return res.Result.Content[0].Text
}

func TestMCPDependencyToolsEndToEnd(t *testing.T) {
	h := toolsE2E(t)
	sessionID := initialize(t, h)
	id := 1

	// Two plans, two items each.
	planA := callText(t, h, sessionID, id, "plan_create", map[string]any{"name": "A", "author_agent_id": "t"})
	id++
	var a struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(planA), &a)

	planB := callText(t, h, sessionID, id, "plan_create", map[string]any{"name": "B", "author_agent_id": "t"})
	id++
	var b struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(planB), &b)

	itemA1 := callText(t, h, sessionID, id, "plan_add_item", map[string]any{"plan_id": a.ID, "title": "a1"})
	id++
	var ia1 struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(itemA1), &ia1)

	itemA2 := callText(t, h, sessionID, id, "plan_add_item", map[string]any{"plan_id": a.ID, "title": "a2"})
	id++
	var ia2 struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(itemA2), &ia2)

	itemB1 := callText(t, h, sessionID, id, "plan_add_item", map[string]any{"plan_id": b.ID, "title": "b1"})
	id++
	var ib1 struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(itemB1), &ib1)

	// plan_add_item_dependency auto-blocks the dependent item.
	callText(t, h, sessionID, id, "plan_add_dependency", map[string]any{"plan_id": a.ID, "item_id": ia2.ID, "depends_on_item_id": ia1.ID})
	id++
	detail := callText(t, h, sessionID, id, "plan_read", map[string]any{"plan_id": a.ID})
	id++
	if !bytes.Contains([]byte(detail), []byte(`"blocked"`)) {
		t.Fatalf("item should be blocked after adding dependency, got: %s", detail)
	}

	// Cycle is rejected as a client error (invalid params), not internal.
	expectToolError(t, h, sessionID, id, "plan_add_dependency",
		map[string]any{"plan_id": a.ID, "item_id": ia1.ID, "depends_on_item_id": ia2.ID}, -32602)
	id++

	// plan_remove_item_dependency auto-unblocks.
	callText(t, h, sessionID, id, "plan_remove_dependency", map[string]any{"plan_id": a.ID, "item_id": ia2.ID, "depends_on_item_id": ia1.ID})
	id++
	detail = callText(t, h, sessionID, id, "plan_read", map[string]any{"plan_id": a.ID})
	id++
	if bytes.Contains([]byte(detail), []byte(`"blocked"`)) {
		t.Fatalf("item should be unblocked after removing dependency, got: %s", detail)
	}

	// plan_add_plan_dependency blocks plan A; completing B would unblock.
	callText(t, h, sessionID, id, "plan_add_plan_dependency", map[string]any{"plan_id": a.ID, "depends_on_plan_id": b.ID})
	id++
	detail = callText(t, h, sessionID, id, "plan_read", map[string]any{"plan_id": a.ID})
	id++
	if !bytes.Contains([]byte(detail), []byte(`"blocked"`)) {
		t.Fatalf("plan A should be blocked by plan dependency, got: %s", detail)
	}

	// Cycle at plan level is rejected as a client error.
	expectToolError(t, h, sessionID, id, "plan_add_plan_dependency",
		map[string]any{"plan_id": b.ID, "depends_on_plan_id": a.ID}, -32602)
	id++

	// plan_remove_plan_dependency unblocks.
	callText(t, h, sessionID, id, "plan_remove_plan_dependency", map[string]any{"plan_id": a.ID, "depends_on_plan_id": b.ID})
	id++
	detail = callText(t, h, sessionID, id, "plan_read", map[string]any{"plan_id": a.ID})
	id++
	if bytes.Contains([]byte(detail), []byte(`"blocked"`)) {
		t.Fatalf("plan A should be unblocked after removing plan dependency, got: %s", detail)
	}

}

func TestMCPReadToolsAcceptAliases(t *testing.T) {
	h := toolsE2E(t)
	sessionID := initialize(t, h)

	created := callText(t, h, sessionID, 1, "plan_create", map[string]any{"name": "P", "author_agent_id": "t"})
	var p struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(created), &p)
	// plan_read accepts both plan_id and the legacy id param.
	if got := callText(t, h, sessionID, 2, "plan_read", map[string]any{"plan_id": p.ID}); !bytes.Contains([]byte(got), []byte(`"P"`)) {
		t.Fatalf("plan_read via plan_id failed: %s", got)
	}
	if got := callText(t, h, sessionID, 3, "plan_read", map[string]any{"id": p.ID}); !bytes.Contains([]byte(got), []byte(`"P"`)) {
		t.Fatalf("plan_read via legacy id failed: %s", got)
	}

	// blackboard_read search accepts author_agent_id as an alias of author.
	callText(t, h, sessionID, 4, "blackboard_write", map[string]any{
		"scope": "project", "entry_type": "finding", "title": "alias probe", "author_agent_id": "agent-x",
	})
	found := callText(t, h, sessionID, 5, "blackboard_read", map[string]any{"author_agent_id": "agent-x"})
	if !bytes.Contains([]byte(found), []byte("alias probe")) {
		t.Fatalf("blackboard_read via author_agent_id alias failed: %s", found)
	}
	if !bytes.Contains([]byte(found), []byte("agent-x")) {
		t.Fatalf("expected author in search result: %s", found)
	}
}

func mustOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

func TestMCPCodeIndexToolsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	repo := dir + "/repo"
	os.MkdirAll(repo, 0o755)
	os.WriteFile(repo+"/main.go", []byte(`package main

func LoadConfig() int { return helper() }

func helper() int { return 42 }
`), 0o644)

	store, err := codeindex.NewStore(dir + "/idx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	svc := codeindex.NewService(store)
	h := NewMCPHandler(
		status.NewService(status.NewStorage(mustOpenDB(t))),
		blackboard.NewService(blackboard.NewStorage(mustOpenDB(t))),
		plans.NewService(plans.NewStorage(mustOpenDB(t))),
		svc,
	)
	sessionID := initialize(t, h)

	// Build a local index for workspace "e2e-ws" and commit it.
	results, head, err := codeindex.Build(context.Background(), parse.NewExtractor(), repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := codeindex.CommitLocal(store, "e2e-ws", "main", "test", results, head); err != nil {
		t.Fatal(err)
	}

	// code_search finds LoadConfig via split-token query.
	got := callText(t, h, sessionID, 1, "code_search", map[string]any{"workspace_id": "e2e-ws", "q": "loadconfig"})
	if !strings.Contains(got, "LoadConfig") {
		t.Fatalf("code_search: %s", got)
	}
	// code_symbol returns the file:line.
	got = callText(t, h, sessionID, 2, "code_symbol", map[string]any{"workspace_id": "e2e-ws", "name": "helper"})
	if !strings.Contains(got, "main.go") {
		t.Fatalf("code_symbol: %s", got)
	}
	// code_callers: helper is called by LoadConfig.
	got = callText(t, h, sessionID, 3, "code_callers", map[string]any{"workspace_id": "e2e-ws", "name": "helper"})
	if !strings.Contains(got, "LoadConfig") {
		t.Fatalf("code_callers: %s", got)
	}
	// code_index_status lists the branch.
	got = callText(t, h, sessionID, 4, "code_index_status", map[string]any{"workspace_id": "e2e-ws"})
	if !strings.Contains(got, "main") {
		t.Fatalf("code_index_status: %s", got)
	}
}

func TestMCPCodeAnalysisToolsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	repo := dir + "/repo"
	os.MkdirAll(repo, 0o755)
	os.WriteFile(repo+"/app.go", []byte(`package main

func Root() { mid(); }

func mid() { leafA(); leafB() }

func leafA() { cyc1() }
func leafB() {}
func cyc1() { cyc2() }
func cyc2() { cyc1() }
func deadSym() {}
`), 0o644)

	store, err := codeindex.NewStore(dir + "/idx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	svc := codeindex.NewService(store)
	h := NewMCPHandler(
		status.NewService(status.NewStorage(mustOpenDB(t))),
		blackboard.NewService(blackboard.NewStorage(mustOpenDB(t))),
		plans.NewService(plans.NewStorage(mustOpenDB(t))),
		svc,
	)
	sessionID := initialize(t, h)

	// Index main, then a feature branch that adds a symbol.
	buildCommit := func(branch string) {
		results, head, err := codeindex.Build(context.Background(), parse.NewExtractor(), repo, branch)
		if err != nil {
			t.Fatal(err)
		}
		if err := codeindex.CommitLocal(store, "an-ws", branch, "test", results, head); err != nil {
			t.Fatal(err)
		}
	}
	buildCommit("main")
	os.WriteFile(repo+"/extra.go", []byte("package main\n\nfunc Extra() {}\n"), 0o644)
	buildCommit("feat/x")

	id := 1
	next := func() int { id++; return id }

	// outline lists a file's definitions in order.
	got := callText(t, h, sessionID, next(), "code_outline", map[string]any{"workspace_id": "an-ws", "path": "app.go"})
	if !strings.Contains(got, "Root") || !strings.Contains(got, "leafB") {
		t.Fatalf("outline: %s", got)
	}

	// callees from mid: leafA and leafB.
	got = callText(t, h, sessionID, next(), "code_callees", map[string]any{"workspace_id": "an-ws", "name": "mid"})
	if !strings.Contains(got, "leafA") || !strings.Contains(got, "leafB") {
		t.Fatalf("callees: %s", got)
	}

	// impact of leafA reaches mid then Root (depth 2).
	got = callText(t, h, sessionID, next(), "code_impact", map[string]any{"workspace_id": "an-ws", "name": "leafA", "depth": 3})
	if !strings.Contains(got, "mid") || !strings.Contains(got, "Root") {
		t.Fatalf("impact: %s", got)
	}

	// dead-code finds deadSym (no callers, not an entry-point prefix).
	got = callText(t, h, sessionID, next(), "code_dead", map[string]any{"workspace_id": "an-ws"})
	if !strings.Contains(got, "deadSym") {
		t.Fatalf("dead: %s", got)
	}

	// cycles finds cyc1 <-> cyc2.
	got = callText(t, h, sessionID, next(), "code_cycles", map[string]any{"workspace_id": "an-ws"})
	if !strings.Contains(got, "cyc1") || !strings.Contains(got, "cyc2") {
		t.Fatalf("cycles: %s", got)
	}

	// branch diff: feat/x adds extra.go / Extra vs main.
	got = callText(t, h, sessionID, next(), "code_branch_diff", map[string]any{"workspace_id": "an-ws", "branch": "feat/x"})
	if !strings.Contains(got, "Extra") || !strings.Contains(got, "extra.go") {
		t.Fatalf("branch diff: %s", got)
	}

	// call_tree from Root expands two levels.
	got = callText(t, h, sessionID, next(), "code_call_tree", map[string]any{"workspace_id": "an-ws", "name": "Root", "depth": 2})
	if !strings.Contains(got, "leafA") {
		t.Fatalf("call tree: %s", got)
	}
}
