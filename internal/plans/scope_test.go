package plans

import (
	"context"
	"errors"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

func scopedCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k1", Name: "ci", Workspaces: map[string]struct{}{"ws-a": {}},
	})
}

func TestPlansWorkspaceScopeMatrix(t *testing.T) {
	st := testFileStorage(t)
	svc := NewService(st)
	ctx := context.Background()

	other, err := svc.CreatePlan(ctx, CreatePlanInput{Name: "other", AuthorAgentID: "a", WorkspaceID: "ws-b"})
	if err != nil {
		t.Fatal(err)
	}
	own, err := svc.CreatePlan(ctx, CreatePlanInput{Name: "own", AuthorAgentID: "a", WorkspaceID: "ws-a"})
	if err != nil {
		t.Fatal(err)
	}

	// Writes: creating in a foreign workspace or without one is rejected.
	if _, err := svc.CreatePlan(scopedCtx(), CreatePlanInput{Name: "x", AuthorAgentID: "a", WorkspaceID: "ws-b"}); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("create out of scope: %v", err)
	}
	if _, err := svc.CreatePlan(scopedCtx(), CreatePlanInput{Name: "x", AuthorAgentID: "a"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("create without workspace: %v", err)
	}

	// By-id operations on foreign plans are 404-shaped (ErrOutOfScope), and
	// mutations under a scoped key cannot touch them.
	if _, err := svc.GetPlan(scopedCtx(), other.ID); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("get foreign plan: %v", err)
	}
	if _, err := svc.AddItem(scopedCtx(), other.ID, CreateItemInput{Title: "i"}); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("add item to foreign plan: %v", err)
	}
	if _, err := svc.UpdateItem(scopedCtx(), other.ID, "any", UpdateItemInput{Status: ItemDone}); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("update item on foreign plan: %v", err)
	}

	// Unscoped list returns exactly the key's slice.
	list, err := svc.ListPlans(scopedCtx(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.WorkspaceID != "ws-a" {
			t.Fatalf("unscoped list leaked plan from %q", p.WorkspaceID)
		}
	}
	// Own plan is fully usable.
	if _, err := svc.AddItem(scopedCtx(), own.ID, CreateItemInput{Title: "i"}); err != nil {
		t.Fatalf("add item to own plan: %v", err)
	}
}
