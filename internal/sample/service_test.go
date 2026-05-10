package sample

import (
	"context"
	"testing"
)

func TestServiceCreate(t *testing.T) {
	svc := NewService(NewStorage())
	s, err := svc.Create(context.Background(), CreateSampleInput{Name: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Name != "test" {
		t.Errorf("expected name 'test', got %q", s.Name)
	}
}

func TestServiceList(t *testing.T) {
	svc := NewService(NewStorage())
	items, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected empty list, got %d items", len(items))
	}
}
