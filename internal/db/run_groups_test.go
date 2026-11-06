package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// A database from before review-only panels has run_groups with the narrow
// stage CHECK: the migration rebuilds it once, keeping its rows and indexes,
// and then accepts 'branch_review'.
func TestWidenRunGroupStages(t *testing.T) {
	conn, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "old.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := RunMigrations(conn); err != nil {
		t.Fatal(err)
	}
	// Recreate the old definition (as an older release left it), with a row.
	var ddl string
	if err := conn.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'run_groups'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(ddl, "'qa', 'branch_review')", "'qa')", 1)
	for _, stmt := range []string{
		`DROP TABLE run_groups`, old,
		`INSERT INTO inbox_items (id, title, author_agent_id, created_at, updated_at) VALUES ('it', 't', 'a', 'x', 'x')`,
		`INSERT INTO run_groups (id, workspace_id, item_id, stage, round, size, policy, deadline_ms, created_ms) VALUES ('g1', 'ws', 'it', 'review', 1, 2, 'any-blocks', 1, 1)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO run_groups (id, workspace_id, item_id, stage, round, size, policy, deadline_ms, created_ms) VALUES ('g0', 'ws', 'it', 'branch_review', 1, 2, 'any-blocks', 1, 1)`); err == nil {
		t.Fatal("the old CHECK should refuse branch_review")
	}
	for i := 0; i < 2; i++ { // the second run is a no-op
		if err := RunMigrations(conn); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM run_groups WHERE id = 'g1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("row lost: %d %v", n, err)
	}
	if _, err := conn.Exec(`INSERT INTO run_groups (id, workspace_id, item_id, stage, round, size, policy, deadline_ms, created_ms) VALUES ('g2', 'ws', 'it', 'branch_review', 1, 2, 'any-blocks', 1, 1)`); err != nil {
		t.Fatalf("branch_review refused after the migration: %v", err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name IN ('idx_run_groups_open', 'idx_run_groups_deadline')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("indexes: %d %v", n, err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'run_groups_wide'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("leftover table: %d", n)
	}
}
