package health

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/paularlott/logger"
)

type Checker struct {
	db        *sql.DB
	threshold time.Duration
	interval  time.Duration
	now       func() time.Time
	log       logger.Logger
}

func NewChecker(db *sql.DB, threshold time.Duration, log logger.Logger) *Checker {
	return &Checker{
		db:        db,
		threshold: threshold,
		interval:  60 * time.Second,
		now:       time.Now,
		log:       log,
	}
}

func (c *Checker) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.check(ctx); err != nil {
				c.log.Warn("health check failed", "error", err)
			}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (c *Checker) check(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	t := c.now().UTC()
	now := formatTime(t)
	staleTime := formatTime(t.Add(-c.threshold))

	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, agent_id, agent_type, workspace
		FROM agent_states
		WHERE status IN ('pending', 'running', 'thinking', 'planning', 'editing', 'testing')
		AND updated_at < ?
		AND stuck_at IS NULL
	`, staleTime)
	if err != nil {
		return fmt.Errorf("querying stale agents: %w", err)
	}

	type staleAgent struct{ sessionID, agentID, agentType, workspace string }
	var stale []staleAgent
	for rows.Next() {
		var a staleAgent
		if err := rows.Scan(&a.sessionID, &a.agentID, &a.agentType, &a.workspace); err != nil {
			rows.Close()
			return fmt.Errorf("scanning stale agent: %w", err)
		}
		stale = append(stale, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating stale agents: %w", err)
	}

	for _, a := range stale {
		if _, err := tx.ExecContext(ctx, `
			UPDATE agent_states
			SET original_status = status, status = 'stuck', stuck_at = ?
			WHERE session_id = ? AND agent_id = ?
		`, now, a.sessionID, a.agentID); err != nil {
			return fmt.Errorf("marking agent stuck: %w", err)
		}

		id, err := uuid.NewV7()
		if err != nil {
			id = uuid.New()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO events (id, session_id, agent_id, agent_type, workspace, status, message, snippet, metadata, created_at)
			VALUES (?, ?, ?, ?, ?, 'stuck', 'agent not responding', '', '{}', ?)
		`, id.String(), a.sessionID, a.agentID, a.agentType, a.workspace, now); err != nil {
			return fmt.Errorf("inserting stuck event: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET status = 'orphaned', updated_at = ?
		WHERE status NOT IN ('succeeded', 'failed', 'cancelled', 'orphaned')
		AND EXISTS (
			SELECT 1 FROM agent_states WHERE agent_states.session_id = sessions.id
		)
		AND NOT EXISTS (
			SELECT 1 FROM agent_states
			WHERE agent_states.session_id = sessions.id
			AND agent_states.status NOT IN ('stuck', 'succeeded', 'failed', 'cancelled', 'handoff')
		)
	`, now); err != nil {
		return fmt.Errorf("marking orphaned sessions: %w", err)
	}

	return tx.Commit()
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
