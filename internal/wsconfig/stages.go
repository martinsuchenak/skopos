package wsconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/martinsuchenak/skopos/internal/policy"
)

// Stage capabilities and wrappers (tmp/central-config — design.md, phase
// 2d). A worker's stage profile did two jobs: it said what a stage's agent
// is given (extra tools, env, settings) and what contains it (a wrapper, a
// sandbox). The first is the workspace's and lives here, per stage; the
// second stays the host's — worker.contained_by, or what the worker's file
// sets — except for a wrapper defined once on the server and attached to a
// stage by rule.

// StageNames are the stages a config may give capabilities to. A second
// opinion and a branch review run as a review.
var StageNames = []string{"plan", "implement", "review", "qa"}

// NoWrapper is the wrapper name of a rule that leaves its runs unwrapped
// (an exemption ahead of a rule that matches more).
const NoWrapper = "none"

// WrapRule attaches a wrapper to the runs of a stage on the runners it
// matches. Every field that is set must match (kind exactly, the others as
// globs); a rule that sets none matches every runner. The first rule that
// matches decides.
type WrapRule struct {
	Kind     string `json:"kind,omitempty" toml:"kind,omitempty"`
	Provider string `json:"provider,omitempty" toml:"provider,omitempty"`
	Model    string `json:"model,omitempty" toml:"model,omitempty"`
	Runner   string `json:"runner,omitempty" toml:"runner,omitempty"`
	// Wrapper names a wrapper definition, or "none" for no wrapper.
	Wrapper string `json:"wrapper" toml:"wrapper"`
}

// Matches reports whether the rule applies to a runner with these facts.
func (r WrapRule) Matches(f policy.Facts) bool {
	return (r.Kind == "" || strings.EqualFold(r.Kind, f.Kind)) &&
		(r.Provider == "" || policy.Glob(r.Provider, f.Provider)) &&
		(r.Model == "" || policy.Glob(r.Model, f.Model)) &&
		(r.Runner == "" || policy.Glob(r.Runner, f.Name))
}

func (r WrapRule) String() string {
	var parts []string
	for _, kv := range [][2]string{{"kind", r.Kind}, {"provider", r.Provider}, {"model", r.Model}, {"runner", r.Runner}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	if len(parts) == 0 {
		parts = []string{"any"}
	}
	return strings.Join(parts, ",") + " → " + r.Wrapper
}

// StageCaps is what one stage of a workspace's runs is given, and which
// wrapper goes around it. A workspace's block for a stage replaces its
// group's for that stage whole: what a stage gets is always one block.
type StageCaps struct {
	// Tools: extra allowed tools (Bash(cmd:*) patterns and the like).
	Tools []string `json:"tools,omitempty" toml:"tools,omitempty"`
	// Env: extra variables for the agent. ${NAME} takes the value the
	// worker declares under worker.vars (a host path).
	Env map[string]string `json:"env,omitempty" toml:"env,omitempty"`
	// Settings: JSON merged into the agent's settings — permissions
	// (allow, deny, additionalDirectories) and the sandbox block, nothing
	// else.
	Settings Settings `json:"settings,omitempty" toml:"settings,omitempty"`
	// Wrap: which wrapper goes around the stage's runs, by runner.
	Wrap []WrapRule `json:"wrap,omitempty" toml:"wrap,omitempty"`
}

// Settings is a stage's settings as JSON text. In JSON it reads as an
// object or as a string holding one, and is written as an object; in TOML
// it is a string.
type Settings string

func (s Settings) MarshalJSON() ([]byte, error) {
	if json.Valid([]byte(s)) && strings.HasPrefix(strings.TrimSpace(string(s)), "{") {
		return []byte(s), nil
	}
	return json.Marshal(string(s))
}

func (s *Settings) UnmarshalJSON(b []byte) error {
	var text string
	if json.Unmarshal(b, &text) == nil {
		*s = Settings(text)
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("settings: a JSON object, or a string holding one")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return err
	}
	*s = Settings(buf.String())
	return nil
}

// Grants reports whether the block gives the stage anything: tools, env,
// or settings beyond the sandbox block.
func (s *StageCaps) Grants() bool {
	if s == nil {
		return false
	}
	if len(s.Tools) > 0 || len(s.Env) > 0 {
		return true
	}
	var settings map[string]json.RawMessage
	_ = json.Unmarshal([]byte(s.Settings), &settings)
	delete(settings, "sandbox")
	return len(settings) > 0
}

// WrapperFor returns the wrapper the block's rules attach to a runner
// ("" = none).
func (s *StageCaps) WrapperFor(f policy.Facts) string {
	if s == nil {
		return ""
	}
	for _, r := range s.Wrap {
		if r.Matches(f) {
			if r.Wrapper == NoWrapper {
				return ""
			}
			return r.Wrapper
		}
	}
	return ""
}

// Wrappers lists the wrapper definitions the block's rules name, sorted.
func (s *StageCaps) Wrappers() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, r := range s.Wrap {
		if r.Wrapper != NoWrapper && !slices.Contains(out, r.Wrapper) {
			out = append(out, r.Wrapper)
		}
	}
	sort.Strings(out)
	return out
}

// stageField is the name Fields gives a stage's block.
func stageField(stage string) string { return "stages." + stage }

// StageFields are the field names of the stage blocks.
func StageFields() []string {
	out := make([]string, len(StageNames))
	for i, s := range StageNames {
		out[i] = stageField(s)
	}
	return out
}

// Limits on a stage block and a wrapper.
const (
	maxStageTools   = 100
	maxToolLen      = 500
	maxStageEnv     = 50
	maxEnvValueLen  = 2000
	maxSettingsLen  = 8 << 10
	maxWrapRules    = 20
	maxWrapperArgs  = 200
	maxWrapperArg   = 2000
	maxWrapperCheck = 50
)

var (
	reEnvName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)
	reWrapperName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)
	reVarRef      = regexp.MustCompile(`\$\{([^}]*)\}`)
	rePlaceholder = regexp.MustCompile(`\{[a-z]+\}`)
)

// loginEnv are the variables that say which login an agent CLI uses and
// where it reads its own configuration. The account is the worker's
// choice, and the CLI's configuration decides where its requests go: a
// stage's env may set neither.
var loginEnv = map[string]bool{
	"HOME": true, "CLAUDE_CONFIG_DIR": true, "CODEX_HOME": true,
	"XDG_DATA_HOME": true, "XDG_CONFIG_HOME": true, "XDG_STATE_HOME": true,
}

// agentEnvPrefixes are the variable families that configure an agent CLI
// or the provider behind it — its model, its credentials, its cloud
// account. They belong to the runner's account on the worker, where the
// runner's facts are derived from them.
var agentEnvPrefixes = []string{"ANTHROPIC_", "CLAUDE_", "OPENAI_", "OPENCODE_", "CODEX_", "GEMINI_", "GOOGLE_", "VERTEX_", "AWS_", "AZURE_"}

// loaderEnv are variables that change what code a process loads or which
// credentials and certificates it trusts.
var loaderEnv = map[string]bool{
	"NODE_OPTIONS": true, "NODE_EXTRA_CA_CERTS": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true, "NO_PROXY": true, "BASH_ENV": true,
}

// StageEnvRefused says why a stage's env may not set a variable ("" = it
// may). The server checks a saved block with it, and a worker what it is
// handed. A stage's env is for the repository's own tools. These checks
// catch a block that would quietly change which model, login or provider a
// run uses; they are not what keeps a server admin out of a worker — only
// worker.server_commands being off is.
func StageEnvRefused(name string) string {
	up := strings.ToUpper(name)
	switch {
	case !reEnvName.MatchString(name):
		return "it is not a variable name"
	case policy.RoutesRequests(name) || loaderEnv[up]:
		return "it changes where the agent sends its requests or what it loads, which the runner's facts would not show"
	case loginEnv[up]:
		return "it selects the agent's login or its own configuration, which is the worker's to choose"
	case strings.Contains(up, "TOKEN") || strings.Contains(up, "SECRET") || strings.Contains(up, "PASSWORD") || strings.HasSuffix(up, "_KEY") || strings.HasSuffix(up, "ASKPASS"):
		return "it looks like a credential: a stage's env is recorded in full, and an agent's credentials are its account's"
	case strings.HasPrefix(up, "LD_") || strings.HasPrefix(up, "DYLD_") || strings.HasPrefix(up, "GIT_CONFIG") || strings.HasPrefix(up, "GIT_SSH"):
		return "it changes what the agent's processes load or how git connects"
	}
	for _, p := range agentEnvPrefixes {
		if strings.HasPrefix(up, p) {
			return "it configures the agent CLI or its provider (model, credentials, cloud account), which the runner's account on the worker says"
		}
	}
	return ""
}

// settingsKeys are the keys a stage's settings may hold, with the keys
// allowed inside each (nil = any).
var settingsKeys = map[string][]string{
	"permissions": {"allow", "deny", "additionalDirectories"},
	"sandbox":     nil,
}

// normalizeSettings checks a stage's settings and returns them compact.
func normalizeSettings(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > maxSettingsLen {
		return "", fmt.Errorf("settings is %d bytes (at most %d)", len(raw), maxSettingsLen)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil || top == nil {
		return "", fmt.Errorf("settings is not a JSON object")
	}
	for k, v := range top {
		allowed, ok := settingsKeys[k]
		if !ok {
			return "", fmt.Errorf("settings may hold permissions and sandbox, not %q", k)
		}
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(v, &inner); err != nil || inner == nil {
			return "", fmt.Errorf("settings.%s is not an object", k)
		}
		if allowed == nil {
			continue
		}
		for ik, iv := range inner {
			if !slices.Contains(allowed, ik) {
				return "", fmt.Errorf("settings.%s may hold %s, not %q", k, strings.Join(allowed, ", "), ik)
			}
			var list []string
			if err := json.Unmarshal(iv, &list); err != nil {
				return "", fmt.Errorf("settings.%s.%s is not a list of strings", k, ik)
			}
		}
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return "", fmt.Errorf("settings is not valid JSON")
	}
	return buf.String(), nil
}

// VarRefs lists the ${NAME} references in s, in order. A reference that is
// not a variable name is an error.
func VarRefs(s string) ([]string, error) {
	var out []string
	for _, m := range reVarRef.FindAllStringSubmatch(s, -1) {
		if !reEnvName.MatchString(m[1]) {
			return nil, fmt.Errorf("%q is not a variable reference (${NAME})", m[0])
		}
		out = append(out, m[1])
	}
	return out, nil
}

// Expand replaces every ${NAME} in s with the worker's declared value
// (worker.vars). A name the worker does not declare is an error: a value
// with a hole in it is never used.
func Expand(s string, vars map[string]string) (string, error) {
	var missing string
	out := reVarRef.ReplaceAllStringFunc(s, func(ref string) string {
		name := ref[2 : len(ref)-1]
		v, ok := vars[name]
		if !ok && missing == "" {
			missing = name
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("${%s} is not declared under worker.vars on this worker", missing)
	}
	return out, nil
}

func plainText(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r == 0 || r == '\n' || r == '\r' })
}

// NormalizeStage validates one stage's block and returns its stored form.
// A nil block is an empty one: the stage is set to nothing.
func NormalizeStage(stage string, in *StageCaps) (*StageCaps, error) {
	bad := func(format string, args ...any) (*StageCaps, error) {
		return nil, fmt.Errorf("%w: stages.%s: %s", ErrInvalid, stage, fmt.Sprintf(format, args...))
	}
	if !slices.Contains(StageNames, stage) {
		return nil, fmt.Errorf("%w: stages: %q is not a stage (%s)", ErrInvalid, stage, strings.Join(StageNames, ", "))
	}
	out := &StageCaps{}
	if in == nil {
		return out, nil
	}
	for _, t := range in.Tools {
		t = strings.TrimSpace(t)
		if t == "" || slices.Contains(out.Tools, t) {
			continue
		}
		if len(t) > maxToolLen || !plainText(t) {
			return bad("a tool entry is not a tool pattern (one line, at most %d characters)", maxToolLen)
		}
		out.Tools = append(out.Tools, t)
	}
	if len(out.Tools) > maxStageTools {
		return bad("tools has %d entries (at most %d)", len(out.Tools), maxStageTools)
	}
	if len(in.Env) > maxStageEnv {
		return bad("env has %d variables (at most %d)", len(in.Env), maxStageEnv)
	}
	for k, v := range in.Env {
		if why := StageEnvRefused(k); why != "" {
			return bad("env may not set %s: %s", k, why)
		}
		if len(v) > maxEnvValueLen || !plainText(v) {
			return bad("env %s: the value must be one line of at most %d characters", k, maxEnvValueLen)
		}
		if _, err := VarRefs(v); err != nil {
			return bad("env %s: %v", k, err)
		}
		if out.Env == nil {
			out.Env = map[string]string{}
		}
		out.Env[k] = v
	}
	settings, err := normalizeSettings(string(in.Settings))
	if err != nil {
		return bad("%v", err)
	}
	out.Settings = Settings(settings)
	if len(in.Wrap) > maxWrapRules {
		return bad("wrap has %d rules (at most %d)", len(in.Wrap), maxWrapRules)
	}
	for i, r := range in.Wrap {
		r = WrapRule{Kind: strings.ToLower(strings.TrimSpace(r.Kind)), Provider: strings.TrimSpace(r.Provider),
			Model: strings.TrimSpace(r.Model), Runner: strings.TrimSpace(r.Runner), Wrapper: strings.TrimSpace(r.Wrapper)}
		if r.Wrapper == "" {
			return bad("wrap rule %d names no wrapper (a wrapper's name, or %q)", i+1, NoWrapper)
		}
		if r.Wrapper != NoWrapper && !reWrapperName.MatchString(r.Wrapper) {
			return bad("wrap rule %d: %q is not a wrapper name", i+1, r.Wrapper)
		}
		for _, g := range []string{r.Kind, r.Provider, r.Model, r.Runner} {
			if len(g) > 200 || !plainText(g) || strings.ContainsAny(g, " \t") {
				return bad("wrap rule %d: %q is not a name or a glob", i+1, g)
			}
		}
		out.Wrap = append(out.Wrap, r)
	}
	return out, nil
}

// WrapperPlaceholders are what a wrapper's argv may name; the worker fills
// them in for each run:
//
//	{dir}        the run's worktree (or the clone, for a branch checkout)
//	{gitdir}     the clone's git directory
//	{login}      the account's login directory
//	{rundir}     the run's own directory under the worker's state_dir
//	{cli}        the directory the agent CLI's binary is in
//	{run} {stage} {workspace}
var WrapperPlaceholders = []string{"{dir}", "{gitdir}", "{login}", "{rundir}", "{cli}", "{run}", "{stage}", "{workspace}"}

// UnknownPlaceholder returns the first {name} in argv that is not a wrapper
// placeholder ("" = none).
func UnknownPlaceholder(argv []string) string {
	for _, a := range argv {
		for _, ph := range rePlaceholder.FindAllString(a, -1) {
			if !slices.Contains(WrapperPlaceholders, ph) {
				return ph
			}
		}
	}
	return ""
}

// EnvOnlyWrapper recognises an argv that only sets variables — `env K=V …`
// — and runs nothing of its own. Such a "wrapper" puts nothing around a
// run. It returns the value of SKOPOS_CONTAINED_BY when the argv sets it
// (the way a worker inside a container used to say so).
//
// An env argv is a real wrapper only when it runs a command of its own
// (`env A=b bwrap …`). Options (-i, -u NAME, -C DIR, --) run nothing, and
// -S hides what it runs: both count as running nothing.
func EnvOnlyWrapper(argv []string) (containedBy string, ok bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != "env" {
		return "", false
	}
	args := argv[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir":
			i++ // the option's value
		case strings.HasPrefix(a, "-S") || strings.HasPrefix(a, "--split-string"):
			return containedBy, true
		case strings.HasPrefix(a, "-"):
		default:
			k, v, isVar := strings.Cut(a, "=")
			if !isVar || k == "" {
				return "", false // a command: env runs a wrapper
			}
			if k == "SKOPOS_CONTAINED_BY" && v != "" {
				containedBy = v
			}
		}
	}
	return containedBy, true
}

// envPrefixNames lists the variables a leading `env K=V … cmd` in argv
// sets, before the command it runs.
func envPrefixNames(argv []string) []string {
	if len(argv) == 0 || filepath.Base(argv[0]) != "env" {
		return nil
	}
	var out []string
	args := argv[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-u" || a == "--unset" || a == "-C" || a == "--chdir":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			k, _, isVar := strings.Cut(a, "=")
			if !isVar || k == "" {
				return out // the command
			}
			out = append(out, k)
		}
	}
	return out
}

// ArgvHash is the 12-hex id of a wrapper's argv: the part of an executor
// id (wrapper:<name>:<hash>) that changes when the wrapper does.
func ArgvHash(argv []string) string {
	buf, _ := json.Marshal(argv)
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])[:12]
}

// WrapperDef defines a wrapper once, for every worker: the argv prefix a
// run — the agent, and the worker's setup and check commands of the stage —
// starts behind. Check is a command that shows the wrapper can start on a
// host; a worker where it fails does not run what the wrapper is for.
type WrapperDef struct {
	Name  string   `json:"name" toml:"-"`
	Argv  []string `json:"argv" toml:"argv"`
	Check []string `json:"check,omitempty" toml:"check,omitempty"`
}

// Executor is the executor id of a run inside the wrapper.
func (d WrapperDef) Executor() string { return policy.WrapperExecutor(d.Name, ArgvHash(d.Argv)) }

func normalizeArgv(what string, in []string, limit int) ([]string, error) {
	if len(in) > limit {
		return nil, fmt.Errorf("%s has %d arguments (at most %d)", what, len(in), limit)
	}
	out := make([]string, 0, len(in))
	for _, a := range in {
		if len(a) > maxWrapperArg || strings.ContainsRune(a, 0) {
			return nil, fmt.Errorf("%s: an argument is too long (at most %d characters) or holds a NUL byte", what, maxWrapperArg)
		}
		if _, err := VarRefs(a); err != nil {
			return nil, fmt.Errorf("%s: %v", what, err)
		}
		out = append(out, a)
	}
	if len(out) > 0 && strings.TrimSpace(out[0]) == "" {
		return nil, fmt.Errorf("%s names no command", what)
	}
	return out, nil
}

// NormalizeWrapper validates a wrapper definition and returns its stored
// form.
func NormalizeWrapper(d WrapperDef) (WrapperDef, error) {
	out := WrapperDef{Name: strings.TrimSpace(d.Name)}
	bad := func(format string, args ...any) (WrapperDef, error) {
		return WrapperDef{}, fmt.Errorf("%w: wrapper %s: %s", ErrInvalid, out.Name, fmt.Sprintf(format, args...))
	}
	if !reWrapperName.MatchString(out.Name) || out.Name == NoWrapper {
		return WrapperDef{}, fmt.Errorf("%w: wrapper name %q: lower-case letters, digits, '.', '_' and '-', at most 40 (and not %q)", ErrInvalid, d.Name, NoWrapper)
	}
	var err error
	if out.Argv, err = normalizeArgv("argv", d.Argv, maxWrapperArgs); err != nil {
		return bad("%v", err)
	}
	if len(out.Argv) == 0 {
		return bad("argv is empty: a wrapper is the command a run starts behind")
	}
	if ph := UnknownPlaceholder(out.Argv); ph != "" {
		return bad("argv uses %s, which is not a placeholder (%s)", ph, strings.Join(WrapperPlaceholders, " "))
	}
	if _, envOnly := EnvOnlyWrapper(out.Argv); envOnly {
		return bad("argv only sets variables, so nothing would go around the run: a wrapper runs a command (a worker whose whole host is a container says so with worker.contained_by)")
	}
	// A leading `env K=V …` sets variables for the agent like a stage's
	// env does: the same ones are refused.
	for _, k := range envPrefixNames(out.Argv) {
		if why := StageEnvRefused(k); why != "" {
			return bad("argv sets %s: %s", k, why)
		}
	}
	if out.Check, err = normalizeArgv("check", d.Check, maxWrapperCheck); err != nil {
		return bad("%v", err)
	}
	if len(out.Check) == 0 {
		out.Check = nil
	}
	if ph := rePlaceholder.FindString(strings.Join(out.Check, " ")); ph != "" && slices.Contains(WrapperPlaceholders, ph) {
		return bad("check uses %s: a check runs outside any run, so it has no run to name", ph)
	}
	return out, nil
}

func describeWrapper(d WrapperDef) string {
	s := "argv " + Describe(mustJSON(d.Argv))
	if len(d.Check) > 0 {
		s += ", check " + Describe(mustJSON(d.Check))
	}
	if len(s) > 1500 {
		s = s[:1500] + "… (the revision holds the rest)"
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	buf, _ := json.Marshal(v)
	return buf
}
