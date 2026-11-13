package runs

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/martinsuchenak/skopos/internal/dbtime"
	"github.com/martinsuchenak/skopos/internal/findings"
	"github.com/martinsuchenak/skopos/internal/integrations"
	"github.com/martinsuchenak/skopos/internal/policy"
)

// Storage persists runs. The queue's correctness lives in Claim: the
// pick-and-mark happens in one transaction so two workers can never hold the
// same run, whatever their timing.
type Storage struct {
	db *sql.DB
	// events records run events in the claim and report transactions
	// (internal/integrations; nil = none).
	events *integrations.Emitter
	// findings records a review's findings and a run's answers to earlier
	// ones in the report transaction (internal/findings; nil = none).
	findings *findings.Service
}

func NewStorage(db *sql.DB) *Storage { return &Storage{db: db} }

func formatTime(t time.Time) string { return dbtime.Format(t) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

const runColumns = `id, workspace_id, item_id, plan_rev_id, stage, runner, worker_id, status,
	resume_kind, lease_expires_at, resume_after, attempt, branch, base_sha, head_sha,
	result_summary, diffstat, checks, log_ref, fail_reason, lease_token_hash, claimed_key_id, created_at, updated_at, executor,
	group_id, focus, cost_usd, findings, pushed, pr_url, setup, fix_rounds, phase, retries, retry_after, avoid_worker,
	prefer_worker, prefer_until, runner_picked, policy_rev, facts, stranded_reason, grants, config_rev, still_open`

func scanRun(row interface{ Scan(dest ...any) error }) (*Run, error) {
	var r Run
	var lease, resumeAfter, retryAfter, preferUntil, createdAt, updatedAt string
	var checks, setup string
	var groupID, focus, findings sql.NullString
	var cost sql.NullFloat64
	var pushed sql.NullBool
	var picked int
	var facts string
	if err := row.Scan(&r.ID, &r.WorkspaceID, &r.ItemID, &r.PlanRevID, &r.Stage, &r.Runner, &r.WorkerID, &r.Status,
		&r.ResumeKind, &lease, &resumeAfter, &r.Attempt, &r.Branch, &r.BaseSHA, &r.HeadSHA,
		&r.ResultSummary, &r.Diffstat, &checks, &r.LogRef, &r.FailReason, &r.LeaseTokenHash, &r.ClaimedKeyID, &createdAt, &updatedAt, &r.Executor,
		&groupID, &focus, &cost, &findings, &pushed, &r.PRURL, &setup, &r.FixRounds, &r.Phase, &r.Retries, &retryAfter, &r.AvoidWorker,
		&r.PreferWorker, &preferUntil, &picked, &r.PolicyRev, &facts, &r.Stranded, &r.Grants, &r.ConfigRev, &r.StillOpen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning run: %w", err)
	}
	r.RunnerPicked = picked != 0
	if facts != "" {
		r.Facts = json.RawMessage(facts)
	}
	r.LeaseExpires = parseTime(lease)
	r.ResumeAfter = parseTime(resumeAfter)
	r.RetryAfter = parseTime(retryAfter)
	r.PreferUntil = parseTime(preferUntil)
	r.CreatedAt = parseTime(createdAt)
	r.UpdatedAt = parseTime(updatedAt)
	if checks != "" && checks != "[]" {
		_ = json.Unmarshal([]byte(checks), &r.Checks)
	}
	if setup != "" {
		_ = json.Unmarshal([]byte(setup), &r.Setup)
	}
	r.GroupID, r.Focus = groupID.String, focus.String
	if cost.Valid {
		c := cost.Float64
		r.CostUSD = &c
	}
	if findings.Valid && findings.String != "" {
		_ = json.Unmarshal([]byte(findings.String), &r.Findings)
	}
	if pushed.Valid {
		b := pushed.Bool
		r.Pushed = &b
	}
	return &r, nil
}

// nullIfEmpty maps "" to SQL NULL: the new columns use NULL for "not set".
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolArg(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// findingsJSON stores findings as a JSON array, or NULL when there are none.
func findingsJSON(fs []Finding) any {
	if len(fs) == 0 {
		return nil
	}
	b, err := json.Marshal(fs)
	if err != nil {
		return nil
	}
	return string(b)
}

// Create inserts a queued run.
func (s *Storage) Create(ctx context.Context, r Run) error {
	return insertRun(ctx, s.db, r)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertRun(ctx context.Context, db execer, r Run) error {
	checks, err := json.Marshal(r.Checks)
	if err != nil {
		checks = []byte("[]")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO runs (id, workspace_id, item_id, plan_rev_id, stage, runner, status, attempt,
			branch, base_sha, head_sha, checks, created_at, updated_at, group_id, focus, prefer_worker, prefer_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkspaceID, r.ItemID, r.PlanRevID, r.Stage, r.Runner, StatusQueued, 1,
		r.Branch, r.BaseSHA, r.HeadSHA, string(checks), formatTime(r.CreatedAt), formatTime(r.UpdatedAt),
		nullIfEmpty(r.GroupID), nullIfEmpty(r.Focus), r.PreferWorker, formatPreferUntil(r.PreferUntil)); err != nil {
		return fmt.Errorf("inserting run: %w", err)
	}
	return nil
}

// Get returns one run by id.
func jsonList(xs []string) string {
	b, err := json.Marshal(xs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// WorkerSeen is a registry row (what the worker reports on every poll) and
// its presence: State is busy (holds a claimed/running run), idle (polled
// recently) or offline (said goodbye, or silent too long).
type WorkerSeen struct {
	ID         string   `json:"id"`
	Runners    []string `json:"runners"`
	Workspaces []string `json:"workspaces"`
	LastSeen   string   `json:"last_seen"`
	Version    string   `json:"version,omitempty"`
	PollMS     int64    `json:"poll_ms,omitempty"`
	OfflineAt  string   `json:"offline_at,omitempty"`
	State      string   `json:"state"`
	// Unhealthy: workspaces whose preflight fails there (→ reason).
	Unhealthy map[string]string `json:"unhealthy,omitempty"`
	// Indexable: workspaces whose repo it can fetch and index.
	Indexable []string `json:"indexable,omitempty"`
	// Paused: an operator's pause in force (nil when none, or it ended).
	Paused *WorkerPause `json:"paused,omitempty"`
	// RunnerFacts: what the worker reports about each runner it has — the
	// CLI, the model, and where its requests go (policy.go). Empty for a
	// worker from before runner facts.
	RunnerFacts []policy.Facts `json:"runner_facts,omitempty"`
	// Executors: what contains a run on the worker, per workspace, then by
	// stage and runner. Empty for a worker that reports none.
	Executors map[string][]policy.ExecutorFact `json:"executors,omitempty"`
	// ServerCommands: the worker takes a managed workspace's commands from
	// the server (nil = it does not say: a worker from before workspace
	// config). FleetRev: the runner definitions it holds.
	ServerCommands *bool  `json:"server_commands,omitempty"`
	FleetRev       string `json:"fleet_rev,omitempty"`
}

// unhealthyJSON stores a health map (nil when all healthy).
func unhealthyJSON(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return string(b)
}

// WorkerUnhealthy is the stored health of a worker (nil when none).
func (s *Storage) WorkerUnhealthy(ctx context.Context, workerID string) map[string]string {
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT unhealthy FROM workers WHERE id = ?`, workerID).Scan(&raw); err != nil || !raw.Valid {
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(raw.String), &m)
	return m
}

// WaitingRuns lists the runs waiting for a worker — queued, and
// interrupted ones any worker may take over from the branch — oldest first.
func (s *Storage) WaitingRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE status = ? OR (status = ? AND resume_kind = ?) ORDER BY created_at ASC`,
		string(StatusQueued), string(StatusInterrupted), ResumeBranch)
	if err != nil {
		return nil, fmt.Errorf("listing waiting runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// WorkerOffers lists what the workers seen since `since` offer: their
// workspaces, their runners and what they report about them. A worker
// gone longer than that is no reason to call a run takeable.
func (s *Storage) WorkerOffers(ctx context.Context, since time.Time) ([]workerOffer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT runners, workspaces, COALESCE(runner_info, ''), COALESCE(executor_info, '') FROM workers WHERE last_seen >= ?`, formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("listing workers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []workerOffer
	for rows.Next() {
		var rawRunners, rawWorkspaces, rawFacts, rawExecutors string
		if err := rows.Scan(&rawRunners, &rawWorkspaces, &rawFacts, &rawExecutors); err != nil {
			return nil, err
		}
		var runners, workspaces []string
		_ = json.Unmarshal([]byte(rawRunners), &runners)
		_ = json.Unmarshal([]byte(rawWorkspaces), &workspaces)
		// What a worker has configured, not only what has a free account
		// this minute: a quota wait is not a policy refusal.
		facts := parseFacts(rawFacts)
		for _, f := range facts {
			runners = append(runners, f.Name)
		}
		o := workerOffer{workspaces: map[string]bool{}, facts: newClaimFacts(runners, facts, parseExecutors(rawExecutors))}
		for _, w := range workspaces {
			o.workspaces[w] = true
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ClearStaleStranded removes the stranded mark from runs that no longer
// wait for a worker (claimed, cancelled, finished).
func (s *Storage) ClearStaleStranded(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET stranded_ms = NULL, stranded_reason = '' WHERE stranded_ms IS NOT NULL AND status NOT IN (?, ?)`,
		string(StatusQueued), string(StatusInterrupted))
	return err
}

// SetStranded records (reason != "") or clears (reason == "") a waiting
// run's stranded mark; changed reports whether the reason changed.
func (s *Storage) SetStranded(ctx context.Context, id, reason string, now time.Time) (changed bool, err error) {
	var res sql.Result
	if reason == "" {
		res, err = s.db.ExecContext(ctx, `UPDATE runs SET stranded_ms = NULL, stranded_reason = '' WHERE id = ? AND stranded_ms IS NOT NULL`, id)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE runs SET stranded_ms = ?, stranded_reason = ? WHERE id = ? AND status IN (?, ?) AND stranded_reason != ?`,
			now.UnixMilli(), reason, id, string(StatusQueued), string(StatusInterrupted), reason)
	}
	if err != nil {
		return false, fmt.Errorf("marking a stranded run: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// workerFactsRow is what the registry holds of a worker's own word: its
// runner facts and its executor facts, as stored ("" = none).
// commands: whether it takes commands from the server ("" = it does not
// say, "0", "1"); fleetRev: the runner definitions it holds.
type workerFactsRow struct{ runners, executors, commands, fleetRev string }

// WorkerFacts returns the facts the registry holds for a worker and
// whether the worker has a row at all.
func (s *Storage) WorkerFacts(ctx context.Context, workerID string) (workerFactsRow, bool) {
	var runners, executors, commands sql.NullString
	var fleet string
	if err := s.db.QueryRowContext(ctx, `SELECT runner_info, executor_info, CAST(server_commands AS TEXT), fleet_rev FROM workers WHERE id = ?`, workerID).Scan(&runners, &executors, &commands, &fleet); err != nil {
		return workerFactsRow{}, false
	}
	return workerFactsRow{runners.String, executors.String, commands.String, fleet}, true
}

// presenceRefresh: an unchanged worker row is rewritten at most this often
// by idle polls and heartbeats (presence without a write per poll).
const presenceRefresh = 30 * time.Second

// recordWorker is the registry write of every claim poll: what the worker
// offers and serves, its version and poll interval; it also clears a
// goodbye. The row belongs to the key that first wrote it (” = root; NULL
// = a row from before keys were recorded, adopted by its next writer):
// another key reusing the worker id cannot overwrite it (review H4); its
// claims are unaffected (runs are fenced by claimed_key_id). force (a
// successful claim) always writes; an idle poll writes only when something
// changed or the row is older than presenceRefresh.
func recordWorker(ctx context.Context, tx *sql.Tx, in ClaimInput, now time.Time, force bool) error {
	var version, poll any
	if in.Version != "" {
		version = in.Version
	}
	if in.PollMS > 0 {
		poll = in.PollMS
	}
	forced := 0
	if force {
		forced = 1
	}
	var commands any
	if in.ServerCommands != nil {
		commands = boolInt(*in.ServerCommands)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO workers (id, runners, workspaces, last_seen, updated_at, key_id, version, poll_ms, offline_at, unhealthy, indexable, runner_info, executor_info, server_commands, fleet_rev)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			runners = excluded.runners, workspaces = excluded.workspaces,
			last_seen = excluded.last_seen, updated_at = excluded.updated_at, key_id = excluded.key_id,
			version = excluded.version, poll_ms = excluded.poll_ms, offline_at = NULL, unhealthy = excluded.unhealthy,
			indexable = excluded.indexable, runner_info = excluded.runner_info, executor_info = excluded.executor_info,
			server_commands = excluded.server_commands, fleet_rev = excluded.fleet_rev
		WHERE (workers.key_id IS NULL OR workers.key_id = excluded.key_id)
			AND (? = 1 OR workers.runners != excluded.runners OR workers.workspaces != excluded.workspaces
				OR COALESCE(workers.version, '') != COALESCE(excluded.version, '')
				OR COALESCE(workers.poll_ms, 0) != COALESCE(excluded.poll_ms, 0)
				OR COALESCE(workers.unhealthy, '') != COALESCE(excluded.unhealthy, '')
				OR workers.indexable != excluded.indexable
				OR COALESCE(workers.runner_info, '') != COALESCE(excluded.runner_info, '')
				OR COALESCE(workers.executor_info, '') != COALESCE(excluded.executor_info, '')
				OR COALESCE(workers.server_commands, -1) != COALESCE(excluded.server_commands, -1)
				OR workers.fleet_rev != excluded.fleet_rev
				OR workers.offline_at IS NOT NULL OR workers.last_seen < ?)`,
		in.WorkerID, jsonList(in.Runners), jsonList(in.Workspaces), formatTime(now), formatTime(now), in.KeyID,
		version, poll, unhealthyJSON(in.Unhealthy), jsonList(in.Indexable), nullIfEmpty(factsJSON(in.RunnerFacts)), nullIfEmpty(executorsJSON(in.Executors)), commands, in.FleetRev, forced, formatTime(now.Add(-presenceRefresh)))
	if err != nil {
		return fmt.Errorf("recording worker: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 && force {
		slog.Warn("worker id is registered to another API key; registry row left unchanged", "worker", in.WorkerID)
	}
	return nil
}

// TouchWorker refreshes a busy worker's presence on a run heartbeat.
func (s *Storage) TouchWorker(ctx context.Context, workerID, keyID string, now time.Time) {
	_, _ = s.db.ExecContext(ctx, `
		UPDATE workers SET last_seen = ?, offline_at = NULL
		WHERE id = ? AND (key_id IS NULL OR key_id = ?) AND (last_seen < ? OR offline_at IS NOT NULL)`,
		formatTime(now), workerID, keyID, formatTime(now.Add(-presenceRefresh)))
}

// WorkerOffline records a worker's clean-shutdown goodbye; false when the
// row does not exist or belongs to another key.
func (s *Storage) WorkerOffline(ctx context.Context, workerID, keyID string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE workers SET offline_at = ? WHERE id = ? AND (key_id IS NULL OR key_id = ?)`,
		formatTime(now), workerID, keyID)
	if err != nil {
		return false, fmt.Errorf("marking worker offline: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BusyWorkers lists worker ids holding a claimed or running run.
func (s *Storage) BusyWorkers(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT worker_id FROM runs WHERE status IN (?, ?) AND worker_id != ''`, string(StatusClaimed), string(StatusRunning))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ListWorkers returns every worker that has ever claimed, newest first.
func (st *Storage) ListWorkers(ctx context.Context) ([]WorkerSeen, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT id, runners, workspaces, last_seen, COALESCE(version, ''), COALESCE(poll_ms, 0), COALESCE(offline_at, ''), COALESCE(unhealthy, ''), indexable,
		COALESCE(paused_at, ''), COALESCE(paused_until, ''), COALESCE(paused_by, ''), COALESCE(pause_reason, ''), COALESCE(runner_info, ''), COALESCE(executor_info, ''),
		server_commands, fleet_rev FROM workers ORDER BY last_seen DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing workers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WorkerSeen
	for rows.Next() {
		var w WorkerSeen
		var runners, workspaces, unhealthy, indexable, pausedAt, pausedUntil, pausedBy, pauseReason, facts, executors string
		var commands sql.NullBool
		if err := rows.Scan(&w.ID, &runners, &workspaces, &w.LastSeen, &w.Version, &w.PollMS, &w.OfflineAt, &unhealthy, &indexable,
			&pausedAt, &pausedUntil, &pausedBy, &pauseReason, &facts, &executors, &commands, &w.FleetRev); err != nil {
			return nil, err
		}
		if commands.Valid {
			w.ServerCommands = &commands.Bool
		}
		w.RunnerFacts = parseFacts(facts)
		w.Executors = parseExecutors(executors)
		if pausedAt != "" {
			w.Paused = &WorkerPause{At: parseTime(pausedAt), By: pausedBy, Reason: pauseReason}
			if pausedUntil != "" {
				w.Paused.Until = parseTime(pausedUntil)
			}
		}
		if unhealthy != "" {
			_ = json.Unmarshal([]byte(unhealthy), &w.Unhealthy)
		}
		_ = json.Unmarshal([]byte(indexable), &w.Indexable)
		_ = json.Unmarshal([]byte(runners), &w.Runners)
		_ = json.Unmarshal([]byte(workspaces), &w.Workspaces)
		if w.Runners == nil {
			w.Runners = []string{}
		}
		if w.Workspaces == nil {
			w.Workspaces = []string{}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetConfigRev records the effective config a claim named for the run's
// attempt (” clears an earlier attempt's).
func (s *Storage) SetConfigRev(ctx context.Context, id string, attempt int, rev string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET config_rev = ? WHERE id = ? AND attempt = ? AND status = ?`, rev, id, attempt, string(StatusClaimed))
	if err != nil {
		return fmt.Errorf("recording the run's config revision: %w", err)
	}
	return nil
}

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
	defer func() { _ = rows.Close() }()
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
func (s *Storage) Claim(ctx context.Context, in ClaimInput, prefs func(workspace string) *WorkspacePrefs, lease time.Duration, now time.Time) (*Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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

	// A worker claims only when idle (its loop runs one run at a time), so a
	// run still claimed or running under this worker and key is orphaned: a
	// claim that committed after the worker gave up waiting for the response,
	// or a run cut off by a worker restart. Release it now so the affinity
	// scan below hands it straight back, instead of after its lease expires
	// with the item left behind in the wrong phase.
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, updated_at = ?
		WHERE worker_id = ? AND claimed_key_id = ? AND status IN (?, ?)`,
		string(StatusInterrupted), formatTime(now),
		in.WorkerID, in.KeyID, string(StatusClaimed), string(StatusRunning)); err != nil {
		return nil, fmt.Errorf("releasing orphaned runs: %w", err)
	}
	// Tokens of runs the sweeps above (or a report, pause or cancel since
	// the last poll) took out of execution get their end recorded.
	if err := settleTokens(ctx, tx, now); err != nil {
		return nil, err
	}

	// A paused worker takes nothing: its own interrupted runs wait for it
	// too (their session lives there).
	var paused int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workers w WHERE w.id = ? AND `+pausedSQL+`)`,
		in.WorkerID, formatTime(now)).Scan(&paused); err != nil {
		return nil, fmt.Errorf("reading the worker's pause: %w", err)
	}
	if paused == 1 {
		if err := recordWorker(ctx, tx, in, now, false); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}

	// Re-read candidates inside the transaction. Paused runs are NOT
	// candidates EXCEPT quota pauses past their resume_after (A2): an
	// amendment pause has no resume_after and only its approval re-arms it
	// (H4).
	offer := newClaimFacts(in.Runners, in.RunnerFacts, in.Executors)
	var cand *Run
	var chosen decision
	{
		// The worker's own runs first — checked against the policy like
		// any other. One it may no longer run (the policy changed while it
		// was paused, or the worker now reports another endpoint for the
		// runner) is released for another worker to take over from the
		// branch, and the scan goes on to the next.
		rows, err := tx.QueryContext(ctx, `
			SELECT `+runColumns+` FROM runs
			WHERE worker_id = ? AND (status = ? OR (status = ? AND resume_after != '' AND resume_after <= ?))
				AND NOT EXISTS (SELECT 1 FROM json_each(?) WHERE json_each.key = runs.workspace_id)
			ORDER BY created_at ASC`,
			in.WorkerID, string(StatusInterrupted), string(StatusPaused), formatTime(now), unhealthyObject(in.Unhealthy))
		if err != nil {
			return nil, fmt.Errorf("own-runs scan: %w", err)
		}
		var own []*Run
		for rows.Next() {
			r, err := scanRun(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			own = append(own, r)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, r := range own {
			// A run is handed back to "its" worker only when this caller
			// may have it: the worker id is the caller's word, so a second
			// key polling under the same id (a copied worker config) must
			// not receive a run — and its credentials — of a workspace
			// outside its own scope, nor may a worker whose release cannot
			// take what the workspace needs. Such a run is left alone: it
			// ages into a takeover like any run whose owner is gone.
			if in.mayTake != nil && !in.mayTake(r.WorkspaceID) {
				continue
			}
			d := decideResume(r, prefs(r.WorkspaceID), offer)
			if d.ok {
				cand, chosen = r, d
				break
			}
			if d.hold {
				// The policy could not be read just now: neither hand the
				// run back nor give it away (its session lives here).
				continue
			}
			if err := releaseForTakeover(ctx, tx, r.ID, now); err != nil {
				return nil, err
			}
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
				_ = rows.Close()
				return nil, err
			}
			queued = append(queued, r)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		wsSet := map[string]bool{}
		for _, w := range in.Workspaces {
			wsSet[w] = in.Unhealthy[w] == ""
		}
		for _, r := range queued {
			if !wsSet[r.WorkspaceID] || retryWaiting(r, in.WorkerID, now) {
				continue
			}
			if r.PreferWorker != "" && r.PreferWorker != in.WorkerID && now.Before(r.PreferUntil) {
				if able, err := workerCanTake(ctx, tx, r.PreferWorker, r, prefs(r.WorkspaceID), now); err != nil {
					return nil, err
				} else if able {
					continue // waiting for the worker whose clone has the item's work
				}
			}
			if d := decide(r, prefs(r.WorkspaceID), offer); d.ok {
				cand, chosen = r, d
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
				_ = rows.Close()
				return nil, err
			}
			takeover = append(takeover, r)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		wsSet := map[string]bool{}
		for _, w := range in.Workspaces {
			wsSet[w] = in.Unhealthy[w] == ""
		}
		for _, r := range takeover {
			if !wsSet[r.WorkspaceID] {
				continue
			}
			if d := decide(r, prefs(r.WorkspaceID), offer); d.ok {
				cand, chosen = r, d
				break
			}
		}
	}
	if cand == nil {
		// Commit even with no candidate: the lease-expiry sweep above must
		// persist (it was silently rolled back here, so runs only ever
		// expired inside a successful claim — review found it via the
		// cancel-interrupted test). An idle poll still records the worker
		// (presence: visible from its first poll, not its first run).
		if err := recordWorker(ctx, tx, in, now, false); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}

	attempt := cand.Attempt
	resumeKind := cand.ResumeKind
	if cand.Status == StatusInterrupted {
		attempt++ // a takeover/re-claim is a new attempt
	} else {
		resumeKind = ResumeSession // first claim: only the owner may resume
	}
	// What the claim decided (policy.go): the one runner the worker runs,
	// whether the server chose it, and the policy and facts it was checked
	// with. A worker without runner facts under no policy keeps choosing
	// for itself (runner stays as queued).
	runner, picked := cand.Runner, cand.RunnerPicked
	switch {
	case chosen.runner != "":
		runner, picked = chosen.runner, chosen.picked || (cand.RunnerPicked && chosen.runner == cand.Runner)
	case cand.RunnerPicked:
		// An earlier claim's pick, now taken by a worker that picks for
		// itself: the old pick names a runner it may not even have.
		runner, picked = "", false
	}
	policyRev, factsRec := chosen.record(prefs(cand.WorkspaceID))
	// M5: a fresh lease token per claim (including re-claims/takeovers) —
	// the fencing token every later call must present.
	token := newLeaseToken()
	tokenHash := hashLeaseToken(token)
	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, worker_id = ?, resume_kind = ?, lease_expires_at = ?, resume_after = '',
			attempt = ?, lease_token_hash = ?, claimed_key_id = ?, phase = '', updated_at = ?,
			runner = ?, runner_picked = ?, policy_rev = ?, facts = ?, stranded_ms = NULL, stranded_reason = ''
		WHERE id = ? AND status = ?`,
		string(StatusClaimed), in.WorkerID, resumeKind, formatTime(now.Add(lease)),
		attempt, tokenHash, in.KeyID, formatTime(now),
		runner, boolInt(picked), policyRev, factsRec, cand.ID, string(cand.Status))
	if err != nil {
		return nil, fmt.Errorf("claiming run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil // raced: another worker claimed it between scan and write
	}

	// The attempt's agent token (tokens.go): minted with the claim, so the
	// run's agent never needs a key shared with other runs.
	agentToken, err := mintAgentToken(ctx, tx, cand.ID, attempt, now)
	if err != nil {
		return nil, err
	}
	if err := recordWorker(ctx, tx, in, now, true); err != nil {
		return nil, err
	}
	row := tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, cand.ID)
	claimed, err := scanRun(row)
	if err != nil {
		return nil, err
	}
	if err := s.events.Emit(ctx, tx, startedEvent(claimed)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	claimed.LeaseToken = token // plain token ONLY in the claim response
	claimed.AgentToken = agentToken
	return claimed, nil
}

// releaseForTakeover gives up a run its owner may no longer resume under
// the policy: it becomes a branch takeover any allowed worker can claim. A
// runner the server had picked is forgotten, so the next worker's own is
// picked. A no-op for a run already released.
// ReleaseClaim gives a run this claim just took back to the queue of
// takeovers, before anything was handed to the worker: what the run needs
// turned out to be something the claiming worker cannot do.
func (s *Storage) ReleaseClaim(ctx context.Context, id string, attempt int, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, resume_kind = ?, resume_after = '', lease_token_hash = '',
			runner = CASE WHEN runner_picked = 1 THEN '' ELSE runner END, runner_picked = 0, updated_at = ?
		WHERE id = ? AND attempt = ? AND status = ?`,
		string(StatusInterrupted), ResumeBranch, formatTime(now), id, attempt, string(StatusClaimed))
	return err
}

func releaseForTakeover(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, resume_kind = ?, resume_after = '',
			runner = CASE WHEN runner_picked = 1 THEN '' ELSE runner END, runner_picked = 0, updated_at = ?
		WHERE id = ? AND NOT (status = ? AND resume_kind = ? AND runner_picked = 0)`,
		string(StatusInterrupted), ResumeBranch, formatTime(now), id, string(StatusInterrupted), ResumeBranch)
	if err != nil {
		return fmt.Errorf("releasing a run its owner may no longer take: %w", err)
	}
	return nil
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
// after a claim also flips claimed -> running. A non-empty phase replaces
// the run's phase.
func (s *Storage) Heartbeat(ctx context.Context, id, workerID, keyID, tokenHash, phase string, lease time.Duration, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET lease_expires_at = ?,
			status = CASE WHEN status = ? THEN ? ELSE status END,
			phase = CASE WHEN ? != '' THEN ? ELSE phase END,
			updated_at = ?
		WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND status IN (?, ?, ?)`,
		formatTime(now.Add(lease)), string(StatusClaimed), string(StatusRunning), phase, phase, formatTime(now),
		id, workerID, keyID, tokenHash, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Still this worker's claim, but cancelled since: it should stop.
		var reason string
		err := s.db.QueryRowContext(ctx, `SELECT fail_reason FROM runs
			WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND status = ?`,
			id, workerID, keyID, tokenHash, string(StatusCancelled)).Scan(&reason)
		if err == nil {
			return fmt.Errorf("%w: %s", ErrRunCancelled, cmp.Or(reason, "cancelled"))
		}
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
	setupJSON := ""
	if len(in.Setup) > 0 {
		if b, err := json.Marshal(in.Setup); err == nil {
			setupJSON = string(b)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET status = ?, result_summary = ?, head_sha = CASE WHEN ? != '' THEN ? ELSE head_sha END, diffstat = ?, checks = ?,
			log_ref = ?, fail_reason = ?, branch = CASE WHEN ? != '' THEN ? ELSE branch END,
			base_sha = CASE WHEN ? != '' THEN ? ELSE base_sha END,
			executor = CASE WHEN ? != '' THEN ? ELSE executor END,
			grants = CASE WHEN ? != '' THEN ? ELSE grants END,
			pr_url = CASE WHEN ? != '' THEN ? ELSE pr_url END,
			setup = CASE WHEN ? != '' THEN ? ELSE setup END, fix_rounds = ?, phase = '',
			cost_usd = `+addCost+`, findings = COALESCE(?, findings), pushed = COALESCE(?, pushed),
			lease_expires_at = '', resume_after = '', updated_at = ?
		WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND status IN (?, ?, ?)`,
		string(in.Status), in.Summary, in.HeadSHA, in.HeadSHA, in.Diffstat, string(checks),
		in.LogRef, in.Reason, in.Branch, in.Branch, in.BaseSHA, in.BaseSHA, in.Executor, in.Executor, in.Grants, in.Grants, in.PRURL, in.PRURL, setupJSON, setupJSON, in.FixRounds,
		costArg(in.CostUSD), costArg(in.CostUSD), findingsJSON(in.Findings), boolArg(in.Pushed),
		formatTime(now), id, workerID, keyID, tokenHash, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return nil, fmt.Errorf("reporting run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrLeaseHeld
	}
	row := tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	run, err := scanRun(row)
	if err != nil {
		return nil, err
	}
	// The record of findings, with the report: what this review found new,
	// and what the run says about earlier findings. Earlier blocking
	// findings it found still open count against it (BlockingFindings).
	if s.findings != nil {
		round, err := findingRound(ctx, tx, run)
		if err != nil {
			return nil, err
		}
		// Passed: a review that found nothing blocking of its own (what it
		// found still open is counted by the record).
		passed := run.Status == StatusSucceeded && BlockingFindings(*run) == 0
		still, err := s.findings.RecordRun(ctx, tx, findingRun(run), round, newFindings(in.Findings), in.FindingAnswers, passed, now)
		if err != nil {
			return nil, err
		}
		if still > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET still_open = ? WHERE id = ?`, still, id); err != nil {
				return nil, fmt.Errorf("recording still-open findings: %w", err)
			}
			run.StillOpen = still
		}
	}
	if err := s.events.Emit(ctx, tx, finishedEvents(run)...); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

// Requeue ends a failed attempt by putting the run back in the queue
// (a retryable failure): the same fencing as Report; the attempt's setup
// and checks stay visible, fail_reason says what happened; owner, lease and
// session go — any worker may claim it after retryAfter, except avoid
// until avoidUntil (the claim decides).
func (s *Storage) Requeue(ctx context.Context, id, workerID, keyID, tokenHash string, in ReportInput, note string, retryAfter time.Time, avoid string, now time.Time) (*Run, error) {
	checks, err := json.Marshal(in.Checks)
	if err != nil {
		checks = []byte("[]")
	}
	setupJSON := ""
	if len(in.Setup) > 0 {
		if b, err := json.Marshal(in.Setup); err == nil {
			setupJSON = string(b)
		}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, worker_id = '', resume_kind = '', lease_expires_at = '', resume_after = '',
			lease_token_hash = '', claimed_key_id = '', phase = '', fail_reason = ?, checks = ?,
			setup = CASE WHEN ? != '' THEN ? ELSE setup END, cost_usd = `+addCost+`,
			retries = retries + 1, retry_after = ?, avoid_worker = ?, prefer_worker = '', prefer_until = '',
			runner = CASE WHEN runner_picked = 1 THEN '' ELSE runner END, runner_picked = 0,
			attempt = attempt + 1, updated_at = ?
		WHERE id = ? AND worker_id = ? AND claimed_key_id = ? AND lease_token_hash = ? AND status IN (?, ?, ?)`,
		string(StatusQueued), note, string(checks), setupJSON, setupJSON, costArg(in.CostUSD), costArg(in.CostUSD),
		formatTime(retryAfter), avoid, formatTime(now),
		id, workerID, keyID, tokenHash, string(StatusClaimed), string(StatusRunning), string(StatusPaused))
	if err != nil {
		return nil, fmt.Errorf("requeueing run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrLeaseHeld
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

// formatPreferUntil stores the zero time as ” (no preference).
func formatPreferUntil(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return formatTime(t)
}

// workerCanTake: the preferred worker could claim r soon. It polled or sent
// a heartbeat within workerState's idle window and has not said goodbye,
// it still offers r's workspace (healthy) and runner. A run waiting for it
// stops waiting the moment that no longer holds.
func workerCanTake(ctx context.Context, tx *sql.Tx, id string, r *Run, prefs *WorkspacePrefs, now time.Time) (bool, error) {
	var lastSeen, offlineAt, rawRunners, rawFacts, rawExecutors string
	var pollMS int64
	var offers, healthy, runs, paused int
	err := tx.QueryRowContext(ctx, `
		SELECT last_seen, COALESCE(poll_ms, 0), COALESCE(offline_at, ''),
			EXISTS (SELECT 1 FROM json_each(w.workspaces) WHERE value = ?),
			NOT EXISTS (SELECT 1 FROM json_each(COALESCE(w.unhealthy, '{}')) WHERE key = ?),
			(? = '' OR EXISTS (SELECT 1 FROM json_each(w.runners) WHERE value = ?)),
			`+pausedSQL+`, w.runners, COALESCE(w.runner_info, ''), COALESCE(w.executor_info, '')
		FROM workers w WHERE w.id = ?`, r.WorkspaceID, r.WorkspaceID, r.Runner, r.Runner, formatTime(now), id).
		Scan(&lastSeen, &pollMS, &offlineAt, &offers, &healthy, &runs, &paused, &rawRunners, &rawFacts, &rawExecutors)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading worker %s: %w", id, err)
	}
	if offlineAt != "" || offers == 0 || healthy == 0 || runs == 0 || paused == 1 {
		return false, nil
	}
	// The policy too, by what that worker reports: a run must not wait out
	// its preference for a worker that may not run it.
	var offered []string
	_ = json.Unmarshal([]byte(rawRunners), &offered)
	if !decide(r, prefs, newClaimFacts(offered, parseFacts(rawFacts), parseExecutors(rawExecutors))).ok {
		return false, nil
	}
	window := 3 * time.Duration(pollMS) * time.Millisecond
	if window < idleWindowMin {
		window = idleWindowMin
	}
	return now.Sub(parseTime(lastSeen)) <= window, nil
}

// retryWaiting: a requeued run not yet claimable by this worker — before
// its back-off, or (for the worker that failed it last) before the avoid
// window ends.
func retryWaiting(r *Run, workerID string, now time.Time) bool {
	if r.RetryAfter.IsZero() {
		return false
	}
	if now.Before(r.RetryAfter) {
		return true
	}
	return r.AvoidWorker != "" && r.AvoidWorker == workerID && now.Before(r.RetryAfter.Add(avoidWindow))
}

// unhealthyObject is the health map as a JSON object for json_each ("{}"
// when all healthy).
func unhealthyObject(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// addCost adds a report's cost to the run's (two costArg parameters): a
// requeued run is reported once per attempt, and each report carries only
// what was spent since the last one.
const addCost = `CASE WHEN ? IS NULL THEN cost_usd ELSE COALESCE(cost_usd, 0) + ? END`

func costArg(c *float64) any {
	if c == nil {
		return nil
	}
	return *c
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
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
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

// AbleWorker is a worker that can index a workspace now; Busy: it holds a
// claimed or running run (it indexes only while idle).
type AbleWorker struct {
	ID   string
	Busy bool
}

// IndexableWorkers is the one rule for who can index a workspace (the run
// queue's index duty and the pipeline view both use it): the worker's
// registry row names ws indexable, it was seen since cutoff and has not
// said goodbye, and is not paused (at now). Sorted by id.
func IndexableWorkers(ctx context.Context, db *sql.DB, ws string, cutoff, now time.Time) ([]AbleWorker, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT w.id, EXISTS (SELECT 1 FROM runs r WHERE r.worker_id = w.id AND r.status IN ('claimed', 'running'))
		FROM workers w, json_each(w.indexable) j
		WHERE j.value = ? AND w.last_seen >= ? AND w.offline_at IS NULL AND NOT `+pausedSQL+`
		ORDER BY w.id`, ws, formatTime(cutoff), formatTime(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AbleWorker
	for rows.Next() {
		var a AbleWorker
		var busy int
		if err := rows.Scan(&a.ID, &busy); err != nil {
			return nil, err
		}
		a.Busy = busy == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// IndexableWorkers: see the package function.
func (s *Storage) IndexableWorkers(ctx context.Context, ws string, cutoff, now time.Time) ([]AbleWorker, error) {
	return IndexableWorkers(ctx, s.db, ws, cutoff, now)
}
