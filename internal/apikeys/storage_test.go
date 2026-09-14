package apikeys

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testStorage(t *testing.T) *Storage {
	t.Helper()
	// File DB (not :memory:): pooled connections must share one database.
	dsn := filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	return NewStorage(sqlDB)
}

func TestLookupKey(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()

	now := "2026-09-14T00:00:00Z"
	s.db.ExecContext(ctx, `INSERT INTO api_keys (id, name, key_hash, key_prefix, all_workspaces, created_at)
		VALUES ('k1', 'ci', 'hash-1', 'sk_prefix1', 0, ?)`, now)
	s.db.ExecContext(ctx, `INSERT INTO api_key_workspaces (api_key_id, workspace_id) VALUES ('k1', 'github.com/o/a'), ('k1', 'github.com/o/b')`)
	s.db.ExecContext(ctx, `INSERT INTO api_keys (id, name, key_hash, key_prefix, all_workspaces, created_at)
		VALUES ('k2', 'revoked', 'hash-2', 'sk_prefix2', 1, ?)`, now)
	s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = 'k2'`, now)
	s.db.ExecContext(ctx, `INSERT INTO api_keys (id, name, key_hash, key_prefix, all_workspaces, created_at)
		VALUES ('k3', 'all', 'hash-3', 'sk_prefix3', 1, ?)`, now)

	info, err := s.LookupKey(ctx, "hash-1")
	if err != nil || info == nil {
		t.Fatalf("expected k1, got %+v err %v", info, err)
	}
	if info.ID != "k1" || info.AllWorkspaces || len(info.Workspaces) != 2 ||
		info.Workspaces[0] != "github.com/o/a" || info.Workspaces[1] != "github.com/o/b" {
		t.Fatalf("unexpected scope: %+v", info)
	}

	if info, err := s.LookupKey(ctx, "hash-2"); err != nil || info != nil {
		t.Fatalf("revoked key must resolve to nil, got %+v err %v", info, err)
	}
	info, err = s.LookupKey(ctx, "hash-3")
	if err != nil || info == nil || !info.AllWorkspaces || len(info.Workspaces) != 0 {
		t.Fatalf("all-workspaces key: %+v err %v", info, err)
	}
	if info, err := s.LookupKey(ctx, "unknown"); err != nil || info != nil {
		t.Fatalf("unknown key must resolve to nil, got %+v err %v", info, err)
	}
}

// Compile-time: Storage satisfies the resolver interface the authenticator uses.
var _ auth.KeyLookup = (*Storage)(nil)

// HashKeyForTest mirrors auth.HashKey without an inter-package test leak.
func HashKeyForTest(secret string) string {
	return auth.HashKey(secret)
}

// TestListAllWorkspacesKeyEmitsEmptySlice pins the API contract: an
// all-workspaces key has no scope rows, and List must emit [] (not null) so
// every client can treat workspaces as a list (regression: the dashboard's
// list rendering crashed on null and showed no keys at all).
func TestListAllWorkspacesKeyEmitsEmptySlice(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := "2026-09-14T00:00:00Z"
	s.db.ExecContext(ctx, `INSERT INTO api_keys (id, name, key_hash, key_prefix, all_workspaces, created_at)
		VALUES ('ka', 'all', 'h', 'sk_p', 1, ?)`, now)
	keys, err := s.List(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list: %+v %v", keys, err)
	}
	if keys[0].Workspaces == nil || len(keys[0].Workspaces) != 0 {
		t.Fatalf("all-workspaces key must emit [], got %#v", keys[0].Workspaces)
	}
	got, err := s.Get(ctx, "ka")
	if err != nil || got.Workspaces == nil {
		t.Fatalf("get: %+v %v", got, err)
	}
}
