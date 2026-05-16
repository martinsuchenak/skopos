package cleanup

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
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

	cleaner := NewCleaner(db, 24*time.Hour, nil)
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
