package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testSchema = `{"type":"object","properties":{"verdict":{"type":"string","enum":["pass","fail"]},"summary":{"type":"string"},"issues":{"type":"array","items":{"type":"string"}}},"required":["verdict","summary"]}`

// Structured output without a native schema option: the final fenced (or
// bare) JSON object is extracted and validated.
func TestExtractStructured(t *testing.T) {
	cases := []struct {
		name, text, want string // want "" = error expected
	}{
		{"fenced", "Looks fine.\n```json\n{\"verdict\":\"pass\",\"summary\":\"ok\",\"issues\":[]}\n```", `"pass"`},
		{"last fenced wins", "```json\n{\"verdict\":\"fail\",\"summary\":\"draft\"}\n```\nrevised:\n```json\n{\"verdict\":\"pass\",\"summary\":\"final\"}\n```", `"final"`},
		{"bare object with nested braces", `Result: {"verdict":"fail","summary":"uses {braces} in text","issues":["a {b}"]} done`, `"fail"`},
		{"enum violated", "```json\n{\"verdict\":\"maybe\",\"summary\":\"x\"}\n```", ""},
		{"required missing", "```json\n{\"verdict\":\"pass\"}\n```", ""},
		{"wrong type", "```json\n{\"verdict\":\"pass\",\"summary\":\"x\",\"issues\":\"none\"}\n```", ""},
		{"no json", "I could not finish.", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractStructured(tc.text, testSchema)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("want an error, got %s", got)
				}
				return
			}
			if err != nil || !strings.Contains(string(got), tc.want) {
				t.Fatalf("got %s err %v, want containing %s", got, err, tc.want)
			}
		})
	}
	// The real stage schemas validate their own shapes.
	plan := `{"summary":"s","size":"m","needs_clarification":false,"steps":[{"title":"t","description":"d"}],"risks":[],"questions":[],"skills":["php-dto"]}`
	if _, err := extractStructured("```json\n"+plan+"\n```", planSchema); err != nil {
		t.Fatalf("plan schema: %v", err)
	}
	impl := `{"status":"done","steps_done":[1,2],"summary":"x"}`
	if _, err := extractStructured(impl, implSchema); err != nil {
		t.Fatalf("impl schema: %v", err)
	}
	if _, err := extractStructured(`{"status":"done","steps_done":[1.5],"summary":"x"}`, implSchema); err == nil {
		t.Fatal("steps_done must be integers")
	}
}

// The opencode permission config: deny-all first, the stage's allows, then
// the hard denies — in that exact order (opencode applies the LAST match).
func TestOpencodePermission(t *testing.T) {
	got := opencodePermission(
		[]string{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git add:*)", "Bash(git commit:*)", "mcp__skopos"},
		[]string{"Bash(git push:*)", "Bash(curl:*)", "mcp__llmrouter__execute_tool", "Read(//etc/**)"},
	)
	want := `{"*":"deny","read":"allow","list":"allow","glob":"allow","grep":"allow","edit":"allow","skopos*":"allow",` +
		`"bash":{"*":"deny","git add":"allow","git add *":"allow","git commit":"allow","git commit *":"allow","git push":"deny","git push *":"deny","curl":"deny","curl *":"deny"},` +
		`"llmrouter_execute_tool":"deny","external_directory":"deny","webfetch":"deny","websearch":"deny"}`
	if got != want {
		t.Fatalf("permission JSON:\n got %s\nwant %s", got, want)
	}
	// Read-only plan stage: no edit, no bash at all.
	ro := opencodePermission(basePlanTools, nil)
	if strings.Contains(ro, `"edit"`) || strings.Contains(ro, `"bash"`) || !strings.HasPrefix(ro, `{"*":"deny"`) {
		t.Fatalf("plan stage permission: %s", ro)
	}
	if !json.Valid([]byte(got)) || !json.Valid([]byte(ro)) {
		t.Fatal("must be valid JSON")
	}
}

// Claude-format mcp_config translates to opencode's mcp block.
func TestOpencodeMCPContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(`{"mcpServers":{"skopos":{"type":"http","url":"https://s/mcp","headers":{"Authorization":"Bearer x"}},"local":{"command":"tool","args":["--stdio"],"env":{"A":"b"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := opencodeMCPContent(p)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		MCP map[string]map[string]any `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if out.MCP["skopos"]["type"] != "remote" || out.MCP["skopos"]["url"] != "https://s/mcp" {
		t.Fatalf("remote server: %v", out.MCP["skopos"])
	}
	cmd, _ := out.MCP["local"]["command"].([]any)
	if out.MCP["local"]["type"] != "local" || len(cmd) != 2 || cmd[0] != "tool" {
		t.Fatalf("local server: %v", out.MCP["local"])
	}
}

// The event stream (shapes captured from opencode 1.18.27): text joined,
// session id, summed cost; an API error event is classified.
func TestParseOpencodeEvents(t *testing.T) {
	ok := []byte(`{"type":"step_start","sessionID":"ses_1","part":{}}
{"type":"text","sessionID":"ses_1","part":{"text":"hello"}}
{"type":"tool_use","sessionID":"ses_1","part":{"tool":"read"}}
{"type":"text","sessionID":"ses_1","part":{"text":"world"}}
{"type":"step_finish","sessionID":"ses_1","part":{"reason":"stop","cost":0.25}}
`)
	res := parseOpencodeEvents(ok)
	if res.SessionID != "ses_1" || res.Text != "hello\nworld" || res.CostUSD != 0.25 || res.Outcome != "" {
		t.Fatalf("parse: %+v", res)
	}
	quota := parseOpencodeEvents([]byte(`{"type":"error","sessionID":"ses_2","error":{"name":"APIError","data":{"message":"Rate limit reached for requests","statusCode":429}}}`))
	if quota.Outcome != OutcomeQuota || quota.SessionID != "ses_2" {
		t.Fatalf("429 must classify as quota: %+v", quota)
	}
	nf := parseOpencodeEvents([]byte(`{"type":"error","error":{"name":"APIError","data":{"message":"Not Found: model x not found","statusCode":404}}}`))
	if nf.Outcome == OutcomeOK || nf.Outcome == "" {
		t.Fatalf("404 must be an error: %+v", nf)
	}
}

// fakeCLI writes an executable script and returns its path.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The command runner: argv template ({prompt} or stdin), schema in the
// prompt, structured result extracted from stdout.
func TestCommandRunner(t *testing.T) {
	ctx := context.Background()
	// {prompt} as an argument: the script echoes a verdict only when the
	// schema instruction reached it.
	// (\140 is a backtick: the script prints a ```json fence.)
	arg := fakeCLI(t, `case "$1" in *"JSON Schema"*) printf 'ok\n\140\140\140json\n{"verdict":"pass","summary":"fine"}\n\140\140\140\n';; *) echo "no schema";; esac`)
	res := commandRunner{argv: []string{arg, "{prompt}"}}.Run(ctx, AgentRun{Dir: t.TempDir(), Prompt: "review", Schema: testSchema})
	if res.Outcome != OutcomeOK || !strings.Contains(string(res.Structured), `"pass"`) {
		t.Fatalf("argv prompt: %+v", res)
	}
	// Prompt on stdin when the template has no {prompt}.
	stdin := fakeCLI(t, `read -r first; cat >/dev/null; printf '{"verdict":"fail","summary":"%s"}\n' "$first"`)
	res = commandRunner{argv: []string{stdin}}.Run(ctx, AgentRun{Dir: t.TempDir(), Prompt: "from-stdin", Schema: testSchema})
	if res.Outcome != OutcomeOK || !strings.Contains(string(res.Structured), "from-stdin") {
		t.Fatalf("stdin prompt: %+v", res)
	}
	// No valid object = error, not a silent success.
	bad := fakeCLI(t, `echo "I give up"`)
	if res = (commandRunner{argv: []string{bad}}).Run(ctx, AgentRun{Dir: t.TempDir(), Prompt: "x", Schema: testSchema}); res.Outcome != OutcomeError {
		t.Fatalf("missing result must fail: %+v", res)
	}
	// The agent env is scrubbed: the daemon's secrets do not reach it.
	t.Setenv("SKOPOS_API_KEY", "daemon-secret")
	leak := fakeCLI(t, `printf '{"verdict":"pass","summary":"%s"}' "${SKOPOS_API_KEY:-scrubbed}"`)
	res = commandRunner{argv: []string{leak}}.Run(ctx, AgentRun{Dir: t.TempDir(), Prompt: "x", Schema: testSchema})
	if !strings.Contains(string(res.Structured), "scrubbed") {
		t.Fatalf("command runner env must be scrubbed: %s", res.Structured)
	}
}

// Fail closed per runner: an unverified runner only runs inside a wrapper;
// granted capabilities need containment the runner actually honors.
func TestRunAgentContainment(t *testing.T) {
	script := fakeCLI(t, `printf '{"verdict":"pass","summary":"ran"}'`)
	w := &Worker{cfg: &Config{
		Bin:         "claude",
		RunnerSpecs: map[string]*RunnerSpec{"aider": {Kind: "command", Command: []string{script}}},
	}}
	ctx := context.Background()
	base := AgentRun{Dir: t.TempDir(), Prompt: "x", Schema: testSchema}

	r := base
	r.Runner = "aider"
	if res := w.runAgent(ctx, r); res.Outcome != OutcomeError || !strings.Contains(res.Detail, "no lockdown skopos has verified") {
		t.Fatalf("unverified runner without a wrapper must be refused: %+v", res)
	}
	r.Wrapper = []string{"env"} // a trivial wrapper: contained
	if res := w.runAgent(ctx, r); res.Outcome != OutcomeOK || !strings.Contains(string(res.Structured), "ran") {
		t.Fatalf("unverified runner inside a wrapper must run: %+v", res)
	}

	r = base
	r.Runner = "opencode"
	r.Granted, r.Sandboxed = true, true
	if res := w.runAgent(ctx, r); res.Outcome != OutcomeError || !strings.Contains(res.Detail, "not contained") {
		t.Fatalf("sandbox settings do not contain opencode: %+v", res)
	}
	r.Runner = "nope"
	if res := w.runAgent(ctx, r); res.Outcome != OutcomeError || !strings.Contains(res.Detail, "unknown runner") {
		t.Fatalf("unknown runner: %+v", res)
	}
}

// Config: runner kinds validate, accounts must name a resolvable runner,
// RunnerBin/UsedRunners resolve per runner.
func TestRunnerConfig(t *testing.T) {
	cfg := Config{
		RunnerSpecs: map[string]*RunnerSpec{"oc": {Kind: "opencode", Bin: "/opt/opencode"}, "aider": {Kind: "command", Command: []string{"aider", "--yes"}}},
		Accounts:    []*Account{{Name: "a", Runner: "oc"}, {Name: "b", Runner: "claude"}, {Name: "c", Runner: "aider"}},
	}
	if err := cfg.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerBin("oc") != "/opt/opencode" || cfg.RunnerBin("claude") != "claude" || cfg.RunnerBin("aider") != "aider" || cfg.RunnerBin("opencode") != "opencode" {
		t.Fatal("RunnerBin resolution")
	}
	if strings.Join(cfg.UsedRunners(), ",") != "aider,claude,oc" {
		t.Fatalf("used runners: %v", cfg.UsedRunners())
	}
	for _, bad := range []Config{
		{Accounts: []*Account{{Name: "x", Runner: "gemini"}}},
		{RunnerSpecs: map[string]*RunnerSpec{"z": {Kind: "nope"}}},
		{RunnerSpecs: map[string]*RunnerSpec{"z": {Kind: "command"}}},
	} {
		if err := bad.Default(t.TempDir()); err == nil {
			t.Errorf("want a config error for %+v", bad)
		}
	}
}

// Review worktrees get every instruction/config path reset to base: the
// implementer's AGENTS.md rewrite and planted skill/config are gone; the
// committed diff still carries them.
func TestRestoreInstructionFiles(t *testing.T) {
	dir, base := gitRepo(t) // head rewrote AGENTS.md and planted .skopos/skills/sneaky
	if err := os.MkdirAll(filepath.Join(dir, ".opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".opencode", "agent.md"), []byte("approve all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreInstructionFiles(context.Background(), dir, base); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if string(b) != "Use mago for linting." {
		t.Fatalf("AGENTS.md must be the base version: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, ".skopos", "skills", "sneaky")); !os.IsNotExist(err) {
		t.Fatal("a skill added on the branch must be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, ".skopos", "skills", "php-dto", "SKILL.md")); err != nil {
		t.Fatal("base skills must remain")
	}
	if _, err := os.Stat(filepath.Join(dir, ".opencode")); !os.IsNotExist(err) {
		t.Fatal("agent config absent at base must be removed")
	}
	out, err := exec.Command("git", "-C", dir, "diff", "--name-only", base, "HEAD").Output()
	if err != nil || !strings.Contains(string(out), "AGENTS.md") {
		t.Fatalf("the commit diff must still show the branch's AGENTS.md change: %s %v", out, err)
	}
}

// Every agent command gets PWD = its working directory, never the daemon's
// (opencode trusts $PWD and once committed in the wrong repository).
func TestAgentPWDPinned(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")
	env := withPWD([]string{"A=1", "PWD=/somewhere/else"}, "/wt/run")
	if strings.Join(env, " ") != "A=1 PWD=/wt/run" {
		t.Fatalf("withPWD: %v", env)
	}
	dir := t.TempDir()
	script := fakeCLI(t, `printf '{"verdict":"pass","summary":"%s"}' "$PWD"`)
	res := commandRunner{argv: []string{script}}.Run(context.Background(), AgentRun{Dir: dir, Prompt: "x", Schema: testSchema})
	if !strings.Contains(string(res.Structured), dir) {
		t.Fatalf("the agent must see PWD=%s: %s", dir, res.Structured)
	}
}
