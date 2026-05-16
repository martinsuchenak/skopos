package plans

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type fakeStore struct {
	plans       map[string]*Plan
	items       map[string]*Item
	deps        map[string][]string
	planDeps    map[string][]string
	createErr   error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		plans:    make(map[string]*Plan),
		items:    make(map[string]*Item),
		deps:     make(map[string][]string),
		planDeps: make(map[string][]string),
	}
}

func (f *fakeStore) CreatePlan(_ context.Context, plan Plan) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.plans[plan.ID] = &plan
	return nil
}

func (f *fakeStore) GetPlan(_ context.Context, id string) (*Plan, error) {
	p, ok := f.plans[id]
	if !ok {
		return nil, fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	return p, nil
}

func (f *fakeStore) ListPlans(_ context.Context, _, _ string) ([]Plan, error) {
	var result []Plan
	for _, p := range f.plans {
		result = append(result, *p)
	}
	return result, nil
}

func (f *fakeStore) UpdatePlan(_ context.Context, id string, input UpdatePlanInput) error {
	p, ok := f.plans[id]
	if !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	if input.Name != "" {
		p.Name = input.Name
	}
	if input.Description != "" {
		p.Description = input.Description
	}
	if input.Status != "" {
		p.Status = input.Status
	}
	return nil
}

func (f *fakeStore) DeletePlan(_ context.Context, id string) error {
	if _, ok := f.plans[id]; !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, id)
	}
	delete(f.plans, id)
	return nil
}

func (f *fakeStore) AddItem(_ context.Context, item Item) error {
	p, ok := f.plans[item.PlanID]
	if !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, item.PlanID)
	}
	p.Items = append(p.Items, item)
	f.items[item.ID] = &item
	return nil
}

func (f *fakeStore) UpdateItem(_ context.Context, planID, itemID string, input UpdateItemInput) error {
	p, ok := f.plans[planID]
	if !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	for i, item := range p.Items {
		if item.ID == itemID {
			if input.Status != "" {
				p.Items[i].Status = input.Status
			}
			if input.ClaimedByAgentID != nil {
				p.Items[i].ClaimedByAgentID = *input.ClaimedByAgentID
			}
			if it, ok := f.items[itemID]; ok {
				it.Status = p.Items[i].Status
			}
			return nil
		}
	}
	return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
}

func (f *fakeStore) GetItem(_ context.Context, planID, itemID string) (*Item, error) {
	p, ok := f.plans[planID]
	if !ok {
		return nil, fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	for i, item := range p.Items {
		if item.ID == itemID {
			return &p.Items[i], nil
		}
	}
	return nil, fmt.Errorf("%w: item %s", ErrNotFound, itemID)
}

func (f *fakeStore) DeleteItem(_ context.Context, planID, itemID string) error {
	p, ok := f.plans[planID]
	if !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	for _, item := range p.Items {
		if item.ID == itemID {
			return nil
		}
	}
	return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
}

func (f *fakeStore) ShiftPositions(_ context.Context, _ string, _ int) error { return nil }

func (f *fakeStore) AddDependency(_ context.Context, itemID, dependsOnID string) error {
	f.deps[itemID] = append(f.deps[itemID], dependsOnID)
	return nil
}

func (f *fakeStore) RemoveDependency(_ context.Context, itemID, dependsOnID string) error {
	deps := f.deps[itemID]
	for i, d := range deps {
		if d == dependsOnID {
			f.deps[itemID] = append(deps[:i], deps[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeStore) ListDependencies(_ context.Context, itemID string) ([]string, error) {
	return f.deps[itemID], nil
}

func (f *fakeStore) ListDependents(_ context.Context, itemID string) ([]string, error) {
	var dependents []string
	for id, deps := range f.deps {
		for _, d := range deps {
			if d == itemID {
				dependents = append(dependents, id)
			}
		}
	}
	return dependents, nil
}

func (f *fakeStore) ItemExistsInPlan(_ context.Context, planID, itemID string) (bool, error) {
	p, ok := f.plans[planID]
	if !ok {
		return false, nil
	}
	for _, item := range p.Items {
		if item.ID == itemID {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) ItemStatus(_ context.Context, itemID string) (ItemStatus, error) {
	it, ok := f.items[itemID]
	if !ok {
		return "", fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	return it.Status, nil
}

func (f *fakeStore) SetItemStatus(_ context.Context, itemID string, status ItemStatus) error {
	it, ok := f.items[itemID]
	if !ok {
		return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	it.Status = status
	return nil
}

func (f *fakeStore) AddPlanDependency(_ context.Context, planID, dependsOnPlanID string) error {
	f.planDeps[planID] = append(f.planDeps[planID], dependsOnPlanID)
	return nil
}

func (f *fakeStore) RemovePlanDependency(_ context.Context, planID, dependsOnPlanID string) error {
	deps := f.planDeps[planID]
	for i, d := range deps {
		if d == dependsOnPlanID {
			f.planDeps[planID] = append(deps[:i], deps[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeStore) ListPlanDependencies(_ context.Context, planID string) ([]string, error) {
	return f.planDeps[planID], nil
}

func (f *fakeStore) ListPlanDependents(_ context.Context, planID string) ([]string, error) {
	var dependents []string
	for id, deps := range f.planDeps {
		for _, d := range deps {
			if d == planID {
				dependents = append(dependents, id)
			}
		}
	}
	return dependents, nil
}

func (f *fakeStore) PlanStatus(_ context.Context, planID string) (PlanStatus, error) {
	p, ok := f.plans[planID]
	if !ok {
		return "", fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	return p.Status, nil
}

func (f *fakeStore) SetPlanStatus(_ context.Context, planID string, status PlanStatus) error {
	p, ok := f.plans[planID]
	if !ok {
		return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	p.Status = status
	return nil
}

func (f *fakeStore) PlanExists(_ context.Context, planID string) (bool, error) {
	_, ok := f.plans[planID]
	return ok, nil
}

func (f *fakeStore) AllItemsDone(_ context.Context, planID string) (bool, error) {
	var hasItems bool
	for _, item := range f.items {
		if item.PlanID == planID {
			hasItems = true
			if item.Status != ItemDone {
				return false, nil
			}
		}
	}
	return hasItems, nil
}

func TestServiceCreatePlanMissingName(t *testing.T) {
	svc := NewService(newFakeStore())
	_, err := svc.CreatePlan(context.Background(), CreatePlanInput{AuthorAgentID: "agent-1"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceCreatePlanMissingAuthorAgentID(t *testing.T) {
	svc := NewService(newFakeStore())
	_, err := svc.CreatePlan(context.Background(), CreatePlanInput{Name: "My Plan"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceCreatePlanSuccess(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	plan, err := svc.CreatePlan(context.Background(), CreatePlanInput{
		Name:          "Auth refactor",
		AuthorAgentID: "agent-1",
		BranchName:    "feat-auth",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.ID == "" {
		t.Fatal("expected generated ID")
	}
	if plan.Status != PlanActive {
		t.Errorf("expected active status, got %q", plan.Status)
	}
	if len(store.plans) != 1 {
		t.Fatalf("expected 1 stored plan, got %d", len(store.plans))
	}
}

func TestServiceUpdatePlanInvalidStatus(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a"}
	svc := NewService(store)
	err := svc.UpdatePlan(context.Background(), "p1", UpdatePlanInput{Status: "unknown"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceUpdatePlanEmptyID(t *testing.T) {
	svc := NewService(newFakeStore())
	err := svc.UpdatePlan(context.Background(), "", UpdatePlanInput{Status: PlanCompleted})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceDeletePlanEmptyID(t *testing.T) {
	svc := NewService(newFakeStore())
	err := svc.DeletePlan(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceAddItemMissingTitle(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a"}
	svc := NewService(store)
	_, err := svc.AddItem(context.Background(), "p1", CreateItemInput{})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceAddItemPositionDefaultsToEnd(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{
			{ID: "i1", PlanID: "p1", Title: "A", Status: ItemPending, Position: 0},
			{ID: "i2", PlanID: "p1", Title: "B", Status: ItemPending, Position: 1},
		},
	}
	svc := NewService(store)
	item, err := svc.AddItem(context.Background(), "p1", CreateItemInput{Title: "C"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.Position != 2 {
		t.Errorf("expected position 2 (append at end), got %d", item.Position)
	}
}

func TestServiceUpdateItemInvalidStatus(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{{ID: "i1", PlanID: "p1", Title: "T", Status: ItemPending}},
	}
	svc := NewService(store)
	_, err := svc.UpdateItem(context.Background(), "p1", "i1", UpdateItemInput{Status: "invalid"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceDeleteItemEmptyID(t *testing.T) {
	svc := NewService(newFakeStore())
	err := svc.DeleteItem(context.Background(), "p1", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceAddDependencyAutoBlocks(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{
			{ID: "i1", PlanID: "p1", Title: "A", Status: ItemPending, Position: 0},
			{ID: "i2", PlanID: "p1", Title: "B", Status: ItemPending, Position: 1},
		},
	}
	store.items["i1"] = &store.plans["p1"].Items[0]
	store.items["i2"] = &store.plans["p1"].Items[1]
	svc := NewService(store)

	err := svc.AddDependency(context.Background(), "p1", "i2", "i1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.items["i2"].Status != ItemBlocked {
		t.Errorf("expected i2 blocked, got %q", store.items["i2"].Status)
	}
}

func TestServiceAddDependencyCycleDetected(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{
			{ID: "i1", PlanID: "p1", Title: "A", Status: ItemDone, Position: 0},
			{ID: "i2", PlanID: "p1", Title: "B", Status: ItemBlocked, Position: 1},
		},
	}
	store.items["i1"] = &store.plans["p1"].Items[0]
	store.items["i2"] = &store.plans["p1"].Items[1]
	store.deps["i2"] = []string{"i1"}
	svc := NewService(store)

	err := svc.AddDependency(context.Background(), "p1", "i1", "i2")
	if !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("expected ErrCycleDetected, got %v", err)
	}
}

func TestServiceRemoveDependencyAutoUnblocks(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{
			{ID: "i1", PlanID: "p1", Title: "A", Status: ItemDone, Position: 0},
			{ID: "i2", PlanID: "p1", Title: "B", Status: ItemBlocked, Position: 1},
		},
	}
	store.items["i1"] = &store.plans["p1"].Items[0]
	store.items["i2"] = &store.plans["p1"].Items[1]
	store.deps["i2"] = []string{"i1"}
	svc := NewService(store)

	err := svc.RemoveDependency(context.Background(), "p1", "i2", "i1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.items["i2"].Status != ItemPending {
		t.Errorf("expected i2 pending after removing last dep, got %q", store.items["i2"].Status)
	}
}

func TestServiceUpdateItemDoneAutoUnblocksDependents(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Name: "P", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{
			{ID: "i1", PlanID: "p1", Title: "A", Status: ItemInProgress, Position: 0},
			{ID: "i2", PlanID: "p1", Title: "B", Status: ItemBlocked, Position: 1},
		},
	}
	store.items["i1"] = &store.plans["p1"].Items[0]
	store.items["i2"] = &store.plans["p1"].Items[1]
	store.deps["i2"] = []string{"i1"}
	svc := NewService(store)

	_, err := svc.UpdateItem(context.Background(), "p1", "i1", UpdateItemInput{Status: ItemDone})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.items["i2"].Status != ItemPending {
		t.Errorf("expected i2 auto-unblocked to pending, got %q", store.items["i2"].Status)
	}
}

func TestServiceAddPlanDependencyAutoBlocks(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{ID: "p1", Name: "A", Status: PlanActive, AuthorAgentID: "a"}
	store.plans["p2"] = &Plan{ID: "p2", Name: "B", Status: PlanActive, AuthorAgentID: "a"}
	svc := NewService(store)
	err := svc.AddPlanDependency(context.Background(), "p2", "p1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.plans["p2"].Status != PlanBlocked {
		t.Errorf("expected p2 blocked, got %q", store.plans["p2"].Status)
	}
}

func TestServiceAddPlanDependencyCycleDetected(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{ID: "p1", Name: "A", Status: PlanActive, AuthorAgentID: "a"}
	store.plans["p2"] = &Plan{ID: "p2", Name: "B", Status: PlanBlocked, AuthorAgentID: "a"}
	store.planDeps["p2"] = []string{"p1"}
	svc := NewService(store)
	err := svc.AddPlanDependency(context.Background(), "p1", "p2")
	if !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("expected ErrCycleDetected, got %v", err)
	}
}

func TestServiceUpdatePlanCompletedAutoUnblocksDependents(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{ID: "p1", Name: "A", Status: PlanActive, AuthorAgentID: "a"}
	store.plans["p2"] = &Plan{ID: "p2", Name: "B", Status: PlanBlocked, AuthorAgentID: "a"}
	store.planDeps["p2"] = []string{"p1"}
	svc := NewService(store)
	err := svc.UpdatePlan(context.Background(), "p1", UpdatePlanInput{Status: PlanCompleted})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.plans["p2"].Status != PlanActive {
		t.Errorf("expected p2 auto-unblocked to active, got %q", store.plans["p2"].Status)
	}
}

func TestServiceRemovePlanDependencyAutoUnblocks(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{ID: "p1", Name: "A", Status: PlanCompleted, AuthorAgentID: "a"}
	store.plans["p2"] = &Plan{ID: "p2", Name: "B", Status: PlanBlocked, AuthorAgentID: "a"}
	store.planDeps["p2"] = []string{"p1"}
	svc := NewService(store)
	err := svc.RemovePlanDependency(context.Background(), "p2", "p1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.plans["p2"].Status != PlanActive {
		t.Errorf("expected p2 auto-unblocked to active, got %q", store.plans["p2"].Status)
	}
}

func TestServiceUpdateItemDoneAutoCompletesPlanAndUnblocksDependentPlan(t *testing.T) {
	store := newFakeStore()
	store.plans["p1"] = &Plan{
		ID: "p1", Name: "A", Status: PlanActive, AuthorAgentID: "a",
		Items: []Item{
			{ID: "i1", PlanID: "p1", Title: "Task 1", Status: ItemInProgress, Position: 0},
		},
	}
	store.plans["p2"] = &Plan{ID: "p2", Name: "B", Status: PlanBlocked, AuthorAgentID: "a"}
	store.items["i1"] = &store.plans["p1"].Items[0]
	store.planDeps["p2"] = []string{"p1"}
	svc := NewService(store)

	_, err := svc.UpdateItem(context.Background(), "p1", "i1", UpdateItemInput{Status: ItemDone})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store.plans["p1"].Status != PlanCompleted {
		t.Errorf("expected p1 auto-completed, got %q", store.plans["p1"].Status)
	}
	if store.plans["p2"].Status != PlanActive {
		t.Errorf("expected p2 auto-unblocked to active, got %q", store.plans["p2"].Status)
	}
}
