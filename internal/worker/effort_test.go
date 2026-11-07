package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An account's effort is checked against what its runner's CLI takes, so a
// setting is never silently dropped.
func TestCheckEffort(t *testing.T) {
	cfg := &Config{RunnerSpecs: map[string]*RunnerSpec{
		"claude-opus": {Kind: "claude"},
		"oc":          {Kind: "opencode"},
		"gpt":         {Kind: "codex"},
		"aider":       {Kind: "command", Command: []string{"aider", "--message", "{prompt}"}},
		"tool":        {Kind: "command", Command: []string{"tool", "--think={effort}", "{prompt}"}},
	}}
	for _, c := range []struct {
		runner, effort, err string
	}{
		{"", "", ""},
		{"", "high", ""},           // plain claude
		{"claude-opus", "max", ""}, // renamed: by kind
		{"claude-opus", "minimal", "takes low, medium, high, xhigh, max"},
		{"gpt", "minimal", ""},
		{"gpt", "max", "takes minimal, low"},
		{"oc", "high", ""},
		{"oc", "a b", "plain word"},
		{"aider", "high", "no {effort} placeholder"},
		{"tool", "deep", ""},
		{"tool", "", "set effort on the account"}, // {effort} would expand empty
	} {
		err := cfg.checkEffort(&Account{Name: "a", Runner: c.runner, Effort: c.effort})
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s/%s: got %v, want %q", c.runner, c.effort, err, c.err)
		}
	}
}

// Default refuses a bad effort at load, naming the account.
func TestEffortCheckedAtLoad(t *testing.T) {
	cfg := &Config{ID: "worker-t", Accounts: []*Account{{Name: "opus-max", Effort: "huge"}}}
	if err := cfg.Default(t.TempDir()); err == nil || !strings.Contains(err.Error(), "account opus-max: effort") {
		t.Fatalf("got %v", err)
	}
}

// claude and opencode get the effort flag their CLI takes; a command runner
// expands {effort}.
func TestEffortFlags(t *testing.T) {
	logDir := t.TempDir()
	record := `printf '%s\n' "$@" > ` + logDir + `/args
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s1"}'`
	claude := fakeCLI(t, record)
	claudeRunner{bin: claude}.Run(context.Background(), AgentRun{Dir: t.TempDir(), Prompt: "p", Model: "opus", Effort: "high", Timeout: time.Minute})
	if a := readArgs(t, logDir); !strings.Contains(a, "--model\nopus\n--effort\nhigh\n") {
		t.Errorf("claude args:\n%s", a)
	}

	oc := fakeCLI(t, record)
	opencodeRunner{bin: oc}.Run(context.Background(), AgentRun{Dir: t.TempDir(), Prompt: "p", Model: "p/m", Effort: "max", Timeout: time.Minute})
	if a := readArgs(t, logDir); !strings.Contains(a, "-m\np/m\n--variant\nmax\n") {
		t.Errorf("opencode args:\n%s", a)
	}

	cmd := fakeCLI(t, record)
	commandRunner{argv: []string{cmd, "--think={effort}", "{prompt}"}}.Run(context.Background(), AgentRun{Dir: t.TempDir(), Prompt: "p", Effort: "deep", Timeout: time.Minute})
	if a := readArgs(t, logDir); !strings.Contains(a, "--think=deep\n") {
		t.Errorf("command args:\n%s", a)
	}

	// No effort: no flag (the CLI's default).
	claudeRunner{bin: claude}.Run(context.Background(), AgentRun{Dir: t.TempDir(), Prompt: "p", Timeout: time.Minute})
	if a := readArgs(t, logDir); strings.Contains(a, "--effort") {
		t.Errorf("no effort set, yet:\n%s", a)
	}
	if runDetail("opus", "high") != "opus, effort high" || runDetail("", "high") != "effort high" || runDetail("opus", "") != "opus" {
		t.Error("runDetail")
	}
}

func readArgs(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, "args"))
	return string(b)
}

// With an effort set, the runner check also needs the CLI's effort flag; an
// older CLI fails worker check / startup instead of every run.
func TestCheckRunnerEffortFlag(t *testing.T) {
	old := fakeCLI(t, `echo "--setting-sources --restricted --strict-mcp-config --mcp-config --tools"`)
	cfg := &Config{RunnerSpecs: map[string]*RunnerSpec{"claude-old": {Kind: "claude", Bin: old}},
		Accounts: []*Account{{Name: "a", Runner: "claude-old"}}}
	r, err := cfg.RunnerFor("claude-old")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.CheckRunner(context.Background(), "claude-old", r, ""); err != nil {
		t.Fatalf("no effort set: the old CLI is fine: %v", err)
	}
	cfg.Accounts[0].Effort = "high"
	if err := cfg.CheckRunner(context.Background(), "claude-old", r, ""); err == nil || !strings.Contains(err.Error(), "--effort") {
		t.Fatalf("got %v", err)
	}
	if ps := cfg.CheckRunners(context.Background(), ""); len(ps) != 1 || !strings.Contains(ps[0], "runner claude-old") {
		t.Fatalf("CheckRunners: %v", ps)
	}
	oc := fakeCLI(t, `echo "--format --session --model"`)
	ocr := opencodeRunner{bin: oc}
	if err := ocr.EffortSupported(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "--variant") {
		t.Fatalf("opencode: %v", err)
	}
}

// Accounts of one runner that differ in model or effort are a warning.
func TestAccountWarnings(t *testing.T) {
	cfg := &Config{Accounts: []*Account{
		{Name: "opus-1", Model: "opus", Effort: "high"},
		{Name: "opus-2", Model: "opus", Effort: "high"},
		{Name: "cheap", Model: "sonnet"},
	}}
	ws := cfg.AccountWarnings()
	if len(ws) != 1 || !strings.Contains(ws[0], "opus-1 (opus, effort high) and cheap (sonnet)") {
		t.Fatalf("%v", ws)
	}
	cfg.Accounts = cfg.Accounts[:2]
	if ws := cfg.AccountWarnings(); len(ws) != 0 {
		t.Fatalf("same settings: %v", ws)
	}
}
