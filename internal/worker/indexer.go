package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/codeindex"
)

// Idle workers keep the code index fresh (tmp/agent-mcp-and-indexing —
// design.md §3). The server's claim answer says what this worker indexes:
// the workspaces with the code_index switch on (it indexes the item
// branches it implemented there) and those whose default branch it keeps
// fresh (one able worker per workspace). Indexing runs only while the
// worker is idle, in the background: a claim that returns a run stops it,
// and what was parsed so far stays in the parse cache. It works in a
// detached worktree of its own per repo, so the clone a run uses is never
// touched beyond a fetch.

const (
	// indexCheckEvery: how often a default branch is fetched to see whether
	// it moved.
	indexCheckEvery = 5 * time.Minute
	// indexRetryAfter: after a failed fetch or build the workspace waits
	// this long (and does not report itself indexable meanwhile).
	indexRetryAfter = 15 * time.Minute
	// indexReconcileEvery: how often the default-branch worker drops the
	// indexes of branches gone from origin.
	indexReconcileEvery = 6 * time.Hour
	// indexStaleAfter: a branch gone from origin keeps its index this long
	// after it was last built (someone may index a branch before pushing it).
	indexStaleAfter = 7 * 24 * time.Hour
)

// indexStopWait bounds how long a claimed run waits for indexing to stop (a
// var: tests shorten it).
var indexStopWait = 30 * time.Second

// indexState is what the worker has indexed, kept in the state dir across
// restarts.
type indexState struct {
	// Default: workspace → the default-branch commit last pushed.
	Default map[string]string `json:"default"`
	// Branches: "<workspace> <branch>" → the commit last pushed.
	Branches map[string]string `json:"branches"`
	// Queue: item branches waiting to be indexed (after an implement run).
	Queue []indexJob `json:"queue"`
}

type indexJob struct {
	Workspace string `json:"workspace"`
	Branch    string `json:"branch"`
}

func branchKey(ws, branch string) string { return ws + " " + branch }

// indexer is the worker's indexing state.
type indexer struct {
	mu      sync.Mutex
	loaded  bool
	state   indexState
	cancel  context.CancelFunc
	done    chan struct{}
	checked map[string]time.Time // workspace → last default-branch check
	failed  map[string]time.Time // workspace → last failure
	pruned  map[string]time.Time // workspace → last stale-branch cleanup
	cache   *codeindex.Store
}

func (w *Worker) indexStatePath() string { return filepath.Join(w.cfg.StateDir, "index-state.json") }

// loadIndexState reads the saved state once; ix.mu must be held.
func (w *Worker) loadIndexState() {
	ix := &w.index
	if ix.loaded {
		return
	}
	ix.loaded = true
	ix.state = indexState{Default: map[string]string{}, Branches: map[string]string{}}
	ix.checked, ix.failed, ix.pruned = map[string]time.Time{}, map[string]time.Time{}, map[string]time.Time{}
	if raw, err := os.ReadFile(w.indexStatePath()); err == nil {
		_ = json.Unmarshal(raw, &ix.state)
	}
	if ix.state.Default == nil {
		ix.state.Default = map[string]string{}
	}
	if ix.state.Branches == nil {
		ix.state.Branches = map[string]string{}
	}
}

// saveIndexState writes the state; ix.mu must be held.
func (w *Worker) saveIndexState() {
	raw, err := json.MarshalIndent(w.index.state, "", "  ")
	if err != nil {
		return
	}
	path := w.indexStatePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path+".tmp", raw, 0o600); err == nil {
		_ = os.Rename(path+".tmp", path)
	}
}

// indexable lists the served workspaces this worker can index now: the repo
// is configured and present, its preflight passes, and its last index fetch
// or build did not fail recently.
func (w *Worker) indexable() []string {
	w.index.mu.Lock()
	defer w.index.mu.Unlock()
	w.loadIndexState()
	var out []string
	for _, ws := range w.cfg.Workspaces {
		repo := w.repo(ws)
		if repo == nil || repo.Path == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(repo.Path, ".git")); err != nil {
			continue
		}
		if h, ok := w.health[ws]; ok && h.reason != "" {
			continue
		}
		if t, ok := w.index.failed[ws]; ok && time.Since(t) < indexRetryAfter {
			continue
		}
		out = append(out, ws)
	}
	return out
}

// queueBranchIndex asks for an item branch to be indexed at the next idle
// moment (its implement run finished).
func (w *Worker) queueBranchIndex(ws, branch string) {
	w.index.mu.Lock()
	defer w.index.mu.Unlock()
	w.loadIndexState()
	job := indexJob{Workspace: ws, Branch: branch}
	if !slices.Contains(w.index.state.Queue, job) {
		w.index.state.Queue = append(w.index.state.Queue, job)
		w.saveIndexState()
	}
}

// indexBranchTimeout bounds indexing an item branch at the end of its
// implement run (a cold parse cache on a large repo); past it the branch
// waits for an idle moment instead.
const indexBranchTimeout = 5 * time.Minute

// indexEnabled reports whether the last claim answer had the code_index
// switch on for ws.
func (w *Worker) indexEnabled(ws string) bool {
	enabled, _ := w.client.IndexDuty()
	return slices.Contains(enabled, ws)
}

// indexBranchNow indexes an item branch at the end of its implement run, so
// the review that follows queries the code it reviews. Only where the
// workspace's code_index switch is on; a failure (or the time bound) leaves
// the branch queued for the next idle moment and never fails the run.
func (w *Worker) indexBranchNow(ctx context.Context, repo *Repo, ws, branch string) {
	if !w.indexEnabled(ws) {
		return
	}
	if w.indexRunning() {
		// A pass that outlived stopIndexing owns the indexing worktree.
		w.queueBranchIndex(ws, branch)
		return
	}
	sha, err := git(ctx, repo.Path, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return
	}
	ictx, cancel := context.WithTimeout(ctx, indexBranchTimeout)
	defer cancel()
	if err := w.indexCommit(ictx, repo, ws, branch, sha, repo.Checkout == checkoutBranch); err != nil {
		w.log.Warn("indexing the branch failed; it waits for an idle moment", "workspace", ws, "branch", branch, "err", err)
		w.queueBranchIndex(ws, branch)
		return
	}
	w.index.mu.Lock()
	w.loadIndexState()
	w.index.state.Branches[branchKey(ws, branch)] = sha
	w.index.state.Queue = slices.DeleteFunc(w.index.state.Queue, func(j indexJob) bool { return j.Workspace == ws && j.Branch == branch })
	w.saveIndexState()
	w.index.mu.Unlock()
}

// dropBranchIndex removes a retired item branch's index from the server
// (only when indexing is on for its workspace) and forgets it here.
func (w *Worker) dropBranchIndex(ctx context.Context, ws, branch string) {
	enabled, _ := w.client.IndexDuty()
	w.index.mu.Lock()
	w.loadIndexState()
	key := branchKey(ws, branch)
	_, had := w.index.state.Branches[key]
	delete(w.index.state.Branches, key)
	w.index.state.Queue = slices.DeleteFunc(w.index.state.Queue, func(j indexJob) bool { return j.Workspace == ws && j.Branch == branch })
	w.saveIndexState()
	w.index.mu.Unlock()
	if had && slices.Contains(enabled, ws) {
		if err := w.client.DropIndexBranch(ctx, ws, branch); err != nil {
			w.log.Warn("dropping the branch index failed", "workspace", ws, "branch", branch, "err", err)
		}
	}
}

// maybeIndex starts a background indexing pass while the worker is idle,
// unless one is running or there is nothing to do.
func (w *Worker) maybeIndex(ctx context.Context) {
	enabled, def := w.client.IndexDuty()
	w.index.mu.Lock()
	defer w.index.mu.Unlock()
	w.loadIndexState()
	if w.index.done != nil {
		select {
		case <-w.index.done:
			w.index.done, w.index.cancel = nil, nil
		default:
			return // still running
		}
	}
	due := false
	for _, ws := range def {
		if time.Since(w.index.checked[ws]) >= indexCheckEvery && time.Since(w.index.failed[ws]) >= indexRetryAfter {
			due = true
		}
		if time.Since(w.index.pruned[ws]) >= indexReconcileEvery {
			due = true
		}
	}
	for _, j := range w.index.state.Queue {
		if slices.Contains(enabled, j.Workspace) && time.Since(w.index.failed[j.Workspace]) >= indexRetryAfter {
			due = true
		}
	}
	if !due {
		return
	}
	ictx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	w.index.cancel, w.index.done = cancel, done
	go func() {
		defer close(done)
		w.indexPass(ictx, enabled, def)
	}()
}

// stopIndexing stops a running pass (a run was claimed) and waits for it,
// bounded: the build stops at its next file, git at once.
func (w *Worker) stopIndexing() {
	w.index.mu.Lock()
	cancel, done := w.index.cancel, w.index.done
	w.index.cancel = nil
	w.index.mu.Unlock()
	if done == nil {
		return
	}
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
		w.index.mu.Lock()
		if w.index.done == done {
			w.index.done = nil
		}
		w.index.mu.Unlock()
	case <-time.After(indexStopWait):
		// done stays set: no second pass starts (maybeIndex,
		// indexBranchNow) until this one has really exited.
		w.log.Warn("indexing did not stop in time; the run goes ahead")
	}
}

// indexRunning reports whether a pass is still running (one that outlived
// stopIndexing included).
func (w *Worker) indexRunning() bool {
	w.index.mu.Lock()
	defer w.index.mu.Unlock()
	if w.index.done == nil {
		return false
	}
	select {
	case <-w.index.done:
		return false
	default:
		return true
	}
}

// indexPass indexes what is due: assigned default branches that moved,
// then queued item branches of workspaces with the switch on.
func (w *Worker) indexPass(ctx context.Context, enabled, def []string) {
	w.index.mu.Lock()
	w.loadIndexState()
	w.index.mu.Unlock()
	for _, ws := range def {
		if ctx.Err() != nil {
			return
		}
		repo := w.repo(ws)
		w.index.mu.Lock()
		due := repo != nil && time.Since(w.index.checked[ws]) >= indexCheckEvery && time.Since(w.index.failed[ws]) >= indexRetryAfter
		if due {
			w.index.checked[ws] = time.Now()
		}
		last := w.index.state.Default[ws]
		w.index.mu.Unlock()
		if !due {
			continue
		}
		sha, err := w.withCloneLock(ctx, repo, func() (string, error) { return fetchBranch(ctx, repo.Path, repo.DefaultBranch) })
		if err != nil {
			w.indexFailed(ctx, ws, repo.DefaultBranch, err)
			continue
		}
		if sha == last {
			continue
		}
		if err := w.indexCommit(ctx, repo, ws, repo.DefaultBranch, sha, false); err != nil {
			w.indexFailed(ctx, ws, repo.DefaultBranch, err)
			continue
		}
		w.index.mu.Lock()
		w.index.state.Default[ws] = sha
		w.saveIndexState()
		w.index.mu.Unlock()
	}
	// The default-branch worker also clears out indexes of branches gone
	// from origin: whoever pushed them (a worker, `skopos index push`).
	for _, ws := range def {
		if ctx.Err() != nil {
			return
		}
		if repo := w.repo(ws); repo != nil {
			w.pruneStaleIndexes(ctx, repo, ws)
		}
	}

	w.index.mu.Lock()
	queue := slices.Clone(w.index.state.Queue)
	w.index.mu.Unlock()
	for _, job := range queue {
		if ctx.Err() != nil {
			return
		}
		repo := w.repo(job.Workspace)
		forget := func() {
			w.index.mu.Lock()
			w.index.state.Queue = slices.DeleteFunc(w.index.state.Queue, func(j indexJob) bool { return j == job })
			w.saveIndexState()
			w.index.mu.Unlock()
		}
		if repo == nil || !slices.Contains(enabled, job.Workspace) {
			forget()
			continue
		}
		w.index.mu.Lock()
		failedAt := w.index.failed[job.Workspace]
		last := w.index.state.Branches[branchKey(job.Workspace, job.Branch)]
		w.index.mu.Unlock()
		if time.Since(failedAt) < indexRetryAfter {
			continue
		}
		sha, err := git(ctx, repo.Path, "rev-parse", "--verify", "refs/heads/"+job.Branch+"^{commit}")
		if err != nil || sha == last {
			forget() // the branch is gone, or this commit is indexed
			continue
		}
		if err := w.indexCommit(ctx, repo, job.Workspace, job.Branch, sha, false); err != nil {
			w.indexFailed(ctx, job.Workspace, job.Branch, err)
			continue
		}
		w.index.mu.Lock()
		w.index.state.Branches[branchKey(job.Workspace, job.Branch)] = sha
		w.index.mu.Unlock()
		forget()
	}
}

// pruneStaleIndexes drops the server's index of every branch that is gone
// from origin and was last built more than indexStaleAfter ago (a branch
// may be indexed before it is pushed). The default branch is never
// dropped. At most every indexReconcileEvery per workspace.
func (w *Worker) pruneStaleIndexes(ctx context.Context, repo *Repo, ws string) {
	w.index.mu.Lock()
	due := time.Since(w.index.pruned[ws]) >= indexReconcileEvery
	if due {
		w.index.pruned[ws] = time.Now()
	}
	w.index.mu.Unlock()
	if !due {
		return
	}
	indexed, err := w.client.IndexedBranches(ctx, ws)
	if err != nil || len(indexed) == 0 {
		return
	}
	out, err := git(ctx, repo.Path, "ls-remote", "--heads", "origin")
	if err != nil {
		// Origin unreachable: never read that as "every branch is gone".
		w.log.Warn("listing origin's branches failed; stale indexes stay", "workspace", ws, "err", err)
		return
	}
	onOrigin := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if _, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			onOrigin[strings.TrimPrefix(ref, "refs/heads/")] = true
		}
	}
	if len(onOrigin) == 0 {
		return // an empty listing is not proof every branch is gone
	}
	for branch, b := range indexed {
		if branch == repo.DefaultBranch || onOrigin[branch] || b.BuiltAt.IsZero() || time.Since(b.BuiltAt) < indexStaleAfter {
			continue
		}
		if err := w.client.DropIndexBranch(ctx, ws, branch); err != nil {
			w.log.Warn("dropping a stale branch index failed", "workspace", ws, "branch", branch, "err", err)
			continue
		}
		w.index.mu.Lock()
		delete(w.index.state.Branches, branchKey(ws, branch))
		w.saveIndexState()
		w.index.mu.Unlock()
		w.log.Info("dropped the index of a branch gone from origin", "workspace", ws, "branch", branch, "built", b.BuiltAt.Format(time.RFC3339))
	}
}

func (w *Worker) indexFailed(ctx context.Context, ws, branch string, err error) {
	if ctx.Err() != nil {
		return // stopped for a run: not a failure
	}
	if errors.Is(err, errCloneBusy) {
		// Another process holds the clone: try at the next check, without
		// the back-off or leaving the indexable list.
		w.log.Debug("clone busy; indexing waits for the next check", "workspace", ws, "branch", branch)
		return
	}
	w.index.mu.Lock()
	w.index.failed[ws] = time.Now()
	w.index.mu.Unlock()
	w.log.Warn("indexing failed; retrying later", "workspace", ws, "branch", branch, "retry_in", indexRetryAfter.String(), "err", err)
}

// errCloneBusy: another worker process holds the clone; try next pass.
var errCloneBusy = errors.New("the clone is busy")

// withCloneLock runs fn holding the clone's lock if it is free.
func (w *Worker) withCloneLock(ctx context.Context, repo *Repo, fn func() (string, error)) (string, error) {
	release, ok, err := tryLockClone(ctx, repo.Path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errCloneBusy
	}
	defer release()
	return fn()
}

// indexCheckout is the repo's own indexing worktree: named after the clone
// and a hash of its path, so two clones with the same directory name never
// share one.
func (w *Worker) indexCheckout(repo *Repo) string {
	sum := sha256.Sum256([]byte(repo.Path))
	return filepath.Join(w.cfg.StateDir, "index", filepath.Base(repo.Path)+"-"+hex.EncodeToString(sum[:4]))
}

// indexCommit checks sha out in the indexing worktree and pushes the
// branch's index. haveLock: the caller already holds the clone's lock (a
// branch-checkout run).
func (w *Worker) indexCommit(ctx context.Context, repo *Repo, ws, branch, sha string, haveLock bool) error {
	dir := w.indexCheckout(repo)
	checkout := func() (string, error) { return "", checkoutIndexTree(ctx, repo.Path, dir, sha) }
	var err error
	if haveLock {
		_, err = checkout()
	} else {
		_, err = w.withCloneLock(ctx, repo, checkout)
	}
	if err != nil {
		return err
	}
	cache, err := w.indexCache()
	if err != nil {
		return err
	}
	start := time.Now()
	res, err := codeindex.Push(ctx, codeindex.PushOptions{
		ServerURL: w.client.base, APIKey: w.client.key,
		Workspace: ws, Branch: branch, Root: dir,
		Source: "worker:" + w.cfg.ID, Cache: cache,
	})
	if err != nil {
		return err
	}
	w.log.Info("indexed", "workspace", ws, "branch", branch, "head", short(res.Head), "files", res.Files, "uploaded", res.Uploaded, "took", time.Since(start).Round(time.Second).String())
	return nil
}

// checkoutIndexTree puts the indexing worktree at sha: created detached on
// first use, moved (forced, detached) after. A pass stopped mid-checkout
// can leave its index.lock behind; the worktree is the indexer's alone, so
// a stale lock is removed.
func checkoutIndexTree(ctx context.Context, clone, dir, sha string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		_ = os.RemoveAll(dir)
		_, _ = git(ctx, clone, "worktree", "prune")
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		if _, err := git(ctx, clone, "worktree", "add", "--detach", "--force", dir, sha); err != nil {
			return fmt.Errorf("creating the indexing worktree: %w", err)
		}
		return nil
	}
	if gitDir, err := git(ctx, dir, "rev-parse", "--absolute-git-dir"); err == nil {
		_ = os.Remove(filepath.Join(strings.TrimSpace(gitDir), "index.lock"))
	}
	if _, err := git(ctx, dir, "checkout", "--quiet", "--detach", "--force", sha); err != nil {
		return fmt.Errorf("moving the indexing worktree: %w", err)
	}
	return nil
}

// indexCache is the worker's parse cache (state dir), opened once.
func (w *Worker) indexCache() (codeindex.BuildCacher, error) {
	w.index.mu.Lock()
	defer w.index.mu.Unlock()
	if w.index.cache == nil {
		st, err := codeindex.NewStore(filepath.Join(w.cfg.StateDir, "index-cache"))
		if err != nil {
			return nil, err
		}
		w.index.cache = st
	}
	return w.index.cache.AsBuildCache("cache"), nil
}
