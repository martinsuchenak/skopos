package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Git helpers, extracted from agent-trial's proven git.go. The push path is
// the ONLY code that pushes: explicit refspec, validated destination, token
// through env-only git config (never argv), never force.

var reBranch = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %w: %s", args[0], err, s)
	}
	return s, nil
}

func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %w: %s", args[0], err, s)
	}
	return s, nil
}

func isProtected(branch string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/**") && strings.HasPrefix(branch, strings.TrimSuffix(p, "**")) {
			return true
		}
		if ok, _ := path.Match(p, branch); ok {
			return true
		}
	}
	return false
}

// createWorktree cuts a fresh worktree on a new branch from origin/<base>,
// no upstream — a tracking misconfiguration cannot redirect a push.
func createWorktree(ctx context.Context, clone, worktree, branch, base string) error {
	if !reBranch.MatchString(branch) {
		return fmt.Errorf("refusing branch name %q", branch)
	}
	if _, err := git(ctx, clone, "fetch", "--quiet", "origin", base); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return err
	}
	_, err := git(ctx, clone, "worktree", "add", "--quiet", "--no-track", "-b", branch, worktree, "origin/"+base)
	return err
}

// createWorktreeAt cuts a worktree on a new branch from an explicit ref
// (the approved base_sha — review H3), no upstream.
func createWorktreeAt(ctx context.Context, clone, worktree, branch, ref string) error {
	if !reBranch.MatchString(branch) {
		return fmt.Errorf("refusing branch name %q", branch)
	}
	if _, err := git(ctx, clone, "cat-file", "-e", ref); err != nil {
		// Not local: fetch the branch refs so a base_sha/branch from origin resolves.
		if _, ferr := git(ctx, clone, "fetch", "--quiet", "origin"); ferr != nil {
			return fmt.Errorf("base ref %q not found: %w", ref, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return err
	}
	_, err := git(ctx, clone, "worktree", "add", "--quiet", "--no-track", "-b", branch, worktree, ref)
	return err
}

// createDetached checks out a ref without creating a branch (review stage).
func createDetached(ctx context.Context, clone, worktree, ref string) error {
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return err
	}
	_, err := git(ctx, clone, "worktree", "add", "--quiet", "--detach", worktree, ref)
	return err
}

func removeWorktree(ctx context.Context, clone, worktree string) {
	if worktree != "" {
		_, _ = git(ctx, clone, "worktree", "remove", "--force", worktree)
	}
}

func headSHA(ctx context.Context, dir string) string {
	s, _ := git(ctx, dir, "rev-parse", "HEAD")
	return s
}

// pushBranch: validated destination, explicit refspec, token via env-only
// git config, never force. remoteAuthKind selects the header flavor.
func pushBranch(ctx context.Context, worktree, remoteURL, token, authKind, branch, prefix string, protected []string) error {
	if !reBranch.MatchString(branch) {
		return fmt.Errorf("push refused: %q is not a safe branch name", branch)
	}
	if prefix != "" && !strings.HasPrefix(branch, prefix) {
		return fmt.Errorf("push refused: %q does not carry the configured prefix %q", branch, prefix)
	}
	if isProtected(branch, protected) {
		return fmt.Errorf("push refused: %q matches a protected pattern", branch)
	}
	cur, err := git(ctx, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if cur != branch {
		return fmt.Errorf("push refused: HEAD is %q, refusing to push %q", cur, branch)
	}
	header := "Authorization: token " + token
	if authKind == "github" {
		header = "Authorization: Bearer " + token
	}
	_, err = gitEnv(ctx, worktree, []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=" + header,
		"GIT_CONFIG_KEY_1=http.sslVerify", "GIT_CONFIG_VALUE_1=true",
	}, "push", remoteURL, "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

func commitCount(ctx context.Context, worktree, base string) int {
	s, err := git(ctx, worktree, "rev-list", "--count", "origin/"+base+"..HEAD")
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(s, "%d", &n)
	return n
}

func diffStat(ctx context.Context, worktree, base string) string {
	s, _ := git(ctx, worktree, "diff", "--stat", "origin/"+base+"..HEAD")
	return strings.TrimSpace(s)
}

func isDirty(ctx context.Context, worktree string) bool {
	s, _ := git(ctx, worktree, "status", "--porcelain")
	return strings.TrimSpace(s) != ""
}

func runSetup(ctx context.Context, worktree string, setup []string, clone string, timeout time.Duration) error {
	for _, c := range setup {
		c = strings.ReplaceAll(c, "{clone}", clone)
		cmd := exec.CommandContext(ctx, "sh", "-c", c)
		cmd.Dir = worktree
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("setup %q failed: %v: %s", c, err, tail(string(out), 300))
		}
	}
	return nil
}

func runChecks(ctx context.Context, worktree string, checks []string, tokenEnvs, fileEnvKeys []string) []Check {
	var out []Check
	for _, c := range checks {
		cmd := exec.CommandContext(ctx, "sh", "-c", c)
		cmd.Dir = worktree
		cmd.Env = scrubEnv(nil, tokenEnvs, fileEnvKeys)
		outBytes, err := cmd.CombinedOutput()
		out = append(out, Check{Name: c, Passed: err == nil, Output: tail(string(outBytes), 2000)})
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
