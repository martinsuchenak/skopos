package plans

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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

// ErrStaleRevision is returned when an approval names a revision that is no
// longer the latest, or whose snapshot no longer matches the plan's current
// steps — approving would bind Martin to content he did not see.
var ErrStaleRevision = errors.New("revision is stale: a newer revision exists or the plan changed since the snapshot")

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
	// Skills are the repo skills the plan selected (capability profiles
	// §3): part of the approved content — the hash covers them, so the
	// approval binds which playbooks the implementer is given.
	Skills    []string   `json:"skills,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	LockedAt  *time.Time `json:"locked_at,omitempty"`
}

// CreateRevision snapshots the plan's current items as a new revision.
// Refused while the latest revision is locked (review fix 2): unlocking
// approved content is the amendment path — the workflow moves the item back
// to awaiting_approval first, then AmendRevision snapshots.
func (s *Service) CreateRevision(ctx context.Context, planID, baseSHA string) (*Revision, error) {
	if err := s.requirePlanScope(ctx, planID); err != nil {
		return nil, err
	}
	if err := s.requireEditable(ctx, planID); err != nil {
		return nil, err
	}
	return s.snapshot(ctx, planID, baseSHA)
}

// AmendRevision snapshots over a LOCKED latest revision — the amendment
// path. Gated by the amendment guard (wired in serve.go to the inbox
// workflow): the linked item must already be back in awaiting_approval,
// which only the executor's amendment transition can do. A stray caller
// with any in-scope key cannot unlock approved content.
func (s *Service) AmendRevision(ctx context.Context, planID, baseSHA string) (*Revision, error) {
	if err := s.requirePlanScope(ctx, planID); err != nil {
		return nil, err
	}
	latest, err := s.store.LatestRevision(ctx, strings.TrimSpace(planID))
	if err != nil {
		return nil, err
	}
	if latest.LockedAt == nil {
		// Nothing to amend over — a plain snapshot suffices.
		return s.snapshot(ctx, planID, baseSHA)
	}
	if s.amendmentGuard != nil {
		if err := s.amendmentGuard(strings.TrimSpace(planID)); err != nil {
			return nil, err
		}
	}
	return s.snapshot(ctx, planID, baseSHA)
}

// Amend is the atomic amendment operation (1b review round 2, N2): append
// the amended steps and snapshot the new revision past the lock, in ONE
// transaction — the piecemeal path (AddPlanItem under a locked revision)
// was refused by requireEditable and left the item looping at the gate.
// Guarded by the amendment guard: the linked item must be implementing (an
// in-flight amendment) or awaiting_approval.
func (s *Service) Amend(ctx context.Context, planID string, steps []RevisionStep, baseSHA string) (*Revision, error) {
	if err := s.requirePlanScope(ctx, planID); err != nil {
		return nil, err
	}
	planID = strings.TrimSpace(planID)
	latest, err := s.store.LatestRevision(ctx, planID)
	if err != nil {
		return nil, err
	}
	if latest.LockedAt == nil {
		return nil, fmt.Errorf("%w: plan has no locked revision to amend", ErrInvalidInput)
	}
	if s.amendmentGuard != nil {
		if err := s.amendmentGuard(planID); err != nil {
			return nil, err
		}
	}
	amender, ok := s.store.(AmendTxer)
	if !ok {
		return nil, fmt.Errorf("%w: storage does not support amendments", ErrInvalidInput)
	}
	if err := amender.AmendTx(ctx, planID, steps, strings.TrimSpace(baseSHA), s.now().UTC()); err != nil {
		return nil, err
	}
	s.publishPlan(ctx, planID)
	return s.store.LatestRevision(ctx, planID)
}

// SetPlannableGuard wires the planning guard (the linked item must be in
// its planning phase — the sibling of SetAmendmentGuard).
func (s *Service) SetPlannableGuard(fn func(planID string) error) { s.plannableGuard = fn }

// Replan replaces ALL non-review steps and snapshots atomically (round-5
// M6): the first plan and every re-plan go through this one call. Requires
// the latest revision to be unlocked (planning has nothing approved).
func (s *Service) Replan(ctx context.Context, planID string, steps []RevisionStep, baseSHA string, skills ...string) (*Revision, error) {
	if err := s.requirePlanScope(ctx, planID); err != nil {
		return nil, err
	}
	planID = strings.TrimSpace(planID)
	if latest, err := s.store.LatestRevision(ctx, planID); err == nil && latest != nil && latest.LockedAt != nil {
		return nil, ErrRevisionLocked
	}
	if s.plannableGuard != nil {
		if err := s.plannableGuard(planID); err != nil {
			return nil, err
		}
	}
	amender, ok := s.store.(AmendTxer)
	if !ok {
		return nil, fmt.Errorf("%w: storage does not support replacement", ErrInvalidInput)
	}
	if err := amender.ReplaceTx(ctx, planID, steps, strings.TrimSpace(baseSHA), "plan", normalizeSkills(skills), s.now().UTC()); err != nil {
		return nil, err
	}
	s.publishPlan(ctx, planID)
	return s.store.LatestRevision(ctx, planID)
}

// VerifyRevisionCurrent checks the latest revision's snapshot against the
// plan's current steps: an approval may only bind to content that still
// matches what the planner snapshot (review fix 3).
func (s *Service) VerifyRevisionCurrent(ctx context.Context, planID string) (*Revision, error) {
	planID = strings.TrimSpace(planID)
	latest, err := s.store.LatestRevision(ctx, planID)
	if err != nil {
		return nil, err
	}
	plan, err := s.store.GetPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	steps := make([]RevisionStep, 0, len(plan.Items))
	for _, it := range plan.Items {
		steps = append(steps, RevisionStep{ID: it.ID, Title: it.Title, Description: it.Description, Phase: it.Phase, Position: it.Position})
	}
	skills, err := s.planSkills(ctx, planID)
	if err != nil {
		return nil, err
	}
	hash, err := hashContent(steps, skills)
	if err != nil {
		return nil, err
	}
	if hash != latest.ContentHash {
		return nil, ErrStaleRevision
	}
	return latest, nil
}

func (s *Service) snapshot(ctx context.Context, planID, baseSHA string) (*Revision, error) {
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
	skills, err := s.planSkills(ctx, planID)
	if err != nil {
		return nil, err
	}
	hash, err := hashContent(steps, skills)
	if err != nil {
		return nil, err
	}
	rev := Revision{
		Skills:      skills,
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

// hashContent is the revision content hash: the steps, plus the selected
// skills when there are any. With no skills it is exactly hashSteps — every
// revision created before skills existed keeps its hash (an in-flight item
// at the gate must not go stale on upgrade).
func hashContent(steps []RevisionStep, skills []string) (string, error) {
	if len(skills) == 0 {
		return hashSteps(steps)
	}
	buf, err := json.Marshal(struct {
		Steps  []RevisionStep `json:"steps"`
		Skills []string       `json:"skills"`
	}{steps, skills})
	if err != nil {
		return "", fmt.Errorf("encoding revision content: %w", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// normalizeSkills trims, drops empties and duplicates, and sorts — the
// selection is a set, and the hash must not depend on its order.
func normalizeSkills(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SkillsStore is the optional storage capability for plan skills.
type SkillsStore interface {
	PlanSkills(ctx context.Context, planID string) ([]string, error)
}

func (s *Service) planSkills(ctx context.Context, planID string) ([]string, error) {
	ss, ok := s.store.(SkillsStore)
	if !ok {
		return nil, nil
	}
	return ss.PlanSkills(ctx, planID)
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
		INSERT INTO plan_revisions (id, plan_id, workspace_id, revision_no, steps_json, content_hash, base_sha, created_at, locked_at, skills_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.PlanID, r.WorkspaceID, r.RevisionNo, string(stepsJSON), r.ContentHash, r.BaseSHA, r.CreatedAt.Format(time.RFC3339Nano), lockedAt, skillsJSON(r.Skills)); err != nil {
		return fmt.Errorf("inserting plan revision: %w", err)
	}
	return nil
}

func (st *Storage) scanRevision(row interface {
	Scan(dest ...any) error
}) (*Revision, error) {
	var r Revision
	var stepsJSON, createdAt, skills string
	var lockedAt sql.NullString
	if err := row.Scan(&r.ID, &r.PlanID, &r.WorkspaceID, &r.RevisionNo, &stepsJSON, &r.ContentHash, &r.BaseSHA, &createdAt, &lockedAt, &skills); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning plan revision: %w", err)
	}
	if err := json.Unmarshal([]byte(stepsJSON), &r.Steps); err != nil {
		return nil, fmt.Errorf("decoding revision steps: %w", err)
	}
	r.Skills = decodeSkills(skills)
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	if lockedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, lockedAt.String)
		r.LockedAt = &t
	}
	return &r, nil
}

const revisionColumns = `id, plan_id, workspace_id, revision_no, steps_json, content_hash, base_sha, created_at, locked_at, skills_json`

// skillsJSON / decodeSkills: the skills columns hold a JSON array, or ”
// for none (the column default — every pre-skills row).
func skillsJSON(skills []string) string {
	if len(skills) == 0 {
		return ""
	}
	b, _ := json.Marshal(skills)
	return string(b)
}

func decodeSkills(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// PlanSkills reads the plan's current skill selection.
func (st *Storage) PlanSkills(ctx context.Context, planID string) ([]string, error) {
	var s string
	if err := st.db.QueryRowContext(ctx, `SELECT skills FROM plans WHERE id = ?`, planID).Scan(&s); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading plan skills: %w", err)
	}
	return decodeSkills(s), nil
}

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

// AmendTx appends the amended steps and snapshots the new revision in one
// transaction, past the lock (1b review round 2, N2).
// AmendTxer is the storage capability Amend/Replan need (Storage impl).
type AmendTxer interface {
	AmendTx(ctx context.Context, planID string, steps []RevisionStep, baseSHA string, now time.Time) error
	ReplaceTx(ctx context.Context, planID string, steps []RevisionStep, baseSHA string, mode string, skills []string, now time.Time) error
}

// ReplaceTx replaces a plan's steps and snapshots in one transaction.
// mode "amend": keep done + review, replace pending implement/amendment
// items (the locked-revision amendment path). mode "plan": replace ALL
// non-review items and ensure exactly one review step (planning / re-plan —
// requires an unlocked or absent latest revision, checked by the caller).
// skills: mode "plan" sets the plan's selection; mode "amend" ignores it and
// keeps the approved one (an amendment re-approves steps, not playbooks).
func (s *Storage) ReplaceTx(ctx context.Context, planID string, steps []RevisionStep, baseSHA string, mode string, skills []string, now time.Time) error {
	tx, err := s.db.(*sql.DB).BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()
	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(revision_no), 0) + 1 FROM plan_revisions WHERE plan_id = ?`, planID).Scan(&next); err != nil {
		return fmt.Errorf("next revision number: %w", err)
	}
	var wsID, planSkills string
	if err := tx.QueryRowContext(ctx, `SELECT workspace_id, skills FROM plans WHERE id = ?`, planID).Scan(&wsID, &planSkills); err != nil {
		return fmt.Errorf("plan workspace: %w", err)
	}
	if mode == "plan" {
		planSkills = skillsJSON(skills)
		if _, err := tx.ExecContext(ctx, `UPDATE plans SET skills = ? WHERE id = ?`, planSkills, planID); err != nil {
			return fmt.Errorf("setting plan skills: %w", err)
		}
	}
	if mode == "plan" {
		// Planning: nothing is approved or executing yet — replace
		// EVERYTHING. The server-owned review step is re-created below, so a
		// re-plan still ends with exactly one (a non-review-only delete would
		// stack a second one on every re-plan).
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM plan_items WHERE plan_id = ?`,
			planID); err != nil {
			return fmt.Errorf("replacing items: %w", err)
		}
	} else {
		// Amend: the amended steps REPLACE the remaining pending
		// implementation steps (the prompt contract), not append after
		// them. Done steps and the review step keep their positions.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM plan_items
			WHERE plan_id = ? AND status = 'pending' AND phase IN ('implement', 'amendment')`,
			planID); err != nil {
			return fmt.Errorf("superseding pending steps: %w", err)
		}
	}
	// Positions (round-4 R3): the amended steps start AFTER the max
	// position of the remaining non-review items (the done steps), and the
	// review step (with anything after it) shifts past the last new one —
	// no overlap with done positions, ORDER BY position stays unambiguous.
	var base int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(position), 0) FROM plan_items
		WHERE plan_id = ? AND (phase IS NULL OR phase != 'review')`, planID).Scan(&base); err != nil {
		return fmt.Errorf("base position: %w", err)
	}
	pos := base
	if _, err := tx.ExecContext(ctx,
		`UPDATE plan_items SET position = position + ? WHERE plan_id = ? AND phase = 'review'`,
		len(steps), planID); err != nil {
		return fmt.Errorf("shifting review step: %w", err)
	}
	// F3: the phase follows the mode — plan-mode steps (and a server-created
	// review step) are 'implement'/'review'; amendment-mode steps are
	// 'amendment'. The worker stops sending its own review step.
	phase := "amendment"
	if mode == "plan" {
		phase = "implement"
	}
	for _, st := range steps {
		pos++
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO plan_items (id, plan_id, title, description, phase, status, position, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
			ids.New(), planID, st.Title, st.Description, phase, pos, formatTime(now), formatTime(now)); err != nil {
			return fmt.Errorf("inserting amended step: %w", err)
		}
	}
	if mode == "plan" {
		// Exactly one review step at the end, owned by the server.
		pos++
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO plan_items (id, plan_id, title, description, phase, status, position, created_at, updated_at)
			VALUES (?, ?, 'Review and push (Martin)', 'Martin reviews the branch and pushes it onward himself.', 'review', 'pending', ?, ?, ?)`,
			ids.New(), planID, pos, formatTime(now), formatTime(now)); err != nil {
			return fmt.Errorf("inserting review step: %w", err)
		}
	}
	// Snapshot the plan's CURRENT items (the amended set) as the new revision.
	items, err := txItems(ctx, tx, planID)
	if err != nil {
		return err
	}
	snap := make([]RevisionStep, 0, len(items))
	for _, it := range items {
		snap = append(snap, RevisionStep{ID: it.ID, Title: it.Title, Description: it.Description, Phase: it.Phase, Position: it.Position})
	}
	hash, err := hashContent(snap, decodeSkills(planSkills))
	if err != nil {
		return err
	}
	checks, _ := json.Marshal(snap)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO plan_revisions (id, plan_id, workspace_id, revision_no, steps_json, content_hash, base_sha, created_at, skills_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ids.New(), planID, wsID, next, string(checks), hash, baseSHA, formatTime(now), planSkills); err != nil {
		return err
	}
	return tx.Commit()
}

// AmendTx keeps the round-2/3 call sites working (mode "amend").
func (s *Storage) AmendTx(ctx context.Context, planID string, steps []RevisionStep, baseSHA string, now time.Time) error {
	return s.ReplaceTx(ctx, planID, steps, baseSHA, "amend", nil, now)
}

func txItems(ctx context.Context, tx *sql.Tx, planID string) ([]Item, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, title, description, COALESCE(phase, ''), position FROM plan_items WHERE plan_id = ? ORDER BY position`, planID)
	if err != nil {
		return nil, fmt.Errorf("listing items for snapshot: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Title, &it.Description, &it.Phase, &it.Position); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
