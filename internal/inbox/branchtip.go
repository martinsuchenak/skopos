package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/rest"
)

// BranchTip is where an item's branch stands after its review and QA, when
// it is not the commit they tested: a developer committed to it afterwards,
// pushed to origin or, with a branch checkout, in the worker's clone
// (Local). The worker that implemented the item notices it between runs and
// reports it; the sign-off card names the commits and offers to check them.
type BranchTip struct {
	// Head: the commit review and QA tested (the implemented head).
	Head string `json:"head"`
	// SHA: the branch's tip now.
	SHA string `json:"sha"`
	// Local: the tip is in the worker's clone only, not pushed.
	Local bool `json:"local,omitempty"`
	// Rewritten: the branch no longer holds Head (a force-push, a rebase).
	Rewritten  bool        `json:"rewritten,omitempty"`
	Worker     string      `json:"worker,omitempty"`
	Commits    []TipCommit `json:"commits,omitempty"`
	ReportedAt time.Time   `json:"reported_at"`
}

// TipCommit is one commit on the branch after the tested head.
type TipCommit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// BranchTipInput is the worker's report. A tip equal to the head (or none)
// clears what was reported before.
type BranchTipInput struct {
	Head      string      `json:"head"`
	SHA       string      `json:"sha"`
	Local     bool        `json:"local"`
	Rewritten bool        `json:"rewritten"`
	Worker    string      `json:"worker"`
	Commits   []TipCommit `json:"commits"`
}

// maxTipCommits bounds the commits a report keeps.
const maxTipCommits = 50

// AdoptQueuer is the run queue's side of taking in commits made after the
// implementation (internal/runs.Service satisfies it).
type AdoptQueuer interface {
	QueueAdopt(ctx context.Context, itemID, workspaceID, planRevID, baseSHA, branch, from, prefer string) (string, error)
	ImplementedHead(ctx context.Context, itemID string) (string, error)
}

// RecordBranchTip stores where the item's branch stands (the worker's
// report). Any key with the item's workspace in scope may report it, as for
// the outcome; never a run's own token.
func (s *Service) RecordBranchTip(ctx context.Context, itemID string, in BranchTipInput) (*BranchTip, error) {
	if p := auth.PrincipalFromContext(ctx); p != nil && p.Run != nil {
		return nil, fmt.Errorf("%w: a run does not report the branch's tip", auth.ErrOutOfScope)
	}
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	in.Head, in.SHA, in.Worker = clipOutcome(in.Head, 64), clipOutcome(in.SHA, 64), clipOutcome(in.Worker, 200)
	if in.SHA == "" || in.SHA == in.Head {
		return nil, s.store.PutBranchTip(ctx, itemID, "", nil)
	}
	if in.Head == "" {
		return nil, fmt.Errorf("%w: head is required with a tip", ErrInvalidInput)
	}
	tip := BranchTip{Head: in.Head, SHA: in.SHA, Local: in.Local, Rewritten: in.Rewritten, Worker: in.Worker, ReportedAt: time.Now().UTC()}
	for _, c := range in.Commits {
		if c.SHA = clipOutcome(c.SHA, 64); c.SHA == "" {
			continue
		}
		if len(tip.Commits) == maxTipCommits {
			break
		}
		tip.Commits = append(tip.Commits, TipCommit{SHA: c.SHA, Author: clipOutcome(c.Author, 200), Subject: clipOutcome(c.Subject, 300)})
	}
	ws, err := s.store.ItemWorkspace(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if err := s.store.PutBranchTip(ctx, itemID, ws, &tip); err != nil {
		return nil, err
	}
	return &tip, nil
}

// CheckCommits takes in the commits made on the branch after review and QA
// (approver, at sign-off): an implement run checks them — setup and the
// checks on the branch as it is, a fix round when a check fails, the push —
// and review and QA run again on the new head. Audited inbox.check_commits.
func (s *Service) CheckCommits(ctx context.Context, itemID, via string) (*Item, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	item, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if err := auth.RequireApprover(ctx); err != nil {
		return nil, err
	}
	if item.Status != StatusInReview {
		return nil, fmt.Errorf("%w: checking new commits applies to items at sign-off (item is %s)", ErrInvalidInput, statusLabel(item.Status))
	}
	tip := item.BranchTip
	if tip == nil {
		return nil, fmt.Errorf("%w: no commits after the tested one are known for this item", ErrInvalidInput)
	}
	if tip.Rewritten {
		return nil, fmt.Errorf("%w: the branch no longer holds the tested commit %s; request changes instead", ErrInvalidInput, shortSHA(tip.Head))
	}
	aq, ok := s.runQueuer.(AdoptQueuer)
	if !ok {
		return nil, fmt.Errorf("%w: this server does not queue runs", ErrInvalidInput)
	}
	if head, herr := aq.ImplementedHead(ctx, item.ID); herr != nil {
		return nil, herr
	} else if head != tip.Head {
		return nil, fmt.Errorf("%w: the item's head is %s now, not %s: wait for the worker's next look at the branch", ErrInvalidInput, shortSHA(head), shortSHA(tip.Head))
	}
	prefer := ""
	if tip.Local {
		prefer = tip.Worker
	}
	revID, baseSHA := s.approvedRevision(ctx, item)
	runID, err := aq.QueueAdopt(ctx, item.ID, item.WorkspaceID, revID, baseSHA, "", tip.Head, prefer)
	if err != nil {
		return nil, err
	}
	note := fmt.Sprintf("Checking %s after %s", pluralWord(len(tip.Commits), "commit"), shortSHA(tip.Head))
	if tip.Local {
		note += " in " + tip.Worker + "'s clone"
	}
	out, err := s.transitionWith(ctx, item.ID, StatusImplementing, ActorHuman, "inbox.check_commits", via, note, "", nil)
	if err != nil {
		_ = s.runQueuer.CancelRun(ctx, runID, "check-commits transition failed")
		return nil, err
	}
	_ = s.store.PutBranchTip(ctx, item.ID, "", nil)
	return out, nil
}

func pluralWord(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// PutBranchTip implements Store.PutBranchTip: tip nil clears the record.
func (s *Storage) PutBranchTip(ctx context.Context, itemID, workspaceID string, tip *BranchTip) error {
	if tip == nil {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM item_branch_tips WHERE item_id = ?`, itemID); err != nil {
			return fmt.Errorf("clearing the branch's tip: %w", err)
		}
		return nil
	}
	commits, err := json.Marshal(tip.Commits)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO item_branch_tips (item_id, workspace_id, head, tip, local, rewritten, worker, commits, reported_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (item_id) DO UPDATE SET workspace_id = excluded.workspace_id, head = excluded.head, tip = excluded.tip,
			local = excluded.local, rewritten = excluded.rewritten, worker = excluded.worker, commits = excluded.commits,
			reported_ms = excluded.reported_ms`,
		itemID, workspaceID, tip.Head, tip.SHA, boolInt(tip.Local), boolInt(tip.Rewritten), tip.Worker, string(commits), tip.ReportedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("recording the branch's tip: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// branchTipFrom builds the item's tip from its joined columns (nil when
// none is reported).
func branchTipFrom(head, sha, worker, commits sql.NullString, local, rewritten, reported sql.NullInt64) *BranchTip {
	if !sha.Valid || sha.String == "" {
		return nil
	}
	t := &BranchTip{Head: head.String, SHA: sha.String, Local: local.Int64 != 0, Rewritten: rewritten.Int64 != 0, Worker: worker.String}
	if reported.Valid {
		t.ReportedAt = time.UnixMilli(reported.Int64).UTC()
	}
	if commits.Valid && commits.String != "" {
		_ = json.Unmarshal([]byte(commits.String), &t.Commits)
	}
	return t
}

// PutBranchTip handles PUT /api/inbox/{id}/branch-tip: the worker's report
// of where the item's branch stands.
func (h *Handler) PutBranchTip(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in BranchTipInput
	if err := rest.DecodeJSON(w, r, &in); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tip, err := h.service.RecordBranchTip(r.Context(), r.PathValue("id"), in)
	if err != nil {
		h.respondTransition(w, nil, err)
		return
	}
	if tip == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	rest.RespondJSON(w, http.StatusOK, tip)
}

// CheckCommits handles POST /api/inbox/{id}/check-commits (approver).
func (h *Handler) CheckCommits(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	item, err := h.service.CheckCommits(r.Context(), r.PathValue("id"), viaFrom(r, ""))
	h.respondTransition(w, item, err)
}
