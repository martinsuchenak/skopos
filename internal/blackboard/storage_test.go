package blackboard

import (
	"context"
	"database/sql"
	"errors"
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
		t.Fatalf("enable foreign keys: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewStorage(sqlDB)
}

func TestStorageWriteAndBundle(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	entry := Entry{
		ID:            "entry-1",
		Scope:         ScopeBranch,
		BranchName:    "feat-auth",
		EntryType:     TypeFinding,
		Title:         "JWT not checked",
		Content:       "Tokens bypass expiry.",
		CodeRef:       "auth/jwt.go:45",
		AuthorAgentID: "agent-1",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := s.Bundle(ctx, "", "feat-auth", "")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Title != "JWT not checked" {
		t.Errorf("title: got %q", entries[0].Title)
	}
	if entries[0].CodeRef != "auth/jwt.go:45" {
		t.Errorf("code_ref: got %q", entries[0].CodeRef)
	}
}

func TestStorageBundleFloatingTypesAlwaysIncluded(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.Write(ctx, Entry{
		ID: "bug-1", Scope: ScopeBranch, BranchName: "feat-auth",
		EntryType: TypeBug, Title: "Critical bug", AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write bug: %v", err)
	}

	entries, err := s.Bundle(ctx, "", "main", "")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 floating entry, got %d", len(entries))
	}
	if entries[0].EntryType != TypeBug {
		t.Errorf("expected bug type, got %q", entries[0].EntryType)
	}
}

func TestStorageBundleSessionScope(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Create a sessions row so the FK is satisfied
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, status, started_at, updated_at)
		 VALUES ('sess-1', 'test', '/repo', 'running', ?, ?)`,
		formatTime(now), formatTime(now))
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if err := s.Write(ctx, Entry{
		ID: "e-1", Scope: ScopeSession, SessionID: "sess-1",
		EntryType: TypeContext, Title: "Local context",
		AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := s.Bundle(ctx, "", "", "sess-1")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry with session, got %d", len(entries))
	}

	entries, err = s.Bundle(ctx, "", "", "sess-other")
	if err != nil {
		t.Fatalf("bundle other: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for other session, got %d", len(entries))
	}
}

func TestStorageOnDeleteCascadeRemovesSessionEntries(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, status, started_at, updated_at)
		 VALUES ('sess-del', 'test', '/repo', 'running', ?, ?)`, formatTime(now), formatTime(now))
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := s.Write(ctx, Entry{
		ID: "e-del", Scope: ScopeSession, SessionID: "sess-del",
		EntryType: TypeContext, Title: "Gone soon",
		AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = 'sess-del'`)
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}

	err = s.DeleteBySession(ctx, "sess-del")
	if err != nil {
		t.Fatalf("delete by session: %v", err)
	}

	entries, err := s.Bundle(ctx, "", "", "sess-del")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries after cascade delete, got %d", len(entries))
	}
}

func TestStoragePromote(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// session -> branch requires a sessions row
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, status, started_at, updated_at) VALUES ('s1','t','/r','running',?,?)`,
		formatTime(now), formatTime(now)); err != nil {
		t.Fatalf("insert session for promote test: %v", err)
	}

	if err := s.Write(ctx, Entry{
		ID: "p-1", Scope: ScopeSession, SessionID: "s1",
		BranchName: "feat", EntryType: TypeFinding, Title: "Promote me",
		AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := s.Promote(ctx, "p-1"); err != nil {
		t.Fatalf("promote session->branch: %v", err)
	}
	e, err := s.Get(ctx, "p-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.Scope != ScopeBranch {
		t.Errorf("expected branch scope, got %q", e.Scope)
	}

	if err := s.Promote(ctx, "p-1"); err != nil {
		t.Fatalf("promote branch->project: %v", err)
	}
	e, err = s.Get(ctx, "p-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.Scope != ScopeProject {
		t.Errorf("expected project scope, got %q", e.Scope)
	}
	if e.BranchName != "" {
		t.Errorf("branch_name should be empty at project scope, got %q", e.BranchName)
	}

	err = s.Promote(ctx, "p-1")
	if err == nil {
		t.Fatal("expected error promoting project-scope entry")
	}
	if !errors.Is(err, ErrAlreadyAtTopScope) {
		t.Errorf("expected ErrAlreadyAtTopScope, got %v", err)
	}
}

func TestStorageDelete(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.Write(ctx, Entry{
		ID: "del-1", Scope: ScopeProject, EntryType: TypeDecision,
		Title: "To delete", AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Delete(ctx, "del-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := s.Get(ctx, "del-1")
	if err == nil {
		t.Fatal("expected ErrNotFound after delete")
	}
}

func TestStorageBundleWorkspaceFiltering(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.Write(ctx, Entry{
		ID: "ws-global", Scope: ScopeProject, EntryType: TypeFinding,
		Title: "Global finding", AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write global: %v", err)
	}
	if err := s.Write(ctx, Entry{
		ID: "ws-a", Scope: ScopeProject, EntryType: TypeFinding, WorkspaceID: "ws-a",
		Title: "Workspace A finding", AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write ws-a: %v", err)
	}
	if err := s.Write(ctx, Entry{
		ID: "ws-b", Scope: ScopeProject, EntryType: TypeFinding, WorkspaceID: "ws-b",
		Title: "Workspace B finding", AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write ws-b: %v", err)
	}

	entries, err := s.Bundle(ctx, "ws-a", "", "")
	if err != nil {
		t.Fatalf("bundle ws-a: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries (global + ws-a), got %d", len(entries))
	}
	titles := map[string]bool{}
	for _, e := range entries {
		titles[e.Title] = true
	}
	if !titles["Global finding"] {
		t.Error("expected global finding in ws-a results")
	}
	if !titles["Workspace A finding"] {
		t.Error("expected workspace A finding in ws-a results")
	}
	if titles["Workspace B finding"] {
		t.Error("did not expect workspace B finding in ws-a results")
	}

	all, err := s.Bundle(ctx, "", "", "")
	if err != nil {
		t.Fatalf("bundle all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 entries with no workspace filter, got %d", len(all))
	}
}
