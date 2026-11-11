package cleanup

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/martinsuchenak/skopos/internal/dbtime"
	"time"

	"github.com/paularlott/logger"

	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/idempotency"
	"github.com/martinsuchenak/skopos/internal/integrations"
)

type Cleaner struct {
	db        *sql.DB
	retention time.Duration
	interval  time.Duration
	log       logger.Logger
	hub       *events.Hub
}

func NewCleaner(db *sql.DB, retention time.Duration, log logger.Logger, hub *events.Hub) *Cleaner {
	return &Cleaner{
		db:        db,
		retention: retention,
		interval:  10 * time.Minute,
		log:       log,
		hub:       hub,
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

// eventBatch bounds one delete statement: the write lock is released between
// batches, so request writes interleave with a large cleanup instead of
// waiting for all of it.
const eventBatch = 1000

// slowCleanup: a cleanup run longer than this is logged at warn.
const slowCleanup = time.Second

// deleteEventsBefore deletes events older than cutoff in batches.
func (c *Cleaner) deleteEventsBefore(ctx context.Context, cutoff string) (int64, error) {
	var total int64
	for {
		res, err := c.db.ExecContext(ctx, `
			DELETE FROM events WHERE rowid IN (
				SELECT rowid FROM events WHERE created_at < ? LIMIT ?)`, cutoff, eventBatch)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < eventBatch || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

func (c *Cleaner) clean(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-c.retention)
	cutoffStr := dbtime.Format(cutoff)

	start := time.Now()
	eventsDeleted, err := c.deleteEventsBefore(ctx, cutoffStr)
	if err != nil {
		return fmt.Errorf("deleting old events: %w", err)
	}
	eventsTook := time.Since(start)

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

	inboxResult, err := c.db.ExecContext(ctx, `DELETE FROM inbox_items WHERE status IN ('discarded', 'done') AND updated_at < ?`, cutoffStr)
	if err != nil {
		return fmt.Errorf("deleting old inbox items: %w", err)
	}
	inboxDeleted, _ := inboxResult.RowsAffected()

	// Run logs (gzipped batches, Unix ms) past the retention.
	if _, err := c.db.ExecContext(ctx, `DELETE FROM run_logs WHERE created_ms < ?`, cutoff.UnixMilli()); err != nil {
		return fmt.Errorf("deleting old run logs: %w", err)
	}

	// Run tokens that ended before the retention (hashes of credentials
	// that stopped working then; a token still open is never touched).
	if _, err := c.db.ExecContext(ctx, `DELETE FROM run_tokens WHERE ended_ms IS NOT NULL AND ended_ms < ?`, cutoff.UnixMilli()); err != nil {
		return fmt.Errorf("deleting old run tokens: %w", err)
	}

	// Integration events and webhook deliveries past the retention (events
	// a subscription still has to deliver stay).
	if err := integrations.Prune(ctx, c.db, cutoff); err != nil {
		return err
	}

	// Idempotency keys past their 24 hours.
	if _, err := idempotency.Prune(ctx, c.db, time.Now()); err != nil {
		return fmt.Errorf("deleting old idempotency keys: %w", err)
	}

	agentsResult, err := c.db.ExecContext(ctx, `DELETE FROM agents WHERE last_seen_at < ?`, cutoffStr)
	if err != nil {
		return fmt.Errorf("deleting stale agents: %w", err)
	}
	agentsDeleted, _ := agentsResult.RowsAffected()

	// Worker stage-plan orphans (round-6 F4, tightened round 7): a plan
	// created but never linked (a crash between CreatePlan and LinkPlan)
	// carries nothing — no items, no revisions, no linked inbox item.
	// Older than a day AND worker-created (the worker-id prefix the config
	// enforces), a hand-made empty plan is never touched. Dependent rows
	// (plan_items, plan_revisions, plan_dependencies) go via the schema's
	// ON DELETE CASCADE — the server DSN enables foreign_keys.
	orphanCutoff := dbtime.Format(time.Now().Add(-24 * time.Hour))
	orphanResult, err := c.db.ExecContext(ctx, `
		DELETE FROM plans
		WHERE created_at < ? AND author_agent_id LIKE 'worker-%'
		  AND NOT EXISTS (SELECT 1 FROM plan_items WHERE plan_items.plan_id = plans.id)
		  AND NOT EXISTS (SELECT 1 FROM plan_revisions WHERE plan_revisions.plan_id = plans.id)
		  AND NOT EXISTS (SELECT 1 FROM inbox_items WHERE inbox_items.plan_id = plans.id)`,
		orphanCutoff)
	if err != nil {
		return fmt.Errorf("deleting orphaned plans: %w", err)
	}
	orphanDeleted, _ := orphanResult.RowsAffected()
	plansDeleted += orphanDeleted

	total := eventsDeleted + sessionsDeleted + bbDeleted + plansDeleted + inboxDeleted + agentsDeleted
	if took := time.Since(start); took >= slowCleanup && c.log != nil {
		// Each delete holds SQLite's write lock while it runs, so a slow
		// cleanup stalls every writer (and, through the pool, readers too).
		c.log.Warn("slow cleanup", "duration", took.Round(time.Millisecond).String(),
			"events_duration", eventsTook.Round(time.Millisecond).String(), "events", eventsDeleted)
	}
	if total > 0 && c.log != nil {
		c.log.Info("cleanup completed",
			"events", eventsDeleted,
			"sessions", sessionsDeleted,
			"blackboard", bbDeleted,
			"plans", plansDeleted,
			"inbox", inboxDeleted,
			"agents", agentsDeleted,
		)
	}

	// These deletions bypass the request-scoped middleware; tell SSE clients
	// which views changed.
	if c.hub != nil {
		if eventsDeleted > 0 || sessionsDeleted > 0 || agentsDeleted > 0 {
			c.hub.Publish(events.Event{Type: "sessions"})
		}
		if bbDeleted > 0 {
			c.hub.Publish(events.Event{Type: "blackboard"})
		}
		if plansDeleted > 0 {
			c.hub.Publish(events.Event{Type: "plans"})
		}
		if inboxDeleted > 0 {
			c.hub.Publish(events.Event{Type: events.TypeInbox})
		}
	}

	return nil
}
