package worker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func guardTestClone(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"checkout", "-q", "-b", "agent/x"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// agentGit is git run as the agent would: straight in the clone, writing
// its config, nothing of the worker's in between.
func agentGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// checkout = "branch": the agent works in the clone and can write its git
// config. A core.fsmonitor it writes would run as the worker's user on the
// worker's next git status (strix vuln-0002): the worker's git refuses the
// clone instead, and the program never runs.
func TestWorkerGitRefusesAnAgentWrittenCommandInTheClone(t *testing.T) {
	clone := guardTestClone(t)
	registerClone(clone)
	ctx := context.Background()
	if _, err := git(ctx, clone, "status", "--porcelain"); err != nil {
		t.Fatalf("an untouched clone: %v", err)
	}

	// Entries the worker itself writes (a branch's upstream) are not
	// guarded: git keeps working.
	agentGit(t, clone, "config", "branch.agent/x.remote", "origin")
	if _, err := git(ctx, clone, "status", "--porcelain"); err != nil {
		t.Fatalf("an unguarded entry changed: %v", err)
	}

	marker := filepath.Join(t.TempDir(), "ran")
	agentGit(t, clone, "config", "core.fsmonitor", "touch "+marker+"; false")
	_, err := git(ctx, clone, "status", "--porcelain")
	if err == nil || !strings.Contains(err.Error(), "core.fsmonitor") {
		t.Fatalf("want a refusal naming core.fsmonitor, got %v", err)
	}
	// Retryable: the clone is this worker's problem, not the item's —
	// another worker takes the run.
	if !isRetryable(err) {
		t.Fatalf("the refusal must give the run back: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the agent's fsmonitor ran")
	}

	// Put back as recorded: the clone is usable again.
	agentGit(t, clone, "config", "--unset", "core.fsmonitor")
	if _, err := git(ctx, clone, "status", "--porcelain"); err != nil {
		t.Fatalf("the config as recorded again: %v", err)
	}

	// A hook the agent plants is refused the same way.
	hook := filepath.Join(clone, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, clone, "checkout", "-q", "main"); err == nil || !strings.Contains(err.Error(), "post-checkout") {
		t.Fatalf("want a refusal naming the hook, got %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the agent's hook ran")
	}
}

// A .git the agent swaps for a file pointing at a directory of its own is
// not followed: the clone is pinned to the git directory the worker
// recorded.
func TestWorkerGitPinsTheClone(t *testing.T) {
	clone := guardTestClone(t)
	registerClone(clone)
	env, pinned, err := gitPinEnv(clone)
	if err != nil || !pinned {
		t.Fatalf("pin: %v %v", pinned, err)
	}
	want := "GIT_DIR=" + filepath.Join(clone, ".git")
	found := false
	for _, kv := range env {
		if samePath(strings.TrimPrefix(kv, "GIT_DIR="), filepath.Join(clone, ".git")) && strings.HasPrefix(kv, "GIT_DIR=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want %s in %v", want, env)
	}
}

// The push names its URL on the command line, and git rewrites that too: a
// url.*.insteadOf rule matching it would send the token's header to the
// rule's host (strix vuln-0001). The push is refused and nothing is sent;
// a rule for other addresses is left alone.
func TestPushRefusesAURLRewriteOfThePushURL(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer collector.Close()
	forge := "https://forge.invalid/globex/shop.git"

	dir := guardTestClone(t)
	agentGit(t, dir, "config", "url."+collector.URL+"/.insteadOf", "https://forge.invalid/")
	err := pushBranch(context.Background(), dir, forge, "github_pat_abc", "github", "agent/x", "agent/", []string{"main"})
	if err == nil || !strings.Contains(err.Error(), "rewrites the push URL") {
		t.Fatalf("want the push refused for the rewrite, got %v", err)
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("the push reached the rule's host: %v", seen)
	}

	// A rule for other addresses: the push goes ahead (to the collector
	// directly here, which refuses it — the request is what counts).
	other := guardTestClone(t)
	agentGit(t, other, "config", "url.https://example.invalid/.insteadOf", "git@example.invalid:")
	_ = pushBranch(context.Background(), other, collector.URL+"/globex/shop.git", "github_pat_abc", "github", "agent/x", "agent/", []string{"main"})
	mu.Lock()
	n = len(seen)
	mu.Unlock()
	if n == 0 {
		t.Fatal("a rule for other addresses stopped the push")
	}
}

// A changed clone holds the workspace back on this worker (the claim's
// unhealthy list), so its runs go to other workers instead of failing
// here one after another; it stays held back until the worker restarts.
func TestChangedCloneMakesTheWorkspaceUnhealthy(t *testing.T) {
	clone := guardTestClone(t)
	registerClone(clone)
	cfg := &Config{Repos: map[string]*Repo{"ws": {Path: clone}}, Workspaces: []string{"ws"}}
	w := &Worker{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if why := w.cloneGuardProblem("ws"); why != "" {
		t.Fatalf("an untouched clone: %s", why)
	}
	agentGit(t, clone, "config", "core.sshCommand", "sh -c 'id > /tmp/x'")
	w.guarded["ws"] = preflightResult{} // look again now, not in a minute
	why := w.cloneGuardProblem("ws")
	if !strings.Contains(why, "core.sshcommand") {
		t.Fatalf("want the workspace held back naming core.sshcommand, got %q", why)
	}
	// Put back: still held back — a person restarts the worker after
	// looking.
	agentGit(t, clone, "config", "--unset", "core.sshCommand")
	if again := w.cloneGuardProblem("ws"); again == "" {
		t.Fatal("a clone that was changed must stay held back until the worker restarts")
	}
}

// With checkout = "branch" the agent can write the clone's git directory
// while it runs: no push is made then, only the final one after it exited.
func TestNoStepPushesWithBranchCheckout(t *testing.T) {
	clone := guardTestClone(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	agentGit(t, filepath.Dir(origin), "init", "-q", "--bare", origin)
	old := stepPushEvery
	stepPushEvery = 20 * time.Millisecond
	defer func() { stepPushEvery = old }()
	t.Setenv("PUSH_TOKEN", "t")
	repo := &Repo{Path: clone, BranchPrefix: "agent/", Protected: []string{"main"}, Checkout: checkoutBranch}
	repo.Push.Kind, repo.Push.RemoteURL, repo.Push.TokenEnv = "gitea", origin, "PUSH_TOKEN"
	w := &Worker{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	stop := w.pushSteps(context.Background(), repo, clone, "agent/x", w.log)
	agentGit(t, clone, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "step 1")
	time.Sleep(200 * time.Millisecond) // ten ticks
	stop()
	if out, _ := exec.Command("git", "-C", origin, "rev-parse", "--verify", "--quiet", "refs/heads/agent/x").Output(); len(out) != 0 {
		t.Fatal("a step push was made while the agent could write the clone")
	}
}
