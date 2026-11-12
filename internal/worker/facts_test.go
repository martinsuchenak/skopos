package worker

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/policy"
)

func factsOf(t *testing.T, c *Config) map[string]policy.Facts {
	t.Helper()
	out := map[string]policy.Facts{}
	for _, f := range c.RunnerFacts() {
		out[f.Name] = f
	}
	return out
}

// Where each CLI sends its requests, and so who receives the code: read
// from where the CLI reads it, or declared when it cannot be derived.
func TestRunnerFactsPerKind(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	oc := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(oc, []byte(`{"provider":{"ollama":{"options":{"baseURL":"http://localhost:11434/v1"}},"router":{"options":{"baseURL":"https://llm.lan/v1"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{
		Runners: []string{"opencode-glm", "opencode-local", "opencode-router", "claude", "claude-bedrock", "claude-proxy", "codex", "my-cli", "lan-llm"},
		RunnerSpecs: map[string]*RunnerSpec{
			"opencode-glm":    {Kind: "opencode"},
			"opencode-local":  {Kind: "opencode", Config: oc},
			"opencode-router": {Kind: "opencode", Config: oc, Provider: "router"},
			"claude-bedrock":  {Kind: "claude"},
			"claude-proxy":    {Kind: "claude"},
			"my-cli":          {Kind: "command", Command: []string{"mycli", "{prompt}"}},
			"lan-llm":         {Kind: "command", Command: []string{"mycli", "{prompt}"}, Endpoint: "http://10.0.0.5:8080", Provider: "local"},
		},
		Accounts: []*Account{
			{Name: "glm", Runner: "opencode-glm", Model: "zai-coding-plan/glm-5.3"},
			{Name: "local", Runner: "opencode-local", Model: "ollama/qwen3"},
			{Name: "router", Runner: "opencode-router", Model: "router/qwen3-coder"},
			{Name: "c1", Runner: "claude", Model: "opus", Effort: "high"},
			{Name: "bedrock", Runner: "claude-bedrock", Env: map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}},
			{Name: "proxy", Runner: "claude-proxy", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example/anthropic"}},
			{Name: "codex", Runner: "codex"},
			{Name: "cli", Runner: "my-cli"},
			{Name: "lan", Runner: "lan-llm"},
		},
	}
	got := factsOf(t, c)
	for name, want := range map[string]policy.Facts{
		"claude":          {Kind: "claude", Model: "opus", Effort: "high", Provider: "anthropic", Endpoint: "api.anthropic.com"},
		"claude-bedrock":  {Kind: "claude", Provider: "aws", Endpoint: "bedrock-runtime.amazonaws.com"},
		"claude-proxy":    {Kind: "claude", Provider: "unknown:gateway.example", Endpoint: "gateway.example"},
		"codex":           {Kind: "codex", Provider: "openai", Endpoint: "api.openai.com"},
		"opencode-glm":    {Kind: "opencode", Model: "zai-coding-plan/glm-5.3", Provider: "zai", Endpoint: "opencode:zai-coding-plan"},
		"opencode-local":  {Kind: "opencode", Model: "ollama/qwen3", Provider: "local", Endpoint: "localhost"},
		"opencode-router": {Kind: "opencode", Model: "router/qwen3-coder", Provider: "router", Endpoint: "llm.lan"},
		"my-cli":          {Kind: "command", Provider: "unknown:command"},
		"lan-llm":         {Kind: "command", Provider: "local", Endpoint: "10.0.0.5"},
	} {
		f := got[name]
		if f.Kind != want.Kind || f.Model != want.Model || f.Effort != want.Effort || f.Provider != want.Provider || f.Endpoint != want.Endpoint {
			t.Errorf("%s: %+v, want %+v", name, f, want)
		}
	}
	// A read-only runner says which stages it can run; the others run all.
	if s := got["codex"].Stages; len(s) == 0 || strings.Contains(strings.Join(s, ","), "implement") || !got["codex"].CanRun("review") || got["codex"].CanRun("implement") {
		t.Fatalf("codex stages: %v", s)
	}
	if len(got["claude"].Stages) != 0 {
		t.Fatalf("claude runs every stage: %v", got["claude"].Stages)
	}
	// claude leads the order (a run nothing pins has always used it).
	if order := c.RunnerFacts(); order[0].Name != "claude" || order[1].Name != "opencode-glm" {
		t.Fatalf("order: %v %v", order[0].Name, order[1].Name)
	}
	if problems := c.FactProblems(); len(problems) != 0 {
		t.Fatalf("this config is sound: %v", problems)
	}
	// The daemon's own environment counts too: it reaches the agent.
	t.Setenv("ANTHROPIC_BASE_URL", "https://corp-gateway.example")
	if f := factsOf(t, c)["claude"]; f.Provider != "unknown:corp-gateway.example" {
		t.Fatalf("an inherited ANTHROPIC_BASE_URL is the endpoint: %+v", f)
	}
}

// A config whose facts would be untrue is refused: a proxy in an account's
// env, accounts of one runner that differ in what a policy matches, and a
// declared provider the endpoint contradicts. Accounts that differ only in
// effort are fine (a policy does not match on it).
func TestFactProblems(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		t.Setenv(k, "")
	}
	problems := func(c *Config) string { return strings.Join(c.FactProblems(), "\n") }

	if p := problems(&Config{Accounts: []*Account{
		{Name: "a", Runner: "claude", Model: "opus", Effort: "high"},
		{Name: "b", Runner: "claude", Model: "opus", Effort: "max"},
	}}); p != "" {
		t.Fatalf("effort alone is not a policy fact: %s", p)
	}
	if p := problems(&Config{Accounts: []*Account{
		{Name: "a", Runner: "claude", Env: map[string]string{"https_proxy": "http://proxy:3128"}},
	}}); !strings.Contains(p, "account a: env sets https_proxy") {
		t.Fatalf("a proxy in an account's env: %q", p)
	}
	if p := problems(&Config{Accounts: []*Account{
		{Name: "a", Runner: "claude", Model: "opus"},
		{Name: "b", Runner: "claude", Model: "sonnet"},
	}}); !strings.Contains(p, "accounts a") || !strings.Contains(p, "differ in what a policy matches") {
		t.Fatalf("different models on one runner: %q", p)
	}
	if p := problems(&Config{Accounts: []*Account{
		{Name: "a", Runner: "claude"},
		{Name: "b", Runner: "claude", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example"}},
	}}); !strings.Contains(p, "differ in what a policy matches") {
		t.Fatalf("different endpoints on one runner: %q", p)
	}
	// Declared local, but the endpoint is a cloud API.
	if p := problems(&Config{
		RunnerSpecs: map[string]*RunnerSpec{"opencode-glm": {Kind: "opencode", Provider: "local"}},
		Accounts:    []*Account{{Name: "a", Runner: "opencode-glm", Model: "zai-coding-plan/glm-5.3"}},
	}); !strings.Contains(p, `provider is declared "local"`) || !strings.Contains(p, "is zai") {
		t.Fatalf("a contradicted declaration: %q", p)
	}
	// Declaring what cannot be derived is the point of the setting.
	if p := problems(&Config{
		RunnerSpecs: map[string]*RunnerSpec{"lan": {Kind: "command", Command: []string{"x"}, Endpoint: "http://10.0.0.5:8080", Provider: "local"}},
		Accounts:    []*Account{{Name: "a", Runner: "lan"}},
	}); p != "" {
		t.Fatalf("a LAN model declared local: %s", p)
	}
}

// A run the server could not have checked for this worker is not run.
func TestRefuseRunner(t *testing.T) {
	c := &Config{Runners: []string{"claude"}, Accounts: []*Account{{Name: "a", Runner: "claude"}}}
	w := New(c, NewClient("https://skopos.example", "k"), nil)
	if why := w.refuseRunner(&Run{Runner: ""}); why != "" {
		t.Fatalf("a server from before runner facts hands out unpinned runs: %q", why)
	}
	w.client.factsUsed = true
	if why := w.refuseRunner(&Run{Runner: "claude"}); why != "" {
		t.Fatalf("its own runner: %q", why)
	}
	if why := w.refuseRunner(&Run{Runner: ""}); !strings.Contains(why, "without a runner") {
		t.Fatalf("an unpinned run from a server that reads facts: %q", why)
	}
	if why := w.refuseRunner(&Run{Runner: "opencode-glm"}); !strings.Contains(why, "does not have") {
		t.Fatalf("a runner this worker does not have: %q", why)
	}
}

// The facts are derived from the environment an agent run really gets: a
// variable from the worker's env file never reaches the CLI (the scrub
// drops it), so it must not shape them; one in the daemon's own
// environment does reach it, and counts.
func TestRunnerFactsUseTheAgentsEnvironment(t *testing.T) {
	for _, k := range []string{"OPENAI_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "OPENCODE_CONFIG"} {
		t.Setenv(k, "")
	}
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.example")
	c := &Config{Runners: []string{"claude"}, Accounts: []*Account{{Name: "a", Runner: "claude"}}}
	if f := factsOf(t, c)["claude"]; f.Provider != "unknown:gateway.example" {
		t.Fatalf("a base URL in the daemon's environment reaches the agent: %+v", f)
	}
	// The same variable, but it came from the env file: scrubbed away.
	c.SetEnvFileKeys([]string{"ANTHROPIC_BASE_URL"})
	if f := factsOf(t, c)["claude"]; f.Provider != "anthropic" || f.Endpoint != "api.anthropic.com" {
		t.Fatalf("an env-file variable never reaches the agent, so the CLI talks to Anthropic: %+v", f)
	}
	// So a "local" declaration on top of it is a contradiction, not a pass.
	c.RunnerSpecs = map[string]*RunnerSpec{"claude": {Kind: "claude", Provider: "local"}}
	if p := strings.Join(c.FactProblems(), "\n"); !strings.Contains(p, `declared "local"`) || !strings.Contains(p, "is anthropic") {
		t.Fatalf("declared local while the CLI talks to Anthropic: %q", p)
	}
}

// More ways an endpoint is chosen: Foundry, a base URL that is set but
// unusable, and opencode's config and SDK variables from the environment.
func TestRunnerFactsEndpointSwitches(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "OPENCODE_CONFIG"} {
		t.Setenv(k, "")
	}
	dir := t.TempDir()
	oc := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(oc, []byte(`{"provider":{"zai-coding-plan":{"options":{"baseURL":"https://proxy.example/v1"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{
		Runners: []string{"foundry", "broken", "oc-env-config", "oc-anthropic"},
		RunnerSpecs: map[string]*RunnerSpec{
			"foundry": {Kind: "claude"}, "broken": {Kind: "claude"},
			"oc-env-config": {Kind: "opencode"}, "oc-anthropic": {Kind: "opencode"},
		},
		Accounts: []*Account{
			{Name: "f", Runner: "foundry", Env: map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "1"}},
			{Name: "b", Runner: "broken", Env: map[string]string{"ANTHROPIC_BASE_URL": "://"}},
			{Name: "o", Runner: "oc-env-config", Model: "zai-coding-plan/glm-5.3", Env: map[string]string{"OPENCODE_CONFIG": oc}},
			{Name: "p", Runner: "oc-anthropic", Model: "anthropic/claude-opus", Env: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example"}},
		},
	}
	got := factsOf(t, c)
	if f := got["foundry"]; f.Provider != "microsoft" {
		t.Errorf("Claude on Foundry goes to Microsoft: %+v", f)
	}
	if f := got["broken"]; f.Provider != "unknown:invalid" {
		t.Errorf("a base URL that is set but unusable is not the default vendor: %+v", f)
	}
	if f := got["oc-env-config"]; f.Provider != "unknown:proxy.example" {
		t.Errorf("opencode reads OPENCODE_CONFIG from its environment when the runner sets none: %+v", f)
	}
	if f := got["oc-anthropic"]; f.Provider != "unknown:gateway.example" {
		t.Errorf("opencode's anthropic provider honours ANTHROPIC_BASE_URL: %+v", f)
	}
}

// A stage profile's env may not change where the stage's agent sends its
// requests: the facts are per runner, and a profile would change them for
// one workspace and stage without the server knowing.
func TestFactProblemsProfileEnv(t *testing.T) {
	c := &Config{
		Accounts: []*Account{{Name: "a", Runner: "claude"}},
		Profiles: map[string]map[string]*StageProfile{"ff3": {
			"implement": {Wrapper: []string{"env"}, Env: map[string]string{"CODECEPTION_ENV": "local", "HTTPS_PROXY": "http://proxy:3128"}},
			"review":    {Wrapper: []string{"env"}, Env: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.example"}},
		}},
		Repos: map[string]*Repo{"ws": {Stages: map[string]*StageProfile{"plan": {Wrapper: []string{"env"}, Env: map[string]string{"OPENCODE_CONFIG": "/x.json"}}}}},
	}
	p := strings.Join(c.FactProblems(), "\n")
	for _, want := range []string{"profile ff3.implement: env sets HTTPS_PROXY", "profile ff3.review: env sets ANTHROPIC_BASE_URL", "repo ws stage plan: env sets OPENCODE_CONFIG"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in:\n%s", want, p)
		}
	}
	if strings.Contains(p, "CODECEPTION_ENV") {
		t.Errorf("an ordinary profile variable is fine:\n%s", p)
	}
}

// A run that comes back naming another runner must not carry on with the
// account (and the session) it used before: the server checked the runner
// it names, not the old one.
func TestPickAccountFollowsTheNamedRunner(t *testing.T) {
	c := &Config{StateDir: t.TempDir(), Runners: []string{"opencode-glm", "claude"},
		RunnerSpecs: map[string]*RunnerSpec{"opencode-glm": {Kind: "opencode"}},
		Accounts: []*Account{
			{Name: "glm", Runner: "opencode-glm", Model: "zai-coding-plan/glm-5.3"},
			{Name: "c1", Runner: "claude"},
		}}
	w := New(c, NewClient("https://skopos.example", "k"), slog.New(slog.DiscardHandler))
	log := slog.New(slog.DiscardHandler)
	run := &Run{ID: "r1", WorkspaceID: "ws", Stage: "implement", Runner: "opencode-glm"}
	first := w.pickAccount(run, log)
	if first == nil || first.Name != "glm" {
		t.Fatalf("first: %+v", first)
	}
	w.rememberAccount(run, first)
	w.saveSession(run, "implement", "session-on-glm")
	// The same run on the same runner: the same account, its session kept.
	if again := w.pickAccount(run, log); again == nil || again.Name != "glm" || w.sessions["r1:implement"] != "session-on-glm" {
		t.Fatalf("the same runner keeps its account and session: %+v", again)
	}
	// The run comes back for claude (the policy changed while it waited).
	run.Runner = "claude"
	now := w.pickAccount(run, log)
	if now == nil || now.Name != "c1" {
		t.Fatalf("the run must use an account of the runner it now names, not the remembered GLM one: %+v", now)
	}
	if w.accounts["r1"] != "" || w.sessions["r1:implement"] != "" {
		t.Fatalf("the old account and session must be forgotten: %q %q", w.accounts["r1"], w.sessions["r1:implement"])
	}
}
