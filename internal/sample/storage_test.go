package sample

import (
	"context"
	"testing"
)

func TestStorageCreateAndGet(t *testing.T) {
	s := NewStorage()
	sample := &Sample{ID: "test-id", Name: "test"}
	if err := s.Create(context.Background(), sample); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := s.Get(context.Background(), "test-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "test" {
		t.Errorf("expected name 'test', got %q", got.Name)
	}
}

func TestStorageGetNotFound(t *testing.T) {
	s := NewStorage()
	_, err := s.Get(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent item")
	}
}
