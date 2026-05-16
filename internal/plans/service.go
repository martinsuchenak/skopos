package plans

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) CreatePlan(ctx context.Context, input CreatePlanInput) (*Plan, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.AuthorAgentID = strings.TrimSpace(input.AuthorAgentID)
	if input.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if input.AuthorAgentID == "" {
		return nil, fmt.Errorf("%w: author_agent_id is required", ErrInvalidInput)
	}
	now := time.Now().UTC()
	plan := Plan{
		ID:            generateID(),
		Name:          input.Name,
		BranchName:    strings.TrimSpace(input.BranchName),
		WorkspaceID:   strings.TrimSpace(input.WorkspaceID),
		Description:   strings.TrimSpace(input.Description),
		Status:        PlanActive,
		AuthorAgentID: input.AuthorAgentID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.CreatePlan(ctx, plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s *Service) GetPlan(ctx context.Context, id string) (*Plan, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.GetPlan(ctx, id)
}

func (s *Service) ListPlans(ctx context.Context, workspaceID, branchName string) ([]Plan, error) {
	return s.store.ListPlans(ctx, strings.TrimSpace(workspaceID), strings.TrimSpace(branchName))
}

func (s *Service) UpdatePlan(ctx context.Context, id string, input UpdatePlanInput) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	if input.Status != "" && !validPlanStatus(input.Status) {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidInput, input.Status)
	}
	if err := s.store.UpdatePlan(ctx, id, input); err != nil {
		return err
	}
	if input.Status == PlanCompleted {
		if err := s.autoUnblockPlanDependents(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) DeletePlan(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.DeletePlan(ctx, id)
}

func (s *Service) AddItem(ctx context.Context, planID string, input CreateItemInput) (*Item, error) {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return nil, fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}

	var pos int
	if input.Position != nil {
		pos = *input.Position
		if err := s.store.ShiftPositions(ctx, planID, pos); err != nil {
			return nil, err
		}
	} else {
		plan, err := s.store.GetPlan(ctx, planID)
		if err != nil {
			return nil, err
		}
		pos = len(plan.Items)
	}

	status := ItemPending
	var deps []string
	if len(input.DependsOn) > 0 {
		for _, depID := range input.DependsOn {
			depID = strings.TrimSpace(depID)
			if depID == "" {
				continue
			}
			exists, err := s.store.ItemExistsInPlan(ctx, planID, depID)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, fmt.Errorf("%w: dependency item %s not found in plan", ErrInvalidInput, depID)
			}
			depStatus, err := s.store.ItemStatus(ctx, depID)
			if err != nil {
				return nil, err
			}
			if depStatus != ItemDone {
				status = ItemBlocked
			}
			deps = append(deps, depID)
		}
	}

	now := time.Now().UTC()
	item := Item{
		ID:          generateID(),
		PlanID:      planID,
		Title:       input.Title,
		Description: strings.TrimSpace(input.Description),
		Phase:       strings.TrimSpace(input.Phase),
		Status:      status,
		Position:    pos,
		DependsOn:   deps,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.store.AddItem(ctx, item); err != nil {
		return nil, err
	}
	for _, depID := range deps {
		if err := s.store.AddDependency(ctx, item.ID, depID); err != nil {
			return nil, err
		}
	}
	return &item, nil
}

func (s *Service) UpdateItem(ctx context.Context, planID, itemID string, input UpdateItemInput) (*Item, error) {
	planID = strings.TrimSpace(planID)
	itemID = strings.TrimSpace(itemID)
	if planID == "" {
		return nil, fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	if itemID == "" {
		return nil, fmt.Errorf("%w: item_id is required", ErrInvalidInput)
	}
	if input.Status != "" && !validItemStatus(input.Status) {
		return nil, fmt.Errorf("%w: invalid status %q", ErrInvalidInput, input.Status)
	}

	if err := s.store.UpdateItem(ctx, planID, itemID, input); err != nil {
		return nil, err
	}
	if input.Status == ItemDone {
		if err := s.autoUnblockDependents(ctx, itemID); err != nil {
			return nil, err
		}
		if err := s.tryAutoCompletePlan(ctx, planID); err != nil {
			return nil, err
		}
	}
	return s.store.GetItem(ctx, planID, itemID)
}

func (s *Service) AddDependency(ctx context.Context, planID, itemID, dependsOnID string) error {
	planID = strings.TrimSpace(planID)
	itemID = strings.TrimSpace(itemID)
	dependsOnID = strings.TrimSpace(dependsOnID)
	if planID == "" || itemID == "" || dependsOnID == "" {
		return fmt.Errorf("%w: plan_id, item_id, and depends_on_id are required", ErrInvalidInput)
	}
	if itemID == dependsOnID {
		return fmt.Errorf("%w: item cannot depend on itself", ErrInvalidInput)
	}
	exists, err := s.store.ItemExistsInPlan(ctx, planID, itemID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: item %s not found in plan", ErrNotFound, itemID)
	}
	depExists, err := s.store.ItemExistsInPlan(ctx, planID, dependsOnID)
	if err != nil {
		return err
	}
	if !depExists {
		return fmt.Errorf("%w: dependency item %s not found in plan", ErrNotFound, dependsOnID)
	}
	if err := s.detectCycle(ctx, itemID, dependsOnID); err != nil {
		return err
	}
	if err := s.store.AddDependency(ctx, itemID, dependsOnID); err != nil {
		return err
	}
	depStatus, err := s.store.ItemStatus(ctx, dependsOnID)
	if err != nil {
		return err
	}
	if depStatus != ItemDone {
		if err := s.store.SetItemStatus(ctx, itemID, ItemBlocked); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RemoveDependency(ctx context.Context, planID, itemID, dependsOnID string) error {
	planID = strings.TrimSpace(planID)
	itemID = strings.TrimSpace(itemID)
	dependsOnID = strings.TrimSpace(dependsOnID)
	if planID == "" || itemID == "" || dependsOnID == "" {
		return fmt.Errorf("%w: plan_id, item_id, and depends_on_id are required", ErrInvalidInput)
	}
	if err := s.store.RemoveDependency(ctx, itemID, dependsOnID); err != nil {
		return err
	}
	return s.recheckItemBlocked(ctx, itemID)
}

func (s *Service) DeleteItem(ctx context.Context, planID, itemID string) error {
	planID = strings.TrimSpace(planID)
	itemID = strings.TrimSpace(itemID)
	if planID == "" {
		return fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	if itemID == "" {
		return fmt.Errorf("%w: item_id is required", ErrInvalidInput)
	}
	return s.store.DeleteItem(ctx, planID, itemID)
}

func (s *Service) AddPlanDependency(ctx context.Context, planID, dependsOnPlanID string) error {
	planID = strings.TrimSpace(planID)
	dependsOnPlanID = strings.TrimSpace(dependsOnPlanID)
	if planID == "" || dependsOnPlanID == "" {
		return fmt.Errorf("%w: plan_id and depends_on_plan_id are required", ErrInvalidInput)
	}
	if planID == dependsOnPlanID {
		return fmt.Errorf("%w: plan cannot depend on itself", ErrInvalidInput)
	}
	exists, err := s.store.PlanExists(ctx, planID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	depExists, err := s.store.PlanExists(ctx, dependsOnPlanID)
	if err != nil {
		return err
	}
	if !depExists {
		return fmt.Errorf("%w: dependency plan %s", ErrNotFound, dependsOnPlanID)
	}
	if err := s.detectPlanCycle(ctx, planID, dependsOnPlanID); err != nil {
		return err
	}
	if err := s.store.AddPlanDependency(ctx, planID, dependsOnPlanID); err != nil {
		return err
	}
	depStatus, err := s.store.PlanStatus(ctx, dependsOnPlanID)
	if err != nil {
		return err
	}
	if depStatus != PlanCompleted {
		if err := s.store.SetPlanStatus(ctx, planID, PlanBlocked); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) RemovePlanDependency(ctx context.Context, planID, dependsOnPlanID string) error {
	planID = strings.TrimSpace(planID)
	dependsOnPlanID = strings.TrimSpace(dependsOnPlanID)
	if planID == "" || dependsOnPlanID == "" {
		return fmt.Errorf("%w: plan_id and depends_on_plan_id are required", ErrInvalidInput)
	}
	if err := s.store.RemovePlanDependency(ctx, planID, dependsOnPlanID); err != nil {
		return err
	}
	return s.recheckPlanBlocked(ctx, planID)
}

func (s *Service) autoUnblockDependents(ctx context.Context, doneItemID string) error {
	dependentIDs, err := s.store.ListDependents(ctx, doneItemID)
	if err != nil {
		return err
	}
	for _, depID := range dependentIDs {
		if err := s.recheckItemBlocked(ctx, depID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) tryAutoCompletePlan(ctx context.Context, planID string) error {
	allDone, err := s.store.AllItemsDone(ctx, planID)
	if err != nil {
		return err
	}
	if !allDone {
		return nil
	}
	status, err := s.store.PlanStatus(ctx, planID)
	if err != nil {
		return err
	}
	if status != PlanActive && status != PlanBlocked {
		return nil
	}
	if err := s.store.SetPlanStatus(ctx, planID, PlanCompleted); err != nil {
		return err
	}
	return s.autoUnblockPlanDependents(ctx, planID)
}

func (s *Service) recheckItemBlocked(ctx context.Context, itemID string) error {
	deps, err := s.store.ListDependencies(ctx, itemID)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		current, err := s.store.ItemStatus(ctx, itemID)
		if err != nil {
			return err
		}
		if current == ItemBlocked {
			return s.store.SetItemStatus(ctx, itemID, ItemPending)
		}
		return nil
	}
	for _, depID := range deps {
		status, err := s.store.ItemStatus(ctx, depID)
		if err != nil {
			return err
		}
		if status != ItemDone {
			return s.store.SetItemStatus(ctx, itemID, ItemBlocked)
		}
	}
	current, err := s.store.ItemStatus(ctx, itemID)
	if err != nil {
		return err
	}
	if current == ItemBlocked {
		return s.store.SetItemStatus(ctx, itemID, ItemPending)
	}
	return nil
}

func (s *Service) detectCycle(ctx context.Context, itemID, newDepID string) error {
	visited := map[string]bool{itemID: true}
	queue := []string{newDepID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			return fmt.Errorf("%w: adding this dependency would create a cycle", ErrCycleDetected)
		}
		visited[cur] = true
		deps, err := s.store.ListDependencies(ctx, cur)
		if err != nil {
			return err
		}
		queue = append(queue, deps...)
	}
	return nil
}

func (s *Service) autoUnblockPlanDependents(ctx context.Context, completedPlanID string) error {
	dependentIDs, err := s.store.ListPlanDependents(ctx, completedPlanID)
	if err != nil {
		return err
	}
	for _, depID := range dependentIDs {
		if err := s.recheckPlanBlocked(ctx, depID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recheckPlanBlocked(ctx context.Context, planID string) error {
	deps, err := s.store.ListPlanDependencies(ctx, planID)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		current, err := s.store.PlanStatus(ctx, planID)
		if err != nil {
			return err
		}
		if current == PlanBlocked {
			return s.store.SetPlanStatus(ctx, planID, PlanActive)
		}
		return nil
	}
	for _, depID := range deps {
		status, err := s.store.PlanStatus(ctx, depID)
		if err != nil {
			return err
		}
		if status != PlanCompleted {
			return s.store.SetPlanStatus(ctx, planID, PlanBlocked)
		}
	}
	current, err := s.store.PlanStatus(ctx, planID)
	if err != nil {
		return err
	}
	if current == PlanBlocked {
		return s.store.SetPlanStatus(ctx, planID, PlanActive)
	}
	return nil
}

func (s *Service) detectPlanCycle(ctx context.Context, planID, newDepID string) error {
	visited := map[string]bool{planID: true}
	queue := []string{newDepID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			return fmt.Errorf("%w: adding this plan dependency would create a cycle", ErrCycleDetected)
		}
		visited[cur] = true
		deps, err := s.store.ListPlanDependencies(ctx, cur)
		if err != nil {
			return err
		}
		queue = append(queue, deps...)
	}
	return nil
}

func validPlanStatus(s PlanStatus) bool {
	switch s {
	case PlanActive, PlanCompleted, PlanArchived, PlanBlocked:
		return true
	}
	return false
}

func validItemStatus(s ItemStatus) bool {
	switch s {
	case ItemPending, ItemInProgress, ItemDone, ItemBlocked:
		return true
	}
	return false
}

func generateID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
