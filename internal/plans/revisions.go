package plans

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/ids"
)

// Plan revisions (docs/design/agent-pipeline.md §4). A revision is an
// immutable snapshot of a plan's steps plus the base commit the planner
// read; approving a revision locks it, and any later structural change
// becomes a new revision (amendment) that supersedes the locked one.

// ErrRevisionLocked is returned when a structural edit (add/remove item,
// dependency change) hits a plan whose latest revision is locked. Status and
// claim updates stay legal — execution must be able to progress.
var ErrRevisionLocked = errors.New("plan revision is locked: structural changes require an amendment (a new revision)")

// RevisionStep is one step in the immutable snapshot. Status is deliberately
// absent: it is execution state, not plan content.
type RevisionStep struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Phase       string `json:"phase,omitempty"`
	Position    int    `json:"position"`
}

// Revision is a snapshot with its content hash and base commit. LockedAt is
// set when an approval locks the revision (nil = still editable history).
type Revision struct {
	ID          string         `json:"id"`
	PlanID      string         `json:"plan_id"`
	WorkspaceID string         `json:"workspace_id,omitempty"`
	RevisionNo  int            `json:"revision_no"`
	Steps       []RevisionStep `json:"steps"`
	ContentHash string         `json:"content_hash"`
	BaseSHA     string         `json:"base_sha,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	LockedAt    *time.Time     `json:"locked_at,omitempty"`
}

// CreateRevision snapshots the plan's current items as a new revision.
// Creating a revision over a locked one is the amendment path: the new
// revision is unlocked and supersedes the old (the item's return to
// awaiting_approval is the caller's workflow move).
func (s *Service) CreateRevision(ctx context.Context, planID, baseSHA string) (*Revision, error) {
	if err := s.requirePlanScope(ctx, planID); err != nil {
		return nil, err
	}
	planID = strings.TrimSpace(planID)
	plan, err := s.store.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if len(plan.Items) == 0 {
		return nil, fmt.Errorf("%w: a revision needs at least one step", ErrInvalidInput)
	}
	steps := make([]RevisionStep, 0, len(plan.Items))
	for _, it := range plan.Items {
		steps = append(steps, RevisionStep{ID: it.ID, Title: it.Title, Description: it.Description, Phase: it.Phase, Position: it.Position})
	}
	hash, err := hashSteps(steps)
	if err != nil {
		return nil, err
	}
	rev := Revision{
		ID:          ids.New(),
		PlanID:      planID,
		WorkspaceID: plan.WorkspaceID,
		RevisionNo:  1,
		Steps:       steps,
		ContentHash: hash,
		BaseSHA:     strings.TrimSpace(baseSHA),
		CreatedAt:   s.now().UTC(),
	}
	if latest, err := s.store.LatestRevision(ctx, planID); err == nil && latest != nil {
		rev.RevisionNo = latest.RevisionNo + 1
	}
	if err := s.store.CreateRevision(ctx, rev); err != nil {
		return nil, err
	}
	s.publishPlan(ctx, planID)
	return &rev, nil
}

func hashSteps(steps []RevisionStep) (string, error) {
	// Deterministic encoding: sorted key order comes from the struct tags.
	buf, err := json.Marshal(steps)
	if err != nil {
		return "", fmt.Errorf("encoding revision steps: %w", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// LatestRevision returns the plan's newest revision, or ErrNotFound when the
// plan has none (interactive plans).
func (s *Service) LatestRevision(ctx context.Context, planID string) (*Revision, error) {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return nil, err
	}
	return s.store.LatestRevision(ctx, strings.TrimSpace(planID))
}

// ListRevisions returns the plan's revisions, newest first.
func (s *Service) ListRevisions(ctx context.Context, planID string) ([]Revision, error) {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return nil, err
	}
	revs, err := s.store.ListRevisions(ctx, strings.TrimSpace(planID))
	if err != nil {
		return nil, err
	}
	if revs == nil {
		revs = []Revision{}
	}
	return revs, nil
}

// LockRevision freezes a revision against structural edits (called when an
// approval names it). Idempotent: locking a locked revision is a no-op.
func (s *Service) LockRevision(ctx context.Context, revisionID string) error {
	revisionID = strings.TrimSpace(revisionID)
	if revisionID == "" {
		return fmt.Errorf("%w: revision id is required", ErrInvalidInput)
	}
	rev, err := s.store.GetRevision(ctx, revisionID)
	if err != nil {
		return err
	}
	if err := s.requirePlanScope(ctx, rev.PlanID); err != nil {
		return err
	}
	if rev.LockedAt != nil {
		return nil
	}
	if err := s.store.LockRevision(ctx, revisionID, s.now().UTC()); err != nil {
		return err
	}
	s.publishPlan(ctx, rev.PlanID)
	return nil
}

// requireEditable guards structural plan edits: a locked latest revision
// means the plan is approved content — deviations are amendments, created as
// new revisions by the workflow, never in place.
func (s *Service) requireEditable(ctx context.Context, planID string) error {
	latest, err := s.store.LatestRevision(ctx, planID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // no revisions: interactive plan, freely editable
		}
		return err
	}
	if latest.LockedAt != nil {
		return ErrRevisionLocked
	}
	return nil
}

// --- storage ---

func (st *Storage) CreateRevision(ctx context.Context, r Revision) error {
	stepsJSON, err := json.Marshal(r.Steps)
	if err != nil {
		return fmt.Errorf("encoding revision steps: %w", err)
	}
	var lockedAt any
	if r.LockedAt != nil {
		lockedAt = r.LockedAt.Format(time.RFC3339Nano)
	}
	if _, err := st.db.ExecContext(ctx, `
		INSERT INTO plan_revisions (id, plan_id, workspace_id, revision_no, steps_json, content_hash, base_sha, created_at, locked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.PlanID, r.WorkspaceID, r.RevisionNo, string(stepsJSON), r.ContentHash, r.BaseSHA, r.CreatedAt.Format(time.RFC3339Nano), lockedAt); err != nil {
		return fmt.Errorf("inserting plan revision: %w", err)
	}
	return nil
}

func (st *Storage) scanRevision(row interface {
	Scan(dest ...any) error
}) (*Revision, error) {
	var r Revision
	var stepsJSON, createdAt string
	var lockedAt sql.NullString
	if err := row.Scan(&r.ID, &r.PlanID, &r.WorkspaceID, &r.RevisionNo, &stepsJSON, &r.ContentHash, &r.BaseSHA, &createdAt, &lockedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning plan revision: %w", err)
	}
	if err := json.Unmarshal([]byte(stepsJSON), &r.Steps); err != nil {
		return nil, fmt.Errorf("decoding revision steps: %w", err)
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	if lockedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, lockedAt.String)
		r.LockedAt = &t
	}
	return &r, nil
}

const revisionColumns = `id, plan_id, workspace_id, revision_no, steps_json, content_hash, base_sha, created_at, locked_at`

func (st *Storage) LatestRevision(ctx context.Context, planID string) (*Revision, error) {
	row := st.db.QueryRowContext(ctx,
		`SELECT `+revisionColumns+` FROM plan_revisions WHERE plan_id = ? ORDER BY revision_no DESC LIMIT 1`, planID)
	return st.scanRevision(row)
}

func (st *Storage) GetRevision(ctx context.Context, id string) (*Revision, error) {
	row := st.db.QueryRowContext(ctx,
		`SELECT `+revisionColumns+` FROM plan_revisions WHERE id = ?`, id)
	return st.scanRevision(row)
}

func (st *Storage) ListRevisions(ctx context.Context, planID string) ([]Revision, error) {
	rows, err := st.db.QueryContext(ctx,
		`SELECT `+revisionColumns+` FROM plan_revisions WHERE plan_id = ? ORDER BY revision_no DESC`, planID)
	if err != nil {
		return nil, fmt.Errorf("listing plan revisions: %w", err)
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := st.scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func (st *Storage) LockRevision(ctx context.Context, id string, at time.Time) error {
	res, err := st.db.ExecContext(ctx,
		`UPDATE plan_revisions SET locked_at = ? WHERE id = ? AND locked_at IS NULL`,
		at.Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("locking plan revision: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int
		if err := st.db.QueryRowContext(ctx, `SELECT 1 FROM plan_revisions WHERE id = ?`, id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
	}
	return nil
}
