package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/events"
)

type capturePublisher struct{ got []events.Event }

func (c *capturePublisher) Publish(e events.Event) { c.got = append(c.got, e) }

func TestServiceCompleteTransitions(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	open := mustCreate(t, svc, ctx, "done without a plan")
	if err := svc.Complete(ctx, open.ID); err != nil {
		t.Fatalf("complete open: %v", err)
	}
	got, err := svc.GetItem(ctx, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone {
		t.Fatalf("expected done, got %s", got.Status)
	}
	// Done is terminal — a second complete is a client error.
	if err := svc.Complete(ctx, open.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("re-complete should be invalid: %v", err)
	}

	// in_progress completes too (claim then complete).
	ip := mustCreate(t, svc, ctx, "claimed but finished")
	if _, err := svc.Claim(ctx, ip.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(ctx, ip.ID); err != nil {
		t.Fatalf("complete in_progress: %v", err)
	}

	// Discarded items must be restored first.
	d := mustCreate(t, svc, ctx, "discarded")
	if err := svc.Discard(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(ctx, d.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("complete discarded should be invalid: %v", err)
	}

	// Converted items can complete early; the later plan-completion flip
	// targets status='converted' and no-ops (pinned in coupling_test).
	c := mustCreate(t, svc, ctx, "converted early")
	seedPlan(t, svc, "p1", "ws-a", "Plan")
	if _, err := svc.Convert(ctx, c.ID, ConvertInput{PlanID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(ctx, c.ID); err != nil {
		t.Fatalf("complete converted: %v", err)
	}

	// Scope: a foreign item is indistinguishable from a missing one.
	foreign, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-b", Title: "foreign-ws", AuthorAgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(scopedCtx(), foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected uniform not-found for foreign item, got %v", err)
	}
}

func TestServiceReopen(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	// Only done items reopen; the fresh cycle clears claim, plan, priority.
	item := mustCreate(t, svc, ctx, "reopened")
	if _, err := svc.Claim(ctx, item.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	seedPlan(t, svc, "p9", "ws-a", "Plan")
	if _, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "p9"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reopen(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOpen || got.ClaimedByAgentID != "" || got.PlanID != "" || got.Priority != nil {
		t.Fatalf("reopen must reset the cycle: %+v", got)
	}
	// After the reset the item is convertible again.
	if _, err := svc.Convert(ctx, item.ID, ConvertInput{PlanID: "p9"}); err != nil {
		t.Fatalf("re-convert after reopen: %v", err)
	}

	// Non-done items are a client error naming the state in human terms.
	open := mustCreate(t, svc, ctx, "still open")
	err = svc.Reopen(ctx, open.ID)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
	if !strings.Contains(err.Error(), "item is open") {
		t.Fatalf("message should name the state: %v", err)
	}
	ip := mustCreate(t, svc, ctx, "claimed")
	if _, err := svc.Claim(ctx, ip.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	err = svc.Reopen(ctx, ip.ID)
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "item is in progress") {
		t.Fatalf("expected human label in message, got %v", err)
	}

	// Scope: foreign items are indistinguishable from missing ones.
	foreign, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-b", Title: "f", AuthorAgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(ctx, foreign.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Reopen(scopedCtx(), foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected uniform not-found, got %v", err)
	}
}

func TestStatusLabelsInMessages(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	item := mustCreate(t, svc, ctx, "frozen")
	if _, err := svc.Claim(ctx, item.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Discard(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	// Editing a frozen item reports the state in human terms, not the slug.
	err := svc.UpdateItem(ctx, item.ID, UpdateInput{Title: "x"})
	if !errors.Is(err, ErrFrozen) || !strings.Contains(err.Error(), "item is discarded") {
		t.Fatalf("unexpected frozen message: %v", err)
	}
	claimed := mustCreate(t, svc, ctx, "conflict")
	if _, err := svc.Claim(ctx, claimed.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Claim(ctx, claimed.ID, "agent-2")
	if !errors.Is(err, ErrClaimConflict) || !strings.Contains(err.Error(), "item is in progress (claimed by") {
		t.Fatalf("expected human label in claim conflict, got %v", err)
	}
}

func TestServiceCompletePublishes(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	item := mustCreate(t, svc, ctx, "publishes")
	pub := &capturePublisher{}
	svc.SetPublisher(pub)
	if err := svc.Complete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	if len(pub.got) != 1 || pub.got[0].Type != events.TypeInbox || pub.got[0].Workspace != "ws-a" {
		t.Fatalf("expected one attributed inbox event, got %+v", pub.got)
	}
}

func TestStorageDeleteByFilter(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Two done + one open in ws-a; one done in ws-b; one unfiled done.
	rows := []struct{ id, ws, status string }{
		{"a1", "ws-a", "done"}, {"a2", "ws-a", "done"}, {"a3", "ws-a", "open"},
		{"b1", "ws-b", "done"},
	}
	for _, r := range rows {
		if err := s.CreateItem(ctx, Item{ID: r.id, WorkspaceID: r.ws, Status: Status(r.status), Title: r.id, AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateItem(ctx, Item{ID: "u1", WorkspaceID: "", Status: StatusDone, Title: "u1", AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	n, err := s.DeleteByFilter(ctx, "ws-a", StatusDone)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 done items purged in ws-a, got %d", n)
	}
	remaining := map[string]bool{}
	items, err := s.ListItems(ctx, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		remaining[it.ID] = true
	}
	if !remaining["a3"] || !remaining["b1"] || !remaining["u1"] || len(remaining) != 3 {
		t.Fatalf("purge touched the wrong rows: %v", remaining)
	}

	// Status-less purge takes every status in the workspace, unfiled never matches.
	n, err = s.DeleteByFilter(ctx, "ws-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected the remaining ws-a item purged, got %d", n)
	}
	items, _ = s.ListItems(ctx, "", "", "", "")
	if len(items) != 2 {
		t.Fatalf("expected b1 + unfiled u1 left, got %d", len(items))
	}
}

func TestServicePurgeValidationAndScope(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()
	scoped := auth.WithPrincipal(ctx, &auth.Principal{KeyID: "k1", Workspaces: map[string]struct{}{"ws-a": {}}})
	mustCreate(t, svc, scoped, "one")

	if _, err := svc.Purge(ctx, "", StatusDone); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing workspace: %v", err)
	}
	if _, err := svc.Purge(ctx, "ws-a", Status("paused")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid status: %v", err)
	}
	if _, err := svc.Purge(scoped, "ws-b", ""); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("foreign workspace: %v", err)
	}
	n, err := svc.Purge(scoped, " ws-a ", StatusDone)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("open item should not match done purge, got %d", n)
	}
	// Root may purge every status in a workspace.
	n, err = svc.Purge(ctx, "ws-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected the open item purged, got %d", n)
	}
}

func TestHandlerCompleteAndPurge(t *testing.T) {
	svc := testService(t)
	h := NewHandler(svc, testAuth("k"))
	ctx := context.Background()
	item := mustCreate(t, svc, ctx, "handler item")

	// Complete via handler → 204.
	r := httptest.NewRequest(http.MethodPost, "/api/inbox/"+item.ID+"/complete", nil)
	r.Header.Set("Authorization", "Bearer k")
	r.SetPathValue("id", item.ID)
	w := httptest.NewRecorder()
	h.Complete(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Purge: missing workspace → 400; ok → 200 with count.
	r = httptest.NewRequest(http.MethodDelete, "/api/inbox", nil)
	r.Header.Set("Authorization", "Bearer k")
	w = httptest.NewRecorder()
	h.Purge(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing workspace, got %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodDelete, "/api/inbox?workspace_id=ws-a&status=done", nil)
	r.Header.Set("Authorization", "Bearer k")
	w = httptest.NewRecorder()
	h.Purge(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Deleted int `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Deleted != 1 {
		t.Fatalf("expected 1 deleted, got %d", out.Deleted)
	}
}
