package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

// Config is the worker's TOML config ([worker] section of skopos-config).
// A worker is deployment-agnostic: it needs only skopos over HTTP, a local
// clone per repo, and the agent CLIs installed on the host.
type Config struct {
	ID         string        `toml:"id"`
	StateDir   string        `toml:"state_dir"`
	Runners    []string      `toml:"runners"`    // what this instance offers ("claude", ...)
	Workspaces []string      `toml:"workspaces"` // scope: must match the repos below
	PollEvery  time.Duration `toml:"poll_every"` // idle claim interval
	// ContainedBy names the container the worker's whole host is (a Knot
	// space, a VM, a Docker container): every run's executor is then
	// host:<name> unless a wrapper or a sandbox goes around the run itself,
	// and stage profiles may grant capabilities without a wrapper. It says
	// the host is confined — not that runs are kept apart from each other:
	// inside it a run still sees every clone on the worker.
	ContainedBy string `toml:"contained_by"`
	// ServerCommands: take a managed workspace's setup, check and preflight
	// commands from the server. Off, those fields stay this file's even for
	// a workspace the server manages (its other fields still come from the
	// server): whoever may edit them on the server runs commands on this
	// host.
	ServerCommands bool `toml:"server_commands"`
	// AutoUpgrade: an idle worker replaces its own binary with the
	// server's release and restarts (nil = true; design §5).
	AutoUpgrade *bool `toml:"auto_upgrade"`
	// UpgradeURL: where releases are downloaded from — <url>/v<version>/
	// skopos_<os>_<arch> and checksums.txt ("" = the GitHub releases).
	UpgradeURL string           `toml:"upgrade_url"`
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

	// envFileKeys: the variables the worker's env file provided (facts.go).
	envFileKeys []string

	// Runner definitions from the server (defs.go). fileAccounts: the
	// accounts as the file has them; Accounts is rebuilt from them and
	// defs. runnersExplicit: the file lists runners itself, which then
	// restricts what the worker offers.
	fileAccounts    []*Account
	defs            []wsconfig.RunnerDef
	defsRev         string
	runnersExplicit bool
	// mcpDenyFile: the deny list the file itself sets (empty when it sets
	// none and the default applies) — kept in force for a workspace the
	// server manages. globalSet: the worker-wide fields the file sets that
	// are per workspace on the server (managed.go).
	mcpDenyFile []string
	globalSet   []string
}

// FileAccounts lists the accounts as the worker's file has them (Accounts
// is what runs use: these combined with the server's runner definitions).
func (c *Config) FileAccounts() []*Account {
	if c.fileAccounts == nil {
		return c.Accounts
	}
	return c.fileAccounts
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
	Wrapper  []string          `toml:"wrapper"`  // argv prefix; {dir} {gitdir} {login} {rundir} {cli} {run} {stage} {workspace}
	Settings string            `toml:"settings"` // JSON merged into --settings (skopos's deny list is kept)
	Tools    []string          `toml:"tools"`    // extra --allowedTools entries
	Env      map[string]string `toml:"env"`      // extra agent env; ${VAR} expands from the environment

	// name: the profile's name ("repo" for a repo's own stage block), the
	// part of a wrapper's executor id a policy rule can match on.
	name string
}

// hostWrapper recognises a wrapper that only sets variables — `env K=V …`,
// the way a worker inside a container used to say "this stage is
// contained" — and returns the name it declares (SKOPOS_CONTAINED_BY's
// value, else "env"). Such a wrapper puts nothing around the run: the
// containment is the host's, so the run's executor is host:<name>, not a
// wrapper. worker.contained_by says the same thing directly.
//
// An env argv is a real wrapper only when it runs a command of its own
// (`env A=b bwrap …`). Options (-i, -u NAME, -C DIR, --) run nothing, and
// -S hides what it runs: both count as the host, never as a wrapper.
func (p *StageProfile) hostWrapper() (string, bool) {
	if p == nil || len(p.Wrapper) == 0 || filepath.Base(p.Wrapper[0]) != "env" {
		return "", false
	}
	name := "env"
	args := p.Wrapper[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir":
			i++ // the option's value
		case strings.HasPrefix(a, "-S") || strings.HasPrefix(a, "--split-string"):
			return name, true
		case strings.HasPrefix(a, "-"):
		default:
			k, v, ok := strings.Cut(a, "=")
			if !ok || k == "" {
				return "", false // a command: env runs a wrapper
			}
			if k == "SKOPOS_CONTAINED_BY" && v != "" {
				name = executorName(v)
			}
		}
	}
	return name, true
}

// executorName makes a profile or container name safe inside an executor
// id: letters, digits, dot, dash and underscore, at most 40 characters.
func executorName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() == 40 {
			break
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}

// wrapperPlaceholder finds {name} placeholders in a wrapper argument.
var wrapperPlaceholder = regexp.MustCompile(`\{[a-z]+\}`)

// wraps reports whether the profile puts a real wrapper around the run.
func (p *StageProfile) wraps() bool {
	if p == nil || len(p.Wrapper) == 0 {
		return false
	}
	_, host := p.hostWrapper()
	return !host
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

// Contained: the stage runs inside something the operator provides — a
// wrapper (one that only declares the host a container included) or the
// runner's sandbox settings. A worker whose whole host is a container
// (worker.contained_by) contains every stage; validate takes that in.
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
func (p *StageProfile) validate(where string, hostContained bool) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(p.Settings) != "" {
		var probe map[string]any
		if err := json.Unmarshal([]byte(p.Settings), &probe); err != nil {
			return fmt.Errorf("%s: settings is not valid JSON: %v", where, err)
		}
	}
	for _, a := range p.Wrapper {
		for _, ph := range wrapperPlaceholder.FindAllString(a, -1) {
			if !slices.Contains(wrapperPlaceholders, ph) {
				return fmt.Errorf("%s: the wrapper uses %s, which is not a placeholder (%s)", where, ph, strings.Join(wrapperPlaceholders, " "))
			}
		}
	}
	if (len(p.Tools) > 0 || len(p.Env) > 0 || p.grantsAllow()) && !p.Contained() && !hostContained {
		return fmt.Errorf("%s: grants capabilities (tools/env/settings allow) but is not contained — set a wrapper, settings with sandbox.enabled = true, or worker.contained_by when the whole host is a container (skopos only grants capabilities to stages the operator contains)", where)
	}
	return nil
}

// shortHash is the 12-hex id of a profile part.
func shortHash(v any) string {
	buf, _ := json.Marshal(v)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])[:12]
}

// sandboxSettings is the sandbox block of the profile's settings (what a
// sandbox executor's hash covers).
func (p *StageProfile) sandboxSettings() string {
	var s struct {
		Sandbox json.RawMessage `json:"sandbox"`
	}
	_ = json.Unmarshal([]byte(p.Settings), &s)
	return string(s.Sandbox)
}

// Grants identifies what the profile gives its stage — extra tools, env
// NAMES (never values) and every setting besides the sandbox block (allow
// rules, additional directories, hooks, …) — as a short hash, "" when it
// gives nothing. Recorded on the run apart from the executor: the executor
// says what contains the run, this says what the run was given.
func (p *StageProfile) Grants() string {
	if p == nil {
		return ""
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal([]byte(p.Settings), &settings)
	delete(settings, "sandbox")
	if len(p.Tools) == 0 && len(p.Env) == 0 && len(settings) == 0 {
		return ""
	}
	names := make([]string, 0, len(p.Env))
	for k := range p.Env {
		names = append(names, k)
	}
	sort.Strings(names)
	tools := append([]string{}, p.Tools...)
	sort.Strings(tools)
	return shortHash(struct {
		T []string                   `json:"t"`
		E []string                   `json:"e"`
		S map[string]json.RawMessage `json:"s,omitempty"`
	}{tools, names, settings})
}

// runsRepoCommands: for these stages the worker itself runs the repo's
// setup and check commands on the branch — code the implementer wrote.
// Only a wrapper goes around those; a runner's own sandbox does not.
func runsRepoCommands(stage string) bool {
	return stage == "implement" || stage == "branch_review"
}

// stagePhase is the profile stage a run's stage uses: a second opinion and
// a branch review run as a review; compare and synthesize have no profile
// of their own.
func stagePhase(stage string) string {
	switch stage {
	case "second_opinion", "branch_review":
		return "review"
	}
	return stage
}

// executorStages are the run stages a worker reports executors for.
var executorStages = []string{"plan", "implement", "review", "qa", "compare", "synthesize", "second_opinion", "branch_review"}

// ExecutorFor identifies what contains a run of a stage (the run's stage:
// plan, implement, review, qa, compare, synthesize, second_opinion,
// branch_review) on a runner, in order (tmp/central-config — design.md,
// phase 1c):
//
//	wrapper:<name>:<hash>  the stage profile's wrapper (the hash covers its
//	                       argv); the agent and the worker's setup and check
//	                       commands for the stage all run inside it
//	runner:<kind>          a runner whose own verified lockdown is a sandbox
//	                       (codex) — not for a stage whose setup and checks
//	                       the worker runs itself, outside that sandbox
//	host:<name>            the worker's whole host is a container
//	                       (worker.contained_by, or a wrapper that only
//	                       declares it)
//	sandbox:<hash>         the profile's sandbox settings, for a runner that
//	                       honours them
//	local                  nothing but the agent CLI's own lockdown
//
// The first that applies is the run's executor. What the profile grants is
// recorded apart (Grants).
func (c *Config) ExecutorFor(workspace, stage, runner string) string {
	prof := c.StageProfile(workspace, stagePhase(stage))
	if prof.wraps() {
		name := prof.name
		if name == "" {
			name = "repo"
		}
		return policy.WrapperExecutor(executorName(name), shortHash(prof.Wrapper))
	}
	r, rerr := c.RunnerFor(runner)
	if rerr == nil && !runsRepoCommands(stage) {
		if sb, ok := r.(interface{ SandboxLockdown() bool }); ok && sb.SandboxLockdown() {
			_, kind := c.runnerSpec(runner)
			return policy.RunnerExecutor(kind)
		}
	}
	if host := c.hostContainer(prof); host != "" {
		return policy.HostExecutor(host)
	}
	if prof != nil && prof.sandboxed() && rerr == nil && r.HonorsSettings() {
		return policy.SandboxExecutor(shortHash(prof.sandboxSettings()))
	}
	return policy.ExecutorLocal
}

// DescribeExecutors renders a workspace's executor facts as one line: the
// default, then what differs ("local; review=sandbox:…; implement/glm=wrapper:…").
func DescribeExecutors(facts []policy.ExecutorFact) string {
	parts := make([]string, 0, len(facts))
	for _, f := range facts {
		switch {
		case f.Stage != "" && f.Runner != "":
			parts = append(parts, f.Stage+"/"+f.Runner+"="+f.Executor)
		case f.Stage != "":
			parts = append(parts, f.Stage+"="+f.Executor)
		case f.Runner != "":
			parts = append(parts, f.Runner+"="+f.Executor)
		default:
			parts = append(parts, f.Executor)
		}
	}
	return strings.Join(parts, "; ")
}

// ExecutorNotes are things the operator should know about what contains
// the worker's runs: a wrapper that only sets variables (it declares the
// host; worker.contained_by says so directly).
func (c *Config) ExecutorNotes() []string {
	var out []string
	note := func(where string, p *StageProfile) {
		if name, ok := p.hostWrapper(); ok && strings.TrimSpace(c.ContainedBy) == "" {
			out = append(out, fmt.Sprintf("%s: the wrapper only sets variables, so nothing goes around the run — it counts as host:%s (contained, not isolated). Say it with worker.contained_by = %q instead", where, name, name))
		}
	}
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, stage := range []string{"plan", "implement", "review", "qa"} {
			note("profile "+n+"."+stage, c.Profiles[n][stage])
		}
	}
	repos := make([]string, 0, len(c.Repos))
	for ws := range c.Repos {
		repos = append(repos, ws)
	}
	sort.Strings(repos)
	for _, ws := range repos {
		for _, stage := range []string{"plan", "implement", "review", "qa"} {
			note("repo "+ws+" stage "+stage, c.Repos[ws].Stages[stage])
		}
	}
	return out
}

// hostContainer names the container the whole host is, when the worker
// says so: worker.contained_by, else a stage wrapper that only declares it.
func (c *Config) hostContainer(prof *StageProfile) string {
	if name := strings.TrimSpace(c.ContainedBy); name != "" {
		return executorName(name)
	}
	if name, ok := prof.hostWrapper(); ok {
		return name
	}
	return ""
}

// ExecutorFacts is what the worker reports about containment: per
// workspace, the executor a run gets by stage and runner. One default per
// workspace, then only the stages (and runners in a stage) that differ.
func (c *Config) ExecutorFacts() map[string][]policy.ExecutorFact {
	var runners []string
	for _, f := range c.RunnerFacts() {
		runners = append(runners, f.Name)
	}
	out := map[string][]policy.ExecutorFact{}
	for _, ws := range c.Workspaces {
		// The default: the most common executor across stages and runners.
		count := map[string]int{}
		table := map[string]map[string]string{}
		for _, stage := range executorStages {
			table[stage] = map[string]string{}
			for _, r := range runners {
				e := c.ExecutorFor(ws, stage, r)
				table[stage][r] = e
				count[e]++
			}
		}
		def, best := policy.ExecutorLocal, 0
		for e, n := range count {
			if n > best || (n == best && e < def) {
				def, best = e, n
			}
		}
		facts := []policy.ExecutorFact{{Executor: def}}
		for _, stage := range executorStages {
			same, first := true, ""
			for i, r := range runners {
				if i == 0 {
					first = table[stage][r]
				} else if table[stage][r] != first {
					same = false
				}
			}
			switch {
			case same && first != def && first != "":
				facts = append(facts, policy.ExecutorFact{Stage: stage, Executor: first})
			case !same:
				for _, r := range runners {
					if e := table[stage][r]; e != def {
						facts = append(facts, policy.ExecutorFact{Stage: stage, Runner: r, Executor: e})
					}
				}
			}
		}
		out[ws] = facts
	}
	return out
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
	Path          string `toml:"path"`
	DefaultBranch string `toml:"default_branch"`
	BranchPrefix  string `toml:"branch_prefix"` // default "agent/"
	// BranchTemplate names an item's branch (default
	// "{prefix}{tail}-{slug}"): {prefix}, {tail} (the item id's tail),
	// {slug} (the title), {ref} (the item's reference). Needs {tail} or
	// {ref}. An item's own branch field overrides it.
	BranchTemplate string   `toml:"branch_template"`
	Setup          []string `toml:"setup"` // run in every fresh worktree, or a branch checkout not set up for the branch ({clone} expands)
	// Checkout: where an implementation runs — "worktree" (default: a
	// worktree per item beside the clone) or "branch" (the clone itself,
	// switched to the item's branch for the run and back to the default
	// branch after it; for a repo whose checkout can't be duplicated, e.g.
	// one tied to a local database and configuration).
	Checkout string `toml:"checkout"`
	// SetupEnv is extra environment for setup commands (they run with the
	// scrubbed environment — e.g. a private registry token via ${VAR}).
	SetupEnv map[string]string `toml:"setup_env"`
	// SetupTimeout bounds all the setup commands together (0 = 10m).
	SetupTimeout time.Duration `toml:"setup_timeout"`
	// CheckTimeout bounds all the checks together (0 = 30m).
	CheckTimeout time.Duration `toml:"check_timeout"`
	// QuietTimeout stops one setup or check command that prints nothing for
	// this long (0 = off).
	QuietTimeout time.Duration `toml:"quiet_timeout"`
	// Preflight: commands that say whether this worker's environment can
	// take the repo's work right now (a database, a queue, a service). One
	// failing keeps the workspace out of the worker's claims until it
	// passes again (tmp/self-healing-workers — design.md §2).
	Preflight []string `toml:"preflight"`
	// PreflightInterval: how often the preflight runs (0 = 5m).
	PreflightInterval time.Duration `toml:"preflight_interval"`
	Checks            []string      `toml:"checks"` // verification commands
	// CheckFixRounds: how many times a failing check goes back to the
	// implementer before the item blocks (nil = 2; 0 = block at once).
	CheckFixRounds *int     `toml:"check_fix_rounds"`
	Protected      []string `toml:"protected_branches"`
	// Profile names an entry of worker.profiles; Stages overrides single
	// stages of it (or stands alone).
	Profile string                   `toml:"profile"`
	Stages  map[string]*StageProfile `toml:"stages"`

	// localSet: the shared fields the file set for this repo, noted before
	// defaults are filled in — what a managed workspace ignores.
	localSet []string
	// tokenOnly: the file's push block names only token_env.
	tokenOnly bool

	// Push: remote-agnostic. Token from env (never argv); Kind selects the
	// credential flavor (pushAuthHeader): "gitea" sends Gitea's token
	// header; "github" sends HTTP Basic with user x-access-token — any
	// GitHub personal access token (classic or fine-grained) or App
	// installation token.
	Push struct {
		Kind      string `toml:"kind"`       // gitea | github | none
		RemoteURL string `toml:"remote_url"` // https push URL
		TokenEnv  string `toml:"token_env"`  // env var holding the token
		// PullRequest: after pushing an implementation, open (or reuse) a
		// pull request into the default branch — "draft", "ready", or ""
		// / "none" for none. The token needs pull-request write access.
		PullRequest string `toml:"pull_request"`
		// Steps: push the implementer's commits as it makes them, so an
		// interrupted round leaves its work on origin for the next one
		// (nil = true; tmp/self-healing-workers — design.md §3).
		Steps *bool `toml:"steps"`
	} `toml:"push"`
}

// PushSteps: whether the implementer's commits are pushed as they come.
func (r *Repo) PushSteps() bool {
	return r.Push.Kind != "none" && (r.Push.Steps == nil || *r.Push.Steps)
}

// Account is one credential in the placement pool.
type Account struct {
	Name     string `toml:"name"`     // "claude-1", "gemini"
	Runner   string `toml:"runner"`   // which runner it feeds
	Priority int    `toml:"priority"` // lower = preferred
	Model    string `toml:"model"`
	// Effort is the reasoning effort the runner's CLI takes ("" = its
	// default): claude/antigravity --effort, codex model_reasoning_effort,
	// opencode --variant, command {effort}. Checked per kind at load.
	Effort string            `toml:"effort"`
	Env    map[string]string `toml:"env"` // ANTHROPIC_API_KEY / CLAUDE_CONFIG_DIR / ...
	// LoginDir is where the account's CLI keeps its login — what a stage
	// wrapper's {login} expands to ("" = the directory its kind uses).
	LoginDir string `toml:"login_dir"`
	// Kind makes the account a login for a CLI (claude, opencode, codex,
	// antigravity) instead of for one runner: it feeds every runner of that
	// kind the server defines, with the definition's model and effort
	// (defs.go). Models narrows that to the definitions whose model matches
	// one of the patterns (`zai-coding-plan/*`).
	Kind   string   `toml:"kind"`
	Models []string `toml:"models"`

	// base: the file's account this one was built from for a defined
	// runner (nil = it is the file's own).
	base *Account `toml:"-"`

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
		if now.Before(a.quota().exhaustedUntil) {
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
		if !now.Before(a.quota().exhaustedUntil) {
			return time.Time{}, true
		}
		if next.IsZero() || a.quota().exhaustedUntil.Before(next) {
			next = a.quota().exhaustedUntil
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
		if !now.Before(a.quota().exhaustedUntil) {
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
	a.quota().exhaustedUntil = until
}

// Default fills unset fields with working defaults. It also normalizes
// operator-facing paths: `~` prefixes are expanded (StateDir and repo Path —
// the config template writes ~/... and neither launchd nor git expand it),
// and ${VAR} in account env values is expanded from the process environment
// (the env file has been loaded by then) so token VALUES stay out of the
// TOML (review M4).
func (c *Config) Default(home string) error {
	if cb := strings.TrimSpace(c.ContainedBy); cb != "" && cb != executorName(cb) {
		return fmt.Errorf("contained_by %q: a short name of letters, digits, dot, dash and underscore (at most 40 characters)", cb)
	}
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
	if c.fileAccounts == nil { // first load only: before defaults are filled in
		c.mcpDenyFile = slices.Clone(c.MCPDeny)
		if c.PlanTimeout != 0 {
			c.globalSet = append(c.globalSet, "plan_timeout")
		}
		if c.ImplTimeout != 0 {
			c.globalSet = append(c.globalSet, "impl_timeout")
		}
		if len(c.MCPServers) > 0 {
			c.globalSet = append(c.globalSet, "mcp_servers")
		}
	}
	if c.PlanTimeout == 0 {
		c.PlanTimeout = 10 * time.Minute
	}
	if c.ImplTimeout == 0 {
		c.ImplTimeout = 90 * time.Minute
	}
	if len(c.MCPDeny) == 0 {
		// R12 default: Fortix production writes + scriptling stores.
		c.MCPDeny = slices.Clone(wsconfig.DefaultMCPDeny)
	}
	// runners defaults to every runner the accounts feed: adding a runner
	// is adding an account (an explicit list still restricts what claims
	// offer).
	c.runnersExplicit = len(c.Runners) > 0
	if err := c.splitAccounts(); err != nil {
		return err
	}
	if len(c.Runners) == 0 {
		c.Runners = c.UsedRunners()
	}
	if len(c.Workspaces) == 0 {
		for ws := range c.Repos {
			c.Workspaces = append(c.Workspaces, ws)
		}
	}
	for ws, r := range c.Repos {
		r.localSet = r.sharedSet()
		r.Path = expandHome(r.Path, home)
		if r.DefaultBranch == "" {
			r.DefaultBranch = "main"
		}
		if r.BranchPrefix == "" {
			r.BranchPrefix = "agent/"
		}
		if r.BranchTemplate != "" {
			if err := validateBranchTemplate(r.BranchTemplate); err != nil {
				return fmt.Errorf("repo %s: %w", ws, err)
			}
		}
		if len(r.Protected) == 0 {
			r.Protected = slices.Clone(wsconfig.DefaultProtected)
		}
		if r.Push.TokenEnv == "" && r.Push.Kind != "" && r.Push.Kind != "none" {
			return fmt.Errorf("repo %s: push.token_env is required (tokens come from the environment)", ws)
		}
		if r.Push.Kind == "" {
			// A push block without a kind used to fall back to "none"
			// silently: the worker never pushed, and links and reviews
			// failed far from the cause.
			if r.Push.RemoteURL != "" {
				return fmt.Errorf("repo %s: push.kind is required when push is configured (github, gitea or none)", ws)
			}
			// Only the token's variable: the rest of the push settings are
			// the server's, for a workspace it manages. For one it does
			// not manage this is the old mistake, reported once the worker
			// knows which it is (managed.go, pushProblem).
			r.tokenOnly = r.Push.TokenEnv != ""
			r.Push.Kind = "none"
		}
		switch r.Push.Kind {
		case "github", "gitea", "none":
		default:
			return fmt.Errorf("repo %s: push.kind %q is not one of github, gitea, none", ws, r.Push.Kind)
		}
		if r.Push.Kind != "none" && r.Push.RemoteURL == "" {
			return fmt.Errorf("repo %s: push.remote_url is required for push.kind %q", ws, r.Push.Kind)
		}
		switch r.Push.PullRequest {
		case "", "none":
			r.Push.PullRequest = "none"
		case "draft", "ready":
			if r.Push.Kind == "none" {
				return fmt.Errorf("repo %s: push.pull_request needs a push (push.kind github or gitea)", ws)
			}
			if _, err := newPRClient(r.Push.Kind, r.Push.RemoteURL, ""); err != nil {
				return fmt.Errorf("repo %s: push.pull_request: %w", ws, err)
			}
		default:
			return fmt.Errorf("repo %s: push.pull_request %q is not one of draft, ready, none", ws, r.Push.PullRequest)
		}
		if r.CheckFixRounds != nil && (*r.CheckFixRounds < 0 || *r.CheckFixRounds > maxCheckFixRounds) {
			return fmt.Errorf("repo %s: check_fix_rounds %d is not between 0 and %d", ws, *r.CheckFixRounds, maxCheckFixRounds)
		}
		if r.SetupTimeout < 0 || r.SetupTimeout > maxSetupTimeout {
			return fmt.Errorf("repo %s: setup_timeout %s is not between 0 and %s", ws, r.SetupTimeout, maxSetupTimeout)
		}
		if r.CheckTimeout < 0 || r.CheckTimeout > maxCheckTimeout {
			return fmt.Errorf("repo %s: check_timeout %s is not between 0 and %s", ws, r.CheckTimeout, maxCheckTimeout)
		}
		if r.QuietTimeout < 0 || r.QuietTimeout > maxQuietTimeout {
			return fmt.Errorf("repo %s: quiet_timeout %s is not between 0 and %s", ws, r.QuietTimeout, maxQuietTimeout)
		}
		if r.PreflightInterval != 0 && (r.PreflightInterval < time.Minute || r.PreflightInterval > maxPreflightInterval) {
			return fmt.Errorf("repo %s: preflight_interval %s is not between 1m and %s", ws, r.PreflightInterval, maxPreflightInterval)
		}
		switch r.Checkout {
		case "":
			r.Checkout = checkoutWorktree
		case checkoutWorktree:
		case checkoutBranch:
			if !branchCheckoutSupported {
				return fmt.Errorf("repo %s: checkout = \"branch\" needs a unix host (the clone lock uses flock)", ws)
			}
		default:
			return fmt.Errorf("repo %s: checkout %q is not one of worktree, branch", ws, r.Checkout)
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
			return fmt.Errorf("runner_config.%s: unknown kind %q (claude, opencode, codex, antigravity, command)", name, kind)
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
		if err := c.checkEffort(a); err != nil {
			return fmt.Errorf("account %s: %v", a.Name, err)
		}
	}
	for name, stages := range c.Profiles {
		for stage, p := range stages {
			if !profileStages[stage] {
				return fmt.Errorf("profile %s: unknown stage %q (plan, implement, review, qa)", name, stage)
			}
			p.name = name
			if err := p.validate("profile "+name+"."+stage, strings.TrimSpace(c.ContainedBy) != ""); err != nil {
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
			p.name = "repo"
			if err := p.validate("repo "+ws+" stage "+stage, strings.TrimSpace(c.ContainedBy) != ""); err != nil {
				return err
			}
			expandProfileEnv(p, home)
		}
		for k, v := range r.SetupEnv {
			r.SetupEnv[k] = expandHome(expandEnvRefs(v), home)
		}
	}
	for _, a := range c.FileAccounts() {
		for k, v := range a.Env {
			// ${NAME} only: os.Expand rewrites every $, silently mangling
			// literal values that merely contain one. Unset names expand
			// empty — the checker flags that. A leading ~ expands too
			// (CLAUDE_CONFIG_DIR = "~/.claude" in the template comment).
			v = expandEnvRefs(v)
			a.Env[k] = expandHome(v, home)
		}
		a.LoginDir = expandHome(a.LoginDir, home)
		if a.LoginDir != "" && !filepath.IsAbs(a.LoginDir) {
			return fmt.Errorf("account %s: login_dir must be an absolute path", a.Name)
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

// normalizeBranch is the default template's name: the item id's tail
// guarantees uniqueness (two same-titled items must never share a branch or
// worktree — review finding 5), then a readable slug.
func normalizeBranch(itemID, title, prefix string) string {
	return prefix + idTail(itemID) + "-" + branchSlug(title)
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
