package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// Outcome is what became of an item's branch after sign-off, as the worker
// that implemented it saw on the forge: merged, closed without merging, or
// still open when it stopped watching. Recorded for later rules and
// analyses; nothing decides on it.
type Outcome struct {
	ItemID     string     `json:"item_id"`
	State      string     `json:"state"` // merged | closed | open
	PRURL      string     `json:"pr_url,omitempty"`
	MergeSHA   string     `json:"merge_sha,omitempty"`
	MergedAt   *time.Time `json:"merged_at,omitempty"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
	ReportedAt time.Time  `json:"reported_at"`
	ReportedBy string     `json:"reported_by,omitempty"`
}

// BranchOutcome is the outcome as an item carries it (Item.Outcome).
type BranchOutcome struct {
	State    string     `json:"state"`
	PRURL    string     `json:"pr_url,omitempty"`
	MergeSHA string     `json:"merge_sha,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	ClosedAt *time.Time `json:"closed_at,omitempty"`
}

// Outcome states.
const (
	OutcomeMerged = "merged"
	OutcomeClosed = "closed"
	OutcomeOpen   = "open"
)

// OutcomeInput is a worker's report.
type OutcomeInput struct {
	State    string     `json:"state"`
	PRURL    string     `json:"pr_url"`
	MergeSHA string     `json:"merge_sha"`
	MergedAt *time.Time `json:"merged_at"`
	ClosedAt *time.Time `json:"closed_at"`
	// Reporter names the worker (its id), for the record.
	Reporter string `json:"reporter"`
}

// RecordOutcome stores what became of the item's branch. Any key with the
// item's workspace in scope may report it (the worker's, as for its system
// transitions), never a run's own token: an agent does not say what became
// of its work. A changed state is audited (inbox.outcome).
func (s *Service) RecordOutcome(ctx context.Context, itemID string, in OutcomeInput) (*Outcome, error) {
	if p := auth.PrincipalFromContext(ctx); p != nil && p.Run != nil {
		return nil, fmt.Errorf("%w: a run does not report an item's outcome", auth.ErrOutOfScope)
	}
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	switch in.State {
	case OutcomeMerged, OutcomeClosed, OutcomeOpen:
	default:
		return nil, fmt.Errorf("%w: state must be merged, closed or open", ErrInvalidInput)
	}
	in.PRURL, in.MergeSHA, in.Reporter = clipOutcome(in.PRURL, 500), clipOutcome(in.MergeSHA, 64), clipOutcome(in.Reporter, 200)
	ws, err := s.store.ItemWorkspace(ctx, itemID)
	if err != nil {
		return nil, err
	}
	before, err := s.store.GetOutcome(ctx, itemID)
	if err != nil {
		return nil, err
	}
	out := Outcome{ItemID: itemID, State: in.State, PRURL: in.PRURL, MergeSHA: in.MergeSHA, MergedAt: in.MergedAt, ClosedAt: in.ClosedAt, ReportedAt: time.Now().UTC(), ReportedBy: in.Reporter}
	if err := s.store.PutOutcome(ctx, ws, out); err != nil {
		return nil, err
	}
	if before == nil || before.State != out.State {
		note := "the branch's pull request is " + out.State
		if out.PRURL != "" {
			note += ": " + out.PRURL
		}
		s.auditItem(ctx, itemID, "inbox.outcome", "worker", note, in.Reporter)
	}
	return &out, nil
}

// GetOutcome reads what became of the item's branch (nil = nothing reported).
func (s *Service) GetOutcome(ctx context.Context, itemID string) (*Outcome, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	return s.store.GetOutcome(ctx, itemID)
}

func clipOutcome(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func msPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

// PutOutcome implements Store.PutOutcome.
func (s *Storage) PutOutcome(ctx context.Context, workspaceID string, o Outcome) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO item_outcomes (item_id, workspace_id, state, pr_url, merge_sha, merged_ms, closed_ms, reported_ms, reported_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (item_id) DO UPDATE SET workspace_id = excluded.workspace_id, state = excluded.state, pr_url = excluded.pr_url,
			merge_sha = excluded.merge_sha, merged_ms = excluded.merged_ms, closed_ms = excluded.closed_ms,
			reported_ms = excluded.reported_ms, reported_by = excluded.reported_by`,
		o.ItemID, workspaceID, o.State, o.PRURL, o.MergeSHA, msPtr(o.MergedAt), msPtr(o.ClosedAt), o.ReportedAt.UnixMilli(), o.ReportedBy)
	if err != nil {
		return fmt.Errorf("recording the item's outcome: %w", err)
	}
	return nil
}

// GetOutcome implements Store.GetOutcome.
func (s *Storage) GetOutcome(ctx context.Context, itemID string) (*Outcome, error) {
	var o Outcome
	var merged, closed sql.NullInt64
	var reported int64
	err := s.db.QueryRowContext(ctx, `SELECT item_id, state, pr_url, merge_sha, merged_ms, closed_ms, reported_ms, reported_by FROM item_outcomes WHERE item_id = ?`, itemID).
		Scan(&o.ItemID, &o.State, &o.PRURL, &o.MergeSHA, &merged, &closed, &reported, &o.ReportedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the item's outcome: %w", err)
	}
	at := func(v sql.NullInt64) *time.Time {
		if !v.Valid {
			return nil
		}
		t := time.UnixMilli(v.Int64).UTC()
		return &t
	}
	o.MergedAt, o.ClosedAt, o.ReportedAt = at(merged), at(closed), time.UnixMilli(reported).UTC()
	return &o, nil
}
