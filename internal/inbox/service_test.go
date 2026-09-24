package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/events"
)

func testService(t *testing.T) *Service {
	t.Helper()
	return NewService(testStorage(t))
}

func mustCreate(t *testing.T, svc *Service, ctx context.Context, title string) *Item {
	t.Helper()
	item, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-a", Title: title, Content: "rough notes", AuthorAgentID: "author"})
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return item
}

func TestServiceCreateValidation(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	if _, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-a", AuthorAgentID: "a"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing title: %v", err)
	}
	// Root (background ctx) may capture UNFILED items; scoped keys must
	// name a workspace.
	unfiled, err := svc.CreateItem(ctx, CreateInput{Title: "T", AuthorAgentID: "a"})
	if err != nil || unfiled.WorkspaceID != "" {
		t.Fatalf("root unfiled create: %v %+v", err, unfiled)
	}
	if _, err := svc.CreateItem(scopedCtx(), CreateInput{Title: "T", AuthorAgentID: "a"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("scoped unfiled create should be rejected: %v", err)
	}
	if _, err := svc.CreateItem(ctx, CreateInput{Title: "T", WorkspaceID: "ws-a"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing author: %v", err)
	}
}

func TestServiceNormalizeTags(t *testing.T) {
	in := []string{"  Auth ", "AUTH", "ui/ux", "Bad Tag!", "", "x", strings.Repeat("y", 41), "ok.tag", "ok-tag_v1.2/3", "#hash", "-dash", "a\x00b"}
	got := NormalizeTags(in)
	// "Bad Tag!", "", over-long, "#hash", "-dash", NUL dropped; "  Auth "=="AUTH" deduped; cap is 10 but only valid ones remain.
	want := []string{"auth", "ui/ux", "x", "ok.tag", "ok-tag_v1.2/3"}
	if len(got) != len(want) {
		t.Fatalf("tags: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tags: got %v want %v", got, want)
		}
	}
	if NormalizeTags(nil) == nil || len(NormalizeTags(nil)) != 0 {
		t.Fatal("nil tags should normalize to empty slice, not nil (JSON [])")
	}
}

func TestServiceLifecycle(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	item := mustCreate(t, svc, ctx, "Implement user management")

	// Detail read renders markdown; list rows carry only an excerpt.
	detail, err := svc.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if detail.ContentHTML == "" {
		t.Fatal("detail should render content_html")
	}
	list, err := svc.ListItems(ctx, "ws-a", "", "", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len: %d", len(list))
	}
	if list[0].Content != "" || list[0].ContentHTML != "" || list[0].Excerpt != "rough notes" {
		t.Fatalf("list row should carry excerpt only: %+v", list[0])
	}

	// Enrichment while open.
	content := "rough notes\n\n## Enrichment — agent-1, 2026-09-19\n\n- found X"
	if err := svc.UpdateItem(ctx, item.ID, UpdateInput{Content: content, Tags: &[]string{"auth"}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	detail, _ = svc.GetItem(ctx, item.ID)
	if detail.Content != content || len(detail.Tags) != 1 || detail.Tags[0] != "auth" {
		t.Fatalf("update mismatch: %+v", detail)
	}

	// Claim, conflict, idempotent re-claim, release.
	if _, err := svc.Claim(ctx, item.ID, "agent-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := svc.Claim(ctx, item.ID, "agent-2"); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("foreign claim should conflict: %v", err)
	}
	if _, err := svc.Claim(ctx, item.ID, "agent-1"); err != nil {
		t.Fatalf("same-agent re-claim should be idempotent: %v", err)
	}
	if _, err := svc.Claim(ctx, item.ID, ""); err != nil {
		t.Fatalf("release: %v", err)
	}
	detail, _ = svc.GetItem(ctx, item.ID)
	if detail.Status != StatusOpen {
		t.Fatalf("released item should be open: %s", detail.Status)
	}
}

func TestServiceConvert(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	item := mustCreate(t, svc, ctx, "Idea")

	if _, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "no-such-plan"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("convert to missing plan: %v", err)
	}

	seedPlan(t, svc, "p1", "ws-a", "Plan A")
	seedPlan(t, svc, "p2", "ws-b", "Plan B")

	if _, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "p2"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("cross-workspace plan must be rejected: %v", err)
	}

	got, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "p1"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got.Status != StatusConverted || got.PlanID != "p1" || got.Plan == nil || got.Plan.Name != "Plan A" {
		t.Fatalf("converted: %+v", got)
	}

	if _, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "p1"}); !errors.Is(err, ErrAlreadyConverted) {
		t.Fatalf("double convert: %v", err)
	}

	// Frozen after conversion.
	if err := svc.UpdateItem(ctx, item.ID, UpdateInput{Title: "X"}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("update converted item: %v", err)
	}

	// Completion flips it to done (the plans completion hook path).
	n, err := svc.CompleteForPlan(ctx, "p1")
	if err != nil || n != 1 {
		t.Fatalf("CompleteForPlan: %d %v", n, err)
	}
	got, _ = svc.GetItem(ctx, item.ID)
	if got.Status != StatusDone {
		t.Fatalf("should be done: %s", got.Status)
	}
	if err := svc.Discard(ctx, item.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("done item is terminal: %v", err)
	}
}

func TestServiceDiscardAndDelete(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	item := mustCreate(t, svc, ctx, "Won't do")

	if err := svc.Discard(ctx, item.ID); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if err := svc.Discard(ctx, item.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("discard twice: %v", err)
	}
	if err := svc.UpdateItem(ctx, item.ID, UpdateInput{Title: "X"}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("discarded item frozen: %v", err)
	}
	if _, err := svc.Claim(ctx, item.ID, "agent"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("claim discarded: %v", err)
	}

	other := mustCreate(t, svc, ctx, "Delete me")
	if err := svc.DeleteItem(ctx, other.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetItem(ctx, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
}

func TestServiceListInvalidStatus(t *testing.T) {
	svc := testService(t)
	if _, err := svc.ListItems(context.Background(), "ws-a", "bogus", "", ""); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid status: %v", err)
	}
}

// recorder captures published events for assertions.
type recorder struct{ events []events.Event }

func (r *recorder) Publish(e events.Event) { r.events = append(r.events, e) }

func TestServicePublishesInboxEvents(t *testing.T) {
	svc := testService(t)
	rec := &recorder{}
	svc.SetPublisher(rec)
	ctx := context.Background()

	item := mustCreate(t, svc, ctx, "Ev")
	if len(rec.events) != 1 || rec.events[0].Type != events.TypeInbox || rec.events[0].Workspace != "ws-a" {
		t.Fatalf("create event: %+v", rec.events)
	}
	if _, err := svc.Claim(ctx, item.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	seedPlan(t, svc, "p1", "ws-a", "P")
	if _, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteForPlan(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 4 {
		t.Fatalf("expected create+claim+convert+complete events, got %+v", rec.events)
	}
	// Reads never publish.
	if _, err := svc.ListItems(ctx, "ws-a", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetItem(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if len(rec.events) != 4 {
		t.Fatalf("reads published events: %+v", rec.events)
	}
}

// TestServiceExcerptTruncates pins the single-line, capped preview.
func TestServiceExcerptTruncates(t *testing.T) {
	long := strings.Repeat("word ", 100)
	got := Excerpt(long)
	if len([]rune(got)) != 201 || !strings.HasSuffix(got, "…") {
		t.Fatalf("excerpt should cap at 200 runes + ellipsis: %d", len([]rune(got)))
	}
	if strings.Contains(Excerpt("a\n\nb\tc"), "\n") {
		t.Fatal("excerpt should collapse whitespace")
	}
}

// seedPlan inserts a plan row through the concrete storage's DB handle.
func seedPlan(t *testing.T, svc *Service, id, ws, name string) {
	t.Helper()
	st, ok := svc.store.(*Storage)
	if !ok {
		t.Fatalf("seedPlan needs *Storage, got %T", svc.store)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.db.ExecContext(context.Background(),
		`INSERT INTO plans (id, name, workspace_id, description, status, author_agent_id, created_at, updated_at) VALUES (?, ?, ?, '', 'active', 'seed', ?, ?)`,
		id, name, ws, now, now); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
}

func TestServicePriorityPartialOrder(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	// Three unprioritized items + two prioritized; creation order a,b,c,d,e.
	mustCreate(t, svc, ctx, "a") // unordered, oldest
	mustCreate(t, svc, ctx, "b") // will become priority 2
	mustCreate(t, svc, ctx, "c") // unordered, newest
	mustCreate(t, svc, ctx, "d") // will become priority 1
	mustCreate(t, svc, ctx, "e") // unordered, middle age

	var two = 2
	var one = 1
	var zero = 0
	if err := svc.UpdateItem(ctx, itemIDByTitle(t, svc, "b"), UpdateInput{Priority: &two}); err != nil {
		t.Fatalf("set priority b: %v", err)
	}
	// Create-time priority.
	if _, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-a", Title: "d2", AuthorAgentID: "a", Priority: 1}); err != nil {
		t.Fatalf("create with priority: %v", err)
	}
	_ = one
	_ = zero

	items, err := svc.ListItems(ctx, "ws-a", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, it := range items {
		order = append(order, it.Title)
	}
	// Prioritized first (ascending), unordered tail newest-first. Creation
	// order is a,b,c,d,e(+d2): the tail reads e,d,c,a.
	want := []string{"d2", "b", "e", "d", "c", "a"}
	if len(order) != len(want) {
		t.Fatalf("order %v want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order %v want %v", order, want)
		}
	}

	// Clearing priority drops the item to the unordered tail.
	if err := svc.UpdateItem(ctx, itemIDByTitle(t, svc, "b"), UpdateInput{Priority: &zero}); err != nil {
		t.Fatalf("clear priority b: %v", err)
	}
	items, _ = svc.ListItems(ctx, "ws-a", "", "", "")
	if items[0].Title != "d2" || items[0].Priority == nil || *items[0].Priority != 1 {
		t.Fatalf("d2 should lead with priority 1: %+v", items[0])
	}
	if items[1].Title != "e" {
		t.Fatalf("cleared b should be in the unordered tail: %v", titles(items))
	}

	// Invalid values rejected; frozen statuses reject priority edits.
	var neg = -3
	if err := svc.UpdateItem(ctx, itemIDByTitle(t, svc, "a"), UpdateInput{Priority: &neg}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative priority: %v", err)
	}
	seedPlan(t, svc, "p1", "ws-a", "P")
	conv := mustCreate(t, svc, ctx, "conv")
	if _, err := svc.Claim(ctx, conv.ID, "ag"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Convert(ctx, conv.ID, ConvertInput{PlanID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateItem(ctx, conv.ID, UpdateInput{Priority: &one}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("priority on converted item must be frozen: %v", err)
	}
}

func TestServiceReorder(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a := mustCreate(t, svc, ctx, "ra")
	b := mustCreate(t, svc, ctx, "rb")
	c := mustCreate(t, svc, ctx, "rc")

	// Empty and duplicate ids rejected.
	if err := svc.Reorder(ctx, ReorderInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty reorder: %v", err)
	}
	if err := svc.Reorder(ctx, ReorderInput{IDs: []string{a.ID, a.ID}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate ids: %v", err)
	}
	if err := svc.Reorder(ctx, ReorderInput{IDs: []string{"no-such"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: %v", err)
	}

	// c,a,b -> priorities 1,2,3.
	if err := svc.Reorder(ctx, ReorderInput{IDs: []string{c.ID, a.ID, b.ID}}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	items, _ := svc.ListItems(ctx, "ws-a", "", "", "")
	want := []string{"rc", "ra", "rb"}
	for i, it := range items {
		if it.Title != want[i] || it.Priority == nil || *it.Priority != i+1 {
			t.Fatalf("position %d: %+v want %s#%d", i, it, want[i], i+1)
		}
	}

	// Non-editable statuses cannot be reordered.
	seedPlan(t, svc, "p1", "ws-a", "P")
	if _, err := svc.Claim(ctx, c.ID, "ag"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Convert(ctx, c.ID, ConvertInput{PlanID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reorder(ctx, ReorderInput{IDs: []string{c.ID}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("reorder converted: %v", err)
	}

	// Foreign-workspace items are uniform not-found (scope-quiet inside Reorder).
	foreign, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-b", Title: "foreign", AuthorAgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	scoped := scopedCtx()
	if err := svc.Reorder(scoped, ReorderInput{IDs: []string{foreign.ID}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reorder foreign item: %v", err)
	}
}

// TestServiceResolveByPriority pins the by-number item reference: a
// workspace + priority pair names the open/in-progress item holding that
// number, stale numbers on frozen items never collide with it, and the
// workspace keeps explicit-target scope semantics (actionable 403, not the
// by-id uniform 404).
func TestServiceResolveByPriority(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	a := mustCreate(t, svc, ctx, "ra")
	b := mustCreate(t, svc, ctx, "rb")
	c := mustCreate(t, svc, ctx, "rc")
	if err := svc.Reorder(ctx, ReorderInput{IDs: []string{a.ID, b.ID, c.ID}}); err != nil {
		t.Fatal(err)
	}

	// Validation: workspace required, priority bounded.
	if _, err := svc.ResolveByPriority(ctx, "", 1); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing workspace: %v", err)
	}
	if _, err := svc.ResolveByPriority(ctx, "ws-a", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("priority 0: %v", err)
	}
	if _, err := svc.ResolveByPriority(ctx, "ws-a", 100001); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("priority cap: %v", err)
	}

	// #2 names rb.
	item, err := svc.ResolveByPriority(ctx, "ws-a", 2)
	if err != nil || item.ID != b.ID {
		t.Fatalf("resolve #2: %v %+v", err, item)
	}
	// Numbers the board does not have are not-found with guidance.
	if _, err := svc.ResolveByPriority(ctx, "ws-a", 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resolve #9: %v", err)
	}

	// Explicit-target scoping: a foreign workspace is the actionable 403
	// flavor, and an in-scope workspace resolves.
	foreign, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-x", Title: "foreign", AuthorAgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.Reorder(ctx, ReorderInput{IDs: []string{foreign.ID}})
	if _, err := svc.ResolveByPriority(scopedCtx(), "ws-x", 1); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("scoped foreign workspace: %v", err)
	}
	if item, err := svc.ResolveByPriority(scopedCtx(), "ws-a", 1); err != nil || item.ID != a.ID {
		t.Fatalf("scoped own workspace: %v %+v", err, item)
	}

	// Frozen rows keep stale numbers; resolution must skip them. Complete b
	// (done rows keep their priority), then renumber the live set so c takes
	// #2 — resolving #2 now names the LIVE item, never the done one.
	if err := svc.Complete(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reorder(ctx, ReorderInput{IDs: []string{a.ID, c.ID}}); err != nil {
		t.Fatal(err)
	}
	item, err = svc.ResolveByPriority(ctx, "ws-a", 2)
	if err != nil || item.ID != c.ID {
		t.Fatalf("resolve #2 after freeze: %v %+v", err, item)
	}
}

func itemIDByTitle(t *testing.T, svc *Service, title string) string {
	t.Helper()
	items, err := svc.ListItems(context.Background(), "ws-a", "", "", title)
	if err != nil || len(items) != 1 {
		t.Fatalf("lookup %q: %v %d", title, err, len(items))
	}
	return items[0].ID
}

func titles(items []Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Title)
	}
	return out
}


func TestServiceFileUnfiled(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	item, err := svc.CreateItem(ctx, CreateInput{Title: "undecided", AuthorAgentID: "u"})
	if err != nil {
		t.Fatalf("unfiled create: %v", err)
	}
	if item.WorkspaceID != "" {
		t.Fatalf("expected unfiled item, got %q", item.WorkspaceID)
	}

	// Root files the unfiled item (scoped keys cannot even see unfiled
	// items — that is the visibility contract).
	if err := svc.UpdateItem(ctx, item.ID, UpdateInput{WorkspaceID: "ws-a"}); err != nil {
		t.Fatalf("root file: %v", err)
	}
	filed, err := svc.GetItem(ctx, item.ID)
	if err != nil || filed.WorkspaceID != "ws-a" {
		t.Fatalf("filed item: %v %+v", err, filed)
	}
	// Now visible to the scoped key; re-filing to a FOREIGN workspace is
	// the actionable 403 flavor (explicit target).
	if _, err := svc.GetItem(scopedCtx(), item.ID); err != nil {
		t.Fatalf("scoped should see the filed item: %v", err)
	}
	if err := svc.UpdateItem(scopedCtx(), item.ID, UpdateInput{WorkspaceID: "ws-b"}); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("scoped re-file to foreign ws: %v", err)
	}
	if items, _ := svc.ListItems(ctx, "ws-a", "", "", ""); len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("filed item should appear in ws-a: %+v", items)
	}
	// Re-filing while open is allowed; frozen after conversion.
	if err := svc.UpdateItem(ctx, item.ID, UpdateInput{WorkspaceID: "ws-b"}); err != nil {
		t.Fatalf("re-file: %v", err)
	}
	seedPlan(t, svc, "p1", "ws-b", "P")
	if _, err := svc.Claim(ctx, item.ID, "ag"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateItem(ctx, item.ID, UpdateInput{WorkspaceID: "ws-a"}); !errors.Is(err, ErrFrozen) {
		t.Fatalf("filing after conversion must be frozen: %v", err)
	}

	// Converting an UNFILED item cannot match any plan's workspace — 400.
	other, err := svc.CreateItem(ctx, CreateInput{Title: "never filed", AuthorAgentID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Convert(ctx, other.ID, ConvertInput{PlanID: "p1"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("convert unfiled should mismatch any plan workspace: %v", err)
	}
}

func TestServiceRestore(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	item := mustCreate(t, svc, ctx, "regret")

	// Restoring a non-discarded item is invalid.
	if err := svc.Restore(ctx, item.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("restore open item: %v", err)
	}

	if _, err := svc.Claim(ctx, item.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Discard(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	// A discarded claimed item restores to open with the stale claim cleared.
	if err := svc.Restore(ctx, item.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := svc.GetItem(ctx, item.ID)
	if err != nil || got.Status != StatusOpen || got.ClaimedByAgentID != "" {
		t.Fatalf("restored item should be open+unclaimed: %+v (%v)", got, err)
	}

	// Done is terminal — restore rejected.
	seedPlan(t, svc, "p1", "ws-a", "P")
	if _, err := svc.Claim(ctx, item.ID, "ag"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteForPlan(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Restore(ctx, item.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("restore done item: %v", err)
	}
}
