package db

import (
	"context"
	"database/sql"
	"io"
	"strings"
	"testing"

	"github.com/paularlott/logger"
	logslog "github.com/paularlott/logger/slog"
	_ "modernc.org/sqlite"
)

func testLogger() logger.Logger {
	return logslog.New(logslog.Config{Level: "error", Writer: io.Discard})
}

func TestSchemaFSExists(t *testing.T) {
	data, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("failed to read embedded schema: %v", err)
	}
	if len(data) == 0 {
		t.Error("schema.sql should not be empty")
	}
}

func TestSQLiteDSN(t *testing.T) {
	dsn := sqliteDSN(":memory:")
	for _, pragma := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(on)"} {
		if !strings.Contains(dsn, pragma) {
			t.Errorf("DSN missing pragma %q: %s", pragma, dsn)
		}
	}
}

func TestConnect(t *testing.T) {
	db, err := Connect(testLogger(), ":memory:")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestRunMigrationsCreatesAllTables(t *testing.T) {
	db, _ := sql.Open("sqlite", sqliteDSN(":memory:"))
	defer db.Close()
	if err := RunMigrations(db); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	if err := RunMigrations(db); err != nil {
		t.Fatalf("second migration (idempotent): %v", err)
	}
	for _, table := range []string{"sessions", "agents", "agent_states", "events", "blackboard_entries", "plans", "plan_items", "plan_item_dependencies", "plan_dependencies", "workspaces"} {
		var name string
		err := db.QueryRowContext(context.Background(), "SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing after migration: %v", table, err)
		}
	}
}

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

func TestResolveHostDirect(t *testing.T) {
	addr, err := ResolveHost(testLogger(), "localhost:5432")
	if err != nil {
		t.Fatalf("ResolveHost: %v", err)
	}
	if addr.Host != "localhost" || addr.Port != 5432 {
		t.Errorf("got host=%s port=%d", addr.Host, addr.Port)
	}
}

func TestResolveHostInvalid(t *testing.T) {
	_, err := ResolveHost(testLogger(), "no-port-here")
	if err == nil {
		t.Error("expected error for invalid host:port")
	}
}
