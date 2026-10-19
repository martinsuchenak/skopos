package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Output shapes captured from agy 1.1.25 (--output-format json).
func TestParseAntigravity(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r := AgentRun{Schema: `{"type":"object","properties":{"verdict":{"type":"string","enum":["pass","fail"]},"summary":{"type":"string"}},"required":["verdict","summary"]}`}
	ok := parseAntigravity([]byte(`{"conversation_id":"c-1","status":"SUCCESS","response":"**Verdict:** pass\n{\"summary\":\"x\",\"verdict\":\"pass\"}\n","num_turns":2,"structured_output":{"summary":"fine","verdict":"pass"},"usage":{"input_tokens":30903}}`), "", nil, nil, r, now)
	if ok.Outcome != OutcomeOK || ok.SessionID != "c-1" || !strings.Contains(string(ok.Structured), `"fine"`) {
		t.Fatalf("structured_output: %+v", ok)
	}
	denied := parseAntigravity([]byte(`{"conversation_id":"c-2","status":"SUCCESS","response":"","denied_actions":[{"action":"command","display_name":"RunCommand"}]}`),
		`jetski: no output produced — a tool required the "command" permission that headless mode cannot prompt for`, nil, nil, r, now)
	if denied.Outcome != OutcomeError || !strings.Contains(denied.Detail, "RunCommand (command)") || denied.SessionID != "c-2" {
		t.Fatalf("a denied tool ends the run, and says which: %+v", denied)
	}
	failed := parseAntigravity([]byte(`{"conversation_id":"","status":"ERROR","response":"","error":"Error: empty prompt."}`), "", errors.New("exit 1"), nil, r, now)
	if failed.Outcome != OutcomeError || !strings.Contains(failed.Detail, "empty prompt") {
		t.Fatalf("status ERROR: %+v", failed)
	}
	quota := parseAntigravity([]byte("not json"), "You have reached your usage limit. It resets at 5pm.", errors.New("exit 1"), nil, r, now)
	if quota.Outcome != OutcomeQuota || quota.ResetAt.IsZero() {
		t.Fatalf("quota: %+v", quota)
	}
	text := parseAntigravity([]byte(`{"conversation_id":"c-3","status":"SUCCESS","response":"Here it is:\n`+"```json\\n{\\\"verdict\\\":\\\"fail\\\",\\\"summary\\\":\\\"bad\\\"}\\n```"+`"}`), "", nil, nil, r, now)
	if text.Outcome != OutcomeOK || !strings.Contains(string(text.Structured), "fail") {
		t.Fatalf("no structured_output falls back to the reply text: %+v", text)
	}
	plain := parseAntigravity([]byte(`{"conversation_id":"c-4","status":"SUCCESS","response":"Hi.\n"}`), "", nil, nil, AgentRun{}, now)
	if plain.Outcome != OutcomeOK || plain.Text != "Hi.\n" {
		t.Fatalf("no schema: %+v", plain)
	}
}

// The stage allowlist as agy rules: reads need none, Bash patterns become
// command(<prefix>), edits write_file(<worktree>), denies stay denied.
func TestAntigravityPermissions(t *testing.T) {
	allow, deny := antigravityPermissions("/w/t", reviewTools, []string{"Bash(git push:*)", "mcp__x__y"})
	if strings.Join(allow, ",") != "command(git diff),command(git log),command(git show)" {
		t.Fatalf("review allow: %v", allow)
	}
	if strings.Join(deny, ",") != "command(git push)" {
		t.Fatalf("deny: %v", deny)
	}
	if allow, _ := antigravityPermissions("/w/t", basePlanTools, nil); len(allow) != 0 {
		t.Fatalf("plan reads only: %v", allow)
	}
	allow, _ = antigravityPermissions("/w/t", baseImplTools, nil)
	if allow[0] != "write_file(/w/t)" || !strings.Contains(strings.Join(allow, ","), "command(git commit)") {
		t.Fatalf("implement: %v", allow)
	}
}

// Settings are rewritten per run, keeping what agy stored itself.
func TestWriteAntigravitySettings(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"model":"Gemini 3.1 Pro (High)","permissions":{"allow":["command(*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAntigravitySettings(home, []string{"command(git diff)"}, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var doc struct {
		Model       string `json:"model"`
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Model != "Gemini 3.1 Pro (High)" || len(doc.Permissions.Allow) != 1 || doc.Permissions.Allow[0] != "command(git diff)" || doc.Permissions.Deny == nil {
		t.Fatalf("settings: %s", raw)
	}
}

func TestAntigravityHomeProblem(t *testing.T) {
	if p := antigravityHomeProblem(nil); !strings.Contains(p, "login directory") {
		t.Fatalf("no HOME: the worker user's ~/.gemini is never written: %q", p)
	}
	home := t.TempDir()
	if p := antigravityHomeProblem(map[string]string{"HOME": home}); p != "" {
		t.Fatalf("clean: %q", p)
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "GEMINI.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := antigravityHomeProblem(map[string]string{"HOME": home}); !strings.Contains(p, "GEMINI.md") {
		t.Fatalf("user instructions refused: %q", p)
	}
	cfg := &Config{
		RunnerSpecs: map[string]*RunnerSpec{"agy": {Kind: "antigravity"}},
		Accounts:    []*Account{{Name: "agy-1", Runner: "agy"}},
	}
	if ps := cfg.AccountProblems(); len(ps) != 1 || !strings.Contains(ps[0], "agy-1") {
		t.Fatalf("worker check reports it: %v", ps)
	}
	if cfg.RunnerBin("agy") != "agy" {
		t.Fatalf("bin: %s", cfg.RunnerBin("agy"))
	}
}

// The invocation: HOME is the login directory, the run's rules are in its
// settings, sandbox + no slash commands + the stage timeout + the schema
// file; a resume names the conversation; never skip-permissions.
func TestAntigravityInvocation(t *testing.T) {
	logDir := t.TempDir()
	script := fakeCLI(t, `printf '%s\n' "$@" > `+logDir+`/args; echo "$HOME" > `+logDir+`/home
printf '{"conversation_id":"c-9","status":"SUCCESS","response":"ok","structured_output":{"verdict":"pass","summary":"ok"}}'`)
	home := t.TempDir()
	wt := t.TempDir()
	r := antigravityRunner{bin: script}
	res := r.Run(context.Background(), AgentRun{Dir: wt, Prompt: "-review this", Schema: reviewSchema, Model: "gemini-3.8-flash-high",
		AllowedTools: reviewTools, Env: map[string]string{"HOME": home}, Timeout: 20 * time.Minute})
	if res.Outcome != OutcomeOK || res.SessionID != "c-9" {
		t.Fatalf("run: %+v", res)
	}
	args, _ := os.ReadFile(filepath.Join(logDir, "args"))
	a := string(args)
	for _, want := range []string{"--output-format\njson\n", "--sandbox\n", "--disable-slash-commands\n", "--print-timeout\n20m0s\n",
		"--model\ngemini-3.8-flash-high\n", "--json-schema\n", "--print=-review this\n"} {
		if !strings.Contains(a, want) {
			t.Errorf("args lack %q:\n%s", want, a)
		}
	}
	if strings.Contains(a, "dangerously") || strings.Contains(a, "--mode\n") {
		t.Errorf("never skip permissions (and plan mode is a no-op with slash commands off):\n%s", a)
	}
	if h, _ := os.ReadFile(filepath.Join(logDir, "home")); strings.TrimSpace(string(h)) != home {
		t.Errorf("HOME must be the login directory: %s", h)
	}
	if raw, _ := os.ReadFile(filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")); !strings.Contains(string(raw), "command(git diff)") {
		t.Errorf("the review allowlist must be in the login directory's settings: %s", raw)
	}
	_ = r.Run(context.Background(), AgentRun{Dir: wt, Prompt: "again", SessionID: "c-9", Resume: true,
		AllowedTools: reviewTools, Env: map[string]string{"HOME": home}, Timeout: time.Minute})
	args, _ = os.ReadFile(filepath.Join(logDir, "args"))
	if !strings.Contains(string(args), "--conversation\nc-9\n") {
		t.Errorf("resume names the conversation:\n%s", args)
	}
}

// Unverified: without a wrapper the worker refuses the run.
func TestAntigravityWrapperOnly(t *testing.T) {
	w := &Worker{cfg: &Config{RunnerSpecs: map[string]*RunnerSpec{"agy": {Kind: "antigravity", Bin: "/nonexistent/agy"}}}}
	res := w.runAgent(context.Background(), AgentRun{Runner: "agy", AllowedTools: reviewTools})
	if res.Outcome != OutcomeError || !strings.Contains(res.Detail, "wrapper") {
		t.Fatalf("an unwrapped antigravity run must be refused: %+v", res)
	}
}
