package plans

import (
	"context"
	"errors"
	"testing"
)

func newPlanWithItems() *fakeStore {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{
			{ID: "i1", PlanID: "p1", Title: "A", Status: ItemPending, Position: 0},
			{ID: "i2", PlanID: "p1", Title: "B", Status: ItemPending, Position: 1},
		},
	}
	return store
}

func TestServiceAddItemRejectsNegativePosition(t *testing.T) {
	svc := NewService(newPlanWithItems())
	_, err := svc.AddItem(context.Background(), "p1", CreateItemInput{Title: "C", Position: ptr(-5)})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for negative position, got %v", err)
	}
}

func TestServiceAddItemRejectsOverflowPosition(t *testing.T) {
	svc := NewService(newPlanWithItems())
	_, err := svc.AddItem(context.Background(), "p1", CreateItemInput{Title: "C", Position: ptr(9223372036854775807)})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for int64-max position, got %v", err)
	}
}

func TestServiceAddItemRejectsPositionBeyondEnd(t *testing.T) {
	svc := NewService(newPlanWithItems())
	_, err := svc.AddItem(context.Background(), "p1", CreateItemInput{Title: "C", Position: ptr(5)})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for position 5 with 2 items, got %v", err)
	}
}

func TestServiceAddItemAcceptsPositionAtEnd(t *testing.T) {
	svc := NewService(newPlanWithItems())
	item, err := svc.AddItem(context.Background(), "p1", CreateItemInput{Title: "C", Position: ptr(2)})
	if err != nil {
		t.Fatalf("unexpected error for position 2 with 2 items: %v", err)
	}
	if item.Position != 2 {
		t.Errorf("expected position 2, got %d", item.Position)
	}
}

func ptr(i int) *int { return &i }
