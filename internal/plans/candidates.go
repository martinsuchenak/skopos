package plans

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/martinsuchenak/skopos/internal/dbtime"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/ids"
)

// Plan candidates (tmp/multi-agent-stages — design.md §3): each planner of
// a plan group writes a candidate revision WITHOUT touching the plan's live
// steps. The approver picks one; approving materialises its steps as the
// live items (with the candidate's own step ids, so the live plan hashes to
// exactly the candidate), locks it, and supersedes its siblings — one
// transaction. The candidate revision itself is what the approval names.

// Candidate states.
const (
	CandidateOpen       = "open"
	CandidateApproved   = "approved"
	CandidateSuperseded = "superseded"
)

const reviewStepTitle = "Review and push (Martin)"
const reviewStepDescription = "Martin reviews the branch and pushes it onward himself."

// CreateCandidate snapshots a planner's steps as an open candidate of a
// plan group. Like Replan it requires planning (the plannable guard) and an
// unlocked plan; unlike Replan it leaves the live items alone.
func (s *Service) CreateCandidate(ctx context.Context, planID string, steps []RevisionStep, baseSHA string, skills []string, groupID, label string) (*Revision, error) {
	if err := s.requirePlanScope(ctx, planID); err != nil {
		return nil, err
	}
	planID, groupID, label = strings.TrimSpace(planID), strings.TrimSpace(groupID), strings.TrimSpace(label)
	if groupID == "" {
		return nil, fmt.Errorf("%w: group_id is required for a candidate", ErrInvalidInput)
	}
	if len(label) > 64 {
		label = label[:64]
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("%w: a candidate needs at least one step", ErrInvalidInput)
	}
	for _, st := range steps {
		if strings.TrimSpace(st.Title) == "" {
			return nil, fmt.Errorf("%w: step titles are required", ErrInvalidInput)
		}
	}
	if latest, err := s.store.LatestRevision(ctx, planID); err == nil && latest != nil && latest.LockedAt != nil {
		return nil, ErrRevisionLocked
	}
	if s.plannableGuard != nil {
		if err := s.plannableGuard(planID); err != nil {
			return nil, err
		}
	}
	cs, ok := s.store.(CandidateStore)
	if !ok {
		return nil, fmt.Errorf("%w: storage does not support candidates", ErrInvalidInput)
	}
	rev, err := cs.CreateCandidate(ctx, planID, steps, strings.TrimSpace(baseSHA), normalizeSkills(skills), groupID, label, s.now().UTC())
	if err != nil {
		return nil, err
	}
	s.publishPlan(ctx, planID)
	return rev, nil
}

// ApproveCandidate makes an open candidate of the plan's current round the
// plan: live items replaced from its snapshot, hash re-verified, the
// candidate locked and marked approved, its siblings superseded. A
// candidate from an older round, or one already decided, is ErrStaleRevision.
func (s *Service) ApproveCandidate(ctx context.Context, planID, revisionID string) (*Revision, error) {
	if err := s.requirePlanScope(ctx, planID); err != nil {
		return nil, err
	}
	cs, ok := s.store.(CandidateStore)
	if !ok {
		return nil, fmt.Errorf("%w: storage does not support candidates", ErrInvalidInput)
	}
	rev, err := cs.ApproveCandidate(ctx, strings.TrimSpace(planID), strings.TrimSpace(revisionID), s.now().UTC())
	if err != nil {
		return nil, err
	}
	s.publishPlan(ctx, planID)
	return rev, nil
}

// SupersedeOpenCandidates ends a round without a winner (a re-plan was
// requested, or a new round opens).
func (s *Service) SupersedeOpenCandidates(ctx context.Context, planID string) error {
	cs, ok := s.store.(CandidateStore)
	if !ok {
		return nil
	}
	if err := cs.SupersedeOpenCandidates(ctx, strings.TrimSpace(planID)); err != nil {
		return err
	}
	s.publishPlan(ctx, planID)
	return nil
}

// OpenCandidates returns the open candidates of the plan's current round,
// oldest first (the order they are numbered in at the gate).
func (s *Service) OpenCandidates(ctx context.Context, planID string) ([]Revision, error) {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return nil, err
	}
	cs, ok := s.store.(CandidateStore)
	if !ok {
		return nil, nil
	}
	return cs.OpenCandidates(ctx, strings.TrimSpace(planID))
}

// GetRevision returns one revision by id (scope-checked through its plan).
func (s *Service) GetRevision(ctx context.Context, revisionID string) (*Revision, error) {
	rev, err := s.store.GetRevision(ctx, strings.TrimSpace(revisionID))
	if err != nil {
		return nil, err
	}
	if err := s.requirePlanScopeQuiet(ctx, rev.PlanID); err != nil {
		return nil, ErrNotFound
	}
	return rev, nil
}

// CandidateStore is the storage capability candidates need (Storage impl).
type CandidateStore interface {
	CreateCandidate(ctx context.Context, planID string, steps []RevisionStep, baseSHA string, skills []string, groupID, label string, now time.Time) (*Revision, error)
	ApproveCandidate(ctx context.Context, planID, revisionID string, now time.Time) (*Revision, error)
	SupersedeOpenCandidates(ctx context.Context, planID string) error
	OpenCandidates(ctx context.Context, planID string) ([]Revision, error)
}

// candidateSnapshot builds the exact step list a plan-mode replan would
// leave live — implementation steps at positions 1..n, then the
// server-owned review step — with fresh ids that approval reuses.
func candidateSnapshot(steps []RevisionStep) []RevisionStep {
	snap := make([]RevisionStep, 0, len(steps)+1)
	pos := 0
	for _, st := range steps {
		pos++
		snap = append(snap, RevisionStep{ID: ids.New(), Title: strings.TrimSpace(st.Title), Description: st.Description, Phase: "implement", Position: pos})
	}
	snap = append(snap, RevisionStep{ID: ids.New(), Title: reviewStepTitle, Description: reviewStepDescription, Phase: "review", Position: pos + 1})
	return snap
}

func (s *Storage) CreateCandidate(ctx context.Context, planID string, steps []RevisionStep, baseSHA string, skills []string, groupID, label string, now time.Time) (*Revision, error) {
	tx, err := s.db.(*sql.DB).BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision_no), 0) + 1 FROM plan_revisions WHERE plan_id = ?`, planID).Scan(&next); err != nil {
		return nil, fmt.Errorf("next revision number: %w", err)
	}
	var wsID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT workspace_id FROM plans WHERE id = ?`, planID).Scan(&wsID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("plan workspace: %w", err)
	}
	snap := candidateSnapshot(steps)
	hash, err := hashContent(snap, skills)
	if err != nil {
		return nil, err
	}
	stepsJSON, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	rev := Revision{
		ID: ids.New(), PlanID: planID, WorkspaceID: wsID.String, RevisionNo: next, Steps: snap,
		ContentHash: hash, BaseSHA: baseSHA, Skills: skills,
		GroupID: groupID, Label: label, CandidateState: CandidateOpen, CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO plan_revisions (id, plan_id, workspace_id, revision_no, steps_json, content_hash, base_sha, created_at, skills_json,
			group_id, label, candidate_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rev.ID, planID, rev.WorkspaceID, next, string(stepsJSON), hash, baseSHA, formatTime(now), skillsJSON(skills),
		groupID, nullIfEmptyStr(label), CandidateOpen); err != nil {
		return nil, fmt.Errorf("inserting candidate: %w", err)
	}
	return &rev, tx.Commit()
}

func nullIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// currentRoundGroup is the group of the plan's newest candidate revision.
func currentRoundGroup(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, planID string) (string, error) {
	var g sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT group_id FROM plan_revisions WHERE plan_id = ? AND group_id IS NOT NULL ORDER BY revision_no DESC LIMIT 1`, planID).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return g.String, err
}

func (s *Storage) ApproveCandidate(ctx context.Context, planID, revisionID string, now time.Time) (*Revision, error) {
	tx, err := s.db.(*sql.DB).BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rev, err := s.scanRevision(tx.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM plan_revisions WHERE id = ? AND plan_id = ?`, revisionID, planID))
	if err != nil {
		return nil, err
	}
	round, err := currentRoundGroup(ctx, tx, planID)
	if err != nil {
		return nil, err
	}
	if rev.GroupID == "" || rev.CandidateState != CandidateOpen || rev.GroupID != round {
		return nil, ErrStaleRevision
	}
	// The candidate becomes the plan: every live item replaced by its
	// snapshot, with the snapshot's own ids and positions.
	if _, err := tx.ExecContext(ctx, `DELETE FROM plan_items WHERE plan_id = ?`, planID); err != nil {
		return nil, fmt.Errorf("replacing items: %w", err)
	}
	for _, st := range rev.Steps {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO plan_items (id, plan_id, title, description, phase, status, position, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
			st.ID, planID, st.Title, st.Description, nullIfEmptyStr(st.Phase), st.Position, formatTime(now), formatTime(now)); err != nil {
			return nil, fmt.Errorf("materialising candidate step: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plans SET skills = ?, updated_at = ? WHERE id = ?`, skillsJSON(rev.Skills), formatTime(now), planID); err != nil {
		return nil, fmt.Errorf("setting plan skills: %w", err)
	}
	// Re-verify: the live plan must hash to exactly the candidate.
	items, err := txItems(ctx, tx, planID)
	if err != nil {
		return nil, err
	}
	live := make([]RevisionStep, 0, len(items))
	for _, it := range items {
		live = append(live, RevisionStep{ID: it.ID, Title: it.Title, Description: it.Description, Phase: it.Phase, Position: it.Position})
	}
	hash, err := hashContent(live, rev.Skills)
	if err != nil {
		return nil, err
	}
	if hash != rev.ContentHash {
		return nil, ErrStaleRevision
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE plan_revisions SET candidate_state = ?, locked_at = ? WHERE id = ?`,
		CandidateApproved, dbtime.Format(now), revisionID); err != nil {
		return nil, fmt.Errorf("approving candidate: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE plan_revisions SET candidate_state = ? WHERE group_id = ? AND id != ? AND candidate_state = ?`,
		CandidateSuperseded, rev.GroupID, revisionID, CandidateOpen); err != nil {
		return nil, fmt.Errorf("superseding siblings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rev.CandidateState = CandidateApproved
	locked := now
	rev.LockedAt = &locked
	return rev, nil
}

func (s *Storage) SupersedeOpenCandidates(ctx context.Context, planID string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE plan_revisions SET candidate_state = ? WHERE plan_id = ? AND candidate_state = ?`,
		CandidateSuperseded, planID, CandidateOpen); err != nil {
		return fmt.Errorf("superseding candidates: %w", err)
	}
	return nil
}

func (s *Storage) OpenCandidates(ctx context.Context, planID string) ([]Revision, error) {
	round, err := currentRoundGroup(ctx, s.db.(*sql.DB), planID)
	if err != nil || round == "" {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+revisionColumns+` FROM plan_revisions WHERE plan_id = ? AND group_id = ? AND candidate_state = ? ORDER BY revision_no ASC`,
		planID, round, CandidateOpen)
	if err != nil {
		return nil, fmt.Errorf("listing candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Revision
	for rows.Next() {
		r, err := s.scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
