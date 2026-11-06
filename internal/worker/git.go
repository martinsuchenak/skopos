package worker

import (
	"context"
	"encoding/base64"
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

// reBranch is the character set of a branch name; validBranch adds git's
// structural rules.
var reBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

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

// createWorktreeAt cuts a worktree on a new branch from an explicit ref
// (the approved base_sha — review H3), no upstream.
func createWorktreeAt(ctx context.Context, clone, worktree, branch, ref string) error {
	if !validBranch(branch) {
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

// implementStart says where an implementation's branch starts. An earlier
// round's branch wins over the base, so a re-implementation continues the
// branch its pull request shows and pushes fast-forward (never force): a
// restored item whose worktree the cleanup removed, a wiped state dir,
// another worker's round, or a branch someone else started (a handover).
// First the local branch (local) — fast-forwarded to origin's when origin
// has moved on from it (another worker pushed a later round) — then the one
// pushed to origin (ref = its commit), else ref = the approved base, fetched
// when the clone doesn't have it (planned on another worker).
//
// The approved base must be in the start's history: a branch rewritten
// since the plan was approved (force-pushed, rebased) is refused rather
// than continued from something nobody approved.
func implementStart(ctx context.Context, clone, branch, base string) (local bool, ref string, err error) {
	if !validBranch(branch) {
		return false, "", fmt.Errorf("refusing branch name %q", branch)
	}
	// A worktree directory removed by hand stays registered and would
	// block checking its branch out again.
	_, _ = git(ctx, clone, "worktree", "prune")
	localSHA, lerr := git(ctx, clone, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	// Into its own ref, not FETCH_HEAD: a fetch by another worker process
	// sharing the clone would overwrite that between the two commands.
	originSHA, _ := fetchBranch(ctx, clone, branch)
	if _, err := git(ctx, clone, "cat-file", "-e", base+"^{commit}"); err != nil {
		if _, ferr := git(ctx, clone, "fetch", "--quiet", "origin"); ferr != nil {
			return false, "", fmt.Errorf("base ref %q not found: %w", base, err)
		}
	}
	start := base
	switch {
	case lerr == nil:
		local, start = true, localSHA
		if originSHA != "" && originSHA != localSHA && isAncestor(ctx, clone, localSHA, originSHA) {
			// The branch isn't checked out anywhere here (a fresh worktree,
			// or a clone parked on its default branch).
			if _, err := git(ctx, clone, "update-ref", "refs/heads/"+branch, originSHA, localSHA); err != nil {
				return false, "", fmt.Errorf("fast-forwarding %s to origin: %w", branch, err)
			}
			start = originSHA
		}
		ref = branch
	case originSHA != "":
		start, ref = originSHA, originSHA
	default:
		ref = base
	}
	// Only a pinned base (a commit id) binds: a run without one starts
	// from origin/<default>, which moves on.
	if start != base && reSHA.MatchString(base) && !isAncestor(ctx, clone, base, start) {
		return false, "", fmt.Errorf("branch %s no longer contains the approved base %s: it was rewritten since the plan was approved; plan the item again", branch, shortSHA(base))
	}
	return local, ref, nil
}

var reSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// isAncestor reports whether a is in b's history.
func isAncestor(ctx context.Context, clone, a, b string) bool {
	_, err := git(ctx, clone, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// prepareImplementWorktree cuts the implementation worktree where
// implementStart says the branch starts.
func prepareImplementWorktree(ctx context.Context, clone, worktree, branch, base string) error {
	local, ref, err := implementStart(ctx, clone, branch, base)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return err
	}
	if local {
		_, err = git(ctx, clone, "worktree", "add", "--quiet", worktree, branch)
	} else {
		_, err = git(ctx, clone, "worktree", "add", "--quiet", "--no-track", "-b", branch, worktree, ref)
	}
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
	if !validBranch(branch) {
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
	_, err = gitEnv(ctx, worktree, []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=" + pushAuthHeader(authKind, token),
		"GIT_CONFIG_KEY_1=http.sslVerify", "GIT_CONFIG_VALUE_1=true",
	}, "push", remoteURL, "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

// pushAuthHeader is the HTTP header that carries the push token.
//   - github: HTTP Basic with the token as the password and the
//     conventional user x-access-token. GitHub's git-over-HTTPS rejects a
//     Bearer header ("remote: invalid credentials", found on the first
//     live push); Basic works for classic and fine-grained personal
//     access tokens and GitHub App installation tokens alike.
//   - gitea (and the default): Gitea's "token <t>" scheme.
func pushAuthHeader(kind, token string) string {
	if kind == "github" {
		return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	}
	return "Authorization: token " + token
}

// commitCount and diffStat measure the branch against its approved base
// (a commit), not the clone's origin/<default>, which lags on a worker that
// didn't plan the item.
func commitCount(ctx context.Context, worktree, base string) int {
	s, err := git(ctx, worktree, "rev-list", "--count", base+"..HEAD")
	if err != nil {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(s, "%d", &n)
	return n
}

func diffStat(ctx context.Context, worktree, base string) string {
	s, _ := git(ctx, worktree, "diff", "--stat", base+"..HEAD")
	return strings.TrimSpace(s)
}

// runSetup runs the repo's setup commands in a fresh worktree. They get the
// scrubbed environment (review: they inherited every daemon secret — env
// file, skopos key, push tokens, SSH agent) plus the repo's setup_env, and
// the setup timeout bounds them all.
//
// Each command's result is returned in the checks' shape (the dashboard and
// the run record show them); the first failure stops the rest and is also
// returned as the error.
func runSetup(ctx context.Context, worktree string, setup []string, clone string, setupEnv map[string]string, tokenEnvs, fileEnvKeys []string, timeout time.Duration) ([]Check, error) {
	if len(setup) == 0 {
		return nil, nil
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	env := scrubEnv(setupEnv, tokenEnvs, fileEnvKeys)
	var results []Check
	for _, c := range setup {
		c = strings.ReplaceAll(c, "{clone}", clone)
		cmd := exec.CommandContext(sctx, "sh", "-c", c)
		cmd.Dir = worktree
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		results = append(results, Check{Name: c, Passed: err == nil, Output: tail(string(out), 2000)})
		if err != nil {
			return results, fmt.Errorf("setup %q failed: %v: %s", c, err, tail(string(out), 300))
		}
	}
	return results, nil
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

// instructionPaths are the files and directories agent CLIs auto-load from
// a working directory as instructions or config (CLAUDE.md, AGENTS.md,
// opencode.json, .claude/, .opencode/, skills, MCP configs, ...).
var instructionPaths = []string{
	"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", "GEMINI.md", ".skopos",
	".claude", ".mcp.json", ".opencode", "opencode.json", "opencode.jsonc",
	".gemini", ".codex", ".agents", ".cursor", ".cursorrules", ".github/copilot-instructions.md",
}

// restoreInstructionFiles resets every instruction/config path in a REVIEW
// worktree to its version at base (removing what the branch added).
// Runner-agnostic defense: the branch under review was written by the
// implementer, and any CLI that auto-loads these files would otherwise take
// the implementer's instructions as the reviewer's. The reviewed diff is
// commit-based (origin/<base>...HEAD), so the branch's changes to these
// files still show up in the review.
func restoreInstructionFiles(ctx context.Context, wt, base string) error {
	for _, p := range instructionPaths {
		if err := os.RemoveAll(filepath.Join(wt, p)); err != nil {
			return err
		}
		if _, err := git(ctx, wt, "cat-file", "-e", base+":"+p); err != nil {
			continue // absent at base: stays removed
		}
		if _, err := git(ctx, wt, "checkout", base, "--", p); err != nil {
			return err
		}
	}
	return nil
}
