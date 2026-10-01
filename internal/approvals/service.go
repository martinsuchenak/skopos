package approvals

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
)

// Service records and reads approvals. Writes derive the actor from the
// request principal (same rendering as the audit log) and are performed by
// the workflow actions in the inbox service — never by agents.
type Service struct {
	storage *Storage
	now     func() time.Time
}

func NewService(storage *Storage) *Service {
	return &Service{storage: storage, now: time.Now}
}

// Record appends one decision. Actor rendering mirrors internal/audit:
// root, key <name> (+actor_key_id), or system for internal callers.
func (s *Service) Record(ctx context.Context, input RecordInput) (Entry, error) {
	input.ItemID = strings.TrimSpace(input.ItemID)
	input.WorkspaceID = strings.TrimSpace(input.WorkspaceID)
	input.Subject = strings.TrimSpace(input.Subject)
	input.Notes = strings.TrimSpace(input.Notes)
	if input.ItemID == "" || input.WorkspaceID == "" {
		return Entry{}, fmt.Errorf("invalid approval input: item and workspace are required")
	}
	switch input.Gate {
	case GatePlan, GateReview:
	default:
		return Entry{}, fmt.Errorf("invalid approval input: gate must be plan or review")
	}
	switch input.Decision {
	case DecisionApproved, DecisionRejected, DecisionChangesRequested:
	default:
		return Entry{}, fmt.Errorf("invalid approval input: unknown decision %q", input.Decision)
	}
	if input.Gate == GatePlan && input.Subject == "" {
		return Entry{}, fmt.Errorf("invalid approval input: a plan approval must name its subject (revision + base)")
	}
	p := auth.PrincipalFromContext(ctx)
	e := Entry{
		ID:          ids.New(),
		WorkspaceID: input.WorkspaceID,
		ItemID:      input.ItemID,
		Gate:        input.Gate,
		Subject:     input.Subject,
		Decision:    input.Decision,
		Via:         strings.TrimSpace(input.Via),
		Notes:       input.Notes,
		CreatedAt:   s.now().UTC(),
	}
	switch {
	case p == nil:
		e.Actor = "system"
	case p.Root:
		e.Actor = "root"
	default:
		e.Actor = "key " + p.Name
		e.ActorKeyID = p.KeyID
	}
	if err := s.storage.Write(ctx, e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// List returns an item's decisions, newest-first, scoped like every domain
// read (foreign items: empty page, same as missing).
func (s *Service) List(ctx context.Context, itemID string) ([]Entry, error) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil, fmt.Errorf("invalid approval input: item is required")
	}
	if auth.ScopedContext(ctx) {
		// Entries carry their workspace; the caller sees exactly their slice.
		entries, err := s.storage.List(ctx, itemID)
		if err != nil {
			return nil, err
		}
		p := auth.PrincipalFromContext(ctx)
		out := make([]Entry, 0, len(entries))
		for _, e := range entries {
			if p.CanAccess(e.WorkspaceID) {
				out = append(out, e)
			}
		}
		return out, nil
	}
	entries, err := s.storage.List(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []Entry{}
	}
	return entries, nil
}

// LatestApproved exposes the newest approval for one gate (nil when none) so
// callers can compare the recorded subject against the current head.
func (s *Service) LatestApproved(ctx context.Context, itemID, gate string) (*Entry, error) {
	return s.storage.LatestApproved(ctx, strings.TrimSpace(itemID), gate)
}
