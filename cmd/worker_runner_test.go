package cmd

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/worker"
)

const baseWorkerTOML = `[client]
server_url = "https://skopos.example.internal"

[worker]
id = "worker-t"
runners = ["claude"]   # explicit list
workspaces = ["ws"]

[[worker.accounts]]
name = "claude-1"
runner = "claude"
`

func decodeW(t *testing.T, content string) *worker.Config {
	t.Helper()
	var f struct {
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.Decode(content, &f); err != nil {
		t.Fatalf("decode: %v\n%s", err, content)
	}
	return f.Worker
}

// The config edit: runner_config block, account, and the explicit runners
// list — and the result validates.
func TestAddRunnerToConfig(t *testing.T) {
	got, err := addRunnerToConfig(baseWorkerTOML, runnerInput{
		Kind: "opencode", Name: "opencode-glm", Model: "zai-coding-plan/glm-5.3",
		Account: "glm", Priority: 2, Env: map[string]string{"ZAI_KEY": "${ZAI_KEY}"}, OpencodeConfig: "/etc/oc.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateWorkerTOML(got); err != nil {
		t.Fatalf("must validate: %v\n%s", err, got)
	}
	w := decodeW(t, got)
	if strings.Join(w.Runners, ",") != "claude,opencode-glm" {
		t.Fatalf("runners list: %v", w.Runners)
	}
	if !strings.Contains(got, "runners = [\"claude\", \"opencode-glm\"]") {
		t.Fatalf("runners line rewritten in place:\n%s", got)
	}
	spec := w.RunnerSpecs["opencode-glm"]
	if spec == nil || spec.Kind != "opencode" || spec.Config != "/etc/oc.json" {
		t.Fatalf("runner_config: %+v", spec)
	}
	a := w.Accounts[1]
	if a.Name != "glm" || a.Runner != "opencode-glm" || a.Model != "zai-coding-plan/glm-5.3" || a.Priority != 2 || a.Env["ZAI_KEY"] != "${ZAI_KEY}" {
		t.Fatalf("account: %+v", a)
	}

	// A second account on the same runner reuses its runner_config.
	again, err := addRunnerToConfig(got, runnerInput{Kind: "opencode", Name: "opencode-glm", Model: "zai-coding-plan/glm-5.3", Account: "glm-2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(again, "[worker.runner_config.opencode-glm]") != 1 || strings.Count(again, "opencode-glm\"]") != 1 {
		t.Fatalf("reuse must not duplicate the runner or the runners entry:\n%s", again)
	}
	if err := validateWorkerTOML(again); err != nil {
		t.Fatal(err)
	}

	// Plain built-in names need no runner_config block.
	plain, err := addRunnerToConfig(baseWorkerTOML, runnerInput{Kind: "claude", Name: "claude", Account: "claude-2", Model: "sonnet"})
	if err != nil || strings.Contains(plain, "runner_config") {
		t.Fatalf("plain claude: %v\n%s", err, plain)
	}

	for name, in := range map[string]runnerInput{
		"duplicate account": {Kind: "claude", Name: "claude", Account: "claude-1"},
		"bad name":          {Kind: "claude", Name: "has space"},
		"bad kind":          {Kind: "gemini", Name: "g"},
		"command no argv":   {Kind: "command", Name: "aider"},
		"kind clash":        {Kind: "command", Name: "opencode-glm", Command: []string{"x"}},
	} {
		base := baseWorkerTOML
		if name == "kind clash" {
			base = got
		}
		if _, err := addRunnerToConfig(base, in); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := addRunnerToConfig("[client]\nx = 1\n", runnerInput{Kind: "claude", Name: "claude"}); err == nil || !strings.Contains(err.Error(), "worker init") {
		t.Fatalf("no [worker]: %v", err)
	}
	multi := strings.Replace(baseWorkerTOML, `runners = ["claude"]   # explicit list`, "runners = [\n  \"claude\",\n]", 1)
	if _, err := addRunnerToConfig(multi, runnerInput{Kind: "opencode", Name: "oc"}); err == nil || !strings.Contains(err.Error(), "by hand") {
		t.Fatalf("multi-line runners must be refused with guidance: %v", err)
	}
	// No explicit runners line: the default covers it, nothing to rewrite.
	noList := strings.Replace(baseWorkerTOML, "runners = [\"claude\"]   # explicit list\n", "", 1)
	out, err := addRunnerToConfig(noList, runnerInput{Kind: "opencode", Name: "oc"})
	if err != nil || strings.Contains(out, "runners =") {
		t.Fatalf("default runners: %v\n%s", err, out)
	}
	if w := decodeW(t, out); w.Default(t.TempDir()) != nil || !strings.Contains(strings.Join(w.Runners, ","), "oc") {
		t.Fatalf("defaulted runners must include the new runner: %v", w.Runners)
	}
}

func TestAppendEnvVar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "w.env")
	if wrote, err := appendEnvVar(p, "K", "v1"); err != nil || !wrote {
		t.Fatalf("create: %v %v", wrote, err)
	}
	if wrote, _ := appendEnvVar(p, "K", "v2"); wrote {
		t.Fatal("an existing key must not be overwritten")
	}
	if _, err := appendEnvVar(p, "L", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	st, _ := os.Stat(p)
	if string(raw) != "K=v1\nL=\n" || st.Mode().Perm() != 0o600 {
		t.Fatalf("env file: %q %v", raw, st.Mode().Perm())
	}
}

// A custom opencode provider moves into a providers-only config: a literal
// key becomes {env:VAR} (value handed back for the env file), an existing
// {env:X} is kept, and the chosen model is listed.
func TestOpencodeProviderExtraction(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(user, []byte(`{"provider":{"llmrouter":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"https://r/v1","apiKey":"sk-secret"},"models":{"glm-5":{}}},"envd":{"options":{"apiKey":"{env:ENVD_KEY}"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	block, keyVar, keyVal, ok := customOpencodeProvider(user, "llmrouter")
	if !ok || keyVar != "LLMROUTER_API_KEY" || keyVal != "sk-secret" {
		t.Fatalf("literal key: %v %q %q", ok, keyVar, keyVal)
	}
	base := filepath.Join(dir, "providers.json")
	if err := mergeOpencodeProvider(base, "llmrouter", block, "llmrouter/qwen/qwen3.8-27b", keyVar); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(base)
	if strings.Contains(string(raw), "sk-secret") || !strings.Contains(string(raw), "{env:LLMROUTER_API_KEY}") || !strings.Contains(string(raw), "qwen/qwen3.8-27b") {
		t.Fatalf("providers file must reference the key, never hold it, and list the model:\n%s", raw)
	}
	if _, kv, val, ok := customOpencodeProvider(user, "envd"); !ok || kv != "ENVD_KEY" || val != "" {
		t.Fatalf("env reference kept: %q %q", kv, val)
	}
	if _, _, _, ok := customOpencodeProvider(user, "zai-coding-plan"); ok {
		t.Fatal("a provider absent from the user config is built in, not custom")
	}
}

// fakeBin writes an executable script into dir.
func fakeBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The wizard, scripted end to end against fake CLIs: opencode with a
// custom provider model → providers-only config written, key moved to the
// env file, runner + account added, list shows it.
func TestRunnerWizardOpencodeCustomProvider(t *testing.T) {
	bin := t.TempDir()
	fakeBin(t, bin, "opencode", `case "$1" in
  --version) echo 1.18.27;;
  models) printf 'zai-coding-plan/glm-5.3\nllmrouter/qwen/qwen3.8-27b\n';;
  run) echo '--format --session --model';;
esac`)
	fakeBin(t, bin, "claude", `echo "2.1.300 (Claude Code)"`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	if err := runWorkerInit(&strings.Builder{}, cfgPath, "https://s.example.internal", nil); err != nil {
		t.Fatal(err)
	}
	userCfg := filepath.Join(dir, "user-opencode.json")
	if err := os.WriteFile(userCfg, []byte(`{"provider":{"llmrouter":{"npm":"@ai-sdk/openai-compatible","options":{"baseURL":"https://r/v1","apiKey":"sk-secret"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "skopos-worker.env")
	answers := strings.Join([]string{
		"2",    // kind: opencode
		"2",    // model #2: llmrouter/qwen/qwen3.8-27b
		"",     // runner name: suggested opencode-qwen3-8-27b
		"",     // write the provider config: yes
		"",     // copy the key: yes
		"qwen", // account name
		"",     // priority default
		"",     // write it: yes
		"n",    // smoke test: no
	}, "\n") + "\n"
	var out strings.Builder
	if err := runRunnerWizard(context.Background(), bufio.NewReader(strings.NewReader(answers)), &out, cfgPath, envPath, wizardEnv{opencodeUserConfig: userCfg}); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out.String())
	}
	transcript := out.String()
	for _, want := range []string{"✓ opencode", "✓ claude", "llmrouter/qwen/qwen3.8-27b", "custom provider", "Written."} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript missing %q:\n%s", want, transcript)
		}
	}
	raw, _ := os.ReadFile(cfgPath)
	w := decodeW(t, string(raw))
	spec := w.RunnerSpecs["opencode-qwen3-8-27b"]
	if spec == nil || spec.Kind != "opencode" || spec.Config != filepath.Join(dir, "opencode-providers.json") {
		t.Fatalf("runner spec: %+v\n%s", spec, raw)
	}
	var acct *worker.Account
	for _, a := range w.Accounts {
		if a.Name == "qwen" {
			acct = a
		}
	}
	if acct == nil || acct.Model != "llmrouter/qwen/qwen3.8-27b" || acct.Env["LLMROUTER_API_KEY"] != "${LLMROUTER_API_KEY}" {
		t.Fatalf("account: %+v", acct)
	}
	env, _ := os.ReadFile(envPath)
	if !strings.Contains(string(env), "LLMROUTER_API_KEY=sk-secret") {
		t.Fatalf("the key must land in the env file:\n%s", env)
	}
	prov, _ := os.ReadFile(filepath.Join(dir, "opencode-providers.json"))
	if strings.Contains(string(prov), "sk-secret") {
		t.Fatal("the providers file must not hold the key")
	}
	if err := validateWorkerTOML(string(raw)); err != nil {
		t.Fatalf("result must validate: %v", err)
	}
	var list strings.Builder
	printRunnerList(context.Background(), &list, w)
	if !strings.Contains(list.String(), "opencode-qwen3-8-27b") || !strings.Contains(list.String(), "account:  qwen  model=llmrouter/qwen/qwen3.8-27b") {
		t.Fatalf("list:\n%s", list.String())
	}
}

// `runner test`: a real one-prompt probe per account; a command runner
// answering the probe word passes, one that doesn't fails with a reason.
func TestRunnerTestProbe(t *testing.T) {
	bin := t.TempDir()
	fakeBin(t, bin, "good-agent", `cat >/dev/null; echo "SKOPOS-PROBE-OK"`)
	fakeBin(t, bin, "bad-agent", `cat >/dev/null; echo "hello?"`)
	cfg := &worker.Config{
		RunnerSpecs: map[string]*worker.RunnerSpec{
			"good": {Kind: "command", Command: []string{filepath.Join(bin, "good-agent")}},
			"bad":  {Kind: "command", Command: []string{filepath.Join(bin, "bad-agent")}},
		},
		Accounts: []*worker.Account{{Name: "g", Runner: "good"}, {Name: "b", Runner: "bad"}},
	}
	if err := cfg.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := testAccounts(context.Background(), &out, cfg, "good", "", time.Minute); err != nil {
		t.Fatalf("good: %v\n%s", err, out.String())
	}
	out.Reset()
	err := testAccounts(context.Background(), &out, cfg, "", "", time.Minute)
	if err == nil || !strings.Contains(out.String(), "b (runner bad") || !strings.Contains(out.String(), "not with SKOPOS-PROBE-OK") {
		t.Fatalf("bad agent must fail with a reason: %v\n%s", err, out.String())
	}
	if err := testAccounts(context.Background(), &out, cfg, "nope", "", time.Minute); err == nil {
		t.Fatal("no matching account must be an error")
	}
	// An env reference that resolved empty fails before any probe: the CLI
	// would otherwise fall back to its own login and pass on the wrong auth.
	cfg.Accounts = append(cfg.Accounts, &worker.Account{Name: "keyless", Runner: "good", Env: map[string]string{"ANTHROPIC_API_KEY": ""}})
	out.Reset()
	if err := testAccounts(context.Background(), &out, cfg, "", "keyless", time.Minute); err == nil || !strings.Contains(out.String(), "ANTHROPIC_API_KEY resolved empty") {
		t.Fatalf("empty env must fail: %v\n%s", err, out.String())
	}
}
