package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

func scopedCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k1", Name: "ci", Workspaces: map[string]struct{}{"ws-a": {}},
	})
}

// TestInboxWorkspaceScopeMatrix mirrors the plans matrix: writes require an
// in-scope workspace for every principal, by-id operations on foreign items
// are uniform not-found (no existence oracle), and unscoped reads for scoped
// keys return exactly the key's slice.
func TestInboxWorkspaceScopeMatrix(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	foreign, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-b", Title: "foreign", AuthorAgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	own, err := svc.CreateItem(ctx, CreateInput{WorkspaceID: "ws-a", Title: "own", AuthorAgentID: "a"})
	if err != nil {
		t.Fatal(err)
	}

	// Writes: creating in a foreign workspace or without one is rejected.
	if _, err := svc.CreateItem(scopedCtx(), CreateInput{Title: "x", AuthorAgentID: "a", WorkspaceID: "ws-b"}); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("create out of scope: %v", err)
	}
	if _, err := svc.CreateItem(scopedCtx(), CreateInput{Title: "x", AuthorAgentID: "a"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("create without workspace: %v", err)
	}

	// By-id operations on foreign items are uniform not-found — same error
	// text as a nonexistent id (no existence/ownership oracle).
	if _, err := svc.GetItem(scopedCtx(), foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get foreign item: %v", err)
	}
	if err := svc.UpdateItem(scopedCtx(), foreign.ID, UpdateInput{Title: "X"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update foreign item: %v", err)
	}
	if _, err := svc.Claim(scopedCtx(), foreign.ID, "agent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim foreign item: %v", err)
	}
	if _, err := svc.Convert(scopedCtx(), foreign.ID, ConvertInput{PlanID: "p"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("convert foreign item: %v", err)
	}
	if err := svc.Discard(scopedCtx(), foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("discard foreign item: %v", err)
	}
	if err := svc.DeleteItem(scopedCtx(), foreign.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete foreign item: %v", err)
	}
	_, foreignErr := svc.GetItem(scopedCtx(), foreign.ID)
	_, unknownErr := svc.GetItem(scopedCtx(), "no-such-item")
	if strings.Replace(foreignErr.Error(), foreign.ID, "X", 1) != strings.Replace(unknownErr.Error(), "no-such-item", "X", 1) {
		t.Fatalf("foreign/unknown errors differ beyond the id: %q vs %q", foreignErr, unknownErr)
	}

	// Unscoped read for a scoped key returns exactly its slice.
	items, err := svc.ListItems(scopedCtx(), "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != own.ID {
		t.Fatalf("scoped unscoped-read should see only ws-a: %+v", items)
	}

	// Explicit foreign workspace read is actionable 403 material (the handler
	// maps ErrOutOfScope), and the error lists the accessible workspaces.
	if _, err := svc.ListItems(scopedCtx(), "ws-b", "", "", ""); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("list foreign workspace: %v", err)
	}

	// Root sees everything, including the foreign workspace.
	rootItems, err := svc.ListItems(ctx, "ws-b", "", "", "")
	if err != nil || len(rootItems) != 1 {
		t.Fatalf("root list ws-b: %v %d", err, len(rootItems))
	}

	// Convert cannot smuggle a foreign-workspace plan into an own item —
	// and a foreign plan is indistinguishable from a missing one (no
	// existence/location oracle across tenants).
	seedPlan(t, svc, "p-b", "ws-b", "Foreign Plan")
	_, foreignPlanErr := svc.Convert(scopedCtx(), own.ID, ConvertInput{PlanID: "p-b"})
	if !errors.Is(foreignPlanErr, ErrNotFound) {
		t.Fatalf("cross-workspace convert should be uniform not-found: %v", foreignPlanErr)
	}
	_, missingPlanErr := svc.Convert(scopedCtx(), own.ID, ConvertInput{PlanID: "no-such-plan"})
	if strings.Replace(foreignPlanErr.Error(), "p-b", "X", 1) != strings.Replace(missingPlanErr.Error(), "no-such-plan", "X", 1) {
		t.Fatalf("foreign/unknown plan errors differ beyond the id: %q vs %q", foreignPlanErr, missingPlanErr)
	}
	// Root still gets the actionable mismatch error for a same-tenancy-view
	// plan in another workspace (both workspaces are visible to root).
	if _, err := svc.Convert(ctx, own.ID, ConvertInput{PlanID: "p-b"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("root cross-workspace convert should be a 400 mismatch: %v", err)
	}

	// Unfiled items: visible to root in unfiltered reads, hidden from every
	// scoped read and by-id access (uniform not-found); never leaked into a
	// concrete-workspace read.
	unfiled, err := svc.CreateItem(ctx, CreateInput{Title: "unfiled idea", AuthorAgentID: "u"})
	if err != nil {
		t.Fatalf("root unfiled create: %v", err)
	}
	allRoot, err := svc.ListItems(ctx, "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range allRoot {
		if it.ID == unfiled.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("root unfiltered read must include unfiled items")
	}
	scopedAll, err := svc.ListItems(scopedCtx(), "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range scopedAll {
		if it.ID == unfiled.ID {
			t.Fatal("scoped unfiltered read must not include unfiled items")
		}
	}
	if _, err := svc.GetItem(scopedCtx(), unfiled.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scoped get unfiled: %v", err)
	}
	if items, _ := svc.ListItems(ctx, "ws-a", "", "", ""); len(items) != 1 {
		t.Fatalf("ws-a read should see only its own item, got %d", len(items))
	}

	// Background/system principal (nil) passes — the completion hook path.
	if _, err := svc.CreateItem(auth.WithPrincipal(ctx, nil), CreateInput{Title: "sys", AuthorAgentID: "sys", WorkspaceID: "ws-b"}); err != nil {
		t.Fatalf("system principal create: %v", err)
	}
}
