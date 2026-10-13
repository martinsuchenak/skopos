package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Storage persists runs. The queue's correctness lives in Claim: the
// pick-and-mark happens in one transaction so two workers can never hold the
// same run, whatever their timing.
type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

func formatTime(t time.Time) string { return t.Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

const runColumns = `id, workspace_id, item_id, plan_rev_id, stage, runner, worker_id, status,
	resume_kind, lease_expires_at, attempt, branch, base_sha, head_sha,
	result_summary, diffstat, checks, log_ref, fail_reason, created_at, updated_at`

func scanRun(row interface{ Scan(dest ...any) error }) (*Run, error) {
	var r Run
	var lease, createdAt, updatedAt string
	var checks string
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.ItemID, &r.PlanRevID, &r.Stage, &r.Runner, &r.WorkerID, &r.Status,
		&r.ResumeKind, &lease, &r.Attempt, &r.Branch, &r.BaseSHA, &r.HeadSHA,
		&r.ResultSummary, &r.Diffstat, &checks, &r.LogRef, &r.FailReason, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning run: %w", err)
	}
	r.LeaseExpires = parseTime(lease)
	r.CreatedAt = parseTime(createdAt)
	r.UpdatedAt = parseTime(updatedAt)
	if checks != "" && checks != "[]" {
		_ = json.Unmarshal([]byte(checks), &r.Checks)
	}
	return &r, nil
}

// Create inserts a queued run.
func (s *Storage) Create(ctx context.Context, r Run) error {
	checks, err := json.Marshal(r.Checks)
	if err != nil {
		checks = []byte("[]")
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, workspace_id, item_id, plan_rev_id, stage, runner, status, attempt,
			branch, base_sha, checks, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkspaceID, r.ItemID, r.PlanRevID, r.Stage, r.Runner, StatusQueued, 1,
		r.Branch, r.BaseSHA, string(checks), formatTime(r.CreatedAt), formatTime(r.UpdatedAt)); err != nil {
		return fmt.Errorf("inserting run: %w", err)
	}
	return nil
}

// Get returns one run by id.
func (s *Storage) Get(ctx context.Context, id string) (*Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// List returns runs filtered by status and/or item; newest first.
func (s *Storage) List(ctx context.Context, status Status, itemID string) ([]Run, error) {
	q := `SELECT ` + runColumns + ` FROM runs WHERE 1=1`
	var args []any
	if status != "" {
		q += ` AND status = ?`
		args = append(args, string(status))
	}
	if itemID != "" {
		q += ` AND item_id = ?`
		args = append(args, itemID)
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing runs: %w", err)
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// Claim atomically claims a run for the worker: candidate selection and the
// status/lease/owner write happen in one transaction. Attempt increments on
// re-claims (interrupted/paused), carrying retry history.
func (s *Storage) Claim(ctx context.Context, in ClaimInput, prefs map[string]*WorkspacePrefs, lease time.Duration, now time.Time) (*Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Expire stale leases first so their runs re-enter the candidate pool
	// (as interrupted, affinity kept).
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, resume_kind = ?, updated_at = ?
		WHERE status IN (?, ?) AND lease_expires_at != '' AND lease_expires_at < ?`,
		string(StatusInterrupted), ResumeSession, formatTime(now),
		string(StatusClaimed), string(StatusRunning), formatTime(now)); err != nil {
		return nil, fmt.Errorf("expiring leases: %w", err)
	}

	// Re-read candidates inside the transaction.
	var cand *Run
	for _, status := range []Status{StatusPaused, StatusInterrupted} {
		row := tx.QueryRowContext(ctx, `
			SELECT `+runColumns+` FROM runs WHERE worker_id = ? AND status = ?
			ORDER BY created_at ASC LIMIT 1`, in.WorkerID, string(status))
		r, err := scanRun(row)
		if err == nil {
			cand = r
			break
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if cand == nil {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+runColumns+` FROM runs WHERE status = ? ORDER BY created_at ASC`, string(StatusQueued))
		if err != nil {
			return nil, fmt.Errorf("queue scan: %w", err)
		}
		var queued []*Run
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			queued = append(queued, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		runnerSet := map[string]bool{}
		for _, r := range in.Runners {
			runnerSet[r] = true
		}
		wsSet := map[string]bool{}
		for _, w := range in.Workspaces {
			wsSet[w] = true
		}
		for _, r := range queued {
			if !wsSet[r.WorkspaceID] {
				continue
			}
			pref := prefs[r.WorkspaceID].StageRunner(r.Stage)
			if pref == "" || runnerSet[pref] {
				cand = r
				break
			}
		}
	}
	if cand == nil {
		// Foreign interrupted runs, branch-resumable only: the owner's
		// session lives elsewhere, so a takeover rebuilds from the branch.
		rows, err := tx.QueryContext(ctx, `
			SELECT `+runColumns+` FROM runs
			WHERE status = ? AND resume_kind = ? ORDER BY created_at ASC`,
			string(StatusInterrupted), ResumeBranch)
		if err != nil {
			return nil, fmt.Errorf("takeover scan: %w", err)
		}
		var takeover []*Run
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			takeover = append(takeover, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		runnerSet := map[string]bool{}
		for _, r := range in.Runners {
			runnerSet[r] = true
		}
		wsSet := map[string]bool{}
		for _, w := range in.Workspaces {
			wsSet[w] = true
		}
		for _, r := range takeover {
			if !wsSet[r.WorkspaceID] {
				continue
			}
			pref := prefs[r.WorkspaceID].StageRunner(r.Stage)
			if pref == "" || runnerSet[pref] {
				cand = r
				break
			}
		}
	}
	if cand == nil {
		return nil, nil // nothing for this worker
	}

	attempt := cand.Attempt
	resumeKind := cand.ResumeKind
	if cand.Status == StatusPaused || cand.Status == StatusInterrupted {
		attempt++ // a resumed run is a new attempt
	} else {
		resumeKind = ResumeSession // first claim: only the owner may resume
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, worker_id = ?, resume_kind = ?, lease_expires_at = ?,
			attempt = ?, updated_at = ? WHERE id = ? AND status = ?`,
		string(StatusClaimed), in.WorkerID, resumeKind, formatTime(now.Add(lease)),
		attempt, formatTime(now), cand.ID, string(cand.Status))
	if err != nil {
		return nil, fmt.Errorf("claiming run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil // raced: another worker claimed it between scan and write
	}

	row := tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, cand.ID)
	claimed, err := scanRun(row)
	if err != nil {
		return nil, err
	}
	return claimed, tx.Commit()
}

// Heartbeat extends the lease of a run the worker holds; first heartbeat
// after a claim also flips claimed -> running.
func (s *Storage) Heartbeat(ctx context.Context, id, workerID string, lease time.Duration, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET lease_expires_at = ?,
			status = CASE WHEN status = ? THEN ? ELSE status END,
			updated_at = ?
		WHERE id = ? AND worker_id = ? AND status IN (?, ?, ?)`,
		formatTime(now.Add(lease)), string(StatusClaimed), string(StatusRunning), formatTime(now),
		id, workerID, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseHeld
	}
	return nil
}

// Report writes the worker's result. Lease must still belong to the worker.
func (s *Storage) Report(ctx context.Context, id, workerID string, in ReportInput, now time.Time) (*Run, error) {
	checks, err := json.Marshal(in.Checks)
	if err != nil {
		checks = []byte("[]")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, result_summary = ?, head_sha = ?, diffstat = ?, checks = ?,
			log_ref = ?, fail_reason = ?, branch = CASE WHEN ? != '' THEN ? ELSE branch END,
			base_sha = CASE WHEN ? != '' THEN ? ELSE base_sha END,
			lease_expires_at = '', updated_at = ?
		WHERE id = ? AND worker_id = ? AND status IN (?, ?, ?)`,
		string(in.Status), in.Summary, in.HeadSHA, in.Diffstat, string(checks),
		in.LogRef, in.Reason, in.Branch, in.Branch, in.BaseSHA, in.BaseSHA,
		formatTime(now), id, workerID, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return nil, fmt.Errorf("reporting run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrLeaseHeld
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// Pause keeps affinity: the run stays with its worker, no lease pressure.
func (s *Storage) Pause(ctx context.Context, id, workerID, reason string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, fail_reason = ?, lease_expires_at = '', updated_at = ?
		WHERE id = ? AND worker_id = ? AND status IN (?, ?, ?)`,
		string(StatusPaused), reason, formatTime(now),
		id, workerID, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return fmt.Errorf("pausing run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseHeld
	}
	return nil
}

// Resume flips a paused run back to claimed for its owner.
func (s *Storage) Resume(ctx context.Context, id, workerID string, lease time.Duration, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND worker_id = ? AND status = ?`,
		string(StatusClaimed), formatTime(now.Add(lease)), formatTime(now),
		id, workerID, string(StatusPaused))
	if err != nil {
		return fmt.Errorf("resuming run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseHeld
	}
	return nil
}

// Cancel retires runs of an item that are still queued/paused (item rejected
// or done). Returns the ids cancelled.
func (s *Storage) CancelItem(ctx context.Context, itemID, reason string, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE item_id = ? AND status IN (?, ?, ?, ?)`,
		itemID, string(StatusQueued), string(StatusPaused), string(StatusClaimed), string(StatusRunning))
	if err != nil {
		return nil, fmt.Errorf("cancelling runs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `
			UPDATE runs SET status = ?, fail_reason = ?, updated_at = ? WHERE id = ?`,
			string(StatusCancelled), reason, formatTime(now), id); err != nil {
			return nil, fmt.Errorf("cancelling run %s: %w", id, err)
		}
	}
	return ids, nil
}
