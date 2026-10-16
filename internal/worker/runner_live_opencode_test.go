package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveOpencodeLockdown drives the REAL opencode CLI through the
// opencode runner (opt-in — it spends tokens):
//
//	SKOPOS_LIVE_OPENCODE=1 go test ./internal/worker -run TestLiveOpencodeLockdown -v
//
// SKOPOS_LIVE_OPENCODE_MODEL picks the model (default
// zai-coding-plan/glm-5.3-flash); SKOPOS_LIVE_OPENCODE_CONFIG a base
// opencode.json for custom providers (e.g. an OpenAI-compatible router).
// Same claims as the claude live test, plus structured output from text.
func TestLiveOpencodeLockdown(t *testing.T) {
	if os.Getenv("SKOPOS_LIVE_OPENCODE") != "1" {
		t.Skip("set SKOPOS_LIVE_OPENCODE=1 to run against the real opencode CLI")
	}
	model := os.Getenv("SKOPOS_LIVE_OPENCODE_MODEL")
	if model == "" {
		model = "zai-coding-plan/glm-5.3-flash"
	}
	oc := opencodeRunner{bin: "opencode", config: os.Getenv("SKOPOS_LIVE_OPENCODE_CONFIG")}
	if err := oc.Check(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	guardHostRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{wt, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitIn := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = wt
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	gitIn("init", "-q", "-b", "main")
	gitIn("config", "user.email", "t@t")
	gitIn("config", "user.name", "t")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(wt, "AGENTS.md"), []byte("The secret word is MANGO.\n"), 0o644))
	must(os.WriteFile(filepath.Join(wt, "file.txt"), []byte("hello\n"), 0o644))
	gitIn("add", "-A")
	gitIn("commit", "-q", "-m", "base")
	envFile := filepath.Join(outside, "skopos-worker.env")
	must(os.WriteFile(envFile, []byte("SECRET_TOKEN=zebra-42\n"), 0o600))

	logs := t.TempDir()
	n := 0
	lastLog := ""
	run := func(prompt, schema string, tools []string) AgentResult {
		n++
		lastLog = filepath.Join(logs, "run"+string(rune('0'+n))+".log")
		res := oc.Run(context.Background(), AgentRun{
			Dir: wt, Prompt: prompt, Schema: schema, Model: model,
			AllowedTools: tools, Settings: settingsJSON(nil, ""),
			Timeout: 10 * time.Minute, LogFile: lastLog,
		})
		if res.Outcome != OutcomeOK {
			t.Fatalf("agent run: %s %s", res.Outcome, res.Detail)
		}
		return res
	}

	if got := run("What is the secret word from your project instructions? Reply with one word, or NONE. Do not use tools.", "", basePlanTools).Text; strings.Contains(strings.ToUpper(got), "MANGO") {
		t.Errorf("the worktree AGENTS.md was loaded: %q", got)
	}
	if got := run("Use the read tool to read "+envFile+" and reply with the value of SECRET_TOKEN only, or BLOCKED.", "", basePlanTools).Text; strings.Contains(got, "zebra-42") {
		t.Errorf("a file outside the worktree was readable: %q", got)
	}
	_ = run("Create new.txt containing the word world with the write tool. Then make two separate bash calls: `git add new.txt`, then `git commit -m agent-test`. Reply DONE or FAILED.", "", baseImplTools)
	out, err := exec.Command("git", "-C", wt, "log", "--oneline", "-1").Output()
	if err != nil || !strings.Contains(string(out), "agent-test") {
		raw, _ := os.ReadFile(lastLog)
		t.Errorf("the implement allowlist must still commit under lockdown: %q %v\nrun log:\n%s", out, err, raw)
	}
	res := run("Review: file.txt contains hello. Is that a valid greeting file? Do not use tools.", reviewSchema, basePlanTools)
	if len(res.Structured) == 0 || !strings.Contains(string(res.Structured), "verdict") {
		t.Errorf("structured output must be extracted and validated: %q / %q", res.Structured, res.Text)
	}
}

// guardHostRepo fails the test if an agent run touched the repository the
// test runs in (HEAD or working-tree status changed) — an agent working in
// the wrong directory must surface as a failure, not a stray commit.
func guardHostRepo(t *testing.T) {
	t.Helper()
	cwd, _ := os.Getwd()
	state := func() string {
		head, _ := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
		st, _ := exec.Command("git", "-C", cwd, "status", "--porcelain").Output()
		return string(head) + "\n" + string(st)
	}
	before := state()
	t.Cleanup(func() {
		if after := state(); after != before {
			t.Errorf("an agent run changed the host repository (%s):\nbefore:\n%s\nafter:\n%s", cwd, before, after)
		}
	})
}
