package worker

import (
	"fmt"
	"os"
	"path/filepath"
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
}

// Repo is one workspace's local clone: worktrees are cut from it per run.
type Repo struct {
	Path          string   `toml:"path"`
	DefaultBranch string   `toml:"default_branch"`
	BranchPrefix  string   `toml:"branch_prefix"` // default "agent/"
	Setup         []string `toml:"setup"`         // run in every fresh worktree ({clone} expands)
	Checks        []string `toml:"checks"`        // verification commands
	Protected     []string `toml:"protected_branches"`

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
		if a.Runner != runner {
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

// MarkExhausted records a quota death until the reset time (or a backoff).
func (a *Account) MarkExhausted(until time.Time, defaultBackoff time.Duration, now time.Time) {
	if until.IsZero() || !until.After(now) {
		until = now.Add(defaultBackoff)
	}
	a.exhaustedUntil = until
}

// Default fills unset fields with working defaults.
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
	if len(c.Workspaces) == 0 {
		for ws := range c.Repos {
			c.Workspaces = append(c.Workspaces, ws)
		}
	}
	for ws, r := range c.Repos {
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
	return nil
}

// RunnerBin resolves the binary for a runner name (first runner wins for
// now; per-runner bins land with more runner types).
func (c *Config) RunnerBin(runner string) string {
	return c.Bin
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
