package inbox

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
		t.Fatalf("enable foreign keys: %v", err)
	}
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewStorage(sqlDB)
}

func testItem(id, ws, title string) Item {
	now := time.Now().UTC()
	return Item{ID: id, WorkspaceID: ws, Title: title, Tags: []string{}, Status: StatusOpen, AuthorAgentID: "author", CreatedAt: now, UpdatedAt: now}
}

func TestStorageItemCRUD(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()

	item := testItem("i1", "ws-a", "First")
	item.Tags = []string{"auth", "refactor"}
	if err := s.CreateItem(ctx, item); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetItem(ctx, "i1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "First" || got.WorkspaceID != "ws-a" || got.Status != StatusOpen {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "auth" || got.Tags[1] != "refactor" {
		t.Fatalf("tags mismatch: %v", got.Tags)
	}
	if got.Plan != nil {
		t.Fatalf("unconverted item should have no plan summary: %+v", got.Plan)
	}

	ws, err := s.ItemWorkspace(ctx, "i1")
	if err != nil || ws != "ws-a" {
		t.Fatalf("ItemWorkspace: %q %v", ws, err)
	}
	if _, err := s.ItemWorkspace(ctx, "missing"); err == nil {
		t.Fatal("expected not-found for missing item workspace")
	}

	if err := s.UpdateItem(ctx, "i1", "ws-a", "First!", "enriched", `["x"]`, nil, time.Now().UTC()); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetItem(ctx, "i1")
	if got.Title != "First!" || got.Content != "enriched" || len(got.Tags) != 1 || got.Tags[0] != "x" {
		t.Fatalf("update mismatch: %+v", got)
	}

	if err := s.DeleteItem(ctx, "i1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteItem(ctx, "i1"); err == nil {
		t.Fatal("second delete should be not-found")
	}
}

func TestStorageListFilters(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()

	a := testItem("a", "ws-a", "Alpha auth")
	a.Tags = []string{"auth"}
	b := testItem("b", "ws-a", "Beta")
	b.Tags = []string{"auth", "ui"}
	c := testItem("c", "ws-b", "Gamma auth")
	for _, it := range []Item{a, b, c} {
		if err := s.CreateItem(ctx, it); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	if err := s.SetStatus(ctx, "b", StatusDiscarded, time.Now().UTC()); err != nil {
		t.Fatalf("discard b: %v", err)
	}

	all, err := s.ListItems(ctx, "ws-a", "", "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("ws-a list: %v %d", err, len(all))
	}
	// created_at DESC ordering: newest first (ids are time-sortable, b created after a).
	if all[0].ID != "b" {
		t.Fatalf("expected DESC order, got %v then %v", all[0].ID, all[1].ID)
	}

	openOnly, _ := s.ListItems(ctx, "ws-a", string(StatusOpen), "", "")
	if len(openOnly) != 1 || openOnly[0].ID != "a" {
		t.Fatalf("status filter: %+v", openOnly)
	}

	tagged, _ := s.ListItems(ctx, "ws-a", "", "auth", "")
	if len(tagged) != 2 {
		t.Fatalf("tag filter should match a and b (discarded included): %+v", tagged)
	}
	uiTagged, _ := s.ListItems(ctx, "ws-a", "", "ui", "")
	if len(uiTagged) != 1 || uiTagged[0].ID != "b" {
		t.Fatalf("tag ui: %+v", uiTagged)
	}

	// LIKE wildcards in the tag pattern must not widen the match.
	none, _ := s.ListItems(ctx, "ws-a", "", "a%", "")
	if len(none) != 0 {
		t.Fatalf("wildcard tag should not match: %+v", none)
	}

	q, _ := s.ListItems(ctx, "", "", "", "gamma")
	if len(q) != 1 || q[0].ID != "c" {
		t.Fatalf("query across workspaces: %+v", q)
	}

	// Same-prefix tag safety: "auth" must not match a hypothetical "oauth2"
	// because the JSON delimiters pin the token boundary.
	d := testItem("d", "ws-a", "Delta")
	d.Tags = []string{"oauth2"}
	if err := s.CreateItem(ctx, d); err != nil {
		t.Fatal(err)
	}
	authOnly, _ := s.ListItems(ctx, "ws-a", "", "auth", "")
	if len(authOnly) != 2 {
		t.Fatalf("tag auth should not match oauth2: %+v", authOnly)
	}
}

func TestStorageLifecycleTransitions(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.CreateItem(ctx, testItem("i1", "ws-a", "T")); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimItem(ctx, "i1", "agent-1", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Claiming again fails as a conflict (guarded by status=open).
	if err := s.ClaimItem(ctx, "i1", "agent-2", now); err != ErrClaimConflict {
		t.Fatalf("second claim: %v", err)
	}
	if err := s.ReleaseItem(ctx, "i1", now); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, _ := s.GetItem(ctx, "i1")
	if got.Status != StatusOpen || got.ClaimedByAgentID != "" {
		t.Fatalf("released item should be open+unclaimed: %+v", got)
	}

	// Convert links a plan and hydrates its summary on read.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO plans (id, name, workspace_id, description, status, author_agent_id, created_at, updated_at) VALUES ('p1', 'Plan', 'ws-a', '', 'active', 'a', ?, ?)`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := s.ConvertItem(ctx, "i1", "p1", now); err != nil {
		t.Fatalf("convert: %v", err)
	}
	got, _ = s.GetItem(ctx, "i1")
	if got.Status != StatusConverted || got.PlanID != "p1" || got.Plan == nil || got.Plan.Name != "Plan" || got.Plan.Status != "active" {
		t.Fatalf("converted item: %+v (%+v)", got, got.Plan)
	}

	// CompleteForPlan flips only converted items linked to that plan.
	if err := s.CreateItem(ctx, testItem("i2", "ws-a", "T2")); err != nil {
		t.Fatal(err)
	}
	n, err := s.CompleteForPlan(ctx, "p1", now)
	if err != nil || n != 1 {
		t.Fatalf("CompleteForPlan: %d %v", n, err)
	}
	got, _ = s.GetItem(ctx, "i1")
	if got.Status != StatusDone {
		t.Fatalf("item should be done: %s", got.Status)
	}
	n, _ = s.CompleteForPlan(ctx, "p1", now)
	if n != 0 {
		t.Fatalf("second CompleteForPlan should be a no-op, got %d", n)
	}
}

func TestStorageDanglingPlanSummary(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateItem(ctx, testItem("i1", "ws-a", "T")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO plans (id, name, workspace_id, description, status, author_agent_id, created_at, updated_at) VALUES ('p1', 'Plan', 'ws-a', '', 'active', 'a', ?, ?)`, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := s.ConvertItem(ctx, "i1", "p1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM plans WHERE id = 'p1'`); err != nil {
		t.Fatal(err)
	}
	// Dangling plan_id: the row survives and reads carry no summary.
	got, err := s.GetItem(ctx, "i1")
	if err != nil {
		t.Fatalf("get with dangling plan: %v", err)
	}
	if got.Status != StatusConverted || got.PlanID != "p1" {
		t.Fatalf("item should still be converted with plan_id: %+v", got)
	}
	if got.Plan != nil {
		t.Fatalf("deleted plan should not hydrate: %+v", got.Plan)
	}
}

func TestParseTagsDegradesOnMalformedRow(t *testing.T) {
	if tags := parseTags(`{not json`); len(tags) != 0 {
		t.Fatalf("malformed tags should degrade to empty, got %v", tags)
	}
	if tags := parseTags(`null`); len(tags) != 0 {
		t.Fatalf("null tags should degrade to empty, got %v", tags)
	}
}
