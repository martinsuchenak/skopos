package blackboard

import (
	"context"
	"errors"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

// scopedCtx mimics a request authenticated with a key scoped to ws-a only.
func scopedCtx() context.Context {
	return auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k1", Name: "ci", Workspaces: map[string]struct{}{"ws-a": {}},
	})
}

func TestBlackboardWorkspaceScopeMatrix(t *testing.T) {
	s := testStorage(t)
	svc := NewService(s)
	ctx := context.Background()
	now := timeNow()

	seed := func(id, ws, title string) {
		t.Helper()
		if err := s.Write(ctx, Entry{ID: id, Scope: ScopeProject, WorkspaceID: ws,
			EntryType: TypeFinding, Title: title, AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	seed("e-a", "ws-a", "in scope")
	seed("e-b", "ws-b", "out of scope")

	// Write: member ok, non-member and missing rejected.
	if _, err := svc.Write(scopedCtx(), WriteInput{Scope: ScopeProject, EntryType: TypeFinding,
		Title: "ok", AuthorAgentID: "a", WorkspaceID: "ws-a"}); err != nil {
		t.Fatalf("write in scope: %v", err)
	}
	_, err := svc.Write(scopedCtx(), WriteInput{Scope: ScopeProject, EntryType: TypeFinding,
		Title: "nope", AuthorAgentID: "a", WorkspaceID: "ws-b"})
	if !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("write out of scope: expected ErrOutOfScope, got %v", err)
	}
	_, err = svc.Write(scopedCtx(), WriteInput{Scope: ScopeProject, EntryType: TypeFinding,
		Title: "nope", AuthorAgentID: "a"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("write without workspace: expected ErrInvalidInput, got %v", err)
	}
	// Root (nil principal) must still be able to write — system callers.
	if _, err := svc.Write(ctx, WriteInput{Scope: ScopeProject, EntryType: TypeFinding,
		Title: "root", AuthorAgentID: "a", WorkspaceID: "ws-c"}); err != nil {
		t.Fatalf("system/root write: %v", err)
	}

	// Read: explicit out-of-scope rejected; unscoped returns exactly the slice.
	if _, err := svc.Bundle(scopedCtx(), "ws-b", "", ""); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("bundle out of scope: %v", err)
	}
	bundle, err := svc.Bundle(scopedCtx(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range bundle.Entries {
		if e.WorkspaceID != "ws-a" {
			t.Fatalf("unscoped scoped-key read leaked workspace %q", e.WorkspaceID)
		}
	}
	found, err := svc.Search(scopedCtx(), SearchFilters{Query: "scope"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range found {
		if e.WorkspaceID != "ws-a" {
			t.Fatalf("search leaked workspace %q", e.WorkspaceID)
		}
	}

	// By-id: out-of-scope is indistinguishable from unknown (404 semantics).
	if err := svc.Delete(scopedCtx(), "e-b"); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("delete out of scope: %v", err)
	}
	if err := svc.Promote(scopedCtx(), "e-b"); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("promote out of scope: %v", err)
	}
	// Root still reaches everything.
	if _, err := svc.Bundle(ctx, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, "e-b"); err != nil {
		t.Fatalf("root delete: %v", err)
	}
}
