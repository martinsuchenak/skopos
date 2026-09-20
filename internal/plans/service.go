package plans

import (
	"context"
	"fmt"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/ids"
	"strings"
	"time"
)

type Service struct {
	store     Store
	now       func() time.Time
	publisher events.Publisher
	// completionHook fires (post-commit, best-effort) whenever a plan
	// transitions to completed — wired in serve to the inbox, whose converted
	// items complete with their plan. Nil (the default) disables it.
	completionHook func(planID string)
}

// SetCompletionHook installs the plan-completion callback (registrar
// pattern: no import between the two domains).
func (s *Service) SetCompletionHook(fn func(planID string)) { s.completionHook = fn }

// fireCompletion invokes the hook after the completing transaction committed.
func (s *Service) fireCompletion(planID string) {
	if s.completionHook != nil {
		s.completionHook(planID)
	}
}

// SetPublisher installs the event bus; mutations publish with their plan's
// authoritative workspace. Nil (the default) disables publishing.
func (s *Service) SetPublisher(p events.Publisher) { s.publisher = p }

// publishPlan emits the mutation event for a plan once the store write
// succeeded; failures are silent (events are advisory).
func (s *Service) publishPlan(ctx context.Context, planID string) {
	if s.publisher == nil {
		return
	}
	ws, err := s.store.PlanWorkspace(ctx, planID)
	if err != nil {
		return
	}
	s.publisher.Publish(events.Event{Type: events.TypePlans, Workspace: ws})
}

func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
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
	if input.WorkspaceID == "" {
		return nil, fmt.Errorf("%w: workspace_id is required on writes (derive it with `skopos workspace` or the git remote)", ErrInvalidInput)
	}
	if err := auth.RequireWorkspace(ctx, input.WorkspaceID); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	plan := Plan{
		ID:            ids.New(),
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
	s.publishPlan(ctx, plan.ID)
	return &plan, nil
}

// requirePlanScope authorizes a by-plan-id operation against the plan's
// workspace; unknown plans report not-found first so existence is not
// leaked across tenants. The workspace is immutable, so checking before the
// transaction is race-free.
func (s *Service) requirePlanScope(ctx context.Context, planID string) error {
	if strings.TrimSpace(planID) == "" {
		return fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	ws, err := s.store.PlanWorkspace(ctx, planID)
	if err != nil {
		return err
	}
	return auth.RequireWorkspace(ctx, ws)
}

// requirePlanScopeQuiet is the by-id variant: foreign plans return the same
// not-found error as nonexistent ones — no existence or ownership oracle.
func (s *Service) requirePlanScopeQuiet(ctx context.Context, planID string) error {
	if strings.TrimSpace(planID) == "" {
		return fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	ws, err := s.store.PlanWorkspace(ctx, planID)
	if err != nil {
		return err
	}
	if err := auth.RequireWorkspaceQuiet(ctx, ws); err != nil {
		return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
	}
	return nil
}

func (s *Service) GetPlan(ctx context.Context, id string) (*Plan, error) {
	if err := s.requirePlanScopeQuiet(ctx, id); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.GetPlan(ctx, id)
}

func (s *Service) ListPlans(ctx context.Context, workspaceID, branchName string) ([]Plan, error) {
	if workspaceID != "" {
		if err := auth.RequireWorkspace(ctx, workspaceID); err != nil {
			return nil, err
		}
	}
	if workspaceID == "" && auth.ScopedContext(ctx) {
		var merged []Plan
		seen := map[string]bool{}
		for _, ws := range auth.PrincipalFromContext(ctx).WorkspaceList() {
			plans, err := s.store.ListPlans(ctx, ws, branchName)
			if err != nil {
				return nil, err
			}
			for _, pl := range plans {
				if !seen[pl.ID] {
					seen[pl.ID] = true
					merged = append(merged, pl)
				}
			}
		}
		return merged, nil
	}
	return s.store.ListPlans(ctx, strings.TrimSpace(workspaceID), strings.TrimSpace(branchName))
}

func (s *Service) UpdatePlan(ctx context.Context, id string, input UpdatePlanInput) error {
	if err := s.requirePlanScopeQuiet(ctx, id); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	if input.Status != "" && !validPlanStatus(input.Status) {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidInput, input.Status)
	}
	var autoCompleted bool
	if err := s.store.RunInTx(ctx, func(tx Store) error {
		prior, err := tx.PlanStatus(ctx, id)
		if err != nil {
			return err
		}
		if err := tx.UpdatePlan(ctx, id, input); err != nil {
			return err
		}
		// Only the transition to completed fires the hook (a redundant
		// completed->completed PATCH must not re-run completion work).
		if input.Status == PlanCompleted && prior != PlanCompleted {
			if err := autoUnblockPlanDependents(ctx, tx, id); err != nil {
				return err
			}
			autoCompleted = true
		}
		return nil
	}); err != nil {
		return err
	}
	s.publishPlan(ctx, id)
	if autoCompleted {
		s.fireCompletion(id)
	}
	return nil
}

func (s *Service) DeletePlan(ctx context.Context, id string) error {
	if err := s.requirePlanScopeQuiet(ctx, id); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	if err := s.store.DeletePlan(ctx, id); err != nil {
		return err
	}
	s.publishPlan(ctx, id)
	return nil
}

func (s *Service) AddItem(ctx context.Context, planID string, input CreateItemInput) (*Item, error) {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return nil, err
	}
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return nil, fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}

	var item Item
	err := s.store.RunInTx(ctx, func(tx Store) error {
		plan, err := tx.GetPlan(ctx, planID)
		if err != nil {
			return err
		}
		var pos int
		if input.Position != nil {
			pos = *input.Position
			if pos < 0 || pos > len(plan.Items) {
				return fmt.Errorf("%w: position %d out of range [0, %d]", ErrInvalidInput, pos, len(plan.Items))
			}
			if err := tx.ShiftPositions(ctx, planID, pos); err != nil {
				return err
			}
		} else {
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
				exists, err := tx.ItemExistsInPlan(ctx, planID, depID)
				if err != nil {
					return err
				}
				if !exists {
					return fmt.Errorf("%w: dependency item %s not found in plan", ErrInvalidInput, depID)
				}
				depStatus, err := tx.ItemStatus(ctx, depID)
				if err != nil {
					return err
				}
				if depStatus != ItemDone {
					status = ItemBlocked
				}
				deps = append(deps, depID)
			}
		}

		now := s.now().UTC()
		item = Item{
			ID:          ids.New(),
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
		if err := tx.AddItem(ctx, item); err != nil {
			return err
		}
		for _, depID := range deps {
			if err := tx.AddDependency(ctx, item.ID, depID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishPlan(ctx, planID)
	return &item, nil
}

func (s *Service) UpdateItem(ctx context.Context, planID, itemID string, input UpdateItemInput) (*Item, error) {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return nil, err
	}
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

	var autoCompleted bool
	err := s.store.RunInTx(ctx, func(tx Store) error {
		if input.Status != "" {
			if err := assertTransitionAllowed(ctx, tx, planID, itemID, input.Status); err != nil {
				return err
			}
		}
		if err := tx.UpdateItem(ctx, planID, itemID, input); err != nil {
			return err
		}
		if input.Status == ItemDone {
			if err := autoUnblockDependents(ctx, tx, itemID); err != nil {
				return err
			}
			completed, err := tryAutoCompletePlan(ctx, tx, planID)
			if err != nil {
				return err
			}
			autoCompleted = completed
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.publishPlan(ctx, planID)
	if autoCompleted {
		s.fireCompletion(planID)
	}
	return s.store.GetItem(ctx, planID, itemID)
}

// assertTransitionAllowed enforces the item state machine on the update path:
// an item may not be marked done while any dependency is unfinished, a done
// item may not be reopened (it would invalidate dependents already unblocked
// by its completion), and items of a completed or archived plan are frozen.
// The create and dependency-add paths already enforce these invariants; this
// guard extends them to direct PATCHes so they cannot be bypassed.
func assertTransitionAllowed(ctx context.Context, store Store, planID, itemID string, next ItemStatus) error {
	planStatus, err := store.PlanStatus(ctx, planID)
	if err != nil {
		return err
	}
	if planStatus == PlanCompleted || planStatus == PlanArchived {
		return fmt.Errorf("%w: plan is %s; items are frozen", ErrInvalidInput, planStatus)
	}
	current, err := store.ItemStatus(ctx, itemID)
	if err != nil {
		return err
	}
	if current == ItemDone && next != ItemDone {
		return fmt.Errorf("%w: item is done and cannot be reopened", ErrInvalidInput)
	}
	if next == ItemDone {
		deps, err := store.ListDependencies(ctx, itemID)
		if err != nil {
			return err
		}
		for _, depID := range deps {
			depStatus, err := store.ItemStatus(ctx, depID)
			if err != nil {
				return err
			}
			if depStatus != ItemDone {
				return fmt.Errorf("%w: dependency %s is not done", ErrInvalidInput, depID)
			}
		}
	}
	return nil
}

func (s *Service) AddDependency(ctx context.Context, planID, itemID, dependsOnID string) error {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return err
	}
	planID = strings.TrimSpace(planID)
	itemID = strings.TrimSpace(itemID)
	dependsOnID = strings.TrimSpace(dependsOnID)
	if planID == "" || itemID == "" || dependsOnID == "" {
		return fmt.Errorf("%w: plan_id, item_id, and depends_on_id are required", ErrInvalidInput)
	}
	if itemID == dependsOnID {
		return fmt.Errorf("%w: item cannot depend on itself", ErrInvalidInput)
	}
	if err := s.store.RunInTx(ctx, func(tx Store) error {
		exists, err := tx.ItemExistsInPlan(ctx, planID, itemID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: item %s not found in plan", ErrNotFound, itemID)
		}
		depExists, err := tx.ItemExistsInPlan(ctx, planID, dependsOnID)
		if err != nil {
			return err
		}
		if !depExists {
			return fmt.Errorf("%w: dependency item %s not found in plan", ErrNotFound, dependsOnID)
		}
		if err := detectCycle(ctx, tx, itemID, dependsOnID); err != nil {
			return err
		}
		if err := tx.AddDependency(ctx, itemID, dependsOnID); err != nil {
			return err
		}
		depStatus, err := tx.ItemStatus(ctx, dependsOnID)
		if err != nil {
			return err
		}
		if depStatus != ItemDone {
			if err := tx.SetItemStatus(ctx, itemID, ItemBlocked); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	s.publishPlan(ctx, planID)
	return nil
}

func (s *Service) RemoveDependency(ctx context.Context, planID, itemID, dependsOnID string) error {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return err
	}
	planID = strings.TrimSpace(planID)
	itemID = strings.TrimSpace(itemID)
	dependsOnID = strings.TrimSpace(dependsOnID)
	if planID == "" || itemID == "" || dependsOnID == "" {
		return fmt.Errorf("%w: plan_id, item_id, and depends_on_id are required", ErrInvalidInput)
	}
	if err := s.store.RunInTx(ctx, func(tx Store) error {
		if err := tx.RemoveDependency(ctx, itemID, dependsOnID); err != nil {
			return err
		}
		return recheckItemBlocked(ctx, tx, itemID)
	}); err != nil {
		return err
	}
	s.publishPlan(ctx, planID)
	return nil
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
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return err
	}
	if err := s.store.DeleteItem(ctx, planID, itemID); err != nil {
		return err
	}
	s.publishPlan(ctx, planID)
	return nil
}

func (s *Service) AddPlanDependency(ctx context.Context, planID, dependsOnPlanID string) error {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return err
	}
	if err := s.requirePlanScopeQuiet(ctx, dependsOnPlanID); err != nil {
		return err
	}
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return err
	}
	planID = strings.TrimSpace(planID)
	dependsOnPlanID = strings.TrimSpace(dependsOnPlanID)
	if planID == "" || dependsOnPlanID == "" {
		return fmt.Errorf("%w: plan_id and depends_on_plan_id are required", ErrInvalidInput)
	}
	if planID == dependsOnPlanID {
		return fmt.Errorf("%w: plan cannot depend on itself", ErrInvalidInput)
	}
	if err := s.store.RunInTx(ctx, func(tx Store) error {
		exists, err := tx.PlanExists(ctx, planID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
		}
		depExists, err := tx.PlanExists(ctx, dependsOnPlanID)
		if err != nil {
			return err
		}
		if !depExists {
			return fmt.Errorf("%w: dependency plan %s", ErrNotFound, dependsOnPlanID)
		}
		if err := detectPlanCycle(ctx, tx, planID, dependsOnPlanID); err != nil {
			return err
		}
		if err := tx.AddPlanDependency(ctx, planID, dependsOnPlanID); err != nil {
			return err
		}
		depStatus, err := tx.PlanStatus(ctx, dependsOnPlanID)
		if err != nil {
			return err
		}
		if depStatus != PlanCompleted {
			if err := tx.SetPlanStatus(ctx, planID, PlanBlocked); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	s.publishPlan(ctx, planID)
	return nil
}

func (s *Service) RemovePlanDependency(ctx context.Context, planID, dependsOnPlanID string) error {
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return err
	}
	if err := s.requirePlanScopeQuiet(ctx, dependsOnPlanID); err != nil {
		return err
	}
	if err := s.requirePlanScopeQuiet(ctx, planID); err != nil {
		return err
	}
	planID = strings.TrimSpace(planID)
	dependsOnPlanID = strings.TrimSpace(dependsOnPlanID)
	if planID == "" || dependsOnPlanID == "" {
		return fmt.Errorf("%w: plan_id and depends_on_plan_id are required", ErrInvalidInput)
	}
	if err := s.store.RunInTx(ctx, func(tx Store) error {
		if err := tx.RemovePlanDependency(ctx, planID, dependsOnPlanID); err != nil {
			return err
		}
		return recheckPlanBlocked(ctx, tx, planID)
	}); err != nil {
		return err
	}
	s.publishPlan(ctx, planID)
	return nil
}

func autoUnblockDependents(ctx context.Context, store Store, doneItemID string) error {
	dependentIDs, err := store.ListDependents(ctx, doneItemID)
	if err != nil {
		return err
	}
	for _, depID := range dependentIDs {
		if err := recheckItemBlocked(ctx, store, depID); err != nil {
			return err
		}
	}
	return nil
}

// tryAutoCompletePlan completes the plan when every item is done, cascading
// the unblock of dependent plans. It reports whether this call performed the
// transition (so the completion hook fires only on the transition).
func tryAutoCompletePlan(ctx context.Context, store Store, planID string) (bool, error) {
	allDone, err := store.AllItemsDone(ctx, planID)
	if err != nil {
		return false, err
	}
	if !allDone {
		return false, nil
	}
	status, err := store.PlanStatus(ctx, planID)
	if err != nil {
		return false, err
	}
	if status != PlanActive && status != PlanBlocked {
		return false, nil
	}
	if err := store.SetPlanStatus(ctx, planID, PlanCompleted); err != nil {
		return false, err
	}
	return true, autoUnblockPlanDependents(ctx, store, planID)
}

func recheckItemBlocked(ctx context.Context, store Store, itemID string) error {
	deps, err := store.ListDependencies(ctx, itemID)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		current, err := store.ItemStatus(ctx, itemID)
		if err != nil {
			return err
		}
		if current == ItemBlocked {
			return store.SetItemStatus(ctx, itemID, ItemPending)
		}
		return nil
	}
	for _, depID := range deps {
		status, err := store.ItemStatus(ctx, depID)
		if err != nil {
			return err
		}
		if status != ItemDone {
			return store.SetItemStatus(ctx, itemID, ItemBlocked)
		}
	}
	current, err := store.ItemStatus(ctx, itemID)
	if err != nil {
		return err
	}
	if current == ItemBlocked {
		return store.SetItemStatus(ctx, itemID, ItemPending)
	}
	return nil
}

func detectCycle(ctx context.Context, store Store, itemID, newDepID string) error {
	visited := map[string]bool{itemID: true}
	queue := []string{newDepID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			return fmt.Errorf("%w: adding this dependency would create a cycle", ErrCycleDetected)
		}
		visited[cur] = true
		deps, err := store.ListDependencies(ctx, cur)
		if err != nil {
			return err
		}
		queue = append(queue, deps...)
	}
	return nil
}

func autoUnblockPlanDependents(ctx context.Context, store Store, completedPlanID string) error {
	dependentIDs, err := store.ListPlanDependents(ctx, completedPlanID)
	if err != nil {
		return err
	}
	for _, depID := range dependentIDs {
		if err := recheckPlanBlocked(ctx, store, depID); err != nil {
			return err
		}
	}
	return nil
}

func recheckPlanBlocked(ctx context.Context, store Store, planID string) error {
	deps, err := store.ListPlanDependencies(ctx, planID)
	if err != nil {
		return err
	}
	if len(deps) == 0 {
		current, err := store.PlanStatus(ctx, planID)
		if err != nil {
			return err
		}
		if current == PlanBlocked {
			return store.SetPlanStatus(ctx, planID, PlanActive)
		}
		return nil
	}
	for _, depID := range deps {
		status, err := store.PlanStatus(ctx, depID)
		if err != nil {
			return err
		}
		if status != PlanCompleted {
			return store.SetPlanStatus(ctx, planID, PlanBlocked)
		}
	}
	current, err := store.PlanStatus(ctx, planID)
	if err != nil {
		return err
	}
	if current == PlanBlocked {
		return store.SetPlanStatus(ctx, planID, PlanActive)
	}
	return nil
}

func detectPlanCycle(ctx context.Context, store Store, planID, newDepID string) error {
	visited := map[string]bool{planID: true}
	queue := []string{newDepID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			return fmt.Errorf("%w: adding this plan dependency would create a cycle", ErrCycleDetected)
		}
		visited[cur] = true
		deps, err := store.ListPlanDependencies(ctx, cur)
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
