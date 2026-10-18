package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/martinsuchenak/skopos/internal/dbtime"
	_ "modernc.org/sqlite"
)

// Rows written in the old RFC3339Nano form (trimmed zeros, an offset) are
// rewritten to the fixed-width format on the next migration run; ” and
// NULL sentinels are left alone; a second run changes nothing.
func TestNormalizeTimestamps(t *testing.T) {
	conn, err := sql.Open("sqlite", sqliteDSN(filepath.Join(t.TempDir(), "n.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := RunMigrations(conn); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]string{
		"w1": "2026-10-17T10:00:05Z",
		"w2": "2026-10-17T10:00:05.5Z",
		"w3": "2026-10-17T18:00:05.12345+08:00",
	}
	for id, ts := range legacy {
		if _, err := conn.Exec(`INSERT INTO workspaces (id, created_at) VALUES (?, ?)`, id, ts); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO runs (id, workspace_id, item_id, stage, status, lease_expires_at, created_at, updated_at) VALUES ('r1','w','i','plan','queued','', '2026-10-17T10:00:05.1Z', '2026-10-17T10:00:05.1Z')`); err != nil {
		t.Fatal(err)
	}
	if err := RunMigrations(conn); err != nil {
		t.Fatal(err)
	}
	for id, ts := range legacy {
		var got string
		if err := conn.QueryRow(`SELECT created_at FROM workspaces WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		want, _ := dbtime.Parse(ts)
		if got != dbtime.Format(want) {
			t.Errorf("%s: %q, want %q", id, got, dbtime.Format(want))
		}
	}
	var lease, created string
	_ = conn.QueryRow(`SELECT lease_expires_at, created_at FROM runs WHERE id = 'r1'`).Scan(&lease, &created)
	if lease != "" || created != "2026-10-17T10:00:05.100000000Z" {
		t.Fatalf("runs: lease %q (sentinel must stay), created %q", lease, created)
	}
	// Ordering now matches time: w3 (…05.12345 in UTC) between w1 and w2.
	rows, _ := conn.Query(`SELECT id FROM workspaces WHERE id LIKE 'w%' ORDER BY created_at`)
	var order []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		order = append(order, id)
	}
	_ = rows.Close()
	if len(order) != 3 || order[0] != "w1" || order[1] != "w3" || order[2] != "w2" {
		t.Fatalf("order after normalising: %v", order)
	}
}
