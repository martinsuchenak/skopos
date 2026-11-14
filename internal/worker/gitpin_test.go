package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An agent that works in a worktree can rewrite the two files that tell git
// where the repository is. The worker's own git commands there must not
// follow them: they would read config and hooks from a directory the agent
// made, outside any wrapper.
func TestWorkerGitIgnoresATamperedWorktreePointer(t *testing.T) {
	ctx := context.Background()
	clone, base := gitRepo(t)
	state := t.TempDir()
	root := filepath.Join(state, "worktrees", filepath.Base(clone))
	registerWorktreeRoot(root, clone)
	wt := filepath.Join(root, "agent-item-1")
	if err := createDetached(ctx, clone, wt, base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeWorktree(ctx, clone, wt) })
	realCommon, err := cloneGitDir(clone)
	if err != nil {
		t.Fatal(err)
	}
	raw := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wt
		out, _ := cmd.CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	if got := raw("rev-parse", "--path-format=absolute", "--git-common-dir"); !samePath(got, realCommon) {
		t.Fatalf("before: %s, want %s", got, realCommon)
	}

	// The agent's own repository, with a file system monitor that leaves a
	// mark when any git command looks at the tree.
	evil := filepath.Join(t.TempDir(), "evil")
	marker := filepath.Join(t.TempDir(), "ran-outside-the-wrapper")
	hook := filepath.Join(t.TempDir(), "monitor.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", evil},
		{"-C", evil, "config", "core.fsmonitor", hook},
		{"-C", evil, "config", "user.email", "agent@example.com"},
		{"-C", evil, "config", "user.name", "agent"},
		{"-C", evil, "commit", "-q", "--allow-empty", "-m", "evil"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	// 1. The worktree's .git pointer now names the agent's repository.
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(evil, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := raw("rev-parse", "--path-format=absolute", "--git-common-dir"); samePath(got, realCommon) {
		t.Fatal("the test's tampering did not take: plain git still finds the clone")
	}
	_ = raw("status", "--short")
	if _, err := os.Stat(marker); err != nil {
		t.Skipf("this git does not run core.fsmonitor from the repository's config (%v): nothing to show", err)
	}
	_ = os.Remove(marker)

	// The worker's commands name the repository themselves.
	if got := gitCommonDir(wt); !samePath(got, realCommon) {
		t.Fatalf("the wrapper would bind %s, want the clone's %s", got, realCommon)
	}
	if _, err := git(ctx, wt, "status", "--short"); err != nil {
		t.Fatalf("status in the worktree: %v", err)
	}
	if head, err := git(ctx, wt, "rev-parse", "HEAD"); err != nil || head != base {
		t.Fatalf("the worker reads the real worktree's head: %q %v (want %s)", head, err, base)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a worker git command ran the agent's configured command")
	}

	// 2. The same through the worktree's own "commondir" file.
	admin, err := git(ctx, wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte(filepath.Join(evil, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitCommonDir(wt); !samePath(got, realCommon) {
		t.Fatalf("after commondir was rewritten the wrapper would bind %s", got)
	}
	if _, err := git(ctx, wt, "status", "--short"); err != nil {
		t.Fatalf("status after commondir was rewritten: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a worker git command ran the agent's configured command through commondir")
	}

	// A worktree whose git directory cannot be told from the clone is not
	// trusted: after a restart nothing is remembered, and an agent that
	// also rewrote the clone's record of it gets an error, not a command.
	forgetWorktreePin(wt)
	if err := os.WriteFile(filepath.Join(admin, "gitdir"), []byte("/somewhere/else/.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, wt, "status", "--short"); err == nil || !strings.Contains(err.Error(), "cannot be told from its clone") {
		t.Fatalf("an unknown worktree must not be run on trust: %v", err)
	}
}
