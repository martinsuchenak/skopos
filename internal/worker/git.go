package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/martinsuchenak/skopos/internal/srvurl"
)

// Git helpers, extracted from agent-trial's proven git.go. The push path is
// the ONLY code that pushes: explicit refspec, validated destination, token
// through env-only git config (never argv), never force.

// reBranch is the character set of a branch name; validBranch adds git's
// structural rules.
var reBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd, err := gitCommand(ctx, dir, nil, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %w: %s", args[0], err, s)
	}
	return s, nil
}

func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd, err := gitCommand(ctx, dir, env, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
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
// A local branch and origin's that have both moved on (an interrupted round
// left commits here while another worker pushed its own) is what a
// developer pulls: merge = origin's commit, which the caller merges once the
// branch is checked out (mergeOrigin), failing only when it doesn't merge.
//
// The approved base must be in the start's history: a branch rewritten
// since the plan was approved (force-pushed, rebased) is refused rather
// than continued from something nobody approved.
func implementStart(ctx context.Context, clone, branch, base string) (local bool, ref, merge string, err error) {
	if !validBranch(branch) {
		return false, "", "", fmt.Errorf("refusing branch name %q", branch)
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
			return false, "", "", fmt.Errorf("base ref %q not found: %w", base, err)
		}
	}
	start := base
	switch {
	case lerr == nil:
		local, start = true, localSHA
		switch {
		case originSHA == "" || originSHA == localSHA || isAncestor(ctx, clone, originSHA, localSHA):
			// Nothing pushed, or local is ahead (a paused round's commits).
		case isAncestor(ctx, clone, localSHA, originSHA):
			// The branch isn't checked out anywhere here (a fresh worktree,
			// or a clone parked on its default branch).
			if _, err := git(ctx, clone, "update-ref", "refs/heads/"+branch, originSHA, localSHA); err != nil {
				return false, "", "", fmt.Errorf("fast-forwarding %s to origin: %w", branch, err)
			}
			start = originSHA
		default:
			// Both moved on: the merge brings origin's history in, so the
			// approved base is checked against that.
			merge, start = originSHA, originSHA
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
		return false, "", "", needsPerson(fmt.Errorf("branch %s no longer contains the approved base %s: it was rewritten since the plan was approved; plan the item again", branch, shortSHA(base)))
	}
	return local, ref, merge, nil
}

// pullWorktree brings an existing worktree's branch up to origin's, as
// implementStart does for a new one: fast-forward when behind, merge when
// both moved on (a conflict needs a person), nothing when origin has no
// such branch or is behind. A worktree with uncommitted changes (a pause
// mid-turn) is left alone: a merge must never mix with them.
func pullWorktree(ctx context.Context, clone, worktree, branch string) error {
	if dirty, err := git(ctx, worktree, "status", "--porcelain"); err != nil || dirty != "" {
		return nil
	}
	origin, err := fetchBranch(ctx, clone, branch)
	if err != nil || origin == "" {
		return nil // not on origin (never pushed), or unreachable: the push says
	}
	head := headSHA(ctx, worktree)
	switch {
	case head == origin || isAncestor(ctx, worktree, origin, head):
		return nil
	case isAncestor(ctx, worktree, head, origin):
		_, err := git(ctx, worktree, "merge", "--ff-only", "--quiet", origin)
		return err
	}
	return mergeOrigin(ctx, worktree, branch, origin)
}

// mergeOrigin merges origin's commit into the checked-out branch in dir, as
// a developer's pull would: the run then continues, and later pushes, on
// top of both. dir is clean (a fresh worktree, or the clone just switched
// from its clean default branch), so aborting a conflicted merge puts it
// back exactly. A conflict fails the run: two rounds of the same work
// rarely merge, and only a person can say which one stays.
func mergeOrigin(ctx context.Context, dir, branch, origin string) error {
	var env []string
	if name, _ := git(ctx, dir, "config", "user.name"); name == "" {
		// The merge commit needs a committer; the agent's commits take the
		// clone's identity when it has one.
		env = []string{"GIT_AUTHOR_NAME=skopos worker", "GIT_AUTHOR_EMAIL=skopos-worker@localhost",
			"GIT_COMMITTER_NAME=skopos worker", "GIT_COMMITTER_EMAIL=skopos-worker@localhost"}
	}
	msg := fmt.Sprintf("Merge origin/%s into %s", branch, branch)
	if _, err := gitEnv(ctx, dir, env, "merge", "--no-edit", "-m", msg, origin); err != nil {
		conflicts, _ := git(ctx, dir, "diff", "--name-only", "--diff-filter=U")
		_, _ = git(ctx, dir, "merge", "--abort")
		if conflicts == "" {
			return fmt.Errorf("merging origin's %s (%s) into the local branch: %w", branch, shortSHA(origin), err)
		}
		return needsPerson(fmt.Errorf("the local branch %s and origin's (%s) have both moved on and don't merge cleanly (conflicts: %s); keep one of them — push the one you want to origin and delete the other, or delete the worker's local branch to continue from origin's — then retry",
			branch, shortSHA(origin), strings.Join(strings.Fields(conflicts), ", ")))
	}
	return nil
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
	local, ref, merge, err := implementStart(ctx, clone, branch, base)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		return err
	}
	if !local {
		_, err = git(ctx, clone, "worktree", "add", "--quiet", "--no-track", "-b", branch, worktree, ref)
		return err
	}
	if _, err := git(ctx, clone, "worktree", "add", "--quiet", worktree, branch); err != nil {
		return err
	}
	if merge != "" {
		if err := mergeOrigin(ctx, worktree, branch, merge); err != nil {
			removeWorktree(ctx, clone, worktree)
			return err
		}
	}
	return nil
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
		forgetWorktreePin(worktree)
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
	// git can't look SRV up: a srv+ remote is resolved for this push.
	if remoteURL, err = srvurl.Resolve(ctx, remoteURL); err != nil {
		return fmt.Errorf("push: %w", err)
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
func runSetup(ctx context.Context, worktree string, setup []string, clone string, setupEnv map[string]string, tokenEnvs, fileEnvKeys []string, lim runLimits) ([]Check, error) {
	if len(setup) == 0 {
		return nil, nil
	}
	sctx, cancel := context.WithTimeout(ctx, lim.total)
	defer cancel()
	env := scrubEnv(setupEnv, tokenEnvs, fileEnvKeys)
	var results []Check
	rl := runLogFrom(ctx)
	for _, c := range setup {
		c = strings.ReplaceAll(c, "{clone}", clone)
		rl.line("setup", "$ "+c, false)
		out, err := runShell(withLogSrc(sctx, "setup"), worktree, env, c, lim.quiet)
		if errors.Is(sctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("setup_timeout (%s) reached; the command was stopped", lim.total)
		}
		logCommandResult(rl, "setup", err)
		results = append(results, Check{Name: c, Passed: err == nil, Output: tail(string(out), 2000)})
		if err != nil {
			return results, fmt.Errorf("setup %q failed: %v: %s", c, err, tail(string(out), 300))
		}
	}
	return results, nil
}

// runLimits bounds a list of setup or check commands: total for all of
// them together, quiet for any one command printing nothing.
type runLimits struct{ total, quiet time.Duration }

func runChecks(ctx context.Context, worktree string, checks []string, tokenEnvs, fileEnvKeys []string, lim runLimits) []Check {
	cctx, cancel := context.WithTimeout(ctx, lim.total)
	defer cancel()
	var out []Check
	for _, c := range checks {
		if cctx.Err() != nil {
			out = append(out, Check{Name: c, Stopped: true, Output: fmt.Sprintf("not run: check_timeout (%s) reached", lim.total)})
			continue
		}
		rl := runLogFrom(ctx)
		rl.line("check", "$ "+c, false)
		outBytes, err := runShell(withLogSrc(cctx, "check"), worktree, scrubEnv(nil, tokenEnvs, fileEnvKeys), c, lim.quiet)
		logCommandResult(rl, "check", err)
		text := tail(string(outBytes), 2000)
		stopped := false
		var qe *quietError
		switch {
		case err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded):
			text, stopped = tail(text+fmt.Sprintf("\n\n(stopped: check_timeout (%s) reached)", lim.total), 2000), true
		case errors.As(err, &qe):
			text, stopped = tail(text+"\n\n(stopped: "+qe.Error()+")", 2000), true
		}
		out = append(out, Check{Name: c, Passed: err == nil, Stopped: stopped, Output: text})
	}
	return out
}

// quietError: the command printed nothing for too long and was stopped.
type quietError struct{ after time.Duration }

func (e *quietError) Error() string {
	return fmt.Sprintf("no output for %s (quiet_timeout); the command was stopped", e.after)
}

// quietWriter collects a command's output and when it last wrote.
type quietWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	last time.Time
}

func (q *quietWriter) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.last = time.Now()
	return q.buf.Write(p)
}

func (q *quietWriter) silentFor() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return time.Since(q.last)
}

func (q *quietWriter) bytes() []byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	return bytes.Clone(q.buf.Bytes())
}

// processWaitDelay: how long Wait waits for a command's pipes after its
// top process has gone (containProcess).
var processWaitDelay = 10 * time.Second

// runShell runs one setup or check command line under sh, contained: a
// timeout stops its whole process tree, and output pipes a background
// child keeps open never block the worker.
func runShell(ctx context.Context, dir string, env []string, line string, quiet time.Duration) ([]byte, error) {
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	name, args := "sh", []string{"-c", line}
	if wrap := shellWrapFrom(ctx); wrap != nil {
		// A wrapped stage: the command runs behind the stage's wrapper,
		// like the agent (it executes the branch's code).
		prefix, err := wrap(dir)
		if err != nil {
			return nil, err
		}
		name, args = prefix[0], append(append([]string{}, prefix[1:]...), "sh", "-c", line)
	}
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	containProcess(cmd)
	out := &quietWriter{last: time.Now()}
	cmd.Stdout, cmd.Stderr = out, out
	if rl := runLogFrom(ctx); rl != nil {
		// Live in the run log too (setup and checks; preflight has none).
		live := rl.writer(logSrcFrom(ctx), false, nil)
		cmd.Stdout = io.MultiWriter(out, live)
		cmd.Stderr = cmd.Stdout
		defer live.Flush()
	}
	done := make(chan struct{})
	if quiet > 0 {
		// A command that prints nothing for quiet is presumed hung (waiting
		// on something that never comes): stopped with its process group,
		// long before the setup or check timeout would.
		tick := quiet / 10
		if tick > 5*time.Second {
			tick = 5 * time.Second
		}
		go func() {
			t := time.NewTicker(tick)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if out.silentFor() >= quiet {
						cancel(&quietError{after: quiet})
						return
					}
				}
			}
		}()
	}
	err := settled(cmd, cmd.Run())
	close(done)
	var qe *quietError
	if cause := context.Cause(cctx); err != nil && errors.As(cause, &qe) {
		err = qe
	}
	return out.bytes(), err
}

func settled(cmd *exec.Cmd, err error) error {
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		return nil
	}
	return err
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

// restoreTrackedInstructionFiles is restoreInstructionFiles for a clone a
// person also works in: only what git tracks goes back to base — files the
// branch added under these paths are removed, the ones it changed or
// deleted are restored — and local files git does not track (an editor's
// or an agent's own settings) stay. The caller resets the clone afterwards.
func restoreTrackedInstructionFiles(ctx context.Context, dir, base string) error {
	for _, p := range instructionPaths {
		added, err := git(ctx, dir, "diff", "--name-only", "--diff-filter=A", base, "HEAD", "--", p)
		if err != nil {
			return err
		}
		for _, f := range strings.Split(added, "\n") {
			if f = strings.TrimSpace(f); f == "" {
				continue
			}
			if _, err := git(ctx, dir, "rm", "--quiet", "-f", "--", f); err != nil {
				return err
			}
		}
		if _, err := git(ctx, dir, "cat-file", "-e", base+":"+p); err != nil {
			continue // absent at base
		}
		if _, err := git(ctx, dir, "checkout", base, "--", p); err != nil {
			return err
		}
	}
	return nil
}

type logSrcKey struct{}

// withLogSrc names the run-log source a command's output goes under.
func withLogSrc(ctx context.Context, src string) context.Context {
	return context.WithValue(ctx, logSrcKey{}, src)
}

func logSrcFrom(ctx context.Context) string {
	if s, ok := ctx.Value(logSrcKey{}).(string); ok {
		return s
	}
	return "worker"
}

// logCommandResult ends a setup or check command's lines in the run log.
func logCommandResult(rl *runLog, src string, err error) {
	if err != nil {
		rl.line(src, "✗ "+err.Error(), true)
		return
	}
	rl.line(src, "✓ passed", false)
}

// stepCommit matches the implementer's commit subjects: "agent: step N -
// <step title>" (implement.md), after any prefix a commit-msg hook adds.
var stepCommit = regexp.MustCompile(`agent: step \d+ - (.+)$`)

// committedSteps marks the steps whose commit is on the branch since base:
// an earlier run committed them (one cut off by its timeout, a crash, a
// retry). A commit names its step by title, since an amendment renumbers
// the steps; a title only counts once.
func committedSteps(ctx context.Context, dir, base string, steps []Step) []bool {
	done := make([]bool, len(steps))
	out, err := git(ctx, dir, "log", "--format=%s", base+"..HEAD")
	if err != nil {
		return done
	}
	titles := map[string]int{}
	for line := range strings.Lines(out) {
		if m := stepCommit.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			titles[normTitle(m[1])]++
		}
	}
	for i, st := range steps {
		if k := normTitle(st.Title); titles[k] > 0 {
			done[i] = true
			titles[k]--
		}
	}
	return done
}

// normTitle keeps only a title's letters and digits, lowercased: the
// implementer leaves shell-special characters out of commit messages (the
// prompt asks it to — opencode refuses `/api`, `->`, `$` in a command), so
// "Add /api/inbox route" is committed as "Add api/inbox route".
func normTitle(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// leftoverChanges lists what an earlier run left uncommitted in dir (git
// status --short, at most 20 lines): a run killed mid-step keeps its edits
// for the next one.
func leftoverChanges(ctx context.Context, dir string) []string {
	// --branch puts a "## branch" line first, so git()'s trimming can't eat
	// the first file's status column (" M" vs "M ").
	out, err := git(ctx, dir, "status", "--short", "--branch", "--untracked-files=all")
	if err != nil {
		return nil
	}
	var files []string
	for line := range strings.Lines(out) {
		if line = strings.TrimRight(line, "\n"); line != "" && !strings.HasPrefix(line, "## ") {
			files = append(files, line)
		}
	}
	if len(files) > 20 {
		files = append(files[:20], fmt.Sprintf("… and %d more", len(files)-20))
	}
	return files
}

// clearStaleIndexLock removes the index.lock a git command killed with its
// agent left behind; git refuses every later command while it exists. The
// run holds the item's worktree (and a branch checkout's clone) alone, and
// the killed run's process group is gone, so nothing else owns the lock.
func clearStaleIndexLock(ctx context.Context, dir string) bool {
	gitDir, err := git(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return false
	}
	lock := filepath.Join(strings.TrimSpace(gitDir), "index.lock")
	if _, err := os.Stat(lock); err != nil {
		return false
	}
	return os.Remove(lock) == nil
}
