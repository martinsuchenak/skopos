//go:build linux

package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBwrapProfileFromDocs runs the bwrap profile from the worker docs
// around a stand-in agent CLI, through the worker's own placeholder
// expansion and command assembly: the run keeps what it needs (worktree,
// commits, its login, its run directory) and sees nothing else of the
// worker. Needs bwrap and git, and a host that allows user namespaces;
// skipped otherwise. SKOPOS_BWRAP_TEST_DIR names a directory outside /tmp
// to build the fixture in (default: the home directory) — the profile
// mounts a fresh /tmp, which would hide a fixture there for free.
func TestBwrapProfileFromDocs(t *testing.T) {
	for _, bin := range []string{"bwrap", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not installed", bin)
		}
	}
	if out, err := exec.Command("bwrap", "--unshare-all", "--ro-bind", "/usr", "/usr", "--symlink", "usr/bin", "/bin",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("bwrap cannot create namespaces here: %v %s", err, out)
	}
	parent := os.Getenv("SKOPOS_BWRAP_TEST_DIR")
	if parent == "" {
		parent, _ = os.UserHomeDir()
	}
	base, err := os.MkdirTemp(parent, "skopos-bwrap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if strings.HasPrefix(base, "/tmp/") {
		t.Fatalf("the fixture must live outside /tmp: %s", base)
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	var (
		clone      = filepath.Join(base, "clones", "app")
		other      = filepath.Join(base, "clones", "other")
		state      = filepath.Join(base, "state")
		worktree   = filepath.Join(state, "worktrees", "item-1")
		login      = filepath.Join(base, "logins", "a")
		otherLogin = filepath.Join(base, "logins", "b")
		home       = filepath.Join(base, "home")
		envFile    = filepath.Join(base, "etc", "skopos-worker.env")
		cli        = filepath.Join(base, "cli", "fakecli")
	)
	write(filepath.Join(clone, "README"), "app\n", 0o644)
	git(clone, "init", "-q", "-b", "main")
	// The agent commits with the identity in the clone's config: a global
	// ~/.gitconfig is not inside the wrapper.
	git(clone, "config", "user.name", "skopos agent")
	git(clone, "config", "user.email", "agent@example.test")
	git(clone, "add", ".")
	git(clone, "commit", "-q", "-m", "base")
	git(clone, "worktree", "add", "-q", "-b", "agent/item-1", worktree)
	write(filepath.Join(other, "secret"), "other repo\n", 0o644)
	write(filepath.Join(login, "auth.json"), "{}\n", 0o600)
	write(filepath.Join(otherLogin, "auth.json"), "{}\n", 0o600)
	write(filepath.Join(home, ".ssh", "id"), "key\n", 0o600)
	write(envFile, "SKOPOS_API_KEY=k\n", 0o600)
	write(filepath.Join(state, "branches", "item-1.json"), "{}\n", 0o600)
	write(filepath.Join(state, "runs", "r-other", "mcp.json"), "{}\n", 0o600)
	write(cli, `#!/bin/sh
try() { name=$1; shift; if "$@" >/dev/null 2>&1; then echo "ok $name"; else echo "FAIL $name"; fi; }
deny() { name=$1; shift; if "$@" >/dev/null 2>&1; then echo "FAIL $name"; else echo "ok $name"; fi; }
try arg test "$1" = "--print"
try cwd test "$(pwd)" = "$T_DIR"
try read-worktree cat "$T_DIR/README"
try write-worktree sh -c "echo x > '$T_DIR/new'"
try git-commit sh -c "cd '$T_DIR' && git add new && git commit -q -m 'agent: step 1'"
try git-log git -C "$T_DIR" log --oneline -1
try read-login cat "$T_LOGIN/auth.json"
try write-login sh -c "echo x > '$T_LOGIN/session'"
try write-rundir sh -c "echo x > '$T_RUNDIR/tmp/probe'"
try write-tmp sh -c "echo x > /tmp/probe"
deny other-clone cat "$T_OTHER/secret"
deny other-clone-dir ls "$T_OTHER"
deny clone-working-tree cat "$T_CLONE/README"
deny env-file cat "$T_ENVFILE"
deny other-login cat "$T_OTHERLOGIN/auth.json"
deny other-run ls "$T_OTHERRUN"
deny state-dir ls "$T_STATE/branches"
deny home cat "$T_HOME/.ssh/id"
deny write-cli sh -c "echo x > '$T_CLIDIR/x'"
deny write-usr sh -c "echo x > /usr/x"
deny write-git-config sh -c "echo '[core]' >> '$T_GITDIR/config'"
deny write-git-hook sh -c "echo '#!/bin/sh' > '$T_GITDIR/hooks/pre-push'"
ln -s "$T_ENVFILE" "$T_RUNDIR/implement.log" 2>/dev/null
ln -s "$T_ENVFILE" "$T_RUNDIR/mcp.json.tmp" 2>/dev/null
`, 0o755)

	cfg := &Config{StateDir: state, Bin: cli, Repos: map[string]*Repo{"ws": {Path: clone,
		Stages: map[string]*StageProfile{"implement": {Wrapper: docsBwrapWrapper(t)}}}}}
	w := &Worker{cfg: cfg, sessions: map[string]string{}, accounts: map[string]string{}, executors: map[string]string{}, grants: map[string]string{}}
	run := &Run{ID: "r1", WorkspaceID: "ws", Stage: "implement"}
	ar := w.agentRun(worktree, "p", "", nil, 0, &Account{Name: "a", Env: map[string]string{"CLAUDE_CONFIG_DIR": login}}, run, "implement")
	if ar.StartErr != "" {
		t.Fatal(ar.StartErr)
	}
	name, argv := agentCommand(ar, []string{"--print"})
	cmd := exec.Command(name, argv...)
	cmd.Dir = worktree
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "PWD=" + worktree,
		"T_DIR=" + worktree, "T_LOGIN=" + login, "T_RUNDIR=" + cfg.runDir("r1"), "T_OTHER=" + other, "T_CLONE=" + clone,
		"T_ENVFILE=" + envFile, "T_OTHERLOGIN=" + otherLogin, "T_OTHERRUN=" + filepath.Join(state, "runs", "r-other"),
		"T_STATE=" + state, "T_HOME=" + home, "T_CLIDIR=" + filepath.Dir(cli), "T_GITDIR=" + filepath.Join(clone, ".git")}
	out, err := cmd.CombinedOutput()
	t.Logf("wrapped run:\n%s", out)
	if err != nil {
		t.Fatalf("the wrapped run failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 22 {
		t.Fatalf("expected 22 probe results, got %d", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "ok ") {
			t.Errorf("probe: %s", l)
		}
	}
	// What the run did is on the host where it should be, and only there.
	if got := git(worktree, "log", "--format=%s", "-1"); got != "agent: step 1" {
		t.Errorf("the agent's commit must land on the branch: %q", got)
	}
	for _, p := range []string{filepath.Join(login, "session"), filepath.Join(cfg.runDir("r1"), "tmp", "probe")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must exist on the host: %v", p, err)
		}
	}
	if _, err := os.Stat("/tmp/probe"); err == nil {
		t.Error("the run's /tmp must not be the host's")
	}
	if got := git(worktree, "log", "--format=%an", "-1"); got != "skopos agent" {
		t.Errorf("the commit carries the identity from the clone's config: %q", got)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "hooks", "pre-push")); err == nil {
		t.Error("the run must not be able to leave a git hook for the worker to run")
	}
	// The run left symlinks in its directory pointing at the env file; the
	// worker's own writes there replace them, they never go through.
	writeRunLog(ar.LogFile, []byte("agent output"), nil)
	if err := writeFile0600(filepath.Join(cfg.runDir("r1"), "mcp.json"), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(envFile); string(b) != "SKOPOS_API_KEY=k\n" {
		t.Errorf("the env file was written through a symlink the run left: %q", b)
	}
	// The worker's own setup and check commands for the stage run behind
	// the same wrapper: a check that executes the branch's code sees what
	// the agent saw, and nothing more.
	acct := &Account{Name: "a", Env: map[string]string{"CLAUDE_CONFIG_DIR": login}}
	cctx := withShellWrap(t.Context(), w.stageShellWrap(run, acct, "implement"))
	checks := runChecks(cctx, worktree, []string{"cat README && test -f new", "cat " + filepath.Join(other, "secret"), "cat " + envFile, "cat " + filepath.Join(clone, "README")}, nil, nil, runLimits{total: time.Minute})
	if len(checks) != 4 || !checks[0].Passed {
		t.Fatalf("a check must run in the worktree behind the wrapper: %+v", checks)
	}
	for _, c := range checks[1:] {
		if c.Passed || strings.Contains(c.Output, "other repo") || strings.Contains(c.Output, "SKOPOS_API_KEY") {
			t.Errorf("a check reached outside the wrapper: %+v", c)
		}
	}
}
