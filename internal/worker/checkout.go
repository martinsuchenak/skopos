package worker

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Branch checkout (repo checkout = "branch"): the implementation runs in the
// clone itself instead of a worktree beside it, for a repo whose checkout
// can't be duplicated (local configuration, a database the setup migrates).
// The clone is held for the whole run — a lock against other worker
// processes on the host — switched to the item's branch, and put back on
// the default branch when the run ends, pushed, failed or paused. Whatever
// the run left uncommitted is stashed under the branch's label, and the
// item's next run applies it again. Setup runs again only when the clone
// was set up for something else since (cloneSetup).

const (
	checkoutWorktree = "worktree"
	checkoutBranch   = "branch"
)

// stashLabel is the stash message for what a run left uncommitted on an
// item's branch (branch names are per item: normalizeBranch).
func stashLabel(branch string) string { return "skopos:" + branch }

// cloneLockPath is the lock file in the clone's git directory.
func cloneLockPath(ctx context.Context, clone string) (string, error) {
	dir, err := git(ctx, clone, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "skopos-checkout.lock"), nil
}

// tryLockClone takes the clone's lock if it is free; ok is false while
// another worker process holds it.
func tryLockClone(ctx context.Context, clone string) (release func(), ok bool, err error) {
	path, err := cloneLockPath(ctx, clone)
	if err != nil {
		return nil, false, err
	}
	f, ok, err := tryLockFile(path)
	if err != nil || !ok {
		return nil, false, err
	}
	return func() { unlockFile(f) }, true, nil
}

// holdClone takes the clone's lock, waiting while another worker process
// holds it, and makes sure the clone is on its default branch with nothing
// uncommitted (readyClone). The caller releases it.
func (w *Worker) holdClone(ctx context.Context, repo *Repo, log *slog.Logger) (func(), error) {
	var waited func()
	for {
		release, ok, err := tryLockClone(ctx, repo.Path)
		if err != nil {
			return nil, fmt.Errorf("locking the clone: %w", err)
		}
		if ok {
			if waited != nil {
				waited()
			}
			if err := w.readyClone(ctx, repo, log); err != nil {
				release()
				return nil, err
			}
			return release, nil
		}
		if waited == nil {
			who := "another worker process"
			if w.indexRunning() {
				who = "this worker's code indexing (stopping)"
			}
			log.Info("waiting for the clone: "+who+" is using it", "clone", repo.Path)
			waited = runLogFrom(ctx).stepf("waiting for the clone %s: %s is using it", repo.Path, who)
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return nil, ctx.Err()
		}
	}
}

// readyClone puts the clone back on its default branch before a run. A
// clean clone on another branch is switched back. Uncommitted changes are
// stashed for the item's next run only when a run of this worker left them
// (the worker was killed: the run marker names the branch); anyone else's —
// on the default branch, on no branch, or on a branch someone checked out
// by hand — stop the run instead of being moved away.
func (w *Worker) readyClone(ctx context.Context, repo *Repo, log *slog.Logger) error {
	if checkMarked(ctx, repo.Path) {
		// A branch review's checks were cut off (the worker died): what
		// they left is nobody's work.
		log.Warn("the clone was left detached by a branch review's checks; discarding what they changed", "clone", repo.Path)
		if err := discardCheckRun(ctx, repo); err != nil {
			return fmt.Errorf("cleaning up after a branch review's checks: %w", err)
		}
	}
	current, _ := git(ctx, repo.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	dirty, err := git(ctx, repo.Path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if dirty != "" && (current == repo.DefaultBranch || current == "") {
		return fmt.Errorf("the clone %s has uncommitted or untracked files on %s; a branch checkout needs it clean (commit, remove or .gitignore them): %s",
			repo.Path, cmp.Or(current, "a detached HEAD"), firstLine(dirty))
	}
	if current == repo.DefaultBranch {
		setRunMarker(ctx, repo.Path, "")
		return nil
	}
	if dirty == "" {
		log.Info("the clone is on another branch with nothing uncommitted; switching it back", "clone", repo.Path, "branch", current)
		runLogFrom(ctx).notef("the clone was on %s with nothing uncommitted; switching it to %s", current, repo.DefaultBranch)
		if _, err := git(ctx, repo.Path, "checkout", "--quiet", repo.DefaultBranch); err != nil {
			return err
		}
		setRunMarker(ctx, repo.Path, "")
		return nil
	}
	if runMarker(ctx, repo.Path) != current {
		return fmt.Errorf("the clone %s is on %s with uncommitted or untracked files no skopos run left (%s); commit or stash them and switch it back to %s, or pause the worker while you work in it",
			repo.Path, current, firstLine(dirty), repo.DefaultBranch)
	}
	log.Warn("a run that never finished left the clone on its branch; putting it back", "clone", repo.Path, "branch", current)
	runLogFrom(ctx).notef("the clone was left on %s by a run that never finished; putting it back on %s", current, repo.DefaultBranch)
	if err := parkClone(ctx, repo.Path, repo.DefaultBranch, stashLabel(current)); err != nil {
		return err
	}
	setRunMarker(ctx, repo.Path, "")
	return nil
}

// The run marker names the branch a run switched the clone to, from just
// before the checkout until the clone is parked again. Only a branch it
// names is a killed run's: readyClone stashes what is uncommitted there.
func runMarkerPath(ctx context.Context, clone string) string {
	dir, err := git(ctx, clone, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "skopos-run-branch")
}

// setRunMarker records branch ("" removes the marker).
func setRunMarker(ctx context.Context, clone, branch string) {
	p := runMarkerPath(ctx, clone)
	if p == "" {
		return
	}
	if branch == "" {
		_ = os.Remove(p)
		return
	}
	_ = os.WriteFile(p, []byte(branch), 0o600)
}

func runMarker(ctx context.Context, clone string) string {
	p := runMarkerPath(ctx, clone)
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// cloneSetup is what a branch-checkout clone was last set up for: the
// branch, and its head when the clone was last parked on it. It lives in
// the clone's git dir beside the lock, so every worker process sharing the
// clone sees it. Switching back to that branch at that head needs no new
// setup: a checkout leaves ignored files (dependencies, builds) alone, and
// only another setup touches the database.
type cloneSetup struct {
	Branch string `json:"branch"`
	Head   string `json:"head"`
}

func cloneSetupPath(ctx context.Context, clone string) string {
	dir, err := git(ctx, clone, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "skopos-setup.json")
}

func readCloneSetup(ctx context.Context, clone string) cloneSetup {
	var cs cloneSetup
	if p := cloneSetupPath(ctx, clone); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &cs)
		}
	}
	return cs
}

func writeCloneSetup(ctx context.Context, clone string, cs cloneSetup) {
	if p := cloneSetupPath(ctx, clone); p != "" {
		b, _ := json.Marshal(cs)
		_ = os.WriteFile(p, b, 0o600)
	}
}

// checkoutImplementBranch switches the clone to the item's branch, starting
// where implementStart says.
func checkoutImplementBranch(ctx context.Context, clone, branch, base string) error {
	local, ref, merge, err := implementStart(ctx, clone, branch, base)
	if err != nil {
		return err
	}
	if !local {
		_, err = git(ctx, clone, "checkout", "--quiet", "--no-track", "-b", branch, ref)
		return err
	}
	if _, err := git(ctx, clone, "checkout", "--quiet", branch); err != nil {
		return err
	}
	if merge != "" {
		// Before the stash of a paused round goes back on: the clone is
		// clean here, so a failed merge aborts cleanly.
		return mergeOrigin(ctx, clone, branch, merge)
	}
	return nil
}

// parkClone stashes anything uncommitted (tracked or untracked, never the
// ignored files — configuration, dependencies) under label and switches the
// clone to the default branch.
func parkClone(ctx context.Context, clone, defaultBranch, label string) error {
	// The run's agent is gone; a git command killed with it may have left
	// index.lock, which would stop the stash and the checkout below.
	clearStaleIndexLock(ctx, clone)
	dirty, err := git(ctx, clone, "status", "--porcelain")
	if err != nil {
		return err
	}
	if dirty != "" {
		if _, err := git(ctx, clone, "stash", "push", "--include-untracked", "--quiet", "-m", label); err != nil {
			return fmt.Errorf("stashing the uncommitted changes: %w", err)
		}
	}
	_, err = git(ctx, clone, "checkout", "--quiet", defaultBranch)
	return err
}

// park is parkClone when a run ends: on its own context, so a run cancelled
// by a shutdown still leaves the clone on the default branch. A clone set
// up for this branch records the head it leaves, for the next run's setup
// check.
func (w *Worker) park(repo *Repo, branch string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if cs := readCloneSetup(ctx, repo.Path); cs.Branch == branch {
		writeCloneSetup(ctx, repo.Path, cloneSetup{Branch: branch, Head: headSHA(ctx, repo.Path)})
	}
	if err := parkClone(ctx, repo.Path, repo.DefaultBranch, stashLabel(branch)); err != nil {
		log.Error("the clone could not be put back on the default branch; the next run on it tries again", "clone", repo.Path, "err", err)
		return
	}
	setRunMarker(ctx, repo.Path, "")
}

// stashRefs lists the stashes saved under label, newest first.
func stashRefs(ctx context.Context, clone, label string) []string {
	out, err := git(ctx, clone, "stash", "list", "--format=%gd%x09%gs")
	if err != nil {
		return nil
	}
	var refs []string
	for line := range strings.Lines(out) {
		ref, subject, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && strings.HasSuffix(subject, ": "+label) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// applyStash brings back the newest stash saved under label and drops it.
// One that doesn't apply cleanly is kept, and the clone is reset to the
// branch's last commit.
func applyStash(ctx context.Context, clone, label string) error {
	refs := stashRefs(ctx, clone, label)
	if len(refs) == 0 {
		return nil
	}
	if _, err := git(ctx, clone, "stash", "apply", "--quiet", refs[0]); err != nil {
		_, _ = git(ctx, clone, "reset", "--hard", "--quiet")
		_, _ = git(ctx, clone, "clean", "-fdq")
		return fmt.Errorf("the uncommitted changes from the last run (%s) did not apply; they are kept in the stash: %w", refs[0], err)
	}
	_, err := git(ctx, clone, "stash", "drop", "--quiet", refs[0])
	return err
}

// dropStashes drops every stash saved under label, oldest first so the
// newer ones' indexes stay put.
func dropStashes(ctx context.Context, clone, label string) {
	refs := stashRefs(ctx, clone, label)
	for i := len(refs) - 1; i >= 0; i-- {
		_, _ = git(ctx, clone, "stash", "drop", "--quiet", refs[i])
	}
}

// checkMarkerPath marks a branch-checkout clone detached by a review-only
// item's checks (checkHead), beside the lock in the clone's git dir.
func checkMarkerPath(ctx context.Context, clone string) string {
	dir, err := git(ctx, clone, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "skopos-checking")
}

func setCheckMarker(ctx context.Context, clone string, on bool) {
	p := checkMarkerPath(ctx, clone)
	if p == "" {
		return
	}
	if on {
		_ = os.WriteFile(p, nil, 0o600)
	} else {
		_ = os.Remove(p)
	}
}

func checkMarked(ctx context.Context, clone string) bool {
	p := checkMarkerPath(ctx, clone)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// discardCheckRun throws away what a branch review's checks changed in the
// clone (tracked edits and new files; ignored files such as dependencies
// stay) and puts it back on its default branch.
func discardCheckRun(ctx context.Context, repo *Repo) error {
	if _, err := git(ctx, repo.Path, "reset", "--hard", "--quiet"); err != nil {
		return err
	}
	if _, err := git(ctx, repo.Path, "clean", "-fd", "--quiet"); err != nil {
		return err
	}
	if _, err := git(ctx, repo.Path, "checkout", "--quiet", repo.DefaultBranch); err != nil {
		return err
	}
	setCheckMarker(ctx, repo.Path, false)
	return nil
}
