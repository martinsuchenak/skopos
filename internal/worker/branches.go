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
	Pushed      bool   `json:"pushed"`
	PRNumber    int    `json:"pr_number,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
}

// sweepEvery spaces the idle-time cleanup: it costs one item lookup per
// record, and nothing is urgent about removing a finished worktree.
const sweepEvery = 5 * time.Minute

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
// polls, so it never delays a run).
func (w *Worker) maybeSweep(ctx context.Context) {
	if time.Since(w.lastSweep) < sweepEvery {
		return
	}
	w.lastSweep = time.Now()
	w.sweepBranches(ctx)
}

// sweepBranches cleans up after finished items:
//   - discarded (rejected) or deleted: close the item's pull request,
//     unmerged;
//   - done or discarded or deleted: remove the worktree, and the local
//     branch when it was pushed (an unpushed branch is the only copy of
//     the work, so it stays).
//
// Done leaves the pull request alone: merging it is the approver's step.
// A lookup or close that fails for a transient reason keeps the record for
// the next sweep.
func (w *Worker) sweepBranches(ctx context.Context) {
	entries, err := os.ReadDir(w.branchesDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		rec, ok := w.loadBranch(id)
		if !ok {
			continue
		}
		status := ""
		if it, err := w.client.GetItem(ctx, rec.ItemID); err == nil {
			status = it.Status
		} else if strings.Contains(err.Error(), ": 404 ") {
			status = "deleted"
		} else {
			continue
		}
		if status != "done" && status != "discarded" && status != "deleted" {
			continue
		}
		log := w.log.With("item", rec.ItemID, "branch", rec.Branch, "status", status)
		repo := w.cfg.Repos[rec.WorkspaceID]
		if repo == nil {
			log.Warn("finished item's repo is no longer configured; forgetting its worktree", "worktree", rec.Worktree)
			w.dropBranch(rec.ItemID)
			continue
		}
		if status != "done" && rec.PRNumber > 0 {
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
		removeWorktree(ctx, repo.Path, rec.Worktree)
		if rec.Pushed {
			_, _ = git(ctx, repo.Path, "branch", "-D", rec.Branch)
		}
		w.dropBranch(rec.ItemID)
		log.Info("cleaned up the finished item's worktree", "worktree", rec.Worktree, "local_branch_deleted", rec.Pushed)
	}
}

var errNoPRConfig = errors.New("the repo's push config no longer allows pull requests")

func (w *Worker) closePullRequest(ctx context.Context, repo *Repo, number int) error {
	c, err := newPRClient(repo.Push.Kind, repo.Push.RemoteURL, os.Getenv(repo.Push.TokenEnv))
	if err != nil {
		return fmt.Errorf("%w: %v", errNoPRConfig, err)
	}
	return c.Close(ctx, number)
}
