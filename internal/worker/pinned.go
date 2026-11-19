package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// headMoved is what a review or QA run reports when the branch holds
// commits after the one it was given (runs.HeadMoved on the wire): the
// server has those commits checked first, then runs review and QA on them.
type headMoved struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Local bool   `json:"local,omitempty"`
}

// branchState is where an item's branch stands against the commit a run
// was given (pinned), after a fetch of origin's branch.
type branchState struct {
	// Pinned: the given commit in full; "" when the clone does not have it
	// (origin never got it).
	Pinned    string
	OriginTip string
	// Moved: commits after Pinned — origin's, or, with a branch checkout,
	// the clone's own branch when it holds commits origin does not (a
	// developer committed there without pushing).
	Moved *headMoved
	// Rewritten: the branch no longer holds Pinned.
	Rewritten bool
}

// branchStateFor reads the branch against pinned in the repo's clone.
func branchStateFor(ctx context.Context, repo *Repo, branch, pinned string) branchState {
	st := branchState{OriginTip: strings.TrimSpace(mustGit(ctx, repo.Path, "rev-parse", "origin/"+branch))}
	full, err := git(ctx, repo.Path, "rev-parse", "--verify", "--quiet", pinned+"^{commit}")
	if st.Pinned = strings.TrimSpace(full); err != nil || st.Pinned == "" {
		st.Pinned = ""
		return st
	}
	if repo.Checkout == checkoutBranch {
		local := strings.TrimSpace(mustGit(ctx, repo.Path, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch))
		if local != "" && local != st.Pinned && isAncestor(ctx, repo.Path, st.Pinned, local) &&
			(st.OriginTip == "" || !isAncestor(ctx, repo.Path, local, st.OriginTip)) {
			st.Moved = &headMoved{From: st.Pinned, To: local, Local: true}
			return st
		}
	}
	switch {
	case st.OriginTip == st.Pinned:
	case st.OriginTip != "" && isAncestor(ctx, repo.Path, st.Pinned, st.OriginTip):
		st.Moved = &headMoved{From: st.Pinned, To: st.OriginTip}
	default:
		st.Rewritten = true
	}
	return st
}

// pinnedHead checks the branch for a review or QA run pinned to the
// implemented commit: nil moved and nil error = test that commit; moved =
// the branch holds commits after it (the run tests nothing and reports
// them); an error = the commit is missing (retried) or the branch was
// rewritten (a person decides).
func pinnedHead(ctx context.Context, repo *Repo, branch, pinned string) (*headMoved, error) {
	st := branchStateFor(ctx, repo, branch, pinned)
	switch {
	case st.Pinned == "":
		// The implementation said it pushed: a fetch that does not bring
		// the commit is most likely a race with that push, worth a retry.
		return nil, retryable(fmt.Errorf("the implemented commit %s is "+notPushedMarker+" to origin/%s (it is at %s)", shortSHA(pinned), branch, shortSHA(st.OriginTip)))
	case st.Rewritten:
		return nil, needsPerson(fmt.Errorf("origin/%s no longer holds the implemented commit %s (it is at %s): the branch was rewritten; Request changes or Retry to implement again from the branch as it is now",
			branch, shortSHA(st.Pinned), shortSHA(st.OriginTip)))
	}
	return st.Moved, nil
}

// tipCommit is one commit after the tested head (inbox.TipCommit, and
// runs.AdoptedCommit, on the wire).
type tipCommit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// commitsAfter lists the commits in to's history after from, newest first
// (at most 50).
func commitsAfter(ctx context.Context, dir, from, to string) []tipCommit {
	out, err := git(ctx, dir, "log", "--max-count=50", "--format=%H%x1f%an%x1f%s", from+".."+to)
	if err != nil {
		return nil
	}
	var cs []tipCommit
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\x1f", 3)
		if len(parts) != 3 || parts[0] == "" {
			continue
		}
		cs = append(cs, tipCommit{SHA: parts[0], Author: parts[1], Subject: parts[2]})
	}
	return cs
}

// adoptSummary describes the commits an adopt run took in.
func adoptSummary(from string, cs []tipCommit) string {
	if len(cs) == 0 {
		return "No commits after " + shortSHA(from) + " on the branch here: the implemented commit is checked again."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Checked %s added to the branch after %s:\n", plural(len(cs), "commit"), shortSHA(from))
	for _, c := range cs {
		fmt.Fprintf(&b, "\n- %s %s", shortSHA(c.SHA), c.Subject)
		if c.Author != "" {
			fmt.Fprintf(&b, " (%s)", c.Author)
		}
	}
	return b.String()
}

// branchTipReport is the worker's report of where an item's branch stands
// after review and QA (inbox.BranchTipInput).
type branchTipReport struct {
	Head      string      `json:"head"`
	SHA       string      `json:"sha"`
	Local     bool        `json:"local,omitempty"`
	Rewritten bool        `json:"rewritten,omitempty"`
	Worker    string      `json:"worker"`
	Commits   []tipCommit `json:"commits,omitempty"`
}

// PutBranchTip reports where an item's branch stands (an empty SHA clears).
func (c *Client) PutBranchTip(ctx context.Context, itemID string, in branchTipReport) error {
	return c.do(ctx, http.MethodPut, "/api/inbox/"+itemID+"/branch-tip", in, nil)
}

// checkBranchTip looks, between runs, at the branch of an item waiting
// for sign-off: commits a developer added after the tested head (pushed,
// or in this worker's clone) are reported, so the sign-off card can name
// them and offer to check them. Reported once per change.
func (w *Worker) checkBranchTip(ctx context.Context, rec branchRecord, log *slog.Logger) {
	repo := w.repo(rec.WorkspaceID)
	if repo == nil || rec.Head == "" {
		return
	}
	if _, err := git(ctx, repo.Path, "fetch", "--quiet", "origin", rec.Branch); err != nil {
		return
	}
	st := branchStateFor(ctx, repo, rec.Branch, rec.Head)
	in := branchTipReport{Head: rec.Head, Worker: w.cfg.ID}
	switch {
	case st.Pinned == "" || st.Rewritten:
		in.SHA, in.Rewritten = st.OriginTip, true
	case st.Moved != nil:
		in.SHA, in.Local = st.Moved.To, st.Moved.Local
		in.Commits = commitsAfter(ctx, repo.Path, rec.Head, st.Moved.To)
	}
	key := in.SHA
	if in.Rewritten {
		key += "!"
	}
	if key == rec.TipReported {
		return
	}
	if err := w.client.PutBranchTip(ctx, rec.ItemID, in); err != nil && !strings.Contains(err.Error(), ": 404 ") && !strings.Contains(err.Error(), ": 405 ") {
		log.Warn("reporting the branch's new commits failed", "err", err)
		return
	}
	if in.SHA != "" {
		log.Info("the branch has commits after the tested one", "head", shortSHA(rec.Head), "tip", shortSHA(in.SHA), "local", in.Local, "rewritten", in.Rewritten)
	}
	rec.TipReported = key
	w.saveBranch(rec)
}

// mustGit is git's output, "" when it failed.
func mustGit(ctx context.Context, dir string, args ...string) string {
	out, _ := git(ctx, dir, args...)
	return out
}
