package audit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
)

// Service writes and reads the audit log. Recording derives the actor from
// the request principal — callers cannot fabricate one — and is best-effort
// by convention: an audit failure is logged, never blocks the mutation that
// produced it.
type Service struct {
	storage *Storage
	now     func() time.Time
}

func NewService(storage *Storage) *Service {
	return &Service{storage: storage, now: time.Now}
}

// Record appends one entry, rendering the actor from the principal:
//
//	root              the root key (or auth disabled)
//	key <name>        a scoped API key (actor_key_id kept alongside)
//	agent <id>        an agent-driven mutation (input.AgentID)
//	system            an internal/background caller (nil principal)
//	migration         via == ViaMigration
func (s *Service) Record(ctx context.Context, input RecordInput) error {
	input.EntityType = strings.TrimSpace(input.EntityType)
	input.EntityID = strings.TrimSpace(input.EntityID)
	input.Action = strings.TrimSpace(input.Action)
	if input.EntityType == "" || input.EntityID == "" || input.Action == "" {
		return fmt.Errorf("%w: entity_type, entity_id and action are required", ErrInvalidInput)
	}
	p := auth.PrincipalFromContext(ctx)
	e := Entry{
		ID:          ids.New(),
		WorkspaceID: strings.TrimSpace(input.WorkspaceID),
		EntityType:  input.EntityType,
		EntityID:    input.EntityID,
		Action:      input.Action,
		Via:         sanitizeVia(input.Via, p),
		Notes:       input.Notes,
		CreatedAt:   s.now().UTC(),
	}
	switch {
	case p == nil && e.Via == ViaMigration:
		e.Actor = ViaMigration
	case input.AgentID != "":
		// Agent attribution wins for any principal (the trial's agents run
		// under the root key — "root" would hide which agent acted).
		e.Actor = "agent " + input.AgentID
		if p != nil && !p.Root {
			e.ActorKeyID = p.KeyID
		}
	case p == nil:
		e.Actor = "system"
	case p.Root:
		e.Actor = "root"
	default:
		e.Actor = "key " + p.Name
		// Always kept (review fix 8): the raw key id is the attribution the
		// threat model leans on, whatever the via label says.
		e.ActorKeyID = p.KeyID
	}
	return s.storage.Write(ctx, e)
}

// validVia is the closed set callers may name (review fix 8): migration is
// reserved for internal callers and system for the server itself.
func validVia(v string) bool {
	switch v {
	case ViaDashboard, ViaSlack, ViaCLI, ViaMCP, ViaWorker, ViaSystem, "":
		return true
	}
	return false
}

// sanitizeVia validates and rights the label: unknown labels drop to empty
// (never trusted), migration is stripped from caller-supplied values, and
// slack requires an approver principal — the threat model's "via: slack"
// means Martin himself.
func sanitizeVia(via string, p *auth.Principal) string {
	via = strings.TrimSpace(via)
	if via == ViaMigration {
		if p == nil {
			return via // internal caller: the one legitimate migration writer
		}
		return ""
	}
	if !validVia(via) {
		return ""
	}
	if via == ViaSlack && p != nil && !p.Root && !p.Approver {
		return ""
	}
	return via
}

// List reads entries, scoped like every domain read: root (and internal)
// callers see everything; scoped keys see exactly their workspaces' entries.
// Server-level entries (empty workspace_id) are root-only.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Entry, error) {
	if auth.ScopedContext(ctx) {
		p := auth.PrincipalFromContext(ctx)
		if f.WorkspaceID != "" {
			// A scoped caller naming a foreign workspace gets the same empty
			// page as a nonexistent one (no existence oracle).
			if !p.CanAccess(f.WorkspaceID) {
				return []Entry{}, nil
			}
			return s.storage.List(ctx, f)
		}
		out := []Entry{}
		for _, ws := range p.WorkspaceList() {
			entries, err := s.storage.List(ctx, ListFilter{WorkspaceID: ws, EntityType: f.EntityType, EntityID: f.EntityID, ActionPrefix: f.ActionPrefix, Before: f.Before, Limit: f.Limit})
			if err != nil {
				return nil, err
			}
			out = append(out, entries...)
		}
		// Each per-workspace page is newest-first; the merge must be too
		// (UUIDv7 ids sort by time).
		sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
		return out, nil
	}
	return s.storage.List(ctx, f)
}

// Timeline is the filtered view one entity's history: newest-first entries
// for (entity_type, entity_id), scoped to the caller's access of the entry's
// workspace. The dashboard item timeline (agent-pipeline §1) is this call
// with entity_type "inbox_item".
func (s *Service) Timeline(ctx context.Context, entityType, entityID string) ([]Entry, error) {
	return s.List(ctx, ListFilter{EntityType: entityType, EntityID: entityID})
}
