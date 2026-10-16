package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Config is the worker's TOML config ([worker] section of skopos-config).
// A worker is deployment-agnostic: it needs only skopos over HTTP, a local
// clone per repo, and the agent CLIs installed on the host.
type Config struct {
	ID         string           `toml:"id"`
	StateDir   string           `toml:"state_dir"`
	Runners    []string         `toml:"runners"`    // what this instance offers ("claude", ...)
	Workspaces []string         `toml:"workspaces"` // scope: must match the repos below
	PollEvery  time.Duration    `toml:"poll_every"` // idle claim interval
	Repos      map[string]*Repo `toml:"repos"`

	// Accounts is the placement pool (lean 1b item 3): per-account
	// credentials picked at RUN START; stickiness is the run's affinity —
	// a session belongs to the account that created it, so resumes reuse it
	// and rotation happens only for new runs.
	Accounts []*Account `toml:"accounts"`

	// Defaults for agent runs.
	Bin         string        `toml:"claude_bin"`
	PlanTimeout time.Duration `toml:"plan_timeout"`
	ImplTimeout time.Duration `toml:"impl_timeout"`
	// MCPDeny is the per-deployment deny list (R12 default: Fortix
	// production writes + scriptling stores). Applied on top of the
	// hard-coded git/network denials.
	MCPDeny []string `toml:"mcp_deny"`
	// MCPServers are allowlisted server prefixes for agent runs (the plan
	// prompt references the skopos MCP tools; without these the allowlist
	// denies them).
	MCPServers []string `toml:"mcp_servers"`
	// MCPConfig is the JSON file (Claude Code `mcpServers` shape) that
	// defines the MCP servers pipeline runs get — passed with
	// --strict-mcp-config. Runs no longer read the worker user's
	// ~/.claude.json (they load no user/project settings at all), so
	// mcp_servers without mcp_config would silently allow nothing.
	MCPConfig string `toml:"mcp_config"`

	// Profiles are operator-supplied executor settings per stage, shared by
	// repos (repo.profile names one). skopos never provides isolation
	// itself — a profile says how the operator contains a stage, and only a
	// contained stage may be granted capabilities (see StageProfile).
	Profiles map[string]map[string]*StageProfile `toml:"profiles"`

	// RunnerSpecs configure runner names beyond the defaults (claude and
	// opencode need no block; "command" runs any CLI, wrapper-only).
	RunnerSpecs map[string]*RunnerSpec `toml:"runner_config"`
}

// Stages a profile may configure.
var profileStages = map[string]bool{"plan": true, "implement": true, "review": true, "qa": true}

// StageProfile is how the operator runs one stage. Containment is the
// operator's: a wrapper (argv prefix — a container, a VM, a sandbox tool)
// or Claude Code's own sandbox via settings. Capabilities — extra tools,
// extra env, settings allow rules — are only valid on a contained stage
// (fail closed: skopos cannot verify a wrapper isolates, but it refuses to
// grant capabilities when nothing at all contains them).
type StageProfile struct {
	Wrapper  []string          `toml:"wrapper"`  // argv prefix; {dir} {run} {stage} {workspace}
	Settings string            `toml:"settings"` // JSON merged into --settings (skopos's deny list is kept)
	Tools    []string          `toml:"tools"`    // extra --allowedTools entries
	Env      map[string]string `toml:"env"`      // extra agent env; ${VAR} expands from the environment
}

// sandboxed reports whether Settings enables the runner's own sandbox.
func (p *StageProfile) sandboxed() bool {
	if p == nil || strings.TrimSpace(p.Settings) == "" {
		return false
	}
	var s struct {
		Sandbox struct {
			Enabled bool `json:"enabled"`
		} `json:"sandbox"`
	}
	return json.Unmarshal([]byte(p.Settings), &s) == nil && s.Sandbox.Enabled
}

// Contained: the stage runs inside something the operator provides.
func (p *StageProfile) Contained() bool {
	return p != nil && (len(p.Wrapper) > 0 || p.sandboxed())
}

// grantsAllow reports permissions.allow entries in Settings — a capability
// grant like Tools.
func (p *StageProfile) grantsAllow() bool {
	if p == nil || strings.TrimSpace(p.Settings) == "" {
		return false
	}
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(p.Settings), &s)
	return len(s.Permissions.Allow) > 0
}

// validate checks one stage profile: parseable settings, and the fail-closed
// rule — capabilities only on a contained stage.
func (p *StageProfile) validate(where string) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.Settings) != "" {
		var probe map[string]any
		if err := json.Unmarshal([]byte(p.Settings), &probe); err != nil {
			return fmt.Errorf("%s: settings is not valid JSON: %v", where, err)
		}
	}
	if (len(p.Tools) > 0 || len(p.Env) > 0 || p.grantsAllow()) && !p.Contained() {
		return fmt.Errorf("%s: grants capabilities (tools/env/settings allow) but is not contained — set a wrapper, or settings with sandbox.enabled = true (skopos only grants capabilities to stages the operator contains)", where)
	}
	return nil
}

// Executor identifies how a stage ran, for the run record: "local", or
// "wrapper:"/"sandbox:" plus a short hash over the resolved profile
// (wrapper argv, settings, tools, env NAMES — never values).
func (p *StageProfile) Executor() string {
	if p == nil {
		return "local"
	}
	kind := "local"
	switch {
	case len(p.Wrapper) > 0:
		kind = "wrapper"
	case p.sandboxed():
		kind = "sandbox"
	}
	if kind == "local" && len(p.Tools) == 0 && len(p.Env) == 0 && strings.TrimSpace(p.Settings) == "" {
		return "local"
	}
	names := make([]string, 0, len(p.Env))
	for k := range p.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	tools := append([]string{}, p.Tools...)
	sort.Strings(tools)
	buf, _ := json.Marshal(struct {
		W []string `json:"w"`
		S string   `json:"s"`
		T []string `json:"t"`
		E []string `json:"e"`
	}{p.Wrapper, strings.TrimSpace(p.Settings), tools, names})
	sum := sha256.Sum256(buf)
	return kind + ":" + hex.EncodeToString(sum[:])[:12]
}

// StageProfile resolves a workspace's profile for a stage: the repo's own
// stages override its named profile, per stage. Nil = plain local run.
func (c *Config) StageProfile(workspace, stage string) *StageProfile {
	r := c.Repos[workspace]
	if r == nil {
		return nil
	}
	if p := r.Stages[stage]; p != nil {
		return p
	}
	if r.Profile != "" {
		return c.Profiles[r.Profile][stage]
	}
	return nil
}

// Repo is one workspace's local clone: worktrees are cut from it per run.
type Repo struct {
	Path          string   `toml:"path"`
	DefaultBranch string   `toml:"default_branch"`
	BranchPrefix  string   `toml:"branch_prefix"` // default "agent/"
	Setup         []string `toml:"setup"`         // run in every fresh worktree ({clone} expands)
	// SetupEnv is extra environment for setup commands (they run with the
	// scrubbed environment — e.g. a private registry token via ${VAR}).
	SetupEnv  map[string]string `toml:"setup_env"`
	Checks    []string          `toml:"checks"` // verification commands
	Protected []string          `toml:"protected_branches"`
	// Profile names an entry of worker.profiles; Stages overrides single
	// stages of it (or stands alone).
	Profile string                   `toml:"profile"`
	Stages  map[string]*StageProfile `toml:"stages"`

	// Push: remote-agnostic. Token from env (never argv); Kind selects the
	// credential flavor — "gitea" (token auth header) is first, "github"
	// (App installation token) plugs in with the same shape.
	Push struct {
		Kind      string `toml:"kind"`       // gitea | github | none
		RemoteURL string `toml:"remote_url"` // https push URL
		TokenEnv  string `toml:"token_env"`  // env var holding the token
	} `toml:"push"`
}

// Account is one credential in the placement pool.
type Account struct {
	Name     string            `toml:"name"`     // "claude-1", "gemini"
	Runner   string            `toml:"runner"`   // which runner it feeds
	Priority int               `toml:"priority"` // lower = preferred
	Model    string            `toml:"model"`
	Env      map[string]string `toml:"env"` // ANTHROPIC_API_KEY / CLAUDE_CONFIG_DIR / ...

	// Quota state is worker-local (learned from usage-limit errors, exactly
	// as agent-trial does); not part of the config file.
	exhaustedUntil time.Time `toml:"-"`
}

// AccountFor picks the account for a NEW run on a runner: available
// (quota-healthy) accounts in priority order. Nil when none is available.
func (c *Config) AccountFor(runner string, now time.Time) *Account {
	var best *Account
	for _, a := range c.Accounts {
		if accountRunner(a) != runner {
			continue
		}
		if now.Before(a.exhaustedUntil) {
			continue
		}
		if best == nil || a.Priority < best.Priority {
			best = a
		}
	}
	return best
}

// accountRunner is the runner an account feeds ("" means claude).
func accountRunner(a *Account) string {
	if a.Runner == "" {
		return "claude"
	}
	return a.Runner
}

// NextAvailable reports whether any account feeds runner, and when the
// first of them is usable again (zero = available now).
func (c *Config) NextAvailable(runner string, now time.Time) (time.Time, bool) {
	var next time.Time
	found := false
	for _, a := range c.Accounts {
		if accountRunner(a) != runner {
			continue
		}
		found = true
		if !now.Before(a.exhaustedUntil) {
			return time.Time{}, true
		}
		if next.IsZero() || a.exhaustedUntil.Before(next) {
			next = a.exhaustedUntil
		}
	}
	return next, found
}

// AvailableRunners is what a claim offers: the configured runners that
// have an account usable right now. Offering a runner whose accounts are
// all quota-exhausted (or that no account feeds) would claim runs the
// worker cannot start — each one paused with affinity to this worker.
func (c *Config) AvailableRunners(now time.Time) []string {
	out := []string{}
	for _, r := range c.Runners {
		if at, ok := c.NextAvailable(r, now); ok && at.IsZero() {
			out = append(out, r)
		}
	}
	return out
}

// anyAccountAvailable: at least one account can start a run now.
func (c *Config) anyAccountAvailable(now time.Time) bool {
	for _, a := range c.Accounts {
		if !now.Before(a.exhaustedUntil) {
			return true
		}
	}
	return false
}

// MarkExhausted records a quota death until the reset time (or a backoff).
func (a *Account) MarkExhausted(until time.Time, defaultBackoff time.Duration, now time.Time) {
	if until.IsZero() || !until.After(now) {
		until = now.Add(defaultBackoff)
	}
	a.exhaustedUntil = until
}

// Default fills unset fields with working defaults. It also normalizes
// operator-facing paths: `~` prefixes are expanded (StateDir and repo Path —
// the config template writes ~/... and neither launchd nor git expand it),
// and ${VAR} in account env values is expanded from the process environment
// (the env file has been loaded by then) so token VALUES stay out of the
// TOML (review M4).
func (c *Config) Default(home string) error {
	if c.ID == "" {
		h, _ := os.Hostname()
		c.ID = "worker-" + h
	} else if !strings.HasPrefix(c.ID, "worker-") {
		// The prefix marks worker-created plans (author_agent_id) so the
		// server's orphan cleanup only ever deletes those, never a plan
		// someone made by hand.
		return fmt.Errorf("worker id must start with %q", "worker-")
	}
	if c.StateDir == "" {
		c.StateDir = filepath.Join(home, ".local", "state", "skopos-worker")
	}
	c.StateDir = expandHome(c.StateDir, home)
	if c.PollEvery == 0 {
		c.PollEvery = 30 * time.Second
	}
	if c.Bin == "" {
		c.Bin = "claude"
	}
	if c.PlanTimeout == 0 {
		c.PlanTimeout = 10 * time.Minute
	}
	if c.ImplTimeout == 0 {
		c.ImplTimeout = 90 * time.Minute
	}
	if len(c.MCPDeny) == 0 {
		// R12 default: Fortix production writes + scriptling stores.
		c.MCPDeny = []string{
			"mcp__llmrouter__execute_tool",
			"mcp__llmrouter__fortix__create_work_request",
			"mcp__llmrouter__fortix__reply_to_work_request",
			"mcp__llmrouter__fortix__create_client_from_email",
			"mcp__llmrouter__fortix__request_file_upload",
			"mcp__llmrouter__scriptling__qdrant_upsert",
			"mcp__llmrouter__scriptling__set_prompt",
			"mcp__llmrouter__scriptling__set_skill",
		}
	}
	// runners defaults to every runner the accounts feed: adding a runner
	// is adding an account (an explicit list still restricts what claims
	// offer).
	if len(c.Runners) == 0 {
		c.Runners = c.UsedRunners()
	}
	if len(c.Workspaces) == 0 {
		for ws := range c.Repos {
			c.Workspaces = append(c.Workspaces, ws)
		}
	}
	for ws, r := range c.Repos {
		r.Path = expandHome(r.Path, home)
		if r.DefaultBranch == "" {
			r.DefaultBranch = "main"
		}
		if r.BranchPrefix == "" {
			r.BranchPrefix = "agent/"
		}
		if len(r.Protected) == 0 {
			r.Protected = []string{"PROD", "main", "master", "develop", "release/**", "hotfix/**"}
		}
		if r.Push.TokenEnv == "" && r.Push.Kind != "" && r.Push.Kind != "none" {
			return fmt.Errorf("repo %s: push.token_env is required (tokens come from the environment)", ws)
		}
		if r.Push.Kind == "" {
			r.Push.Kind = "none"
		}
	}
	if c.MCPConfig != "" {
		c.MCPConfig = expandHome(c.MCPConfig, home)
	} else if len(c.MCPServers) > 0 {
		return fmt.Errorf("mcp_servers is set but mcp_config is not: pipeline runs no longer load the worker user's MCP servers (~/.claude.json) — point mcp_config at a JSON file with those servers (mcpServers shape)")
	}
	for name, spec := range c.RunnerSpecs {
		if spec == nil {
			continue
		}
		kind := spec.Kind
		if kind == "" {
			kind = name
		}
		if !runnerKinds[kind] {
			return fmt.Errorf("runner_config.%s: unknown kind %q (claude, opencode, command)", name, kind)
		}
		if kind == "command" && len(spec.Command) == 0 {
			return fmt.Errorf("runner_config.%s: kind command needs command = [...]", name)
		}
		spec.Config = expandHome(spec.Config, home)
	}
	for _, a := range c.Accounts {
		if _, err := c.RunnerFor(a.Runner); err != nil {
			return fmt.Errorf("account %s: %v", a.Name, err)
		}
	}
	for name, stages := range c.Profiles {
		for stage, p := range stages {
			if !profileStages[stage] {
				return fmt.Errorf("profile %s: unknown stage %q (plan, implement, review, qa)", name, stage)
			}
			if err := p.validate("profile " + name + "." + stage); err != nil {
				return err
			}
			expandProfileEnv(p, home)
		}
	}
	for ws, r := range c.Repos {
		if r.Profile != "" && c.Profiles[r.Profile] == nil {
			return fmt.Errorf("repo %s: profile %q is not defined under worker.profiles", ws, r.Profile)
		}
		for stage, p := range r.Stages {
			if !profileStages[stage] {
				return fmt.Errorf("repo %s: unknown stage %q (plan, implement, review, qa)", ws, stage)
			}
			if err := p.validate("repo " + ws + " stage " + stage); err != nil {
				return err
			}
			expandProfileEnv(p, home)
		}
		for k, v := range r.SetupEnv {
			r.SetupEnv[k] = expandHome(expandEnvRefs(v), home)
		}
	}
	for _, a := range c.Accounts {
		for k, v := range a.Env {
			// ${NAME} only: os.Expand rewrites every $, silently mangling
			// literal values that merely contain one. Unset names expand
			// empty — the checker flags that. A leading ~ expands too
			// (CLAUDE_CONFIG_DIR = "~/.claude" in the template comment).
			v = expandEnvRefs(v)
			a.Env[k] = expandHome(v, home)
		}
	}
	return nil
}

// expandProfileEnv resolves ${VAR} (and a leading ~) in a stage's env once,
// at load — the same treatment as account env.
func expandProfileEnv(p *StageProfile, home string) {
	if p == nil {
		return
	}
	for k, v := range p.Env {
		p.Env[k] = expandHome(expandEnvRefs(v), home)
	}
}

var reEnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvRefs expands only the ${NAME} form from the environment.
func expandEnvRefs(s string) string {
	return reEnvRef.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

// expandHome expands a leading ~ (never ~user) against home.
func expandHome(p, home string) string {
	if len(p) > 1 && p[0] == '~' {
		return filepath.Join(home, p[1:])
	}
	return p
}

// TokenEnvs lists the configured push-token env vars (repos.*.push.token_env)
// — scrubbed from every agent environment.
func (c *Config) TokenEnvs() []string {
	var out []string
	for _, r := range c.Repos {
		if r.Push.TokenEnv != "" {
			out = append(out, r.Push.TokenEnv)
		}
	}
	return out
}

// normalizeBranch makes a safe, collision-free branch name: the item id's
// tail guarantees uniqueness (two same-titled items must never share a
// branch or worktree — review finding 5), then a readable slug.
func normalizeBranch(itemID, title, prefix string) string {
	slug := branchSlug(title)
	// Last TWO segments of the UUIDv7 text id: enough to be unique and
	// time-recognizable without the full 36 chars.
	segs := strings.Split(itemID, "-")
	tail := itemID
	if len(segs) >= 2 {
		tail = segs[len(segs)-2] + "-" + segs[len(segs)-1]
	}
	return prefix + tail + "-" + slug
}

func branchSlug(title string) string {
	s := strings.ToLower(title)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 30 { // keep branch readable: tail (~13) + slug (<=30)
		out = out[:30]
	}
	if out == "" {
		out = "item"
	}
	return out
}
