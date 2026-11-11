package fleetfile

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	"github.com/martinsuchenak/skopos/internal/wsconfig"

	_ "modernc.org/sqlite"
)

type recSink struct {
	mu      sync.Mutex
	entries []audit.RecordInput
}

func (r *recSink) Record(_ context.Context, in audit.RecordInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, in)
	return nil
}

func (r *recSink) by(action string) []audit.RecordInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []audit.RecordInput
	for _, e := range r.entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

type fixtureT struct {
	svc    *Service
	sink   *recSink
	config *wsconfig.Service
	pipe   *pipeline.Service
}

func fixture(t *testing.T) *fixtureT {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "f.db") + "?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	sink := &recSink{}
	revs := confrev.NewStore(sqlDB)
	storage := pipeline.NewStorage(sqlDB)
	pipe := pipeline.NewService(storage)
	pipe.SetRevisions(revs)
	pipe.SetAuditSink(sink)
	cfg := wsconfig.NewService(sqlDB, revs, storage)
	cfg.SetAuditSink(sink)
	pipe.SetRunnerDefs(cfg.RunnerFacts)
	svc := NewService(apikeys.NewService(apikeys.NewStorage(sqlDB)), workspaces.NewService(workspaces.NewStorage(sqlDB)), pipe, cfg)
	svc.SetAuditSink(sink)
	return &fixtureT{svc: svc, sink: sink, config: cfg, pipe: pipe}
}

func root() context.Context { return context.Background() }

func admin(all bool, ws ...string) context.Context {
	set := map[string]struct{}{}
	for _, w := range ws {
		set[w] = struct{}{}
	}
	return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k1", Name: "ops", Admin: true, AllWorkspaces: all, Workspaces: set})
}

const sample = `
[fleet.default]
allow = [{ provider = "anthropic" }, { provider = "local" }]

[runners.claude-opus]
kind = "claude"
model = "opus"
effort = "high"
provider = "anthropic"

[runners.glm]
kind = "opencode"
model = "zai-coding-plan/glm-5.3"
provider = "zai"

[groups.fortix]
description = "Fortix repositories"
workspaces = ["fx/api"]
patterns = ["fx/*"]

[groups.fortix.policy]
allow = [{ provider = "anthropic" }, { provider = "openai", stages = ["second_opinion"] }]

[groups.fortix.config]
default_branch = "develop"
check_timeout = "45m0s"

[groups.fortix.pipeline]
second_opinion_runner = "codex"
[groups.fortix.pipeline.stages]
review = { members = "security@claude-opus,tests@claude-opus", policy = "any-blocks", timeout = "30m0s" }

[workspaces."fx/api"]
[workspaces."fx/api".config]
setup = ["npm ci"]
checks = ["npm test"]
[workspaces."fx/api".config.push]
kind = "github"
remote_url = "https://github.com/fx/api.git"
pull_request = "draft"
[workspaces."fx/api".pipeline]
code_index = true
[workspaces."fx/api".pipeline.stages]
plan = { members = "claude-opus" }
`

func parse(t *testing.T, text string) Document {
	t.Helper()
	var doc Document
	md, err := toml.Decode(text, &doc)
	if err != nil {
		t.Fatal(err)
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		t.Fatalf("the file has keys the document does not read: %v", extra)
	}
	return doc
}

func lines(res *Result) string {
	var out []string
	for _, c := range res.Changes {
		out = append(out, c.Action+" "+c.Kind+" "+c.Owner)
	}
	return strings.Join(out, "\n")
}

// A plan lists every change and makes none; the apply makes them, records
// the file's hash and commit, and a second apply of the same file changes
// nothing. An export then reads back as the same document.
func TestPlanApplyExportRoundTrip(t *testing.T) {
	f := fixture(t)
	doc := parse(t, sample)
	in := Input{Document: doc, FileHash: "abc123", Commit: "deadbeef", Plan: true}
	plan, err := f.svc.Apply(root(), in)
	if err != nil {
		t.Fatal(err)
	}
	want := `create runner runner:claude-opus
create runner runner:glm
create workspace workspace:fx/api
create group group:fortix
update policy fleet:default
create policy group:fortix
create config group:fortix
create config workspace:fx/api
create pipeline group:fortix
create pipeline workspace:fx/api`
	if got := lines(plan); got != want || plan.Applied {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
	if n := len(f.sink.entries); n != 0 {
		t.Fatalf("a plan writes nothing: %d audit events", n)
	}
	if eff, _ := f.config.Effective(root(), "fx/api"); eff.Managed {
		t.Fatal("a plan must not manage the workspace")
	}

	in.Plan = false
	res, err := f.svc.Apply(root(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || lines(res) != want {
		t.Fatalf("apply:\n%s", lines(res))
	}
	for _, c := range res.Changes {
		if (c.Kind == "config" || c.Kind == "policy" || c.Kind == "runner") && c.Revision == "" {
			t.Fatalf("%s %s: the created revision is reported", c.Kind, c.Owner)
		}
	}
	applied := f.sink.by("config.apply")
	if len(applied) != 1 || applied[0].EntityType != AuditEntity || applied[0].EntityID != "abc123" || applied[0].Via != audit.ViaApply ||
		!strings.Contains(applied[0].Notes, "file sha256 abc123, commit deadbeef: 10 changes") {
		t.Fatalf("the apply is recorded with where it came from: %+v", applied)
	}
	// Every change inside it is recorded by its own service, via apply.
	for _, action := range []string{"runner_def.create", "policy.set", "config.update", "pipeline.update"} {
		got := f.sink.by(action)
		if len(got) == 0 {
			t.Fatalf("no %s event", action)
		}
		for _, e := range got {
			if e.Via != audit.ViaApply {
				t.Fatalf("%s via %q, want apply", action, e.Via)
			}
		}
	}
	eff, err := f.config.Effective(root(), "fx/api")
	if err != nil || !eff.Managed || *eff.Config.DefaultBranch != "develop" || (*eff.Config.Checks)[0] != "npm test" || eff.Config.CheckTimeout.D().Minutes() != 45 {
		t.Fatalf("the workspace's config, with the group's under it: %+v %v", eff, err)
	}

	// Again: nothing to do, nothing recorded.
	before := len(f.sink.entries)
	again, err := f.svc.Apply(root(), in)
	if err != nil || len(again.Changes) != 0 || len(f.sink.entries) != before {
		t.Fatalf("a second apply is a no-op: %v %s", err, lines(again))
	}

	// Export, write as TOML, read back, apply: still nothing to do.
	exported, err := f.svc.Export(root())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(exported); err != nil {
		t.Fatal(err)
	}
	back := parse(t, buf.String())
	round, err := f.svc.Apply(root(), Input{Document: back, Plan: true})
	if err != nil || len(round.Changes) != 0 {
		t.Fatalf("export then apply must change nothing: %v\n%s\n--- exported ---\n%s", err, lines(round), buf.String())
	}
	// The same through JSON, the form the API carries.
	raw, _ := json.Marshal(exported)
	var viaJSON Document
	if err := json.Unmarshal(raw, &viaJSON); err != nil {
		t.Fatal(err)
	}
	if round, err := f.svc.Apply(root(), Input{Document: viaJSON, Plan: true}); err != nil || len(round.Changes) != 0 {
		t.Fatalf("export then apply through JSON: %v\n%s", err, lines(round))
	}
}

// The file is authoritative for what it lists: a section left out of a
// listed owner is cleared, a runner left out of [runners] is deleted, and
// what the file does not list is left alone and named.
func TestApplyClearsWhatAListedOwnerLeavesOut(t *testing.T) {
	f := fixture(t)
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, sample)}); err != nil {
		t.Fatal(err)
	}
	// Something the next file does not mention at all.
	next := parse(t, `
[runners.claude-opus]
kind = "claude"
model = "opus"
effort = "max"
provider = "anthropic"

[workspaces."fx/api"]
[workspaces."fx/api".pipeline.stages]
plan = { members = "claude-opus" }
`)
	plan, err := f.svc.Apply(root(), Input{Document: next, Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	want := `update runner runner:claude-opus
delete runner runner:glm
clear config workspace:fx/api
update pipeline workspace:fx/api`
	if got := lines(plan); got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
	if strings.Join(plan.Unlisted, ",") != "group:fortix" {
		t.Fatalf("what the file does not list is named and left alone: %v", plan.Unlisted)
	}
	if _, err := f.svc.Apply(root(), Input{Document: next}); err != nil {
		t.Fatal(err)
	}
	if eff, _ := f.config.Effective(root(), "fx/api"); eff.Managed {
		t.Fatal("the workspace's config was left out: it is no longer managed")
	}
	// The group and the fleet default were not in the file: untouched.
	if v, _ := f.pipe.GetPolicy(root(), pipeline.KindGroup, "fortix"); v.Policy == nil {
		t.Fatal("an unlisted group keeps its policy")
	}
	if v, _ := f.pipe.GetPolicy(root(), pipeline.KindFleetDefault, ""); v.Policy == nil {
		t.Fatal("a file without [fleet] leaves the fleet policies")
	}
	// A file without [runners] leaves the definitions; an empty section
	// removes them.
	if plan, _ := f.svc.Apply(root(), Input{Document: Document{}, Plan: true}); len(plan.Changes) != 0 {
		t.Fatalf("an empty file changes nothing: %s", lines(plan))
	}
	if plan, _ := f.svc.Apply(root(), Input{Document: Document{Runners: map[string]wsconfig.RunnerDef{}}, Plan: true}); lines(plan) != "delete runner runner:claude-opus" {
		t.Fatalf("an empty [runners] is the full set: %s", lines(plan))
	}
}

// Nothing is written unless every change passes its checks: a file with one
// bad section applies none of the others.
func TestApplyChecksEverythingFirst(t *testing.T) {
	f := fixture(t)
	doc := parse(t, sample)
	bad := "open"
	doc.Workspaces["fx/api"].Config.Push.PullRequest = &bad
	if _, err := f.svc.Apply(root(), Input{Document: doc}); !errors.Is(err, wsconfig.ErrInvalid) || !strings.Contains(err.Error(), "config workspace:fx/api") {
		t.Fatalf("want the bad section named: %v", err)
	}
	if runners, _ := f.config.Runners(root()); len(runners) != 0 {
		t.Fatal("nothing may be written when a later change fails its check")
	}
	if len(f.sink.entries) != 0 {
		t.Fatalf("no audit events: %+v", f.sink.entries)
	}
	// A pipeline section that cannot be parsed.
	doc = parse(t, sample)
	doc.Groups["fortix"].Pipeline.Stages["review"] = StageDoc{Members: "bad member spec@@"}
	if _, err := f.svc.Apply(root(), Input{Document: doc, Plan: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

// Each change needs the permission its own save needs.
func TestApplyPermissions(t *testing.T) {
	f := fixture(t)
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, sample)}); err != nil {
		t.Fatal(err)
	}
	plain := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k2", Workspaces: map[string]struct{}{"fx/api": {}}})
	if _, err := f.svc.Apply(plain, Input{Plan: true}); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a key without the admin permission: %v", err)
	}
	if _, err := f.svc.Export(admin(false, "fx/api")); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("an export lists everything: fleet admins only (%v)", err)
	}
	if _, err := f.svc.Export(admin(true)); err != nil {
		t.Fatalf("a fleet admin may export: %v", err)
	}
	// An admin of one workspace may apply a file that changes only it …
	one := parse(t, `
[workspaces."fx/api".config]
checks = ["npm run lint"]
[workspaces."fx/api".pipeline]
code_index = true
[workspaces."fx/api".pipeline.stages]
plan = { members = "claude-opus" }
`)
	scoped := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k1", Name: "ops", Admin: true, Approver: true, Workspaces: map[string]struct{}{"fx/api": {}}})
	res, err := f.svc.Apply(scoped, Input{Document: one})
	if err != nil || lines(res) != "update config workspace:fx/api" {
		t.Fatalf("a workspace admin applies its workspace: %v %s", err, lines(res))
	}
	// … and not one that touches the fleet or another workspace.
	fleet := Document{Fleet: &FleetDoc{Default: &policy.Policy{Allow: []policy.Rule{{Provider: "anthropic"}}}}}
	if _, err := f.svc.Apply(scoped, Input{Document: fleet, Plan: true}); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("the fleet's policy is the fleet admin's: %v", err)
	}
	runners := Document{Runners: map[string]wsconfig.RunnerDef{"x": {Kind: "claude"}}}
	if _, err := f.svc.Apply(scoped, Input{Document: runners, Plan: true}); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("runner definitions are the fleet admin's: %v", err)
	}
	group := parse(t, "[groups.fortix]\nworkspaces = [\"fx/api\", \"fx/web\"]\n")
	if _, err := f.svc.Apply(admin(true), Input{Document: group, Plan: true}); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("a group's members are root's to change: %v", err)
	}
}

// What a plan says is "not in the file" is cut to what the caller may know
// of, and the refusal for a workspace or group is the same whether it
// exists or is merely out of the key's reach.
func TestApplyRevealsNothingOutOfScope(t *testing.T) {
	f := fixture(t)
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, sample)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, "[workspaces.\"secret/repo\".config]\nchecks = [\"x\"]\n")}); err != nil {
		t.Fatal(err)
	}
	scoped := admin(false, "fx/api")
	plan, err := f.svc.Apply(scoped, Input{Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan.Unlisted, ","); got != "workspace:fx/api" {
		t.Fatalf("only the caller's own workspace is named: %s", got)
	}
	full, _ := f.svc.Apply(root(), Input{Plan: true})
	if got := strings.Join(full.Unlisted, ","); got != "group:fortix,workspace:fx/api,workspace:secret/repo" {
		t.Fatalf("root sees everything unlisted: %s", got)
	}
	var messages []string
	for _, text := range []string{
		"[workspaces.\"secret/repo\".config]\nchecks = [\"y\"]\n",
		"[workspaces.\"no/such\".config]\nchecks = [\"y\"]\n",
	} {
		_, err := f.svc.Apply(scoped, Input{Document: parse(t, text), Plan: true})
		if !errors.Is(err, auth.ErrAdminRequired) {
			t.Fatalf("want ErrAdminRequired, got %v", err)
		}
		messages = append(messages, strings.NewReplacer("secret/repo", "X", "no/such", "X").Replace(err.Error()))
	}
	if messages[0] != messages[1] {
		t.Fatalf("an existing foreign workspace and a missing one must read the same:\n%s\n%s", messages[0], messages[1])
	}
	// A group whose members the key does not all administer, and one that
	// does not exist.
	messages = nil
	elsewhere := admin(false, "secret/repo")
	for _, text := range []string{"[groups.fortix]\nworkspaces = [\"fx/api\"]\n", "[groups.nosuch]\nworkspaces = [\"fx/api\"]\n"} {
		_, err := f.svc.Apply(elsewhere, Input{Document: parse(t, text), Plan: true})
		if !errors.Is(err, auth.ErrAdminRequired) {
			t.Fatalf("want ErrAdminRequired, got %v", err)
		}
		messages = append(messages, strings.NewReplacer("fortix", "X", "nosuch", "X").Replace(err.Error()))
	}
	if messages[0] != messages[1] {
		t.Fatalf("a group out of reach and a missing one must read the same:\n%s\n%s", messages[0], messages[1])
	}
}

// A pipeline the file leaves as it is needs no permission: a fleet admin
// that is not root applies the server's own export, and a workspace admin
// without the approver permission applies a file that changes only config.
// Changing a pipeline still needs what the dashboard needs.
func TestUnchangedPipelinesNeedNoPermission(t *testing.T) {
	f := fixture(t)
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, sample)}); err != nil {
		t.Fatal(err)
	}
	exported, err := f.svc.Export(admin(true))
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Apply(admin(true), Input{Document: *exported})
	if err != nil || len(res.Changes) != 0 {
		t.Fatalf("a fleet admin applies the export: %v %s", err, lines(res))
	}
	one := parse(t, `
[workspaces."fx/api".config]
checks = ["npm run lint"]
[workspaces."fx/api".pipeline]
code_index = true
[workspaces."fx/api".pipeline.stages]
plan = { members = "claude-opus" }
`)
	res, err = f.svc.Apply(admin(false, "fx/api"), Input{Document: one})
	if err != nil || lines(res) != "update config workspace:fx/api" {
		t.Fatalf("an admin without approver, pipeline unchanged: %v %s", err, lines(res))
	}
	changed := parse(t, `
[workspaces."fx/api".config]
checks = ["npm run lint"]
[workspaces."fx/api".pipeline.stages]
plan = { members = "claude-opus" }
review = { members = "claude-opus" }
`)
	if _, err := f.svc.Apply(admin(false, "fx/api"), Input{Document: changed, Plan: true}); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("changing a workspace's pipeline needs the approver permission: %v", err)
	}
}

// What a plan cannot check in full it says so: a config for a group the
// same file creates is still checked against the groups its members are
// in, and a pipeline that the server's current policy refuses is left to
// the apply when the file changes a policy.
func TestPlanSaysWhatItCouldNotCheck(t *testing.T) {
	f := fixture(t)
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, sample)}); err != nil {
		t.Fatal(err)
	}
	// fortix already sets default_branch for fx/api.
	clash := parse(t, "[groups.second]\nworkspaces = [\"fx/api\"]\n[groups.second.config]\ndefault_branch = \"main\"\n")
	if _, err := f.svc.Apply(root(), Input{Document: clash, Plan: true}); !errors.Is(err, wsconfig.ErrConflict) {
		t.Fatalf("a new group's config is checked against its members' other groups: %v", err)
	}
	if groups, _ := f.svc.groups.ListGroups(root()); len(groups) != 1 {
		t.Fatalf("nothing was created: %d groups", len(groups))
	}
	fine := parse(t, "[groups.second]\nworkspaces = [\"fx/api\"]\n[groups.second.config]\nsetup_timeout = \"20m0s\"\n")
	plan, err := f.svc.Apply(root(), Input{Document: fine, Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	for _, c := range plan.Changes {
		if c.Kind == "config" && c.Owner == "group:second" && len(c.Warnings) == 1 && strings.Contains(c.Warnings[0], "created by this apply") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the plan says the new group's config is checked in full at the apply: %+v", plan.Changes)
	}

	// Without a policy change in the file, a pipeline the policy refuses is
	// refused at the plan.
	refused := parse(t, strings.Replace(sample, `plan = { members = "claude-opus" }`, `plan = { members = "glm" }`, 1))
	if _, err := f.svc.Apply(root(), Input{Document: refused, Plan: true}); !errors.Is(err, pipeline.ErrPolicyRefuses) {
		t.Fatalf("glm on plan under a policy without zai: %v", err)
	}
	// The fortix policy allows anthropic only; this file widens it and
	// uses glm in the same go.
	together := parse(t, strings.Replace(sample, `allow = [{ provider = "anthropic" }, { provider = "openai", stages = ["second_opinion"] }]`,
		`allow = [{ provider = "anthropic" }, { provider = "zai" }]`, 1)+`
qa = { members = "glm" }
`)
	plan, err = f.svc.Apply(root(), Input{Document: together, Plan: true, Force: true})
	if err != nil {
		t.Fatalf("a pipeline that needs the policy this file sets must plan: %v", err)
	}
	deferred := false
	for _, c := range plan.Changes {
		if c.Kind == "pipeline" && len(c.Warnings) == 1 && strings.Contains(c.Warnings[0], "checked again when applied") {
			deferred = true
		}
	}
	if !deferred {
		t.Fatalf("the plan marks the pipeline as decided at the apply: %+v", plan.Changes)
	}
	if _, err := f.svc.Apply(root(), Input{Document: together, Force: true}); err != nil {
		t.Fatalf("and the apply, in order, goes through: %v", err)
	}
}
