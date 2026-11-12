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
	"github.com/martinsuchenak/skopos/internal/secretbox"
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
	cfg.SetSecrets(secretbox.NewStore(sqlDB, secretbox.New("test key")))
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

const wrapped = `
[wrappers.opencode-bwrap]
argv = ["bwrap", "--bind", "{dir}", "{dir}"]
check = ["bwrap", "--version"]

[workspaces."fx/api".config]
default_branch = "main"
[workspaces."fx/api".config.stages.implement]
tools = ["Bash(npm run:*)"]
env = { NODE_ENV = "test" }
[[workspaces."fx/api".config.stages.review.wrap]]
kind = "opencode"
wrapper = "opencode-bwrap"
`

// Wrappers and the stage blocks that name them travel in the file: a
// wrapper the file defines is there before the config that names it, one
// it drops goes after, and a drop that would leave a stage block naming
// nothing is refused.
func TestWrappersAndStageBlocksInTheFile(t *testing.T) {
	f := fixture(t)
	plan, err := f.svc.Apply(root(), Input{Document: parse(t, wrapped), Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "create wrapper wrapper:opencode-bwrap\ncreate workspace workspace:fx/api\ncreate config workspace:fx/api"
	if got := lines(plan); got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, wrapped)}); err != nil {
		t.Fatal(err)
	}
	eff, err := f.config.Effective(root(), "fx/api")
	if err != nil || eff.Config.Stages["implement"].Env["NODE_ENV"] != "test" || eff.Config.Stages["review"].Wrap[0].Wrapper != "opencode-bwrap" {
		t.Fatalf("the stage blocks: %+v %v", eff, err)
	}
	if got := f.sink.by("wrapper.create"); len(got) != 1 || got[0].Via != audit.ViaApply {
		t.Fatalf("the wrapper is recorded by its own service, via apply: %+v", got)
	}

	// Export reads back as the same document.
	exported, err := f.svc.Export(root())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(exported); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[wrappers.opencode-bwrap]") || !strings.Contains(buf.String(), "stages") {
		t.Fatalf("the export:\n%s", buf.String())
	}
	if round, err := f.svc.Apply(root(), Input{Document: parse(t, buf.String()), Plan: true}); err != nil || len(round.Changes) != 0 {
		t.Fatalf("export then apply must change nothing: %v\n%s\n--- exported ---\n%s", err, lines(round), buf.String())
	}

	// A file that drops the wrapper and keeps the rule that names it.
	dropped := strings.Replace(wrapped, "[wrappers.opencode-bwrap]\nargv = [\"bwrap\", \"--bind\", \"{dir}\", \"{dir}\"]\ncheck = [\"bwrap\", \"--version\"]", "[wrappers]", 1)
	doc := parse(t, dropped)
	if doc.Wrappers == nil || len(doc.Wrappers) != 0 {
		t.Fatalf("an empty [wrappers] section states the full set: %+v", doc.Wrappers)
	}
	if _, err := f.svc.Apply(root(), Input{Document: doc, Plan: true}); !errors.Is(err, wsconfig.ErrInvalid) || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("a listed config naming a dropped wrapper: %v", err)
	}
	// The same drop, with the workspace that names it not in the file.
	only := Document{Wrappers: map[string]wsconfig.WrapperDef{}}
	if _, err := f.svc.Apply(root(), Input{Document: only, Plan: true}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "workspace:fx/api") {
		t.Fatalf("dropping a wrapper an unlisted workspace names: %v", err)
	}
	forced, err := f.svc.Apply(root(), Input{Document: only, Plan: true, Force: true})
	if err != nil || lines(forced) != "delete wrapper wrapper:opencode-bwrap" || len(forced.Changes[0].Warnings) == 0 {
		t.Fatalf("forced: %v %s", err, lines(forced))
	}
	// Dropping the rule and the wrapper together: the config changes first.
	without := parse(t, "[wrappers]\n[workspaces.\"fx/api\".config]\ndefault_branch = \"main\"\n")
	res, err := f.svc.Apply(root(), Input{Document: without})
	if err != nil || lines(res) != "update config workspace:fx/api\ndelete wrapper wrapper:opencode-bwrap" {
		t.Fatalf("apply: %v\n%s", err, lines(res))
	}
	if fleet, _ := f.config.Fleet(root()); len(fleet.Wrappers) != 0 {
		t.Fatalf("the wrapper is still defined: %+v", fleet)
	}
	// A file without a [wrappers] section leaves the definitions alone.
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, wrapped)}); err != nil {
		t.Fatal(err)
	}
	if res, err := f.svc.Apply(root(), Input{Document: parse(t, "[workspaces.\"fx/api\".config]\ndefault_branch = \"main\"\n"), Plan: true}); err != nil || lines(res) != "update config workspace:fx/api" {
		t.Fatalf("no [wrappers] section: %v\n%s", err, lines(res))
	}
	// Only a fleet admin defines wrappers.
	if _, err := f.svc.Apply(admin(false, "fx/api"), Input{Document: parse(t, wrapped), Plan: true}); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a workspace admin changed a wrapper: %v", err)
	}
}

const withMCP = `
[mcp.llmrouter]
type = "http"
url = "https://llmrouter.example.com/mcp"
workspaces = ["fx/*"]
[mcp.llmrouter.headers]
Authorization = "Bearer ${secret:token}"

[mcp.scriptling]
type = "command"
command = ["${TOOLS_DIR}/scriptling", "mcp"]

[workspaces."fx/api".config]
mcp = ["llmrouter", "scriptling"]
mcp_servers = ["mcp__llmrouter", "mcp__scriptling__run"]
`

// MCP servers travel in the file as definitions that name their secrets;
// a value is never in it. Applying a definition keeps the stored value of
// a secret it still names, and the export shows none.
func TestMCPServersInTheFile(t *testing.T) {
	f := fixture(t)
	plan, err := f.svc.Apply(root(), Input{Document: parse(t, withMCP), Plan: true})
	if err != nil {
		t.Fatal(err)
	}
	want := "create mcp mcp:llmrouter\ncreate mcp mcp:scriptling\ncreate workspace workspace:fx/api\ncreate config workspace:fx/api"
	if got := lines(plan); got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
	// The plan says which secret has no value yet: the file cannot carry it.
	if w := plan.Changes[0].Warnings; len(w) != 1 || !strings.Contains(w[0], "secret token") || !strings.Contains(w[0], "set-secret llmrouter") {
		t.Fatalf("the plan names the secret to set: %v", w)
	}
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, withMCP)}); err != nil {
		t.Fatal(err)
	}
	const value = "tok-never-in-a-file"
	if err := f.config.SetMCPSecret(root(), "llmrouter", "token", []byte(value), "cli"); err != nil {
		t.Fatal(err)
	}
	// Applying the same file again changes nothing and keeps the value.
	if again, err := f.svc.Apply(root(), Input{Document: parse(t, withMCP)}); err != nil || len(again.Changes) != 0 {
		t.Fatalf("a second apply: %v %s", err, lines(again))
	}
	// A changed definition that still names the secret, on the same host,
	// keeps its value.
	changed := strings.Replace(withMCP, "llmrouter.example.com/mcp", "llmrouter.example.com/mcp/v2", 1)
	if res, err := f.svc.Apply(root(), Input{Document: parse(t, changed)}); err != nil || lines(res) != "update mcp mcp:llmrouter" || len(res.Changes[0].Warnings) != 0 {
		t.Fatalf("a changed definition: %v %s %v", err, lines(res), res.Changes)
	}
	mcp, err := f.config.RunMCP(root(), "fx/api", true)
	if err != nil || len(mcp.Servers) != 2 || !strings.Contains(string(mcp.Servers["llmrouter"]), value) || !strings.Contains(string(mcp.Servers["llmrouter"]), "/mcp/v2") {
		t.Fatalf("the run's servers: %+v %v", mcp, err)
	}
	// A config for a workspace the server is not for is refused, also when
	// the same file narrows the server.
	narrowed := strings.Replace(changed, `workspaces = ["fx/*"]`, `workspaces = ["other/*"]`, 1)
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, narrowed), Plan: true}); !errors.Is(err, wsconfig.ErrInvalid) || !strings.Contains(err.Error(), "may not be given to the runs of fx/api") {
		t.Fatalf("a file that gives a workspace a server it narrows away from it: %v", err)
	}

	// The export: definitions and secret names, never the value.
	exported, err := f.svc.Export(root())
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(exported); err != nil {
		t.Fatal(err)
	}
	asJSON, _ := json.Marshal(exported)
	if strings.Contains(buf.String(), value) || strings.Contains(string(asJSON), value) {
		t.Fatalf("the export holds a secret's value:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "${secret:token}") || !strings.Contains(buf.String(), "[mcp.scriptling]") {
		t.Fatalf("the export:\n%s", buf.String())
	}
	if round, err := f.svc.Apply(root(), Input{Document: parse(t, buf.String()), Plan: true}); err != nil || len(round.Changes) != 0 {
		t.Fatalf("export then apply must change nothing: %v\n%s", err, lines(round))
	}
	// No audit note holds it either.
	for _, e := range f.sink.entries {
		if strings.Contains(e.Notes, value) {
			t.Fatalf("an audit note holds the value: %+v", e)
		}
	}

	// A file with a credential written into it is refused.
	inline := strings.Replace(withMCP, "Bearer ${secret:token}", "Bearer abc123def456", 1)
	if _, err := f.svc.Apply(root(), Input{Document: parse(t, inline), Plan: true}); !errors.Is(err, wsconfig.ErrInvalid) || !strings.Contains(err.Error(), "looks like a credential") {
		t.Fatalf("a credential in the file: %v", err)
	}
	// Dropping a server a workspace outside the file still uses.
	only := Document{MCP: map[string]wsconfig.MCPServerDef{"scriptling": exported.MCP["scriptling"]}}
	if _, err := f.svc.Apply(root(), Input{Document: only, Plan: true}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "workspace:fx/api") {
		t.Fatalf("dropping a server an unlisted workspace uses: %v", err)
	}
	// Dropping it together with its use: the config changes first, and the
	// stored secret goes with the definition.
	without := parse(t, "[mcp]\n[workspaces.\"fx/api\".config]\nmcp = []\n")
	res, err := f.svc.Apply(root(), Input{Document: without})
	if err != nil || lines(res) != "update config workspace:fx/api\ndelete mcp mcp:llmrouter\ndelete mcp mcp:scriptling" {
		t.Fatalf("apply: %v\n%s", err, lines(res))
	}
	if _, _, err := f.config.SetMCPServer(root(), exported.MCP["llmrouter"], "cli"); err != nil {
		t.Fatal(err)
	}
	if views, _ := f.config.MCPServers(root()); len(views) != 1 || views[0].Secrets[0].Set {
		t.Fatalf("a deleted server's secret came back with a new definition: %+v", views)
	}
	// Only a fleet admin defines MCP servers.
	if _, err := f.svc.Apply(admin(false, "fx/api"), Input{Document: parse(t, withMCP), Plan: true}); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a workspace admin changed an MCP server: %v", err)
	}
}
