package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A run that came with an agent token gets its own MCP config: the
// operator's servers plus skopos with the run's token, in a directory and a
// file only the worker can read; agent runs use it instead of the agent
// key's; the token is redacted from the run's log; the file is gone and the
// token forgotten when the run ends.
func TestRunTokenMCPConfig(t *testing.T) {
	state := t.TempDir()
	op := filepath.Join(state, "op.json")
	// The operator's file may even define "skopos" (a setup from before
	// agent keys): the run's own token replaces it.
	if err := os.WriteFile(op, []byte(`{"mcpServers":{"llmrouter":{"type":"http","url":"https://llm.example/mcp"},"skopos":{"type":"http","url":"https://old.example/mcp","headers":{"Authorization":"Bearer sk_shared"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := New(&Config{StateDir: state, MCPConfig: op}, NewClient("https://skopos.example", "sk_worker"), slog.New(slog.DiscardHandler))
	w.EnableAgentMCP("/state/agent-mcp.json", nil)
	run := &Run{ID: "run-1", WorkspaceID: "ws", AgentToken: "skr_this-run-only-token"}

	// An earlier attempt's log left the run directory world-readable.
	runDir := filepath.Join(state, "runs", run.ID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	forget, err := w.prepareRunMCP(run)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(runDir, "mcp.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	sk := cfg.MCPServers["skopos"]
	if sk.URL != "https://skopos.example/mcp" || sk.Headers["Authorization"] != "Bearer skr_this-run-only-token" {
		t.Fatalf("skopos server: %+v", sk)
	}
	if cfg.MCPServers["llmrouter"].URL != "https://llm.example/mcp" || strings.Contains(string(raw), "sk_shared") || strings.Contains(string(raw), "sk_worker") {
		t.Fatalf("operator servers kept, the shared key and the worker key never in it: %s", raw)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(runDir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("the run directory must be closed even when it already existed: %v", fi.Mode().Perm())
	}

	// The run's agents get that file, and the stage's skopos tools.
	ar := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, run, "plan")
	if ar.MCPConfig != path || !slices.Contains(ar.AllowedTools, "mcp__skopos__code_search") {
		t.Fatalf("agent run: %v %v", ar.MCPConfig, ar.AllowedTools)
	}
	if w.skoposPrompt(t.Context(), run, &Account{Runner: "claude"}, "main", "b", "plan", "", nil, "") == nil {
		t.Fatal("the prompt must describe the skopos tools")
	}
	// Codex has no MCP: it gets the operator's file and no skopos tools.
	if codex := w.agentRun("/wt", "p", "{}", reviewTools, 0, &Account{Runner: "codex"}, run, "review"); codex.MCPConfig != op {
		t.Fatalf("codex: %v", codex.MCPConfig)
	}
	// The token never reaches the run log.
	if got := redact(w, "Authorization: Bearer skr_this-run-only-token"); strings.Contains(got, "skr_this-run-only-token") {
		t.Fatalf("the run token must be redacted: %q", got)
	}

	forget()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file must be removed when the run ends: %v", err)
	}
	if got := redact(w, "skr_this-run-only-token"); got != "skr_this-run-only-token" {
		t.Fatal("the token is forgotten after the run (the redactor must not grow with every run)")
	}
	// Another run, or the same one after it ended, falls back to the
	// agent-key config.
	if after := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, run, "plan"); after.MCPConfig != "/state/agent-mcp.json" {
		t.Fatalf("after the run: %v", after.MCPConfig)
	}
}

// A server from before run tokens sends none: nothing is written, and the
// run uses the agent key's config when the worker has one, else only the
// operator's servers.
func TestRunWithoutTokenFallsBack(t *testing.T) {
	state := t.TempDir()
	w := New(&Config{StateDir: state, MCPConfig: "/op.json"}, NewClient("https://skopos.example", "sk_worker"), slog.New(slog.DiscardHandler))
	run := &Run{ID: "run-1", WorkspaceID: "ws"}
	forget, err := w.prepareRunMCP(run)
	if err != nil {
		t.Fatal(err)
	}
	defer forget()
	if _, err := os.Stat(filepath.Join(state, "runs", run.ID, "mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("no token, no file: %v", err)
	}
	if ar := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, run, "plan"); ar.MCPConfig != "/op.json" ||
		slices.ContainsFunc(ar.AllowedTools, func(s string) bool { return strings.HasPrefix(s, "mcp__skopos__") }) {
		t.Fatalf("no token and no agent key: %+v", ar)
	}
	w.EnableAgentMCP("/state/agent-mcp.json", nil)
	if ar := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, run, "plan"); ar.MCPConfig != "/state/agent-mcp.json" {
		t.Fatalf("no token, agent key: %v", ar.MCPConfig)
	}
}

// A run's config that cannot be written (a broken operator file) is an
// error the caller logs; the token is still redacted, and the run falls
// back.
func TestRunTokenMCPConfigError(t *testing.T) {
	state := t.TempDir()
	op := filepath.Join(state, "op.json")
	_ = os.WriteFile(op, []byte(`{not json`), 0o600)
	w := New(&Config{StateDir: state, MCPConfig: op}, NewClient("https://skopos.example", "sk_worker"), slog.New(slog.DiscardHandler))
	run := &Run{ID: "run-1", WorkspaceID: "ws", AgentToken: "skr_this-run-only-token"}
	forget, err := w.prepareRunMCP(run)
	if err == nil || forget == nil {
		t.Fatalf("a broken mcp_config must be an error with a usable cleanup: %v", err)
	}
	if got := redact(w, "skr_this-run-only-token"); strings.Contains(got, "skr_") {
		t.Fatal("the token is redacted even when the file could not be written")
	}
	// The shared agent key is never used in the token's place: the run's
	// agents get the operator's servers and no skopos tools.
	w.EnableAgentMCP("/state/agent-mcp.json", nil)
	ar := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, run, "plan")
	if ar.MCPConfig != op || slices.ContainsFunc(ar.AllowedTools, func(s string) bool { return strings.HasPrefix(s, "mcp__skopos__") }) {
		t.Fatalf("a run with a token must not fall back to the agent key: %v %v", ar.MCPConfig, ar.AllowedTools)
	}
	if w.skoposPrompt(t.Context(), run, &Account{Runner: "claude"}, "main", "b", "plan", "", nil, "") != nil {
		t.Fatal("no skopos tools, no skopos prompt section")
	}
	forget()
	// The next run, from a server that sent no token, does use it.
	if ar := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, &Run{ID: "run-2", WorkspaceID: "ws"}, "plan"); ar.MCPConfig != "/state/agent-mcp.json" {
		t.Fatalf("a run without a token: %v", ar.MCPConfig)
	}
}

// The run id becomes a directory that is chmodded and holds a credential:
// an id that is not a plain name writes nothing.
func TestRunTokenRefusesPathLikeRunID(t *testing.T) {
	state := t.TempDir()
	w := New(&Config{StateDir: state}, NewClient("https://skopos.example", "sk_worker"), slog.New(slog.DiscardHandler))
	for _, id := range []string{"../escape", "a/b", "..", ".", `a\b`, ""} {
		forget, err := w.prepareRunMCP(&Run{ID: id, WorkspaceID: "ws", AgentToken: "skr_this-run-only-token"})
		if err == nil {
			t.Errorf("run id %q must be refused", id)
		}
		forget()
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatalf("nothing may be written for a refused id: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(state), "escape")); !os.IsNotExist(err) {
		t.Fatalf("nothing outside the state dir: %v", err)
	}
}

// The worker's own key must not hold the admin permission; the root key and
// an approver key work with a warning; a plain scoped key is silent.
func TestCheckWorkerKey(t *testing.T) {
	check := func(body string) ([]string, error) {
		return CheckWorkerKey(context.Background(), whoamiServerKey(t, "sk_worker", body), "sk_worker")
	}
	if warn, err := check(`{"root":false,"key":{"approver":false,"admin":false}}`); err != nil || len(warn) != 0 {
		t.Fatalf("plain key: %v %v", warn, err)
	}
	if _, err := check(`{"root":false,"key":{"approver":false,"admin":true}}`); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("an admin key must be refused: %v", err)
	}
	if warn, err := check(`{"root":true}`); err != nil || len(warn) != 1 || !strings.Contains(warn[0], "root key") {
		t.Fatalf("root key: %v %v", warn, err)
	}
	if warn, err := check(`{"root":false,"key":{"approver":true,"admin":false}}`); err != nil || len(warn) != 1 || !strings.Contains(warn[0], "approver") {
		t.Fatalf("approver key: %v %v", warn, err)
	}
	// No key (a server without authentication) and an unreachable server
	// are not reasons to refuse: the claim loop reports those.
	if warn, err := CheckWorkerKey(context.Background(), "http://127.0.0.1:1", ""); err != nil || warn != nil {
		t.Fatalf("no key: %v %v", warn, err)
	}
	if warn, err := CheckWorkerKey(context.Background(), "http://127.0.0.1:1", "sk_worker"); err != nil || warn != nil {
		t.Fatalf("unreachable server: %v %v", warn, err)
	}
}

// An agent key that holds the admin permission is refused like an approver.
func TestCheckAgentKeyRefusesAdmin(t *testing.T) {
	body := `{"root":false,"key":{"approver":false,"admin":true,"capabilities":["mcp","code:read"]}}`
	if err := CheckAgentKey(context.Background(), whoamiServer(t, body), "sk_agent"); err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("an admin agent key must be refused: %v", err)
	}
}

// redact applies the worker's run-log redaction (no redactor = no secrets).
func redact(w *Worker, s string) string {
	if r := w.redactor(); r != nil {
		return r.Replace(s)
	}
	return s
}
