package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/runs"
)

// AuditEntity is the audit-log entity type of pipeline changes; the entity
// id is "workspace:<id>" or "group:<id>".
const AuditEntity = "workspace_pipeline"

// importedBy marks rows written by the one-time flag import.
const importedBy = "flag-import"

// cacheTTL bounds how stale resolved routing may be after a change this
// service did not make (group membership edits); its own writes drop the
// cache at once.
const cacheTTL = 10 * time.Second

// AuditSink records pipeline changes (audit.Service in production).
type AuditSink interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

type cached struct {
	prefs *runs.WorkspacePrefs
	at    time.Time
}

type Service struct {
	storage   *Storage
	now       func() time.Time
	audit     AuditSink
	publisher events.Publisher

	mu    sync.Mutex
	cache map[string]cached
}

func NewService(storage *Storage) *Service {
	return &Service{storage: storage, now: time.Now, cache: map[string]cached{}}
}

func (s *Service) SetAuditSink(a AuditSink)        { s.audit = a }
func (s *Service) SetPublisher(p events.Publisher) { s.publisher = p }
func (s *Service) SetClock(now func() time.Time)   { s.now = now }
func (s *Service) invalidate()                     { s.mu.Lock(); s.cache = map[string]cached{}; s.mu.Unlock() }
func ownerEntity(kind, id string) string           { return kind + ":" + id }

// Prefs is the run queue's lookup: the effective routing of a workspace.
// Internal (no scope check) — the queue has already authorized the caller.
// On a read error it keeps the last known routing rather than silently
// dropping review and QA.
func (s *Service) Prefs(ctx context.Context, ws string) *runs.WorkspacePrefs {
	s.mu.Lock()
	c, ok := s.cache[ws]
	s.mu.Unlock()
	if ok && s.now().Sub(c.at) < cacheTTL {
		return c.prefs
	}
	r, err := s.resolve(ctx, ws)
	if err != nil {
		slog.Warn("pipeline: resolving routing failed; using the last known", "workspace", ws, "err", err)
		return c.prefs
	}
	stages := make(map[string]StageSetting, len(r.effective))
	for stage, e := range r.effective {
		stages[stage] = e.StageSetting
	}
	p := ToPrefs(stages, r.agents)
	s.mu.Lock()
	s.cache[ws] = cached{prefs: p, at: s.now()}
	s.mu.Unlock()
	return p
}

type resolved struct {
	own          Settings
	effective    map[string]EffectiveStage
	agents       string
	agentsSource string
	groups       []GroupRef
	conflicts    []string
}

// resolve: per stage the workspace's own setting, else the first group (by
// name) that sets it; two groups setting the same stage is a conflict,
// reported and resolved by name until someone fixes it.
func (s *Service) resolve(ctx context.Context, ws string) (*resolved, error) {
	own, err := s.storage.Get(ctx, KindWorkspace, ws)
	if err != nil {
		return nil, err
	}
	groups, err := s.storage.GroupsContaining(ctx, ws)
	if err != nil {
		return nil, err
	}
	gsets := make([]Settings, len(groups))
	for i, g := range groups {
		if gsets[i], err = s.storage.Get(ctx, KindGroup, g.ID); err != nil {
			return nil, err
		}
	}
	r := &resolved{own: own, effective: map[string]EffectiveStage{}, groups: groups}
	for _, stage := range Stages {
		if st, ok := own.Stages[stage]; ok {
			r.effective[stage] = EffectiveStage{StageSetting: st, Source: KindWorkspace}
			continue
		}
		var setters []string
		for i, g := range groups {
			if st, ok := gsets[i].Stages[stage]; ok {
				if len(setters) == 0 {
					r.effective[stage] = EffectiveStage{StageSetting: st, Source: "group:" + g.Name}
				}
				setters = append(setters, g.Name)
			}
		}
		if len(setters) > 1 {
			r.conflicts = append(r.conflicts, fmt.Sprintf("%s is set by groups %s; using %s", stage, strings.Join(setters, ", "), setters[0]))
		}
	}
	var agentSetters []string
	for i, g := range groups {
		if gsets[i].AgentsDefault != "" {
			if len(agentSetters) == 0 {
				r.agents, r.agentsSource = gsets[i].AgentsDefault, "group:"+g.Name
			}
			agentSetters = append(agentSetters, g.Name)
		}
	}
	if own.AgentsDefault != "" {
		r.agents, r.agentsSource = own.AgentsDefault, KindWorkspace
	} else if len(agentSetters) > 1 {
		r.conflicts = append(r.conflicts, fmt.Sprintf("the agents default is set by groups %s; using %s", strings.Join(agentSetters, ", "), agentSetters[0]))
	}
	return r, nil
}

// Get returns an owner's settings view. Workspaces: any key in scope (the
// routing is not secret to the team that uses it). Groups: root only, like
// every group read.
func (s *Service) Get(ctx context.Context, kind, id string) (*View, error) {
	kind, id, err := s.authorizeRead(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if kind == KindGroup {
		own, err := s.storage.Get(ctx, KindGroup, id)
		if err != nil {
			return nil, err
		}
		return &View{OwnerKind: kind, OwnerID: id, Own: own}, nil
	}
	r, err := s.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	v := &View{OwnerKind: kind, OwnerID: id, Own: r.own, Effective: r.effective,
		EffectiveAgentsDefault: r.agents, AgentsDefaultSource: r.agentsSource, Groups: r.groups, Conflicts: r.conflicts}
	if offered, err := s.storage.RunnersServing(ctx, id); err == nil {
		unknown := map[string]bool{}
		for _, e := range r.effective {
			for _, m := range e.Members {
				if !offered[m.Runner] {
					unknown[m.Runner] = true
				}
			}
		}
		for name := range unknown {
			v.UnknownRunners = append(v.UnknownRunners, name)
		}
		sort.Strings(v.UnknownRunners)
	}
	return v, nil
}

func (s *Service) authorizeRead(ctx context.Context, kind, id string) (string, string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return kind, id, fmt.Errorf("%w: %s is required", ErrInvalidInput, kind)
	}
	switch kind {
	case KindWorkspace:
		if err := auth.RequireWorkspaceQuiet(ctx, id); err != nil {
			return kind, id, fmt.Errorf("%w: workspace %s", ErrNotFound, id)
		}
		ok, err := s.storage.WorkspaceExists(ctx, id)
		if err != nil {
			return kind, id, err
		}
		if !ok {
			return kind, id, fmt.Errorf("%w: workspace %s is not registered", ErrNotFound, id)
		}
		return kind, id, nil
	case KindGroup:
		if err := auth.RequireRoot(ctx); err != nil {
			return kind, id, err
		}
		g, err := s.storage.Group(ctx, id)
		if err != nil {
			return kind, id, err
		}
		return kind, g.ID, nil
	}
	return kind, id, fmt.Errorf("%w: owner kind %q", ErrInvalidInput, kind)
}

// Put replaces an owner's settings. Workspaces: approvers whose scope
// covers it (it decides which models check the work). Groups: root only.
// Every change is audit-logged with what changed.
func (s *Service) Put(ctx context.Context, kind, id string, in Settings, via string) (*View, error) {
	if kind == KindWorkspace {
		if err := auth.RequireWorkspace(ctx, strings.TrimSpace(id)); err != nil {
			return nil, err
		}
		if err := auth.RequireApprover(ctx); err != nil {
			return nil, err
		}
	}
	kind, id, err := s.authorizeRead(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	next, err := Normalize(in)
	if err != nil {
		return nil, err
	}
	if kind == KindGroup {
		if err := s.checkGroupConflicts(ctx, id, next); err != nil {
			return nil, err
		}
	}
	before, err := s.storage.Get(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	by := ""
	if p := auth.PrincipalFromContext(ctx); p != nil {
		by = p.KeyID
	}
	if err := s.storage.Replace(ctx, kind, id, next, by, s.now().UTC()); err != nil {
		return nil, err
	}
	s.invalidate()
	if changes := diff(before, next); changes != "" {
		s.record(ctx, kind, id, "pipeline.update", via, changes)
	}
	s.publish(kind, id)
	return s.Get(ctx, kind, id)
}

// checkGroupConflicts refuses a group setting a stage that another group
// already sets for one of its workspaces — resolution must never depend on
// group names.
func (s *Service) checkGroupConflicts(ctx context.Context, groupID string, next Settings) error {
	members, err := s.storage.GroupWorkspaces(ctx, groupID)
	if err != nil {
		return err
	}
	var problems []string
	check := func(what string, setters map[string]bool) error {
		delete(setters, groupID)
		if len(setters) == 0 {
			return nil
		}
		for _, ws := range members {
			groups, err := s.storage.GroupsContaining(ctx, ws)
			if err != nil {
				return err
			}
			for _, g := range groups {
				if setters[g.ID] {
					problems = append(problems, fmt.Sprintf("%s: workspace %s is also in group %s, which sets it", what, ws, g.Name))
				}
			}
		}
		return nil
	}
	for stage := range next.Stages {
		setters, err := s.storage.GroupsSettingStage(ctx, stage)
		if err != nil {
			return err
		}
		if err := check(stage, setters); err != nil {
			return err
		}
	}
	if next.AgentsDefault != "" {
		setters, err := s.storage.GroupsSettingAgentsDefault(ctx)
		if err != nil {
			return err
		}
		if err := check("agents default", setters); err != nil {
			return err
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		if len(problems) > 5 {
			problems = append(problems[:5], fmt.Sprintf("and %d more", len(problems)-5))
		}
		return fmt.Errorf("%w: %s — set it on one group only, or on the workspace", ErrConflict, strings.Join(problems, "; "))
	}
	return nil
}

func (s *Service) record(ctx context.Context, kind, id, action, via, notes string) {
	if s.audit == nil {
		return
	}
	ws := ""
	if kind == KindWorkspace {
		ws = id
	}
	_ = s.audit.Record(ctx, audit.RecordInput{WorkspaceID: ws, EntityType: AuditEntity, EntityID: ownerEntity(kind, id), Action: action, Via: via, Notes: notes})
}

func (s *Service) publish(kind, id string) {
	if s.publisher == nil {
		return
	}
	ws := ""
	if kind == KindWorkspace {
		ws = id
	}
	s.publisher.Publish(events.Event{Type: events.TypeWorkspaces, Workspace: ws})
}

// diff renders what changed, stage by stage, for the audit log.
func diff(before, after Settings) string {
	var parts []string
	for _, stage := range Stages {
		b, hb := before.Stages[stage]
		a, ha := after.Stages[stage]
		if hb == ha && (!hb || describe(b) == describe(a)) {
			continue
		}
		from, to := "not set", "not set"
		if hb {
			from = describe(b)
		}
		if ha {
			to = describe(a)
		}
		parts = append(parts, fmt.Sprintf("%s: %s → %s", stage, from, to))
	}
	if before.AgentsDefault != after.AgentsDefault {
		parts = append(parts, fmt.Sprintf("agents default: %s → %s", orDefault(before.AgentsDefault), orDefault(after.AgentsDefault)))
	}
	return strings.Join(parts, "; ")
}

func describe(st StageSetting) string {
	out := FormatMembers(st.Members)
	if st.Policy != "" {
		out += " (" + st.Policy
		if st.EarlyExit {
			out += ", early exit"
		}
		out += ")"
	} else if st.EarlyExit {
		out += " (early exit)"
	}
	if st.TimeoutMS > 0 {
		out += fmt.Sprintf(" timeout %s", time.Duration(st.TimeoutMS)*time.Millisecond)
	}
	return out
}

func orDefault(a string) string {
	if a == "" {
		return "not set"
	}
	return a
}

// Import stores the old --run-stage / --stage-policy / --agents-default
// flags once: stages a workspace has not stored yet are added (registering
// the workspace if needed) and audited as a system import; a flag that
// differs from a stored setting only logs — stored settings win. Returns
// one log line per action.
func (s *Service) Import(ctx context.Context, flags map[string]*runs.WorkspacePrefs, register func(ws string) error) []string {
	var log []string
	wsIDs := make([]string, 0, len(flags))
	for ws := range flags {
		wsIDs = append(wsIDs, ws)
	}
	sort.Strings(wsIDs)
	for _, ws := range wsIDs {
		raw := FromPrefs(flags[ws])
		valid := Settings{Stages: map[string]StageSetting{}, AgentsDefault: raw.AgentsDefault}
		for stage, st := range raw.Stages {
			if err := ValidateStage(stage, st); err != nil {
				log = append(log, fmt.Sprintf("flag import: %s: skipping %s: %v", ws, stage, err))
				continue
			}
			valid.Stages[stage] = st
		}
		if register != nil {
			if err := register(ws); err != nil {
				log = append(log, fmt.Sprintf("flag import: %s: cannot register the workspace: %v", ws, err))
				continue
			}
		}
		stored, err := s.storage.Get(ctx, KindWorkspace, ws)
		if err != nil {
			log = append(log, fmt.Sprintf("flag import: %s: %v", ws, err))
			continue
		}
		added, err := s.storage.AddMissing(ctx, KindWorkspace, ws, valid, importedBy, s.now().UTC())
		if err != nil {
			log = append(log, fmt.Sprintf("flag import: %s: %v", ws, err))
			continue
		}
		if changes := diff(Settings{Stages: map[string]StageSetting{}}, added); changes != "" {
			s.record(ctx, KindWorkspace, ws, "pipeline.import", audit.ViaSystem, "imported from server flags: "+changes)
			log = append(log, fmt.Sprintf("flag import: %s: imported %s", ws, changes))
		}
		for stage, st := range valid.Stages {
			if cur, ok := stored.Stages[stage]; ok && describe(cur) != describe(st) {
				log = append(log, fmt.Sprintf("flag import: %s: %s flag (%s) differs from the stored setting (%s); stored setting wins — remove the flag", ws, stage, describe(st), describe(cur)))
			}
		}
		if stored.AgentsDefault != "" && valid.AgentsDefault != "" && stored.AgentsDefault != valid.AgentsDefault {
			log = append(log, fmt.Sprintf("flag import: %s: --agents-default %s differs from the stored %s; stored setting wins — remove the flag", ws, valid.AgentsDefault, stored.AgentsDefault))
		}
	}
	s.invalidate()
	return log
}

// DropOrphans removes settings of deleted workspaces and groups (startup).
func (s *Service) DropOrphans(ctx context.Context) error {
	err := s.storage.DropOrphans(ctx)
	s.invalidate()
	return err
}
