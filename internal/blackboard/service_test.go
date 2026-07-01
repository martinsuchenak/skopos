package blackboard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeStore struct {
	entries  []Entry
	writeErr error
}

func (f *fakeStore) Write(_ context.Context, e Entry) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.entries = append(f.entries, e)
	return nil
}
func (f *fakeStore) Bundle(_ context.Context, _, _, _ string) ([]Entry, error) {
	return f.entries, nil
}
func (f *fakeStore) Promote(_ context.Context, _ string) error                  { return nil }
func (f *fakeStore) Delete(_ context.Context, _ string) error                   { return nil }
func (f *fakeStore) Search(_ context.Context, _ SearchFilters) ([]Entry, error) { return nil, nil }
func (f *fakeStore) DeleteBySession(_ context.Context, _ string) error          { return nil }
func (f *fakeStore) SessionExists(_ context.Context, _ string) (bool, error) { return false, nil }
func (f *fakeStore) Get(_ context.Context, _ string) (*Entry, error) {
	return nil, ErrNotFound
}

func TestServiceWriteValidationMissingTitle(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeProject, EntryType: TypeFinding, AuthorAgentID: "agent-1",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationMissingAuthorAgentID(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeProject, EntryType: TypeFinding, Title: "T",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationInvalidScope(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: "global", EntryType: TypeFinding, Title: "T", AuthorAgentID: "a",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationInvalidEntryType(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeProject, EntryType: "memo", Title: "T", AuthorAgentID: "a",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationBranchScopeRequiresBranchName(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeBranch, EntryType: TypeFinding, Title: "T", AuthorAgentID: "a",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationSessionScopeRequiresSessionID(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeSession, EntryType: TypeContext, Title: "T", AuthorAgentID: "a",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteSuccess(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store)
	result, err := svc.Write(context.Background(), WriteInput{
		Scope:         ScopeProject,
		EntryType:     TypeFinding,
		Title:         "Found something",
		Content:       "Details here.",
		AuthorAgentID: "agent-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ID == "" {
		t.Fatal("expected generated ID")
	}
	if result.Scope != ScopeProject {
		t.Errorf("scope: got %q, want %q", result.Scope, ScopeProject)
	}
	if len(store.entries) != 1 {
		t.Fatalf("expected 1 stored entry, got %d", len(store.entries))
	}
}

func TestServiceBundleMarkdownNotEmpty(t *testing.T) {
	store := &fakeStore{entries: []Entry{
		{EntryType: TypeBug, Title: "Crash on nil", AuthorAgentID: "a", Scope: ScopeBranch, BranchName: "feat"},
	}}
	svc := NewService(store)
	bundle, err := svc.Bundle(context.Background(), "", "feat", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bundle.MarkdownBundle == "" {
		t.Fatal("expected non-empty MarkdownBundle")
	}
	if !strings.Contains(bundle.MarkdownBundle, "Crash on nil") {
		t.Errorf("expected entry title in markdown, got: %s", bundle.MarkdownBundle)
	}
}

func TestServiceBundleEmptyMarkdown(t *testing.T) {
	svc := NewService(&fakeStore{})
	bundle, err := svc.Bundle(context.Background(), "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bundle.MarkdownBundle == "" {
		t.Fatal("expected non-empty markdown even with no entries")
	}
}

func TestServicePromoteEmptyID(t *testing.T) {
	svc := NewService(&fakeStore{})
	err := svc.Promote(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceDeleteEmptyID(t *testing.T) {
	svc := NewService(&fakeStore{})
	err := svc.Delete(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteStoreError(t *testing.T) {
	store := &fakeStore{writeErr: ErrInvalidInput}
	svc := NewService(store)
	_, err := svc.Write(context.Background(), WriteInput{
		Scope:         ScopeProject,
		EntryType:     TypeFinding,
		Title:         "T",
		AuthorAgentID: "a",
	})
	if err == nil {
		t.Fatal("expected error from store, got nil")
	}
}
