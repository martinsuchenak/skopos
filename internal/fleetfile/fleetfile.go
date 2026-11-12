// Package fleetfile is the one-file form of everything the server holds
// about how runs are configured (tmp/central-config — design.md, phase 2c):
// runner and wrapper definitions, groups, runner policies, pipeline
// settings and workspace config (each stage's capabilities among it). `skopos config export` writes it, `skopos config apply`
// makes the server match it, and `--plan` lists what an apply would change —
// what the reviewer of the pull request reads.
//
// The file is authoritative for what it lists:
//   - a workspace or group in the file gets exactly the policy, pipeline and
//     config the file gives it; a section left out is cleared;
//   - `[fleet]`, when present, states the ceiling and the default policy;
//   - `[runners]`, when present, is the full set of runner definitions;
//   - `[wrappers]`, when present, is the full set of wrapper definitions;
//   - `[mcp]`, when present, is the full set of MCP server definitions.
//
// A workspace or group the file does not list, and a `[fleet]`,
// `[runners]`, `[wrappers]` or `[mcp]` section it does not have, are left
// as they are.
//
// The file never holds a secret: an MCP server's definition names its
// secrets (${secret:NAME}), and their values are set apart (`skopos
// mcp-server set-secret`). Applying a definition keeps the values stored
// for the secrets it still names.
package fleetfile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	"github.com/martinsuchenak/skopos/internal/wsconfig"
)

var ErrInvalid = errors.New("invalid configuration file")

// AuditEntity is the audit-log entity type of an apply; the entity id is
// the file's hash.
const AuditEntity = "fleet_config"

// Document is the file's content.
type Document struct {
	Fleet *FleetDoc `json:"fleet,omitempty" toml:"fleet,omitempty"`
	// Runners: nil = the file has no [runners] section (the server's
	// definitions stay); otherwise the full set.
	Runners map[string]wsconfig.RunnerDef `json:"runners" toml:"runners,omitempty"`
	// Wrappers: nil = the file has no [wrappers] section (the server's
	// definitions stay); otherwise the full set.
	Wrappers map[string]wsconfig.WrapperDef `json:"wrappers" toml:"wrappers,omitempty"`
	// MCP: nil = the file has no [mcp] section (the server's definitions
	// stay); otherwise the full set of MCP server definitions.
	MCP        map[string]wsconfig.MCPServerDef `json:"mcp" toml:"mcp,omitempty"`
	Groups     map[string]*GroupDoc             `json:"groups,omitempty" toml:"groups,omitempty"`
	Workspaces map[string]*OwnerDoc             `json:"workspaces,omitempty" toml:"workspaces,omitempty"`
}

// FleetDoc states the fleet's two policies; one left out is cleared.
type FleetDoc struct {
	Ceiling *policy.Policy `json:"ceiling,omitempty" toml:"ceiling,omitempty"`
	Default *policy.Policy `json:"default,omitempty" toml:"default,omitempty"`
}

// OwnerDoc is what a workspace has: a section left out is cleared.
type OwnerDoc struct {
	Policy   *policy.Policy   `json:"policy,omitempty" toml:"policy,omitempty"`
	Pipeline *PipelineDoc     `json:"pipeline,omitempty" toml:"pipeline,omitempty"`
	Config   *wsconfig.Config `json:"config,omitempty" toml:"config,omitempty"`
}

// GroupDoc is a group: who is in it, and what it has.
type GroupDoc struct {
	Description string           `json:"description,omitempty" toml:"description,omitempty"`
	Workspaces  []string         `json:"workspaces,omitempty" toml:"workspaces,omitempty"`
	Patterns    []string         `json:"patterns,omitempty" toml:"patterns,omitempty"`
	Policy      *policy.Policy   `json:"policy,omitempty" toml:"policy,omitempty"`
	Pipeline    *PipelineDoc     `json:"pipeline,omitempty" toml:"pipeline,omitempty"`
	Config      *wsconfig.Config `json:"config,omitempty" toml:"config,omitempty"`
}

// PipelineDoc is pipeline settings in the file's form.
type PipelineDoc struct {
	Stages              map[string]StageDoc `json:"stages,omitempty" toml:"stages,omitempty"`
	AgentsDefault       string              `json:"agents_default,omitempty" toml:"agents_default,omitempty"`
	SecondOpinionRunner string              `json:"second_opinion_runner,omitempty" toml:"second_opinion_runner,omitempty"`
	CodeIndex           *bool               `json:"code_index,omitempty" toml:"code_index,omitempty"`
}

// StageDoc is one stage: its members as "[focus@]runner,…" and, for a
// stage with several, how it is decided.
type StageDoc struct {
	Members   string `json:"members" toml:"members"`
	Policy    string `json:"policy,omitempty" toml:"policy,omitempty"`
	EarlyExit bool   `json:"early_exit,omitempty" toml:"early_exit,omitempty"`
	Timeout   string `json:"timeout,omitempty" toml:"timeout,omitempty"`
}

func pipelineDoc(st pipeline.Settings) *PipelineDoc {
	if len(st.Stages) == 0 && st.AgentsDefault == "" && st.SecondOpinionRunner == "" && st.CodeIndex == nil {
		return nil
	}
	d := &PipelineDoc{AgentsDefault: st.AgentsDefault, SecondOpinionRunner: st.SecondOpinionRunner, CodeIndex: st.CodeIndex}
	for stage, s := range st.Stages {
		if d.Stages == nil {
			d.Stages = map[string]StageDoc{}
		}
		sd := StageDoc{Members: pipeline.FormatMembers(s.Members), Policy: s.Policy, EarlyExit: s.EarlyExit}
		if s.TimeoutMS > 0 {
			sd.Timeout = (time.Duration(s.TimeoutMS) * time.Millisecond).String()
		}
		d.Stages[stage] = sd
	}
	return d
}

func (d *PipelineDoc) settings() (pipeline.Settings, error) {
	st := pipeline.Settings{Stages: map[string]pipeline.StageSetting{}}
	if d == nil {
		return st, nil
	}
	st.AgentsDefault, st.SecondOpinionRunner, st.CodeIndex = d.AgentsDefault, d.SecondOpinionRunner, d.CodeIndex
	for stage, sd := range d.Stages {
		members, err := pipeline.ParseMembers(sd.Members)
		if err != nil {
			return st, fmt.Errorf("stage %s: %w", stage, err)
		}
		set := pipeline.StageSetting{Members: members, Policy: sd.Policy, EarlyExit: sd.EarlyExit}
		if sd.Timeout != "" {
			t, err := time.ParseDuration(sd.Timeout)
			if err != nil {
				return st, fmt.Errorf("stage %s: timeout %q: %v", stage, sd.Timeout, err)
			}
			set.TimeoutMS = t.Milliseconds()
		}
		st.Stages[stage] = set
	}
	return st, nil
}

// Change is one thing an apply does (or, in a plan, would do).
type Change struct {
	Kind   string `json:"kind"`   // runner | workspace | group | policy | pipeline | config
	Owner  string `json:"owner"`  // fleet:ceiling | group:<name> | workspace:<id> | runner:<name>
	Action string `json:"action"` // create | update | clear | delete
	Detail string `json:"detail,omitempty"`
	// Revision: the revision the apply created (policies, config, runner
	// definitions).
	Revision string   `json:"revision,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Result is what an apply or a plan answers.
type Result struct {
	Applied bool     `json:"applied"`
	Changes []Change `json:"changes"`
	// Unlisted: workspaces and groups that have settings on the server and
	// are not in the file — left as they are.
	Unlisted []string `json:"unlisted,omitempty"`
	FileHash string   `json:"file_hash,omitempty"`
	Commit   string   `json:"commit,omitempty"`
}

// Input is an apply request.
type Input struct {
	Document Document `json:"document"`
	// FileHash: sha256 of the file as it is on disk; Commit: the git commit
	// it comes from. Both are recorded with the apply.
	FileHash string `json:"file_hash,omitempty"`
	Commit   string `json:"commit,omitempty"`
	// Force saves a policy that strands current settings.
	Force bool `json:"force,omitempty"`
	// Plan lists the changes without making them.
	Plan bool `json:"plan,omitempty"`
}

// Groups and Workspaces are what the service needs of the group and
// workspace registries (apikeys.Service, workspaces.Service).
type Groups interface {
	ListGroups(ctx context.Context) ([]apikeys.Group, error)
	CreateGroup(ctx context.Context, in apikeys.GroupInput) (*apikeys.Group, error)
	UpdateGroup(ctx context.Context, id string, in apikeys.GroupUpdateInput) (*apikeys.Group, error)
}

type Workspaces interface {
	List(ctx context.Context) ([]workspaces.Workspace, error)
	Create(ctx context.Context, in workspaces.CreateInput) (*workspaces.Workspace, bool, error)
}

// AuditSink records an apply (audit.Service in production).
type AuditSink interface {
	Record(ctx context.Context, in audit.RecordInput) error
}

type Service struct {
	groups     Groups
	workspaces Workspaces
	pipeline   *pipeline.Service
	config     *wsconfig.Service
	audit      AuditSink
}

func NewService(groups Groups, ws Workspaces, pl *pipeline.Service, cfg *wsconfig.Service) *Service {
	return &Service{groups: groups, workspaces: ws, pipeline: pl, config: cfg}
}

func (s *Service) SetAuditSink(a AuditSink) { s.audit = a }

// system is the context reads run with once the caller has been checked:
// the registries' own listings are root-only.
func system(ctx context.Context) context.Context { return auth.WithPrincipal(ctx, nil) }

// Export returns everything as one document. Fleet admins only: it lists
// every group's members and every workspace's commands.
func (s *Service) Export(ctx context.Context) (*Document, error) {
	if err := auth.RequireFleetAdmin(ctx); err != nil {
		return nil, err
	}
	sys := system(ctx)
	doc := &Document{Runners: map[string]wsconfig.RunnerDef{}, Wrappers: map[string]wsconfig.WrapperDef{}, Fleet: &FleetDoc{}}
	runners, err := s.config.Runners(sys)
	if err != nil {
		return nil, err
	}
	for _, r := range runners {
		doc.Runners[r.Name] = r.RunnerDef
	}
	wrappers, err := s.config.Wrappers(sys)
	if err != nil {
		return nil, err
	}
	for _, w := range wrappers {
		doc.Wrappers[w.Name] = w.WrapperDef
	}
	// The definitions only: which secrets each names, never a value.
	doc.MCP = map[string]wsconfig.MCPServerDef{}
	servers, err := s.config.MCPServers(sys)
	if err != nil {
		return nil, err
	}
	for _, m := range servers {
		doc.MCP[m.Name] = m.MCPServerDef
	}
	for kind, dst := range map[string]**policy.Policy{pipeline.KindFleetCeiling: &doc.Fleet.Ceiling, pipeline.KindFleetDefault: &doc.Fleet.Default} {
		v, err := s.pipeline.GetPolicy(sys, kind, "")
		if err != nil {
			return nil, err
		}
		*dst = v.Policy
	}
	groups, err := s.groups.ListGroups(sys)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		gd := &GroupDoc{Description: g.Description, Workspaces: slices.Clone(g.Members), Patterns: slices.Clone(g.Patterns)}
		sort.Strings(gd.Workspaces)
		sort.Strings(gd.Patterns)
		o, err := s.owner(sys, pipeline.KindGroup, g.ID)
		if err != nil {
			return nil, err
		}
		gd.Policy, gd.Pipeline, gd.Config = o.Policy, o.Pipeline, o.Config
		if doc.Groups == nil {
			doc.Groups = map[string]*GroupDoc{}
		}
		doc.Groups[g.Name] = gd
	}
	all, err := s.workspaces.List(sys)
	if err != nil {
		return nil, err
	}
	for _, w := range all {
		o, err := s.owner(sys, pipeline.KindWorkspace, w.ID)
		if err != nil {
			return nil, err
		}
		if o.Policy == nil && o.Pipeline == nil && o.Config == nil {
			continue // nothing set: listing it would say nothing
		}
		if doc.Workspaces == nil {
			doc.Workspaces = map[string]*OwnerDoc{}
		}
		doc.Workspaces[w.ID] = o
	}
	return doc, nil
}

// owner reads what a workspace or group has.
func (s *Service) owner(ctx context.Context, kind, id string) (*OwnerDoc, error) {
	o := &OwnerDoc{}
	pv, err := s.pipeline.GetPolicy(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	o.Policy = pv.Policy
	st, err := s.pipeline.Own(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	o.Pipeline = pipelineDoc(st)
	cv, err := s.config.Get(ctx, kind, id)
	if err != nil {
		return nil, err
	}
	o.Config = cv.Config
	return o, nil
}

// Notes a plan attaches to a change it could not check in full.
const (
	noteFresh  = "created by this apply: checked in full when it is applied"
	notePolicy = "the server's current policy refuses it; this file changes a policy, so it is checked again when applied"
)

// step is one change: plan says what it would do ("" action = nothing),
// run does it.
type step struct {
	kind, owner string
	plan        func(ctx context.Context) (action, detail string, warnings []string, err error)
	run         func(ctx context.Context) (revision string, err error)
}

// Apply makes the server match the document, or with in.Plan lists what
// that would change. Every step is checked first (permissions, validation,
// conflicts, what a policy strands); nothing is written unless all of them
// pass. Each change needs the permission its own save needs: the admin
// permission at the owner's level, root for a group's membership.
func (s *Service) Apply(ctx context.Context, in Input) (*Result, error) {
	if p := auth.PrincipalFromContext(ctx); p != nil && !p.Root && (!p.Admin || p.Run != nil) {
		return nil, auth.ErrAdminRequired
	}
	if len(in.FileHash) > 128 || len(in.Commit) > 128 {
		return nil, fmt.Errorf("%w: file_hash and commit are at most 128 characters", ErrInvalid)
	}
	if in.Document.Wrappers != nil {
		// The file states the full set of wrappers: every config in it is
		// checked against the set it leaves behind, not the stored one.
		ctx = wsconfig.WithWrapperSet(ctx, sortedKeys(in.Document.Wrappers))
	}
	if in.Document.MCP != nil {
		ctx = wsconfig.WithMCPSet(ctx, in.Document.MCP)
	}
	steps, unlisted, err := s.steps(ctx, in)
	if err != nil {
		return nil, err
	}
	res := &Result{Changes: []Change{}, Unlisted: unlisted, FileHash: in.FileHash, Commit: in.Commit}
	var todo []step
	policyChanges := false
	for _, st := range steps {
		action, detail, warnings, err := st.plan(ctx)
		// A pipeline the current policy refuses may be fine under the
		// policy this same file sets: the steps run in order, so it is
		// decided when it is applied.
		if err != nil && st.kind == "pipeline" && policyChanges && errors.Is(err, pipeline.ErrPolicyRefuses) {
			action, detail, warnings, err = "update", "", []string{notePolicy}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", st.kind, st.owner, err)
		}
		if action == "" {
			continue
		}
		if st.kind == "policy" {
			policyChanges = true
		}
		res.Changes = append(res.Changes, Change{Kind: st.kind, Owner: st.owner, Action: action, Detail: detail, Warnings: warnings})
		todo = append(todo, st)
	}
	if in.Plan {
		return res, nil
	}
	for i, st := range todo {
		rev, err := st.run(ctx)
		if err != nil {
			s.record(ctx, in, res.Changes[:i], fmt.Sprintf("stopped at %s %s: %v", st.kind, st.owner, err))
			return nil, fmt.Errorf("%s %s (%d of %d changes were applied before it): %w", st.kind, st.owner, i, len(todo), err)
		}
		res.Changes[i].Revision = rev
	}
	res.Applied = true
	s.record(ctx, in, res.Changes, "")
	return res, nil
}

func (s *Service) record(ctx context.Context, in Input, done []Change, failure string) {
	if s.audit == nil || (len(done) == 0 && failure == "") {
		return
	}
	parts := make([]string, len(done))
	for i, c := range done {
		parts[i] = c.Action + " " + c.Kind + " " + c.Owner
	}
	notes := fmt.Sprintf("file sha256 %s, commit %s: %d changes", orNone(in.FileHash), orNone(in.Commit), len(done))
	if len(parts) > 0 {
		notes += ": " + strings.Join(parts, "; ")
	}
	if failure != "" {
		notes += " — " + failure
	}
	if len(notes) > 4000 {
		notes = notes[:4000] + "…"
	}
	id := in.FileHash
	if id == "" {
		id = "unknown"
	}
	_ = s.audit.Record(ctx, audit.RecordInput{EntityType: AuditEntity, EntityID: id, Action: "config.apply", Via: audit.ViaApply, Notes: notes})
}

func orNone(s string) string {
	if s == "" {
		return "not given"
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// steps lays out everything the document asks for, in the order it is
// applied: runner definitions, workspaces to register, groups, then
// policies, config and pipelines (a pipeline save is checked against the
// policy, so policies come first).
func (s *Service) steps(ctx context.Context, in Input) ([]step, []string, error) {
	doc := in.Document
	sys := system(ctx)
	var out []step

	// A key that does not administer the whole fleet may only name what it
	// administers, and learns nothing else: the same refusal for a
	// workspace or group that does not exist as for one out of its reach.
	fleetAdmin := auth.RequireFleetAdmin(ctx) == nil
	if !fleetAdmin {
		for _, id := range sortedKeys(doc.Workspaces) {
			if err := auth.RequireAdmin(ctx, id); err != nil {
				return nil, nil, fmt.Errorf("%w for workspace %s", auth.ErrAdminRequired, id)
			}
		}
		for _, name := range sortedKeys(doc.Groups) {
			members, found, err := s.config.GroupMembers(sys, name)
			if err != nil {
				return nil, nil, err
			}
			if !found || auth.RequireAdminAll(ctx, members) != nil {
				return nil, nil, fmt.Errorf("%w for group %s (the key must reach every workspace in it)", auth.ErrAdminRequired, name)
			}
		}
	}

	if doc.Runners != nil {
		have, err := s.config.Runners(sys)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range sortedKeys(doc.Runners) {
			def := doc.Runners[name]
			def.Name = name
			out = append(out, step{kind: "runner", owner: "runner:" + name,
				plan: func(ctx context.Context) (string, string, []string, error) {
					action, err := s.config.PlanRunner(ctx, def)
					return action, describeRunner(def), nil, err
				},
				run: func(ctx context.Context) (string, error) {
					v, _, err := s.config.SetRunner(ctx, def, audit.ViaApply)
					if err != nil {
						return "", err
					}
					return v.Rev, nil
				}})
		}
		for _, r := range have {
			if _, keep := doc.Runners[r.Name]; keep {
				continue
			}
			out = append(out, step{kind: "runner", owner: "runner:" + r.Name,
				plan: func(ctx context.Context) (string, string, []string, error) {
					return "delete", "", nil, auth.RequireFleetAdmin(ctx)
				},
				run: func(ctx context.Context) (string, error) {
					_, err := s.config.DeleteRunner(ctx, r.Name, audit.ViaApply)
					return "", err
				}})
		}
	}

	// Wrappers the file defines come before the configs that name them;
	// the ones it drops go last, after those configs have changed.
	var dropWrappers []wsconfig.WrapperView
	if doc.Wrappers != nil {
		have, err := s.config.Wrappers(sys)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range sortedKeys(doc.Wrappers) {
			def := doc.Wrappers[name]
			def.Name = name
			out = append(out, step{kind: "wrapper", owner: "wrapper:" + name,
				plan: func(ctx context.Context) (string, string, []string, error) {
					action, err := s.config.PlanWrapper(ctx, def)
					return action, describeWrapper(def), nil, err
				},
				run: func(ctx context.Context) (string, error) {
					v, _, err := s.config.SetWrapper(ctx, def, audit.ViaApply)
					if err != nil {
						return "", err
					}
					return v.Rev, nil
				}})
		}
		for _, w := range have {
			if _, keep := doc.Wrappers[w.Name]; !keep {
				dropWrappers = append(dropWrappers, w)
			}
		}
	}

	// MCP servers, like wrappers: defined before the configs that name
	// them, dropped after.
	var dropMCP []wsconfig.MCPServerView
	if doc.MCP != nil {
		have, err := s.config.MCPServers(sys)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range sortedKeys(doc.MCP) {
			def := doc.MCP[name]
			def.Name = name
			out = append(out, step{kind: "mcp", owner: "mcp:" + name,
				plan: func(ctx context.Context) (string, string, []string, error) {
					action, err := s.config.PlanMCPServer(ctx, def)
					if err != nil || action == "" {
						return action, "", nil, err
					}
					// A secret the definition names and the server has no
					// value for: said in the plan, since the file cannot
					// carry it.
					var warnings []string
					unset := def.SecretNames()
					for _, m := range have {
						if m.Name != name {
							continue
						}
						for _, st := range m.Secrets {
							if st.Set {
								unset = slices.DeleteFunc(unset, func(n string) bool { return n == st.Name })
							}
						}
					}
					if len(unset) > 0 {
						warnings = []string{"no value is stored for secret " + strings.Join(unset, ", ") + ": set it with `skopos mcp-server set-secret " + name + " <secret>`; until then runs go without this server"}
					}
					return action, describeMCP(def), warnings, nil
				},
				run: func(ctx context.Context) (string, error) {
					v, _, err := s.config.SetMCPServer(ctx, def, audit.ViaApply)
					if err != nil {
						return "", err
					}
					return v.Rev, nil
				}})
		}
		for _, m := range have {
			if _, keep := doc.MCP[m.Name]; !keep {
				dropMCP = append(dropMCP, m)
			}
		}
	}

	// Workspaces the file names that are not registered yet.
	registered := map[string]bool{}
	all, err := s.workspaces.List(sys)
	if err != nil {
		return nil, nil, err
	}
	for _, w := range all {
		registered[w.ID] = true
	}
	newWS := map[string]bool{}
	want := sortedKeys(doc.Workspaces)
	for _, g := range sortedKeys(doc.Groups) {
		if doc.Groups[g] == nil {
			return nil, nil, fmt.Errorf("%w: group %s is empty", ErrInvalid, g)
		}
		want = append(want, doc.Groups[g].Workspaces...)
	}
	for _, id := range want {
		id = strings.TrimSpace(id)
		if id == "" || registered[id] || newWS[id] {
			continue
		}
		newWS[id] = true
		out = append(out, step{kind: "workspace", owner: "workspace:" + id,
			plan: func(ctx context.Context) (string, string, []string, error) {
				return "create", "register the workspace", nil, auth.RequireRoot(ctx)
			},
			run: func(ctx context.Context) (string, error) {
				_, _, err := s.workspaces.Create(ctx, workspaces.CreateInput{ID: id})
				return "", err
			}})
	}

	groups, err := s.groups.ListGroups(sys)
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]apikeys.Group{}
	for _, g := range groups {
		byName[g.Name] = g
	}
	newGroup := map[string]bool{}
	for _, name := range sortedKeys(doc.Groups) {
		gd := doc.Groups[name]
		members, patterns := sortedCopy(gd.Workspaces), sortedCopy(gd.Patterns)
		cur, exists := byName[name]
		if !exists {
			newGroup[name] = true
			out = append(out, step{kind: "group", owner: "group:" + name,
				plan: func(ctx context.Context) (string, string, []string, error) {
					return "create", describeGroup(members, patterns), nil, auth.RequireRoot(ctx)
				},
				run: func(ctx context.Context) (string, error) {
					_, err := s.groups.CreateGroup(apikeys.WithVia(ctx, audit.ViaApply), apikeys.GroupInput{Name: name, Description: gd.Description, Members: members, Patterns: patterns})
					return "", err
				}})
			continue
		}
		if cur.Description == gd.Description && slices.Equal(sortedCopy(cur.Members), members) && slices.Equal(sortedCopy(cur.Patterns), patterns) {
			continue
		}
		out = append(out, step{kind: "group", owner: "group:" + name,
			plan: func(ctx context.Context) (string, string, []string, error) {
				return "update", describeGroup(members, patterns), nil, auth.RequireRoot(ctx)
			},
			run: func(ctx context.Context) (string, error) {
				_, err := s.groups.UpdateGroup(apikeys.WithVia(ctx, audit.ViaApply), cur.ID, apikeys.GroupUpdateInput{Description: &gd.Description, Members: members, Patterns: patterns})
				return "", err
			}})
	}

	// Policies, then config, then pipelines; the fleet first, then groups,
	// then workspaces.
	type owner struct {
		kind, id, label string
		doc             OwnerDoc
		fresh           bool     // created by this apply: nothing to compare with yet
		members         []string // a group's workspaces, as the file lists them
	}
	var owners []owner
	for _, name := range sortedKeys(doc.Groups) {
		gd := doc.Groups[name]
		owners = append(owners, owner{pipeline.KindGroup, name, "group:" + name, OwnerDoc{gd.Policy, gd.Pipeline, gd.Config}, newGroup[name], sortedCopy(gd.Workspaces)})
	}
	for _, id := range sortedKeys(doc.Workspaces) {
		od := doc.Workspaces[id]
		if od == nil {
			od = &OwnerDoc{}
		}
		owners = append(owners, owner{pipeline.KindWorkspace, id, "workspace:" + id, *od, newWS[id], nil})
	}
	policyStep := func(kind, id, label string, next *policy.Policy, fresh bool) step {
		return step{kind: "policy", owner: label,
			plan: func(ctx context.Context) (string, string, []string, error) {
				if fresh {
					if next == nil {
						return "", "", nil, nil
					}
					norm, err := policy.Normalize(*next)
					if err != nil {
						return "", "", nil, fmt.Errorf("%w: %v", ErrInvalid, err)
					}
					if len(norm.Allow) == 0 {
						return "", "", nil, fmt.Errorf("%w: a policy with no rules allows nothing; leave the section out for no policy", ErrInvalid)
					}
					return "create", describePolicy(&norm), []string{noteFresh}, nil
				}
				changed, warnings, err := s.pipeline.PlanPolicy(ctx, kind, id, next, in.Force)
				if err != nil || !changed {
					return "", "", nil, err
				}
				if next == nil {
					return "clear", "", warnings, nil
				}
				return "update", describePolicy(next), warnings, nil
			},
			run: func(ctx context.Context) (string, error) {
				var v *pipeline.PolicyView
				var err error
				if next == nil {
					v, err = s.pipeline.ClearPolicy(ctx, kind, id, in.Force, audit.ViaApply)
				} else {
					v, err = s.pipeline.SetPolicy(ctx, kind, id, *next, in.Force, audit.ViaApply)
				}
				if err != nil {
					return "", err
				}
				return v.Rev, nil
			}}
	}
	if doc.Fleet != nil {
		out = append(out, policyStep(pipeline.KindFleetCeiling, "", "fleet:ceiling", doc.Fleet.Ceiling, false),
			policyStep(pipeline.KindFleetDefault, "", "fleet:default", doc.Fleet.Default, false))
	}
	for _, o := range owners {
		out = append(out, policyStep(o.kind, o.id, o.label, o.doc.Policy, o.fresh))
	}
	for _, o := range owners {
		next := o.doc.Config
		out = append(out, step{kind: "config", owner: o.label,
			plan: func(ctx context.Context) (string, string, []string, error) {
				if o.fresh {
					if next == nil {
						return "", "", nil, nil
					}
					fields, err := s.config.PlanNew(ctx, o.kind, o.id, o.members, *next)
					return "create", strings.Join(fields, ", "), []string{noteFresh}, err
				}
				fields, err := s.config.Plan(ctx, o.kind, o.id, next)
				if err != nil || len(fields) == 0 {
					return "", "", nil, err
				}
				if next == nil {
					return "clear", configCleared(o.kind), nil, nil
				}
				return "update", strings.Join(fields, ", "), nil, nil
			},
			run: func(ctx context.Context) (string, error) {
				var v *wsconfig.View
				var err error
				if next == nil {
					v, err = s.config.Clear(ctx, o.kind, o.id, audit.ViaApply)
				} else {
					v, err = s.config.Set(ctx, o.kind, o.id, *next, audit.ViaApply, noteOf(in))
				}
				if err != nil {
					return "", err
				}
				return v.Rev, nil
			}})
	}
	for _, o := range owners {
		st, err := o.doc.Pipeline.settings()
		if err != nil {
			return nil, nil, fmt.Errorf("%w: pipeline of %s: %v", ErrInvalid, o.label, err)
		}
		out = append(out, step{kind: "pipeline", owner: o.label,
			plan: func(ctx context.Context) (string, string, []string, error) {
				if o.fresh {
					norm, err := pipeline.Normalize(st)
					if err != nil || len(norm.Stages) == 0 && norm.AgentsDefault == "" && norm.SecondOpinionRunner == "" && norm.CodeIndex == nil {
						return "", "", nil, err
					}
					return "create", describePipeline(norm), []string{noteFresh}, nil
				}
				// Whether it differs comes first, read as the server: a
				// pipeline the file leaves as it is needs no permission
				// (a group's needs root, a workspace's the approver
				// permission), and the caller administers the owner.
				if same, err := s.pipeline.PlanPut(sys, o.kind, o.id, st); err == nil && same == "" {
					return "", "", nil, nil
				}
				changes, err := s.pipeline.PlanPut(ctx, o.kind, o.id, st)
				if err != nil || changes == "" {
					return "", "", nil, err
				}
				return "update", changes, nil, nil
			},
			run: func(ctx context.Context) (string, error) {
				_, err := s.pipeline.Put(ctx, o.kind, o.id, st, audit.ViaApply)
				return "", err
			}})
	}

	// Wrappers the file drops. The configs the file lists were checked
	// against the file's own set; what still names a dropped wrapper is a
	// workspace or group the file does not list.
	if len(dropWrappers) > 0 {
		users, err := s.config.WrapperUsers(sys)
		if err != nil {
			return nil, nil, err
		}
		listed := map[string]bool{}
		for _, id := range sortedKeys(doc.Workspaces) {
			listed["workspace:"+id] = true
		}
		groupName := map[string]string{}
		for _, g := range groups {
			groupName["group:"+g.ID] = "group:" + g.Name
			if _, ok := doc.Groups[g.Name]; ok {
				listed["group:"+g.ID] = true
			}
		}
		for _, w := range dropWrappers {
			var still []string
			for _, owner := range users[w.Name] {
				if !listed[owner] {
					still = append(still, cmp.Or(groupName[owner], owner))
				}
			}
			out = append(out, step{kind: "wrapper", owner: "wrapper:" + w.Name,
				plan: func(ctx context.Context) (string, string, []string, error) {
					if err := auth.RequireFleetAdmin(ctx); err != nil {
						return "", "", nil, err
					}
					if len(still) > 0 && !in.Force {
						return "", "", nil, fmt.Errorf("%w: the file drops wrapper %s, and the stage blocks of %s (not in the file) name it: runs their rules match would stop. Keep the wrapper, change those, or force the apply (--force)", ErrInvalid, w.Name, strings.Join(still, ", "))
					}
					var warnings []string
					if len(still) > 0 {
						warnings = []string{"still named by " + strings.Join(still, ", ") + ": runs their rules match stop until the rule goes"}
					}
					return "delete", "", warnings, nil
				},
				run: func(ctx context.Context) (string, error) {
					_, err := s.config.DeleteWrapper(ctx, w.Name, audit.ViaApply, true)
					return "", err
				}})
		}
	}

	// MCP servers the file drops, the same way: what still gives its runs
	// a dropped server is a workspace or group the file does not list.
	if len(dropMCP) > 0 {
		users, err := s.config.MCPUsers(sys)
		if err != nil {
			return nil, nil, err
		}
		listed := map[string]bool{}
		for _, id := range sortedKeys(doc.Workspaces) {
			listed["workspace:"+id] = true
		}
		groupName := map[string]string{}
		for _, g := range groups {
			groupName["group:"+g.ID] = "group:" + g.Name
			if _, ok := doc.Groups[g.Name]; ok {
				listed["group:"+g.ID] = true
			}
		}
		for _, m := range dropMCP {
			var still []string
			for _, owner := range users[m.Name] {
				if !listed[owner] {
					still = append(still, cmp.Or(groupName[owner], owner))
				}
			}
			out = append(out, step{kind: "mcp", owner: "mcp:" + m.Name,
				plan: func(ctx context.Context) (string, string, []string, error) {
					if err := auth.RequireFleetAdmin(ctx); err != nil {
						return "", "", nil, err
					}
					if len(still) > 0 && !in.Force {
						return "", "", nil, fmt.Errorf("%w: the file drops MCP server %s, and the config of %s (not in the file) gives its runs that server. Keep it, change those, or force the apply (--force)", ErrInvalid, m.Name, strings.Join(still, ", "))
					}
					warnings := []string{"its stored secrets are deleted with it"}
					if len(still) > 0 {
						warnings = append(warnings, "still given to the runs of "+strings.Join(still, ", ")+": they go without it")
					}
					return "delete", "", warnings, nil
				},
				run: func(ctx context.Context) (string, error) {
					_, err := s.config.DeleteMCPServer(ctx, m.Name, audit.ViaApply, true)
					return "", err
				}})
		}
	}

	// What has settings on the server and is not in the file.
	// Only what the caller may know of: groups for a key that administers
	// the fleet, workspaces in the key's scope.
	var unlisted []string
	if fleetAdmin {
		for _, g := range groups {
			if _, ok := doc.Groups[g.Name]; !ok {
				unlisted = append(unlisted, "group:"+g.Name)
			}
		}
	}
	for _, w := range all {
		if _, ok := doc.Workspaces[w.ID]; ok {
			continue
		}
		if !fleetAdmin && auth.RequireWorkspaceQuiet(ctx, w.ID) != nil {
			continue
		}
		o, err := s.owner(sys, pipeline.KindWorkspace, w.ID)
		if err != nil {
			return nil, nil, err
		}
		if o.Policy != nil || o.Pipeline != nil || o.Config != nil {
			unlisted = append(unlisted, "workspace:"+w.ID)
		}
	}
	sort.Strings(unlisted)
	return out, unlisted, nil
}

func noteOf(in Input) string {
	if in.Commit == "" {
		return ""
	}
	return "commit " + in.Commit
}

func sortedCopy(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func configCleared(kind string) string {
	if kind == pipeline.KindWorkspace {
		return "the workspace is no longer managed"
	}
	return ""
}

// describeMCP renders an MCP server definition for a plan line: it holds
// secret names, never values.
func describeMCP(d wsconfig.MCPServerDef) string {
	s := d.Type + " " + d.URL
	if d.Type == wsconfig.MCPCommand {
		s = "command " + strings.Join(d.Command, " ")
	}
	if names := d.SecretNames(); len(names) > 0 {
		s += "; secrets " + strings.Join(names, ", ")
	}
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

func describeWrapper(d wsconfig.WrapperDef) string {
	s := "argv " + strings.Join(d.Argv, " ")
	if len(d.Check) > 0 {
		s += "; check " + strings.Join(d.Check, " ")
	}
	if len(s) > 600 {
		s = s[:600] + "…"
	}
	return s
}

func describeRunner(d wsconfig.RunnerDef) string {
	parts := []string{"kind " + d.Kind}
	for _, kv := range [][2]string{{"model", d.Model}, {"effort", d.Effort}, {"provider", d.Provider}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+" "+kv[1])
		}
	}
	return strings.Join(parts, ", ")
}

func describeGroup(members, patterns []string) string {
	var parts []string
	if len(members) > 0 {
		parts = append(parts, "workspaces "+strings.Join(members, ", "))
	}
	if len(patterns) > 0 {
		parts = append(parts, "patterns "+strings.Join(patterns, ", "))
	}
	return strings.Join(parts, "; ")
}

func describePolicy(p *policy.Policy) string {
	rules := make([]string, len(p.Allow))
	for i, r := range p.Allow {
		rules[i] = r.String()
	}
	return "allow " + strings.Join(rules, "; ")
}

func describePipeline(st pipeline.Settings) string {
	var parts []string
	for _, stage := range pipeline.Stages {
		if set, ok := st.Stages[stage]; ok {
			parts = append(parts, stage+" "+pipeline.FormatMembers(set.Members))
		}
	}
	if st.AgentsDefault != "" {
		parts = append(parts, "agents default "+st.AgentsDefault)
	}
	if st.SecondOpinionRunner != "" {
		parts = append(parts, "second opinion "+st.SecondOpinionRunner)
	}
	if st.CodeIndex != nil {
		parts = append(parts, fmt.Sprintf("code index %v", *st.CodeIndex))
	}
	return strings.Join(parts, "; ")
}
