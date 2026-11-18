package worker

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitT runs git in dir and fails the test on an error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// originRepo is a repository with one commit on branch, to clone from.
func originRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	return dir
}

// A repo without a path is the worker's own clone, under its state folder;
// a workspace id that cannot name such a folder is refused.
func TestCloneDirFor(t *testing.T) {
	for _, ok := range []string{"github.com/acme/shop", "github.com/acme/.github", "gitea.example.com/a/b/c"} {
		if dir, err := cloneDirFor("/state", ok); err != nil || dir != filepath.Join("/state", "clones", filepath.FromSlash(ok)) {
			t.Errorf("%s: %s %v", ok, dir, err)
		}
	}
	for _, bad := range []string{"", "/abs/x", "a/../b", "a//b", "./a", "a/b/", `a\b`, "c:x", "a/x.cloning-1"} {
		if _, err := cloneDirFor("/state", bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	cfg := &Config{ID: "worker-t", StateDir: t.TempDir(), Repos: map[string]*Repo{
		"github.com/acme/shop": {},
		"github.com/acme/api":  {Path: "/srv/api"},
	}}
	if err := cfg.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if r := cfg.Repos["github.com/acme/shop"]; !r.SelfClone() || r.Path != filepath.Join(cfg.StateDir, "clones", "github.com", "acme", "shop") {
		t.Errorf("path-less repo: %+v", r)
	}
	if r := cfg.Repos["github.com/acme/api"]; r.SelfClone() || r.Path != "/srv/api" {
		t.Errorf("a repo with a path keeps it: %+v", r)
	}
	bad := &Config{ID: "worker-t", StateDir: t.TempDir(), Repos: map[string]*Repo{"github.com/acme/shop": {CloneURL: "https://u:p@example.com/x.git"}}}
	if err := bad.Default(t.TempDir()); err == nil || !strings.Contains(err.Error(), "clone_url") {
		t.Errorf("a clone URL with credentials: %v", err)
	}
}

// Where a clone comes from: clone_url, else the push URL, else the
// workspace's https URL.
func TestCloneSource(t *testing.T) {
	r := &Repo{}
	if got := cloneSource("github.com/acme/shop", r); got != "https://github.com/acme/shop.git" {
		t.Errorf("default: %s", got)
	}
	r.Push.RemoteURL = "https://git.example.com/acme/shop.git"
	if got := cloneSource("github.com/acme/shop", r); got != r.Push.RemoteURL {
		t.Errorf("push URL: %s", got)
	}
	r.CloneURL = "https://mirror.example.com/shop.git"
	if got := cloneSource("github.com/acme/shop", r); got != r.CloneURL {
		t.Errorf("clone_url: %s", got)
	}
}

// A clone lands in place only when complete, on the default branch; a
// failed one leaves nothing that could be taken for a clone.
func TestCloneInto(t *testing.T) {
	src := originRepo(t, "trunk")
	dest := filepath.Join(t.TempDir(), "clones", "acme", "shop")
	if err := cloneInto(context.Background(), src, dest, "trunk", nil); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, dest, "rev-parse", "--abbrev-ref", "HEAD"); got != "trunk" {
		t.Errorf("branch: %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "file.txt")); err != nil || string(b) != "hello\n" {
		t.Errorf("checkout: %q %v", b, err)
	}

	bad := filepath.Join(t.TempDir(), "shop")
	if err := cloneInto(context.Background(), src, bad, "main", nil); err == nil || !strings.Contains(err.Error(), `default branch "main"`) {
		t.Fatalf("a missing default branch: %v", err)
	}
	if _, err := os.Stat(bad); err == nil {
		t.Error("a failed clone must not be in place")
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(bad), ".shop.cloning-*"))
	if len(left) != 0 {
		t.Errorf("the temporary folder must go: %v", left)
	}
}

// The worker clones in the background and reports the workspace unhealthy
// until the clone is there; a failure is retried after a back-off.
func TestMaybeClone(t *testing.T) {
	src := originRepo(t, "main")
	state := t.TempDir()
	ws := "github.com/acme/shop"
	cfg := &Config{ID: "worker-t", StateDir: state, Workspaces: []string{ws}, Repos: map[string]*Repo{ws: {}}}
	if err := cfg.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cfg.Repos[ws].CloneURL = src // a local path: Default only takes http(s)
	w := New(cfg, nil, slog.New(slog.DiscardHandler))
	if why := w.cloneProblem(ws); !strings.Contains(why, "not cloned") {
		t.Fatalf("before: %q", why)
	}
	// Leftovers of a killed worker's clone go before the first clone.
	leftover := filepath.Join(state, "clones", "github.com", "acme", ".shop.cloning-123")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	w.maybeClone(t.Context())
	if why := w.cloneProblem(ws); why != "" && !strings.Contains(why, "cloning") {
		t.Fatalf("while cloning: %q", why)
	}
	waitClone(t, w)
	if why := w.cloneProblem(ws); why != "" {
		t.Fatalf("after: %q", why)
	}
	if !isGitClone(cfg.Repos[ws].Path) {
		t.Fatal("the clone must be in place")
	}
	if _, err := os.Stat(leftover); err == nil {
		t.Error("the leftover must be removed")
	}

	// A source that fails: unhealthy with the reason, tried again later.
	ws2 := "github.com/acme/gone"
	cfg.Workspaces = append(cfg.Workspaces, ws2)
	cfg.Repos[ws2] = &Repo{Path: filepath.Join(state, "clones", "github.com", "acme", "gone"), selfClone: true, DefaultBranch: "main",
		CloneURL: filepath.Join(t.TempDir(), "missing")}
	w.maybeClone(t.Context())
	waitClone(t, w)
	if why := w.cloneProblem(ws2); !strings.Contains(why, "failed") || !strings.Contains(why, "trying again") {
		t.Fatalf("failed clone: %q", why)
	}
	w.maybeClone(t.Context()) // not due yet: no second attempt
	w.clones.mu.Lock()
	busy, failures := w.clones.busy, w.clones.state[ws2].failures
	w.clones.mu.Unlock()
	if busy || failures != 1 {
		t.Fatalf("back-off: busy=%v failures=%d", busy, failures)
	}
	// The health check reports it for the claim.
	if h := w.checkHealth(t.Context()); !strings.Contains(h[ws2], "failed") || h[ws] != "" {
		t.Fatalf("health: %v", h)
	}
}

func waitClone(t *testing.T, w *Worker) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		w.clones.mu.Lock()
		busy := w.clones.busy
		w.clones.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the clone did not finish")
}

// Clones the worker made for workspaces it no longer has are removed a
// week after it first saw them unused; one in use, and anything outside
// <state_dir>/clones, never.
func TestSweepClones(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "clones")
	inUse := filepath.Join(root, "github.com", "acme", "shop")
	unused := filepath.Join(root, "github.com", "old", "gone")
	for _, d := range []string{inUse, unused} {
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &Config{StateDir: state, Repos: map[string]*Repo{"github.com/acme/shop": {Path: inUse, selfClone: true}}}
	w := New(cfg, nil, slog.New(slog.DiscardHandler))
	now := time.Now()
	w.sweepClones(now)
	if !isGitClone(unused) {
		t.Fatal("an unused clone stays for a week")
	}
	w.sweepClones(now.Add(cloneKeep - time.Hour))
	if !isGitClone(unused) {
		t.Fatal("still within the week")
	}
	w.sweepClones(now.Add(cloneKeep + time.Minute))
	if isGitClone(unused) {
		t.Fatal("a week later the unused clone goes")
	}
	if _, err := os.Stat(filepath.Join(root, "github.com", "old")); err == nil {
		t.Error("its empty parent folders go too")
	}
	if !isGitClone(inUse) {
		t.Fatal("a clone in use stays")
	}
	if _, err := os.Stat(filepath.Join(root, ".unused.json")); err == nil {
		t.Error("nothing left unused: no record")
	}
}

// The push token goes with the worker's own fetches, only to the push
// URL's host, and never when the caller passes its own git settings.
func TestFetchAuth(t *testing.T) {
	t.Setenv("SKOPOS_TEST_PUSH_TOKEN", "tok-123456789")
	repo := &Repo{}
	repo.Push.Kind, repo.Push.RemoteURL, repo.Push.TokenEnv = "github", "https://github.com/acme/shop.git", "SKOPOS_TEST_PUSH_TOKEN"
	if env := gitAuthEnv(context.Background(), repo, "https://github.com/acme/shop.git"); len(env) != 5 || !strings.Contains(env[2], "Authorization: Basic") || env[4] != "GIT_CONFIG_VALUE_1=true" {
		t.Fatalf("same host: %v", env)
	}
	for _, other := range []string{"https://evil.example.com/acme/shop.git", "https://u:p@github.com/acme/shop.git", "http://github.com/acme/shop.git", "file:///tmp/x"} {
		if env := gitAuthEnv(context.Background(), repo, other); env != nil {
			t.Errorf("%s: the token must not go there: %v", other, env)
		}
	}
	none := *repo
	none.Push.Kind = "none"
	if env := gitAuthEnv(context.Background(), &none, repo.Push.RemoteURL); env != nil {
		t.Errorf("no push, no token: %v", env)
	}

	clone := originRepo(t, "main")
	gitT(t, clone, "remote", "add", "origin", "https://github.com/acme/shop.git")
	registerCloneRepo(clone, func() *Repo { return repo })
	if env := fetchAuthEnv(context.Background(), clone, nil, []string{"fetch", "--quiet", "origin"}); len(env) != 5 {
		t.Fatalf("a fetch in the clone: %v", env)
	}
	if env := fetchAuthEnv(context.Background(), clone, nil, []string{"-c", "x=y", "ls-remote", "origin"}); len(env) != 5 {
		t.Fatalf("ls-remote: %v", env)
	}
	if env := fetchAuthEnv(context.Background(), clone, nil, []string{"status"}); env != nil {
		t.Errorf("not a network command: %v", env)
	}
	if env := fetchAuthEnv(context.Background(), clone, []string{"GIT_CONFIG_COUNT=2"}, []string{"fetch"}); env != nil {
		t.Errorf("the caller's own settings stand: %v", env)
	}
	// A URL rewrite in the clone's config (writable by setup and check
	// code) would carry the header elsewhere: no token then.
	gitT(t, clone, "config", "url.https://evil.example.com/.insteadOf", "https://github.com/")
	if env := fetchAuthEnv(context.Background(), clone, nil, []string{"fetch"}); env != nil {
		t.Errorf("an insteadOf rule: no token: %v", env)
	}
	gitT(t, clone, "config", "--unset", "url.https://evil.example.com/.insteadOf")
	gitT(t, clone, "remote", "set-url", "origin", "https://evil.example.com/acme/shop.git")
	if env := fetchAuthEnv(context.Background(), clone, nil, []string{"fetch"}); env != nil {
		t.Errorf("an origin on another host gets no token: %v", env)
	}
	if env := fetchAuthEnv(context.Background(), t.TempDir(), nil, []string{"fetch"}); env != nil {
		t.Errorf("a folder the worker does not know: %v", env)
	}
}

// Without worker.server_commands the clone comes from the file's own repo,
// not from what the server's config says: the worker runs its setup and
// checks in what it cloned.
func TestCloneSourceNeedsServerCommands(t *testing.T) {
	local := &Repo{}
	local.Push.RemoteURL = "https://github.com/acme/shop.git"
	merged := &Repo{CloneURL: "https://evil.example.com/x.git"}
	c := &Config{Repos: map[string]*Repo{"ws": local}}
	if got := c.cloneSourceFor("ws", merged); got != local.Push.RemoteURL {
		t.Errorf("server_commands off: %s", got)
	}
	c.ServerCommands = true
	if got := c.cloneSourceFor("ws", merged); got != merged.CloneURL {
		t.Errorf("server_commands on: %s", got)
	}
}

// A paused worker starts no clone.
func TestNoCloneWhilePaused(t *testing.T) {
	ws := "github.com/acme/shop"
	cfg := &Config{ID: "worker-t", StateDir: t.TempDir(), Workspaces: []string{ws}, Repos: map[string]*Repo{ws: {}}}
	if err := cfg.Default(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	cfg.Repos[ws].CloneURL = originRepo(t, "main")
	w := New(cfg, nil, slog.New(slog.DiscardHandler))
	w.paused = "resume"
	w.maybeClone(t.Context())
	w.clones.mu.Lock()
	busy := w.clones.busy
	w.clones.mu.Unlock()
	if busy {
		t.Fatal("a paused worker must not clone")
	}
}
