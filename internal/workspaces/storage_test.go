package workspaces

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testStorage(t *testing.T) *Storage {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("fk: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	return NewStorage(sqlDB)
}

func TestStorageCreateInsertsThenUpdates(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, err := s.Create(ctx, Workspace{ID: "ws-a", Name: "first", CreatedAt: now})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Fatal("first create should insert (created=true)")
	}

	created, err = s.Create(ctx, Workspace{ID: "ws-a", Name: "renamed", CreatedAt: now})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Fatal("second create should update (created=false)")
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Name != "renamed" {
		t.Fatalf("expected single renamed workspace, got %+v", list)
	}
}

func TestStorageListNewestFirst(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	base := time.Now().UTC()

	for i, id := range []string{"ws-old", "ws-new"} {
		if _, err := s.Create(ctx, Workspace{ID: id, Name: id, CreatedAt: base.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != "ws-new" {
		t.Fatalf("expected newest workspace first, got %+v", list)
	}
}

func TestStorageDeleteMissingReturnsNotFound(t *testing.T) {
	s := testStorage(t)
	err := s.Delete(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected error deleting missing workspace")
	}
}
