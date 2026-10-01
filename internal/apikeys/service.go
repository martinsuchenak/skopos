package apikeys

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
)

var (
	ErrInvalidInput = errors.New("invalid api keys input")
	ErrNotFound     = errors.New("not found")
)

type Service struct {
	storage  *Storage
	now      func() time.Time
	onRevoke func(keyID string)
	onScope  func(keyIDs []string)
}

func NewService(storage *Storage) *Service {
	return &Service{storage: storage, now: time.Now}
}

// SetRevocationNotifier installs a callback invoked whenever a key actually
// transitions to revoked — wired to the event hub to terminate the key's
// open SSE streams.
func (s *Service) SetRevocationNotifier(fn func(keyID string)) { s.onRevoke = fn }

// SetScopeNotifier installs a callback invoked whenever a key's effective
// scope changes without the key itself being revoked (its groups or their
// members/patterns changed) — wired to the event hub so the affected keys'
// open SSE streams terminate and reconnect with the new scope
// (docs/design/agent-pipeline.md §3).
func (s *Service) SetScopeNotifier(fn func(keyIDs []string)) { s.onScope = fn }

func (s *Service) dropKeys(keyIDs []string) {
	if s.onScope != nil && len(keyIDs) > 0 {
		s.onScope(keyIDs)
	}
}

// OnWorkspaceRegistered reacts to a registry entry: keys holding a group
// whose pattern matches the new id gain access, so their streams must
// re-evaluate. Wired in serve.go to the workspaces service's create notifier.
func (s *Service) OnWorkspaceRegistered(ctx context.Context, wsID string) {
	keyIDs, err := s.storage.KeysInGroupsMatching(ctx, wsID)
	if err != nil {
		return // best-effort: streams re-evaluate scope on their next reconnect anyway
	}
	s.dropKeys(keyIDs)
}

type CreateInput struct {
	Name          string
	Workspaces    []string // exact ids, or a single "*" for all workspaces
	Groups        []string // group names or ids (docs/design/agent-pipeline.md §3)
	AllWorkspaces bool
	Approver      bool // may perform human-only workflow actions (agent-pipeline §2); root-only to grant
}

// resolveGroups turns group refs (id first, then unique name) into rows,
// rejecting unknown refs. The returned rows carry both the storage id and the
// display name.
func (s *Service) resolveGroups(ctx context.Context, refs []string) ([]Group, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	out := make([]Group, 0, len(refs))
	seen := map[string]bool{}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		g, err := s.storage.ResolveGroupRef(ctx, ref)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, fmt.Errorf("%w: unknown group %q", ErrInvalidInput, ref)
			}
			return nil, err
		}
		if seen[g.ID] {
			continue
		}
		seen[g.ID] = true
		out = append(out, g)
	}
	return out, nil
}

// Create mints a key. The plaintext secret is returned once and never stored.
func (s *Service) Create(ctx context.Context, input CreateInput) (*CreateResult, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if len(input.Name) > 100 {
		return nil, fmt.Errorf("%w: name must be at most 100 characters", ErrInvalidInput)
	}

	workspaces := make([]string, 0, len(input.Workspaces))
	for _, w := range input.Workspaces {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if w == "*" {
			input.AllWorkspaces = true
			continue
		}
		workspaces = append(workspaces, w)
	}
	if input.AllWorkspaces && len(workspaces) > 0 {
		return nil, fmt.Errorf("%w: workspaces \"*\" cannot be combined with an explicit list", ErrInvalidInput)
	}
	groups, err := s.resolveGroups(ctx, input.Groups)
	if err != nil {
		return nil, err
	}
	groupIDs := make([]string, 0, len(groups))
	groupNames := make([]string, 0, len(groups))
	for _, g := range groups {
		groupIDs = append(groupIDs, g.ID)
		groupNames = append(groupNames, g.Name)
	}
	if input.AllWorkspaces && len(groupIDs) > 0 {
		return nil, fmt.Errorf("%w: groups cannot be combined with \"*\" (all workspaces)", ErrInvalidInput)
	}
	if !input.AllWorkspaces && len(workspaces) == 0 && len(groupIDs) == 0 {
		return nil, fmt.Errorf("%w: workspaces must be a list of workspace ids, group names, or \"*\"", ErrInvalidInput)
	}
	seen := map[string]bool{}
	unique := workspaces[:0]
	for _, w := range workspaces {
		if seen[w] {
			continue
		}
		seen[w] = true
		unique = append(unique, w)
	}
	workspaces = unique
	for _, w := range workspaces {
		exists, err := s.storage.WorkspaceExists(ctx, w)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("%w: workspace %s is not registered (register it first, e.g. via a session report or POST /api/workspaces)", ErrInvalidInput, w)
		}
	}

	secret, err := GenerateSecret()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	key := Key{
		ID:            ids.New(),
		Name:          input.Name,
		Prefix:        secret[:14],
		AllWorkspaces: input.AllWorkspaces,
		Approver:      input.Approver,
		Workspaces:    workspaces,
		Groups:        groupNames,
		CreatedAt:     now,
	}
	if err := s.storage.Write(ctx, key, auth.HashKey(secret), groupIDs); err != nil {
		return nil, err
	}
	return &CreateResult{Key: key, Secret: secret}, nil
}

// List returns every key including revoked ones (audit trail), newest first.
func (s *Service) List(ctx context.Context) ([]Key, error) {
	return s.storage.List(ctx)
}

// Revoke soft-deletes a key; revoking an already-revoked key is a no-op.
// A successful transition fires the revocation notifier (the event hub
// terminates the key's open SSE streams).
func (s *Service) Revoke(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	revoked, err := s.storage.Revoke(ctx, id)
	if err != nil {
		return err
	}
	if revoked && s.onRevoke != nil {
		s.onRevoke(id)
	}
	return nil
}

// UpdateInput changes a key's name and/or scope. Nil fields are left
// unchanged (partial PATCH semantics).
type UpdateInput struct {
	Name       *string
	Workspaces []string // nil = unchanged; ["*"] or explicit ids otherwise
	Groups     []string // nil = unchanged; names or ids, [] clears
	Approver   *bool    // nil = unchanged
}

// Update edits a key's name and/or workspace scope. Revoked keys are frozen —
// re-mint instead. Scope changes take effect on the next request (lookups are
// not cached) and terminate the key's open SSE streams.
func (s *Service) Update(ctx context.Context, id string, input UpdateInput) (*Key, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	existing, err := s.storage.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if existing.RevokedAt != nil {
		return nil, fmt.Errorf("%w: key is revoked; mint a new key instead", ErrInvalidInput)
	}

	name := existing.Name
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: name is required", ErrInvalidInput)
		}
		if len(name) > 100 {
			return nil, fmt.Errorf("%w: name must be at most 100 characters", ErrInvalidInput)
		}
	}
	all := existing.AllWorkspaces
	workspaces := existing.Workspaces
	if input.Workspaces != nil {
		all = false
		workspaces = make([]string, 0, len(input.Workspaces))
		for _, w := range input.Workspaces {
			w = strings.TrimSpace(w)
			if w == "" {
				continue
			}
			if w == "*" {
				all = true
				continue
			}
			workspaces = append(workspaces, w)
		}
		if all && len(workspaces) > 0 {
			return nil, fmt.Errorf("%w: workspaces \"*\" cannot be combined with an explicit list", ErrInvalidInput)
		}
		seen := map[string]bool{}
		unique := workspaces[:0]
		for _, w := range workspaces {
			if seen[w] {
				continue
			}
			seen[w] = true
			unique = append(unique, w)
		}
		workspaces = unique
		for _, w := range workspaces {
			exists, err := s.storage.WorkspaceExists(ctx, w)
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, fmt.Errorf("%w: workspace %s is not registered (register it first, e.g. via a session report or POST /api/workspaces)", ErrInvalidInput, w)
			}
		}
	}
	var groupIDs *[]string
	if input.Groups != nil {
		groups, err := s.resolveGroups(ctx, input.Groups)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(groups))
		for _, g := range groups {
			ids = append(ids, g.ID)
		}
		groupIDs = &ids
		if all && len(ids) > 0 {
			return nil, fmt.Errorf("%w: groups cannot be combined with \"*\" (all workspaces)", ErrInvalidInput)
		}
	}
	// The key must keep some scope: explicit workspaces, groups (updated or
	// carried over), or the "*" scope.
	effectiveGroups := len(existing.Groups)
	if groupIDs != nil {
		effectiveGroups = len(*groupIDs)
	}
	if !all && len(workspaces) == 0 && effectiveGroups == 0 && (input.Workspaces != nil || input.Groups != nil) {
		return nil, fmt.Errorf("%w: workspaces must be a list of workspace ids, group names, or \"*\"", ErrInvalidInput)
	}
	approver := existing.Approver
	if input.Approver != nil {
		approver = *input.Approver
	}
	if err := s.storage.Update(ctx, id, name, all, approver, workspaces, groupIDs); err != nil {
		return nil, err
	}
	// A scope change re-shapes what the key's open SSE streams may see.
	if input.Workspaces != nil || groupIDs != nil {
		s.dropKeys([]string{id})
	}
	updated, err := s.storage.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// Delete hard-deletes a key (audit trail included) — for cleaning up old or
// revoked keys. Active keys should usually be revoked first so their SSE
// streams terminate; hard-deleting an active key leaves its streams open
// until they reconnect.
func (s *Service) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.storage.Delete(ctx, id)
}

// GenerateSecret produces a key like "sk_" + 43 base64url chars (256 bits).
// Used for scoped keys and for suggesting a root key (skopos key
// generate-root) — the server treats whatever sits in auth.api_key as root.
func GenerateSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating key material: %w", err)
	}
	return "sk_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// --- workspace groups (docs/design/agent-pipeline.md §3) ---

// GroupInput creates or replaces a group's fields. Members must be registered
// workspaces; patterns are Go path.Match (`*` never crosses `/`, no `**`).
type GroupInput struct {
	Name        string
	Description string
	Members     []string
	Patterns    []string
}

// GroupUpdateInput patches a group: nil lists leave members/patterns unchanged.
type GroupUpdateInput struct {
	Name        *string
	Description *string
	Members     []string
	Patterns    []string
}

func validateGroupName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: group name is required", ErrInvalidInput)
	}
	if len(name) > 100 {
		return fmt.Errorf("%w: group name must be at most 100 characters", ErrInvalidInput)
	}
	return nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// validateMembers checks that every explicit member is a registered workspace.
func (s *Service) validateMembers(ctx context.Context, members []string) error {
	for _, w := range members {
		exists, err := s.storage.WorkspaceExists(ctx, w)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%w: workspace %s is not registered (register it first, e.g. via a session report or POST /api/workspaces)", ErrInvalidInput, w)
		}
	}
	return nil
}

// validatePatterns checks path.Match patterns: non-empty, well-formed, no
// `**` (there is no multi-segment wildcard — the design deliberately keeps
// path.Match semantics).
func validatePatterns(patterns []string) error {
	for _, p := range patterns {
		if p == "" {
			return fmt.Errorf("%w: patterns must be non-empty", ErrInvalidInput)
		}
		if strings.Contains(p, "**") {
			return fmt.Errorf("%w: pattern %q uses **, which is not supported (path.Match: * matches one segment and never crosses /)", ErrInvalidInput, p)
		}
		if _, err := path.Match(p, "probe"); err != nil {
			return fmt.Errorf("%w: invalid pattern %q (%s)", ErrInvalidInput, p, err)
		}
	}
	return nil
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return dedupe(out)
}

// CreateGroup adds a group. Group management is root-only, enforced here so
// no surface (REST, MCP, background) can bypass it.
func (s *Service) CreateGroup(ctx context.Context, input GroupInput) (*Group, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if err := validateGroupName(input.Name); err != nil {
		return nil, err
	}
	members := cleanList(input.Members)
	patterns := cleanList(input.Patterns)
	if len(members) == 0 && len(patterns) == 0 {
		return nil, fmt.Errorf("%w: a group needs at least one member or pattern", ErrInvalidInput)
	}
	if err := s.validateMembers(ctx, members); err != nil {
		return nil, err
	}
	if err := validatePatterns(patterns); err != nil {
		return nil, err
	}
	g := Group{
		ID:          ids.New(),
		Name:        input.Name,
		Description: input.Description,
		Members:     members,
		Patterns:    patterns,
		CreatedAt:   s.now().UTC(),
	}
	if err := s.storage.WriteGroup(ctx, g); err != nil {
		if errors.Is(err, ErrDuplicateName) {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateName, input.Name)
		}
		return nil, err
	}
	return &g, nil
}

// ListGroups returns every group.
func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	groups, err := s.storage.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	if groups == nil {
		groups = []Group{}
	}
	return groups, nil
}

// GetGroup returns one group by id.
func (s *Service) GetGroup(ctx context.Context, id string) (*Group, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	g, err := s.storage.GetGroup(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// UpdateGroup patches a group's fields; member or pattern changes terminate
// the SSE streams of every key holding the group.
func (s *Service) UpdateGroup(ctx context.Context, id string, input GroupUpdateInput) (*Group, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	existing, err := s.storage.GetGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	name := existing.Name
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
		if err := validateGroupName(name); err != nil {
			return nil, err
		}
	}
	description := existing.Description
	if input.Description != nil {
		description = strings.TrimSpace(*input.Description)
	}
	members := existing.Members
	var memberPtr *[]string
	if input.Members != nil {
		members = cleanList(input.Members)
		memberPtr = &members
	}
	patterns := existing.Patterns
	var patternPtr *[]string
	if input.Patterns != nil {
		patterns = cleanList(input.Patterns)
		patternPtr = &patterns
	}
	if len(members) == 0 && len(patterns) == 0 {
		return nil, fmt.Errorf("%w: a group needs at least one member or pattern", ErrInvalidInput)
	}
	if err := s.validateMembers(ctx, members); err != nil {
		return nil, err
	}
	if err := validatePatterns(patterns); err != nil {
		return nil, err
	}
	updated := existing
	updated.Name = name
	updated.Description = description
	if err := s.storage.UpdateGroup(ctx, updated, memberPtr, patternPtr); err != nil {
		if errors.Is(err, ErrDuplicateName) {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateName, name)
		}
		return nil, err
	}
	// Scope widened or narrowed for every key holding the group.
	if memberPtr != nil || patternPtr != nil {
		keyIDs, err := s.storage.KeysInGroup(ctx, id)
		if err == nil {
			s.dropKeys(keyIDs)
		}
	}
	g, err := s.storage.GetGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// DeleteGroup removes a group; keys holding it lose that slice of scope, so
// their streams terminate and reconnect.
func (s *Service) DeleteGroup(ctx context.Context, id string) error {
	if err := auth.RequireRoot(ctx); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	keyIDs, err := s.storage.KeysInGroup(ctx, id)
	if err != nil {
		return err
	}
	if err := s.storage.DeleteGroup(ctx, id); err != nil {
		return err
	}
	s.dropKeys(keyIDs)
	return nil
}

// WhoCan answers the reverse query: which active keys reach wsID, and how
// (explicit scope, group member, or group pattern match).
func (s *Service) WhoCan(ctx context.Context, wsID string) ([]KeyReach, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	wsID = strings.TrimSpace(wsID)
	if wsID == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrInvalidInput)
	}
	reach, err := s.storage.KeysReachingWorkspace(ctx, wsID)
	if err != nil {
		return nil, err
	}
	if reach == nil {
		reach = []KeyReach{}
	}
	return reach, nil
}

// WorkspaceGroups lists the groups holding wsID as an explicit member
// (root-only, like every group read).
func (s *Service) WorkspaceGroups(ctx context.Context, wsID string) ([]Group, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	wsID = strings.TrimSpace(wsID)
	if wsID == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrInvalidInput)
	}
	groups, err := s.storage.GroupsOfWorkspace(ctx, wsID)
	if err != nil {
		return nil, err
	}
	if groups == nil {
		groups = []Group{}
	}
	return groups, nil
}

// SetWorkspaceGroups replaces a workspace's explicit group allocations —
// the workspace-centric counterpart of editing a group's members. Keys in
// any touched group (joined or left) re-evaluate their SSE scope.
func (s *Service) SetWorkspaceGroups(ctx context.Context, wsID string, refs []string) ([]Group, error) {
	if err := auth.RequireRoot(ctx); err != nil {
		return nil, err
	}
	wsID = strings.TrimSpace(wsID)
	if wsID == "" {
		return nil, fmt.Errorf("%w: workspace is required", ErrInvalidInput)
	}
	exists, err := s.storage.WorkspaceExists(ctx, wsID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: workspace %s is not registered", ErrInvalidInput, wsID)
	}
	before, err := s.storage.GroupIDsOfWorkspace(ctx, wsID)
	if err != nil {
		return nil, err
	}
	groups, err := s.resolveGroups(ctx, refs)
	if err != nil {
		return nil, err
	}
	after := make([]string, 0, len(groups))
	for _, g := range groups {
		after = append(after, g.ID)
	}
	if err := s.storage.SetWorkspaceGroups(ctx, wsID, after); err != nil {
		return nil, err
	}
	// Scope changed for keys holding any touched group (before ∪ after).
	touched := map[string]struct{}{}
	for _, gid := range before {
		touched[gid] = struct{}{}
	}
	for _, gid := range after {
		touched[gid] = struct{}{}
	}
	drop := map[string]struct{}{}
	for gid := range touched {
		keyIDs, err := s.storage.KeysInGroup(ctx, gid)
		if err == nil {
			for _, id := range keyIDs {
				drop[id] = struct{}{}
			}
		}
	}
	list := make([]string, 0, len(drop))
	for id := range drop {
		list = append(list, id)
	}
	s.dropKeys(list)
	return s.storage.GroupsOfWorkspace(ctx, wsID)
}

// ResolveScope explains a key's effective scope for whoami: the groups it
// holds and how each reachable workspace is reached. Pattern matches are
// evaluated against the current registry.
func (s *Service) ResolveScope(ctx context.Context, keyID string) (*ScopeResolution, error) {
	groups, err := s.storage.KeyGroups(ctx, keyID)
	if err != nil {
		return nil, err
	}
	key, err := s.storage.Get(ctx, keyID)
	if err != nil {
		return nil, err
	}
	res := &ScopeResolution{Groups: groups, Via: map[string]string{}}
	for _, ws := range key.Workspaces {
		res.Via[ws] = "explicit"
	}
	var registered []string
	for _, g := range groups {
		for _, ws := range g.Members {
			if _, ok := res.Via[ws]; !ok {
				res.Via[ws] = "group:" + g.Name
			}
		}
		if len(g.Patterns) > 0 {
			if registered == nil {
				if registered, err = s.storage.RegisteredWorkspaces(ctx); err != nil {
					return nil, err
				}
			}
			for _, pat := range g.Patterns {
				for _, ws := range registered {
					if ok, err := path.Match(pat, ws); err == nil && ok {
						if _, seen := res.Via[ws]; !seen {
							res.Via[ws] = "pattern:" + g.Name + ":" + pat
						}
					}
				}
			}
		}
	}
	if res.Groups == nil {
		res.Groups = []Group{}
	}
	return res, nil
}
