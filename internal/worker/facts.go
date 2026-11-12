package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/martinsuchenak/skopos/internal/policy"
)

// Runner facts (tmp/central-config — design.md, phase 1a): what the worker
// tells the server about each runner it offers — the CLI, the model, and
// above all where the code goes: the endpoint the CLI sends its requests
// to and the provider behind it. The server matches a workspace's policy
// against them at every claim.
//
// The endpoint is read from where each CLI reads it; the provider is
// declared on the runner ([worker.runner_config.<name>] provider = "…") or
// derived from the endpoint (internal/policy). A model on another machine
// on the local network, or behind a router that forwards to several
// companies, cannot be derived: declare it.

// readOnlyStages are the stages a read-only runner (codex) can run:
// every stage but implement.
var readOnlyStages = []string{"branch_review", "compare", "plan", "qa", "review", "second_opinion", "synthesize"}

// proxyEnv and endpointEnv: the variables that change where an agent
// CLI's requests go (internal/policy/env.go — the server checks a stage's
// env against the same lists).
var (
	proxyEnv    = policy.ProxyEnv
	endpointEnv = policy.EndpointEnv
)

// SetEnvFileKeys names the variables the worker's env file provided: the
// scrub keeps them from every agent run, so they say nothing about where
// an agent's requests go.
func (c *Config) SetEnvFileKeys(keys []string) { c.envFileKeys = keys }

// agentEnv is the environment an agent run of this account really gets:
// the daemon's own, scrubbed exactly as the runners scrub it, with the
// account's env on top. The facts are derived from this and nothing else —
// a variable the scrub drops (anything from the env file, anything named
// like a credential) never reaches the CLI, so it must not shape them.
func (c *Config) agentEnv(a *Account) map[string]string {
	out := map[string]string{}
	for _, kv := range scrubEnv(a.Env, c.TokenEnvs(), c.envFileKeys) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// endpointOf is the host of a configured base URL, def when none is set,
// and "invalid" for one that is set but has no host: something is
// configured there, and it is not the default vendor.
func endpointOf(value, def string) string {
	if strings.TrimSpace(value) == "" {
		return def
	}
	if h := policy.Host(value); h != "" {
		return h
	}
	return "invalid"
}

// opencodeBaseURL reads provider.<id>.options.baseURL from an opencode
// config ("" when the file or the entry is missing).
func opencodeBaseURL(configPath, providerID string) string {
	if configPath == "" || providerID == "" {
		return ""
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var cfg struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return ""
	}
	return cfg.Provider[providerID].Options.BaseURL
}

// accountFacts derives one account's facts for its runner, and what the
// endpoint alone says about the provider (derived: before a declaration
// replaces it — FactProblems compares the two).
func (c *Config) accountFacts(a *Account) (f policy.Facts, derived string) {
	name := accountRunner(a)
	spec, kind := c.runnerSpec(name)
	env := c.agentEnv(a)
	f = policy.Facts{Name: name, Kind: kind, Model: a.Model, Effort: a.Effort}
	switch kind {
	case "claude":
		switch {
		case truthy(env["CLAUDE_CODE_USE_BEDROCK"]):
			f.Endpoint = endpointOf(env["ANTHROPIC_BEDROCK_BASE_URL"], "bedrock-runtime.amazonaws.com")
		case truthy(env["CLAUDE_CODE_USE_VERTEX"]):
			f.Endpoint = endpointOf(env["ANTHROPIC_VERTEX_BASE_URL"], "aiplatform.googleapis.com")
		case truthy(env["CLAUDE_CODE_USE_FOUNDRY"]):
			f.Endpoint = endpointOf(env["ANTHROPIC_FOUNDRY_BASE_URL"], "services.ai.azure.com")
		default:
			f.Endpoint = endpointOf(env["ANTHROPIC_BASE_URL"], "api.anthropic.com")
		}
		f.Provider = policy.ProviderForEndpoint(f.Endpoint)
	case "codex":
		f.Endpoint = endpointOf(env["OPENAI_BASE_URL"], "api.openai.com")
		f.Provider = policy.ProviderForEndpoint(f.Endpoint)
	case "opencode":
		id, _, _ := strings.Cut(strings.TrimSpace(a.Model), "/")
		id = strings.ToLower(id)
		// The runner passes its own config as OPENCODE_CONFIG; without
		// one, a config named in the agent's environment is what opencode
		// reads.
		config := spec.Config
		if config == "" {
			config = env["OPENCODE_CONFIG"]
		}
		baseURL := opencodeBaseURL(config, id)
		// opencode's built-in anthropic and openai providers honour the
		// SDKs' base URL variables.
		if baseURL == "" {
			switch id {
			case "anthropic":
				baseURL = env["ANTHROPIC_BASE_URL"]
			case "openai":
				baseURL = env["OPENAI_BASE_URL"]
			}
		}
		f.Provider, f.Endpoint = policy.ProviderForOpencode(a.Model, baseURL)
		if baseURL != "" && f.Endpoint == "" {
			f.Provider, f.Endpoint = policy.UnknownPrefix+"invalid", "invalid"
		}
	case "antigravity":
		f.Endpoint = "googleapis.com"
		f.Provider = "google"
	default: // command: any CLI; nothing can be derived
		f.Provider = policy.UnknownPrefix + "command"
	}
	// A declared endpoint replaces the derived one; the provider follows it.
	if strings.TrimSpace(spec.Endpoint) != "" {
		f.Endpoint = endpointOf(spec.Endpoint, "")
		f.Provider = policy.ProviderForEndpoint(f.Endpoint)
	}
	derived = f.Provider
	if p := strings.ToLower(strings.TrimSpace(spec.Provider)); p != "" {
		f.Provider = p
	}
	// The server's definition declares the provider for every worker.
	if d := c.def(name); d != nil && d.Provider != "" {
		f.Provider = d.Provider
	}
	if r, err := c.RunnerFor(name); err == nil {
		if ro, ok := r.(interface{ ReadOnly() bool }); ok && ro.ReadOnly() {
			f.Stages = readOnlyStages
		}
	}
	return f, derived
}

// RunnerFacts lists the facts of every runner the worker has configured,
// in its order of preference: claude first (what a run nothing pins has
// always used), then the configured order. One entry per runner: its
// accounts agree on what a policy matches (FactProblems refuses a config
// where they do not).
func (c *Config) RunnerFacts() []policy.Facts {
	first := map[string]*Account{}
	for _, a := range c.Accounts {
		if _, ok := first[accountRunner(a)]; !ok {
			first[accountRunner(a)] = a
		}
	}
	order := append([]string{}, c.Runners...)
	sort.SliceStable(order, func(i, j int) bool { return order[i] == "claude" && order[j] != "claude" })
	out := make([]policy.Facts, 0, len(order))
	seen := map[string]bool{}
	for _, name := range order {
		a := first[name]
		if a == nil || seen[name] {
			continue
		}
		seen[name] = true
		f, _ := c.accountFacts(a)
		out = append(out, f)
	}
	return out
}

// FactProblems lists what makes the worker's runner facts untrustworthy —
// each a reason not to start:
//   - a proxy variable in an account's env (requests would go through a
//     host the facts do not name);
//   - a stage profile's env that sets a proxy or an endpoint variable: the
//     facts are per runner, and a profile would change them for one
//     workspace and stage only;
//   - accounts of one runner that differ in what a policy matches (model,
//     provider, endpoint): the policy is checked once per runner, so a
//     quota switch to the other account would run something else;
//   - a declared provider that contradicts the endpoint (declared local,
//     the endpoint is a cloud API).
func (c *Config) FactProblems() []string {
	var out []string
	firstFacts := map[string]policy.Facts{}
	firstAccount := map[string]string{}
	// Every login in the file, whatever it feeds: an account that names a
	// kind feeds nothing until the server's definitions arrive, and its
	// proxy would be as wrong then.
	for _, a := range c.FileAccounts() {
		keys := make([]string, 0, len(a.Env))
		for k := range a.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if proxyEnv[strings.ToUpper(k)] {
				out = append(out, fmt.Sprintf("account %s: env sets %s — an agent's requests would go through another host than the one the worker reports; remove it (a proxy for the whole host belongs in the service's environment)", a.Name, k))
			}
		}
	}
	for _, a := range c.Accounts {
		name := accountRunner(a)
		f, derived := c.accountFacts(a)
		if policy.Contradicts(f.Provider, derived) {
			out = append(out, fmt.Sprintf("runner %s: provider is declared %q, but its endpoint %s is %s — fix the declaration or the endpoint", name, f.Provider, f.Endpoint, derived))
		}
		prev, ok := firstFacts[name]
		if !ok {
			firstFacts[name], firstAccount[name] = f, a.Name
			continue
		}
		if prev.Model != f.Model || prev.Provider != f.Provider || prev.Endpoint != f.Endpoint {
			out = append(out, fmt.Sprintf("runner %s: accounts %s (%s) and %s (%s) differ in what a policy matches — give each its own runner name",
				name, firstAccount[name], factsLine(prev), a.Name, factsLine(f)))
		}
	}
	profileEnv := func(where string, p *StageProfile) {
		if p == nil {
			return
		}
		keys := make([]string, 0, len(p.Env))
		for k := range p.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if up := strings.ToUpper(k); proxyEnv[up] || endpointEnv[up] {
				out = append(out, fmt.Sprintf("%s: env sets %s — it would change where the stage's agent sends its requests without the worker reporting it; set it on the runner's account (or declare the runner's provider) instead", where, k))
			}
		}
	}
	profiles := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		profiles = append(profiles, name)
	}
	sort.Strings(profiles)
	for _, name := range profiles {
		for _, stage := range sortedStages(c.Profiles[name]) {
			profileEnv("profile "+name+"."+stage, c.Profiles[name][stage])
		}
	}
	repos := make([]string, 0, len(c.Repos))
	for id := range c.Repos {
		repos = append(repos, id)
	}
	sort.Strings(repos)
	for _, id := range repos {
		for _, stage := range sortedStages(c.Repos[id].Stages) {
			profileEnv("repo "+id+" stage "+stage, c.Repos[id].Stages[stage])
		}
	}
	return out
}

func sortedStages(m map[string]*StageProfile) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func factsLine(f policy.Facts) string {
	parts := []string{"provider " + f.Provider}
	if f.Model != "" {
		parts = append(parts, "model "+f.Model)
	}
	if f.Endpoint != "" {
		parts = append(parts, "endpoint "+f.Endpoint)
	}
	return strings.Join(parts, ", ")
}
