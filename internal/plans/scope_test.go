package plans

import (
	"strings"
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

	// By-id operations on foreign plans are uniform not-found — same error
	// text as a nonexistent id (no existence/ownership oracle).
	if _, err := svc.GetPlan(scopedCtx(), other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get foreign plan: %v", err)
	}
	if _, err := svc.AddItem(scopedCtx(), other.ID, CreateItemInput{Title: "i"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("add item to foreign plan: %v", err)
	}
	_, foreignErr := svc.GetPlan(scopedCtx(), other.ID)
	_, unknownErr := svc.GetPlan(scopedCtx(), "no-such-plan")
	if strings.Replace(foreignErr.Error(), other.ID, "X", 1) != strings.Replace(unknownErr.Error(), "no-such-plan", "X", 1) {
		t.Fatalf("foreign/unknown plan errors differ beyond the id: %q vs %q", foreignErr, unknownErr)
	}
	if _, err := svc.UpdateItem(scopedCtx(), other.ID, "any", UpdateItemInput{Status: ItemDone}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update item on foreign plan: %v", err)
	}

	// Plan-level dependencies cannot reference foreign-workspace plans
	// (fourth pentest round, vuln-0002).
	foreignDep, err := svc.CreatePlan(ctx, CreatePlanInput{Name: "dep-foreign", AuthorAgentID: "a", WorkspaceID: "ws-b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AddPlanDependency(scopedCtx(), own.ID, foreignDep.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign plan dependency must be rejected uniformly: %v", err)
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
