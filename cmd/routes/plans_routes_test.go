package routes

import (
	"context"
	"net/http"
	"testing"

	"github.com/martinsuchenak/skopos/internal/plans"
)

func TestRegisterPlansRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerPlansRoutes(mux, plans.NewHandler(
		plans.NewService(&noopPlansStore{}), "",
	))
}

type noopPlansStore struct{}

func (s *noopPlansStore) CreatePlan(_ context.Context, _ plans.Plan) error { return nil }
func (s *noopPlansStore) GetPlan(_ context.Context, _ string) (*plans.Plan, error) {
	return nil, plans.ErrNotFound
}
func (s *noopPlansStore) ListPlans(_ context.Context, _, _ string) ([]plans.Plan, error) {
	return nil, nil
}
func (s *noopPlansStore) UpdatePlan(_ context.Context, _ string, _ plans.UpdatePlanInput) error {
	return nil
}
func (s *noopPlansStore) DeletePlan(_ context.Context, _ string) error { return nil }
func (s *noopPlansStore) AddItem(_ context.Context, _ plans.Item) error { return nil }
func (s *noopPlansStore) UpdateItem(_ context.Context, _, _ string, _ plans.UpdateItemInput) error {
	return nil
}
func (s *noopPlansStore) GetItem(_ context.Context, _, _ string) (*plans.Item, error) {
	return nil, plans.ErrNotFound
}
func (s *noopPlansStore) DeleteItem(_ context.Context, _, _ string) error { return nil }
func (s *noopPlansStore) ShiftPositions(_ context.Context, _ string, _ int) error {
	return nil
}
func (s *noopPlansStore) AddDependency(_ context.Context, _, _ string) error { return nil }
func (s *noopPlansStore) RemoveDependency(_ context.Context, _, _ string) error {
	return nil
}
func (s *noopPlansStore) ListDependencies(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *noopPlansStore) ListDependents(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *noopPlansStore) ItemExistsInPlan(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (s *noopPlansStore) ItemStatus(_ context.Context, _ string) (plans.ItemStatus, error) {
	return plans.ItemPending, nil
}
func (s *noopPlansStore) SetItemStatus(_ context.Context, _ string, _ plans.ItemStatus) error {
	return nil
}
func (s *noopPlansStore) AddPlanDependency(_ context.Context, _, _ string) error { return nil }
func (s *noopPlansStore) RemovePlanDependency(_ context.Context, _, _ string) error {
	return nil
}
func (s *noopPlansStore) ListPlanDependencies(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *noopPlansStore) ListPlanDependents(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *noopPlansStore) PlanStatus(_ context.Context, _ string) (plans.PlanStatus, error) {
	return plans.PlanActive, nil
}
func (s *noopPlansStore) SetPlanStatus(_ context.Context, _ string, _ plans.PlanStatus) error {
	return nil
}
func (s *noopPlansStore) PlanExists(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (s *noopPlansStore) AllItemsDone(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (s *noopPlansStore) RunInTx(_ context.Context, fn func(plans.Store) error) error {
	return fn(s)
}
