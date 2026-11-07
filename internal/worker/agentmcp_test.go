package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func whoamiServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/whoami" || r.Header.Get("Authorization") != "Bearer sk_agent" {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The worker refuses an agent key that would give an agent more than MCP.
func TestCheckAgentKey(t *testing.T) {
	ok := `{"root":false,"key":{"approver":false,"capabilities":["blackboard:write","code:read","mcp","plans:write"]}}`
	if err := CheckAgentKey(context.Background(), whoamiServer(t, ok), "sk_agent"); err != nil {
		t.Fatalf("agent preset key: %v", err)
	}
	for name, body := range map[string]string{
		"root":        `{"root":true}`,
		"full access": `{"root":false,"key":{"approver":false,"capabilities":null}}`,
		"approver":    `{"root":false,"key":{"approver":true,"capabilities":["mcp","code:read"]}}`,
		"rest":        `{"root":false,"key":{"approver":false,"capabilities":["mcp","rest","code:read"]}}`,
		"no mcp":      `{"root":false,"key":{"approver":false,"capabilities":["code:read"]}}`,
		"no code":     `{"root":false,"key":{"approver":false,"capabilities":["mcp","blackboard:read"]}}`,
	} {
		if err := CheckAgentKey(context.Background(), whoamiServer(t, body), "sk_agent"); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
}

// The run MCP config holds the operator's servers plus skopos with the
// agent key, readable only by the worker; an operator "skopos" server
// clashes and is refused.
func TestWriteAgentMCPConfig(t *testing.T) {
	dir := t.TempDir()
	op := filepath.Join(dir, "op.json")
	_ = os.WriteFile(op, []byte(`{"mcpServers":{"llmrouter":{"type":"http","url":"https://llm.example/mcp"}}}`), 0o600)
	path, err := WriteAgentMCPConfig(filepath.Join(dir, "state"), "https://skopos.example/", "sk_agent", op)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	sk := cfg.MCPServers["skopos"]
	if sk.URL != "https://skopos.example/mcp" || sk.Headers["Authorization"] != "Bearer sk_agent" || cfg.MCPServers["llmrouter"].URL == "" {
		t.Fatalf("config: %s", raw)
	}
	// opencode translates the same file (remote server with its header).
	if content, err := opencodeMCPContent(path); err != nil || !strings.Contains(content, `"skopos":{"enabled":true,"headers":{"Authorization":"Bearer sk_agent"}`) {
		t.Fatalf("opencode translation: %s %v", content, err)
	}
	_ = os.WriteFile(op, []byte(`{"mcpServers":{"skopos":{"type":"http","url":"https://other/mcp"}}}`), 0o600)
	if _, err := WriteAgentMCPConfig(filepath.Join(dir, "state"), "https://skopos.example", "sk_agent", op); err == nil {
		t.Fatal("an operator skopos server must be refused")
	}
}

// With an agent key, claude and opencode runs get the run MCP config and
// their stage's skopos tools (implement also plan_update_item); codex runs
// get neither, and without an agent key nothing changes.
func TestAgentRunGetsSkoposTools(t *testing.T) {
	w := New(&Config{StateDir: t.TempDir(), MCPConfig: "/op.json"}, nil, slog.New(slog.DiscardHandler))
	run := &Run{ID: "r1", WorkspaceID: "ws"}
	plain := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, run, "plan")
	if plain.MCPConfig != "/op.json" || slices.ContainsFunc(plain.AllowedTools, func(s string) bool { return strings.HasPrefix(s, "mcp__skopos__") }) {
		t.Fatalf("without an agent key: %+v", plain)
	}
	w.EnableAgentMCP("/state/agent-mcp.json")
	plan := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, run, "plan")
	if plan.MCPConfig != "/state/agent-mcp.json" || !slices.Contains(plan.AllowedTools, "mcp__skopos__code_search") || slices.Contains(plan.AllowedTools, "mcp__skopos__plan_update_item") {
		t.Fatalf("plan: %v %v", plan.MCPConfig, plan.AllowedTools)
	}
	if slices.Contains(planTools, "mcp__skopos__code_search") {
		t.Fatal("the shared planTools slice must not be modified")
	}
	impl := w.agentRun("/wt", "p", "{}", implTools, 0, &Account{Runner: "opencode"}, run, "implement")
	if !slices.Contains(impl.AllowedTools, "mcp__skopos__plan_update_item") {
		t.Fatalf("implement: %v", impl.AllowedTools)
	}
	codex := w.agentRun("/wt", "p", "{}", reviewTools, 0, &Account{Runner: "codex"}, run, "review")
	if codex.MCPConfig != "/op.json" || slices.ContainsFunc(codex.AllowedTools, func(s string) bool { return strings.HasPrefix(s, "mcp__skopos__") }) {
		t.Fatalf("codex: %+v", codex)
	}
	if w.skoposPrompt(t.Context(), run, &Account{Runner: "codex"}, "main", "b", "review", "", nil, "") != nil {
		t.Fatal("codex prompts get no skopos section")
	}
}

// indexStatusServer answers GET /api/codeindex/<ws>/status with branches.
func indexStatusServer(t *testing.T, branches map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/status") {
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		var out []map[string]string
		for b, h := range branches {
			out = append(out, map[string]string{"branch": b, "head_sha": h})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The skopos section appears only when the run has the tools; its code
// part only when the switch is on and the server holds a usable index, and
// it names exactly which branch that is per stage; implement gets its step
// ids.
func TestSkoposPromptSection(t *testing.T) {
	run := &Run{ID: "r1", WorkspaceID: "github.com/acme/app"}
	steps := []Step{{Title: "Add the flag", ID: "item-1"}, {Title: "Agent-proposed", ID: ""}, {Title: "Document it", ID: "item-3"}}
	worker := func(branches map[string]string, enabled bool) *Worker {
		w := New(&Config{StateDir: t.TempDir()}, NewClient(indexStatusServer(t, branches), "k"), slog.New(slog.DiscardHandler))
		w.EnableAgentMCP("/state/agent-mcp.json")
		if enabled {
			w.client.indexEnabled = []string{run.WorkspaceID}
		}
		return w
	}
	render := func(w *Worker, phase, head string) string {
		t.Helper()
		name := "review.md"
		data := map[string]any{"Base": "main"}
		if phase == "implement" {
			name = "implement.md"
			data = map[string]any{"Title": "T", "Content": "c", "Steps": steps, "Branch": "agent/x", "Base": "main"}
		}
		planID := ""
		if phase == "implement" {
			planID = "plan-9"
		}
		data["Skopos"] = w.skoposPrompt(t.Context(), run, &Account{Runner: "claude"}, "main", "agent/x", phase, planID, steps, head)
		out, err := renderPrompt(name, data)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	expect := func(name, got string, want, not []string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q:\n%s", name, w, got)
			}
		}
		for _, n := range not {
			if strings.Contains(got, n) {
				t.Errorf("%s: must not contain %q:\n%s", name, n, got)
			}
		}
	}

	noKey := New(&Config{StateDir: t.TempDir()}, nil, slog.New(slog.DiscardHandler))
	if out, _ := renderPrompt("implement.md", map[string]any{"Title": "T", "Steps": steps, "Skopos": noKey.skoposPrompt(t.Context(), run, &Account{}, "main", "agent/x", "implement", "p", steps, "")}); strings.Contains(out, "# Skopos") {
		t.Fatal("no agent key: no skopos section")
	}

	indexed := map[string]string{"main": "aaa", "agent/x": "bbb"}
	expect("switch off", render(worker(indexed, false), "implement", ""),
		[]string{"# Skopos (shared memory)", "blackboard_read", `plan_id: "plan-9"`},
		[]string{"code_search", "code index"})
	expect("no index yet", render(worker(map[string]string{}, true), "implement", ""),
		[]string{"# Skopos"}, []string{"code_search"})
	expect("implement", render(worker(indexed, true), "implement", ""),
		[]string{"# Skopos (shared memory and the code index)", `workspace_id: "github.com/acme/app"`, "Query branch `main`", "Your own changes are not in it",
			`author_agent_id: "claude@r1"`, "step 1 (Add the flag): `item-1`", "step 3 (Document it): `item-3`"},
		[]string{"step 2 (Agent-proposed)"})
	expect("review at the indexed head", render(worker(indexed, true), "review", "bbb"),
		[]string{"Query branch `agent/x`", "exactly the commit you are reviewing"}, []string{"plan_update_item"})
	expect("review ahead of the index", render(worker(indexed, true), "review", "ccc"),
		[]string{"Query branch `main`", "use the diff for what changed"}, nil)
}
