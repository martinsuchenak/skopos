package worker

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/paularlott/mcp"
)

// TestLiveRunTokenMCP drives the REAL claude CLI through the worker's own
// path for a run that came with an agent token (opt-in — it spends tokens):
//
//	SKOPOS_LIVE_CLAUDE=1 go test ./internal/worker -run TestLiveRunTokenMCP -v
//
// It proves what the unit tests can only assert about a file: under the
// lockdown, the agent CLI loads the run's own MCP config, reaches the
// skopos server named in it, and presents the run's token — not the agent
// key's config, and no other credential. The MCP server here is a stand-in
// with one tool; what a token may do on a real server is covered by
// cmd/routes TestIntegrationRunToken.
func TestLiveRunTokenMCP(t *testing.T) {
	if os.Getenv("SKOPOS_LIVE_CLAUDE") != "1" {
		t.Skip("set SKOPOS_LIVE_CLAUDE=1 to run against the real claude CLI")
	}
	bin := os.Getenv("SKOPOS_LIVE_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	if err := LockdownSupported(context.Background(), bin, ""); err != nil {
		t.Fatal(err)
	}
	guardHostRepo(t)

	const token = "skr_live-run-token-7c1e9a"
	const marker = "PERSIMMON-4417"
	server := mcplib.NewServer("skopos", "1.0.0")
	server.RegisterTool(
		mcplib.NewTool("skopos_context", "Load the recorded context for the workspace", mcplib.String("workspace_id", "Workspace ID")),
		func(context.Context, *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			return mcplib.NewToolResponseText("The recorded context word is " + marker + "."), nil
		})
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("Authorization")]++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		server.HandleRequest(w, r)
	}))
	defer srv.Close()

	state := t.TempDir()
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	w := New(&Config{StateDir: state}, NewClient(srv.URL, "sk_worker-key-not-for-agents"), slog.New(slog.DiscardHandler))
	// The agent-key config a worker may still hold: it must not be the one
	// the run uses.
	w.EnableAgentMCP(filepath.Join(state, "agent-mcp.json"), nil)
	run := &Run{ID: "live-run-1", WorkspaceID: "github.com/acme/app", Stage: "plan", AgentToken: token}
	forget, err := w.prepareRunMCP(run)
	if err != nil {
		t.Fatal(err)
	}
	defer forget()

	ar := w.agentRun(wt, "Call the mcp__skopos__skopos_context tool with workspace_id \"github.com/acme/app\" and reply with the recorded context word only. If the tool is unavailable, reply UNAVAILABLE.",
		"", planTools, 3*time.Minute, &Account{Runner: "claude", Model: "haiku"}, run, "plan")
	ar.Bin = bin
	if ar.MCPConfig != filepath.Join(state, "runs", run.ID, "mcp.json") {
		t.Fatalf("the run must use its own MCP config: %s", ar.MCPConfig)
	}
	res := runClaude(context.Background(), ar)
	if res.Outcome != OutcomeOK {
		t.Fatalf("agent run: %s %s", res.Outcome, res.Detail)
	}
	if !strings.Contains(res.Text, marker) {
		t.Errorf("the agent must reach the skopos tool with the run token: %q", res.Text)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["Bearer "+token] == 0 {
		t.Errorf("the server never saw the run token: %v", keysOf(seen))
	}
	for auth := range seen {
		if auth != "Bearer "+token {
			t.Errorf("a request came with another credential: %q", auth)
		}
	}
	// The run log's redactor hides the token.
	if got := redact(w, "Bearer "+token); strings.Contains(got, token) {
		t.Errorf("the token must be redacted from run logs: %q", got)
	}
}

func keysOf(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
