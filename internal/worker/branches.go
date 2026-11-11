package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// branchRecord is what an implementation left on this worker for one item:
// its worktree, its branch, and the pull request opened for it. The sweep
// reads these to clean up once the item is finished.
type branchRecord struct {
	ItemID      string `json:"item_id"`
	WorkspaceID string `json:"workspace_id"`
	Branch      string `json:"branch"`
	Worktree    string `json:"worktree"`
	// Pushed: the latest round pushed everything on the branch. Reset at
	// the start of every round, so commits a round never pushed are never
	// deleted with the local branch.
	Pushed   bool   `json:"pushed"`
	PRNumber int    `json:"pr_number,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	// PROwned: skopos opened the pull request. One on a branch taken over
	// from someone is theirs: the sweep never closes it. Records from
	// before this field have it unset and are treated as owned.
	PROwned *bool `json:"pr_owned,omitempty"`
	// ImplementedRun: the run whose implement turn finished (committed),
	// with its summary and the check-fix rounds it has used. A pause during
	// a fix round resumes that run straight at the checks.
	ImplementedRun     string `json:"implemented_run,omitempty"`
	ImplementedSummary string `json:"implemented_summary,omitempty"`
	FixRounds          int    `json:"fix_rounds,omitempty"`
	// SetupRun: the run that set the worktree up, with the setup results
	// its report carries. SetupPending: setup started in the worktree but
	// never finished, so the next run on it runs setup again.
	SetupRun     string  `json:"setup_run,omitempty"`
	Setup        []Check `json:"setup,omitempty"`
	SetupPending bool    `json:"setup_pending,omitempty"`
	// When the sweep first saw the item discarded, or missing (404), in
	// Unix ms; 0 = not seen. Both reset if the item comes back.
	DiscardedSeenMS int64 `json:"discarded_seen_ms,omitempty"`
	MissingSeenMS   int64 `json:"missing_seen_ms,omitempty"`
}

const (
	// sweepEvery spaces the idle-time cleanup: it costs one item lookup
	// per record, and nothing is urgent about removing a finished worktree.
	sweepEvery = 5 * time.Minute
	// sweepBudget bounds one sweep, so a slow forge or server never keeps
	// the worker from claiming work for long.
	sweepBudget = 2 * time.Minute
	// discardGrace: a rejected item can be restored (the undo for an
	// accidental discard), so its pull request and branch outlive the
	// Reject by a day.
	discardGrace = 24 * time.Hour
	// missingGrace: skopos answers 404 both for a deleted item and for one
	// outside the worker key's scope, so a 404 never closes anything. After
	// a week of 404s the worktree goes; the branch and pull request stay.
	missingGrace = 7 * 24 * time.Hour
)

var reRecordID = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

func (w *Worker) branchesDir() string { return filepath.Join(w.cfg.StateDir, "branches") }

func (w *Worker) loadBranch(itemID string) (branchRecord, bool) {
	var rec branchRecord
	if !reRecordID.MatchString(itemID) {
		return rec, false
	}
	b, err := os.ReadFile(filepath.Join(w.branchesDir(), itemID+".json"))
	if err != nil || json.Unmarshal(b, &rec) != nil {
		return rec, false
	}
	return rec, true
}

func (w *Worker) saveBranch(rec branchRecord) {
	if !reRecordID.MatchString(rec.ItemID) {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = os.MkdirAll(w.branchesDir(), 0o700)
	_ = os.WriteFile(filepath.Join(w.branchesDir(), rec.ItemID+".json"), b, 0o600)
}

func (w *Worker) dropBranch(itemID string) {
	if reRecordID.MatchString(itemID) {
		_ = os.Remove(filepath.Join(w.branchesDir(), itemID+".json"))
	}
}

// maybeSweep runs sweepBranches at most every sweepEvery (called on idle
// polls, so it never delays a run), within sweepBudget.
func (w *Worker) maybeSweep(ctx context.Context) {
	if time.Since(w.lastSweep) < sweepEvery {
		return
	}
	w.lastSweep = time.Now()
	// The sweep runs git in the clones; an indexing pass (background) would
	// race it. It stops here and resumes at the next idle poll.
	w.stopIndexing()
	ctx, cancel := context.WithTimeout(ctx, sweepBudget)
	defer cancel()
	w.sweepBranches(ctx, time.Now())
}

// sweepBranches cleans up after finished items:
//   - done: remove the worktree, the item's stashed changes (branch
//     checkout), and the local branch when the last round pushed it (an
//     unpushed branch is the only copy of the work). The pull request
//     stays: merging it is the approver's step.
//   - discarded for over discardGrace: close the pull request unmerged,
//     then clean up as for done.
//   - missing (404) for over missingGrace: remove the worktree only.
//
// A lookup or close that fails for a transient reason keeps the record for
// the next sweep.
func (w *Worker) sweepBranches(ctx context.Context, now time.Time) {
	entries, err := os.ReadDir(w.branchesDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		rec, ok := w.loadBranch(strings.TrimSuffix(e.Name(), ".json"))
		if !ok {
			continue
		}
		status := ""
		if it, err := w.client.GetItem(ctx, rec.ItemID); err == nil {
			status = it.Status
		} else if strings.Contains(err.Error(), ": 404 ") {
			status = "missing"
		} else {
			continue
		}
		log := w.log.With("item", rec.ItemID, "branch", rec.Branch, "status", status)
		// seen marks when a status was first seen, and reports whether it
		// has lasted past grace.
		seen := func(at *int64, grace time.Duration) bool {
			if *at == 0 {
				*at = now.UnixMilli()
				w.saveBranch(rec)
				return false
			}
			return now.Sub(time.UnixMilli(*at)) >= grace
		}
		switch status {
		case "done":
		case "discarded":
			if !seen(&rec.DiscardedSeenMS, discardGrace) {
				continue
			}
		case "missing":
			if !seen(&rec.MissingSeenMS, missingGrace) {
				continue
			}
		default:
			// Live (or restored, or back in scope): forget any marks.
			if rec.DiscardedSeenMS != 0 || rec.MissingSeenMS != 0 {
				rec.DiscardedSeenMS, rec.MissingSeenMS = 0, 0
				w.saveBranch(rec)
			}
			continue
		}
		repo := w.repo(rec.WorkspaceID)
		if repo == nil {
			log.Warn("finished item's repo is no longer configured; forgetting its worktree", "worktree", rec.Worktree)
			w.dropBranch(rec.ItemID)
			continue
		}
		if status == "discarded" && rec.PRNumber > 0 && (rec.PROwned == nil || *rec.PROwned) {
			if err := w.closePullRequest(ctx, repo, rec.PRNumber); err != nil {
				// Retry an outage (network, 5xx); a 4xx or a push config
				// that no longer allows pull requests is final.
				var pe *prError
				if errors.As(err, &pe) && pe.Status < 500 || errors.Is(err, errNoPRConfig) {
					log.Warn("the pull request could not be closed; leaving it", "pr", rec.PRURL, "err", err)
				} else {
					log.Warn("closing the pull request failed; retrying next sweep", "pr", rec.PRURL, "err", err)
					continue
				}
			} else {
				log.Info("closed the pull request", "pr", rec.PRURL)
			}
		}
		// A branch-checkout clone another worker process is using waits
		// for the next sweep: its branch may be the one checked out.
		deleteBranch := rec.Pushed && status != "missing"
		if !w.cleanUpItem(ctx, repo, rec, deleteBranch, status != "missing") {
			continue
		}
		w.dropBranch(rec.ItemID)
		w.dropBranchIndex(ctx, rec.WorkspaceID, rec.Branch)
		log.Info("cleaned up the finished item's worktree", "worktree", rec.Worktree, "local_branch_deleted", deleteBranch)
	}
}

var errNoPRConfig = errors.New("the repo's push config no longer allows pull requests")

// cleanUpItem removes what a finished item left in the clone: its worktree,
// its local branch (deleteBranch) and its stashed changes (dropStash).
// False when a branch-checkout clone is busy, for the next sweep.
func (w *Worker) cleanUpItem(ctx context.Context, repo *Repo, rec branchRecord, deleteBranch, dropStash bool) bool {
	if repo.Checkout == checkoutBranch {
		release, ok, err := tryLockClone(ctx, repo.Path)
		if err != nil || !ok {
			return false
		}
		defer release()
	}
	removeWorktree(ctx, repo.Path, rec.Worktree)
	if deleteBranch {
		_, _ = git(ctx, repo.Path, "branch", "-D", rec.Branch)
	}
	if dropStash {
		dropStashes(ctx, repo.Path, stashLabel(rec.Branch))
	}
	return true
}

func (w *Worker) closePullRequest(ctx context.Context, repo *Repo, number int) error {
	c, err := newPRClient(repo.Push.Kind, repo.Push.RemoteURL, os.Getenv(repo.Push.TokenEnv))
	if err != nil {
		return fmt.Errorf("%w: %v", errNoPRConfig, err)
	}
	return c.Close(ctx, number)
}
