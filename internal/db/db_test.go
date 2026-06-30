package db

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestSchemaFSExists(t *testing.T) {
	data, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("failed to read embedded schema: %v", err)
	}
	if len(data) == 0 {
		t.Error("schema.sql should not be empty")
	}
}

// TestSessionDeleteCascadesBlackboard proves that foreign-key enforcement is
// active (via the DSN pragma) and that deleting a session cascades to its
// session-scoped blackboard entries.
func TestSessionDeleteCascadesBlackboard(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("sqlite", sqliteDSN(":memory:"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })

	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, status, started_at, updated_at) VALUES ('s1','t','w','running','2024-01-01T00:00:00Z','2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO blackboard_entries (id, scope, session_id, entry_type, title, content, author_agent_id, created_at, updated_at)
		 VALUES ('b1','session','s1','finding','x','','a1','2024-01-01T00:00:00Z','2024-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM sessions WHERE id = 's1'`); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM blackboard_entries WHERE id = 'b1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected blackboard entry to cascade-delete with session, still found %d", n)
	}
}
