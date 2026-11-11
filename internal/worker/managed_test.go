package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

func strs(v ...string) *[]string { return &v }

// managedConfig: one repo whose file sets every kind of shared field, so a
// test can tell the file's values from the server's and from the defaults.
func managedConfig(t *testing.T, serverCommands bool) *Config {
	t.Helper()
	rounds := 4
	c := &Config{
		ID: "worker-a", StateDir: t.TempDir(), ServerCommands: serverCommands,
		PlanTimeout: 7 * time.Minute, MCPDeny: []string{"mcp__local__deny"}, MCPServers: []string{"mcp__local"}, MCPConfig: "/etc/mcp.json",
		Repos: map[string]*Repo{"ws": {
			Path: "/clones/ws", DefaultBranch: "trunk", BranchPrefix: "bot/",
			Setup: []string{"local setup"}, Checks: []string{"local check"}, Preflight: []string{"local preflight"},
			CheckFixRounds: &rounds, SetupTimeout: 3 * time.Minute,
			SetupEnv: map[string]string{"TOKEN": "x"},
		}},
		Accounts: []*Account{{Name: "c1", Runner: "claude"}},
	}
	c.Repos["ws"].Push.Kind, c.Repos["ws"].Push.RemoteURL, c.Repos["ws"].Push.TokenEnv = "github", "https://github.com/a/local.git", "PUSH_TOKEN"
	if err := c.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return c
}

// A managed workspace takes every shared field from the server, and a field
// the server leaves out gets its default — never the file's value. The
// file keeps the machine's own: path, setup_env, the push token's variable.
func TestMergeManagedReplacesTheFilesSharedFields(t *testing.T) {
	c := managedConfig(t, true)
	if got := strings.Join(c.Repos["ws"].localSet, ","); got != "default_branch,branch_prefix,setup,checks,check_fix_rounds,preflight,setup_timeout,push.kind,push.remote_url" {
		t.Fatalf("the fields the file set: %s", got)
	}
	eff := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{
		DefaultBranch: new("main"), Checks: strs("server check"), PlanTimeout: new(wsconfig.Duration(20 * time.Minute)),
		MCPServers: strs("mcp__server"),
		Push:       &wsconfig.Push{Kind: new("github"), RemoteURL: new("https://github.com/a/server.git"), PullRequest: new("draft")},
	}}
	m, err := c.mergeManaged("ws", eff)
	if err != nil {
		t.Fatal(err)
	}
	r := m.repo
	if r.DefaultBranch != "main" || r.Checks[0] != "server check" || r.Push.RemoteURL != "https://github.com/a/server.git" || r.Push.PullRequest != "draft" {
		t.Fatalf("the server's fields: %+v", r)
	}
	// Left out on the server: defaults, not what the file says.
	if r.BranchPrefix != "agent/" || len(r.Setup) != 0 || len(r.Preflight) != 0 || *r.CheckFixRounds != 2 || r.SetupTimeout != 10*time.Minute || m.implTimeout != 90*time.Minute {
		t.Fatalf("a field the server leaves out must be its default: %+v", r)
	}
	if r.Path != "/clones/ws" || r.SetupEnv["TOKEN"] != "x" || r.Push.TokenEnv != "PUSH_TOKEN" {
		t.Fatalf("the machine's own fields stay the file's: %+v", r)
	}
	// The deny list: the default (the server sets none) plus the file's own.
	if m.planTimeout != 20*time.Minute || !slices.Equal(m.mcpServers, []string{"mcp__server"}) || !slices.Equal(m.mcpDeny, append(slices.Clone(wsconfig.DefaultMCPDeny), "mcp__local__deny")) {
		t.Fatalf("worker-wide settings become the workspace's: %+v", m)
	}
	// The file's repo is not touched.
	if c.Repos["ws"].DefaultBranch != "trunk" || c.Repos["ws"].Checks[0] != "local check" {
		t.Fatal("merging must not change the file's repo")
	}
}

// Without worker.server_commands the command fields stay the file's, even
// for a managed workspace; everything else still comes from the server.
func TestServerCommandsOffKeepsLocalCommands(t *testing.T) {
	c := managedConfig(t, false)
	eff := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{
		DefaultBranch: new("main"), Setup: strs("server setup"), Checks: strs("server check"),
	}}
	m, err := c.mergeManaged("ws", eff)
	if err != nil {
		t.Fatal(err)
	}
	if m.repo.Setup[0] != "local setup" || m.repo.Checks[0] != "local check" || m.repo.Preflight[0] != "local preflight" {
		t.Fatalf("commands must stay local: %+v", m.repo)
	}
	if m.repo.DefaultBranch != "main" {
		t.Fatal("the other fields come from the server")
	}
	if strings.Join(m.local, ",") != "setup,checks" {
		t.Fatalf("the fields kept local are named: %v", m.local)
	}
}

// What the server cannot know about this host is a problem named to it, not
// a run that fails later: a push with no token variable in the file.
func TestMergeManagedProblems(t *testing.T) {
	c := managedConfig(t, true)
	c.Repos["ws"].Push.TokenEnv = ""
	_, err := c.mergeManaged("ws", &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{
		Push: &wsconfig.Push{Kind: new("github"), RemoteURL: new("https://github.com/a/b.git")}}})
	if err == nil || !strings.Contains(err.Error(), "push.token_env") {
		t.Fatalf("a push without a token variable: %v", err)
	}
	if _, err := c.mergeManaged("other", &wsconfig.Effective{Workspace: "other", Managed: true}); err == nil {
		t.Fatal("a workspace without a repo in the file cannot be merged")
	}
}

// configServer answers the config endpoints from a table of revisions.
type configServer struct {
	mu        sync.Mutex
	effective map[string]*wsconfig.Effective // by workspace: what applies now
	revs      map[string]*wsconfig.Effective // by revision
	fleet     *wsconfig.Fleet
	gets      []string
	fail      bool
}

func (s *configServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = append(s.gets, r.URL.Path)
	if s.fail {
		http.Error(w, `{"error":"down"}`, http.StatusBadGateway)
		return
	}
	answer := func(v any, ok bool) {
		if !ok {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	switch p := r.URL.Path; {
	case p == "/api/fleet/config":
		answer(s.fleet, s.fleet != nil)
	case strings.HasSuffix(p, "/config/effective"):
		ws := strings.TrimSuffix(strings.TrimPrefix(p, "/api/workspaces/"), "/config/effective")
		e, ok := s.effective[ws]
		if !ok {
			e, ok = &wsconfig.Effective{Workspace: ws}, true
		}
		answer(e, ok)
	case strings.Contains(p, "/config/"):
		rev := p[strings.LastIndex(p, "/")+1:]
		e, ok := s.revs[rev]
		answer(e, ok)
	default:
		http.Error(w, "unexpected "+p, http.StatusTeapot)
	}
}

func managedWorker(t *testing.T, c *Config, s *configServer) (*Worker, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	var logs bytes.Buffer
	w := New(c, NewClient(srv.URL, "k"), slog.New(slog.NewTextHandler(&logs, nil)))
	return w, &logs
}

// A run executes under exactly the revision its claim names: fetched when
// the worker holds another, also when the config changed between two
// claims, and never started under one that cannot be fetched.
func TestHoldConfigRunsUnderTheClaimsRevision(t *testing.T) {
	c := managedConfig(t, true)
	one := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{Checks: strs("one")}}
	two := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r2", Config: wsconfig.Config{Checks: strs("two")}}
	s := &configServer{effective: map[string]*wsconfig.Effective{"ws": two}, revs: map[string]*wsconfig.Effective{"r1": one, "r2": two}}
	w, logs := managedWorker(t, c, s)
	ctx := context.Background()

	if got := w.repo("ws").Checks[0]; got != "local check" {
		t.Fatalf("before anything is fetched the file applies: %s", got)
	}
	// Idle refresh: the newest.
	if !w.refreshConfig(ctx) || w.repo("ws").Checks[0] != "two" {
		t.Fatalf("the idle refresh holds the newest: %v", w.repo("ws").Checks)
	}
	if !strings.Contains(logs.String(), "managed by the server") || !strings.Contains(logs.String(), "fields of this worker's file are ignored") {
		t.Fatalf("becoming managed is logged with the ignored fields:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "commands from the server changed") {
		t.Fatalf("a command change from the server is logged:\n%s", logs.String())
	}
	// A run claimed with the older revision runs under the older one.
	if err := w.holdConfig(ctx, &Run{WorkspaceID: "ws", ConfigRev: "r1"}); err != nil {
		t.Fatal(err)
	}
	if w.repo("ws").Checks[0] != "one" {
		t.Fatalf("the run's revision, not the newest: %v", w.repo("ws").Checks)
	}
	// Idle work must not keep the older revision: the next idle poll
	// refetches the newest although the claim answer's digest is the same.
	w.maybeRefreshConfig(ctx)
	if w.repo("ws").Checks[0] != "two" {
		t.Fatalf("after the run, idle work is back on the newest: %v", w.repo("ws").Checks)
	}
	if err := w.holdConfig(ctx, &Run{WorkspaceID: "ws", ConfigRev: "r1"}); err != nil {
		t.Fatal(err)
	}
	// Held already: no fetch.
	s.mu.Lock()
	before := len(s.gets)
	s.mu.Unlock()
	if err := w.holdConfig(ctx, &Run{WorkspaceID: "ws", ConfigRev: "r1"}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	if len(s.gets) != before {
		t.Fatalf("a held revision is not fetched again: %v", s.gets[before:])
	}
	s.mu.Unlock()
	// The config changed between two claims: the next run gets the new one.
	if err := w.holdConfig(ctx, &Run{WorkspaceID: "ws", ConfigRev: "r2"}); err != nil || w.repo("ws").Checks[0] != "two" {
		t.Fatalf("the next claim's revision: %v %v", err, w.repo("ws").Checks)
	}
	// A revision the server does not have: the run is not started.
	if err := w.holdConfig(ctx, &Run{WorkspaceID: "ws", ConfigRev: wsconfig.UnreadableRev}); err == nil {
		t.Fatal("a revision that cannot be fetched must not start a run")
	}
	if w.repo("ws").Checks[0] != "two" {
		t.Fatal("a failed fetch leaves what the worker holds")
	}
	// The server no longer manages the workspace: the file applies again.
	if err := w.holdConfig(ctx, &Run{WorkspaceID: "ws", ConfigRev: ""}); err != nil || w.repo("ws").Checks[0] != "local check" {
		t.Fatalf("an unmanaged run uses the file: %v %v", err, w.repo("ws").Checks)
	}
}

// A server that cannot be reached leaves what the worker holds, and one
// from before workspace config (404) means the file applies.
func TestRefreshConfigKeepsWhatItHolds(t *testing.T) {
	c := managedConfig(t, true)
	one := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{Checks: strs("one")}}
	s := &configServer{effective: map[string]*wsconfig.Effective{"ws": one}}
	w, _ := managedWorker(t, c, s)
	ctx := context.Background()
	if !w.refreshConfig(ctx) {
		t.Fatal("first fetch")
	}
	s.mu.Lock()
	s.fail = true
	s.mu.Unlock()
	if w.refreshConfig(ctx) || w.repo("ws").Checks[0] != "one" {
		t.Fatalf("an unreachable server: keep what is held (%v)", w.repo("ws").Checks)
	}
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	w2 := New(managedConfig(t, true), NewClient(old.URL, "k"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !w2.refreshConfig(ctx) || !w2.refreshFleet(ctx) || w2.repo("ws").Checks[0] != "local check" {
		t.Fatal("a server without the endpoints: the file applies")
	}
}

// A config that cannot be used on this host keeps the workspace out of the
// worker's claims, with the reason.
func TestConfigProblemIsReportedUnhealthy(t *testing.T) {
	c := managedConfig(t, true)
	c.Repos["ws"].Push.TokenEnv = ""
	c.Repos["ws"].Push.Kind = "none"
	bad := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{
		Push: &wsconfig.Push{Kind: new("github"), RemoteURL: new("https://github.com/a/b.git")}}}
	s := &configServer{effective: map[string]*wsconfig.Effective{"ws": bad}, revs: map[string]*wsconfig.Effective{"r1": bad}}
	w, _ := managedWorker(t, c, s)
	w.refreshConfig(context.Background())
	if got := w.checkHealth(context.Background())["ws"]; !strings.Contains(got, "config: ") || !strings.Contains(got, "push.token_env") {
		t.Fatalf("unhealthy with the reason: %q", got)
	}
	if err := w.holdConfig(context.Background(), &Run{WorkspaceID: "ws", ConfigRev: "r1"}); err == nil {
		t.Fatal("a run must not start under a config that cannot be used here")
	}
}

// Timeouts, the MCP deny list and the MCP tool prefixes are per workspace
// once the server manages it; an unmanaged workspace keeps the worker's.
func TestPerWorkspaceSettings(t *testing.T) {
	c := managedConfig(t, true)
	c.Repos["free"] = &Repo{Path: "/clones/free"}
	c.Workspaces = append(c.Workspaces, "free")
	eff := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{
		MCPDeny: strs("mcp__srv__write"), MCPServers: strs("mcp__srv"), ImplTimeout: new(wsconfig.Duration(30 * time.Minute))}}
	s := &configServer{effective: map[string]*wsconfig.Effective{"ws": eff}}
	w, _ := managedWorker(t, c, s)
	w.refreshConfig(context.Background())
	if w.implTimeout("ws") != 30*time.Minute || w.planTimeout("ws") != 10*time.Minute || !slices.Equal(w.mcpDeny("ws"), []string{"mcp__srv__write", "mcp__local__deny"}) {
		t.Fatalf("the managed workspace: %s %s %v", w.implTimeout("ws"), w.planTimeout("ws"), w.mcpDeny("ws"))
	}
	if tools := w.planToolsFor("ws"); !slices.Contains(tools, "mcp__srv") || slices.Contains(tools, "mcp__local") {
		t.Fatalf("the workspace's MCP prefixes replace the worker's: %v", tools)
	}
	if w.planTimeout("free") != 7*time.Minute || !slices.Equal(w.mcpDeny("free"), []string{"mcp__local__deny"}) || !slices.Contains(w.implToolsFor("free"), "mcp__local") {
		t.Fatalf("an unmanaged workspace keeps the worker's own: %s %v", w.planTimeout("free"), w.mcpDeny("free"))
	}
}

func defsConfig(t *testing.T, accounts ...*Account) *Config {
	t.Helper()
	c := &Config{ID: "worker-a", StateDir: t.TempDir(), Repos: map[string]*Repo{"ws": {Path: "/clones/ws"}}, Accounts: accounts,
		RunnerSpecs: map[string]*RunnerSpec{"glm": {Kind: "opencode"}}}
	if err := c.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	return c
}

// An account that names a kind feeds every runner of that kind the server
// defines, with the definition's model and effort; `models` narrows it.
func TestKindAccountsFeedDefinedRunners(t *testing.T) {
	c := defsConfig(t,
		&Account{Name: "claude-1", Kind: "claude", Priority: 1},
		&Account{Name: "oc-zai", Kind: "opencode", Models: []string{"zai-coding-plan/*"}},
	)
	if got := c.UsedRunners(); len(got) != 0 || len(c.Accounts) != 0 {
		t.Fatalf("no definitions yet: nothing is offered (%v)", got)
	}
	notes := c.ApplyRunnerDefs(&wsconfig.Fleet{Rev: "f1", Runners: []wsconfig.RunnerDef{
		{Name: "claude-opus", Kind: "claude", Model: "opus", Effort: "high", Provider: "anthropic"},
		{Name: "claude-sonnet", Kind: "claude", Model: "sonnet", Provider: "anthropic"},
		{Name: "glm-5-3", Kind: "opencode", Model: "zai-coding-plan/glm-5.3", Provider: "zai"},
		{Name: "qwen", Kind: "opencode", Model: "openrouter/qwen", Provider: "openrouter"},
	}})
	if len(notes) != 0 {
		t.Fatalf("notes: %v", notes)
	}
	if got := strings.Join(c.UsedRunners(), ","); got != "claude-opus,claude-sonnet,glm-5-3" {
		t.Fatalf("offered: %s (qwen's model does not match the account's models)", got)
	}
	now := time.Now()
	opus := c.AccountFor("claude-opus", now)
	if opus == nil || opus.Name != "claude-1" || opus.Model != "opus" || opus.Effort != "high" {
		t.Fatalf("the definition's model and effort: %+v", opus)
	}
	if _, kind := c.runnerSpec("glm-5-3"); kind != "opencode" {
		t.Fatalf("a defined runner is of the definition's kind: %s", kind)
	}
	facts := map[string]string{}
	for _, f := range c.RunnerFacts() {
		facts[f.Name] = f.Kind + "/" + f.Model + "/" + f.Provider
	}
	if facts["claude-opus"] != "claude/opus/anthropic" || facts["glm-5-3"] != "opencode/zai-coding-plan/glm-5.3/zai" {
		t.Fatalf("facts: %v", facts)
	}
	// One login, one quota: a limit hit as opus stops sonnet too.
	opus.MarkExhausted(now.Add(time.Hour), time.Minute, now)
	if c.AccountFor("claude-sonnet", now) != nil {
		t.Fatal("the same login's other runner must be exhausted too")
	}
	if got := strings.Join(c.AvailableRunners(now), ","); got != "glm-5-3" {
		t.Fatalf("available: %s", got)
	}
	// The definitions go: nothing is offered again.
	c.ApplyRunnerDefs(nil)
	if len(c.UsedRunners()) != 0 {
		t.Fatal("without definitions a kind account feeds nothing")
	}
}

// An account that names its runner keeps working; when the server defines
// that name the definition wins, and a name the server defines as another
// kind, or with a provider this worker's endpoint contradicts, is not
// offered.
func TestDefinitionsOverNamedAccounts(t *testing.T) {
	c := defsConfig(t,
		&Account{Name: "c1", Runner: "claude", Model: "sonnet"},
		&Account{Name: "g1", Runner: "glm", Model: "zai-coding-plan/glm-5.3"},
	)
	if got := strings.Join(c.UsedRunners(), ","); got != "claude,glm" {
		t.Fatalf("named accounts need no definitions: %s", got)
	}
	notes := c.ApplyRunnerDefs(&wsconfig.Fleet{Rev: "f1", Runners: []wsconfig.RunnerDef{
		{Name: "claude", Kind: "claude", Model: "opus", Effort: "max"},
		{Name: "glm", Kind: "claude"},
	}})
	a := c.AccountFor("claude", time.Now())
	if a == nil || a.Model != "opus" || a.Effort != "max" {
		t.Fatalf("the definition's model replaces the account's: %+v", a)
	}
	if got := strings.Join(c.UsedRunners(), ","); got != "claude" {
		t.Fatalf("glm is opencode here and claude on the server: not offered (%s)", got)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "the account's own sonnet is not used") || !strings.Contains(joined, "runner glm is opencode here and the server defines it as claude") {
		t.Fatalf("notes:\n%s", joined)
	}
	// The file's accounts are untouched: without definitions they are back.
	c.ApplyRunnerDefs(nil)
	if a := c.AccountFor("claude", time.Now()); a == nil || a.Model != "sonnet" {
		t.Fatalf("the file's own account: %+v", a)
	}
	// A declared provider the endpoint contradicts: not offered.
	notes = c.ApplyRunnerDefs(&wsconfig.Fleet{Rev: "f2", Runners: []wsconfig.RunnerDef{{Name: "claude", Kind: "claude", Provider: "local"}}})
	if slices.Contains(c.UsedRunners(), "claude") || !strings.Contains(strings.Join(notes, "\n"), "runner claude is not offered: the server declares provider local") {
		t.Fatalf("declared local, the endpoint is anthropic's: %v %v", c.UsedRunners(), notes)
	}
}

// A kind account may not also name a runner, model or effort, and its kind
// must be one the server can define.
func TestKindAccountValidation(t *testing.T) {
	for _, a := range []*Account{
		{Name: "x", Kind: "claude", Model: "opus"},
		{Name: "x", Kind: "claude", Runner: "claude"},
		{Name: "x", Kind: "command"},
		{Kind: "claude"},
	} {
		c := &Config{ID: "worker-a", Repos: map[string]*Repo{"ws": {Path: "/c"}}, Accounts: []*Account{a}}
		if err := c.Default(t.TempDir()); err == nil {
			t.Errorf("%+v must be refused", a)
		}
	}
}

// A resumed run finds its account by name and runner: one login feeds
// several defined runners.
func TestPickAccountAmongSharedLogins(t *testing.T) {
	c := defsConfig(t, &Account{Name: "claude-1", Kind: "claude"})
	c.ApplyRunnerDefs(&wsconfig.Fleet{Rev: "f1", Runners: []wsconfig.RunnerDef{
		{Name: "claude-opus", Kind: "claude", Model: "opus"}, {Name: "claude-sonnet", Kind: "claude", Model: "sonnet"}}})
	w := &Worker{cfg: c, accounts: map[string]string{"run1": "claude-1"}, sessions: map[string]string{}, executors: map[string]string{}, grants: map[string]string{}, costs: map[string]float64{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a := w.pickAccount(&Run{ID: "run1", Runner: "claude-sonnet", Stage: "plan"}, w.log)
	if a == nil || a.Model != "sonnet" || w.accounts["run1"] != "claude-1" {
		t.Fatalf("the run's account for its runner: %+v", a)
	}
}

// A repo whose file names only the push token is complete when the server
// manages the workspace (the kind and URL come from there). For a workspace
// the server does not manage it would never push: reported, not ignored.
func TestTokenOnlyPushNeedsAManagedWorkspace(t *testing.T) {
	c := &Config{ID: "worker-a", StateDir: t.TempDir(), ServerCommands: true, Repos: map[string]*Repo{"ws": {Path: "/clones/ws"}},
		Accounts: []*Account{{Name: "c1", Runner: "claude"}}}
	c.Repos["ws"].Push.TokenEnv = "PUSH_TOKEN"
	if err := c.Default(t.TempDir()); err != nil {
		t.Fatalf("a push block with only the token's variable must load: %v", err)
	}
	managed := &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{
		Push: &wsconfig.Push{Kind: new("github"), RemoteURL: new("https://github.com/a/b.git")}}}
	s := &configServer{effective: map[string]*wsconfig.Effective{}}
	w, _ := managedWorker(t, c, s)
	ctx := context.Background()
	w.refreshConfig(ctx)
	if got := w.checkHealth(ctx)["ws"]; !strings.Contains(got, "push.token_env is set without push.kind") {
		t.Fatalf("not managed: the workspace is unhealthy with the reason, got %q", got)
	}
	s.mu.Lock()
	s.effective["ws"] = managed
	s.mu.Unlock()
	w.refreshConfig(ctx)
	if got := w.checkHealth(ctx)["ws"]; got != "" {
		t.Fatalf("managed: the server's push settings complete it, got %q", got)
	}
	if r := w.repo("ws"); r.Push.Kind != "github" || r.Push.TokenEnv != "PUSH_TOKEN" || !r.PushSteps() {
		t.Fatalf("the merged push: %+v", r.Push)
	}
	// A remote URL without a kind is still the old mistake.
	bad := &Config{ID: "worker-a", Repos: map[string]*Repo{"ws": {Path: "/c"}}}
	bad.Repos["ws"].Push.RemoteURL = "https://github.com/a/b.git"
	if err := bad.Default(t.TempDir()); err == nil {
		t.Fatal("a remote URL without push.kind must still be refused")
	}
}

// A worker that keeps its own commands and whose file has none of one kind
// (no preflight, say) must merge cleanly: that list is empty, not missing.
func TestServerCommandsOffWithNoLocalCommands(t *testing.T) {
	c := &Config{ID: "worker-a", StateDir: t.TempDir(), Repos: map[string]*Repo{"ws": {Path: "/clones/ws", Checks: []string{"local check"}}},
		Accounts: []*Account{{Name: "c1", Runner: "claude"}}}
	if err := c.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m, err := c.mergeManaged("ws", &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{
		Setup: strs("server setup"), Preflight: strs("server preflight")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.repo.Setup) != 0 || len(m.repo.Preflight) != 0 || m.repo.Checks[0] != "local check" {
		t.Fatalf("the file's commands, empty where it has none: %+v", m.repo)
	}
	// And a workspace the server manages with nothing set at all.
	if _, err := c.mergeManaged("ws", &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r2"}); err != nil {
		t.Fatal(err)
	}
}

// The deny list the worker's file sets stays in force for a managed
// workspace, on top of the workspace's; a file that sets none adds nothing
// (the server's list, or the default, is then the whole of it).
func TestFileDenyListStaysInForce(t *testing.T) {
	c := managedConfig(t, true) // sets mcp_deny = ["mcp__local__deny"]
	m, err := c.mergeManaged("ws", &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{MCPDeny: strs()}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.mcpDeny, []string{"mcp__local__deny"}) {
		t.Fatalf("the server denies nothing, the file's list still applies: %v", m.mcpDeny)
	}
	m, _ = c.mergeManaged("ws", &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r2", Config: wsconfig.Config{MCPDeny: strs("mcp__srv__write")}})
	if !slices.Equal(m.mcpDeny, []string{"mcp__srv__write", "mcp__local__deny"}) {
		t.Fatalf("both lists: %v", m.mcpDeny)
	}
	if got := strings.Join(c.ignoredFor("ws"), ","); !strings.Contains(got, "plan_timeout") || !strings.Contains(got, "mcp_servers") || strings.Contains(got, "mcp_deny") {
		t.Fatalf("the worker-wide fields a managed workspace does not use are named; mcp_deny is not ignored: %s", got)
	}
	plain := &Config{ID: "worker-a", Repos: map[string]*Repo{"ws": {Path: "/c"}}, Accounts: []*Account{{Name: "c1", Runner: "claude"}}}
	if err := plain.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m, _ = plain.mergeManaged("ws", &wsconfig.Effective{Workspace: "ws", Managed: true, Rev: "r1", Config: wsconfig.Config{MCPDeny: strs()}})
	if len(m.mcpDeny) != 0 {
		t.Fatalf("a file without a deny list adds none: %v", m.mcpDeny)
	}
}

// A proxy variable in an account's env is a startup problem whatever the
// account feeds: one that names a kind feeds nothing until the server's
// definitions arrive, and the check runs before that.
func TestFactProblemsCoverKindAccounts(t *testing.T) {
	c := defsConfig(t, &Account{Name: "claude-1", Kind: "claude", Env: map[string]string{"HTTPS_PROXY": "http://proxy:3128"}})
	problems := strings.Join(c.FactProblems(), "\n")
	if !strings.Contains(problems, "account claude-1: env sets HTTPS_PROXY") {
		t.Fatalf("problems: %q", problems)
	}
}

// worker.runners restricts what is offered: a defined runner the accounts
// feed and the list leaves out is named, not silently dropped.
func TestExplicitRunnersListIsNoted(t *testing.T) {
	c := &Config{ID: "worker-a", Runners: []string{"claude-opus"}, Repos: map[string]*Repo{"ws": {Path: "/c"}},
		Accounts: []*Account{{Name: "claude-1", Kind: "claude"}}}
	if err := c.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	notes := c.ApplyRunnerDefs(&wsconfig.Fleet{Rev: "f1", Runners: []wsconfig.RunnerDef{
		{Name: "claude-opus", Kind: "claude", Model: "opus"}, {Name: "claude-sonnet", Kind: "claude", Model: "sonnet"}}})
	if got := strings.Join(c.AvailableRunners(time.Now()), ","); got != "claude-opus" {
		t.Fatalf("only what the list names is offered: %s", got)
	}
	if joined := strings.Join(notes, "\n"); !strings.Contains(joined, "claude-sonnet") || !strings.Contains(joined, "worker.runners does not list it") {
		t.Fatalf("notes: %v", notes)
	}
}

// A server that answers and refuses the config (the key may not read it)
// must not hold the worker at startup for ever; one that is down is waited
// for.
func TestStartupDoesNotWaitOnARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fleet/config" {
			_, _ = w.Write([]byte(`{"rev":"","runners":[]}`))
			return
		}
		http.Error(w, `{"error":"this key lacks workspaces:read"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	c := managedConfig(t, true)
	c.PollEvery = time.Hour // a wait would hang the test
	w := New(c, NewClient(srv.URL, "k"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() { w.loadServerConfig(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a refused fetch must not block startup")
	}
	if w.repo("ws").Checks[0] != "local check" {
		t.Fatal("idle work uses the file")
	}
	if !refused(io.EOF) == false || refused(nil) {
		t.Fatal("only a 4xx answer counts as a refusal")
	}
}
