package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/runs"

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

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "p.db") + "?_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	return sqlDB
}

func exec(t *testing.T, sqlDB *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := sqlDB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// fixture: workspaces fx/api and fx/web (+ other/x), group "fortix" by the
// pattern fx/*, group "web-team" holding fx/web explicitly.
func fixture(t *testing.T) (*Service, *sql.DB, *recSink) {
	t.Helper()
	sqlDB := testDB(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, ws := range []string{"fx/api", "fx/web", "other/x"} {
		exec(t, sqlDB, `INSERT INTO workspaces (id, created_at) VALUES (?, ?)`, ws, now)
	}
	exec(t, sqlDB, `INSERT INTO workspace_groups (id, name, created_at) VALUES ('g1', 'fortix', ?), ('g2', 'web-team', ?)`, now, now)
	exec(t, sqlDB, `INSERT INTO workspace_group_patterns (group_id, pattern) VALUES ('g1', 'fx/*')`)
	exec(t, sqlDB, `INSERT INTO workspace_group_members (group_id, workspace_id) VALUES ('g2', 'fx/web')`)
	s := NewService(NewStorage(sqlDB))
	sink := &recSink{}
	s.SetAuditSink(sink)
	return s, sqlDB, sink
}

func root() context.Context { return context.Background() }

func key(id string, approver bool, ws ...string) context.Context {
	set := map[string]struct{}{}
	for _, w := range ws {
		set[w] = struct{}{}
	}
	return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: id, Workspaces: set, Approver: approver})
}

func members(t *testing.T, spec string) []runs.Member {
	t.Helper()
	ms, err := ParseMembers(spec)
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func TestValidation(t *testing.T) {
	cases := []struct {
		stage string
		st    StageSetting
		want  string
	}{
		{"implement", StageSetting{Members: []runs.Member{{Runner: "a"}, {Runner: "b"}}}, "takes one runner"},
		{"review", StageSetting{}, "at least one runner"},
		{"plan", StageSetting{Members: []runs.Member{{Runner: "a"}}, Policy: "majority"}, "only to review and qa"},
		{"review", StageSetting{Members: []runs.Member{{Runner: "a", Focus: "Bad Focus"}}}, "focus"},
		{"review", StageSetting{Members: []runs.Member{{Runner: "a"}}, TimeoutMS: 1000}, "at least one minute"},
		{"implement", StageSetting{Members: []runs.Member{{Runner: "a"}}, TimeoutMS: 120000}, "timeout applies only"},
		{"review", StageSetting{Members: []runs.Member{{Runner: "a"}, {Runner: "a"}}}, "twice"},
		{"deploy", StageSetting{Members: []runs.Member{{Runner: "a"}}}, "unknown stage"},
		{"review", StageSetting{Members: []runs.Member{{Runner: "a"}}, Policy: "sometimes"}, "policy is"},
	}
	for _, c := range cases {
		if err := ValidateStage(c.stage, c.st); err == nil || !strings.Contains(err.Error(), c.want) || !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s %+v: want %q, got %v", c.stage, c.st, c.want, err)
		}
	}
	if err := ValidateStage("review", StageSetting{Members: []runs.Member{{Runner: "claude", Focus: "security"}, {Runner: "opencode-glm"}}, Policy: "majority", EarlyExit: true, TimeoutMS: 45 * 60000}); err != nil {
		t.Fatalf("valid review group refused: %v", err)
	}
	if _, err := Normalize(Settings{AgentsDefault: "crowd"}); err == nil {
		t.Fatal("bad agents default must be refused")
	}
	if ms, err := ParseMembers(" security@claude , tests@opencode-glm "); err != nil || FormatMembers(ms) != "security@claude,tests@opencode-glm" {
		t.Fatalf("parse/format round trip: %v %v", ms, err)
	}
}

// Workspace beats group per stage; a group reaches its workspaces by
// pattern; sources say where each value comes from; Prefs feeds the queue.
func TestResolutionAndSources(t *testing.T) {
	s, _, _ := fixture(t)
	if _, err := s.Put(root(), KindGroup, "fortix", Settings{
		Stages: map[string]StageSetting{
			"plan":   {Members: members(t, "claude,opencode-glm")},
			"review": {Members: members(t, "security@claude,tests@opencode-glm"), Policy: "any-blocks", TimeoutMS: 45 * 60000},
		},
		AgentsDefault: "solo",
	}, audit.ViaDashboard); err != nil {
		t.Fatal(err)
	}
	v, err := s.Put(root(), KindWorkspace, "fx/api", Settings{Stages: map[string]StageSetting{"plan": {Members: members(t, "claude")}}}, audit.ViaCLI)
	if err != nil {
		t.Fatal(err)
	}
	if v.Effective["plan"].Source != "workspace" || FormatMembers(v.Effective["plan"].Members) != "claude" {
		t.Fatalf("workspace overrides plan: %+v", v.Effective["plan"])
	}
	if v.Effective["review"].Source != "group:fortix" || v.EffectiveAgentsDefault != "solo" || v.AgentsDefaultSource != "group:fortix" {
		t.Fatalf("group supplies review and the agents default: %+v", v)
	}
	if _, ok := v.Effective["qa"]; ok {
		t.Fatal("qa is not set anywhere")
	}
	p := s.Prefs(root(), "fx/api")
	if p.StageRunner("plan") != "claude" || p.GroupMembers("plan") != nil {
		t.Fatalf("plan single: %+v", p)
	}
	if g := p.GroupMembers("review"); len(g) != 2 || p.PolicyFor("review").Timeout != 45*time.Minute || p.AgentsDefault != "solo" {
		t.Fatalf("review group from fortix: %+v", p)
	}
	if other := s.Prefs(root(), "other/x"); other.Configured("review") {
		t.Fatalf("a workspace outside the group gets nothing: %+v", other)
	}
	// Clearing the workspace override falls back to the group — at once
	// (writes drop the cache).
	if _, err := s.Put(root(), KindWorkspace, "fx/api", Settings{}, audit.ViaCLI); err != nil {
		t.Fatal(err)
	}
	if p := s.Prefs(root(), "fx/api"); p.GroupMembers("plan") == nil {
		t.Fatalf("plan falls back to the group's two planners: %+v", p)
	}
}

// Two groups may not set the same stage for a shared workspace; if
// membership changes make it happen anyway, the view reports it and the
// first group by name wins.
func TestGroupConflicts(t *testing.T) {
	s, sqlDB, _ := fixture(t)
	if _, err := s.Put(root(), KindGroup, "fortix", Settings{Stages: map[string]StageSetting{"review": {Members: members(t, "claude")}}}, ""); err != nil {
		t.Fatal(err)
	}
	_, err := s.Put(root(), KindGroup, "web-team", Settings{Stages: map[string]StageSetting{"review": {Members: members(t, "opencode-glm")}}}, "")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "fx/web is also in group fortix") {
		t.Fatalf("shared workspace, same stage: %v", err)
	}
	if _, err := s.Put(root(), KindGroup, "web-team", Settings{Stages: map[string]StageSetting{"qa": {Members: members(t, "opencode-glm")}}}, ""); err != nil {
		t.Fatalf("a different stage is fine: %v", err)
	}
	// Membership changes outside this service create a conflict anyway.
	exec(t, sqlDB, `INSERT INTO pipeline_stages (owner_kind, owner_id, stage, members, updated_ms, updated_by) VALUES ('group', 'g2', 'review', '[{"runner":"qwen"}]', 0, '')`)
	v, err := s.Get(root(), KindWorkspace, "fx/web")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Conflicts) != 1 || v.Effective["review"].Source != "group:fortix" {
		t.Fatalf("conflict reported, first group by name wins: %+v", v)
	}
}

func TestPermissions(t *testing.T) {
	s, _, _ := fixture(t)
	set := Settings{Stages: map[string]StageSetting{"review": {Members: members(t, "claude")}}}
	if _, err := s.Put(key("k1", false, "fx/api"), KindWorkspace, "fx/api", set, ""); !errors.Is(err, auth.ErrApproverRequired) {
		t.Fatalf("plain scoped key: %v", err)
	}
	if _, err := s.Put(key("k2", true, "fx/web"), KindWorkspace, "fx/api", set, ""); !errors.Is(err, auth.ErrOutOfScope) {
		t.Fatalf("approver out of scope: %v", err)
	}
	if _, err := s.Put(key("k3", true, "fx/api"), KindWorkspace, "fx/api", set, ""); err != nil {
		t.Fatalf("approver in scope: %v", err)
	}
	if _, err := s.Put(key("k3", true, "fx/api"), KindGroup, "fortix", set, ""); !errors.Is(err, auth.ErrRootRequired) {
		t.Fatalf("groups are root-only: %v", err)
	}
	if _, err := s.Get(key("k1", false, "fx/api"), KindGroup, "fortix"); err == nil {
		t.Fatal("group reads are root-only")
	}
	if _, err := s.Get(key("k1", false, "fx/web"), KindWorkspace, "fx/api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign workspace reads 404: %v", err)
	}
	if v, err := s.Get(key("k1", false, "fx/api"), KindWorkspace, "fx/api"); err != nil || FormatMembers(v.Effective["review"].Members) != "claude" {
		t.Fatalf("any key in scope may read: %+v %v", v, err)
	}
	if _, err := s.Put(root(), KindWorkspace, "not/registered", set, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered workspace: %v", err)
	}
}

func TestAuditAndUnknownRunners(t *testing.T) {
	s, sqlDB, sink := fixture(t)
	exec(t, sqlDB, `INSERT INTO workers (id, runners, workspaces, last_seen, updated_at) VALUES ('w1', '["claude"]', '["fx/api"]', '', '')`)
	v, err := s.Put(key("k3", true, "fx/api"), KindWorkspace, "fx/api", Settings{
		Stages:        map[string]StageSetting{"review": {Members: members(t, "security@claude,tests@opencode-glm")}},
		AgentsDefault: "panel",
	}, audit.ViaDashboard)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.UnknownRunners, ",") != "opencode-glm" {
		t.Fatalf("runner no worker serving fx/api offers: %v", v.UnknownRunners)
	}
	if len(sink.entries) != 1 {
		t.Fatalf("one audit entry: %+v", sink.entries)
	}
	e := sink.entries[0]
	if e.Action != "pipeline.update" || e.EntityID != "workspace:fx/api" || e.WorkspaceID != "fx/api" || e.Via != audit.ViaDashboard ||
		!strings.Contains(e.Notes, "review: not set → security@claude,tests@opencode-glm") || !strings.Contains(e.Notes, "agents default: not set → panel") {
		t.Fatalf("audit entry: %+v", e)
	}
	// Saving the same thing again is not a change.
	if _, err := s.Put(key("k3", true, "fx/api"), KindWorkspace, "fx/api", v.Own, audit.ViaDashboard); err != nil || len(sink.entries) != 1 {
		t.Fatalf("no-op save must not audit: %v %d", err, len(sink.entries))
	}
}

// The old flags are imported once: missing stages are added (registering
// the workspace), stored settings win, invalid entries are skipped.
func TestImport(t *testing.T) {
	s, sqlDB, sink := fixture(t)
	if _, err := s.Put(root(), KindWorkspace, "fx/api", Settings{Stages: map[string]StageSetting{"review": {Members: members(t, "claude")}}}, ""); err != nil {
		t.Fatal(err)
	}
	sink.entries = nil
	flags := map[string]*runs.WorkspacePrefs{
		"fx/api": {Stages: map[string]string{"plan": "claude", "review": "qwen"}, Groups: map[string][]runs.Member{}, Policies: map[string]runs.StagePolicy{}, AgentsDefault: "solo"},
		"new/repo": {Stages: map[string]string{"implement": "claude"}, Groups: map[string][]runs.Member{
			"qa": {{Runner: "a"}, {Runner: "b"}}}, Policies: map[string]runs.StagePolicy{"plan": {Policy: "majority"}}},
	}
	registered := []string{}
	lines := s.Import(root(), flags, func(ws string) error {
		registered = append(registered, ws)
		exec(t, sqlDB, `INSERT OR IGNORE INTO workspaces (id, created_at) VALUES (?, '')`, ws)
		return nil
	})
	all := strings.Join(lines, "\n")
	for _, want := range []string{"fx/api: imported plan: not set → claude; agents default: not set → solo", "review flag (qwen) differs from the stored setting (claude); stored setting wins", "new/repo: imported implement: not set → claude; qa: not set → a,b"} {
		if !strings.Contains(all, want) {
			t.Errorf("import log missing %q:\n%s", want, all)
		}
	}
	v, _ := s.Get(root(), KindWorkspace, "fx/api")
	if FormatMembers(v.Own.Stages["review"].Members) != "claude" || FormatMembers(v.Own.Stages["plan"].Members) != "claude" {
		t.Fatalf("stored wins, missing added: %+v", v.Own)
	}
	if len(sink.entries) != 2 || sink.entries[0].Action != "pipeline.import" || sink.entries[0].Via != audit.ViaSystem {
		t.Fatalf("imports audited as system: %+v", sink.entries)
	}
	// Second start: nothing new.
	sink.entries = nil
	lines = s.Import(root(), flags, func(string) error { return nil })
	if strings.Contains(strings.Join(lines, "\n"), "imported") || len(sink.entries) != 0 {
		t.Fatalf("re-import must be a no-op: %v %+v", lines, sink.entries)
	}
}

func TestDropOrphans(t *testing.T) {
	s, sqlDB, _ := fixture(t)
	if _, err := s.Put(root(), KindGroup, "fortix", Settings{Stages: map[string]StageSetting{"review": {Members: members(t, "claude")}}, AgentsDefault: "solo"}, ""); err != nil {
		t.Fatal(err)
	}
	exec(t, sqlDB, `DELETE FROM workspace_groups WHERE id = 'g1'`)
	if err := s.DropOrphans(root()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = sqlDB.QueryRow(`SELECT (SELECT count(*) FROM pipeline_stages) + (SELECT count(*) FROM pipeline_defaults)`).Scan(&n)
	if n != 0 {
		t.Fatalf("orphaned group settings remain: %d", n)
	}
}
