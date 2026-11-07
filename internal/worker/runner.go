// Package worker is the execution plane: a deployment-agnostic daemon that
// claims run stages from skopos, executes them with an agent CLI, and
// reports results. Extracted from the live-proven agent-trial executor.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Outcome classifies how an agent run ended.
type Outcome string

const (
	OutcomeOK        Outcome = "ok"
	OutcomeQuota     Outcome = "quota"     // allowance or rate limit: pause, resume later
	OutcomeTransient Outcome = "transient" // overloaded / 5xx / network: retry
	OutcomeAuth      Outcome = "auth"      // login problem: tell Martin
	OutcomeTimeout   Outcome = "timeout"
	OutcomeError     Outcome = "error"
)

// AgentRun is one agent invocation (runner-agnostic; the claude runner is
// first, others map onto the same shape).
type AgentRun struct {
	Bin          string
	Dir          string
	Prompt       string
	SessionID    string // new session id, or the one to resume
	Resume       bool
	Model        string
	Effort       string // reasoning effort, validated for the runner's kind at load
	Schema       string // JSON schema for structured output
	AllowedTools []string
	Settings     string // settings JSON (deny rules); deny beats allow
	Timeout      time.Duration
	LogFile      string
	// TokenEnvs are the deployment's configured push-token variables —
	// passed in, not package state (round 7), so scrubbing has no hidden
	// config dependency. FileEnvKeys are everything the env file provided;
	// all of it is dropped from the agent environment.
	TokenEnvs   []string
	FileEnvKeys []string
	// Env carries the account's credentials (API key/config dir) — injected
	// per run by the placement pool — plus the stage profile's env;
	// scrubbing still strips everything else credential-shaped.
	Env map[string]string
	// MCPConfig is the only MCP source (--strict-mcp-config); empty = none.
	MCPConfig string
	// Wrapper is the operator's argv prefix (placeholders already
	// expanded); the agent command is appended to it.
	Wrapper []string
	// Runner names the runner (account runner; "" = claude). Granted: the
	// stage profile grants capabilities (tools/env/allow). Sandboxed: the
	// profile settings enable the runner's own sandbox. runAgent uses both
	// for the fail-closed containment rule.
	Runner    string
	Granted   bool
	Sandboxed bool
}

// withPWD pins $PWD to the run's directory. The scrubbed environment
// inherits the daemon's PWD (under a service: the config dir), and some
// agent CLIs trust $PWD over the process's real working directory —
// opencode did, and edited + committed in the wrong repository. Every
// agent command goes through this.
func withPWD(env []string, dir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PWD=") {
			out = append(out, kv)
		}
	}
	if dir == "" {
		return out
	}
	return append(out, "PWD="+dir)
}

// claudeRunner is Claude Code (verified lockdown: lockdownArgs).
type claudeRunner struct{ bin string }

func (claudeRunner) Verified() bool       { return true }
func (claudeRunner) HonorsSettings() bool { return true }
func (c claudeRunner) Check(ctx context.Context, pathEnv string) error {
	return LockdownSupported(ctx, c.bin, pathEnv)
}
func (c claudeRunner) EffortSupported(ctx context.Context, pathEnv string) error {
	return effortSupported(ctx, c.bin, pathEnv, []string{"--help"}, "--effort", "Claude Code")
}
func (c claudeRunner) Run(ctx context.Context, r AgentRun) AgentResult {
	r.Bin = c.bin
	return runClaude(ctx, r)
}

// lockdownArgs are the flags every agent run gets (capability profiles §1,
// verified against Claude Code 2.1.300):
//   - --setting-sources "": no user/project/local settings — so no
//     worktree CLAUDE.md (during review that file is the implementer's
//     branch), no worker-user CLAUDE.md, allow rules, hooks or MCP servers;
//     instructions reach the agent only through the prompt;
//   - --restricted: file tools confined to the working directory (Read
//     otherwise returned the worker env file from outside the worktree);
//     it drops code-running built-ins unless --tools names them, so the
//     caller passes the derived --tools list;
//   - --strict-mcp-config: MCP servers only from --mcp-config.
func lockdownArgs(r AgentRun) []string {
	args := []string{"--setting-sources", "", "--restricted", "--strict-mcp-config"}
	if t := builtinTools(r.AllowedTools); len(t) > 0 {
		args = append(args, "--tools", strings.Join(t, ","))
	}
	if r.MCPConfig != "" {
		args = append(args, "--mcp-config", r.MCPConfig)
	}
	return args
}

// builtinTools derives --tools from the allowlist: the built-in tool names
// (pattern arguments stripped, MCP tools excluded — they are not built-ins),
// first occurrence order, deduped.
func builtinTools(allowed []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range allowed {
		name, _, _ := strings.Cut(strings.TrimSpace(a), "(")
		if name == "" || strings.HasPrefix(name, "mcp__") || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// agentCommand assembles argv: the wrapper prefix (if any), then the agent
// binary and its arguments.
func agentCommand(r AgentRun, args []string) (string, []string) {
	if len(r.Wrapper) == 0 {
		return r.Bin, args
	}
	argv := append(append(append([]string{}, r.Wrapper[1:]...), r.Bin), args...)
	return r.Wrapper[0], argv
}

// AgentResult is the outcome.
type AgentResult struct {
	Outcome    Outcome
	SessionID  string
	Structured json.RawMessage
	Text       string
	CostUSD    float64
	ResetAt    time.Time // quota: when the allowance resets
	Detail     string
}

// scrubEnv removes credentials an agent must never use. Beyond the fixed
// names, every configured push-token variable and every *TOKEN* / *SECRET* /
// *_KEY variable is dropped UNLESS it is one the runner explicitly injects
// for its account (review H5: a custom token_env name sailed through the
// fixed list into the agent's environment).
func scrubEnv(extra map[string]string, tokenEnvs, fileEnvKeys []string) []string {
	drop := map[string]bool{
		"SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true, "GH_TOKEN": true, "GITHUB_TOKEN": true,
		"GITEA_TOKEN": true, "SLACK_BOT_TOKEN": true, "SKOPOS_API_KEY": true,
		"SKOPOS_APPROVER_KEY": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true,
		// The log output's URL and header may carry credentials.
		"LOG_OUTPUT_URL": true, "LOG_OUTPUT_HEADER": true,
	}
	for _, tok := range tokenEnvs {
		drop[tok] = true
	}
	// Env-file values are the daemon's secrets wholesale (a GITEA_PASS-style
	// name matches no pattern) — nothing from the file reaches the agent.
	for _, k := range fileEnvKeys {
		drop[k] = true
	}
	keep := map[string]bool{}
	for k := range extra {
		keep[k] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] || strings.HasPrefix(k, "GIT_CONFIG") {
			continue
		}
		// Pattern sweep: any credential-looking variable not explicitly
		// injected for the account is dropped.
		if (strings.Contains(k, "TOKEN") || strings.Contains(k, "SECRET") || strings.HasSuffix(k, "_KEY")) && !keep[k] {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return append(env,
		"GIT_SSH_COMMAND=/usr/bin/false",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
	)
}

type claudeJSON struct {
	Type             string          `json:"type"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	SessionID        string          `json:"session_id"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	APIErrorStatus   *int            `json:"api_error_status"`
}

// runClaude executes the claude CLI headless with the guards applied. Proven
// in agent-trial through weeks of live runs; carried over nearly verbatim.
func runClaude(ctx context.Context, r AgentRun) AgentResult {
	// stream-json: events as they happen (the run log shows them live); the
	// result event carries what --output-format json printed alone.
	args := []string{"-p", r.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", "dontAsk"}
	if r.Resume {
		args = append(args, "--resume", r.SessionID)
	} else if r.SessionID != "" {
		args = append(args, "--session-id", r.SessionID)
	}
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.Effort != "" {
		args = append(args, "--effort", r.Effort)
	}
	if r.Schema != "" {
		args = append(args, "--json-schema", r.Schema)
	}
	if len(r.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(r.AllowedTools, ","))
	}
	if r.Settings != "" {
		args = append(args, "--settings", r.Settings)
	}
	args = append(args, lockdownArgs(r)...)

	cctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	name, argv := agentCommand(r, args)
	cmd := exec.CommandContext(cctx, name, argv...)
	containProcess(cmd) // a timeout stops the agent and every tool process it started
	cmd.Dir = r.Dir
	cmd.Env = withPWD(scrubEnv(r.Env, r.TokenEnvs, r.FileEnvKeys), r.Dir)
	// The event stream is not kept whole (tool results make it large): the
	// raw copy streams to the log file, the result event and a tail are
	// kept for the outcome.
	var stderr bytes.Buffer
	result := &claudeResultCatcher{}
	raw, closeRaw := openRawLog(r.LogFile)
	stdoutW, stderrW, flush := teeOutput(ctx, io.MultiWriter(raw, result), &stderr, claudeEventLines)
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	err := settled(cmd, cmd.Run())
	flush()
	closeRaw(stderr.Bytes())

	res := AgentResult{SessionID: r.SessionID}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.Outcome, res.Detail = OutcomeTimeout, fmt.Sprintf("no result after %s", r.Timeout)
		return res
	}
	var out claudeJSON
	if jerr := json.Unmarshal(result.result(), &out); jerr != nil {
		text := result.plainText() + "\n" + stderr.String()
		res.Outcome, res.Detail = classifyText(text, nil, err)
		res.ResetAt = parseReset(text, time.Now())
		return res
	}
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	res.Text, res.CostUSD, res.Structured = out.Result, out.TotalCostUSD, out.StructuredOutput
	if !out.IsError && err == nil {
		res.Outcome = OutcomeOK
		return res
	}
	text := out.Result + "\n" + stderr.String()
	res.Outcome, res.Detail = classifyText(text, out.APIErrorStatus, err)
	res.ResetAt = parseReset(text, time.Now())
	return res
}

// claudeResultCatcher keeps the stream's result event (the last one, if
// several) and the stdout lines that are not events (an old CLI, an error
// printed before the first event) — never the events themselves: their tool
// output must not decide the outcome (a file saying "rate limit" is not a
// quota).
type claudeResultCatcher struct {
	mu       sync.Mutex
	pending  []byte
	skipping bool // inside a line past maxEventLine: dropped up to its newline
	last     []byte
	plain    []byte
}

// maxEventLine bounds one stream line the catcher holds.
const maxEventLine = 64 << 20

func (c *claudeResultCatcher) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = append(c.pending, p...)
	for {
		i := bytes.IndexByte(c.pending, '\n')
		if i < 0 {
			break
		}
		if !c.skipping {
			c.consider(c.pending[:i])
		}
		c.skipping = false
		c.pending = c.pending[i+1:]
	}
	if len(c.pending) > maxEventLine { // not an event we need; drop it whole
		c.pending, c.skipping = nil, true
	}
	return len(p), nil
}

func (c *claudeResultCatcher) consider(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var probe struct {
		Type string `json:"type"`
	}
	if line[0] != '{' || json.Unmarshal(line, &probe) != nil || probe.Type == "" {
		c.plain = append(append(c.plain, line...), '\n')
		if over := len(c.plain) - 64<<10; over > 0 {
			c.plain = c.plain[over:]
		}
		return
	}
	if probe.Type == "result" {
		c.last = bytes.Clone(line)
	}
}

// result is the result event ("" when the stream had none); a trailing
// line without a newline counts.
func (c *claudeResultCatcher) result() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) > 0 && !c.skipping {
		c.consider(c.pending)
	}
	c.pending = nil
	return c.last
}

// plainText is what stdout printed outside the event stream.
func (c *claudeResultCatcher) plainText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.plain)
}

// openRawLog streams an agent's raw stdout into its log file (state dir,
// 0600); the returned close appends stderr. path "" = no file.
func openRawLog(path string) (io.Writer, func(stderr []byte)) {
	if path == "" {
		return io.Discard, func([]byte) {}
	}
	_ = os.MkdirAll(logsDirOf(path), 0o700)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return io.Discard, func([]byte) {}
	}
	return f, func(stderr []byte) {
		_, _ = f.Write(append([]byte("\n--- stderr\n"), stderr...))
		_ = f.Close()
	}
}

func logsDirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "."
}

var (
	reQuota     = regexp.MustCompile(`(?i)usage limit|limit reached|hit your (usage )?limit|rate[ _-]?limit|quota|too many requests|out of (extra )?usage`)
	reTransient = regexp.MustCompile(`(?i)overloaded|internal server error|bad gateway|service unavailable|timed? ?out|connection (reset|refused)|ECONNRESET|network`)
	reAuth      = regexp.MustCompile(`(?i)not logged in|please run /login|authentication|unauthori[sz]ed|invalid api key|oauth token`)
)

func classifyText(text string, status *int, runErr error) (Outcome, string) {
	detail := firstLine(text)
	if status != nil {
		switch {
		case *status == 429:
			return OutcomeQuota, detail
		case *status == 401 || *status == 403:
			return OutcomeAuth, detail
		case *status >= 500:
			return OutcomeTransient, detail
		}
	}
	switch {
	case reQuota.MatchString(text):
		return OutcomeQuota, detail
	case reAuth.MatchString(text):
		return OutcomeAuth, detail
	case reTransient.MatchString(text):
		return OutcomeTransient, detail
	}
	if detail == "" && runErr != nil {
		detail = runErr.Error()
	}
	return OutcomeError, detail
}

var (
	reResetEpoch = regexp.MustCompile(`\|(\d{10})\b`)
	reResetClock = regexp.MustCompile(`(?i)resets?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)
)

func parseReset(text string, now time.Time) time.Time {
	if m := reResetEpoch.FindStringSubmatch(text); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if t := time.Unix(n, 0); t.After(now) && t.Before(now.Add(8*24*time.Hour)) {
				return t
			}
		}
	}
	if m := reResetClock.FindStringSubmatch(text); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi := 0
		if m[2] != "" {
			mi, _ = strconv.Atoi(m[2])
		}
		switch strings.ToLower(m[3]) {
		case "pm":
			if h < 12 {
				h += 12
			}
		case "am":
			if h == 12 {
				h = 0
			}
		}
		if h > 23 || mi > 59 {
			return time.Time{}
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		return t
	}
	return time.Time{}
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 300 {
				l = l[:300]
			}
			return l
		}
	}
	return ""
}

// LockdownSupported verifies the agent CLI understands the flags every run
// now passes (lockdownArgs). An older CLI would fail EVERY run on an unknown
// option, so the daemon refuses to start and `worker check` reports it.
// pathEnv, when set, is the PATH to resolve bin on (the service's).
func LockdownSupported(ctx context.Context, bin, pathEnv string) error {
	missing, err := missingFlags(ctx, bin, pathEnv, []string{"--help"}, "--setting-sources", "--restricted", "--strict-mcp-config", "--mcp-config", "--tools")
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s does not support %s — upgrade the agent CLI (pipeline runs need them to load no user/project settings and keep file tools inside the worktree)", bin, strings.Join(missing, ", "))
	}
	return nil
}

// missingFlags runs `bin <helpArgs>` on pathEnv (bounded) and returns the
// flags its help text does not mention.
func missingFlags(ctx context.Context, bin, pathEnv string, helpArgs []string, flags ...string) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, helpArgs...)
	if pathEnv != "" {
		cmd.Env = append(os.Environ(), "PATH="+pathEnv)
		if p, err := lookPathIn(bin, pathEnv); err == nil {
			cmd.Path = p
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("running %s %s: %v", bin, strings.Join(helpArgs, " "), err)
	}
	var missing []string
	for _, f := range flags {
		if !strings.Contains(string(out), f) {
			missing = append(missing, f)
		}
	}
	return missing, nil
}

// effortSupported: the CLI's help names its effort flag (an older build
// would refuse it on every run, or ignore it).
func effortSupported(ctx context.Context, bin, pathEnv string, helpArgs []string, flag, upgrade string) error {
	missing, err := missingFlags(ctx, bin, pathEnv, helpArgs, flag)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s does not support %s, which an account's effort needs — upgrade %s", bin, flag, upgrade)
	}
	return nil
}

// lookPathIn resolves bin against an explicit PATH value.
func lookPathIn(bin, pathEnv string) (string, error) {
	if strings.Contains(bin, "/") {
		return bin, nil
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			continue
		}
		p := dir + "/" + bin
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on PATH", bin)
}
