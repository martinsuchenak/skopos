package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// codexRunner is the OpenAI Codex CLI (`codex exec --json`). Lockdown,
// verified live against codex-cli 0.163.0 on macOS (TestLiveCodexLockdown;
// the instruction and sandbox checks in TestCodexLockdownProbe spend no
// tokens):
//   - --ignore-user-config + --ignore-rules: $CODEX_HOME/config.toml (MCP
//     servers, plugins, trusted projects — and with them a project's
//     .codex/config.toml) and execpolicy rules are not loaded; everything
//     the run needs is passed with -c, and --strict-config rejects a key
//     this codex doesn't know;
//   - project_doc_max_bytes=0: no AGENTS.md from the worktree. CODEX_HOME's
//     own AGENTS.md has no switch, so an account directory holding one is
//     refused (codexHomeProblem);
//   - skills.include_instructions=false and the disabled features below: no
//     skill listings (CODEX_HOME/skills, the repo's .codex / .agents
//     skills), plugins, apps, hooks or browser/computer use;
//   - a permission profile: reads only from :minimal (what a shell needs,
//     minus the temp directories), the worktree, the clone's git directory
//     and codex's own install; no writes; network off; approval_policy never (no escalation). Files
//     outside the worktree — the worker's env file, the account's auth —
//     are unreadable by the agent's shell;
//   - --output-schema: native structured output (strict: codexStrictSchema).
//
// Read-only stages only (plan, review, second opinion, compare): codex
// write-protects .git inside writable roots, so an implementer could not
// commit. A stage that asks for edits is refused.
type codexRunner struct {
	bin string
	// skipHomeCheck: the live test may run on a personal login whose own
	// AGENTS.md then rides along (production refuses that directory).
	skipHomeCheck bool
}

func (codexRunner) Verified() bool       { return true }
func (codexRunner) HonorsSettings() bool { return false }

// ReadOnly: the runner refuses stages that edit (runner list says so).
func (codexRunner) ReadOnly() bool { return true }

// codexFeaturesOff are switched off on every run. Check verifies each one
// still exists, so a renamed feature fails closed instead of silently
// staying on.
var codexFeaturesOff = []string{"plugins", "apps", "hooks", "skill_search", "memories", "browser_use", "computer_use"}

func (c codexRunner) Check(ctx context.Context, pathEnv string) error {
	run := func(args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, c.bin, args...)
		if pathEnv != "" {
			cmd.Env = append(os.Environ(), "PATH="+pathEnv)
			if p, err := lookPathIn(c.bin, pathEnv); err == nil {
				cmd.Path = p
			}
		}
		out, err := cmd.CombinedOutput()
		if err != nil && len(out) == 0 {
			return "", fmt.Errorf("running %s %s: %v", c.bin, strings.Join(args, " "), err)
		}
		return string(out), nil
	}
	help, err := run("exec", "--help")
	if err != nil {
		return err
	}
	var missing []string
	for _, f := range []string{"--ignore-user-config", "--ignore-rules", "--strict-config", "--output-schema", "--json", "--cd", "--disable"} {
		if !strings.Contains(help, f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s exec does not support %s — upgrade codex (runs need them to load no user config and keep to the worktree)", c.bin, strings.Join(missing, ", "))
	}
	features, err := run("features", "list")
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for line := range strings.SplitSeq(features, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			known[f[0]] = true
		}
	}
	for _, f := range codexFeaturesOff {
		if !known[f] {
			return fmt.Errorf("%s has no feature %q to switch off — this codex is newer than the verified lockdown; re-run TestLiveCodexLockdown", c.bin, f)
		}
	}
	return nil
}

// codexHome is the CODEX_HOME a run uses: the account's, else the worker's
// environment, else ~/.codex.
func codexHome(env map[string]string) string {
	if h := env["CODEX_HOME"]; h != "" {
		return expandHome(h, userHome())
	}
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	return filepath.Join(userHome(), ".codex")
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// CodexHomeProblem reports why the codex login directory an account env
// resolves to can't be used ("" = fine); the wizard warns with it.
func CodexHomeProblem(env map[string]string) string { return codexHomeProblem(env) }

// codexHomeProblem: codex always loads CODEX_HOME's own AGENTS.md (there is
// no switch), so a login directory holding one is refused.
func codexHomeProblem(env map[string]string) string {
	home := codexHome(env)
	for _, f := range []string{"AGENTS.override.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			return fmt.Sprintf("%s has %s, which codex loads into every run — use a login directory of its own (CODEX_HOME=<dir> codex login)", home, f)
		}
	}
	return ""
}

// codexWrites: the stage asks for edits (implement), which this adapter
// does not run.
func codexWrites(allowed []string) bool {
	for _, a := range allowed {
		name, pattern, _ := strings.Cut(strings.TrimSpace(a), "(")
		switch {
		case name == "Edit" || name == "Write" || name == "MultiEdit":
			return true
		case name == "Bash" && (strings.HasPrefix(pattern, "git commit") || strings.HasPrefix(pattern, "git add")):
			return true
		}
	}
	return false
}

func (c codexRunner) Run(ctx context.Context, r AgentRun) AgentResult {
	if codexWrites(r.AllowedTools) {
		return AgentResult{Outcome: OutcomeError, Detail: "the codex runner runs read-only stages (plan, review, second opinion, compare); implement is not verified for it yet"}
	}
	if p := codexHomeProblem(r.Env); p != "" && !c.skipHomeCheck {
		return AgentResult{Outcome: OutcomeError, Detail: p}
	}
	tmp, err := os.MkdirTemp("", "skopos-codex-")
	if err != nil {
		return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	args := []string{"exec"}
	if r.Resume && r.SessionID != "" {
		args = append(args, "resume") // resume has no --cd: the process cwd is the worktree
	} else {
		args = append(args, "--cd", r.Dir)
	}
	args = append(args, codexLockdownArgs(ctx, c.bin, r)...)
	if r.Model != "" {
		args = append(args, "-m", r.Model)
	}
	if r.Effort != "" {
		args = append(args, "-c", `model_reasoning_effort="`+r.Effort+`"`)
	}
	if r.Schema != "" {
		strict, err := codexStrictSchema(r.Schema)
		if err != nil {
			return AgentResult{Outcome: OutcomeError, Detail: "schema: " + err.Error()}
		}
		path := filepath.Join(tmp, "schema.json")
		if err := os.WriteFile(path, []byte(strict), 0o600); err != nil {
			return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
		}
		args = append(args, "--output-schema", path)
	}
	if r.Resume && r.SessionID != "" {
		args = append(args, r.SessionID)
	}
	args = append(args, "-") // the prompt on stdin (never argv: ps shows argv)

	cctx, cancel := context.WithTimeout(ctx, orTimeout(r.Timeout))
	defer cancel()
	sub := r
	sub.Bin = c.bin
	name, argv := agentCommand(sub, args)
	cmd := exec.CommandContext(cctx, name, argv...)
	containProcess(cmd) // a timeout stops the agent and every tool process it started
	cmd.Dir = r.Dir
	// The agent's shell can't read the worker user's ~/.gitconfig (outside
	// the sandbox), and git refuses to run when it exists but is unreadable.
	cmd.Env = append(withPWD(scrubEnv(r.Env, r.TokenEnvs, r.FileEnvKeys), r.Dir),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Stdin = strings.NewReader(r.Prompt)
	var stdout, stderr bytes.Buffer
	var flush func()
	cmd.Stdout, cmd.Stderr, flush = teeOutput(ctx, &stdout, &stderr, codexEventLines)
	runErr := settled(cmd, cmd.Run())
	flush()
	writeRunLog(r.LogFile, stdout.Bytes(), stderr.Bytes())

	res := parseCodexEvents(stdout.Bytes(), time.Now())
	if res.SessionID == "" {
		res.SessionID = r.SessionID
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.Outcome, res.Detail = OutcomeTimeout, fmt.Sprintf("no result after %s", r.Timeout)
		return res
	}
	if res.Outcome == "" && runErr != nil {
		text := res.Detail + "\n" + stderr.String()
		res.Outcome, res.Detail = classifyText(text, nil, runErr)
		res.ResetAt = codexReset(text, time.Now())
	}
	if res.Outcome == "" {
		res.Outcome, res.Detail = OutcomeError, "codex ended without finishing the turn: "+firstLine(res.Detail+"\n"+stderr.String())
	}
	if res.Outcome != OutcomeOK || r.Schema == "" {
		return res
	}
	obj, err := codexStructured(res.Text, r.Schema)
	if err != nil {
		res.Outcome, res.Detail = OutcomeError, "no valid structured result: "+err.Error()
		return res
	}
	res.Structured = obj
	return res
}

// codexLockdownArgs: the flags every codex run gets (see codexRunner).
func codexLockdownArgs(ctx context.Context, bin string, r AgentRun) []string {
	args := []string{"--ignore-user-config", "--ignore-rules", "--strict-config", "--json",
		"-c", "project_doc_max_bytes=0",
		"-c", "skills.include_instructions=false",
		"-c", `approval_policy="never"`,
		"-c", `web_search="disabled"`,
		"-c", "allow_login_shell=false",
		"-c", `default_permissions="skopos"`,
		"-c", "permissions.skopos.filesystem=" + codexFilesystem(codexReadRoots(ctx, bin, r)),
		"-c", "permissions.skopos.network.enabled=false",
	}
	for _, f := range codexFeaturesOff {
		args = append(args, "--disable", f)
	}
	return args
}

// codexFilesystem renders the profile's filesystem table: the shell's
// minimum and the worktree, plus the extra read roots — read, never write.
// The temp directories :minimal would open (other runs' files live there)
// are closed; the worktree stays readable even when it sits inside one.
func codexFilesystem(extra []string) string {
	parts := []string{`":minimal"="read"`, `":tmpdir"="none"`, `":slash_tmp"="none"`, `":workspace_roots"="read"`}
	for _, p := range extra {
		q, _ := json.Marshal(p) // a JSON string is a TOML basic string
		parts = append(parts, string(q)+`="read"`)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// codexReadRoots are what a confined run still needs to read: the clone's
// git directory (a review worktree's .git points into it), codex's own
// install (its sandbox helper re-executes the binary), and on macOS the
// developer tools behind /usr/bin/git.
func codexReadRoots(ctx context.Context, bin string, r AgentRun) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		if _, err := os.Stat(p); err != nil {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if r.Dir != "" {
		gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		o, err := exec.CommandContext(gctx, "git", "-C", r.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
		cancel()
		if err == nil {
			gd := strings.TrimSpace(string(o))
			if rel, err := filepath.Rel(r.Dir, gd); err != nil || strings.HasPrefix(rel, "..") {
				add(gd)
			}
		}
	}
	add(codexInstallDir(bin, r.Env))
	if runtime.GOOS == "darwin" {
		add("/Library/Developer/CommandLineTools")
		if o, err := exec.CommandContext(ctx, "xcode-select", "-p").Output(); err == nil {
			p := strings.TrimSpace(string(o))
			if i := strings.Index(p, ".app/"); i > 0 {
				p = p[:i+len(".app")]
			}
			add(p)
		}
	}
	sort.Strings(out)
	return out
}

// codexInstallDir: the directory the codex binary really lives in (through
// symlinks), one level up from a bin/ directory — but never the user's home
// or the filesystem root.
func codexInstallDir(bin string, env map[string]string) string {
	path := os.Getenv("PATH")
	if p := env["PATH"]; p != "" {
		path = p
	}
	p, err := lookPathIn(bin, path)
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	dir := filepath.Dir(p)
	if filepath.Base(dir) == "bin" {
		if up := filepath.Dir(dir); up != "/" && up != userHome() {
			dir = up
		}
	}
	if dir == "/" || dir == userHome() {
		return ""
	}
	return dir
}

// parseCodexEvents reads the JSONL stream: the thread id (the session to
// resume), the last agent message, and how the turn ended. Plain `error`
// events are also retries ("Reconnecting… 2/5"); only turn.failed is final.
func parseCodexEvents(out []byte, now time.Time) AgentResult {
	var res AgentResult
	lastErr := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			res.SessionID = ev.ThreadID
		case "item.completed":
			if ev.Item.Type == "agent_message" {
				res.Text = ev.Item.Text
			}
		case "error":
			lastErr = ev.Message
		case "turn.completed":
			res.Outcome, res.Detail = OutcomeOK, ""
		case "turn.failed":
			msg, status := codexErrorMessage(ev.Error.Message)
			res.Outcome, res.Detail = classifyText(msg, status, errors.New(msg))
			res.ResetAt = codexReset(ev.Error.Message, now)
		}
	}
	if res.Outcome == "" && lastErr != "" {
		msg, _ := codexErrorMessage(lastErr)
		res.Detail = msg
	}
	return res
}

// codexErrorMessage unwraps the API error codex embeds as a JSON string
// ({"type":"error","status":400,"error":{"message":…}}).
func codexErrorMessage(raw string) (string, *int) {
	var e struct {
		Status int `json:"status"`
		Error  struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &e) != nil || e.Error.Message == "" {
		return raw, nil
	}
	msg := e.Error.Message
	if e.Error.Type != "" {
		msg = e.Error.Type + ": " + msg
	}
	if e.Status == 0 {
		return msg, nil
	}
	return msg, &e.Status
}

var (
	reCodexResetSecs = regexp.MustCompile(`resets_in_seconds\\?"?\s*:\s*(\d+)`)
	reCodexResetAt   = regexp.MustCompile(`resets_at\\?"?\s*:\s*(\d{10})\b`)
	reTryAgainIn     = regexp.MustCompile(`(?i)try again in\s+((?:\d+\s*(?:days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)[\s,and]*)+)`)
	reDurationPart   = regexp.MustCompile(`(?i)(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b`)
)

// codexReset finds when a usage limit resets: the API's resets_in_seconds
// / resets_at, else "try again in 2 hours 5 minutes", else the shared
// "resets at 5pm" forms.
func codexReset(text string, now time.Time) time.Time {
	if m := reCodexResetSecs.FindStringSubmatch(text); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 && n < 8*24*3600 {
			return now.Add(time.Duration(n) * time.Second)
		}
	}
	if m := reCodexResetAt.FindStringSubmatch(text); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if t := time.Unix(n, 0); t.After(now) && t.Before(now.Add(8*24*time.Hour)) {
				return t
			}
		}
	}
	if m := reTryAgainIn.FindStringSubmatch(text); m != nil {
		var d time.Duration
		for _, p := range reDurationPart.FindAllStringSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(p[1])
			switch u := strings.ToLower(p[2]); {
			case strings.HasPrefix(u, "d"):
				d += time.Duration(n) * 24 * time.Hour
			case strings.HasPrefix(u, "h"):
				d += time.Duration(n) * time.Hour
			case strings.HasPrefix(u, "m"):
				d += time.Duration(n) * time.Minute
			default:
				d += time.Duration(n) * time.Second
			}
		}
		if d > 0 && d < 8*24*time.Hour {
			return now.Add(d)
		}
	}
	return parseReset(text, now)
}

// codexStrictSchema rewrites a stage schema into the strict form OpenAI's
// structured outputs require: every object closed (additionalProperties
// false) and every property required — the ones the stage leaves optional
// become nullable, and codexStructured drops the nulls again.
func codexStrictSchema(schema string) (string, error) {
	var s map[string]any
	if err := json.Unmarshal([]byte(schema), &s); err != nil {
		return "", err
	}
	strictNode(s)
	b, err := json.Marshal(s)
	return string(b), err
}

func strictNode(s map[string]any) {
	switch s["type"] {
	case "object":
		props, _ := s["properties"].(map[string]any)
		required := map[string]bool{}
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				required[fmt.Sprint(r)] = true
			}
		}
		keys := make([]string, 0, len(props))
		for k, v := range props {
			keys = append(keys, k)
			ps, ok := v.(map[string]any)
			if !ok {
				continue
			}
			strictNode(ps)
			if !required[k] {
				nullable(ps)
			}
		}
		sort.Strings(keys)
		all := make([]any, len(keys))
		for i, k := range keys {
			all[i] = k
		}
		s["required"] = all
		s["additionalProperties"] = false
		if props == nil {
			s["properties"] = map[string]any{}
		}
	case "array":
		if is, ok := s["items"].(map[string]any); ok {
			strictNode(is)
		}
	}
}

// nullable lets an optional property be null (type [T, "null"]; an enum
// gains null too).
func nullable(s map[string]any) {
	if t, ok := s["type"].(string); ok {
		s["type"] = []any{t, "null"}
	}
	if enum, ok := s["enum"].([]any); ok {
		s["enum"] = append(enum, nil)
	}
}

// codexStructured parses the final message (the schema-constrained JSON),
// drops the nulls the strict form forced in, and validates the result
// against the stage's own schema.
func codexStructured(text, schema string) (json.RawMessage, error) {
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &v); err != nil {
		// Not pure JSON (an older codex, a model that wrapped it): fall back
		// to the text extraction the other runners use.
		return extractStructured(text, schema)
	}
	v = dropNulls(v)
	if err := validateSchema(v, schema); err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}

func dropNulls(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if e == nil {
				delete(t, k)
				continue
			}
			t[k] = dropNulls(e)
		}
	case []any:
		for i := range t {
			t[i] = dropNulls(t[i])
		}
	}
	return v
}
