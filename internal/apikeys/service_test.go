package apikeys

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func seedWorkspace(t *testing.T, s *Storage, id string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, git_url, created_at) VALUES (?, '', '', ?)`, id, formatTime(timeNowUTC())); err != nil {
		t.Fatal(err)
	}
}

func TestServiceCreateValidation(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")

	for _, tc := range []struct {
		name  string
		input CreateInput
	}{
		{"missing name", CreateInput{Workspaces: []string{"ws-a"}}},
		{"no scope", CreateInput{Name: "x"}},
		{"star mixed with list", CreateInput{Name: "x", Workspaces: []string{"*", "ws-a"}}},
		{"unknown workspace", CreateInput{Name: "x", Workspaces: []string{"nope"}}},
		{"name too long", CreateInput{Name: strings.Repeat("n", 101), Workspaces: []string{"ws-a"}}},
	} {
		if _, err := svc.Create(ctx, tc.input); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: expected ErrInvalidInput, got %v", tc.name, err)
		}
	}
}

func TestServiceCreateAndLookup(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")
	seedWorkspace(t, st, "ws-b")

	// "*" scope, deduped duplicates tolerated.
	result, err := svc.Create(ctx, CreateInput{Name: "all-key", Workspaces: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Secret, "sk_") || len(result.Secret) != 46 {
		t.Fatalf("unexpected secret shape: %q", result.Secret)
	}
	if result.Key.Prefix != result.Secret[:14] {
		t.Fatalf("prefix must be the secret head, got %q", result.Key.Prefix)
	}
	if !result.Key.AllWorkspaces {
		t.Fatal("expected all-workspaces scope")
	}

	scoped, err := svc.Create(ctx, CreateInput{Name: "ci", Workspaces: []string{"ws-a", "ws-a", "ws-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.Key.Workspaces) != 2 {
		t.Fatalf("duplicates must be deduped, got %v", scoped.Key.Workspaces)
	}

	// Both keys authenticate via the storage lookup path.
	info, err := st.LookupKey(ctx, hashOf(t, scoped.Secret))
	if err != nil || info == nil || info.ID != scoped.Key.ID || len(info.Workspaces) != 2 {
		t.Fatalf("lookup mismatch: %+v err %v", info, err)
	}
}

func TestServiceRevokeLifecycle(t *testing.T) {
	st := testStorage(t)
	svc := NewService(st)
	ctx := context.Background()
	seedWorkspace(t, st, "ws-a")

	result, err := svc.Create(ctx, CreateInput{Name: "temp", Workspaces: []string{"ws-a"}})
	if err != nil {
		t.Fatal(err)
	}

	keys, err := svc.List(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("list: %v %+v", err, keys)
	}

	if err := svc.Revoke(ctx, result.Key.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Revoked key no longer authenticates.
	if info, _ := st.LookupKey(ctx, hashOf(t, result.Secret)); info != nil {
		t.Fatal("revoked key must not resolve")
	}
	// Idempotent second revoke; unknown id -> ErrNotFound.
	if err := svc.Revoke(ctx, result.Key.ID); err != nil {
		t.Fatalf("second revoke must be a no-op: %v", err)
	}
	if err := svc.Revoke(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// List still shows the revoked row (audit).
	keys, _ = svc.List(ctx)
	if len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("revoked key must remain listed: %+v", keys)
	}
}

func hashOf(t *testing.T, secret string) string {
	t.Helper()
	return HashKeyForTest(secret)
}
