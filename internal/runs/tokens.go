package runs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// Per-attempt tokens (tmp/central-config — design.md, phases 0 and 1b). A
// claim mints an agent token for the attempt: the credential the run's
// agent reaches skopos with over MCP, in place of an agent key shared by
// every run on the worker. It is returned in clear only in the claim
// response; run_tokens keeps its hash.
//
// A token is valid only while its run is being executed: the run is
// claimed or running, the token is the attempt's and has not been ended,
// and the lease has not expired. A report, a requeue, a pause, a cancel or
// an expired lease therefore ends it at once — read from the run, so no
// transition has to remember tokens. ended_ms does two things:
//   - the next claim's mint sets it on the run's earlier tokens. This is
//     what kills the old token when a paused run resumes: the attempt
//     number does not change there, so nothing else would.
//   - settleTokens sets it at every claim poll for runs no longer being
//     executed, so the table says when a token stopped working (to the
//     nearest poll, not to the instant).
//
// The table also has room for the lease token and a run-log token
// (soc2-audit — design.md §3.10); only the agent token lives here so far.

// TokenAgent is the run_tokens kind of a run's agent token.
const TokenAgent = "agent"

// newAgentToken: 128 random bits behind the run-token prefix.
func newAgentToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return auth.RunTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// mintAgentToken ends the run's earlier agent tokens and stores a new one
// for this attempt, inside the claim's transaction.
func mintAgentToken(ctx context.Context, tx *sql.Tx, runID string, attempt int, now time.Time) (string, error) {
	if _, err := tx.ExecContext(ctx, `
		UPDATE run_tokens SET ended_ms = ? WHERE run_id = ? AND kind = ? AND ended_ms IS NULL`,
		now.UnixMilli(), runID, TokenAgent); err != nil {
		return "", fmt.Errorf("ending the run's earlier agent tokens: %w", err)
	}
	token := newAgentToken()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO run_tokens (token_hash, run_id, attempt, kind, issued_ms) VALUES (?, ?, ?, ?, ?)`,
		auth.HashKey(token), runID, attempt, TokenAgent, now.UnixMilli()); err != nil {
		return "", fmt.Errorf("storing the agent token: %w", err)
	}
	return token, nil
}

// settleTokens records the end of every open token whose run is no longer
// being executed. Those tokens are already refused (LookupRunToken reads
// the run); this keeps ended_ms true for whoever reads the table later.
func settleTokens(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE run_tokens SET ended_ms = ?
		WHERE ended_ms IS NULL AND NOT EXISTS (
			SELECT 1 FROM runs r WHERE r.id = run_tokens.run_id AND r.attempt = run_tokens.attempt
				AND r.status IN (?, ?) AND r.lease_expires_at != '' AND r.lease_expires_at > ?)`,
		now.UnixMilli(), string(StatusClaimed), string(StatusRunning), formatTime(now))
	if err != nil {
		return fmt.Errorf("settling run tokens: %w", err)
	}
	return nil
}

// LookupRunToken implements auth.RunLookup: the run an agent token belongs
// to, while that run is being executed. An unknown, ended or stale token
// returns (nil, nil), like an unknown key.
func (s *Storage) LookupRunToken(ctx context.Context, tokenHash string) (*auth.RunIdentity, error) {
	return s.lookupRunToken(ctx, tokenHash, time.Now().UTC())
}

func (s *Storage) lookupRunToken(ctx context.Context, tokenHash string, now time.Time) (*auth.RunIdentity, error) {
	var run auth.RunIdentity
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.workspace_id, r.item_id, COALESCE(i.plan_id, ''), r.stage, r.runner, r.worker_id
		FROM run_tokens t
		JOIN runs r ON r.id = t.run_id
		LEFT JOIN inbox_items i ON i.id = r.item_id
		WHERE t.token_hash = ? AND t.kind = ? AND t.ended_ms IS NULL
			AND t.attempt = r.attempt AND r.status IN (?, ?) AND r.worker_id != ''
			AND r.lease_expires_at != '' AND r.lease_expires_at > ?`,
		tokenHash, TokenAgent, string(StatusClaimed), string(StatusRunning), formatTime(now)).
		Scan(&run.ID, &run.WorkspaceID, &run.ItemID, &run.PlanID, &run.Stage, &run.Runner, &run.WorkerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up run token: %w", err)
	}
	return &run, nil
}
