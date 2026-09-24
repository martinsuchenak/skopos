package inbox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/ids"
)

type Service struct {
	store     Store
	now       func() time.Time
	publisher events.Publisher
}

func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// SetPublisher installs the event bus; mutations publish with their item's
// authoritative workspace. Nil (the default) disables publishing.
func (s *Service) SetPublisher(p events.Publisher) { s.publisher = p }

// publishItem emits the mutation event for an item once the store write
// succeeded; failures are silent (events are advisory).
func (s *Service) publishItem(ctx context.Context, itemID string) {
	if s.publisher == nil {
		return
	}
	ws, err := s.store.ItemWorkspace(ctx, itemID)
	if err != nil {
		return
	}
	s.publisher.Publish(events.Event{Type: events.TypeInbox, Workspace: ws})
}

func (s *Service) CreateItem(ctx context.Context, input CreateInput) (*Item, error) {
	input.Title = strings.TrimSpace(input.Title)
	input.AuthorAgentID = strings.TrimSpace(input.AuthorAgentID)
	input.WorkspaceID = strings.TrimSpace(input.WorkspaceID)
	if input.Title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if input.AuthorAgentID == "" {
		return nil, fmt.Errorf("%w: author_agent_id is required", ErrInvalidInput)
	}
	if input.WorkspaceID == "" {
		// Unfiled capture (docs/design/inbox.md, decision 13): rough ideas
		// precede the where-does-this-belong decision. Root-only — an
		// unfiled item is invisible to scoped keys, so a scoped key's own
		// capture would disappear on it.
		if !auth.PrincipalFromContext(ctx).IsRoot() {
			return nil, fmt.Errorf("%w: workspace_id is required for scoped keys (root may capture unfiled items)", ErrInvalidInput)
		}
	} else if err := auth.RequireWorkspace(ctx, input.WorkspaceID); err != nil {
		return nil, err
	}
	priority, err := resolvePriority(&input.Priority, nil)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	item := Item{
		ID:            ids.New(),
		WorkspaceID:   input.WorkspaceID,
		Title:         input.Title,
		Content:       strings.TrimSpace(input.Content),
		Tags:          NormalizeTags(input.Tags),
		Status:        StatusOpen,
		Priority:      priority,
		AuthorAgentID: input.AuthorAgentID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.CreateItem(ctx, item); err != nil {
		return nil, err
	}
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: events.TypeInbox, Workspace: item.WorkspaceID})
	}
	return &item, nil
}

// requireItemScopeQuiet authorizes a by-id operation: foreign items return the
// same not-found error as nonexistent ones — no existence or ownership oracle.
func (s *Service) requireItemScopeQuiet(ctx context.Context, itemID string) error {
	if strings.TrimSpace(itemID) == "" {
		return fmt.Errorf("%w: item_id is required", ErrInvalidInput)
	}
	ws, err := s.store.ItemWorkspace(ctx, itemID)
	if err != nil {
		return err
	}
	if err := auth.RequireWorkspaceQuiet(ctx, ws); err != nil {
		return fmt.Errorf("%w: item %s", ErrNotFound, itemID)
	}
	return nil
}

func (s *Service) GetItem(ctx context.Context, id string) (*Item, error) {
	if err := s.requireItemScopeQuiet(ctx, id); err != nil {
		return nil, err
	}
	item, err := s.store.GetItem(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	item.ContentHTML = RenderMarkdown(item.Content)
	return item, nil
}

// ResolveByPriority turns a workspace + priority pair into the item it names —
// the by-number reference for agents ("claim item #2 in <workspace>"). The
// number addresses open/in_progress items (the renumbered set); a miss returns
// not-found with a pointer to inbox_list, since numbers shift on every reorder.
// The workspace is an explicit target: out-of-scope keeps the actionable 403
// flavor, unlike by-id lookups' uniform 404.
func (s *Service) ResolveByPriority(ctx context.Context, workspaceID string, priority int) (*Item, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return nil, fmt.Errorf("%w: workspace_id is required when addressing an item by priority", ErrInvalidInput)
	}
	if priority < 1 || priority > maxPriority {
		return nil, fmt.Errorf("%w: priority must be between 1 and %d", ErrInvalidInput, maxPriority)
	}
	if err := auth.RequireWorkspace(ctx, workspaceID); err != nil {
		return nil, err
	}
	items, err := s.store.ItemsByPriority(ctx, workspaceID, priority)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: no open or in-progress item with priority %d in this workspace — run inbox_list for current numbers (unprioritized items have no number)", ErrNotFound, priority)
	}
	if len(items) > 1 {
		return nil, fmt.Errorf("%w: %d items share priority %d — address them by item_id", ErrInvalidInput, len(items), priority)
	}
	return &items[0], nil
}

// ListItems returns rows for the dashboard and agents: an excerpt instead of
// the full content (detail is a GetItem away), plus the linked plan summary.
func (s *Service) ListItems(ctx context.Context, workspaceID, status, tag, query string) ([]Item, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	status = strings.TrimSpace(status)
	if status != "" && !ValidStatus(Status(status)) {
		return nil, fmt.Errorf("%w: invalid status %q. Use: open, in_progress, converted, done, or discarded", ErrInvalidInput, status)
	}
	tag = strings.ToLower(strings.TrimSpace(tag))
	if workspaceID != "" {
		if err := auth.RequireWorkspace(ctx, workspaceID); err != nil {
			return nil, err
		}
	}
	items, err := s.listAcrossScope(ctx, workspaceID, status, tag, strings.TrimSpace(query))
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].Excerpt = Excerpt(items[i].Content)
		items[i].Content = ""
	}
	return items, nil
}

// listAcrossScope reads across the caller's scope: an explicit workspace uses
// one query; a scoped key with no filter enumerates its workspaces so the
// unscoped read returns exactly the key's slice, never other tenants' data.
func (s *Service) listAcrossScope(ctx context.Context, workspaceID, status, tag, query string) ([]Item, error) {
	if workspaceID != "" || !auth.ScopedContext(ctx) {
		return s.store.ListItems(ctx, workspaceID, status, tag, query)
	}
	var merged []Item
	seen := map[string]bool{}
	for _, ws := range auth.PrincipalFromContext(ctx).WorkspaceList() {
		items, err := s.store.ListItems(ctx, ws, status, tag, query)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if !seen[it.ID] {
				seen[it.ID] = true
				merged = append(merged, it)
			}
		}
	}
	return merged, nil
}

// UpdateItem enriches an item (title/content/tags/priority) and files (or
// re-files) it when WorkspaceID is set. Only open and in_progress items are
// editable — converted/done/discarded are frozen, mirroring the plan freeze
// semantics.
func (s *Service) UpdateItem(ctx context.Context, itemID string, input UpdateInput) error {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	if input.Title != "" {
		input.Title = strings.TrimSpace(input.Title)
	}
	input.WorkspaceID = strings.TrimSpace(input.WorkspaceID)
	// Filing names an explicit target workspace: an out-of-scope target is
	// the actionable 403 flavor (the item itself was already scope-checked).
	if input.WorkspaceID != "" {
		if err := auth.RequireWorkspace(ctx, input.WorkspaceID); err != nil {
			return err
		}
	}
	var item *Item
	err := s.store.RunInTx(ctx, func(tx Store) error {
		var err error
		item, err = tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if !editable(item.Status) {
			return fmt.Errorf("%w: item is %s; only open and in-progress items can be edited", ErrFrozen, statusLabel(item.Status))
		}
		title, content, tags, workspace := item.Title, item.Content, item.Tags, item.WorkspaceID
		if input.Title != "" {
			title = input.Title
		}
		if input.Content != "" {
			content = strings.TrimSpace(input.Content)
		}
		if input.Tags != nil {
			tags = NormalizeTags(*input.Tags)
		}
		if input.WorkspaceID != "" {
			workspace = input.WorkspaceID
		}
		priority, err := resolvePriority(input.Priority, item.Priority)
		if err != nil {
			return err
		}
		return tx.UpdateItem(ctx, itemID, workspace, title, content, tagsJSON(tags), priority, s.now().UTC())
	})
	if err != nil {
		return err
	}
	s.publishItem(ctx, itemID)
	return nil
}

// Reorder assigns priorities 1..N to the listed ids, in order (the board's
// drag-and-drop renumbering). Every id must exist, be in scope, and be
// open/in_progress — priorities order actionable work.
func (s *Service) Reorder(ctx context.Context, input ReorderInput) error {
	if len(input.IDs) == 0 {
		return fmt.Errorf("%w: ids must list at least one item", ErrInvalidInput)
	}
	if len(input.IDs) > maxReorder {
		return fmt.Errorf("%w: reorder accepts at most %d ids", ErrInvalidInput, maxReorder)
	}
	seen := map[string]bool{}
	ws := ""
	err := s.store.RunInTx(ctx, func(tx Store) error {
		for _, id := range input.IDs {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				return fmt.Errorf("%w: ids must be unique non-empty item ids", ErrInvalidInput)
			}
			seen[id] = true
			item, err := tx.GetItem(ctx, id)
			if err != nil {
				return err
			}
			// Uniform not-found for foreign items (no existence oracle).
			if err := auth.RequireWorkspaceQuiet(ctx, item.WorkspaceID); err != nil {
				return fmt.Errorf("%w: item %s", ErrNotFound, id)
			}
			if !editable(item.Status) {
				return fmt.Errorf("%w: item %s is %s; only open and in-progress items can be reordered", ErrInvalidInput, id, statusLabel(item.Status))
			}
			// Reorder is a board operation: one workspace per call (the SSE
			// event is attributed to it). Unfiled items share "" — a
			// root-only board of their own.
			if ws == "" {
				ws = item.WorkspaceID
			} else if item.WorkspaceID != ws {
				return fmt.Errorf("%w: reorder spans multiple workspaces — reorder one board at a time", ErrInvalidInput)
			}
		}
		return tx.ReorderItems(ctx, input.IDs, s.now().UTC())
	})
	if err != nil {
		return err
	}
	if s.publisher != nil && ws != "" {
		s.publisher.Publish(events.Event{Type: events.TypeInbox, Workspace: ws})
	}
	return nil
}

// Claim moves an open item to in_progress under the claiming agent
// (compare-and-swap; a foreign claim on an in_progress item is a 409
// conflict). An empty agent_id releases an in_progress item back to open.
func (s *Service) Claim(ctx context.Context, itemID, agentID string) (*Item, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	itemID = strings.TrimSpace(itemID)
	agentID = strings.TrimSpace(agentID)
	if itemID == "" {
		return nil, fmt.Errorf("%w: item_id is required", ErrInvalidInput)
	}
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if agentID != "" {
			switch {
			case item.Status == StatusOpen:
				return tx.ClaimItem(ctx, itemID, agentID, s.now().UTC())
			case item.Status == StatusInProgress && item.ClaimedByAgentID == agentID:
				return nil // idempotent re-claim by the same agent
			case item.Status == StatusInProgress:
				return fmt.Errorf("%w: item is in progress (claimed by %q)", ErrClaimConflict, item.ClaimedByAgentID)
			default:
				return fmt.Errorf("%w: item is %s; only open items can be claimed", ErrInvalidInput, statusLabel(item.Status))
			}
		}
		// Release.
		switch item.Status {
		case StatusOpen:
			return nil // nothing to release
		case StatusInProgress:
			return tx.ReleaseItem(ctx, itemID, s.now().UTC())
		default:
			return fmt.Errorf("%w: only open and in-progress items can be claimed or released (item is %s)", ErrInvalidInput, statusLabel(item.Status))
		}
	})
	if err != nil {
		return nil, err
	}
	s.publishItem(ctx, itemID)
	return s.store.GetItem(ctx, itemID)
}

// Convert links an existing plan to the item and moves it to converted. The
// plan is authored separately with the plan tools; this only validates the
// link (plan exists, same workspace) and records it.
func (s *Service) Convert(ctx context.Context, itemID string, input ConvertInput) (*Item, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	itemID = strings.TrimSpace(itemID)
	planID := strings.TrimSpace(input.PlanID)
	if itemID == "" {
		return nil, fmt.Errorf("%w: item_id is required", ErrInvalidInput)
	}
	if planID == "" {
		return nil, fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.PlanID != "" || item.Status == StatusConverted || item.Status == StatusDone {
			return fmt.Errorf("%w (plan %s)", ErrAlreadyConverted, item.PlanID)
		}
		if item.Status == StatusDiscarded {
			return fmt.Errorf("%w: item is discarded", ErrInvalidInput)
		}
		if item.WorkspaceID == "" {
			return fmt.Errorf("%w: file the item into a workspace before converting", ErrInvalidInput)
		}
		planWS, err := tx.PlanWorkspace(ctx, planID)
		if err != nil {
			return err
		}
		// A foreign-workspace plan must be indistinguishable from a missing
		// one (no existence/location oracle across tenants); the mismatch
		// detail is reserved for plans the caller can already see.
		if err := auth.RequireWorkspaceQuiet(ctx, planWS); err != nil {
			return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
		}
		if planWS != item.WorkspaceID {
			return fmt.Errorf("%w: plan %s belongs to a different workspace than the item — convert within one workspace",
				ErrInvalidInput, planID)
		}
		return tx.ConvertItem(ctx, itemID, planID, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}
	s.publishItem(ctx, itemID)
	return s.store.GetItem(ctx, itemID)
}

// Restore brings a discarded item back to open — the undo for an
// accidental discard (a real risk now that the board supports drag-to-
// discarded). Done stays terminal: it is derived from the linked plan.
func (s *Service) Restore(ctx context.Context, itemID string) error {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.Status != StatusDiscarded {
			return fmt.Errorf("%w: only discarded items can be restored (item is %s)", ErrInvalidInput, statusLabel(item.Status))
		}
		return tx.RestoreItem(ctx, itemID, s.now().UTC())
	})
	if err != nil {
		return err
	}
	s.publishItem(ctx, itemID)
	return nil
}

// Discard retires an item without a plan (won't do / superseded). Done items
// are terminal.
func (s *Service) Discard(ctx context.Context, itemID string) error {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		switch item.Status {
		case StatusDone, StatusDiscarded:
			return fmt.Errorf("%w: item is %s (terminal)", ErrInvalidInput, statusLabel(item.Status))
		default:
			return tx.SetStatus(ctx, itemID, StatusDiscarded, s.now().UTC())
		}
	})
	if err != nil {
		return err
	}
	s.publishItem(ctx, itemID)
	return nil
}

// Complete manually marks an item done — the operator path for work that
// finished without a plan, or ahead of it. Allowed from open, in_progress,
// and converted (the plan-completion hook remains the automatic route for
// converted items; completing early is safe — the later hook flip targets
// status='converted' and no-ops). Done and discarded are terminal: restore
// a discarded item first. The linked plan, if any, is untouched.
func (s *Service) Complete(ctx context.Context, itemID string) error {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		switch item.Status {
		case StatusDone:
			return fmt.Errorf("%w: item is already done", ErrInvalidInput)
		case StatusDiscarded:
			return fmt.Errorf("%w: item is discarded — restore it before completing", ErrInvalidInput)
		default:
			return tx.SetStatus(ctx, itemID, StatusDone, s.now().UTC())
		}
	})
	if err != nil {
		return err
	}
	s.publishItem(ctx, itemID)
	return nil
}

// Reopen brings a done item back to open — the undo for a wrong manual
// complete (done is no longer strictly terminal for humans; the automatic
// plan-completion flip still never reopens anything). Fresh cycle: claim,
// plan link, and priority are cleared, so the item re-enters the actionable
// pool unclaimed and can be re-converted.
func (s *Service) Reopen(ctx context.Context, itemID string) error {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.Status != StatusDone {
			return fmt.Errorf("%w: only done items can be reopened (item is %s)", ErrInvalidInput, statusLabel(item.Status))
		}
		return tx.ReopenItem(ctx, itemID, s.now().UTC())
	})
	if err != nil {
		return err
	}
	s.publishItem(ctx, itemID)
	return nil
}

// Purge bulk-deletes items in one workspace — every status, or one status
// when set (the board's per-lane "clear"). The workspace is required for
// every principal, root included; there is deliberately no cross-workspace
// variant, and unfiled items (NULL workspace) are never matched. Returns
// the number of items deleted.
func (s *Service) Purge(ctx context.Context, workspaceID string, status Status) (int, error) {
	workspaceID = strings.TrimSpace(workspaceID)
	status = Status(strings.TrimSpace(string(status)))
	if workspaceID == "" {
		return 0, fmt.Errorf("%w: workspace_id is required (derive it with `skopos workspace` or the git remote)", ErrInvalidInput)
	}
	if status != "" && !ValidStatus(status) {
		return 0, fmt.Errorf("%w: invalid status %q. Use: open, in_progress, converted, done, or discarded", ErrInvalidInput, status)
	}
	if err := auth.RequireWorkspace(ctx, workspaceID); err != nil {
		return 0, err
	}
	n, err := s.store.DeleteByFilter(ctx, workspaceID, status)
	if err != nil {
		return 0, err
	}
	if s.publisher != nil && n > 0 {
		s.publisher.Publish(events.Event{Type: events.TypeInbox, Workspace: workspaceID})
	}
	return int(n), nil
}

func (s *Service) DeleteItem(ctx context.Context, itemID string) error {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return err
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return fmt.Errorf("%w: item_id is required", ErrInvalidInput)
	}
	if err := s.store.DeleteItem(ctx, itemID); err != nil {
		return err
	}
	s.publishItem(ctx, itemID)
	return nil
}

// CompleteForPlan flips every converted item linked to planID to done. It is
// wired to the plans completion hook (background context, system principal)
// and is best-effort: completion of the plan itself must never fail because
// an inbox update did.
func (s *Service) CompleteForPlan(ctx context.Context, planID string) (int64, error) {
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return 0, fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	n, err := s.store.CompleteForPlan(ctx, planID, s.now().UTC())
	if err != nil {
		return n, err
	}
	if n > 0 && s.publisher != nil {
		// Attribute to the plan's workspace; a deleted plan leaves the event
		// unattributed (root/internal subscribers only) rather than wrong.
		ws, wsErr := s.store.PlanWorkspace(ctx, planID)
		if wsErr != nil {
			ws = ""
		}
		s.publisher.Publish(events.Event{Type: events.TypeInbox, Workspace: ws})
	}
	return n, nil
}

func editable(status Status) bool {
	return status == StatusOpen || status == StatusInProgress
}

// statusLabel renders a status for human-facing error messages ("in
// progress"); messages that enumerate valid INPUT values keep the raw
// slugs, since that is what callers must type.
func statusLabel(s Status) string {
	if s == StatusInProgress {
		return "in progress"
	}
	return string(s)
}

func ValidStatus(s Status) bool {
	switch s {
	case StatusOpen, StatusInProgress, StatusConverted, StatusDone, StatusDiscarded:
		return true
	}
	return false
}

// Tag normalization (docs/design/inbox.md, decision 7): lowercase, charset
// [a-z0-9][a-z0-9._/-], <= 40 chars, deduped, capped at 10 tags. Invalid
// tokens are dropped, never rejected — tags are advisory metadata.
var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*$`)

const (
	maxTags    = 10
	maxTagLen  = 40
	excerptMax = 200
	maxPriority = 100000
	maxReorder  = 500
)

// resolvePriority validates a create/update priority value. Create: 0 means
// unprioritized; update: 0 clears, >= 1 sets, nil keeps the current value.
func resolvePriority(in *int, current *int) (*int, error) {
	if in == nil {
		return current, nil
	}
	if *in == 0 {
		return nil, nil
	}
	if *in < 0 || *in > maxPriority {
		return nil, fmt.Errorf("%w: priority must be between 1 and %d (0 clears)", ErrInvalidInput, maxPriority)
	}
	return in, nil
}

func NormalizeTags(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range in {
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" || len(t) > maxTagLen || !tagPattern.MatchString(t) {
			continue
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) == maxTags {
			break
		}
	}
	return out
}

// Excerpt collapses content to a single-line plain-text preview for list rows.
func Excerpt(content string) string {
	flat := strings.Join(strings.Fields(content), " ")
	r := []rune(flat)
	if len(r) <= excerptMax {
		return flat
	}
	return string(r[:excerptMax]) + "…"
}
