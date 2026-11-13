package worker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The strict form OpenAI requires: every object closed, every property
// required, the stage's optional ones nullable (enums gain null).
func TestCodexStrictSchema(t *testing.T) {
	got, err := codexStrictSchema(reviewSchema)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(got), &s); err != nil {
		t.Fatal(err)
	}
	if s["additionalProperties"] != false {
		t.Fatalf("top object must be closed: %s", got)
	}
	if req := s["required"].([]any); len(req) != 5 {
		t.Fatalf("every property required: %v", req)
	}
	props := s["properties"].(map[string]any)
	if typ := props["verdict"].(map[string]any)["type"]; typ != "string" {
		t.Fatalf("a required property stays non-null: %v", typ)
	}
	findings := props["findings"].(map[string]any)
	if typ, _ := findings["type"].([]any); len(typ) != 2 || typ[1] != "null" {
		t.Fatalf("an optional property becomes nullable: %v", findings["type"])
	}
	item := findings["items"].(map[string]any)
	if item["additionalProperties"] != false || len(item["required"].([]any)) != 3 {
		t.Fatalf("nested objects are closed too: %v", item)
	}
	file := item["properties"].(map[string]any)["file"].(map[string]any)
	if typ, _ := file["type"].([]any); len(typ) != 2 {
		t.Fatalf("optional nested property nullable: %v", file)
	}
	plan, _ := codexStrictSchema(planSchema)
	if !strings.Contains(plan, `"enum":["s","m","l"]`) {
		t.Fatalf("a required enum keeps its values: %s", plan)
	}
}

// The final message is the schema JSON: nulls the strict form forced in are
// dropped and the result validates against the stage's own schema.
func TestCodexStructured(t *testing.T) {
	obj, err := codexStructured(`{"verdict":"fail","summary":"x","findings":[{"severity":"blocking","text":"t","file":null}],"issues":null}`, reviewSchema)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(obj), "null") {
		t.Fatalf("nulls must be dropped: %s", obj)
	}
	if _, err := codexStructured(`{"verdict":"maybe","summary":"x"}`, reviewSchema); err == nil {
		t.Fatal("an invalid verdict must not validate")
	}
	if obj, err := codexStructured("Here:\n```json\n{\"verdict\":\"pass\",\"summary\":\"ok\"}\n```", reviewSchema); err != nil || !strings.Contains(string(obj), "pass") {
		t.Fatalf("text fallback: %s %v", obj, err)
	}
}

// Event shapes captured from codex-cli 0.163.0: the thread id is the
// session, the last agent message the answer; retry errors are not final,
// turn.failed is (its embedded API status classifies it).
func TestParseCodexEvents(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ok := parseCodexEvents([]byte(strings.Join([]string{
		`{"type":"thread.started","thread_id":"th-1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I'll read it."}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"cat file.txt","aggregated_output":"hello\n","exit_code":0}}`,
		`{"type":"error","message":"Reconnecting... 1/5 (stream disconnected)"}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"{\"verdict\":\"pass\",\"summary\":\"fine\"}"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":22420,"output_tokens":77}}`,
	}, "\n")), now)
	if ok.Outcome != OutcomeOK || ok.SessionID != "th-1" || !strings.Contains(ok.Text, `"verdict"`) {
		t.Fatalf("success: %+v", ok)
	}
	bad := parseCodexEvents([]byte(`{"type":"thread.started","thread_id":"th-2"}
{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"The 'x' model is not supported when using Codex with a ChatGPT account.\"}}"}}`), now)
	if bad.Outcome != OutcomeError || !strings.Contains(bad.Detail, "not supported") {
		t.Fatalf("400: %+v", bad)
	}
	quota := parseCodexEvents([]byte(`{"type":"turn.failed","error":{"message":"{\"type\":\"error\",\"status\":429,\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"You've hit your usage limit.\",\"resets_in_seconds\":5400}}"}}`), now)
	if quota.Outcome != OutcomeQuota || !quota.ResetAt.Equal(now.Add(90*time.Minute)) {
		t.Fatalf("quota: %+v", quota)
	}
	retrying := parseCodexEvents([]byte(`{"type":"thread.started","thread_id":"th-3"}
{"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 Unauthorized: Missing bearer or basic authentication in header)"}`), now)
	if retrying.Outcome != "" || !strings.Contains(retrying.Detail, "401") {
		t.Fatalf("a retry error is not final, but is the detail: %+v", retrying)
	}
}

func TestCodexReset(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	for text, want := range map[string]time.Time{
		"You've hit your usage limit. Try again in 2 hours 5 minutes.": now.Add(2*time.Hour + 5*time.Minute),
		"usage limit reached, try again in 3 days":                     now.Add(72 * time.Hour),
		`{"resets_at":1792454400}`:                                     time.Unix(1792454400, 0),
		"no hint here":                                                 {},
	} {
		if got := codexReset(text, now); !got.Equal(want) {
			t.Errorf("%q: got %v want %v", text, got, want)
		}
	}
}

// What codex refuses up front: an editing stage (read-only adapter), and a
// login directory holding an AGENTS.md codex would load.
func TestCodexRefusals(t *testing.T) {
	if !codexWrites(baseImplTools) || codexWrites(basePlanTools) || codexWrites(reviewTools) {
		t.Fatal("implement writes; plan and review don't")
	}
	home := t.TempDir()
	if p := codexHomeProblem(map[string]string{"CODEX_HOME": home}); p != "" {
		t.Fatalf("a clean login dir is fine: %s", p)
	}
	if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("be nice"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := codexHomeProblem(map[string]string{"CODEX_HOME": home}); !strings.Contains(p, "AGENTS.md") {
		t.Fatalf("an AGENTS.md in the login dir is refused: %q", p)
	}
	cfg := &Config{Accounts: []*Account{{Name: "codex-1", Runner: "codex", Env: map[string]string{"CODEX_HOME": home}}, {Name: "c", Runner: "claude"}}}
	if ps := cfg.AccountProblems(); len(ps) != 1 || !strings.Contains(ps[0], "codex-1") {
		t.Fatalf("worker check reports it: %v", ps)
	}
	r := codexRunner{bin: "/nonexistent/codex"}
	if res := r.Run(context.Background(), AgentRun{AllowedTools: baseImplTools}); res.Outcome != OutcomeError || !strings.Contains(res.Detail, "read-only") {
		t.Fatalf("implement refused: %+v", res)
	}
	if res := r.Run(context.Background(), AgentRun{AllowedTools: reviewTools, Env: map[string]string{"CODEX_HOME": home}}); res.Outcome != OutcomeError || !strings.Contains(res.Detail, "AGENTS.md") {
		t.Fatalf("home instructions refused at run time: %+v", res)
	}
}

// The invocation: lockdown flags, the prompt on stdin (never argv), the
// strict schema file, the worktree as --cd; a resume names the session and
// drops --cd (the process cwd is the worktree).
func TestCodexInvocation(t *testing.T) {
	logDir := t.TempDir()
	script := fakeCLI(t, `printf '%s\n' "$@" > `+logDir+`/args; cat > `+logDir+`/stdin; pwd > `+logDir+`/pwd
for a in "$@"; do case "$prev" in --output-schema) cp "$a" `+logDir+`/schema;; esac; prev="$a"; done
echo '{"type":"thread.started","thread_id":"th-9"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"{\"verdict\":\"pass\",\"summary\":\"ok\",\"findings\":null,\"issues\":null}"}}'
echo '{"type":"turn.completed","usage":{}}'`)
	wt := t.TempDir()
	home := t.TempDir()
	r := codexRunner{bin: script}
	res := r.Run(context.Background(), AgentRun{Dir: wt, Prompt: "review this, secret-in-prompt", Schema: reviewSchema, Model: "gpt-x", Effort: "xhigh",
		AllowedTools: reviewTools, Env: map[string]string{"CODEX_HOME": home}, Timeout: time.Minute})
	if res.Outcome != OutcomeOK || res.SessionID != "th-9" || !strings.Contains(string(res.Structured), `"pass"`) {
		t.Fatalf("run: %+v", res)
	}
	args, _ := os.ReadFile(filepath.Join(logDir, "args"))
	a := string(args)
	for _, want := range []string{"exec\n", "--ignore-user-config\n", "--ignore-rules\n", "--strict-config\n", "--json\n",
		"project_doc_max_bytes=0\n", "skills.include_instructions=false\n", `approval_policy="never"` + "\n",
		`default_permissions="skopos"` + "\n", "permissions.skopos.network.enabled=false\n", "--disable\nhooks\n",
		"--cd\n" + wt + "\n", "-m\ngpt-x\n", "-c\nmodel_reasoning_effort=\"xhigh\"\n", "--output-schema\n"} {
		if !strings.Contains(a, want) {
			t.Errorf("args lack %q:\n%s", want, a)
		}
	}
	if !strings.HasSuffix(a, "\n-\n") || strings.Contains(a, "secret-in-prompt") {
		t.Errorf("the prompt goes on stdin, never argv:\n%s", a)
	}
	for _, never := range []string{"dangerously", "workspace-write", "danger-full-access", "--ephemeral"} {
		if strings.Contains(a, never) {
			t.Errorf("never %s:\n%s", never, a)
		}
	}
	if !strings.Contains(a, `permissions.skopos.filesystem={":minimal"="read", ":tmpdir"="none", ":slash_tmp"="none", ":workspace_roots"="read"`) || strings.Contains(a, `"write"`) {
		t.Errorf("read-only confined profile:\n%s", a)
	}
	// The account env (its credentials) never reaches the agent's shell.
	if !strings.Contains(a, `shell_environment_policy.exclude=["*TOKEN*", "*SECRET*", "*PASSWORD*", "*API_KEY*", "*ACCESS_KEY*", "CODEX_HOME"]`+"\n") {
		t.Errorf("shell env exclude:\n%s", a)
	}
	if got := codexShellExclude(map[string]string{"CODEX_ACCESS_TOKEN": "x", "PATH": "/p", "HOME": "/h", "MY_VAR": "y"}); got != `["*TOKEN*", "*SECRET*", "*PASSWORD*", "*API_KEY*", "*ACCESS_KEY*", "CODEX_ACCESS_TOKEN", "MY_VAR"]` {
		t.Errorf("account keys except PATH/HOME: %s", got)
	}
	if in, _ := os.ReadFile(filepath.Join(logDir, "stdin")); string(in) != "review this, secret-in-prompt" {
		t.Errorf("stdin: %q", in)
	}
	if sch, _ := os.ReadFile(filepath.Join(logDir, "schema")); !strings.Contains(string(sch), `"additionalProperties":false`) {
		t.Errorf("the schema file is the strict form: %s", sch)
	}

	res = r.Run(context.Background(), AgentRun{Dir: wt, Prompt: "again", SessionID: "th-9", Resume: true,
		AllowedTools: reviewTools, Env: map[string]string{"CODEX_HOME": home}, Timeout: time.Minute})
	if res.Outcome != OutcomeOK {
		t.Fatalf("resume: %+v", res)
	}
	args, _ = os.ReadFile(filepath.Join(logDir, "args"))
	a = string(args)
	if !strings.HasPrefix(a, "exec\nresume\n") || !strings.HasSuffix(a, "\nth-9\n-\n") || strings.Contains(a, "--cd") {
		t.Errorf("resume argv:\n%s", a)
	}
	if pwd, _ := os.ReadFile(filepath.Join(logDir, "pwd")); !strings.Contains(string(pwd), filepath.Base(wt)) {
		t.Errorf("resume runs in the worktree: %s", pwd)
	}
}

// A run that dies without finishing the turn reports why (the last error).
func TestCodexUnfinished(t *testing.T) {
	script := fakeCLI(t, `cat >/dev/null; echo '{"type":"thread.started","thread_id":"t"}'; echo '{"type":"error","message":"Reconnecting... 5/5 (unexpected status 401 Unauthorized)"}' ; exit 1`)
	r := codexRunner{bin: script}
	res := r.Run(context.Background(), AgentRun{Dir: t.TempDir(), Prompt: "x", AllowedTools: reviewTools,
		Env: map[string]string{"CODEX_HOME": t.TempDir()}, Timeout: time.Minute})
	if res.Outcome != OutcomeAuth {
		t.Fatalf("a 401 that never recovered is an auth problem: %+v", res)
	}
}
