package wsconfig

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/runs"
	"github.com/martinsuchenak/skopos/internal/secretbox"
)

// Revision kinds this package stores.
const (
	KindWorkspaceConfig = "workspace_config"
	KindRunnerDef       = "runner_def"
)

// Owner kinds, as the pipeline names them.
const (
	KindWorkspace = pipeline.KindWorkspace
	KindGroup     = pipeline.KindGroup
)

// AuditEntity is the audit-log entity type of config changes; the entity id
// is "workspace:<id>" or "group:<id>".
const AuditEntity = "workspace_config"

// Via labels a config change can arrive with besides the usual surfaces.
const (
	ViaApply  = audit.ViaApply
	ViaImport = audit.ViaImport
)

// UnreadableRev is what a claim records when the workspace's config could
// not be resolved: no revision has this id, so the worker cannot fetch it
// and gives the run back instead of running it with other settings.
const UnreadableRev = runs.UnreadableConfigRev

// cacheTTL bounds how stale a resolved config may be after a change this
// service did not make (group membership); its own writes drop the cache.
const cacheTTL = 10 * time.Second

// Lookup is what the service needs to know about workspaces and groups
// (pipeline.Storage in production).
type Lookup interface {
	WorkspaceExists(ctx context.Context, id string) (bool, error)
	Group(ctx context.Context, ref string) (pipeline.GroupRef, error)
	GroupsContaining(ctx context.Context, ws string) ([]pipeline.GroupRef, error)
	GroupWorkspaces(ctx context.Context, groupID string) ([]string, error)
	AllWorkspaces(ctx context.Context) ([]string, error)
}

// AuditSink records config changes (audit.Service in production).
type AuditSink interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

// Part is one stored revision that makes up a workspace's effective config,
// in the order they apply (groups, then the workspace's own).
type Part struct {
	Owner    string `json:"owner"`    // "group:<id>" | "workspace:<id>"
	Revision string `json:"revision"` // the confrev revision id
	Source   string `json:"source"`   // "group:<name>" | "workspace"
}

// Effective is what applies to a workspace: nothing (not managed: every
// worker uses its own file), or the merged fields with where each comes
// from. Rev names exactly this combination of revisions.
type Effective struct {
	Workspace string            `json:"workspace"`
	Managed   bool              `json:"managed"`
	Rev       string            `json:"rev,omitempty"`
	Config    Config            `json:"config"`
	Sources   map[string]string `json:"sources,omitempty"`
	Parts     []Part            `json:"parts,omitempty"`
	// Conflicts: fields two of the workspace's groups both set (the first
	// group by name wins until someone fixes it).
	Conflicts []string `json:"conflicts,omitempty"`
}

// View is the read model of one owner's config.
type View struct {
	OwnerKind string     `json:"owner_kind"`
	OwnerID   string     `json:"owner_id"`
	Config    *Config    `json:"config"` // nil = none stored
	Rev       string     `json:"rev,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
	Via       string     `json:"via,omitempty"`
	// Effective: for a workspace, what applies to it.
	Effective *Effective `json:"effective,omitempty"`
	// Changed: the fields the save changed (empty = it changed nothing).
	Changed []string `json:"changed,omitempty"`
	// Defaults: what a field nothing sets is, for a form to show.
	Defaults *Config `json:"defaults,omitempty"`
}

type cachedRev struct {
	eff *Effective
	at  time.Time
}

type Service struct {
	db        *sql.DB
	revs      *confrev.Store
	lookup    Lookup
	audit     AuditSink
	publisher events.Publisher
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]cachedRev
	// gen counts writes: a config resolved before a write is not cached
	// after it.
	gen int
	// remembered: effective revisions already written to config_effective.
	remembered map[string]bool
	// fleetRev: the runner definitions' revision as last read (runners.go).
	fleetRev string
	fleetAt  time.Time

	// onChange is told which workspaces a save reached (nil = every one).
	onChange func(workspaces []string)
	// onRunners is told when a runner definition changed (runners.go).
	onRunners func()
	// secrets: where the values of MCP servers' secrets live (mcp.go).
	secrets *secretbox.Store
}

func NewService(db *sql.DB, revs *confrev.Store, lookup Lookup) *Service {
	return &Service{db: db, revs: revs, lookup: lookup, now: time.Now, cache: map[string]cachedRev{}, remembered: map[string]bool{}}
}

func (s *Service) SetAuditSink(a AuditSink)                { s.audit = a }
func (s *Service) SetPublisher(p events.Publisher)         { s.publisher = p }
func (s *Service) SetClock(now func() time.Time)           { s.now = now }
func (s *Service) SetChanged(fn func(workspaces []string)) { s.onChange = fn }
func (s *Service) SetRunnersChanged(fn func())             { s.onRunners = fn }
func (s *Service) invalidate() {
	s.mu.Lock()
	s.cache = map[string]cachedRev{}
	s.gen++
	s.mu.Unlock()
}

func ownerOf(kind, id string) string {
	if kind == KindGroup {
		return confrev.GroupOwner(id)
	}
	return confrev.WorkspaceOwner(id)
}

// resolveOwner validates the owner and returns its canonical id (a group's
// id for a name), its display name and the workspaces a change there
// reaches.
func (s *Service) resolveOwner(ctx context.Context, kind, id string) (canonical, name string, members []string, err error) {
	id = strings.TrimSpace(id)
	switch kind {
	case KindWorkspace:
		if id == "" {
			return "", "", nil, fmt.Errorf("%w: workspace is required", ErrInvalid)
		}
		ok, err := s.lookup.WorkspaceExists(ctx, id)
		if err != nil {
			return "", "", nil, err
		}
		if !ok {
			return "", "", nil, fmt.Errorf("%w: workspace %s is not registered", ErrNotFound, id)
		}
		return id, id, []string{id}, nil
	case KindGroup:
		if id == "" {
			return "", "", nil, fmt.Errorf("%w: group is required", ErrInvalid)
		}
		g, err := s.lookup.Group(ctx, id)
		if err != nil {
			if errors.Is(err, pipeline.ErrNotFound) {
				return "", "", nil, fmt.Errorf("%w: group %s", ErrNotFound, id)
			}
			return "", "", nil, err
		}
		members, err := s.lookup.GroupWorkspaces(ctx, g.ID)
		if err != nil {
			return "", "", nil, err
		}
		if members == nil {
			members = []string{}
		}
		return g.ID, g.Name, members, nil
	}
	return "", "", nil, fmt.Errorf("%w: owner kind %q", ErrInvalid, kind)
}

// requireSomeAdmin is the check before anything is looked up: the caller
// holds the admin permission at all, and for a workspace has it in scope. A
// key without it gets the same refusal whether or not the owner exists.
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

// requireAdmin enforces the admin permission at the owner's level.
func requireAdmin(ctx context.Context, kind, id string, members []string) error {
	if kind == KindWorkspace {
		return auth.RequireAdmin(ctx, id)
	}
	// The refusal does not name the workspace the key lacks: a group's
	// members are not the caller's to learn.
	if err := auth.RequireAdminAll(ctx, members); err != nil {
		return fmt.Errorf("%w for this group (the key must reach every workspace in it)", auth.ErrAdminRequired)
	}
	return nil
}

func decode(rev *confrev.Revision) (Config, error) {
	var c Config
	if err := json.Unmarshal(rev.Content, &c); err != nil {
		return Config{}, fmt.Errorf("stored config %s: %w", rev.ID, err)
	}
	return c, nil
}

// Get returns one owner's config. A workspace's is readable by any key with
// the workspace in scope (it is what the workspace's workers run); a
// group's by an admin of the group.
func (s *Service) Get(ctx context.Context, kind, id string) (*View, error) {
	if kind == KindGroup {
		if err := requireSomeAdmin(ctx, kind, id); err != nil {
			return nil, err
		}
	} else if err := auth.RequireWorkspaceQuiet(ctx, strings.TrimSpace(id)); err != nil {
		return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, strings.TrimSpace(id))
	}
	id, _, members, err := s.resolveOwner(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if kind == KindGroup {
		if err := requireAdmin(ctx, kind, id, members); err != nil {
			return nil, err
		}
	}
	return s.view(ctx, kind, id)
}

func (s *Service) view(ctx context.Context, kind, id string) (*View, error) {
	defaults := Defaults()
	v := &View{OwnerKind: kind, OwnerID: id, Defaults: &defaults}
	rev, err := s.revs.Head(ctx, KindWorkspaceConfig, ownerOf(kind, id))
	if err != nil {
		return nil, err
	}
	if rev != nil {
		c, err := decode(rev)
		if err != nil {
			return nil, err
		}
		at := rev.CreatedAt
		v.Config, v.Rev, v.UpdatedAt, v.UpdatedBy, v.Via = &c, rev.ID, &at, rev.Actor, rev.Via
	}
	if kind == KindWorkspace {
		eff, err := s.resolve(ctx, id, "", nil, false)
		if err != nil {
			return nil, err
		}
		v.Effective = eff
	}
	return v, nil
}

// Set replaces an owner's config. It needs the admin permission at the
// owner's level and is recorded with the old and the new revision and the
// fields that changed. Saving a workspace's config makes it managed: every
// shared field then comes from the server.
func (s *Service) Set(ctx context.Context, kind, id string, c Config, via, note string) (*View, error) {
	return s.change(ctx, kind, id, &c, via, note, false, nil)
}

// SetIf is Set for a read-modify-write: refused (ErrConflict) when the
// owner's stored revision is no longer ifRev ("" = it had none) — someone
// else saved in between.
func (s *Service) SetIf(ctx context.Context, kind, id string, c Config, via, note, ifRev string) (*View, error) {
	return s.change(ctx, kind, id, &c, via, note, false, &ifRev)
}

// Plan checks a save (next nil = a clear) exactly as Set and Clear would —
// permission, validation, conflicts — and returns the fields it would
// change, writing nothing.
func (s *Service) Plan(ctx context.Context, kind, id string, next *Config) ([]string, error) {
	v, err := s.change(ctx, kind, id, next, "", "", true, nil)
	if err != nil {
		return nil, err
	}
	return v.Changed, nil
}

// Clear removes an owner's config. A workspace is then no longer managed:
// its workers go back to their own files.
func (s *Service) Clear(ctx context.Context, kind, id, via string) (*View, error) {
	return s.change(ctx, kind, id, nil, via, "", false, nil)
}

func (s *Service) change(ctx context.Context, kind, id string, next *Config, via, note string, dry bool, ifRev *string) (*View, error) {
	via = audit.CleanVia(via)
	// Whether the caller is an admin at all comes first: a key that is not
	// learns nothing about which workspaces and groups exist.
	if err := requireSomeAdmin(ctx, kind, id); err != nil {
		return nil, err
	}
	id, name, members, err := s.resolveOwner(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	if err := requireAdmin(ctx, kind, id, members); err != nil {
		return nil, err
	}
	owner := ownerOf(kind, id)
	before, err := s.revs.Head(ctx, KindWorkspaceConfig, owner)
	if err != nil {
		return nil, err
	}
	if ifRev != nil {
		held := ""
		if before != nil {
			held = before.ID
		}
		if held != strings.TrimSpace(*ifRev) {
			return nil, fmt.Errorf("%w: the config was saved by someone else since it was read (it is at revision %s); read it again", ErrConflict, cmp.Or(held, "none"))
		}
	}
	var was Config
	if before != nil {
		if was, err = decode(before); err != nil {
			return nil, err
		}
	}
	var content []byte
	if next != nil {
		norm, err := Normalize(*next)
		if err != nil {
			return nil, err
		}
		next = &norm
		content = Canonical(norm)
		if err := s.checkWrappers(ctx, next); err != nil {
			return nil, err
		}
		reach := members
		if kind == KindWorkspace {
			reach = []string{id}
		}
		if err := s.checkMCP(ctx, next, reach); err != nil {
			return nil, err
		}
	}
	// A clear is checked too: taking a group's config away can leave a
	// managed member without something its own config needs.
	if err := s.checkSave(ctx, kind, id, name, members, next); err != nil {
		return nil, err
	}
	if dry {
		now := Config{}
		if next != nil {
			now = *next
		}
		v := &View{OwnerKind: kind, OwnerID: id, Changed: Changed(was, now)}
		if (before == nil) != (next == nil) && len(v.Changed) == 0 {
			// Becoming managed with an empty config (or the reverse) is a
			// change though no field differs.
			v.Changed = []string{"managed"}
		}
		return v, nil
	}
	actor, keyID := auth.Actor(ctx)
	var after *confrev.Revision
	changed := false
	if next == nil {
		changed = before != nil
		if _, err := s.revs.Clear(ctx, KindWorkspaceConfig, owner); err != nil {
			return nil, err
		}
	} else if after, changed, err = s.revs.Put(ctx, KindWorkspaceConfig, owner, content, confrev.Meta{Actor: actor, ActorKeyID: keyID, Via: via, Note: note}); err != nil {
		return nil, err
	}
	s.invalidate()
	var fields []string
	if changed {
		now := Config{}
		if next != nil {
			now = *next
		}
		fields = Changed(was, now)
		if (before == nil) != (next == nil) && len(fields) == 0 {
			// Becoming managed with an empty config (or the reverse) is a
			// change though no field differs: a workspace's workers stop
			// reading their own file for it. Said as the dry run says it.
			fields = []string{"managed"}
		}
		s.record(ctx, kind, id, before, after, fields, now, via)
		if s.publisher != nil {
			ws := ""
			if kind == KindWorkspace {
				ws = id
			}
			s.publisher.Publish(events.Event{Type: events.TypeWorkspaces, Workspace: ws})
		}
		if s.onChange != nil {
			s.onChange(members)
		}
	}
	v, err := s.view(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	v.Changed = fields
	return v, nil
}

// GroupMembers returns the workspaces a group reaches now and whether the
// group exists. Internal (no scope check): for a caller that has to decide
// whether someone may act on the group.
func (s *Service) GroupMembers(ctx context.Context, ref string) ([]string, bool, error) {
	_, _, members, err := s.resolveOwner(ctx, KindGroup, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	return members, err == nil, err
}

// PlanNew checks a config for an owner that does not exist yet (a group or
// a workspace the same apply creates): the permission, the values, and for
// a group the fields another group already sets for one of its members.
// What depends on the owner existing — a managed member's merged config —
// is checked when the config is saved.
func (s *Service) PlanNew(ctx context.Context, kind, id string, members []string, next Config) ([]string, error) {
	norm, err := Normalize(next)
	if err != nil {
		return nil, err
	}
	if err := s.checkWrappers(ctx, &norm); err != nil {
		return nil, err
	}
	reach := members
	if kind == KindWorkspace {
		reach = []string{id}
	}
	if err := s.checkMCP(ctx, &norm, reach); err != nil {
		return nil, err
	}
	if kind == KindWorkspace {
		if err := auth.RequireAdmin(ctx, id); err != nil {
			return nil, err
		}
		return norm.FieldNames(), CheckEffective(norm)
	}
	if err := requireAdmin(ctx, kind, "", members); err != nil {
		return nil, err
	}
	return norm.FieldNames(), s.checkSave(ctx, kind, "", id, members, &norm)
}

// checkSave refuses what would leave a workspace with a config that cannot
// work, and a group setting a field another group already sets for a
// workspace both contain.
func (s *Service) checkSave(ctx context.Context, kind, id, name string, members []string, save *Config) error {
	if kind == KindWorkspace {
		if save == nil {
			return nil // no longer managed: nothing is left to check
		}
		eff, err := s.resolve(ctx, id, ownerOf(kind, id), save, true)
		if err != nil {
			return err
		}
		return CheckEffective(eff.Config)
	}
	// A cleared group sets nothing: its members are checked without it.
	var next Config
	if save != nil {
		next = *save
	}
	mine := next.Fields()
	for _, ws := range members {
		groups, err := s.lookup.GroupsContaining(ctx, ws)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if g.ID == id {
				continue
			}
			rev, err := s.revs.Head(ctx, KindWorkspaceConfig, confrev.GroupOwner(g.ID))
			if err != nil {
				return err
			}
			if rev == nil {
				continue
			}
			theirs, err := decode(rev)
			if err != nil {
				return err
			}
			for _, f := range theirs.FieldNames() {
				if _, ok := mine[f]; ok {
					return fmt.Errorf("%w: group %s already sets %s for %s, which is in both groups", ErrConflict, g.Name, f, ws)
				}
			}
		}
		// A managed member must still have a config that works.
		eff, err := s.resolve(ctx, ws, ownerOf(kind, id), save, true)
		if err != nil {
			return err
		}
		if !eff.Managed {
			continue
		}
		if err := CheckEffective(eff.Config); err != nil {
			return fmt.Errorf("%w (for workspace %s, which group %s reaches)", err, ws, name)
		}
	}
	return nil
}

func (s *Service) record(ctx context.Context, kind, id string, before, after *confrev.Revision, fields []string, now Config, via string) {
	if s.audit == nil {
		return
	}
	was := "none"
	if before != nil {
		was = before.ID
	}
	action := "config.update"
	if via == ViaImport {
		action = "config.import"
	}
	var notes string
	if after == nil {
		action = "config.delete"
		notes = "was revision " + was
		if kind == KindWorkspace {
			notes += ": the workspace is no longer managed"
		}
	} else {
		notes = "revision " + after.ID + " (was " + was + "): " + strings.Join(fields, ", ")
		// Command fields are recorded with their values: they are the
		// change, and they run on every worker serving the workspace.
		set := now.Fields()
		for _, f := range fields {
			if !IsCommandField(f) {
				continue
			}
			v := Describe(set[f])
			if len(v) > 1500 {
				v = v[:1500] + "… (the revision holds the rest)"
			}
			notes += "\n" + f + " = " + v
		}
	}
	ws := ""
	if kind == KindWorkspace {
		ws = id
	}
	_ = s.audit.Record(ctx, audit.RecordInput{WorkspaceID: ws, EntityType: AuditEntity, EntityID: ownerOf(kind, id), Action: action, Via: via, Notes: notes})
}

// resolve merges what applies to a workspace: its groups' configs in name
// order (the first to set a field keeps it; a second is a conflict), then
// its own on top. override replaces one owner's stored config (a save being
// checked); it is never persisted.
func (s *Service) resolve(ctx context.Context, ws, overrideOwner string, override *Config, overrideSet bool) (*Effective, error) {
	eff := &Effective{Workspace: ws}
	load := func(owner string) (*Config, string, error) {
		if overrideSet && owner == overrideOwner {
			return override, "pending", nil // nil: the owner would have none
		}
		rev, err := s.revs.Head(ctx, KindWorkspaceConfig, owner)
		if err != nil || rev == nil {
			return nil, "", err
		}
		c, err := decode(rev)
		if err != nil {
			return nil, "", err
		}
		return &c, rev.ID, nil
	}
	own, ownRev, err := load(confrev.WorkspaceOwner(ws))
	if err != nil {
		return nil, err
	}
	if own == nil {
		return eff, nil // not managed: a group's values never manage a workspace on their own
	}
	eff.Managed = true
	groups, err := s.lookup.GroupsContaining(ctx, ws)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	fields := map[string]json.RawMessage{}
	eff.Sources = map[string]string{}
	for _, g := range groups {
		c, rev, err := load(confrev.GroupOwner(g.ID))
		if err != nil {
			return nil, err
		}
		if c == nil {
			continue
		}
		source := "group:" + g.Name
		eff.Parts = append(eff.Parts, Part{Owner: confrev.GroupOwner(g.ID), Revision: rev, Source: source})
		for k, v := range c.Fields() {
			if prev, ok := eff.Sources[k]; ok {
				eff.Conflicts = append(eff.Conflicts, fmt.Sprintf("%s: set by %s and %s", k, prev, source))
				continue
			}
			fields[k], eff.Sources[k] = v, source
		}
	}
	for k, v := range own.Fields() {
		fields[k], eff.Sources[k] = v, "workspace"
	}
	eff.Parts = append(eff.Parts, Part{Owner: confrev.WorkspaceOwner(ws), Revision: ownRev, Source: "workspace"})
	sort.Strings(eff.Conflicts)
	if eff.Config, err = FromFields(fields); err != nil {
		return nil, err
	}
	if !overrideSet {
		eff.Rev = revOf(ws, eff.Parts)
		if err := s.remember(ctx, eff); err != nil {
			return nil, err
		}
	}
	return eff, nil
}

// revOf names a combination of revisions for a workspace.
func revOf(ws string, parts []Part) string {
	h := sha256.New()
	h.Write([]byte(ws))
	for _, p := range parts {
		h.Write([]byte("\n" + p.Owner + "=" + p.Revision))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// remember stores which revisions an effective revision stands for, so it
// can be read back after the heads have moved on.
func (s *Service) remember(ctx context.Context, eff *Effective) error {
	// Once per revision and process: it is resolved again every few
	// seconds, and the row never changes.
	s.mu.Lock()
	known := s.remembered[eff.Rev]
	s.mu.Unlock()
	if known {
		return nil
	}
	parts, _ := json.Marshal(eff.Parts)
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO config_effective (rev, workspace_id, parts, created_ms) VALUES (?, ?, ?, ?)`,
		eff.Rev, eff.Workspace, string(parts), s.now().UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("recording effective config %s: %w", eff.Rev, err)
	}
	s.mu.Lock()
	if len(s.remembered) > 10000 {
		s.remembered = map[string]bool{}
	}
	s.remembered[eff.Rev] = true
	s.mu.Unlock()
	return nil
}

// Effective resolves what applies to a workspace now. Internal (no scope
// check): the run queue calls it at every claim. Cached briefly.
func (s *Service) Effective(ctx context.Context, ws string) (*Effective, error) {
	s.mu.Lock()
	c, ok := s.cache[ws]
	gen := s.gen
	s.mu.Unlock()
	if ok && s.now().Sub(c.at) < cacheTTL {
		return c.eff, nil
	}
	eff, err := s.resolve(ctx, ws, "", nil, false)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.gen == gen {
		s.cache[ws] = cachedRev{eff: eff, at: s.now()}
	}
	s.mu.Unlock()
	return eff, nil
}

// Rev is the revision a claim records for a run in the workspace: "" when
// it is not managed, UnreadableRev when its config could not be resolved
// (the worker then cannot fetch it and gives the run back).
func (s *Service) Rev(ctx context.Context, ws string) string {
	eff, err := s.Effective(ctx, ws)
	if err != nil {
		// Once more before giving the run a revision nothing can fetch: a
		// busy database is the usual reason.
		if eff, err = s.Effective(ctx, ws); err != nil {
			return UnreadableRev
		}
	}
	return eff.Rev
}

// Digest names the effective configs of a set of workspaces together, so a
// worker can tell from one value that something it holds is out of date.
func (s *Service) Digest(ctx context.Context, workspaces []string) string {
	ws := append([]string{}, workspaces...)
	sort.Strings(ws)
	h := sha256.New()
	any := false
	for _, w := range ws {
		rev := s.Rev(ctx, w)
		if rev != "" {
			any = true
		}
		h.Write([]byte(w + "=" + rev + "\n"))
	}
	if !any {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// EffectiveFor is Effective for a caller: the workspace must be in its
// scope (a foreign workspace is not found).
func (s *Service) EffectiveFor(ctx context.Context, ws string) (*Effective, error) {
	ws = strings.TrimSpace(ws)
	if err := auth.RequireWorkspaceQuiet(ctx, ws); err != nil {
		return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, ws)
	}
	return s.resolve(ctx, ws, "", nil, false)
}

// At returns the effective config a revision id names, built from the
// stored revisions it stands for — the same answer whatever was saved
// since.
func (s *Service) At(ctx context.Context, ws, rev string) (*Effective, error) {
	ws, rev = strings.TrimSpace(ws), strings.TrimSpace(rev)
	if err := auth.RequireWorkspaceQuiet(ctx, ws); err != nil {
		return nil, fmt.Errorf("%w: workspace %s", ErrNotFound, ws)
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT parts FROM config_effective WHERE rev = ? AND workspace_id = ?`, rev, ws).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: config revision %s of %s", ErrNotFound, rev, ws)
	}
	if err != nil {
		return nil, fmt.Errorf("reading effective config: %w", err)
	}
	eff := &Effective{Workspace: ws, Managed: true, Rev: rev, Sources: map[string]string{}}
	if err := json.Unmarshal([]byte(raw), &eff.Parts); err != nil {
		return nil, fmt.Errorf("stored effective config %s: %w", rev, err)
	}
	fields := map[string]json.RawMessage{}
	for _, p := range eff.Parts {
		r, err := s.revs.Get(ctx, p.Revision)
		if err != nil {
			return nil, err
		}
		c, err := decode(r)
		if err != nil {
			return nil, err
		}
		own := p.Source == "workspace"
		for k, v := range c.Fields() {
			if _, ok := eff.Sources[k]; ok && !own {
				continue // an earlier group keeps the field, as when it was resolved
			}
			fields[k], eff.Sources[k] = v, p.Source
		}
	}
	if eff.Config, err = FromFields(fields); err != nil {
		return nil, err
	}
	return eff, nil
}

// DropWorkspace forgets a deleted workspace's config (its revisions stay:
// they are evidence).
func (s *Service) DropWorkspace(ctx context.Context, ws string) {
	_, _ = s.revs.Clear(ctx, KindWorkspaceConfig, confrev.WorkspaceOwner(ws))
	s.invalidate()
}
