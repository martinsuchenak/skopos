package runs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
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
	resume_kind, lease_expires_at, resume_after, attempt, branch, base_sha, head_sha,
	result_summary, diffstat, checks, log_ref, fail_reason, lease_token_hash, claimed_key_id, created_at, updated_at`

func scanRun(row interface{ Scan(dest ...any) error }) (*Run, error) {
	var r Run
	var lease, resumeAfter, createdAt, updatedAt string
	var checks string
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.ItemID, &r.PlanRevID, &r.Stage, &r.Runner, &r.WorkerID, &r.Status,
		&r.ResumeKind, &lease, &resumeAfter, &r.Attempt, &r.Branch, &r.BaseSHA, &r.HeadSHA,
		&r.ResultSummary, &r.Diffstat, &checks, &r.LogRef, &r.FailReason, &r.LeaseTokenHash, &r.ClaimedKeyID, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning run: %w", err)
	}
	r.LeaseExpires = parseTime(lease)
	r.ResumeAfter = parseTime(resumeAfter)
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
	// (as interrupted, affinity kept). Runs expired longer than the
	// takeover grace become branch-resumable: the owner's session is
	// presumed gone, so any worker may take over from the pushed branch.
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?,
			resume_kind = CASE WHEN lease_expires_at < ? THEN ? ELSE ? END,
			updated_at = ?
		WHERE status IN (?, ?) AND lease_expires_at != '' AND lease_expires_at < ?`,
		string(StatusInterrupted), formatTime(now.Add(-takeoverGrace)), ResumeBranch, ResumeSession, formatTime(now),
		string(StatusClaimed), string(StatusRunning), formatTime(now)); err != nil {
		return nil, fmt.Errorf("expiring leases: %w", err)
	}
	// N4: runs already interrupted keep aging — the grace is re-evaluated on
	// every sweep, so a worker that dies for good has its runs released to
	// branch takeovers after takeoverGrace regardless of polling cadence.
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET resume_kind = ?, updated_at = ?
		WHERE status = ? AND resume_kind = ? AND lease_expires_at != '' AND lease_expires_at < ?`,
		ResumeBranch, formatTime(now),
		string(StatusInterrupted), ResumeSession, formatTime(now.Add(-takeoverGrace))); err != nil {
		return nil, fmt.Errorf("grace flip: %w", err)
	}
	// Round-4 low: an overdue QUOTA pause whose owner never returns also
	// ages into a branch takeover (amendment pauses carry no resume_after
	// and stay approval-gated).
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, resume_kind = ?, updated_at = ?
		WHERE status = ? AND resume_after != '' AND resume_after < ?`,
		string(StatusInterrupted), ResumeBranch, formatTime(now),
		string(StatusPaused), formatTime(now.Add(-takeoverGrace))); err != nil {
		return nil, fmt.Errorf("quota grace flip: %w", err)
	}

	// Re-read candidates inside the transaction. Paused runs are NOT
	// candidates EXCEPT quota pauses past their resume_after (A2): an
	// amendment pause has no resume_after and only its approval re-arms it
	// (H4).
	var cand *Run
	{
		row := tx.QueryRowContext(ctx, `
			SELECT `+runColumns+` FROM runs
			WHERE worker_id = ? AND (status = ? OR (status = ? AND resume_after != '' AND resume_after <= ?))
			ORDER BY created_at ASC LIMIT 1`,
			in.WorkerID, string(StatusInterrupted), string(StatusPaused), formatTime(now))
		r, err := scanRun(row)
		if err == nil {
			cand = r
		} else if !errors.Is(err, ErrNotFound) {
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
		// Commit even with no candidate: the lease-expiry sweep above must
		// persist (it was silently rolled back here, so runs only ever
		// expired inside a successful claim — review found it via the
		// cancel-interrupted test).
		return nil, tx.Commit()
	}

	attempt := cand.Attempt
	resumeKind := cand.ResumeKind
	if cand.Status == StatusInterrupted {
		attempt++ // a takeover/re-claim is a new attempt
	} else {
		resumeKind = ResumeSession // first claim: only the owner may resume
	}
	// M5: a fresh lease token per claim (including re-claims/takeovers) —
	// the fencing token every later call must present.
	token := newLeaseToken()
	tokenHash := hashLeaseToken(token)
	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, worker_id = ?, resume_kind = ?, lease_expires_at = ?, resume_after = '',
			attempt = ?, lease_token_hash = ?, claimed_key_id = ?, updated_at = ?
		WHERE id = ? AND status = ?`,
		string(StatusClaimed), in.WorkerID, resumeKind, formatTime(now.Add(lease)),
		attempt, tokenHash, in.KeyID, formatTime(now), cand.ID, string(cand.Status))
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	claimed.LeaseToken = token // plain token ONLY in the claim response
	return claimed, nil
}

// newLeaseToken: 128 random bits, base64url.
func newLeaseToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashLeaseToken: the stored form (tokens are never persisted in clear).
func hashLeaseToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Heartbeat extends the lease of a run the worker holds; first heartbeat
// after a claim also flips claimed -> running.
func (s *Storage) Heartbeat(ctx context.Context, id, workerID, keyID, tokenHash string, lease time.Duration, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET lease_expires_at = ?,
			status = CASE WHEN status = ? THEN ? ELSE status END,
			updated_at = ?
		WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND status IN (?, ?, ?)`,
		formatTime(now.Add(lease)), string(StatusClaimed), string(StatusRunning), formatTime(now),
		id, workerID, keyID, tokenHash, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseHeld
	}
	return nil
}

// Report writes the worker's result. Lease must still belong to the worker.
func (s *Storage) Report(ctx context.Context, id, workerID, keyID, tokenHash string, in ReportInput, now time.Time) (*Run, error) {
	checks, err := json.Marshal(in.Checks)
	if err != nil {
		checks = []byte("[]")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, result_summary = ?, head_sha = ?, diffstat = ?, checks = ?,
			log_ref = ?, fail_reason = ?, branch = CASE WHEN ? != '' THEN ? ELSE branch END,
			base_sha = CASE WHEN ? != '' THEN ? ELSE base_sha END,
			lease_expires_at = '', resume_after = '', updated_at = ?
		WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND status IN (?, ?, ?)`,
		string(in.Status), in.Summary, in.HeadSHA, in.Diffstat, string(checks),
		in.LogRef, in.Reason, in.Branch, in.Branch, in.BaseSHA, in.BaseSHA,
		formatTime(now), id, workerID, keyID, tokenHash, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
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
// A non-empty resumeAfter (quota pauses) lets Claim hand the run back to
// its owner once that time passes — restart-safe (A2).
func (s *Storage) Pause(ctx context.Context, id, workerID, reason string, resumeAfter time.Time, keyID, tokenHash string, now time.Time) error {
	// Round 7: the quota reset lives in resume_after; a paused run carries
	// no lease deadline (sweeps only age claimed/running leases).
	resume := ""
	if !resumeAfter.IsZero() {
		resume = formatTime(resumeAfter)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, fail_reason = ?, lease_expires_at = '', resume_after = ?, updated_at = ?
		WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND status IN (?, ?, ?)`,
		string(StatusPaused), reason, resume, formatTime(now),
		id, workerID, keyID, tokenHash, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return fmt.Errorf("pausing run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLeaseHeld
	}
	return nil
}

// Resume returns a paused run to its owner as an owner-claimable
// interrupted run (review N7: setting it straight to claimed made it
// invisible to Claim until the lease expired — an approved amendment took
// ~10 minutes to re-arm).
func (s *Storage) Resume(ctx context.Context, id, workerID string, lease time.Duration, now time.Time) error {
	// lease_expires_at = now: resumed runs keep aging into the branch
	// grace like any other interrupted run (A3 — an empty lease exempted
	// them forever).
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, resume_kind = ?, lease_expires_at = ?, resume_after = '', updated_at = ?
		WHERE id = ? AND worker_id = ? AND status = ?`,
		string(StatusInterrupted), ResumeSession, formatTime(now), formatTime(now),
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
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM runs WHERE item_id = ? AND status IN (?, ?, ?, ?, ?)`,
		itemID, string(StatusQueued), string(StatusPaused), string(StatusClaimed), string(StatusRunning), string(StatusInterrupted))
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

// CancelOne retires a single run by id (targeted compensation, review N1).
func (s *Storage) CancelOne(ctx context.Context, id, workerID, reason, keyID, tokenHash string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, fail_reason = ?, updated_at = ?
		WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND status IN (?, ?, ?, ?)`,
		string(StatusCancelled), reason, formatTime(now),
		id, workerID, keyID, tokenHash, string(StatusQueued), string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return fmt.Errorf("cancelling run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Note appends a chain-failure note to a run's fail_reason (best-effort).
func (s *Storage) Note(ctx context.Context, id, note string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET fail_reason = CASE WHEN fail_reason = '' THEN ? ELSE fail_reason END WHERE id = ?`, note, id)
	return err
}
