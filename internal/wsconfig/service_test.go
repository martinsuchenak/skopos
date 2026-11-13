package wsconfig

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/pipeline"
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

func (r *recSink) actions() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.entries {
		out = append(out, e.Action)
	}
	return strings.Join(out, " ")
}

func (r *recSink) last() audit.RecordInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[len(r.entries)-1]
}

func exec(t *testing.T, sqlDB *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := sqlDB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// fixture: workspaces fabrikam/api and fabrikam/web (+ other/x), group "globex" by the
// pattern fabrikam/*, group "web-team" holding fabrikam/web explicitly.
func fixture(t *testing.T) (*Service, *sql.DB, *recSink) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "c.db") + "?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, ws := range []string{"fabrikam/api", "fabrikam/web", "other/x"} {
		exec(t, sqlDB, `INSERT INTO workspaces (id, created_at) VALUES (?, ?)`, ws, now)
	}
	exec(t, sqlDB, `INSERT INTO workspace_groups (id, name, created_at) VALUES ('g1', 'globex', ?), ('g2', 'web-team', ?)`, now, now)
	exec(t, sqlDB, `INSERT INTO workspace_group_patterns (group_id, pattern) VALUES ('g1', 'fabrikam/*')`)
	exec(t, sqlDB, `INSERT INTO workspace_group_members (group_id, workspace_id) VALUES ('g2', 'fabrikam/web')`)
	s := NewService(sqlDB, confrev.NewStore(sqlDB), pipeline.NewStorage(sqlDB))
	sink := &recSink{}
	s.SetAuditSink(sink)
	return s, sqlDB, sink
}

func root() context.Context { return context.Background() }

func key(admin, all bool, ws ...string) context.Context {
	set := map[string]struct{}{}
	for _, w := range ws {
		set[w] = struct{}{}
	}
	return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "k1", Name: "ops", Admin: admin, AllWorkspaces: all, Workspaces: set})
}

func list(v ...string) *[]string { return &v }

func TestNormalize(t *testing.T) {
	bad := []Config{
		{DefaultBranch: new("-x")},
		{BranchTemplate: new("{prefix}{slug}")},
		{BranchTemplate: new("{prefix}{tail}{what}")},
		{Checkout: new("copy")},
		{CheckFixRounds: new(9)},
		{SetupTimeout: new(Duration(3 * time.Hour))},
		{PreflightInterval: new(Duration(time.Second))},
		{Push: &Push{Kind: new("gitlab")}},
		{Push: &Push{PullRequest: new("open")}},
		{Push: &Push{RemoteURL: new("git@github.com:a/b.git")}},
		{Push: &Push{RemoteURL: new("https://user:tok@github.com/a/b.git")}},
		{MCPServers: list("router")},
		{Setup: list(strings.Repeat("x", 5000))},
	}
	for i, c := range bad {
		if _, err := Normalize(c); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d: want ErrInvalid, got %v", i, err)
		}
	}
	// Empty text is not set; an empty list is set, to nothing.
	got, err := Normalize(Config{DefaultBranch: new("  "), Checks: list(" go test ./... ", ""), Setup: list(), QuietTimeout: new(Duration(0)),
		Push: &Push{Kind: new(" ")}})
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultBranch != nil || got.Push != nil {
		t.Fatalf("empty text must not count as set: %+v", got)
	}
	if got.Checks == nil || len(*got.Checks) != 1 || (*got.Checks)[0] != "go test ./..." {
		t.Fatalf("checks: %+v", got.Checks)
	}
	if got.Setup == nil || len(*got.Setup) != 0 {
		t.Fatalf("an empty list stays set: %+v", got.Setup)
	}
	if names := strings.Join(got.FieldNames(), ","); names != "checks,quiet_timeout,setup" {
		t.Fatalf("fields: %s", names)
	}
}

func TestFieldsRoundTripAndDefaults(t *testing.T) {
	c := Config{Checks: list("make test"), Push: &Push{Kind: new("github"), Steps: new(false)}, PlanTimeout: new(Duration(20 * time.Minute))}
	back, err := FromFields(c.Fields())
	if err != nil {
		t.Fatal(err)
	}
	if string(Canonical(back)) != string(Canonical(c)) {
		t.Fatalf("round trip: %s != %s", Canonical(back), Canonical(c))
	}
	full := Complete(c)
	if *full.DefaultBranch != "main" || *full.Push.Kind != "github" || *full.Push.PullRequest != "none" || *full.Push.Steps ||
		full.ImplTimeout.D() != 90*time.Minute || full.PlanTimeout.D() != 20*time.Minute || full.MCPDeny == nil || len(*full.MCPDeny) != 0 {
		t.Fatalf("defaults under the set fields: %s", Canonical(full))
	}
	if ch := strings.Join(Changed(c, Config{Checks: list("make test"), Setup: list("npm ci")}), ","); ch != "plan_timeout,push.kind,push.steps,setup" {
		t.Fatalf("changed: %s", ch)
	}
}

// A workspace is managed only by a config of its own: a group's values are
// defaults under it and never manage a workspace on their own.
func TestManagedAndGroupDefaults(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.Set(root(), KindGroup, "globex", Config{Checks: list("make lint"), DefaultBranch: new("develop")}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	eff, err := s.Effective(root(), "fabrikam/api")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Managed || eff.Rev != "" || s.Rev(root(), "fabrikam/api") != "" {
		t.Fatalf("a group default alone must not manage a workspace: %+v", eff)
	}
	v, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("go test ./...")}, audit.ViaCLI, "")
	if err != nil {
		t.Fatal(err)
	}
	e := v.Effective
	if !e.Managed || e.Rev == "" || len(e.Parts) != 2 {
		t.Fatalf("managed with the group under it: %+v", e)
	}
	if (*e.Config.Checks)[0] != "go test ./..." || e.Sources["checks"] != "workspace" {
		t.Fatalf("the workspace's own field wins: %+v", e)
	}
	if *e.Config.DefaultBranch != "develop" || e.Sources["default_branch"] != "group:globex" {
		t.Fatalf("the group supplies what the workspace leaves out: %+v", e)
	}
	if e.Config.Setup != nil {
		t.Fatal("a field nothing sets stays unset (the worker fills the default)")
	}
	if eff, _ := s.Effective(root(), "other/x"); eff.Managed {
		t.Fatal("a workspace outside every group with no config is not managed")
	}
	// Clearing the workspace's config hands it back to the workers' files.
	if _, err := s.Clear(root(), KindWorkspace, "fabrikam/api", audit.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if s.Rev(root(), "fabrikam/api") != "" {
		t.Fatal("cleared: not managed")
	}
}

// The revision a claim records names exactly one combination of stored
// revisions, and reads back the same after the config has moved on.
func TestRevNamesWhatApplied(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("one")}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	first := s.Rev(root(), "fabrikam/api")
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("two")}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	second := s.Rev(root(), "fabrikam/api")
	if first == "" || second == "" || first == second {
		t.Fatalf("a save must change the revision at once: %q %q", first, second)
	}
	// A group change under the workspace changes it too.
	if _, err := s.Set(root(), KindGroup, "globex", Config{Setup: list("npm ci")}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	third := s.Rev(root(), "fabrikam/api")
	if third == second {
		t.Fatal("a group's change must change the revision of the workspaces under it")
	}
	for rev, want := range map[string]string{first: "one", second: "two", third: "two"} {
		at, err := s.At(root(), "fabrikam/api", rev)
		if err != nil {
			t.Fatal(err)
		}
		if (*at.Config.Checks)[0] != want || at.Rev != rev {
			t.Fatalf("revision %s: %s", rev, Canonical(at.Config))
		}
		if hasSetup := at.Config.Setup != nil; hasSetup != (rev == third) {
			t.Fatalf("revision %s: the group's setup belongs to the third only", rev)
		}
	}
	// Saving the same content is not a change.
	v, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("two")}, audit.ViaCLI, "")
	if err != nil || len(v.Changed) != 0 || s.Rev(root(), "fabrikam/api") != third {
		t.Fatalf("an identical save: %v %+v", err, v.Changed)
	}
	// A revision belongs to its workspace, and an unknown one is not found.
	if _, err := s.At(root(), "fabrikam/web", first); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another workspace's revision: %v", err)
	}
	if _, err := s.At(root(), "fabrikam/api", UnreadableRev); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the unreadable marker must never resolve: %v", err)
	}
}

func TestGroupConflictsAndEffectiveChecks(t *testing.T) {
	s, sqlDB, _ := fixture(t)
	if _, err := s.Set(root(), KindGroup, "globex", Config{Checks: list("a")}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	// fabrikam/web is in both groups: the second may not set the same field.
	if _, err := s.Set(root(), KindGroup, "web-team", Config{Checks: list("b")}, audit.ViaCLI, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if _, err := s.Set(root(), KindGroup, "web-team", Config{Setup: list("npm ci")}, audit.ViaCLI, ""); err != nil {
		t.Fatalf("another field is fine: %v", err)
	}
	// A conflict that membership changes create is reported; the first
	// group by name keeps the field.
	exec(t, sqlDB, `INSERT INTO workspace_groups (id, name, created_at) VALUES ('g0', 'alpha', '')`)
	exec(t, sqlDB, `INSERT INTO config_revisions (id, kind, owner, content, content_hash, created_ms) VALUES ('rx', 'workspace_config', 'group:g0', '{"checks":["z"]}', 'h', 1)`)
	exec(t, sqlDB, `INSERT INTO config_heads (kind, owner, revision_id, updated_ms) VALUES ('workspace_config', 'group:g0', 'rx', 1)`)
	exec(t, sqlDB, `INSERT INTO workspace_group_members (group_id, workspace_id) VALUES ('g0', 'fabrikam/web')`)
	v, err := s.Set(root(), KindWorkspace, "fabrikam/web", Config{}, audit.ViaCLI, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Effective.Conflicts) != 1 || (*v.Effective.Config.Checks)[0] != "z" || v.Effective.Sources["checks"] != "group:alpha" {
		t.Fatalf("conflict reported, first group by name wins: %+v", v.Effective)
	}
	// What only holds for the merged config: a push needs a remote URL.
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Push: &Push{Kind: new("github")}}, audit.ViaCLI, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("push without a remote URL: %v", err)
	}
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Push: &Push{Kind: new("github"), RemoteURL: new("https://github.com/fabrikam/api.git"), PullRequest: new("draft")}}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
}

func TestGroupSaveRefusedForAManagedMember(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.Set(root(), KindGroup, "globex", Config{Push: &Push{PullRequest: new("draft")}}, audit.ViaCLI, "")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "fabrikam/api") {
		t.Fatalf("a pull request without a push on a managed member: %v", err)
	}
}

func TestPermissions(t *testing.T) {
	s, _, _ := fixture(t)
	c := Config{Checks: list("make test")}
	// Not an admin: the same refusal whether or not the owner exists.
	for _, id := range []string{"fabrikam/api", "nope/x"} {
		if _, err := s.Set(key(false, false, "fabrikam/api"), KindWorkspace, id, c, audit.ViaCLI, ""); !errors.Is(err, auth.ErrAdminRequired) {
			t.Fatalf("%s: want ErrAdminRequired, got %v", id, err)
		}
	}
	if _, err := s.Set(key(true, false, "fabrikam/api"), KindWorkspace, "fabrikam/web", c, audit.ViaCLI, ""); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("an admin outside its scope: %v", err)
	}
	if _, err := s.Set(key(true, false, "fabrikam/api"), KindWorkspace, "fabrikam/api", c, audit.ViaCLI, ""); err != nil {
		t.Fatalf("an admin in scope: %v", err)
	}
	// A group needs every member in scope.
	if _, err := s.Set(key(true, false, "fabrikam/api"), KindGroup, "globex", c, audit.ViaCLI, ""); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a group with a member out of scope: %v", err)
	}
	if _, err := s.Set(key(true, false, "fabrikam/api", "fabrikam/web"), KindGroup, "globex", Config{Setup: list("x")}, audit.ViaCLI, ""); err != nil {
		t.Fatalf("a group with every member in scope: %v", err)
	}
	// Reading a workspace's config needs only the workspace in scope; a
	// foreign one is not found.
	if _, err := s.Get(key(false, false, "fabrikam/api"), KindWorkspace, "fabrikam/api"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(key(false, false, "fabrikam/api"), KindWorkspace, "fabrikam/web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a foreign workspace: %v", err)
	}
	if _, err := s.EffectiveFor(key(false, false, "fabrikam/api"), "fabrikam/web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a foreign workspace's effective config: %v", err)
	}
	if _, err := s.Get(key(false, false, "fabrikam/api"), KindGroup, "globex"); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a group's config is its admins': %v", err)
	}
	// A run never passes.
	run := auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "run:1", Admin: true, Run: &auth.RunIdentity{}, Workspaces: map[string]struct{}{"fabrikam/api": {}}})
	if _, err := s.Set(run, KindWorkspace, "fabrikam/api", c, audit.ViaCLI, ""); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a run principal: %v", err)
	}
}

func TestAudit(t *testing.T) {
	s, _, sink := fixture(t)
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("make test"), DefaultBranch: new("develop")}, audit.ViaDashboard, ""); err != nil {
		t.Fatal(err)
	}
	e := sink.last()
	if e.Action != "config.update" || e.EntityType != AuditEntity || e.EntityID != "workspace:fabrikam/api" || e.WorkspaceID != "fabrikam/api" || e.Via != audit.ViaDashboard {
		t.Fatalf("event: %+v", e)
	}
	// The command field is recorded with its value; the others by name.
	if !strings.Contains(e.Notes, "(was none): checks, default_branch") || !strings.Contains(e.Notes, `checks = ["make test"]`) || strings.Contains(e.Notes, "develop") {
		t.Fatalf("notes: %s", e.Notes)
	}
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("make test"), DefaultBranch: new("develop")}, audit.ViaDashboard, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("make test")}, ViaImport, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clear(root(), KindWorkspace, "fabrikam/api", audit.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if got := sink.actions(); got != "config.update config.import config.delete" {
		t.Fatalf("an identical save writes nothing; an import and a delete have their own actions: %s", got)
	}
	// Revisions are never removed: the cleared config's history is there.
	hist, err := confrevHistory(s, "workspace:fabrikam/api")
	if err != nil || len(hist) != 2 {
		t.Fatalf("history: %v %d", err, len(hist))
	}
}

func confrevHistory(s *Service, owner string) ([]confrev.Revision, error) {
	return s.revs.History(context.Background(), KindWorkspaceConfig, owner, 0)
}

func TestRunnerDefinitions(t *testing.T) {
	s, _, sink := fixture(t)
	bad := []RunnerDef{
		{Name: "bad name", Kind: "claude"},
		{Name: "x", Kind: "command"},
		{Name: "x", Kind: "gemini"},
		{Name: "x", Kind: "claude", Effort: "extreme"},
		{Name: "x", Kind: "opencode", Model: "glm-5.3"},
		{Name: "x", Kind: "opencode", Model: "zai-coding-plan/glm-5.3", Provider: "local"},
		{Name: "x", Kind: "claude", Provider: "Not A Provider"},
	}
	for i, d := range bad {
		if _, _, err := s.SetRunner(root(), d, audit.ViaCLI); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d: want ErrInvalid, got %v", i, err)
		}
	}
	if f, _ := s.Fleet(root()); f.Rev != "" || len(f.Runners) != 0 {
		t.Fatalf("nothing defined: %+v", f)
	}
	if _, _, err := s.SetRunner(key(true, false, "fabrikam/api"), RunnerDef{Name: "claude-opus", Kind: "claude"}, audit.ViaCLI); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a runner definition is the fleet's: %v", err)
	}
	v, changed, err := s.SetRunner(key(true, true), RunnerDef{Name: "claude-opus", Kind: "Claude", Model: "opus", Effort: "high", Provider: "Anthropic"}, audit.ViaCLI)
	if err != nil || !changed || v.Kind != "claude" || v.Provider != "anthropic" {
		t.Fatalf("define: %v %+v", err, v)
	}
	if _, _, err := s.SetRunner(root(), RunnerDef{Name: "glm", Kind: "opencode", Model: "zai-coding-plan/glm-5.3", Provider: "zai"}, audit.ViaCLI); err != nil {
		t.Fatal(err)
	}
	first := s.FleetRev(root())
	if _, changed, _ := s.SetRunner(root(), RunnerDef{Name: "claude-opus", Kind: "claude", Model: "opus", Effort: "high", Provider: "anthropic"}, audit.ViaCLI); changed {
		t.Fatal("an identical definition is not a change")
	}
	if _, _, err := s.SetRunner(root(), RunnerDef{Name: "claude-opus", Kind: "claude", Model: "opus", Effort: "max", Provider: "anthropic"}, audit.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if second := s.FleetRev(root()); first == "" || second == first {
		t.Fatalf("a changed definition changes the fleet revision at once: %q %q", first, second)
	}
	facts, err := s.RunnerFacts(root())
	if err != nil || facts["glm"].Provider != "zai" || facts["claude-opus"].Effort != "max" {
		t.Fatalf("facts: %v %+v", err, facts)
	}
	if _, err := s.DeleteRunner(root(), "glm", audit.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRunner(root(), "glm", audit.ViaCLI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
	f, _ := s.Fleet(root())
	if len(f.Runners) != 1 || f.Runners[0].Name != "claude-opus" {
		t.Fatalf("after the delete: %+v", f)
	}
	if got := sink.actions(); got != "runner_def.create runner_def.create runner_def.update runner_def.delete" {
		t.Fatalf("events: %s", got)
	}
	buf, _ := json.Marshal(f.Runners[0])
	if !strings.Contains(string(buf), `"name":"claude-opus"`) {
		t.Fatalf("a definition names itself in JSON: %s", buf)
	}
}

func TestDigest(t *testing.T) {
	s, _, _ := fixture(t)
	if d := s.Digest(root(), []string{"fabrikam/api", "fabrikam/web"}); d != "" {
		t.Fatalf("nothing managed: %q", d)
	}
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	a := s.Digest(root(), []string{"fabrikam/web", "fabrikam/api"})
	if a == "" || a != s.Digest(root(), []string{"fabrikam/api", "fabrikam/web"}) {
		t.Fatalf("the digest does not depend on the order: %q", a)
	}
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("x")}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	if b := s.Digest(root(), []string{"fabrikam/api", "fabrikam/web"}); b == a {
		t.Fatal("a change must change the digest")
	}
}

// A list set to a nil slice is set to nothing: it must not read back as a
// missing field (a worker that keeps its own commands and has none of one
// kind passes exactly that).
func TestNilListIsSetToNothing(t *testing.T) {
	var none []string
	c := Config{Setup: &none, Checks: list("x")}
	if got := string(c.Fields()["setup"]); got != "[]" {
		t.Fatalf("a nil list flattens to []: %s", got)
	}
	full := Complete(c)
	if full.Setup == nil || len(*full.Setup) != 0 || full.Preflight == nil {
		t.Fatalf("completing it must leave every list non-nil: %s", Canonical(full))
	}
	back, err := FromFields(c.Fields())
	if err != nil || back.Setup == nil {
		t.Fatalf("round trip keeps it set: %v %s", err, Canonical(back))
	}
}

// Taking a group's config away is checked like a save: a managed member
// that relies on it must not be left with a config that cannot work.
func TestGroupClearRefusedForADependentMember(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.Set(root(), KindGroup, "globex", Config{Push: &Push{Kind: new("github"), RemoteURL: new("https://github.com/fabrikam/x.git")}}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Push: &Push{PullRequest: new("draft")}}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clear(root(), KindGroup, "globex", audit.ViaCLI); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "fabrikam/api") {
		t.Fatalf("clearing the group would leave fabrikam/api with a pull request and no push: %v", err)
	}
	if _, err := s.Plan(root(), KindGroup, "globex", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a plan of the clear says the same: %v", err)
	}
	// Once the workspace no longer needs it, the clear goes through.
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clear(root(), KindGroup, "globex", audit.ViaCLI); err != nil {
		t.Fatal(err)
	}
}

// A read-modify-write names the revision it read: a save in between
// refuses it.
func TestSetIfRefusesALostRace(t *testing.T) {
	s, _, _ := fixture(t)
	first, err := s.SetIf(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("a")}, audit.ViaCLI, "", "")
	if err != nil {
		t.Fatalf("nothing stored, as read: %v", err)
	}
	if _, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("b")}, audit.ViaDashboard, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetIf(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("c")}, audit.ViaCLI, "", first.Rev); !errors.Is(err, ErrConflict) {
		t.Fatalf("someone else saved since: %v", err)
	}
	if _, err := s.SetIf(root(), KindWorkspace, "fabrikam/api", Config{Checks: list("c")}, audit.ViaCLI, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("read as empty, and it is not: %v", err)
	}
	v, _ := s.Get(root(), KindWorkspace, "fabrikam/api")
	if (*v.Config.Checks)[0] != "b" {
		t.Fatalf("the refused saves wrote nothing: %s", Canonical(*v.Config))
	}
}

// A workspace can set a group's branch template back to the default; and a
// surface label nobody knows is not kept with the revision.
func TestEmptyBranchTemplateAndViaLabel(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.Set(root(), KindGroup, "globex", Config{BranchTemplate: new("{prefix}{ref}")}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	v, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{BranchTemplate: new("")}, "made-up-surface", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Effective.Config.BranchTemplate == nil || *v.Effective.Config.BranchTemplate != "" || v.Effective.Sources["branch_template"] != "workspace" {
		t.Fatalf("an empty template is the workspace's value (the default template): %+v", v.Effective)
	}
	if v.Via != audit.ViaAPI {
		t.Fatalf("an unknown surface is recorded as api, got %q", v.Via)
	}
}

// A config for a group that does not exist yet is still checked against
// the groups its members are already in.
func TestPlanNewGroupFindsConflicts(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.Set(root(), KindGroup, "globex", Config{DefaultBranch: new("develop")}, audit.ViaCLI, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlanNew(root(), KindGroup, "new-team", []string{"fabrikam/api"}, Config{DefaultBranch: new("main")}); !errors.Is(err, ErrConflict) {
		t.Fatalf("globex already sets default_branch for fabrikam/api: %v", err)
	}
	fields, err := s.PlanNew(root(), KindGroup, "new-team", []string{"fabrikam/api"}, Config{Checks: list("x")})
	if err != nil || strings.Join(fields, ",") != "checks" {
		t.Fatalf("another field is fine: %v %v", fields, err)
	}
	if _, err := s.PlanNew(key(true, false, "fabrikam/web"), KindGroup, "new-team", []string{"fabrikam/api"}, Config{}); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a new group's config needs every member in scope: %v", err)
	}
	if _, err := s.PlanNew(root(), KindWorkspace, "new/ws", nil, Config{Push: &Push{Kind: new("github")}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a new workspace's config is checked on its own: %v", err)
	}
}

// A workspace that starts to hold a config on the server is a change even
// when the config sets nothing: its workers stop reading their own files
// for it. The save says so, as the dry run does.
func TestBecomingManagedIsReportedAsAChange(t *testing.T) {
	s, _, _ := fixture(t)
	plan, err := s.Plan(root(), KindWorkspace, "fabrikam/api", &Config{})
	if err != nil || !slices.Equal(plan, []string{"managed"}) {
		t.Fatalf("the dry run: %v %v", plan, err)
	}
	v, err := s.Set(root(), KindWorkspace, "fabrikam/api", Config{}, audit.ViaCLI, "")
	if err != nil || !slices.Equal(v.Changed, []string{"managed"}) {
		t.Fatalf("an empty config that makes the workspace managed is a change: %+v %v", v, err)
	}
	if v, err = s.Set(root(), KindWorkspace, "fabrikam/api", Config{}, audit.ViaCLI, ""); err != nil || len(v.Changed) != 0 {
		t.Fatalf("saved again, nothing changes: %+v %v", v, err)
	}
	if v, err = s.Clear(root(), KindWorkspace, "fabrikam/api", audit.ViaCLI); err != nil || !slices.Equal(v.Changed, []string{"managed"}) {
		t.Fatalf("and so is giving it back to the workers' files: %+v %v", v, err)
	}
}
