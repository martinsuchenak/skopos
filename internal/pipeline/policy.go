package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/policy"
)

// Runner policy (tmp/central-config — design.md, phase 1a): which runners
// may work on which workspaces. A policy is a list of allow rules
// (internal/policy), stored as revisions (internal/confrev) at four levels:
//
//	fleet ceiling   always applied; nothing in the fleet may use a runner
//	                it does not allow
//	fleet default   for a workspace with no policy of its own and in no
//	                group that has one
//	group           every workspace in the group
//	workspace       that workspace
//
// A workspace's effective policy is the ceiling, then its groups' and its
// own policies — or the default when it has none of those. A runner must
// pass every one, so each level can only narrow. Changing a policy needs
// the admin permission at that level (auth.RequireAdmin*).

// Policy owner kinds, beside KindWorkspace and KindGroup.
const (
	KindFleetCeiling = "fleet-ceiling"
	KindFleetDefault = "fleet-default"
)

// PolicyAuditEntity is the audit-log entity type of policy changes; the
// entity id is the owner ("fleet:ceiling", "group:<id>", "workspace:<id>").
const PolicyAuditEntity = "pipeline_policy"

// ErrStrands: the policy would leave configured runners with no worker
// allowed to run them (or stop a workspace being embedded). Saved only
// with force.
var ErrStrands = errors.New("the policy would strand current settings")

// SetRevisions gives the service the revision store policies live in.
// Without it no policy applies (and none can be saved).
func (s *Service) SetRevisions(store *confrev.Store) { s.revs = store }

// EmbeddingsCheck reports whether a workspace has embedded symbols and
// which provider its embeddings endpoint is ("" = embeddings are off): a
// policy that stops a workspace being embedded drops those vectors, so
// saving it needs force.
type EmbeddingsCheck func(ctx context.Context, ws string) (embedded bool, facts policy.Facts)

// SetEmbeddingsCheck installs the embeddings lookup.
func (s *Service) SetEmbeddingsCheck(fn EmbeddingsCheck) { s.embeddings = fn }

// SetPolicyChanged installs a callback run after every policy change with
// the workspaces it reaches (the embedder re-checks them: a workspace the
// policy now keeps from the embeddings endpoint loses its vectors, one it
// now allows is embedded).
func (s *Service) SetPolicyChanged(fn func(workspaces []string)) { s.onPolicyChange = fn }

// PolicyView is one owner's policy as stored.
type PolicyView struct {
	OwnerKind string         `json:"owner_kind"`
	OwnerID   string         `json:"owner_id,omitempty"`
	Policy    *policy.Policy `json:"policy"` // null = none set at this level
	Rev       string         `json:"rev,omitempty"`
	UpdatedAt *time.Time     `json:"updated_at,omitempty"`
	UpdatedBy string         `json:"updated_by,omitempty"`
	// Warnings: what a save did despite (forced), or what to know about it.
	Warnings []string `json:"warnings,omitempty"`
	// changed: a plan's answer (PlanPolicy).
	changed bool
}

// EffectivePolicyView is what applies to one workspace, and where each
// part comes from.
type EffectivePolicyView struct {
	WorkspaceID string         `json:"workspace_id"`
	AllowAll    bool           `json:"allow_all"`
	Rev         string         `json:"rev,omitempty"`
	Layers      []policy.Layer `json:"layers"`
}

func policyOwner(kind, id string) string {
	switch kind {
	case KindFleetCeiling:
		return confrev.OwnerFleetCeiling
	case KindFleetDefault:
		return confrev.OwnerFleetDefault
	case KindGroup:
		return confrev.GroupOwner(id)
	default:
		return confrev.WorkspaceOwner(id)
	}
}

// resolvePolicyOwner validates the owner and returns its canonical id (a
// group's id for a name) and the workspaces a change there affects (nil =
// every workspace).
func (s *Service) resolvePolicyOwner(ctx context.Context, kind, id string) (string, []string, error) {
	id = strings.TrimSpace(id)
	switch kind {
	case KindFleetCeiling, KindFleetDefault:
		return "", nil, nil
	case KindWorkspace:
		if id == "" {
			return "", nil, fmt.Errorf("%w: workspace is required", ErrInvalidInput)
		}
		ok, err := s.storage.WorkspaceExists(ctx, id)
		if err != nil {
			return "", nil, err
		}
		if !ok {
			return "", nil, fmt.Errorf("%w: workspace %s is not registered", ErrNotFound, id)
		}
		return id, []string{id}, nil
	case KindGroup:
		if id == "" {
			return "", nil, fmt.Errorf("%w: group is required", ErrInvalidInput)
		}
		g, err := s.storage.Group(ctx, id)
		if err != nil {
			return "", nil, err
		}
		members, err := s.storage.GroupWorkspaces(ctx, g.ID)
		if err != nil {
			return "", nil, err
		}
		if members == nil {
			members = []string{}
		}
		return g.ID, members, nil
	}
	return "", nil, fmt.Errorf("%w: policy owner kind %q", ErrInvalidInput, kind)
}

// requireSomeAdmin is the check before anything is looked up: the caller
// holds the admin permission at all, and for a workspace has it in scope.
// A key without it gets the same refusal whether or not the owner exists.
func requireSomeAdmin(ctx context.Context, kind, id string) error {
	p := auth.PrincipalFromContext(ctx)
	if p == nil || p.Root {
		return nil
	}
	if !p.Admin || p.Run != nil {
		return auth.ErrAdminRequired
	}
	if kind == KindWorkspace {
		return auth.RequireAdmin(ctx, strings.TrimSpace(id))
	}
	return nil
}

// ErrPatternGroup is the refusal of requireFleetForPatterns.
var ErrPatternGroup = fmt.Errorf("%w: this group takes in workspaces by pattern, so what is set on it also binds workspaces that register later; that needs a key that reaches every workspace (--all-workspaces)", auth.ErrAdminRequired)

// requireFleetForPatterns asks for a fleet admin to change what a group
// with patterns sets. Such a group gains members by itself, so an admin of
// its members of today would decide for workspaces that are not in the
// key's scope and may not exist yet. A group of explicit members only
// grows when root adds one.
func (s *Service) requireFleetForPatterns(ctx context.Context, kind, groupID string) error {
	if kind != KindGroup {
		return nil
	}
	patterns, err := s.storage.GroupHasPatterns(ctx, groupID)
	if err != nil {
		return err
	}
	if patterns && auth.RequireFleetAdmin(ctx) != nil {
		return ErrPatternGroup
	}
	return nil
}

// requirePolicyAdmin enforces the admin permission at the owner's level.
func requirePolicyAdmin(ctx context.Context, kind, id string, members []string) error {
	switch kind {
	case KindWorkspace:
		return auth.RequireAdmin(ctx, id)
	case KindGroup:
		// The refusal does not name the workspace the key lacks: a group's
		// members are not the caller's to learn.
		if err := auth.RequireAdminAll(ctx, members); err != nil {
			return fmt.Errorf("%w for this group (the key must reach every workspace in it)", auth.ErrAdminRequired)
		}
		return nil
	default:
		return auth.RequireFleetAdmin(ctx)
	}
}

// GetPolicy returns one owner's policy. Fleet policies are readable by
// every authenticated caller (they bind everyone); a workspace's by any key
// with it in scope; a group's by an admin of the group.
func (s *Service) GetPolicy(ctx context.Context, kind, id string) (*PolicyView, error) {
	if kind == KindGroup {
		if err := requireSomeAdmin(ctx, kind, id); err != nil {
			return nil, err
		}
	}
	// The scope first: a key that does not have the workspace gets the
	// same answer whether or not it is registered.
	asked := strings.TrimSpace(id)
	if kind == KindWorkspace && asked != "" {
		if err := auth.RequireWorkspaceQuiet(ctx, asked); err != nil {
			return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, asked)
		}
	}
	id, members, err := s.resolvePolicyOwner(ctx, kind, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) && kind == KindWorkspace {
			return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, asked)
		}
		return nil, err
	}
	switch kind {
	case KindWorkspace:
		if err := auth.RequireWorkspaceQuiet(ctx, id); err != nil {
			return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, id)
		}
	case KindGroup:
		if err := requirePolicyAdmin(ctx, kind, id, members); err != nil {
			return nil, err
		}
	}
	return s.policyView(ctx, kind, id)
}

func (s *Service) policyView(ctx context.Context, kind, id string) (*PolicyView, error) {
	v := &PolicyView{OwnerKind: kind, OwnerID: id}
	if s.revs == nil {
		return v, nil
	}
	rev, err := s.revs.Head(ctx, confrev.KindPolicy, policyOwner(kind, id))
	if err != nil || rev == nil {
		return v, err
	}
	var p policy.Policy
	if err := json.Unmarshal(rev.Content, &p); err != nil {
		return nil, fmt.Errorf("stored policy %s: %w", rev.ID, err)
	}
	at := rev.CreatedAt
	v.Policy, v.Rev, v.UpdatedAt, v.UpdatedBy = &p, rev.ID, &at, rev.Actor
	return v, nil
}

// SetPolicy replaces an owner's policy. It needs the admin permission at
// the owner's level, is refused (ErrStrands) when it would strand current
// settings unless force is set, and is recorded with the old and the new
// revision.
func (s *Service) SetPolicy(ctx context.Context, kind, id string, p policy.Policy, force bool, via string) (*PolicyView, error) {
	return s.changePolicy(ctx, kind, id, &p, force, via, false)
}

// PlanPolicy checks a policy save (next nil = a clear) as SetPolicy and
// ClearPolicy would — permission, validation, what it strands — and
// reports whether it would change anything, writing nothing. The view's
// warnings list what it would strand.
func (s *Service) PlanPolicy(ctx context.Context, kind, id string, next *policy.Policy, force bool) (changed bool, warnings []string, err error) {
	v, err := s.changePolicy(ctx, kind, id, next, force, "", true)
	if err != nil {
		return false, nil, err
	}
	return v.changed, v.Warnings, nil
}

// ClearPolicy removes an owner's policy (the same rules as SetPolicy: what
// then applies may be narrower, e.g. the fleet default instead of a
// workspace's own).
func (s *Service) ClearPolicy(ctx context.Context, kind, id string, force bool, via string) (*PolicyView, error) {
	return s.changePolicy(ctx, kind, id, nil, force, via, false)
}

func (s *Service) changePolicy(ctx context.Context, kind, id string, next *policy.Policy, force bool, via string, dry bool) (*PolicyView, error) {
	if s.revs == nil {
		return nil, fmt.Errorf("%w: this server stores no policies", ErrInvalidInput)
	}
	// Whether the caller is an admin at all comes first: a key that is not
	// learns nothing about which workspaces and groups exist.
	if err := requireSomeAdmin(ctx, kind, id); err != nil {
		return nil, err
	}
	id, members, err := s.resolvePolicyOwner(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if err := requirePolicyAdmin(ctx, kind, id, members); err != nil {
		return nil, err
	}
	if err := s.requireFleetForPatterns(ctx, kind, id); err != nil {
		return nil, err
	}
	var content []byte
	if next != nil {
		norm, err := policy.Normalize(*next)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
		}
		if len(norm.Allow) == 0 {
			return nil, fmt.Errorf("%w: a policy with no rules allows nothing; clear the policy to remove it", ErrInvalidInput)
		}
		next = &norm
		if content, err = json.Marshal(norm); err != nil {
			return nil, err
		}
	}
	owner := policyOwner(kind, id)
	before, err := s.revs.Head(ctx, confrev.KindPolicy, owner)
	if err != nil {
		return nil, err
	}
	// What the change would strand, across the workspaces it reaches.
	stranded, err := s.wouldStrand(ctx, kind, id, members, next)
	if err != nil {
		return nil, err
	}
	if len(stranded) > 0 && !force {
		return nil, fmt.Errorf("%w (save with force to do it anyway):\n- %s", ErrStrands, strings.Join(stranded, "\n- "))
	}

	if dry {
		v := &PolicyView{OwnerKind: kind, OwnerID: id, Warnings: stranded}
		v.changed = (before == nil) != (next == nil) || (before != nil && before.Hash != confrev.Hash(content))
		return v, nil
	}
	actor, keyID := auth.Actor(ctx)
	var after *confrev.Revision
	changed := false
	if next == nil {
		changed = before != nil
		if _, err := s.revs.Clear(ctx, confrev.KindPolicy, owner); err != nil {
			return nil, err
		}
	} else {
		if after, changed, err = s.revs.Put(ctx, confrev.KindPolicy, owner, content, confrev.Meta{Actor: actor, ActorKeyID: keyID, Via: via}); err != nil {
			return nil, err
		}
	}
	s.invalidate()
	if changed {
		s.recordPolicy(ctx, kind, id, before, after, next, force && len(stranded) > 0, via)
		s.publishPolicy(kind, id)
		if s.onPolicyChange != nil {
			affected := members
			if affected == nil { // fleet level
				affected, _ = s.storage.AllWorkspaces(ctx)
			}
			s.onPolicyChange(affected)
		}
	}
	v, err := s.policyView(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if len(stranded) > 0 {
		v.Warnings = append(v.Warnings, stranded...)
	}
	return v, nil
}

func (s *Service) recordPolicy(ctx context.Context, kind, id string, before, after *confrev.Revision, next *policy.Policy, forced bool, via string) {
	if s.audit == nil {
		return
	}
	action, notes := "policy.set", ""
	was := "none"
	if before != nil {
		was = before.ID
	}
	if after == nil {
		action = "policy.delete"
		notes = "was revision " + was
	} else {
		rules := make([]string, len(next.Allow))
		for i, r := range next.Allow {
			rules[i] = r.String()
		}
		notes = "revision " + after.ID + " (was " + was + "): allow " + strings.Join(rules, "; ")
	}
	if forced {
		notes += " [forced: it strands current settings]"
	}
	ws := ""
	if kind == KindWorkspace {
		ws = id
	}
	_ = s.audit.Record(ctx, audit.RecordInput{WorkspaceID: ws, EntityType: PolicyAuditEntity, EntityID: policyOwner(kind, id), Action: action, Via: via, Notes: notes})
}

func (s *Service) publishPolicy(kind, id string) {
	if s.publisher == nil {
		return
	}
	ws := ""
	if kind == KindWorkspace {
		ws = id
	}
	s.publisher.Publish(events.Event{Type: events.TypeWorkspaces, Workspace: ws})
}

// EffectivePolicy resolves what applies to a workspace. Internal (no scope
// check): the run queue and the embedder call it.
func (s *Service) EffectivePolicy(ctx context.Context, ws string) (*policy.Effective, error) {
	return s.effectivePolicyWith(ctx, ws, "", nil, false)
}

// effectivePolicyWith resolves a workspace's policy as it would be with one
// owner's policy replaced (override; overrideOwner "" = as stored;
// overrideSet with a nil override = that owner cleared).
func (s *Service) effectivePolicyWith(ctx context.Context, ws, overrideOwner string, override *policy.Policy, overrideSet bool) (*policy.Effective, error) {
	e := &policy.Effective{}
	if s.revs == nil {
		return e, nil
	}
	heads, err := s.revs.Heads(ctx, confrev.KindPolicy)
	if err != nil {
		return nil, err
	}
	layer := func(owner, source string) (*policy.Layer, error) {
		if overrideSet && owner == overrideOwner {
			if override == nil {
				return nil, nil
			}
			return &policy.Layer{Source: source, Rev: "(proposed)", Policy: *override}, nil
		}
		rev := heads[owner]
		if rev == nil {
			return nil, nil
		}
		var p policy.Policy
		if err := json.Unmarshal(rev.Content, &p); err != nil {
			return nil, fmt.Errorf("stored policy %s: %w", rev.ID, err)
		}
		return &policy.Layer{Source: source, Rev: rev.ID, Policy: p}, nil
	}
	add := func(owner, source string) (bool, error) {
		l, err := layer(owner, source)
		if err != nil || l == nil {
			return false, err
		}
		e.Layers = append(e.Layers, *l)
		return true, nil
	}
	if _, err := add(confrev.OwnerFleetCeiling, "fleet ceiling"); err != nil {
		return nil, err
	}
	groups, err := s.storage.GroupsContaining(ctx, ws)
	if err != nil {
		return nil, err
	}
	own := false
	for _, g := range groups {
		ok, err := add(confrev.GroupOwner(g.ID), "group "+g.Name)
		if err != nil {
			return nil, err
		}
		own = own || ok
	}
	ok, err := add(confrev.WorkspaceOwner(ws), "workspace")
	if err != nil {
		return nil, err
	}
	if !own && !ok {
		if _, err := add(confrev.OwnerFleetDefault, "fleet default"); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// EffectivePolicyView is EffectivePolicy for a caller: any key with the
// workspace in scope may see what binds it.
func (s *Service) EffectivePolicyView(ctx context.Context, ws string) (*EffectivePolicyView, error) {
	ws = strings.TrimSpace(ws)
	if err := auth.RequireWorkspaceQuiet(ctx, ws); err != nil {
		return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, ws)
	}
	if ok, err := s.storage.WorkspaceExists(ctx, ws); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: workspace %s is not registered", ErrNotFound, ws)
	}
	e, err := s.EffectivePolicy(ctx, ws)
	if err != nil {
		return nil, err
	}
	v := &EffectivePolicyView{WorkspaceID: ws, AllowAll: e.AllowAll(), Rev: e.Rev(), Layers: e.Layers}
	if v.Layers == nil {
		v.Layers = []policy.Layer{}
	}
	return v, nil
}

// wouldStrand lists what a policy change would newly leave unusable in the
// workspaces it reaches: a configured runner (a stage's, a panel member's,
// the second-opinion default) that workers offer but none would be allowed
// to run, and a workspace whose embedded symbols would be dropped. Only
// what the change itself causes: something an earlier policy already
// stranded is not this save's doing. A runner no worker offers at all is
// not listed — nothing is known about it (the pipeline view already says
// no worker offers it).
func (s *Service) wouldStrand(ctx context.Context, kind, id string, members []string, next *policy.Policy) ([]string, error) {
	workspaces := members
	if workspaces == nil { // fleet level: every workspace
		all, err := s.storage.AllWorkspaces(ctx)
		if err != nil {
			return nil, err
		}
		workspaces = all
	}
	owner := policyOwner(kind, id)
	var out []string
	for _, ws := range workspaces {
		after, err := s.effectivePolicyWith(ctx, ws, owner, next, true)
		if err != nil {
			return nil, err
		}
		if after.AllowAll() {
			continue
		}
		before, err := s.EffectivePolicy(ctx, ws)
		if err != nil {
			return nil, err
		}
		now, err := s.strandedUnder(ctx, ws, after)
		if err != nil {
			return nil, err
		}
		was, err := s.strandedUnder(ctx, ws, before)
		if err != nil {
			return nil, err
		}
		already := map[string]bool{}
		for _, st := range was {
			already[st.key] = true
		}
		for _, st := range now {
			if !already[st.key] {
				out = append(out, st.text)
			}
		}
	}
	return out, nil
}

// strandedItem is one thing a policy leaves unusable: key identifies it
// whatever layer refuses it, text says it to a person.
type strandedItem struct{ key, text string }

// unpinnedStages always run, on whatever runner a worker has when the
// workspace names none.
var unpinnedStages = []string{"plan", "implement"}

// strandedUnder lists what eff leaves unusable in one workspace, judged by
// the workers seen in the last day that serve it.
func (s *Service) strandedUnder(ctx context.Context, ws string, eff *policy.Effective) ([]strandedItem, error) {
	if eff.AllowAll() {
		return nil, nil
	}
	r, err := s.resolve(ctx, ws)
	if err != nil {
		return nil, err
	}
	workers, err := s.storage.WorkerFacts(ctx, ws, s.now())
	if err != nil {
		return nil, err
	}
	type use struct{ stage, runner string }
	var uses []use
	for stage, e := range r.effective {
		for _, m := range e.Members {
			uses = append(uses, use{stage, m.Runner})
		}
	}
	if r.second != "" {
		uses = append(uses, use{"second_opinion", r.second})
	}
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].stage != uses[j].stage {
			return uses[i].stage < uses[j].stage
		}
		return uses[i].runner < uses[j].runner
	})
	var out []strandedItem
	seen := map[use]bool{}
	for _, u := range uses {
		if seen[u] {
			continue
		}
		seen[u] = true
		known := byRunner(workers, u.stage)[u.runner]
		if len(known) == 0 {
			continue
		}
		allowed, refused := false, ""
		for _, f := range known {
			// A worker that reports it cannot run the stage with the
			// runner (executor none:…) is not what the policy refuses.
			if !f.CanRun(u.stage) {
				continue
			}
			d := eff.Allows(f, u.stage)
			if d.Allowed {
				allowed = true
				break
			}
			refused = d.Refused
		}
		if !allowed && refused != "" {
			out = append(out, strandedItem{
				key:  u.stage + "\x00" + u.runner,
				text: fmt.Sprintf("%s: %s uses runner %s (%s), which the %s policy does not allow on any worker", ws, u.stage, u.runner, describeFacts(known[0]), refused),
			})
		}
	}
	// A stage that always runs and names no runner takes whatever a worker
	// has: stranded when workers serve the workspace and none has a runner
	// the policy allows for it. A worker that reports no facts never passes
	// a policy.
	if len(workers) > 0 {
		for _, stage := range unpinnedStages {
			if e, ok := r.effective[stage]; ok && len(e.Members) > 0 {
				continue
			}
			able, refused, factless, facts := false, "", 0, 0
			for _, w := range workers {
				if len(w.runners) == 0 {
					factless++
					continue
				}
				facts++
				for _, f := range w.at(stage) {
					if !f.CanRun(stage) {
						continue
					}
					d := eff.Allows(f, stage)
					if d.Allowed {
						able = true
						break
					}
					refused = d.Refused
				}
			}
			if able {
				continue
			}
			why := "no worker serving it has a runner the " + refused + " policy allows"
			if refused == "" && facts > 0 && factless == 0 {
				// Workers report facts and none has a runner that can run
				// the stage there: not a matter of the policy.
				continue
			}
			if refused == "" {
				why = "the workers serving it report no runner facts, so none passes a policy (update them)"
			} else if factless > 0 {
				why += " (workers that report no runner facts never pass a policy)"
			}
			out = append(out, strandedItem{key: stage + "\x00*", text: fmt.Sprintf("%s: %s names no runner, and %s", ws, stage, why)})
		}
	}
	if s.embeddings != nil {
		if embedded, f := s.embeddings(ctx, ws); embedded && f.Provider != "" {
			if d := eff.Allows(f, policy.StageEmbeddings); !d.Allowed {
				out = append(out, strandedItem{
					key:  "embeddings",
					text: fmt.Sprintf("%s: its embedded code index would be dropped: the embeddings endpoint (provider %s) is not allowed by the %s policy — a model on another machine on your network is declared with codeindex.embeddings.provider = \"local\"", ws, f.Provider, d.Refused),
				})
			}
		}
	}
	return out, nil
}

func describeFacts(f policy.Facts) string {
	parts := []string{"provider " + orUnknown(f.Provider)}
	if f.Model != "" {
		parts = append(parts, "model "+f.Model)
	}
	if f.Executor != "" {
		parts = append(parts, "executor "+f.Executor)
	}
	return strings.Join(parts, ", ")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// RunnerAllowed answers whether a named runner could work on a workspace's
// stage: known is false when no worker serving the workspace reports facts
// for that name (nothing can be said); otherwise allowed says whether any
// of them passes the policy, and refused names the layer that refused.
func (s *Service) RunnerAllowed(ctx context.Context, ws, stage, runner string) (allowed, known bool, refused string, err error) {
	eff, err := s.EffectivePolicy(ctx, ws)
	if err != nil {
		return false, false, "", err
	}
	if eff.AllowAll() {
		return true, true, "", nil
	}
	workers, err := s.storage.WorkerFacts(ctx, ws, s.now())
	if err != nil {
		return false, false, "", err
	}
	for _, f := range byRunner(workers, stage)[runner] {
		// A worker that cannot run the stage with the runner says nothing
		// about what the policy allows.
		if !f.CanRun(stage) {
			continue
		}
		d := eff.Allows(f, stage)
		if d.Allowed {
			return true, true, "", nil
		}
		known, refused = true, d.Refused
	}
	return false, known, refused, nil
}

// dropOrphanPolicies clears the policy of a group or workspace that no
// longer exists (the revisions stay: they are what applied while it did).
func (s *Service) dropOrphanPolicies(ctx context.Context) error {
	if s.revs == nil {
		return nil
	}
	heads, err := s.revs.Heads(ctx, confrev.KindPolicy)
	if err != nil {
		return err
	}
	for owner := range heads {
		kind, id, ok := strings.Cut(owner, ":")
		if !ok || kind == "fleet" {
			continue
		}
		exists := true
		switch kind {
		case KindWorkspace:
			exists, err = s.storage.WorkspaceExists(ctx, id)
		case KindGroup:
			_, gerr := s.storage.Group(ctx, id)
			exists, err = gerr == nil, nil
			if gerr != nil && !errors.Is(gerr, ErrNotFound) {
				err = gerr
			}
		}
		if err != nil {
			return err
		}
		if !exists {
			was, err := s.revs.Clear(ctx, confrev.KindPolicy, owner)
			if err != nil {
				return err
			}
			// Recorded like any other removal: a workspace registered
			// again later comes back without its policy, and the trail
			// must say why.
			if was != nil && s.audit != nil {
				_ = s.audit.Record(ctx, audit.RecordInput{EntityType: PolicyAuditEntity, EntityID: owner, Action: "policy.delete",
					Via: audit.ViaSystem, Notes: "was revision " + was.ID + ": the " + kind + " no longer exists"})
			}
		}
	}
	s.invalidate()
	return nil
}
