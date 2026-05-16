package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/paularlott/logger"
)

type Cleaner struct {
	db        *sql.DB
	retention time.Duration
	interval  time.Duration
	log       logger.Logger
}

func NewCleaner(db *sql.DB, retention time.Duration, log logger.Logger) *Cleaner {
	return &Cleaner{
		db:        db,
		retention: retention,
		interval:  10 * time.Minute,
		log:       log,
	}
}

func (c *Cleaner) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.clean(ctx); err != nil {
					if c.log != nil {
						c.log.Warn("cleanup failed", "error", err)
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (c *Cleaner) RunOnce(ctx context.Context) error {
	return c.clean(ctx)
}

func (c *Cleaner) clean(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-c.retention)
	cutoffStr := cutoff.Format(time.RFC3339Nano)

	eventsResult, err := c.db.ExecContext(ctx, `DELETE FROM events WHERE created_at < ?`, cutoffStr)
	if err != nil {
		return fmt.Errorf("deleting old events: %w", err)
	}
	eventsDeleted, _ := eventsResult.RowsAffected()

	sessionsResult, err := c.db.ExecContext(ctx, `DELETE FROM sessions WHERE status = 'orphaned' AND updated_at < ?`, cutoffStr)
	if err != nil {
		return fmt.Errorf("deleting orphaned sessions: %w", err)
	}
	sessionsDeleted, _ := sessionsResult.RowsAffected()

	blackboardResult, err := c.db.ExecContext(ctx, `DELETE FROM blackboard_entries WHERE scope = 'session' AND session_id NOT IN (SELECT id FROM sessions)`)
	if err != nil {
		return fmt.Errorf("deleting orphaned blackboard entries: %w", err)
	}
	bbDeleted, _ := blackboardResult.RowsAffected()

	plansResult, err := c.db.ExecContext(ctx, `DELETE FROM plans WHERE status IN ('completed', 'archived') AND updated_at < ?`, cutoffStr)
	if err != nil {
		return fmt.Errorf("deleting old completed plans: %w", err)
	}
	plansDeleted, _ := plansResult.RowsAffected()

	agentsResult, err := c.db.ExecContext(ctx, `DELETE FROM agents WHERE last_seen_at < ?`, cutoffStr)
	if err != nil {
		return fmt.Errorf("deleting stale agents: %w", err)
	}
	agentsDeleted, _ := agentsResult.RowsAffected()

	total := eventsDeleted + sessionsDeleted + bbDeleted + plansDeleted + agentsDeleted
	if total > 0 && c.log != nil {
		c.log.Info("cleanup completed",
			"events", eventsDeleted,
			"sessions", sessionsDeleted,
			"blackboard", bbDeleted,
			"plans", plansDeleted,
			"agents", agentsDeleted,
		)
	}

	return nil
}
