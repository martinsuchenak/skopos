package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What became of a signed-off item's branch: after the sweep cleans up a
// done item that has a pull request, the worker keeps watching the pull
// request and reports to the server when it is merged or closed, or that it
// is still open when the watch ends. Recorded for later rules and analyses
// (the server's inbox.Outcome); nothing decides on it.

const (
	// outcomeCheckEvery spaces the lookups of one pull request.
	outcomeCheckEvery = time.Hour
	// outcomeWatch: how long after sign-off a pull request is watched; one
	// still open then is reported as open and no longer watched.
	outcomeWatch = 30 * 24 * time.Hour
)

// outcomeWatchRecord is one signed-off item's pull request being watched.
type outcomeWatchRecord struct {
	ItemID      string `json:"item_id"`
	WorkspaceID string `json:"workspace_id"`
	PRNumber    int    `json:"pr_number"`
	PRURL       string `json:"pr_url"`
	SinceMS     int64  `json:"since_ms"`
	CheckedMS   int64  `json:"checked_ms,omitempty"`
}

func (w *Worker) outcomesDir() string { return filepath.Join(w.cfg.StateDir, "outcomes") }

// watchOutcome starts watching a done item's pull request.
func (w *Worker) watchOutcome(rec branchRecord, now time.Time) {
	if rec.PRNumber <= 0 || !reRecordID.MatchString(rec.ItemID) {
		return
	}
	w.saveWatch(outcomeWatchRecord{ItemID: rec.ItemID, WorkspaceID: rec.WorkspaceID, PRNumber: rec.PRNumber, PRURL: rec.PRURL, SinceMS: now.UnixMilli()})
}

func (w *Worker) saveWatch(o outcomeWatchRecord) {
	b, err := json.Marshal(o)
	if err != nil {
		return
	}
	_ = os.MkdirAll(w.outcomesDir(), 0o700)
	_ = os.WriteFile(filepath.Join(w.outcomesDir(), o.ItemID+".json"), b, 0o600)
}

func (w *Worker) dropWatch(itemID string) {
	if reRecordID.MatchString(itemID) {
		_ = os.Remove(filepath.Join(w.outcomesDir(), itemID+".json"))
	}
}

// sweepOutcomes looks up each watched pull request (at most once per
// outcomeCheckEvery) and reports a merged or closed one, or an open one at
// the end of the watch. A lookup or report that fails for a passing reason
// keeps the watch for the next sweep.
func (w *Worker) sweepOutcomes(ctx context.Context, now time.Time) {
	entries, err := os.ReadDir(w.outcomesDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !reRecordID.MatchString(id) {
			continue
		}
		var o outcomeWatchRecord
		b, err := os.ReadFile(filepath.Join(w.outcomesDir(), e.Name()))
		if err != nil || json.Unmarshal(b, &o) != nil || o.ItemID != id {
			w.dropWatch(id)
			continue
		}
		if o.CheckedMS != 0 && now.Sub(time.UnixMilli(o.CheckedMS)) < outcomeCheckEvery {
			continue
		}
		log := w.log.With("item", o.ItemID, "pr", o.PRURL)
		repo := w.repo(o.WorkspaceID)
		if repo == nil {
			w.dropWatch(id)
			continue
		}
		c, err := newPRClient(repo.Push.Kind, repo.Push.RemoteURL, os.Getenv(repo.Push.TokenEnv))
		if err != nil {
			log.Info("no longer watching the pull request: the repo's push config does not reach it", "err", err)
			w.dropWatch(id)
			continue
		}
		pr, err := c.Get(ctx, o.PRNumber)
		var pe *prError
		switch {
		case errors.As(err, &pe) && pe.Status < 500:
			log.Info("no longer watching the pull request: the forge refuses the lookup", "err", err)
			w.dropWatch(id)
			continue
		case err != nil:
			continue // an outage: the next sweep tries again
		}
		o.CheckedMS = now.UnixMilli()
		in, done := outcomeOf(pr, now.Sub(time.UnixMilli(o.SinceMS)) >= outcomeWatch)
		if !done {
			w.saveWatch(o)
			continue
		}
		if in.PRURL == "" {
			in.PRURL = o.PRURL
		}
		in.Reporter = w.cfg.ID
		if err := w.client.ReportOutcome(ctx, o.ItemID, in); err != nil {
			// A server from before outcomes, or an item gone or out of
			// scope: nothing to report to.
			if strings.Contains(err.Error(), ": 404 ") || strings.Contains(err.Error(), ": 405 ") {
				w.dropWatch(id)
				continue
			}
			log.Warn("reporting the pull request's outcome failed; retrying next sweep", "err", err)
			w.saveWatch(o)
			continue
		}
		log.Info("reported what became of the branch", "state", in.State)
		w.dropWatch(id)
	}
}

// OutcomeReport is what the worker reports (PUT /api/inbox/{id}/outcome).
type OutcomeReport struct {
	State    string     `json:"state"`
	PRURL    string     `json:"pr_url,omitempty"`
	MergeSHA string     `json:"merge_sha,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	Reporter string     `json:"reporter,omitempty"`
}

// outcomeOf reads a pull request: merged or closed is final; open is
// reported only when the watch ends (final).
func outcomeOf(pr *PullRequest, final bool) (OutcomeReport, bool) {
	at := func(s *string) *time.Time {
		if s == nil {
			return nil
		}
		t, err := time.Parse(time.RFC3339, *s)
		if err != nil {
			return nil
		}
		t = t.UTC()
		return &t
	}
	out := OutcomeReport{PRURL: pr.URL, ClosedAt: at(pr.ClosedAt)}
	switch {
	case pr.Merged || pr.MergedAt != nil:
		out.State, out.MergeSHA, out.MergedAt = "merged", pr.MergeCommitSHA, at(pr.MergedAt)
		return out, true
	case pr.State == "closed":
		out.State = "closed"
		return out, true
	case final:
		out.State = "open"
		return out, true
	}
	return out, false
}

// Get reads one pull request by number.
func (c *prClient) Get(ctx context.Context, number int) (*PullRequest, error) {
	var pr PullRequest
	if err := c.call(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", c.repoPath(), number), nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// ReportOutcome reports what became of an item's branch.
func (c *Client) ReportOutcome(ctx context.Context, itemID string, in OutcomeReport) error {
	return c.do(ctx, http.MethodPut, "/api/inbox/"+itemID+"/outcome", in, nil)
}
