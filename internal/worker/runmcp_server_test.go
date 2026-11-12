package worker

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

func delivered(servers map[string]string, secrets ...string) *RunMCP {
	m := &RunMCP{Servers: map[string]json.RawMessage{}, Secrets: secrets}
	for name, raw := range servers {
		m.Servers[name] = json.RawMessage(raw)
	}
	return m
}

// When the server holds a workspace's MCP servers, a run gets exactly the
// ones its claim answer carries, in its own file — and nothing from the
// worker's mcp_config. Their credentials are redacted from the run's log,
// and gone with the file when the run ends.
func TestRunGetsTheServersItsClaimCarries(t *testing.T) {
	state := t.TempDir()
	op := filepath.Join(state, "op.json")
	if err := os.WriteFile(op, []byte(`{"mcpServers":{"fortix":{"type":"http","url":"https://fortix.example/mcp","headers":{"Authorization":"Bearer operator-file-token"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const secret = "tok-for-this-workspace-only"
	cfg := &Config{StateDir: state, MCPConfig: op, ServerCommands: true, Vars: map[string]string{"TOOLS_DIR": "/opt/tools"}}
	w := New(cfg, NewClient("https://skopos.example", "sk_worker"), slog.New(slog.DiscardHandler))
	run := &Run{ID: "run-1", WorkspaceID: "ws", AgentToken: "skr_this-run-only-token", MCP: delivered(map[string]string{
		"llmrouter":  `{"type":"http","url":"https://llm.example/mcp","headers":{"Authorization":"Bearer ` + secret + `"}}`,
		"scriptling": `{"command":"${TOOLS_DIR}/scriptling","args":["mcp","--data","${TOOLS_DIR}/data"],"env":{"DATA":"${TOOLS_DIR}/d"}}`,
		"novar":      `{"command":"${NOT_DECLARED}/x","args":[]}`,
		"skopos":     `{"type":"http","url":"https://elsewhere.example/mcp"}`,
		"odd":        `{"type":"http","url":"https://x/mcp","hooks":{"x":1}}`,
		"neither":    `{"type":"http"}`,
	}, secret)}
	run.MCP.Notes = []string{"mcp server gone is no longer defined: left out"}
	forget, err := w.prepareRunMCP(run)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "runs", "run-1", "mcp.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		MCPServers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(file.MCPServers))
	for n := range file.MCPServers {
		names = append(names, n)
	}
	slices.Sort(names)
	if strings.Join(names, ",") != "llmrouter,scriptling,skopos" {
		t.Fatalf("servers in the run's file: %v", names)
	}
	// The operator's file is not used at all for this run.
	if strings.Contains(string(raw), "fortix") || strings.Contains(string(raw), "operator-file-token") {
		t.Fatalf("the worker's own mcp_config reached a run whose servers the server holds: %s", raw)
	}
	if file.MCPServers["llmrouter"].Headers["Authorization"] != "Bearer "+secret {
		t.Fatalf("the delivered server: %+v", file.MCPServers["llmrouter"])
	}
	// skopos is the worker's own entry, with the run's token.
	if sk := file.MCPServers["skopos"]; sk.URL != "https://skopos.example/mcp" || sk.Headers["Authorization"] != "Bearer skr_this-run-only-token" {
		t.Fatalf("skopos: %+v", sk)
	}
	// A command server: this host's variables filled in.
	if c := file.MCPServers["scriptling"]; c.Command != "/opt/tools/scriptling" || c.Args[2] != "/opt/tools/data" || c.Env["DATA"] != "/opt/tools/d" {
		t.Fatalf("command server: %+v", c)
	}
	notes := strings.Join(w.runMCPNotes, "\n")
	for _, want := range []string{"gone is no longer defined", "novar: ${NOT_DECLARED} is not declared", `"skopos": not a name`, "odd: its definition is not one this worker reads", "neither: neither an http server nor a command"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	if strings.Join(w.runMCPNames, ",") != "llmrouter,scriptling" {
		t.Fatalf("the servers the run got: %v", w.runMCPNames)
	}
	// The credential and the token are redacted from the run's log.
	if got := redact(w, "calling with Bearer "+secret+" and skr_this-run-only-token"); strings.Contains(got, secret) || strings.Contains(got, "skr_this-run-only-token") {
		t.Fatalf("not redacted: %s", got)
	}
	// claude and opencode runs get the file; a runner without MCP gets
	// nothing — never the operator's file.
	cfg.RunnerSpecs = map[string]*RunnerSpec{"codex": {Kind: "codex"}}
	if p, skopos := w.mcpConfigFor(run, "claude"); p != path || !skopos {
		t.Fatalf("claude: %q %v", p, skopos)
	}
	if p, skopos := w.mcpConfigFor(run, "codex"); p != "" || skopos {
		t.Fatalf("codex: %q %v", p, skopos)
	}
	ar := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, run, "plan")
	if ar.MCPConfig != path || !slices.Contains(ar.AllowedTools, "mcp__skopos__code_search") {
		t.Fatalf("agent run: %q", ar.MCPConfig)
	}
	forget()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the run's file must be gone: %v", err)
	}
	if got := redact(w, secret); got != secret || len(w.runSecrets) != 0 || w.runOwned {
		t.Fatalf("the run's secrets are forgotten: %q %v", got, w.runSecrets)
	}
	// The next run, whose workspace the server holds no servers for, gets
	// the operator's file again.
	next := &Run{ID: "run-2", WorkspaceID: "other", AgentToken: "skr_another-run-token"}
	forget, err = w.prepareRunMCP(next)
	if err != nil {
		t.Fatal(err)
	}
	defer forget()
	raw, _ = os.ReadFile(filepath.Join(state, "runs", "run-2", "mcp.json"))
	if !strings.Contains(string(raw), "fortix") || strings.Contains(string(raw), secret) {
		t.Fatalf("a run of a workspace the server holds nothing for: %s", raw)
	}
}

// A command server from the server is not run on a worker that does not
// take commands from it; an empty list still means "no MCP servers", not
// the worker's own.
func TestServerHeldMCPFailsClosed(t *testing.T) {
	state := t.TempDir()
	op := filepath.Join(state, "op.json")
	if err := os.WriteFile(op, []byte(`{"mcpServers":{"fortix":{"type":"http","url":"https://fortix.example/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := New(&Config{StateDir: state, MCPConfig: op}, NewClient("https://skopos.example", "sk_worker"), slog.New(slog.DiscardHandler))
	run := &Run{ID: "run-1", WorkspaceID: "ws", AgentToken: "skr_this-run-only-token", MCP: delivered(map[string]string{"tool": `{"command":"/bin/tool","args":[]}`})}
	forget, err := w.prepareRunMCP(run)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(state, "runs", "run-1", "mcp.json"))
	if strings.Contains(string(raw), "/bin/tool") || strings.Contains(string(raw), "fortix") || !strings.Contains(strings.Join(w.runMCPNotes, " "), "server_commands is off") {
		t.Fatalf("a command server without server_commands: %s / %v", raw, w.runMCPNotes)
	}
	forget()
	// No servers at all, and no token: the run still does not get the
	// operator's file.
	bare := &Run{ID: "run-2", WorkspaceID: "ws", MCP: delivered(nil)}
	forget, err = w.prepareRunMCP(bare)
	if err != nil {
		t.Fatal(err)
	}
	defer forget()
	if p, skopos := w.mcpConfigFor(bare, "claude"); skopos || strings.Contains(p, "op.json") {
		t.Fatalf("an empty list: %q %v", p, skopos)
	}
	if ar := w.agentRun("/wt", "p", "{}", planTools, 0, &Account{Runner: "claude"}, bare, "plan"); ar.MCPConfig == op || slices.Contains(ar.AllowedTools, "mcp__skopos__code_search") {
		t.Fatalf("agent run: %q %v", ar.MCPConfig, ar.AllowedTools)
	}
	// The run's file could not be written: nothing, not the operator's.
	w2 := New(&Config{StateDir: filepath.Join(op, "not-a-dir"), MCPConfig: op}, NewClient("https://skopos.example", "sk_worker"), slog.New(slog.DiscardHandler))
	forget2, err := w2.prepareRunMCP(run)
	defer forget2()
	if err == nil {
		t.Fatal("the write should have failed")
	}
	if p, skopos := w2.mcpConfigFor(run, "claude"); p != "" || skopos {
		t.Fatalf("after a failed write: %q %v", p, skopos)
	}
}

// Servers the server could not resolve: the run is given back, not run.
func TestUnreadableMCPGivesTheRunBack(t *testing.T) {
	var mu sync.Mutex
	var report map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/report") {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&report)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	c := executorConfig(t)
	c.StateDir = t.TempDir()
	client := NewClient(srv.URL, "k")
	client.leases["r1"] = "lease-1"
	w := New(c, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.execute(t.Context(), &Run{ID: "r1", ItemID: "i1", WorkspaceID: "bare", Stage: "plan", Runner: "claude", Attempt: 1, MCP: &RunMCP{Unreadable: true}})
	mu.Lock()
	defer mu.Unlock()
	reason, _ := report["reason"].(string)
	if report == nil || report["retryable"] != true || !strings.Contains(reason, "MCP servers") {
		t.Fatalf("the report: %v", report)
	}
}

// Every claim says the worker takes a run's MCP servers from the answer.
func TestClaimSendsTheFeature(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	if _, err := NewClient(srv.URL, "k").Claim(t.Context(), "w1", []string{"claude"}, []string{"ws"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	features, _ := body["features"].([]any)
	if len(features) != 1 || features[0] != FeatureRunMCP {
		t.Fatalf("features: %v", body["features"])
	}
}

// A run whose MCP file cannot be written, where the server holds its MCP
// servers, is given back — not run without them — and its log never names
// servers it does not have.
func TestFailedMCPWriteGivesTheRunBack(t *testing.T) {
	var mu sync.Mutex
	var report map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/report") {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&report)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := executorConfig(t)
	c.StateDir = filepath.Join(notADir, "state")
	client := NewClient(srv.URL, "k")
	client.leases["r1"] = "lease-1"
	w := New(c, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run := &Run{ID: "r1", ItemID: "i1", WorkspaceID: "bare", Stage: "plan", Runner: "claude", Attempt: 1, AgentToken: "skr_a-token-for-this-run",
		MCP: delivered(map[string]string{"llmrouter": `{"type":"http","url":"https://llm.example/mcp"}`})}
	if _, err := w.prepareRunMCP(run); err == nil || len(w.runMCPNames) != 0 {
		t.Fatalf("a failed write names the servers the run has: %v %v", err, w.runMCPNames)
	}
	w.execute(t.Context(), run)
	mu.Lock()
	defer mu.Unlock()
	reason, _ := report["reason"].(string)
	if report == nil || report["retryable"] != true || !strings.Contains(reason, "MCP config could not be written") {
		t.Fatalf("the report: %v", report)
	}
}

// A worker killed in the middle of a run leaves that run's MCP file — and
// the workspace's credentials — behind. The next start removes it.
func TestStartRemovesLeftoverRunMCPFiles(t *testing.T) {
	state := t.TempDir()
	left := filepath.Join(state, "runs", "run-killed", "mcp.json")
	tmp := filepath.Join(state, "runs", "run-killed-2", "mcp.json.tmp")
	keep := filepath.Join(state, "runs", "run-killed", "plan.log")
	for _, p := range []string{left, tmp, keep} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"mcpServers":{"x":{"headers":{"Authorization":"Bearer left-behind-secret"}}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := New(&Config{StateDir: state}, NewClient("https://skopos.example", "k"), slog.New(slog.DiscardHandler))
	w.sweepRunMCP()
	for _, p := range []string{left, tmp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s was left on disk: %v", p, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("a run's log was removed with it: %v", err)
	}
}

// A secret is kept out of the log also where it stands inside JSON, with
// its quotes and angle brackets escaped.
func TestRedactorCoversTheJSONForm(t *testing.T) {
	const secret = `p"ass<w>ord&more\x`
	w := New(&Config{StateDir: t.TempDir()}, NewClient("https://skopos.example", "k"), slog.New(slog.DiscardHandler))
	w.runSecrets = []string{secret}
	asJSON, _ := json.Marshal(map[string]string{"Authorization": "Bearer " + secret})
	for _, line := range []string{"plain: " + secret, string(asJSON)} {
		if got := redact(w, line); strings.Contains(got, "ass<w>") || strings.Contains(got, `ass<w`) || strings.Contains(got, `p\"ass`) {
			t.Fatalf("not redacted: %s", got)
		}
	}
}

// The config a run executes under says the server holds the workspace's
// MCP servers; a claim that carries none is not run with this file's.
func TestClaimWithoutServersUnderAConfigThatNamesThem(t *testing.T) {
	var mu sync.Mutex
	var report map[string]any
	c := executorConfig(t)
	c.StateDir = t.TempDir()
	eff := &wsconfig.Effective{Workspace: "bare", Managed: true, Rev: "r1", Config: wsconfig.Config{MCP: &[]string{"llmrouter"}}}
	cs := &configServer{effective: map[string]*wsconfig.Effective{"bare": eff}, revs: map[string]*wsconfig.Effective{"r1": eff}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/report") {
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&report)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.Contains(r.URL.Path, "/config") {
			cs.ServeHTTP(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, "k")
	client.leases["r1"] = "lease-1"
	w := New(c, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.execute(t.Context(), &Run{ID: "r1", ItemID: "i1", WorkspaceID: "bare", Stage: "plan", Runner: "claude", Attempt: 1, ConfigRev: "r1"})
	mu.Lock()
	defer mu.Unlock()
	reason, _ := report["reason"].(string)
	if report == nil || report["retryable"] != true || !strings.Contains(reason, "the claim carried none") {
		t.Fatalf("the report: %v", report)
	}
}
