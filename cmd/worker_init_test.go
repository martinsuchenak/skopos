package cmd

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/worker"
	"github.com/paularlott/cli"
)

// worker init on a fresh directory: an annotated config (0600) that actually
// parses into a worker.Config, plus the secrets env file beside it.
func TestWorkerInitCreatesConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	var out bytes.Buffer
	if err := runWorkerInit(&out, cfgPath, "https://skopos.example.com", []string{"github.com/you/repo"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if st := statMode(cfgPath, 0); st != 0o600 {
		t.Fatalf("config must be 0600, got %o", st)
	}
	var file struct {
		Client map[string]string
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.Decode(string(raw), &file); err != nil {
		t.Fatalf("generated config must parse: %v\n%s", err, raw)
	}
	if file.Worker == nil {
		t.Fatal("generated config has no [worker] section")
	}
	if file.Worker.ID == "" || !strings.HasPrefix(file.Worker.ID, "worker-") {
		t.Fatalf("worker id must be prefilled with the worker- prefix: %q", file.Worker.ID)
	}
	if err := file.Worker.Default(t.TempDir()); err != nil {
		t.Fatalf("generated [worker] section must pass Default(): %v", err)
	}
	if !strings.Contains(string(raw), "github.com/you/repo") {
		t.Fatal("workspace blocks must be emitted")
	}
	// The env file sits where the daemon's default --env-file finds it.
	envPath := filepath.Join(dir, "skopos-worker.env")
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("env file missing: %v", err)
	}
	if st := statMode(envPath, 0); st != 0o600 {
		t.Fatalf("env file must be 0600, got %o", st)
	}
}

// worker init on an existing config (e.g. from `skopos setup`): only the
// [worker] section is added, everything else preserved; a re-run refuses.
func TestWorkerInitAppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	existing := "[client]\nserver_url = \"https://skopos.example.com\"\n\n[server]\nport = 8080\n"
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkerInit(&out, cfgPath, "", nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"[client]", "skopos.example.com", "[server]", "port = 8080", "[worker]"} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("preserved/generated content missing %q:\n%s", keep, raw)
		}
	}
	var file struct {
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.Decode(string(raw), &file); err != nil {
		t.Fatalf("merged config must parse: %v\n%s", err, raw)
	}
	if file.Worker == nil {
		t.Fatal("[worker] missing after append")
	}
	// The template protects the full default set (hotfix/** included — a
	// narrower list would protect LESS than an unconfigured worker).
	if !strings.Contains(string(raw), "hotfix/**") {
		t.Errorf("protected_branches must include hotfix/**:\n%s", raw)
	}
	// Re-run: the section holds operator edits by now — refuse.
	if err := runWorkerInit(&out, cfgPath, "", nil); err == nil || !strings.Contains(err.Error(), "already has a [worker] section") {
		t.Fatalf("re-run must refuse: %v", err)
	}
}

// --server-url on an existing config without [client] adds the section; an
// existing [client] wins.
func TestWorkerInitServerURLOnExisting(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\nport = 8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkerInit(&out, cfgPath, "https://skopos.real.com", nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), "[client]") || !strings.Contains(string(raw), "skopos.real.com") {
		t.Fatalf("--server-url must add [client] when absent:\n%s", raw)
	}
	if !strings.Contains(string(raw), "[server]") {
		t.Fatalf("existing sections must survive:\n%s", raw)
	}

	// An existing [client] is left alone.
	other := filepath.Join(dir, "other.toml")
	if err := os.WriteFile(other, []byte("[client]\nserver_url = \"https://chosen.example\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runWorkerInit(&bytes.Buffer{}, other, "https://ignored.example", nil); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(other)
	if strings.Contains(string(raw), "ignored.example") || !strings.Contains(string(raw), "chosen.example") {
		t.Fatalf("existing [client] must win over --server-url:\n%s", raw)
	}
}

// The env file holds secrets once touched — init never overwrites it.
func TestWorkerInitKeepsExistingEnvFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "skopos-config.toml")
	envPath := filepath.Join(dir, "skopos-worker.env")
	secret := "SKOPOS_API_KEY=sk_real\n"
	if err := os.WriteFile(envPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runWorkerInit(&out, cfgPath, "", nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	raw, err := os.ReadFile(envPath)
	if err != nil || string(raw) != secret {
		t.Fatalf("env file must be untouched, got %q err %v", raw, err)
	}
}

// The interactive init asks for what's missing, reads the clone's default
// branch, sets up the GitHub push with a draft PR, and puts the worker key
// and the token in the env file, never in the TOML.
func TestWorkerInitAsksAndFillsIn(t *testing.T) {
	// A clone whose origin's default branch is PROD (like FF3).
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "PROD"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", src}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	clone := filepath.Join(t.TempDir(), "repo")
	if out, err := exec.Command("git", "clone", "-q", src, clone).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}

	o := workerInitOptions{Push: "auto", TokenEnv: "GH_TOKEN", RepoPaths: map[string]string{}, Clone: map[string]bool{}}
	answers := strings.Join([]string{
		"https://skopos.example.com", // server
		"github.com/you/repo",        // workspace
		"sk_worker_secret",           // worker key (secret)
		clone,                        // the clone
		"",                           // push to GitHub: yes
		"ghp_token_secret",           // the token (secret)
	}, "\n") + "\n"
	in := bufio.NewReader(strings.NewReader(answers))
	var out bytes.Buffer
	secret := func(in *bufio.Reader, out io.Writer, label string) string { return promptTo(in, out, label, "") }
	askWorkerInit(in, &out, &o, secret)
	repos, secrets, err := prepareWorkerRepos(t.Context(), &out, o)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "skopos-config.toml")
	if err := runWorkerInitRepos(&out, cfgPath, o.ServerURL, repos, secrets, o.ClaudeEnv); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(cfgPath)
	var file struct {
		Client map[string]string
		Worker *worker.Config `toml:"worker"`
	}
	if _, err := toml.Decode(string(raw), &file); err != nil {
		t.Fatalf("config must parse: %v\n%s", err, raw)
	}
	r := file.Worker.Repos["github.com/you/repo"]
	if r == nil || r.Path != clone || r.DefaultBranch != "PROD" || r.Push.Kind != "github" ||
		r.Push.RemoteURL != "https://github.com/you/repo.git" || r.Push.TokenEnv != "GH_TOKEN" || r.Push.PullRequest != "draft" {
		t.Fatalf("repo block: %+v\n%s", r, raw)
	}
	if file.Client["server_url"] != "https://skopos.example.com" {
		t.Fatalf("server: %v", file.Client)
	}
	if err := file.Worker.Default(t.TempDir()); err != nil {
		t.Fatalf("must pass Default(): %v", err)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("no secret in the TOML:\n%s", raw)
	}
	env, _ := os.ReadFile(workerEnvFilePath(cfgPath))
	if !strings.Contains(string(env), "\nSKOPOS_API_KEY=sk_worker_secret\n") || !strings.Contains(string(env), "\nGH_TOKEN=ghp_token_secret\n") {
		t.Fatalf("env file:\n%s", env)
	}
}

// Unattended: a bare --repo path with one workspace; no token means no push.
func TestWorkerInitUnattendedOptions(t *testing.T) {
	cmd := workerInitCmd()
	var got workerInitOptions
	cmd.Run = func(_ context.Context, c *cli.Command) error {
		var err error
		got, err = workerInitOptionsFrom(c)
		return err
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("SKOPOS_API_KEY", "sk_env")
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"init", "--workspace", "github.com/you/repo", "--repo", "/srv/repo", "--clone"}
	if err := cmd.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.RepoPaths["github.com/you/repo"] != "/srv/repo" || !got.CloneAll || got.APIKey != "sk_env" || got.Push != "auto" {
		t.Fatalf("%+v", got)
	}
	repos, secrets, err := prepareWorkerRepos(t.Context(), io.Discard, workerInitOptions{
		Workspaces: got.Workspaces, RepoPaths: got.RepoPaths, Push: "auto", TokenEnv: "GH_TOKEN",
	})
	if err != nil || repos[0].PushKind != "" || secrets["GH_TOKEN"] != "" {
		t.Fatalf("no token, no push: %+v %v %v", repos, secrets, err)
	}
}

// A Claude credential in the environment becomes the claude account's env
// (the value in the env file); without one the account uses the machine's
// own login, with no ${VAR} to resolve empty.
func TestWorkerInitClaudeCredential(t *testing.T) {
	for _, withToken := range []bool{true, false} {
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", map[bool]string{true: "sk-ant-oat-test", false: ""}[withToken])
		key, value := claudeCredential()
		cfgPath := filepath.Join(t.TempDir(), "skopos-config.toml")
		secrets := map[string]string{"SKOPOS_API_KEY": "sk_k"}
		if key != "" {
			secrets[key] = value
		}
		var out bytes.Buffer
		if err := runWorkerInitRepos(&out, cfgPath, "https://s.internal", []repoInit{{Workspace: "github.com/you/repo", Path: "/srv/repo"}}, secrets, key); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(cfgPath)
		var file struct {
			Worker *worker.Config `toml:"worker"`
		}
		if _, err := toml.Decode(string(raw), &file); err != nil {
			t.Fatal(err)
		}
		a := file.Worker.Accounts[0]
		env, _ := os.ReadFile(workerEnvFilePath(cfgPath))
		if withToken {
			if a.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "${CLAUDE_CODE_OAUTH_TOKEN}" || !strings.Contains(string(env), "\nCLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-test\n") {
				t.Fatalf("token account: %+v\n%s", a, env)
			}
		} else if len(a.Env) != 0 {
			t.Fatalf("no credential, no env: %+v", a)
		}
		if strings.Contains(out.String(), "skopos key create") || strings.Contains(out.String(), "fill in the repos") {
			t.Fatalf("next steps must skip what init did:\n%s", out.String())
		}
	}
}
