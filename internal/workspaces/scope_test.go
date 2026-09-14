package workspaces

import (
	"context"
	"errors"
	"testing"

	"github.com/martinsuchenak/skopos/internal/auth"
)

func TestWorkspacesRootOnlyMutations(t *testing.T) {
	svc := NewService(testStorage(t))
	scoped := auth.WithPrincipal(context.Background(), &auth.Principal{
		KeyID: "k1", Name: "ci", Workspaces: map[string]struct{}{"ws-a": {}},
	})

	if _, _, err := svc.Create(scoped, CreateInput{ID: "ws-new"}); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("scoped create: %v", err)
	}
	if err := svc.Delete(scoped, "ws-a"); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("scoped delete: %v", err)
	}
	// System callers (background context) keep registering workspaces —
	// the session-derived registrar depends on it.
	if _, _, err := svc.Create(context.Background(), CreateInput{ID: "ws-registrar"}); err != nil {
		t.Fatalf("system create: %v", err)
	}
	// The scoped key's own workspace must be registered to appear in its list.
	if _, _, err := svc.Create(context.Background(), CreateInput{ID: "ws-a"}); err != nil {
		t.Fatal(err)
	}
	// Root lists everything; a scoped key lists only its slice.
	if _, _, err := svc.Create(context.Background(), CreateInput{ID: "ws-b"}); err != nil {
		t.Fatal(err)
	}
	all, err := svc.List(context.Background())
	if err != nil || len(all) != 3 {
		t.Fatalf("root list: %+v %v", all, err)
	}
	filtered, err := svc.List(scoped)
	if err != nil || len(filtered) != 1 || filtered[0].ID != "ws-a" {
		t.Fatalf("scoped list: %+v %v", filtered, err)
	}
}
