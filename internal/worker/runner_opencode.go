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
	"strings"
)

// opencodeRunner is opencode (`opencode run --format json`). Lockdown,
// verified live against opencode 1.18.27 (TestLiveOpencodeLockdown):
//   - an EMPTY config dir (OPENCODE_CONFIG_DIR) plus
//     OPENCODE_DISABLE_PROJECT_CONFIG / _CLAUDE_CODE / _EXTERNAL_SKILLS: the
//     worker user's ~/.config/opencode (AGENTS.md, agents, MCP, plugins) and
//     the repo's AGENTS.md / opencode.json / .claude are not loaded;
//   - OPENCODE_PERMISSION: deny-all first, then the stage's allows, then
//     the hard denies — opencode applies the LAST matching rule, and tools
//     with no allow are removed from the model's tool set;
//     external_directory is denied, so files outside the worktree are
//     unreadable by the read tool and by bash;
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

func (o opencodeRunner) Check(ctx context.Context, pathEnv string) error {
	cmd := exec.CommandContext(ctx, o.bin, "run", "--help")
	if pathEnv != "" {
		cmd.Env = append(os.Environ(), "PATH="+pathEnv)
		if p, err := lookPathIn(o.bin, pathEnv); err == nil {
			cmd.Path = p
		}
	}
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return fmt.Errorf("running %s run --help: %v", o.bin, err)
	}
	for _, f := range []string{"--format", "--session", "--model"} {
		if !strings.Contains(string(out), f) {
			return fmt.Errorf("%s run does not support %s — upgrade opencode", o.bin, f)
		}
	}
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
	cfgDir, err := os.MkdirTemp("", "skopos-oc-cfg-")
	if err != nil {
		return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
	}
	defer func() { _ = os.RemoveAll(cfgDir) }()

	// --dir AND $PWD: opencode resolves its project from $PWD, not the
	// process cwd (found live: with the daemon's PWD it worked in the wrong
	// repository).
	args := []string{"run", "--format", "json", "--dir", r.Dir}
	if r.Model != "" {
		args = append(args, "-m", r.Model)
	}
	if resume && session != "" {
		args = append(args, "-s", session)
	}
	args = append(args, prompt)

	env := withPWD(scrubEnv(r.Env, r.TokenEnvs, r.FileEnvKeys), r.Dir)
	env = append(env,
		"OPENCODE_CONFIG_DIR="+cfgDir,
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_CLAUDE_CODE=1",
		"OPENCODE_DISABLE_EXTERNAL_SKILLS=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_SHARE=1",
		"OPENCODE_PERMISSION="+opencodePermission(r.AllowedTools, settingsDeny(r.Settings)),
	)
	if o.config != "" {
		env = append(env, "OPENCODE_CONFIG="+o.config)
	}
	if r.MCPConfig != "" {
		content, err := opencodeMCPContent(r.MCPConfig)
		if err != nil {
			return AgentResult{Outcome: OutcomeError, Detail: err.Error()}
		}
		env = append(env, "OPENCODE_CONFIG_CONTENT="+content)
	}

	cctx, cancel := context.WithTimeout(ctx, orTimeout(r.Timeout))
	defer cancel()
	sub := r
	sub.Bin = o.bin
	name, argv := agentCommand(sub, args)
	cmd := exec.CommandContext(cctx, name, argv...)
	cmd.Dir = r.Dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
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
				Text string  `json:"text"`
				Cost float64 `json:"cost"`
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
	add := func(k, v string) {
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
			add("edit", "allow")
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
				prefix := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(p, ")"), ":*"), "*")
				b = append(b, kv{prefix, "deny"}, kv{prefix + " *", "deny"})
			}
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

// opencodeMCPContent translates a Claude-format mcp_config file
// ({"mcpServers": {...}}) into opencode's {"mcp": {...}}.
func opencodeMCPContent(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("mcp_config: %v", err)
	}
	var in struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("mcp_config %s: %v", path, err)
	}
	out := map[string]any{}
	for name, s := range in.MCPServers {
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
