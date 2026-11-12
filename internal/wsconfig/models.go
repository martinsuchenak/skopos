// Package wsconfig holds the part of a workspace's configuration every
// worker serving it shares, on the server (tmp/central-config — design.md,
// phase 2): branches, setup and check commands, timeouts, push settings and
// the MCP allow and deny lists. Machine facts and secrets (clone paths,
// tokens, accounts) stay in each worker's file.
//
// A workspace is managed when it has a config of its own; its groups' values
// are then defaults under it, field by field. A workspace without one keeps
// using each worker's local file. Every save is an append-only revision
// (internal/confrev), and what applies to a workspace at a moment has a
// revision of its own — the config_rev a run records and a worker fetches.
package wsconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid config")
	ErrNotFound = errors.New("not found")
	// ErrConflict: a workspace would get the same field from two groups.
	ErrConflict = errors.New("config conflict")
)

// Duration is a time.Duration written as text ("90s", "10m") in JSON and
// TOML.
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("duration %q: %v", string(b), err)
	}
	*d = Duration(v)
	return nil
}

// D is the duration's value.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Push is the shared part of a repository's push settings. The token stays
// on the worker (push.token_env in its file).
type Push struct {
	Kind        *string `json:"kind,omitempty" toml:"kind,omitempty"`                 // github | gitea | none
	RemoteURL   *string `json:"remote_url,omitempty" toml:"remote_url,omitempty"`     // https push URL
	PullRequest *string `json:"pull_request,omitempty" toml:"pull_request,omitempty"` // draft | ready | none
	Steps       *bool   `json:"steps,omitempty" toml:"steps,omitempty"`               // push commits as they are made
}

// Config is what one owner (a workspace or a group) stores. A nil field is
// not set at this level; an empty list is set, to nothing.
type Config struct {
	DefaultBranch     *string   `json:"default_branch,omitempty" toml:"default_branch,omitempty"`
	BranchPrefix      *string   `json:"branch_prefix,omitempty" toml:"branch_prefix,omitempty"`
	BranchTemplate    *string   `json:"branch_template,omitempty" toml:"branch_template,omitempty"`
	Checkout          *string   `json:"checkout,omitempty" toml:"checkout,omitempty"`
	ProtectedBranches *[]string `json:"protected_branches,omitempty" toml:"protected_branches,omitempty"`

	Setup             *[]string `json:"setup,omitempty" toml:"setup,omitempty"`
	Checks            *[]string `json:"checks,omitempty" toml:"checks,omitempty"`
	CheckFixRounds    *int      `json:"check_fix_rounds,omitempty" toml:"check_fix_rounds,omitempty"`
	Preflight         *[]string `json:"preflight,omitempty" toml:"preflight,omitempty"`
	PreflightInterval *Duration `json:"preflight_interval,omitempty" toml:"preflight_interval,omitempty"`

	SetupTimeout *Duration `json:"setup_timeout,omitempty" toml:"setup_timeout,omitempty"`
	CheckTimeout *Duration `json:"check_timeout,omitempty" toml:"check_timeout,omitempty"`
	QuietTimeout *Duration `json:"quiet_timeout,omitempty" toml:"quiet_timeout,omitempty"`
	PlanTimeout  *Duration `json:"plan_timeout,omitempty" toml:"plan_timeout,omitempty"`
	ImplTimeout  *Duration `json:"impl_timeout,omitempty" toml:"impl_timeout,omitempty"`

	Push *Push `json:"push,omitempty" toml:"push,omitempty"`

	MCPDeny    *[]string `json:"mcp_deny,omitempty" toml:"mcp_deny,omitempty"`
	MCPServers *[]string `json:"mcp_servers,omitempty" toml:"mcp_servers,omitempty"`
	// MCP names the MCP server definitions the workspace's runs get
	// (mcp.go). Set — to an empty list too — the server holds the runs' MCP
	// servers and a worker's own mcp_config is not used for the workspace;
	// not set at any level, each worker's mcp_config applies.
	MCP *[]string `json:"mcp,omitempty" toml:"mcp,omitempty"`

	// Stages: what each stage's agent is given and which wrapper goes
	// around it (stages.go). A stage with a block is set — an empty block
	// sets it to nothing; a stage without one is not set at this level.
	Stages map[string]*StageCaps `json:"stages,omitempty" toml:"stages,omitempty"`
}

// Limits both the server (at save) and the worker (at load) enforce.
const (
	MaxCheckFixRounds    = 5
	MaxSetupTimeout      = 2 * time.Hour
	MaxCheckTimeout      = 2 * time.Hour
	MaxQuietTimeout      = 2 * time.Hour
	MaxPreflightInterval = 24 * time.Hour
	MaxAgentTimeout      = 24 * time.Hour

	maxCommands    = 50
	maxCommandLen  = 4000
	maxListEntries = 200
	// MaxContentBytes bounds one owner's stored config.
	MaxContentBytes = 64 << 10
)

// ShellFields are the fields that hold shell commands the worker itself
// runs.
var ShellFields = []string{"setup", "checks", "preflight"}

// CommandFields are the fields whose values decide what executes on a
// worker's host: the shell commands the worker runs, and each stage's block
// — the tools and env its agent is given and the wrapper it starts behind
// are commands under another name. A worker takes them from the server only
// with worker.server_commands.
var CommandFields = append(slices.Clone(ShellFields), StageFields()...)

// IsCommandField reports whether a field (as Fields names it) decides what
// executes on a worker's host.
func IsCommandField(name string) bool {
	return slices.Contains(CommandFields, name)
}

// DefaultProtected are the branches no run pushes to when none are set.
var DefaultProtected = []string{"PROD", "main", "master", "develop", "release/**", "hotfix/**"}

// Defaults is the value of every field nothing sets. A managed workspace
// gets these, never the local file's values.
func Defaults() Config {
	return Config{
		DefaultBranch: new("main"), BranchPrefix: new("agent/"), BranchTemplate: new(""),
		Checkout: new("worktree"), ProtectedBranches: new(append([]string{}, DefaultProtected...)),
		Setup: new([]string{}), Checks: new([]string{}), CheckFixRounds: new(2),
		Preflight: new([]string{}), PreflightInterval: new(Duration(5 * time.Minute)),
		SetupTimeout: new(Duration(10 * time.Minute)), CheckTimeout: new(Duration(30 * time.Minute)),
		QuietTimeout: new(Duration(0)), PlanTimeout: new(Duration(10 * time.Minute)), ImplTimeout: new(Duration(90 * time.Minute)),
		Push:    &Push{Kind: new("none"), RemoteURL: new(""), PullRequest: new("none"), Steps: new(true)},
		MCPDeny: new([]string{}), MCPServers: new([]string{}),
	}
}

// Fields flattens a config to its set fields by name ("setup",
// "push.kind", …), each as its JSON value. It is how configs are merged,
// compared and checked for two groups setting the same thing.
func (c Config) Fields() map[string]json.RawMessage {
	buf, _ := json.Marshal(c)
	var top map[string]json.RawMessage
	_ = json.Unmarshal(buf, &top)
	out := make(map[string]json.RawMessage, len(top)+3)
	for k, v := range top {
		switch k {
		case "push":
			var push map[string]json.RawMessage
			_ = json.Unmarshal(v, &push)
			for pk, pv := range push {
				out["push."+pk] = pv
			}
		case "stages":
			// One field per stage, holding its whole block: a workspace's
			// block replaces its group's, it is never merged into it.
			var stages map[string]json.RawMessage
			_ = json.Unmarshal(v, &stages)
			for stage, block := range stages {
				if string(block) == "null" {
					block = json.RawMessage("{}") // set, to nothing
				}
				out[stageField(stage)] = block
			}
		default:
			// A list set to a nil slice is set to nothing, not unset: it
			// must never read back as a missing field.
			if string(v) == "null" {
				v = json.RawMessage("[]")
			}
			out[k] = v
		}
	}
	return out
}

// FromFields is the inverse of Fields.
func FromFields(fields map[string]json.RawMessage) (Config, error) {
	top := map[string]json.RawMessage{}
	push := map[string]json.RawMessage{}
	stages := map[string]json.RawMessage{}
	for k, v := range fields {
		if name, ok := strings.CutPrefix(k, "push."); ok {
			push[name] = v
		} else if name, ok := strings.CutPrefix(k, "stages."); ok {
			stages[name] = v
		} else {
			top[k] = v
		}
	}
	if len(push) > 0 {
		buf, _ := json.Marshal(push)
		top["push"] = buf
	}
	if len(stages) > 0 {
		buf, _ := json.Marshal(stages)
		top["stages"] = buf
	}
	buf, _ := json.Marshal(top)
	var c Config
	if err := json.Unmarshal(buf, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// FieldNames lists the set fields of a config, sorted.
func (c Config) FieldNames() []string {
	f := c.Fields()
	out := make([]string, 0, len(f))
	for k := range f {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Empty reports a config that sets nothing.
func (c Config) Empty() bool { return len(c.Fields()) == 0 }

// Overlay returns base with every field over sets replacing it.
func Overlay(base, over Config) Config {
	f := base.Fields()
	for k, v := range over.Fields() {
		f[k] = v
	}
	out, _ := FromFields(f)
	return out
}

// Complete fills every field c leaves unset with its default.
func Complete(c Config) Config { return Overlay(Defaults(), c) }

// Changed lists the fields that differ between two configs (set in one and
// not the other, or set to different values), sorted.
func Changed(a, b Config) []string {
	fa, fb := a.Fields(), b.Fields()
	seen := map[string]bool{}
	var out []string
	for k, v := range fa {
		seen[k] = true
		if w, ok := fb[k]; !ok || string(w) != string(v) {
			out = append(out, k)
		}
	}
	for k := range fb {
		if !seen[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

var (
	reTemplatePlaceholder = regexp.MustCompile(`\{[a-z]+\}`)
	reBranchName          = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// ValidateBranchTemplate checks an item-branch template: known placeholders
// only, and {tail} or {ref} so two items never share a branch.
func ValidateBranchTemplate(t string) error {
	for _, p := range reTemplatePlaceholder.FindAllString(t, -1) {
		switch p {
		case "{prefix}", "{tail}", "{slug}", "{ref}":
		default:
			return fmt.Errorf("branch_template %q: unknown placeholder %s (use {prefix}, {tail}, {slug}, {ref})", t, p)
		}
	}
	if !strings.Contains(t, "{tail}") && !strings.Contains(t, "{ref}") {
		return fmt.Errorf("branch_template %q: needs {tail} or {ref}, or two items with the same title would share a branch", t)
	}
	return nil
}

func trimList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func trimStr(p *string) *string {
	if p == nil {
		return nil
	}
	return new(strings.TrimSpace(*p))
}

// Normalize validates one owner's config and returns its canonical form:
// trimmed, with empty text fields dropped (not set). Each field is checked
// on its own; what only holds for the merged result (a push needs a remote
// URL) is CheckEffective's.
func Normalize(in Config) (Config, error) {
	c := in
	bad := func(format string, args ...any) (Config, error) {
		return Config{}, fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
	}
	for _, s := range []**string{&c.DefaultBranch, &c.BranchPrefix, &c.Checkout} {
		if *s = trimStr(*s); *s != nil && **s == "" {
			*s = nil
		}
	}
	// An empty branch_template is a value: the default template, which a
	// workspace may set to undo its group's.
	c.BranchTemplate = trimStr(c.BranchTemplate)
	if c.DefaultBranch != nil && (!reBranchName.MatchString(*c.DefaultBranch) || strings.HasPrefix(*c.DefaultBranch, "-") || len(*c.DefaultBranch) > 200) {
		return bad("default_branch %q is not a branch name", *c.DefaultBranch)
	}
	if c.BranchPrefix != nil && (!reBranchName.MatchString(*c.BranchPrefix) || strings.HasPrefix(*c.BranchPrefix, "-") || len(*c.BranchPrefix) > 100) {
		return bad("branch_prefix %q may hold letters, digits, '.', '_', '/' and '-'", *c.BranchPrefix)
	}
	if c.BranchTemplate != nil && *c.BranchTemplate != "" {
		if err := ValidateBranchTemplate(*c.BranchTemplate); err != nil {
			return bad("%v", err)
		}
	}
	if c.Checkout != nil && *c.Checkout != "worktree" && *c.Checkout != "branch" {
		return bad("checkout %q is not one of worktree, branch", *c.Checkout)
	}
	lists := []struct {
		name    string
		p       **[]string
		command bool
	}{
		{"protected_branches", &c.ProtectedBranches, false}, {"setup", &c.Setup, true}, {"checks", &c.Checks, true},
		{"preflight", &c.Preflight, true}, {"mcp_deny", &c.MCPDeny, false}, {"mcp_servers", &c.MCPServers, false},
		{"mcp", &c.MCP, false},
	}
	for _, l := range lists {
		if *l.p == nil {
			continue
		}
		list := trimList(**l.p)
		limit := maxListEntries
		if l.command {
			limit = maxCommands
		}
		if len(list) > limit {
			return bad("%s has %d entries (at most %d)", l.name, len(list), limit)
		}
		for _, e := range list {
			if len(e) > maxCommandLen {
				return bad("%s: an entry is %d characters long (at most %d)", l.name, len(e), maxCommandLen)
			}
			if strings.ContainsRune(e, 0) {
				return bad("%s: an entry holds a NUL byte", l.name)
			}
		}
		*l.p = &list
	}
	if c.MCPServers != nil {
		for _, e := range *c.MCPServers {
			if !strings.HasPrefix(e, "mcp__") {
				return bad("mcp_servers: %q is not an MCP tool prefix (mcp__<server>…)", e)
			}
		}
	}
	if c.MCP != nil {
		names := make([]string, 0, len(*c.MCP))
		for _, e := range *c.MCP {
			if !reMCPName.MatchString(e) || e == ReservedMCPName {
				return bad("mcp: %q is not the name of an MCP server definition", e)
			}
			if !slices.Contains(names, e) {
				names = append(names, e)
			}
		}
		if len(names) > 20 {
			return bad("mcp names %d servers (at most 20)", len(names))
		}
		c.MCP = &names
	}
	if c.CheckFixRounds != nil && (*c.CheckFixRounds < 0 || *c.CheckFixRounds > MaxCheckFixRounds) {
		return bad("check_fix_rounds %d is not between 0 and %d", *c.CheckFixRounds, MaxCheckFixRounds)
	}
	durations := []struct {
		name     string
		p        *Duration
		min, max time.Duration
		zeroOK   bool
	}{
		{"setup_timeout", c.SetupTimeout, time.Second, MaxSetupTimeout, false},
		{"check_timeout", c.CheckTimeout, time.Second, MaxCheckTimeout, false},
		{"quiet_timeout", c.QuietTimeout, time.Second, MaxQuietTimeout, true},
		{"preflight_interval", c.PreflightInterval, time.Minute, MaxPreflightInterval, false},
		{"plan_timeout", c.PlanTimeout, time.Minute, MaxAgentTimeout, false},
		{"impl_timeout", c.ImplTimeout, time.Minute, MaxAgentTimeout, false},
	}
	for _, d := range durations {
		if d.p == nil || (d.zeroOK && *d.p == 0) {
			continue
		}
		if v := d.p.D(); v < d.min || v > d.max {
			return bad("%s %s is not between %s and %s", d.name, v, d.min, d.max)
		}
	}
	if c.Push != nil {
		p := *c.Push
		for _, s := range []**string{&p.Kind, &p.RemoteURL, &p.PullRequest} {
			if *s = trimStr(*s); *s != nil && **s == "" {
				*s = nil
			}
		}
		if p.Kind != nil {
			switch *p.Kind {
			case "github", "gitea", "none":
			default:
				return bad("push.kind %q is not one of github, gitea, none", *p.Kind)
			}
		}
		if p.PullRequest != nil {
			switch *p.PullRequest {
			case "draft", "ready", "none":
			default:
				return bad("push.pull_request %q is not one of draft, ready, none", *p.PullRequest)
			}
		}
		if p.RemoteURL != nil {
			u, err := url.Parse(*p.RemoteURL)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				return bad("push.remote_url %q is not an http(s) URL", *p.RemoteURL)
			}
			if u.User != nil {
				return bad("push.remote_url must not carry credentials: the token comes from the worker's environment")
			}
		}
		if p.Kind == nil && p.RemoteURL == nil && p.PullRequest == nil && p.Steps == nil {
			c.Push = nil
		} else {
			c.Push = &p
		}
	}
	if c.Stages != nil {
		stages := make(map[string]*StageCaps, len(c.Stages))
		for stage, block := range c.Stages {
			norm, err := NormalizeStage(strings.ToLower(strings.TrimSpace(stage)), block)
			if err != nil {
				return Config{}, err
			}
			stages[strings.ToLower(strings.TrimSpace(stage))] = norm
		}
		c.Stages = stages
		if len(stages) == 0 {
			c.Stages = nil
		}
	}
	buf, _ := json.Marshal(c)
	if len(buf) > MaxContentBytes {
		return bad("the config is %d bytes (at most %d)", len(buf), MaxContentBytes)
	}
	return c, nil
}

// Wrappers lists the wrapper definitions a config's stage blocks name,
// sorted.
func (c Config) Wrappers() []string {
	var out []string
	for _, block := range c.Stages {
		for _, w := range block.Wrappers() {
			if !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	sort.Strings(out)
	return out
}

// CheckEffective checks what holds only for a workspace's merged config
// with defaults filled in: a push needs a remote URL, a pull request needs
// a push.
func CheckEffective(c Config) error {
	full := Complete(c)
	if kind := *full.Push.Kind; kind != "none" && *full.Push.RemoteURL == "" {
		return fmt.Errorf("%w: push.remote_url is required for push.kind %q", ErrInvalid, kind)
	}
	if pr := *full.Push.PullRequest; pr != "none" && *full.Push.Kind == "none" {
		return fmt.Errorf("%w: push.pull_request needs a push (push.kind github or gitea)", ErrInvalid)
	}
	// A server the runs get and may call no tool of is a mistake: the tool
	// allowlist is apart from which servers are there.
	if full.MCP != nil {
		for _, name := range *full.MCP {
			if !AllowsMCPServer(*full.MCPServers, name) {
				return fmt.Errorf("%w: mcp gives the runs server %s, and mcp_servers allows none of its tools: add %q (or the tools to allow, %q…)", ErrInvalid, name, "mcp__"+name, "mcp__"+name+"__")
			}
		}
	}
	return nil
}

// AllowsMCPServer reports whether a tool allowlist (mcp_servers) lets a run
// call at least one tool of the named server.
func AllowsMCPServer(allow []string, name string) bool {
	for _, e := range allow {
		if e == "mcp__"+name || strings.HasPrefix(e, "mcp__"+name+"__") {
			return true
		}
	}
	return false
}

// Canonical is the bytes a config is stored and hashed as.
func Canonical(c Config) []byte {
	buf, _ := json.Marshal(c)
	return buf
}

// Describe renders one field's value for a diff or an audit note.
func Describe(v json.RawMessage) string {
	if v == nil {
		return "(not set)"
	}
	return string(v)
}
