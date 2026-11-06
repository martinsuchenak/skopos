package worker

import (
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The step pusher sends each new commit to origin while the implementer
// works, and stops on request; push.steps = false turns it off.
func TestPushSteps(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	wt := filepath.Join(root, "wt")
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(root, "init", "-q", "--bare", origin)
	run(root, "init", "-q", "-b", "main", wt)
	run(wt, "commit", "-q", "--allow-empty", "-m", "base")
	run(wt, "checkout", "-q", "-b", "agent/x")

	old := stepPushEvery
	stepPushEvery = 50 * time.Millisecond
	defer func() { stepPushEvery = old }()
	t.Setenv("PUSH_TOKEN", "t")
	repo := &Repo{Path: wt, BranchPrefix: "agent/", Protected: []string{"main"}}
	repo.Push.Kind, repo.Push.RemoteURL, repo.Push.TokenEnv = "gitea", origin, "PUSH_TOKEN"
	w := &Worker{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	originHead := func() string {
		out, _ := exec.Command("git", "-C", origin, "rev-parse", "--verify", "--quiet", "refs/heads/agent/x").Output()
		return strings.TrimSpace(string(out))
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for originHead() != want {
			if time.Now().After(deadline) {
				t.Fatalf("origin has %q, want %q", originHead(), want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	stop := w.pushSteps(t.Context(), repo, wt, "agent/x", w.log)
	run(wt, "commit", "-q", "--allow-empty", "-m", "agent: step 1")
	waitFor(run(wt, "rev-parse", "HEAD"))
	run(wt, "commit", "-q", "--allow-empty", "-m", "agent: step 2")
	waitFor(run(wt, "rev-parse", "HEAD"))
	stop()
	stop() // idempotent
	pushed := originHead()
	run(wt, "commit", "-q", "--allow-empty", "-m", "after stop")
	time.Sleep(200 * time.Millisecond)
	if originHead() != pushed {
		t.Fatal("nothing is pushed after stop")
	}

	off := false
	repo.Push.Steps = &off
	stop = w.pushSteps(t.Context(), repo, wt, "agent/x", w.log)
	run(wt, "commit", "-q", "--allow-empty", "-m", "steps off")
	time.Sleep(200 * time.Millisecond)
	stop()
	if originHead() != pushed {
		t.Fatal("push.steps = false must not push")
	}
}
