package audit

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testStorage(t *testing.T) *Storage {
	t.Helper()
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

func TestRecordDerivesActor(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()

	// Internal callers render as system.
	if err := svc.Record(ctx, RecordInput{EntityType: "inbox_item", EntityID: "i1", Action: "inbox.transition", Via: ViaSystem}); err != nil {
		t.Fatal(err)
	}
	// Root renders as root.
	rootCtx := auth.WithPrincipal(ctx, &auth.Principal{Root: true})
	if err := svc.Record(rootCtx, RecordInput{EntityType: "inbox_item", EntityID: "i1", Action: "inbox.queue", Via: ViaDashboard}); err != nil {
		t.Fatal(err)
	}
	// Scoped keys render with their name and keep the raw key id.
	keyCtx := auth.WithPrincipal(ctx, &auth.Principal{KeyID: "k1", Name: "relay", Workspaces: map[string]struct{}{"ws": {}}})
	if err := svc.Record(keyCtx, RecordInput{EntityType: "inbox_item", EntityID: "i1", Action: "inbox.approve", Via: ViaSlack, Notes: "wr=108900"}); err != nil {
		t.Fatal(err)
	}
	// Agent-driven mutations render the agent.
	if err := svc.Record(rootCtx, RecordInput{EntityType: "inbox_item", EntityID: "i1", Action: "inbox.enrich", Via: ViaMCP, AgentID: "agent-7"}); err != nil {
		t.Fatal(err)
	}
	// Migration entries render as migration regardless of principal.
	if err := svc.Record(rootCtx, RecordInput{EntityType: "inbox_item", EntityID: "i2", Action: "inbox.migrate", Via: ViaMigration}); err != nil {
		t.Fatal(err)
	}

	entries, err := svc.Timeline(ctx, "inbox_item", "i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d", len(entries))
	}
	// Newest first: the agent entry, then approve, queue, transition.
	wantActors := []string{"agent agent-7", "key relay", "root", "system"}
	for i, want := range wantActors {
		if entries[i].Actor != want {
			t.Errorf("entry %d actor: got %q want %q", i, entries[i].Actor, want)
		}
	}
	if entries[1].ActorKeyID != "k1" {
		t.Errorf("key entry must keep actor_key_id, got %q", entries[1].ActorKeyID)
	}

	// Validation: the triple must be complete.
	if err := svc.Record(ctx, RecordInput{EntityType: "x", Action: "y"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing entity_id must fail: %v", err)
	}
}

func TestListFiltersAndPagination(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := svc.Record(ctx, RecordInput{WorkspaceID: "ws-a", EntityType: "inbox_item", EntityID: "item-a", Action: "inbox.transition"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Record(ctx, RecordInput{WorkspaceID: "ws-b", EntityType: "inbox_item", EntityID: "item-b", Action: "inbox.transition"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Record(ctx, RecordInput{EntityType: "api_key", EntityID: "k1", Action: "key.create"}); err != nil {
		t.Fatal(err)
	}

	// Entity filter.
	entries, err := svc.Timeline(ctx, "inbox_item", "item-a")
	if err != nil || len(entries) != 5 {
		t.Fatalf("entity filter: %d entries, err %v", len(entries), err)
	}

	// Workspace filter.
	entries, err = svc.List(ctx, ListFilter{WorkspaceID: "ws-b"})
	if err != nil || len(entries) != 1 || entries[0].EntityID != "item-b" {
		t.Fatalf("workspace filter: %+v err %v", entries, err)
	}

	// Action prefix.
	entries, err = svc.List(ctx, ListFilter{ActionPrefix: "key."})
	if err != nil || len(entries) != 1 || entries[0].Action != "key.create" {
		t.Fatalf("action prefix: %+v err %v", entries, err)
	}

	// Pagination: page one at limit 3, then continue before the last id.
	page1, err := svc.List(ctx, ListFilter{EntityType: "inbox_item", EntityID: "item-a", Limit: 3})
	if err != nil || len(page1) != 3 {
		t.Fatalf("page 1: %d err %v", len(page1), err)
	}
	page2, err := svc.List(ctx, ListFilter{EntityType: "inbox_item", EntityID: "item-a", Before: page1[len(page1)-1].ID})
	if err != nil || len(page2) != 2 {
		t.Fatalf("page 2: %d err %v", len(page2), err)
	}
	// Pages must not overlap and stay newest-first.
	if page1[0].ID < page1[1].ID || page1[2].ID <= page2[0].ID {
		t.Fatal("pagination order broken")
	}
}

func TestListScoping(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()

	for _, ws := range []string{"ws-a", "ws-b"} {
		if err := svc.Record(ctx, RecordInput{WorkspaceID: ws, EntityType: "inbox_item", EntityID: "i-" + ws, Action: "inbox.transition"}); err != nil {
			t.Fatal(err)
		}
	}
	// Server-level entry: root-only.
	if err := svc.Record(ctx, RecordInput{EntityType: "api_key", EntityID: "k1", Action: "key.create"}); err != nil {
		t.Fatal(err)
	}

	scoped := auth.WithPrincipal(ctx, &auth.Principal{
		KeyID: "k1", Name: "a-only",
		Workspaces: map[string]struct{}{"ws-a": {}},
	})

	// Unscoped read: exactly its slice.
	entries, err := svc.List(scoped, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].WorkspaceID != "ws-a" {
		t.Fatalf("scoped unscoped read must see only ws-a: %+v", entries)
	}

	// Naming a foreign workspace yields an empty page, same as a missing one.
	entries, err = svc.List(scoped, ListFilter{WorkspaceID: "ws-b"})
	if err != nil || len(entries) != 0 {
		t.Fatalf("foreign workspace must be an empty page: %+v err %v", entries, err)
	}
	// Its own workspace works.
	if entries, err = svc.List(scoped, ListFilter{WorkspaceID: "ws-a"}); err != nil || len(entries) != 1 {
		t.Fatalf("own workspace read: %+v err %v", entries, err)
	}

	// Root sees everything, server-level included.
	rootCtx := auth.WithPrincipal(ctx, &auth.Principal{Root: true})
	if entries, err := svc.List(rootCtx, ListFilter{}); err != nil || len(entries) != 3 {
		t.Fatalf("root read: %d entries err %v", len(entries), err)
	}
}
