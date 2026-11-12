package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/confrev"
	"github.com/martinsuchenak/skopos/internal/dbtime"
	"github.com/martinsuchenak/skopos/internal/policy"
	"github.com/martinsuchenak/skopos/internal/runs"
)

func policyFixture(t *testing.T) (*Service, *sql.DB, *recSink) {
	t.Helper()
	s, sqlDB, sink := fixture(t)
	s.SetRevisions(confrev.NewStore(sqlDB))
	return s, sqlDB, sink
}

func admin(all bool, ws ...string) context.Context {
	set := map[string]struct{}{}
	for _, w := range ws {
		set[w] = struct{}{}
	}
	return auth.WithPrincipal(context.Background(), &auth.Principal{KeyID: "admin", Name: "ops", Admin: true, AllWorkspaces: all, Workspaces: set})
}

func allow(rules ...string) policy.Policy {
	var p policy.Policy
	for _, r := range rules {
		rule, err := policy.ParseRule(r)
		if err != nil {
			panic(err)
		}
		p.Allow = append(p.Allow, rule)
	}
	return p
}

func sources(e *policy.Effective) string {
	var out []string
	for _, l := range e.Layers {
		out = append(out, l.Source)
	}
	return strings.Join(out, " > ")
}

func worker(t *testing.T, sqlDB *sql.DB, id string, workspaces []string, facts ...policy.Facts) {
	t.Helper()
	var names []string
	for _, f := range facts {
		names = append(names, f.Name)
	}
	ws, _ := json.Marshal(workspaces)
	rn, _ := json.Marshal(names)
	fj, _ := json.Marshal(facts)
	var info any
	if len(facts) > 0 {
		info = string(fj)
	}
	exec(t, sqlDB, `INSERT INTO workers (id, runners, workspaces, last_seen, updated_at, runner_info) VALUES (?, ?, ?, ?, '', ?)`,
		id, string(rn), string(ws), dbtime.Format(time.Now()), info)
}

var (
	wClaude = policy.Facts{Name: "claude", Kind: "claude", Provider: "anthropic", Model: "opus"}
	wGLM    = policy.Facts{Name: "opencode-glm", Kind: "opencode", Provider: "zai", Model: "zai-coding-plan/glm-5.3"}
)

// What applies to a workspace: the ceiling always; then its groups' and its
// own policies; the default only when it has none of those. Every layer
// must allow a runner, so each level can only narrow.
func TestEffectivePolicyLayers(t *testing.T) {
	s, _, _ := policyFixture(t)
	ctx := root()
	eff := func(ws string) *policy.Effective {
		t.Helper()
		e, err := s.EffectivePolicy(ctx, ws)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	if e := eff("fx/api"); !e.AllowAll() {
		t.Fatalf("no policy anywhere allows everything: %+v", e)
	}
	// Only a default: it covers every workspace.
	if _, err := s.SetPolicy(ctx, KindFleetDefault, "", allow("provider=anthropic", "provider=local"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	if got := sources(eff("fx/api")); got != "fleet default" {
		t.Fatalf("default only: %s", got)
	}
	// A group policy replaces the default for its workspaces — here it is
	// wider than the default, which a default does not prevent.
	if _, err := s.SetPolicy(ctx, KindGroup, "globex", allow("provider=anthropic", "provider=zai"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	if got := sources(eff("fx/api")); got != "group globex" {
		t.Fatalf("group replaces the default: %s", got)
	}
	if got := sources(eff("other/x")); got != "fleet default" {
		t.Fatalf("a workspace outside the group keeps the default: %s", got)
	}
	if !eff("fx/api").Allows(wGLM, "review").Allowed || eff("other/x").Allows(wGLM, "review").Allowed {
		t.Fatal("GLM is allowed in the group and not under the default")
	}
	// A workspace narrows its group; a second group narrows too.
	if _, err := s.SetPolicy(ctx, KindWorkspace, "fx/web", allow("provider=anthropic"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPolicy(ctx, KindGroup, "web-team", allow("any"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	if got := sources(eff("fx/web")); got != "group globex > group web-team > workspace" {
		t.Fatalf("groups then the workspace: %s", got)
	}
	if eff("fx/web").Allows(wGLM, "review").Allowed || !eff("fx/web").Allows(wClaude, "review").Allowed {
		t.Fatal("the workspace narrows what its groups allow")
	}
	// The ceiling applies to everything, on top.
	if _, err := s.SetPolicy(ctx, KindFleetCeiling, "", allow("provider=anthropic", "provider=local"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	if got := sources(eff("fx/api")); got != "fleet ceiling > group globex" {
		t.Fatalf("ceiling first: %s", got)
	}
	if eff("fx/api").Allows(wGLM, "review").Allowed {
		t.Fatal("the ceiling refuses what a group would allow")
	}
	// The run queue gets the same policy with the routing.
	if p := s.Prefs(ctx, "fx/api"); p.Policy == nil || p.Policy.Rev() != eff("fx/api").Rev() {
		t.Fatalf("the routing carries the policy: %+v", p.Policy)
	}
	// Clearing a level takes it out.
	if _, err := s.ClearPolicy(ctx, KindFleetCeiling, "", false, "cli"); err != nil {
		t.Fatal(err)
	}
	if got := sources(eff("fx/api")); got != "group globex" {
		t.Fatalf("after clearing the ceiling: %s", got)
	}
	if p := s.Prefs(ctx, "fx/api"); p.Policy.Rev() != eff("fx/api").Rev() {
		t.Fatal("a policy change must drop the cached routing")
	}
}

// Who may change a policy: an admin at the owner's level. Approvers and
// plain keys never; an admin over one workspace not a group or the fleet.
func TestPolicyNeedsAdminAtTheLevel(t *testing.T) {
	s, _, _ := policyFixture(t)
	p := allow("provider=anthropic")
	set := func(ctx context.Context, kind, id string) error {
		_, err := s.SetPolicy(ctx, kind, id, p, false, "cli")
		return err
	}
	approver := key("appr", true, "fx/api", "fx/web")
	for name, c := range map[string]struct {
		ctx      context.Context
		kind, id string
		ok       bool
	}{
		"root, fleet":                         {root(), KindFleetCeiling, "", true},
		"approver, workspace":                 {approver, KindWorkspace, "fx/api", false},
		"plain key, workspace":                {key("k", false, "fx/api"), KindWorkspace, "fx/api", false},
		"admin of the workspace":              {admin(false, "fx/api"), KindWorkspace, "fx/api", true},
		"admin of another workspace":          {admin(false, "fx/web"), KindWorkspace, "fx/api", false},
		"admin of one workspace, its group":   {admin(false, "fx/api"), KindGroup, "globex", false},
		"admin of every workspace in a group": {admin(false, "fx/api", "fx/web"), KindGroup, "globex", true},
		"admin of a group, the fleet":         {admin(false, "fx/api", "fx/web"), KindFleetDefault, "", false},
		"admin of all workspaces, the fleet":  {admin(true), KindFleetDefault, "", true},
	} {
		err := set(c.ctx, c.kind, c.id)
		if c.ok && err != nil {
			t.Errorf("%s: must be allowed: %v", name, err)
		}
		if !c.ok && !errors.Is(err, auth.ErrAdminRequired) {
			t.Errorf("%s: want ErrAdminRequired, got %v", name, err)
		}
	}
	// Reads: a workspace's policy by any key in scope, a foreign one looks
	// missing; the fleet's by anyone; a group's by its admins.
	if _, err := s.GetPolicy(key("k", false, "fx/api"), KindWorkspace, "fx/api"); err != nil {
		t.Fatalf("a key reads its workspace's policy: %v", err)
	}
	if _, err := s.GetPolicy(key("k", false, "fx/web"), KindWorkspace, "fx/api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a foreign workspace's policy: %v", err)
	}
	if _, err := s.GetPolicy(key("k", false, "fx/api"), KindFleetCeiling, ""); err != nil {
		t.Fatalf("the fleet policy binds everyone, so everyone reads it: %v", err)
	}
	if _, err := s.GetPolicy(key("k", false, "fx/api"), KindGroup, "globex"); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a group's policy by a plain key: %v", err)
	}
	if _, err := s.EffectivePolicyView(key("k", false, "fx/api"), "fx/api"); err != nil {
		t.Fatalf("a key sees what binds its workspace: %v", err)
	}
	if _, err := s.EffectivePolicyView(key("k", false, "fx/web"), "fx/api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("effective policy of a foreign workspace: %v", err)
	}
}

// A policy is stored as revisions and every change is recorded: who, the
// old and new revision, the rules. Saving the same policy again is no
// change; a policy with no rules, or a malformed one, is refused.
func TestPolicyRevisionsAndAudit(t *testing.T) {
	s, sqlDB, sink := policyFixture(t)
	ctx := admin(true)
	first, err := s.SetPolicy(ctx, KindGroup, "globex", allow("provider=anthropic"), false, "cli")
	if err != nil || first.Rev == "" || first.Policy == nil || first.UpdatedBy != "key ops" {
		t.Fatalf("first: %+v %v", first, err)
	}
	if again, err := s.SetPolicy(ctx, KindGroup, "g1", allow("provider=anthropic"), false, "cli"); err != nil || again.Rev != first.Rev {
		t.Fatalf("the same policy is not a change (and a group answers to its id too): %+v %v", again, err)
	}
	second, err := s.SetPolicy(ctx, KindGroup, "globex", allow("provider=anthropic", "provider=openai,stages=second_opinion"), false, "dashboard")
	if err != nil || second.Rev == first.Rev {
		t.Fatalf("second: %+v %v", second, err)
	}
	if _, err := s.ClearPolicy(ctx, KindGroup, "globex", false, "cli"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetPolicy(ctx, KindGroup, "globex"); err != nil || v.Policy != nil || v.Rev != "" {
		t.Fatalf("cleared: %+v %v", v, err)
	}
	var n int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM config_revisions WHERE kind = 'policy' AND owner = 'group:g1'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("both revisions stay after the clear: %d %v", n, err)
	}

	var got []string
	for _, e := range sink.entries {
		if e.EntityType != PolicyAuditEntity {
			continue
		}
		if e.EntityID != "group:g1" {
			t.Errorf("entity: %+v", e)
		}
		got = append(got, e.Action+" via "+e.Via+": "+e.Notes)
	}
	if len(got) != 3 {
		t.Fatalf("three changes, three events: %q", got)
	}
	if !strings.HasPrefix(got[0], "policy.set via cli: revision "+first.Rev+" (was none): allow provider=anthropic") {
		t.Fatalf("first event: %q", got[0])
	}
	if !strings.Contains(got[1], "revision "+second.Rev+" (was "+first.Rev+")") || !strings.Contains(got[1], "provider=openai,stages=second_opinion") || !strings.Contains(got[1], "via dashboard") {
		t.Fatalf("second event names both revisions and the rules: %q", got[1])
	}
	if got[2] != "policy.delete via cli: was revision "+second.Rev {
		t.Fatalf("delete event: %q", got[2])
	}

	for name, bad := range map[string]policy.Policy{
		"no rules":      {},
		"unknown stage": {Allow: []policy.Rule{{Stages: []string{"deploy"}}}},
	} {
		if _, err := s.SetPolicy(ctx, KindWorkspace, "fx/api", bad, false, "cli"); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: want ErrInvalidInput, got %v", name, err)
		}
	}
	if _, err := s.SetPolicy(ctx, KindWorkspace, "nope/x", allow("any"), false, "cli"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown workspace: %v", err)
	}
	if _, err := s.SetPolicy(ctx, KindGroup, "nope", allow("any"), false, "cli"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group: %v", err)
	}
}

// A policy that would leave a configured runner with no worker allowed to
// run it is refused unless forced, names what it strands, and a forced
// save is recorded as forced. A runner no worker reports is not judged.
func TestPolicySaveThatStrands(t *testing.T) {
	s, sqlDB, sink := policyFixture(t)
	ctx := root()
	// fx/api reviews on GLM and asks Claude for second opinions; a worker
	// offers both. fx/web plans on a runner nobody reports yet.
	if _, err := s.Put(ctx, KindWorkspace, "fx/api", Settings{
		Stages:              map[string]StageSetting{"review": {Members: []runs.Member{{Runner: "opencode-glm"}}}},
		SecondOpinionRunner: "claude",
	}, "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, KindWorkspace, "fx/web", Settings{Stages: map[string]StageSetting{"plan": {Members: []runs.Member{{Runner: "gemini"}}}}}, "cli"); err != nil {
		t.Fatal(err)
	}
	worker(t, sqlDB, "w1", []string{"fx/api", "fx/web"}, wClaude, wGLM)

	_, err := s.SetPolicy(ctx, KindGroup, "globex", allow("provider=anthropic"), false, "cli")
	if !errors.Is(err, ErrStrands) {
		t.Fatalf("want ErrStrands, got %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "fx/api: review uses runner opencode-glm (provider zai, model zai-coding-plan/glm-5.3)") ||
		!strings.Contains(msg, "the group globex policy does not allow") || strings.Contains(msg, "claude") || strings.Contains(msg, "gemini") {
		t.Fatalf("the refusal names exactly what is stranded: %s", msg)
	}
	if v, _ := s.GetPolicy(ctx, KindGroup, "globex"); v.Policy != nil {
		t.Fatal("a refused save must not be stored")
	}
	// A policy that keeps GLM is fine.
	if _, err := s.SetPolicy(ctx, KindGroup, "globex", allow("provider=anthropic", "provider=zai"), false, "cli"); err != nil {
		t.Fatalf("nothing stranded: %v", err)
	}
	// Forced: saved, with the warning returned and the event marked.
	v, err := s.SetPolicy(ctx, KindGroup, "globex", allow("provider=anthropic"), true, "cli")
	if err != nil || len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "opencode-glm") {
		t.Fatalf("forced save: %+v %v", v, err)
	}
	last := sink.entries[len(sink.entries)-1]
	if last.Action != "policy.set" || !strings.Contains(last.Notes, "[forced: it strands current settings]") {
		t.Fatalf("a forced save must be recorded as forced: %+v", last)
	}
	// Clearing a workspace's own policy can strand too (what then applies
	// may be narrower): the same check.
	if _, err := s.SetPolicy(ctx, KindWorkspace, "other/x", allow("any"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, KindWorkspace, "other/x", Settings{Stages: map[string]StageSetting{"review": {Members: []runs.Member{{Runner: "opencode-glm"}}}}}, "cli"); err != nil {
		t.Fatal(err)
	}
	worker(t, sqlDB, "w2", []string{"other/x"}, wGLM)
	if _, err := s.SetPolicy(ctx, KindFleetDefault, "", allow("provider=anthropic"), false, "cli"); err != nil {
		t.Fatalf("the default does not reach a workspace with its own policy: %v", err)
	}
	if _, err := s.ClearPolicy(ctx, KindWorkspace, "other/x", false, "cli"); !errors.Is(err, ErrStrands) {
		t.Fatalf("clearing falls back to the narrower default: want ErrStrands, got %v", err)
	}
}

// RunnerAllowed answers for one named runner on one stage: allowed, refused
// (with the layer), or unknown when no worker reports the name.
func TestRunnerAllowed(t *testing.T) {
	s, sqlDB, _ := policyFixture(t)
	ctx := root()
	if ok, known, _, err := s.RunnerAllowed(ctx, "fx/api", "second_opinion", "anything"); err != nil || !ok || !known {
		t.Fatalf("no policy: everything is allowed: %v %v %v", ok, known, err)
	}
	if _, err := s.SetPolicy(ctx, KindGroup, "globex", allow("provider=anthropic", "provider=openai,stages=second_opinion"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	codex := policy.Facts{Name: "codex", Kind: "codex", Provider: "openai"}
	worker(t, sqlDB, "w1", []string{"fx/api"}, wClaude, wGLM, codex)
	for _, c := range []struct {
		stage, runner string
		ok, known     bool
	}{
		{"second_opinion", "codex", true, true},
		{"review", "codex", false, true},
		{"second_opinion", "opencode-glm", false, true},
		{"second_opinion", "gemini", false, false},
	} {
		ok, known, refused, err := s.RunnerAllowed(ctx, "fx/api", c.stage, c.runner)
		if err != nil || ok != c.ok || known != c.known {
			t.Errorf("%s on %s: allowed=%v known=%v (%v), want %v %v", c.runner, c.stage, ok, known, err, c.ok, c.known)
		}
		if !ok && known && refused != "group globex" {
			t.Errorf("%s on %s: refused by %q", c.runner, c.stage, refused)
		}
	}
}

// A deleted group's policy stops applying; its revisions stay.
func TestOrphanPoliciesAreCleared(t *testing.T) {
	s, sqlDB, _ := policyFixture(t)
	ctx := root()
	if _, err := s.SetPolicy(ctx, KindGroup, "web-team", allow("provider=anthropic"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	exec(t, sqlDB, `DELETE FROM workspace_group_members WHERE group_id = 'g2'`)
	exec(t, sqlDB, `DELETE FROM workspace_groups WHERE id = 'g2'`)
	if err := s.DropOrphans(ctx); err != nil {
		t.Fatal(err)
	}
	heads, _ := confrev.NewStore(sqlDB).Heads(ctx, confrev.KindPolicy)
	if len(heads) != 0 {
		t.Fatalf("the deleted group's policy must be cleared: %v", heads)
	}
	var n int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM config_revisions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("its revision stays: %d %v", n, err)
	}
}

// The strand check also covers a stage that names no runner: stranded when
// workers serve the workspace and none has a runner the policy allows —
// including when the only workers report no facts — and it ignores a
// worker not seen for a day.
func TestPolicySaveStrandsUnpinnedStages(t *testing.T) {
	s, sqlDB, _ := policyFixture(t)
	ctx := root()
	// other/x: no stage preferences; a GLM worker and a worker without
	// facts serve it.
	worker(t, sqlDB, "w-glm", []string{"other/x"}, wGLM)
	worker(t, sqlDB, "w-old", []string{"other/x"})
	_, err := s.SetPolicy(ctx, KindWorkspace, "other/x", allow("provider=anthropic"), false, "cli")
	if !errors.Is(err, ErrStrands) {
		t.Fatalf("want ErrStrands, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "other/x: plan names no runner") || !strings.Contains(msg, "other/x: implement names no runner") ||
		!strings.Contains(msg, "workers that report no runner facts never pass a policy") {
		t.Fatalf("both always-on stages are stranded, and the fact-less worker is named as no help: %s", msg)
	}
	// With a Claude worker serving it, nothing is stranded.
	worker(t, sqlDB, "w-claude", []string{"other/x"}, wClaude)
	if _, err := s.SetPolicy(ctx, KindWorkspace, "other/x", allow("provider=anthropic"), false, "cli"); err != nil {
		t.Fatalf("a worker with an allowed runner serves it: %v", err)
	}
	// Only a worker without facts: said plainly.
	worker(t, sqlDB, "w-old-2", []string{"fx/api"})
	_, err = s.SetPolicy(ctx, KindWorkspace, "fx/api", allow("provider=anthropic"), false, "cli")
	if !errors.Is(err, ErrStrands) || !strings.Contains(err.Error(), "report no runner facts, so none passes a policy") {
		t.Fatalf("only fact-less workers: %v", err)
	}
	// A worker gone for two days does not make a runner look available.
	exec(t, sqlDB, `UPDATE workers SET last_seen = ? WHERE id = 'w-claude'`, dbtime.Format(time.Now().Add(-48*time.Hour)))
	if _, err := s.ClearPolicy(ctx, KindWorkspace, "other/x", false, "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPolicy(ctx, KindWorkspace, "other/x", allow("provider=anthropic"), false, "cli"); !errors.Is(err, ErrStrands) {
		t.Fatalf("the Claude worker is gone; the save strands again: %v", err)
	}
}

// A key that is not an admin learns nothing from the policy routes about
// which workspaces and groups exist; a deleted owner's policy removal is in
// the audit trail.
func TestPolicyRefusalsLeakNothingAndOrphansAreAudited(t *testing.T) {
	s, sqlDB, sink := policyFixture(t)
	plain := key("k", false, "fx/api")
	for name, call := range map[string]func() error{
		"set on a missing workspace": func() error {
			_, err := s.SetPolicy(plain, KindWorkspace, "nope/x", allow("any"), false, "cli")
			return err
		},
		"set on a real workspace": func() error {
			_, err := s.SetPolicy(plain, KindWorkspace, "fx/web", allow("any"), false, "cli")
			return err
		},
		"set on a missing group": func() error { _, err := s.SetPolicy(plain, KindGroup, "nope", allow("any"), false, "cli"); return err },
		"set on a real group": func() error {
			_, err := s.SetPolicy(plain, KindGroup, "globex", allow("any"), false, "cli")
			return err
		},
		"get a missing group": func() error { _, err := s.GetPolicy(plain, KindGroup, "nope"); return err },
		"get a real group":    func() error { _, err := s.GetPolicy(plain, KindGroup, "globex"); return err },
	} {
		if err := call(); !errors.Is(err, auth.ErrAdminRequired) || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: a non-admin gets the same refusal either way, got %v", name, err)
		}
	}
	// An admin of one workspace in a group is refused without the name of
	// the workspace it lacks.
	_, err := s.SetPolicy(admin(false, "fx/api"), KindGroup, "globex", allow("any"), false, "cli")
	if !errors.Is(err, auth.ErrAdminRequired) || strings.Contains(err.Error(), "fx/web") {
		t.Fatalf("the refusal must not name the group's other members: %v", err)
	}

	if _, err := s.SetPolicy(root(), KindGroup, "web-team", allow("provider=anthropic"), false, "cli"); err != nil {
		t.Fatal(err)
	}
	exec(t, sqlDB, `DELETE FROM workspace_group_members WHERE group_id = 'g2'`)
	exec(t, sqlDB, `DELETE FROM workspace_groups WHERE id = 'g2'`)
	if err := s.DropOrphans(root()); err != nil {
		t.Fatal(err)
	}
	last := sink.entries[len(sink.entries)-1]
	if last.Action != "policy.delete" || last.EntityID != "group:g2" || last.Via != "system" || !strings.Contains(last.Notes, "no longer exists") {
		t.Fatalf("an orphaned policy's removal must be recorded: %+v", last)
	}
}

// workerExec registers a worker with runner facts and what contains its
// runs in each workspace.
func workerExec(t *testing.T, sqlDB *sql.DB, id string, executors map[string][]policy.ExecutorFact, facts ...policy.Facts) {
	t.Helper()
	var workspaces []string
	for ws := range executors {
		workspaces = append(workspaces, ws)
	}
	worker(t, sqlDB, id, workspaces, facts...)
	ej, _ := json.Marshal(executors)
	exec(t, sqlDB, `UPDATE workers SET executor_info = ? WHERE id = ?`, string(ej), id)
}

// A rule with `executor` is judged per stage with what each worker says
// contains a run there: a save that would leave a configured runner with no
// worker confining it is refused, and RunnerAllowed answers by stage.
func TestExecutorRulesInSavesAndRunnerAllowed(t *testing.T) {
	s, sqlDB, _ := policyFixture(t)
	ctx := root()
	if _, err := s.Put(ctx, KindWorkspace, "fx/api", Settings{Stages: map[string]StageSetting{"review": {Members: []runs.Member{{Runner: "opencode-glm"}}}}}, "cli"); err != nil {
		t.Fatal(err)
	}
	// GLM runs unwrapped on w1; on w2 only its reviews are wrapped.
	workerExec(t, sqlDB, "w1", map[string][]policy.ExecutorFact{"fx/api": {{Executor: "local"}}}, wClaude, wGLM)
	rules := allow("provider=anthropic", "provider=zai,executor=isolated")
	_, err := s.SetPolicy(ctx, KindWorkspace, "fx/api", rules, false, "cli")
	if !errors.Is(err, ErrStrands) || !strings.Contains(err.Error(), "review uses runner opencode-glm") || !strings.Contains(err.Error(), "executor local") {
		t.Fatalf("an unwrapped GLM review would be stranded: %v", err)
	}
	workerExec(t, sqlDB, "w2", map[string][]policy.ExecutorFact{"fx/api": {{Executor: "local"}, {Stage: "review", Runner: "opencode-glm", Executor: "wrapper:bwrap:0123456789ab"}}}, wGLM)
	if _, err := s.SetPolicy(ctx, KindWorkspace, "fx/api", rules, false, "cli"); err != nil {
		t.Fatalf("a worker that wraps GLM reviews keeps the stage usable: %v", err)
	}
	for _, c := range []struct {
		stage string
		ok    bool
	}{{"review", true}, {"second_opinion", false}, {"qa", false}} {
		ok, known, refused, err := s.RunnerAllowed(ctx, "fx/api", c.stage, "opencode-glm")
		if err != nil || !known || ok != c.ok {
			t.Errorf("opencode-glm on %s: allowed=%v known=%v (%v), want %v", c.stage, ok, known, err, c.ok)
		}
		if !ok && refused != "workspace" {
			t.Errorf("%s refused by %q", c.stage, refused)
		}
	}
}

// A worker that reports it cannot run a stage with a runner (executor
// none:…) says nothing about the policy: it does not make a policy save
// look as if it stranded the runner, and a second opinion on that runner is
// not refused "by the policy" because of it.
func TestUnavailableExecutorIsNotAPolicyRefusal(t *testing.T) {
	s, sqlDB, _ := policyFixture(t)
	ctx := root()
	if _, err := s.Put(ctx, KindWorkspace, "fx/api", Settings{Stages: map[string]StageSetting{"review": {Members: []runs.Member{{Runner: "opencode-glm"}}}}}, "cli"); err != nil {
		t.Fatal(err)
	}
	// The only worker with GLM cannot run its reviews (its wrapper does
	// not start), and can run the rest.
	workerExec(t, sqlDB, "w1", map[string][]policy.ExecutorFact{"fx/api": {{Executor: "host:knot"},
		{Stage: "review", Runner: "opencode-glm", Executor: policy.NoExecutor(policy.NoneWrapper)},
		{Stage: "second_opinion", Runner: "opencode-glm", Executor: policy.NoExecutor(policy.NoneWrapper)}}}, wClaude, wGLM)
	// A policy that allows GLM anywhere: the save strands nothing.
	if _, err := s.SetPolicy(ctx, KindWorkspace, "fx/api", allow("provider=anthropic", "provider=zai"), false, "cli"); err != nil {
		t.Fatalf("a worker that cannot run the stage is not a policy refusal: %v", err)
	}
	ok, known, refused, err := s.RunnerAllowed(ctx, "fx/api", "second_opinion", "opencode-glm")
	if err != nil || ok || known || refused != "" {
		t.Fatalf("no worker can run it, and the policy refused nothing: allowed=%v known=%v refused=%q %v", ok, known, refused, err)
	}
	if ok, known, _, err := s.RunnerAllowed(ctx, "fx/api", "qa", "opencode-glm"); err != nil || !ok || !known {
		t.Fatalf("a stage the worker can run: allowed=%v known=%v %v", ok, known, err)
	}
}

// Once the server defines a runner, a pipeline save that newly names it is
// refused when the workspace's policy can never allow it — the answer does
// not depend on any worker. A runner the server does not define, one
// without a declared provider, and a setting that was already there are not
// refused.
func TestPipelineSaveRefusesADefinedRunnerThePolicyForbids(t *testing.T) {
	s, _, _ := policyFixture(t)
	defs := map[string]policy.Facts{
		"claude-opus": {Name: "claude-opus", Kind: "claude", Model: "opus", Provider: "anthropic"},
		"glm":         {Name: "glm", Kind: "opencode", Model: "zai-coding-plan/glm-5.3", Provider: "zai"},
		"undeclared":  {Name: "undeclared", Kind: "opencode", Model: "x/y"},
	}
	s.SetRunnerDefs(func(context.Context) (map[string]policy.Facts, error) { return defs, nil })
	put := func(kind, id string, stages map[string]string, second string) error {
		t.Helper()
		st := Settings{Stages: map[string]StageSetting{}, SecondOpinionRunner: second}
		for stage, spec := range stages {
			st.Stages[stage] = StageSetting{Members: members(t, spec)}
		}
		_, err := s.Put(root(), kind, id, st, "cli")
		return err
	}
	// Before any policy: anything goes.
	if err := put(KindWorkspace, "fx/api", map[string]string{"review": "glm"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPolicy(root(), KindGroup, "globex", allow("provider=anthropic", "provider=openai,stages=second_opinion", "provider=zai,executor=isolated,stages=qa"), true, "cli"); err != nil {
		t.Fatal(err)
	}
	// The setting that was already there does not block an unrelated edit.
	if err := put(KindWorkspace, "fx/api", map[string]string{"review": "glm", "plan": "claude-opus"}, ""); err != nil {
		t.Fatalf("an existing setting is not checked again: %v", err)
	}
	// Newly naming it is refused.
	err := put(KindWorkspace, "fx/api", map[string]string{"review": "glm", "plan": "glm"}, "")
	if !errors.Is(err, ErrPolicyRefuses) || !strings.Contains(err.Error(), "glm") || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("want ErrPolicyRefuses naming the runner and stage, got %v", err)
	}
	if err := put(KindWorkspace, "fx/api", map[string]string{"review": "glm"}, "glm"); !errors.Is(err, ErrPolicyRefuses) {
		t.Fatalf("the second-opinion default is checked too: %v", err)
	}
	// A rule that needs an executor could still allow it somewhere: no
	// worker's containment is known here, so the save is not refused.
	if err := put(KindWorkspace, "fx/api", map[string]string{"review": "glm", "qa": "glm"}, ""); err != nil {
		t.Fatalf("zai is allowed for qa where something isolates the run: %v", err)
	}
	// Not defined on the server, or no declared provider: nothing certain.
	if err := put(KindWorkspace, "fx/api", map[string]string{"plan": "some-local-runner", "review": "undeclared"}, ""); err != nil {
		t.Fatalf("an undefined runner is only warned about: %v", err)
	}
	// A group save is checked for the workspaces that take the stage from
	// it; one that sets the stage itself is skipped.
	if err := put(KindWorkspace, "fx/web", map[string]string{"plan": "claude-opus"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := put(KindWorkspace, "fx/api", map[string]string{"plan": "claude-opus"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := put(KindGroup, "globex", map[string]string{"plan": "glm"}, ""); err != nil {
		t.Fatalf("every member sets plan itself: %v", err)
	}
	if err := put(KindGroup, "globex", map[string]string{"plan": "glm", "review": "glm"}, ""); !errors.Is(err, ErrPolicyRefuses) {
		t.Fatalf("the members take review from the group: %v", err)
	}
}
