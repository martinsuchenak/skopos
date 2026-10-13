package cleanup

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/events"
	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable fk: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return sqlDB
}

func TestCleanerRunOnce(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)

	_, err := db.ExecContext(ctx, `INSERT INTO sessions (id, title, workspace, status, started_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"s1", "Test", "/repo", "orphaned", old.Format(time.RFC3339Nano), old.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO agents (id, type, workspace, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		"a1", "test", "/repo", old.Format(time.RFC3339Nano), old.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO agent_states (session_id, agent_id, agent_type, workspace, status, message, snippet, metadata, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"s1", "a1", "test", "/repo", "stuck", "msg", "", "{}", old.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insert agent state: %v", err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO events (id, session_id, agent_id, agent_type, workspace, status, message, snippet, metadata, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"e1", "s1", "a1", "test", "/repo", "running", "msg", "", "{}", old.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}

	cleaner := NewCleaner(db, 24*time.Hour, nil, nil)
	if err := cleaner.RunOnce(ctx); err != nil {
		t.Fatalf("clean: %v", err)
	}

	var count int
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 sessions, got %d", count)
	}
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 events, got %d", count)
	}
	db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents`).Scan(&count)
	if count != 0 {
		t.Errorf("expected 0 agents, got %d", count)
	}
}

// TestCleanerPublishesEvents verifies out-of-band deletions notify SSE clients
// for the views they affect.
func TestCleanerPublishesEvents(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)

	if _, err := db.ExecContext(ctx, `INSERT INTO sessions (id, title, workspace, status, started_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"s1", "Test", "/repo", "orphaned", old, old); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agents (id, type, workspace, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?, ?)`,
		"a1", "test", "/repo", old, old); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO events (id, session_id, agent_id, agent_type, workspace, status, message, snippet, metadata, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"e1", "s1", "a1", "test", "/repo", "running", "msg", "", "{}", old); err != nil {
		t.Fatalf("insert event: %v", err)
	}

	hub := events.NewHub()
	ch, unsub := hub.Subscribe()
	defer unsub()

	cleaner := NewCleaner(db, 24*time.Hour, nil, hub)
	if err := cleaner.RunOnce(ctx); err != nil {
		t.Fatalf("clean: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Type != "sessions" {
			t.Fatalf("expected sessions event, got %q", ev.Type)
		}
	default:
		t.Fatal("expected a sessions event after deleting old sessions/events")
	}
}

// Round-6 F4 + round 7: empty, unlinked, day-old WORKER-created plans are
// garbage (worker crashes between CreatePlan and LinkPlan); a plan someone
// made by hand, or one with content or a link, survives.
func TestCleanerOrphanPlans(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	fresh := time.Now().UTC().Format(time.RFC3339Nano)

	mkPlan := func(id, author, created string) {
		t.Helper()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO plans (id, workspace_id, name, author_agent_id, created_at, updated_at) VALUES (?, 'ws', ?, ?, ?, ?)`,
			id, id, author, created, created); err != nil {
			t.Fatalf("insert plan %s: %v", id, err)
		}
	}
	// The orphan: empty, unlinked, old, worker-created — dies.
	mkPlan("orphan", "worker-mac", old)
	// Identical but hand-made (no worker- prefix) — kept (round 7).
	mkPlan("hand-made", "martin", old)
	// Fresh but otherwise identical — kept (younger than the grace).
	mkPlan("fresh-empty", "worker-mac", fresh)
	// Old, empty and worker-made, but LINKED to an inbox item — kept.
	mkPlan("linked", "worker-mac", old)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO inbox_items (id, title, author_agent_id, plan_id, created_at, updated_at) VALUES ('i1', 't', 'w', 'linked', ?, ?)`,
		old, old); err != nil {
		t.Fatalf("insert inbox item: %v", err)
	}
	// Old, unlinked and worker-made, but has an item — kept.
	mkPlan("has-items", "worker-mac", old)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO plan_items (id, plan_id, title, created_at, updated_at) VALUES ('pi1', 'has-items', 'step', ?, ?)`,
		old, old); err != nil {
		t.Fatalf("insert plan item: %v", err)
	}

	cleaner := NewCleaner(db, 24*time.Hour, nil, nil)
	if err := cleaner.RunOnce(ctx); err != nil {
		t.Fatalf("clean: %v", err)
	}
	for _, id := range []string{"hand-made", "fresh-empty", "linked", "has-items"} {
		var one int
		if err := db.QueryRowContext(ctx, `SELECT 1 FROM plans WHERE id = ?`, id).Scan(&one); err != nil {
			t.Fatalf("plan %s must survive: %v", id, err)
		}
	}
	var one int
	if err := db.QueryRowContext(ctx, `SELECT 1 FROM plans WHERE id = 'orphan'`).Scan(&one); err == nil {
		t.Fatal("orphan plan must be deleted")
	}
}
