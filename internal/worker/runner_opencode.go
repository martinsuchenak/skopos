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
	"strings"
)

// opencodeRunner is opencode (`opencode run --format json`). Lockdown,
// verified live against opencode 1.19.5 (TestLiveOpencodeLockdown):
//   - an EMPTY config dir (OPENCODE_CONFIG_DIR) plus
//     OPENCODE_DISABLE_PROJECT_CONFIG / _CLAUDE_CODE / _EXTERNAL_SKILLS: the
//     worker user's ~/.config/opencode (AGENTS.md, agents, MCP, plugins) and
//     the repo's AGENTS.md / opencode.json / .claude are not loaded;
//   - OPENCODE_PERMISSION: deny-all first, then the stage's allows, then
//     the hard denies — opencode applies the LAST matching rule, and tools
//     with no allow are removed from the model's tool set;
//     external_directory is denied, which opencode enforces for its file
//     tools (read, grep, glob, list, edit) and only a few bash commands, so
//     the bash allow list is followed by opencodeBashEscapes; edits to git's
//     own files are denied (opencodeEdit);
//   - the operator's base config (providers/models) via OPENCODE_CONFIG,
//     MCP servers translated from worker.mcp_config via
//     OPENCODE_CONFIG_CONTENT.
//
// opencode has no structured-output option: the schema rides in the prompt
// and the worker extracts + validates the final JSON object (one repair
// turn on the same session when it does not validate).
type opencodeRunner struct {
	bin    string
	config string
}

func (opencodeRunner) Verified() bool       { return true }
func (opencodeRunner) HonorsSettings() bool { return false }

func (o opencodeRunner) EffortSupported(ctx context.Context, pathEnv string) error {
	if opencodeVersion(ctx, o.bin, pathEnv).v2() {
		return nil // the effort rides on the model as its #variant
	}
	return effortSupported(ctx, o.bin, pathEnv, []string{"run", "--help"}, "--variant", "opencode")
}

func (o opencodeRunner) Check(ctx context.Context, pathEnv string) error {
	if v := opencodeVersion(ctx, o.bin, pathEnv); v.v2() {
		if err := o.checkV2(ctx, v, pathEnv); err != nil {
			return err
		}
		return o.checkConfig()
	}
	missing, err := missingFlags(ctx, o.bin, pathEnv, []string{"run", "--help"}, "--format", "--session", "--model")
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s run does not support %s — upgrade opencode", o.bin, strings.Join(missing, ", "))
	}
	return o.checkConfig()
}

// checkConfig: the operator's base config (providers, models) exists.
func (o opencodeRunner) checkConfig() error {
	if o.config != "" {
		if _, err := os.Stat(o.config); err != nil {
			return fmt.Errorf("opencode base config %s: %v", o.config, err)
		}
	}
	return nil
}

func (o opencodeRunner) Run(ctx context.Context, r AgentRun) AgentResult {
	prompt := r.Prompt
	if r.Schema != "" {
		prompt += schemaInstruction(r.Schema)
	}
	res := o.invoke(ctx, r, prompt, r.SessionID, r.Resume)
	if res.Outcome != OutcomeOK || r.Schema == "" {
		return res
	}
	obj, err := extractStructured(res.Text, r.Schema)
	if err != nil && res.SessionID != "" {
		// One repair turn on the same session (local models drift).
		repair := o.invoke(ctx, r, "Your reply did not end with a valid JSON object for the schema ("+err.Error()+
			"). Reply with ONLY that JSON object, in a ```json fenced block."+schemaInstruction(r.Schema), res.SessionID, true)
		if repair.Outcome == OutcomeOK {
			repair.CostUSD += res.CostUSD
			if res.Usage != nil {
				if repair.Usage == nil {
					repair.Usage = &Usage{}
				}
				repair.Usage.Add(res.Usage)
			}
			res = repair
			obj, err = extractStructured(res.Text, r.Schema)
		}
	}
	if err != nil {
		res.Outcome, res.Detail = OutcomeError, "no valid structured result: "+err.Error()
		return res
	}
	res.Structured = obj
	return res
}

func (o opencodeRunner) invoke(ctx context.Context, r AgentRun, prompt, session string, resume bool) AgentResult {
	cfgDir, err := os.MkdirTemp(r.TempDir, "skopos-oc-cfg-")
	if err != nil {
		return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
	}
	defer func() { _ = os.RemoveAll(cfgDir) }()

	v2 := opencodeVersion(ctx, o.bin, r.Env["PATH"]).v2()
	env := withPWD(scrubEnv(r.Env, r.TokenEnvs, r.FileEnvKeys), r.Dir)
	env = append(env,
		"OPENCODE_CONFIG_DIR="+cfgDir,
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_SHARE=1",
	)
	if o.config != "" {
		env = append(env, "OPENCODE_CONFIG="+o.config)
	}
	var args []string
	if v2 {
		// opencode 2: the folder is the working directory and $PWD, the
		// rules and MCP servers live in the run's config directory.
		args = opencodeV2Args(r.Model, r.Effort, session, resume, prompt)
		var servers map[string]any
		if r.MCPConfig != "" {
			if servers, err = opencodeV2Servers(r.MCPConfig); err != nil {
				return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
			}
		}
		cfg := opencodeV2Config(r.AllowedTools, settingsDeny(r.Settings), servers)
		if err := os.WriteFile(filepath.Join(cfgDir, "opencode.json"), []byte(cfg), 0o600); err != nil {
			return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
		}
		env = append(env, "OPENCODE_DISABLE_CLAUDE_CODE_PROMPT=1")
	} else {
		// --dir AND $PWD: opencode resolves its project from $PWD, not the
		// process cwd (found live: with the daemon's PWD it worked in the
		// wrong repository).
		args = []string{"run", "--format", "json", "--dir", r.Dir}
		if r.Model != "" {
			args = append(args, "-m", r.Model)
		}
		if r.Effort != "" {
			args = append(args, "--variant", r.Effort)
		}
		if resume && session != "" {
			args = append(args, "-s", session)
		}
		args = append(args, prompt)
		env = append(env, "OPENCODE_PERMISSION="+opencodePermission(r.AllowedTools, settingsDeny(r.Settings)))
		if r.MCPConfig != "" {
			content, err := opencodeMCPContent(r.MCPConfig)
			if err != nil {
				return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
			}
			env = append(env, "OPENCODE_CONFIG_CONTENT="+content)
		}
	}

	cctx, cancel := context.WithTimeout(ctx, orTimeout(r.Timeout))
	defer cancel()
	sub := r
	sub.Bin = o.bin
	name, argv := agentCommand(sub, args)
	cmd := exec.CommandContext(cctx, name, argv...)
	containProcess(cmd) // a timeout stops the agent and every tool process it started
	cmd.Dir = r.Dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	var flush func()
	cmd.Stdout, cmd.Stderr, flush = teeOutput(ctx, &stdout, &stderr, opencodeEventLines)
	runErr := settled(cmd, cmd.Run())
	flush()
	writeRunLog(r.LogFile, stdout.Bytes(), stderr.Bytes())

	res := parseOpencodeEvents(stdout.Bytes())
	if res.SessionID == "" {
		res.SessionID = session
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.Outcome, res.Detail = OutcomeTimeout, fmt.Sprintf("no result after %s", r.Timeout)
		return res
	}
	if res.Outcome != "" { // an error event classified it
		return res
	}
	if runErr != nil {
		res.Outcome, res.Detail = classifyText(res.Text+"\n"+stderr.String(), nil, runErr)
		return res
	}
	res.Outcome = OutcomeOK
	return res
}

// parseOpencodeEvents reads the JSON event stream: text parts (joined),
// the session id, summed cost, and error events (classified).
func parseOpencodeEvents(out []byte) AgentResult {
	var res AgentResult
	var texts []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      struct {
				Text   string          `json:"text"`
				Cost   float64         `json:"cost"`
				Tokens json.RawMessage `json:"tokens"`
			} `json:"part"`
			Error struct {
				Name string `json:"name"`
				Data struct {
					Message    string `json:"message"`
					StatusCode int    `json:"statusCode"`
				} `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "text":
			texts = append(texts, ev.Part.Text)
		case "step_finish":
			res.CostUSD += ev.Part.Cost
			// One model call: its tokens (input, output, reasoning,
			// cache.read, cache.write).
			if c := usageFrom(ev.Part.Tokens); c != nil {
				if res.Usage == nil {
					res.Usage = &Usage{}
				}
				for k, v := range c {
					res.Usage.Count(k, v)
				}
				res.Usage.Count("turns", 1)
			}
		case "error":
			msg := strings.TrimSpace(ev.Error.Name + ": " + ev.Error.Data.Message)
			var status *int
			if ev.Error.Data.StatusCode != 0 {
				sc := ev.Error.Data.StatusCode
				status = &sc
			}
			res.Outcome, res.Detail = classifyText(msg, status, errors.New(msg))
		}
	}
	res.Text = strings.Join(texts, "\n")
	return res
}

// opencodePermission maps the stage allowlist (Claude-style names, the
// worker's lingua franca) onto opencode's permission config, in order:
// deny everything, allow what the stage allows, then the hard denies (git
// push/remote, network tools, MCP deny list) and external_directory.
func opencodePermission(allowed, denied []string) string {
	top := orderedJSON{{"*", "deny"}}
	var bash orderedJSON
	bashAll := false
	add := func(k string, v any) {
		for i := range top {
			if top[i].k == k {
				top[i].v = v
				return
			}
		}
		top = append(top, kv{k, v})
	}
	for _, a := range allowed {
		name, pattern, hasPattern := strings.Cut(strings.TrimSpace(a), "(")
		pattern = strings.TrimSuffix(pattern, ")")
		switch {
		case name == "Read":
			add("read", "allow")
			add("list", "allow")
		case name == "Glob":
			add("glob", "allow")
		case name == "Grep":
			add("grep", "allow")
		case name == "Edit" || name == "Write" || name == "MultiEdit":
			add("edit", opencodeEdit)
		case name == "TodoWrite":
			add("todowrite", "allow")
			add("todoread", "allow")
		case name == "Bash" && !hasPattern:
			bashAll = true
		case name == "Bash":
			prefix := strings.TrimSuffix(strings.TrimSuffix(pattern, ":*"), "*")
			bash = append(bash, kv{prefix, "allow"}, kv{prefix + " *", "allow"})
		case strings.HasPrefix(name, "mcp__"):
			add(opencodeMCPName(name)+"*", "allow")
		}
	}
	if bashAll || len(bash) > 0 {
		head := "deny"
		if bashAll {
			head = "allow"
		}
		b := orderedJSON{{"*", head}}
		b = append(b, bash...)
		for _, d := range denied {
			if n, p, ok := strings.Cut(d, "("); ok && n == "Bash" {
				p = strings.TrimSuffix(p, ")")
				if prefix, legacy := strings.CutSuffix(p, ":*"); legacy || !strings.Contains(p, "*") {
					b = append(b, kv{prefix, "deny"}, kv{prefix + " *", "deny"})
				} else {
					// A wildcard pattern (`git *--output*`) means the
					// same in opencode: kept as written.
					b = append(b, kv{p, "deny"})
				}
			}
		}
		for _, p := range opencodeBashEscapes {
			b = append(b, kv{p, "deny"})
		}
		top = append(top, kv{"bash", b})
	}
	for _, d := range denied {
		if strings.HasPrefix(d, "mcp__") {
			top = append(top, kv{opencodeMCPName(d), "deny"})
		}
	}
	top = append(top, kv{"external_directory", "deny"}, kv{"webfetch", "deny"}, kv{"websearch", "deny"})
	return top.String()
}

// opencodeEdit: edits anywhere in the worktree except git's own files. With
// checkout = "branch" the run dir is the clone, and its .git/config is
// otherwise editable (found live on 1.19.5): a core.fsmonitor or pager line
// there runs any command at the next allowed `git status`. A linked
// worktree's .git file is denied too (it could point git at a directory the
// agent wrote). Both the relative and the absolute form are matched.
var opencodeEdit = orderedJSON{{"*", "allow"}, {".git", "deny"}, {".git/*", "deny"}, {"*/.git", "deny"}, {"*/.git/*", "deny"}}

// opencodeBashEscapes are denied after every bash allow (the last matching
// rule wins). opencode checks external_directory only for its file tools
// and a few commands (cat yes, head/tail/grep/find/wc/ls no — found live on
// 1.19.5), and an allowed prefix says nothing about the arguments; so these
// refuse, by the command text, what reaches outside the worktree or runs
// something else:
//   - expansion: $VAR, $(…), `…` (the account env, the worker's env) and ~;
//   - absolute paths: a / that starts a word — after a space, =, quote,
//     redirect, bracket, brace, comma, colon, backslash or a short option
//     (-f/x) — and parent traversal (..);
//   - commands that run other commands or read arbitrary files: find
//     -exec/-execdir/-ok/-okdir (and -delete/-fprint/-fls), rg --pre, git
//     diff --no-index;
//   - output redirection (>): files are written with the edit tool, and
//     `cat x > .git/config` would get round opencodeEdit.
//
// Text matching is not a sandbox (a symlink committed in the repo still
// points out): a wrapper profile is what confines the shell.
var opencodeBashEscapes = []string{
	"*$*", "*`*",
	"* ~*", "*=~*", "*:~*",
	"* /*", "*\t/*", "*=/*", "*'/*", `*"/*`, "*</*", "*[/*", "*{/*", "*,/*", "*:/*", `*\/*`, "* -?/*",
	"*../*", "*/..", "*/.. *", "* ..", "* .. *",
	"* -exec*", "* -ok *", "* -okdir*", "* -delete*", "* -fprint*", "* -fls*",
	"* --pre *", "*--pre=*",
	"*--no-index*",
	"*>*",
}

// opencodeMCPName: Claude's mcp__server__tool is opencode's server_tool.
func opencodeMCPName(claudeName string) string {
	n := strings.TrimPrefix(claudeName, "mcp__")
	return strings.ReplaceAll(n, "__", "_")
}

// settingsDeny extracts permissions.deny from the run's settings JSON.
func settingsDeny(settings string) []string {
	var s struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	_ = json.Unmarshal([]byte(settings), &s)
	return s.Permissions.Deny
}

// mcpServerEntry is one server of a Claude-format mcp_config file.
type mcpServerEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// readMCPServers reads a Claude-format mcp_config file ({"mcpServers": {...}}).
func readMCPServers(path string) (map[string]mcpServerEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp_config: %v", err)
	}
	var in struct {
		MCPServers map[string]mcpServerEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("mcp_config %s: %v", path, err)
	}
	return in.MCPServers, nil
}

// opencodeMCPContent translates a Claude-format mcp_config file into
// opencode 1's {"mcp": {...}}.
func opencodeMCPContent(path string) (string, error) {
	servers, err := readMCPServers(path)
	if err != nil {
		return "", err
	}
	out := map[string]any{}
	for name, s := range servers {
		if s.URL != "" {
			m := map[string]any{"type": "remote", "url": s.URL, "enabled": true}
			if len(s.Headers) > 0 {
				m["headers"] = s.Headers
			}
			out[name] = m
			continue
		}
		m := map[string]any{"type": "local", "command": append([]string{s.Command}, s.Args...), "enabled": true}
		if len(s.Env) > 0 {
			m["environment"] = s.Env
		}
		out[name] = m
	}
	b, _ := json.Marshal(map[string]any{"mcp": out})
	return string(b), nil
}

// orderedJSON is a JSON object that keeps key order (opencode permission
// rules are order-sensitive: the last matching rule wins).
type kv struct {
	k string
	v any
}
type orderedJSON []kv

func (o orderedJSON) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.k)
		b.Write(k)
		b.WriteByte(':')
		switch v := e.v.(type) {
		case orderedJSON:
			b.WriteString(v.String())
		default:
			j, _ := json.Marshal(v)
			b.Write(j)
		}
	}
	b.WriteByte('}')
	return b.String()
}
