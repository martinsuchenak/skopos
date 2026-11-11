package routes

import (
	"net/url"
	"strings"
	"testing"
	"time"

	wk "github.com/martinsuchenak/skopos/internal/worker"
)

// Workspace config on the server, end to end through the real routes and
// the worker's own client: an admin saves a workspace's config (an approver
// and the worker's key may not); the claim names the revision in force and
// the worker fetches exactly that one; a change gives the next claim a new
// revision while the old one still reads back as it was; runner definitions
// reach the worker by their revision; a pipeline save that names a defined
// runner the policy forbids is refused; the whole of it exports as one
// document that applies back with nothing to do; and clearing the config
// hands the workspace back to the workers' files.
func TestIntegrationWorkspaceConfig(t *testing.T) {
	sqlDB, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws = "github.com/fortix/freedom3"
	wsPath := "/api/workspaces/" + url.PathEscape(ws)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"freedom3"}`), 201)
	mustDo(t, root.req("POST", "/api/groups", `{"name":"fortix","members":["`+ws+`"]}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"martin","workspaces":["`+ws+`"],"approver":true}`)}
	ops := workerClient{base: baseURL, key: mint(t, root, `{"name":"ops","groups":["fortix"],"admin":true}`)}
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`)
	worker := workerClient{base: baseURL, key: workerKey}
	wc := wk.NewClient(baseURL, workerKey)
	ctx := t.Context()

	// Not managed yet: the worker's file applies, and a claim names nothing.
	eff, err := wc.EffectiveConfig(ctx, ws)
	if err != nil || eff == nil || eff.Managed || eff.Rev != "" {
		t.Fatalf("before any config: %+v %v", eff, err)
	}

	// Who may save it.
	body := `{"config":{"default_branch":"develop","setup":["npm ci"],"checks":["npm test"],"plan_timeout":"20m","push":{"kind":"github","remote_url":"https://github.com/fortix/freedom3.git","pull_request":"draft"}}}`
	mustDo(t, approver.req("PUT", wsPath+"/config", body), 403, "an approver is not an admin")
	mustDo(t, worker.req("PUT", wsPath+"/config", body), 403, "the worker's key")
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"checkout":"copy"}}`), 400, "an unknown checkout")
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"push":{"kind":"github"}}}`), 400, "a push without a remote URL")
	// A body that would be read as "nothing set" by mistake is refused: a
	// misspelt field, or the fields without the "config" wrapper.
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"check":["npm test"]}}`), 400, "a misspelt field")
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"checks":["npm test"]}`), 400, "the fields without the config wrapper")
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"note":"nothing"}`), 400, "no config at all")
	if eff, err := wc.EffectiveConfig(ctx, ws); err != nil || eff.Managed {
		t.Fatalf("a refused body must not manage the workspace: %+v %v", eff, err)
	}
	saved := mustJSON(t, ops.req("PUT", wsPath+"/config", body), 200).(map[string]any)
	// A save that names the revision it read is refused once that is stale.
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"if_rev":"","config":{}}`), 409, "read as empty, saved since")
	if saved["updated_by"] != "key ops" || saved["rev"] == "" {
		t.Fatalf("the saved config names its revision and who saved it: %v", saved)
	}
	// A group's values are defaults under it.
	mustDo(t, ops.req("PUT", "/api/groups/fortix/config", `{"config":{"check_timeout":"45m","checks":["group check"]}}`), 200)
	mustDo(t, approver.req("GET", "/api/groups/fortix/config", ""), 403, "a group's config is its admins'")

	eff, err = wc.EffectiveConfig(ctx, ws)
	if err != nil || !eff.Managed || eff.Rev == "" {
		t.Fatalf("managed: %+v %v", eff, err)
	}
	first := eff.Rev
	if (*eff.Config.Checks)[0] != "npm test" || eff.Sources["checks"] != "workspace" || eff.Config.CheckTimeout.D() != 45*time.Minute || eff.Sources["check_timeout"] != "group:fortix" {
		t.Fatalf("the workspace's own field wins, the group fills in: %+v", eff)
	}

	// The claim names the revision; the worker fetches exactly that one.
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"config item","author_agent_id":"martin"}`), 201).(map[string]any)
	mustDo(t, approver.req("POST", "/api/inbox/"+item["id"].(string)+"/queue", ""), 200)
	run, err := wc.Claim(ctx, "w1", []string{"claude"}, []string{ws}, nil, nil, nil)
	if err != nil || run == nil {
		t.Fatalf("claim: %+v %v", run, err)
	}
	if run.ConfigRev != first {
		t.Fatalf("the claim names the config in force: %q, want %q", run.ConfigRev, first)
	}
	if wc.ConfigDigest() == "" {
		t.Fatal("the claim answer carries one value for the offered workspaces' config")
	}
	at, err := wc.ConfigAt(ctx, ws, run.ConfigRev)
	if err != nil || at.Rev != first || (*at.Config.Setup)[0] != "npm ci" {
		t.Fatalf("the run's revision: %+v %v", at, err)
	}

	// The config changes while the run is out.
	digest := wc.ConfigDigest()
	mustDo(t, ops.req("PUT", wsPath+"/config", strings.Replace(body, `"npm test"`, `"npm run ci"`, 1)), 200)
	if at, err := wc.ConfigAt(ctx, ws, first); err != nil || (*at.Config.Checks)[0] != "npm test" {
		t.Fatalf("the run's revision reads back as it was: %+v %v", at, err)
	}
	if err := wc.Pause(ctx, run.ID, "w1", "quota", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	again, err := wc.Claim(ctx, "w1", []string{"claude"}, []string{ws}, nil, nil, nil)
	if err != nil || again == nil || again.ID != run.ID {
		t.Fatalf("the run comes back: %+v %v", again, err)
	}
	if again.ConfigRev == "" || again.ConfigRev == first {
		t.Fatalf("the next attempt runs under the new revision: %q (was %q)", again.ConfigRev, first)
	}
	if wc.ConfigDigest() == digest {
		t.Fatal("a config change must change the claim answer's digest")
	}
	if at, _ := wc.ConfigAt(ctx, ws, again.ConfigRev); (*at.Config.Checks)[0] != "npm run ci" {
		t.Fatalf("the new revision: %+v", at)
	}
	// A revision is its workspace's: another key's scope does not reach it.
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"github.com/other/repo"}`), 201)
	outsider := wk.NewClient(baseURL, mint(t, root, `{"name":"elsewhere","workspaces":["github.com/other/repo"]}`))
	if _, err := outsider.ConfigAt(ctx, ws, first); err == nil {
		t.Fatal("a key without the workspace must not read its config")
	}

	// Runner definitions: the fleet's, read by every worker.
	def := `{"kind":"opencode","model":"zai-coding-plan/glm-5.3","provider":"zai"}`
	mustDo(t, ops.req("PUT", "/api/fleet/runners/glm", def), 403, "a group admin does not define runners")
	mustDo(t, root.req("PUT", "/api/fleet/runners/glm", `{"kind":"command"}`), 400, "a command runner stays in the worker's file")
	mustDo(t, root.req("PUT", "/api/fleet/runners/glm", def), 200)
	mustDo(t, root.req("PUT", "/api/fleet/runners/claude-opus", `{"kind":"claude","model":"opus","effort":"high","provider":"anthropic"}`), 200)
	if _, err := wc.Claim(ctx, "w1", []string{"claude"}, []string{ws}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	fleet, err := wc.FleetConfig(ctx)
	if err != nil || len(fleet.Runners) != 2 || fleet.Rev == "" || wc.FleetRev() != fleet.Rev {
		t.Fatalf("the claim answer names the definitions' revision: %+v %v %q", fleet, err, wc.FleetRev())
	}

	// A pipeline save that newly names a defined runner the policy never
	// allows is refused; one it allows is saved.
	mustDo(t, ops.req("PUT", "/api/groups/fortix/policy", `{"allow":[{"provider":"anthropic"}],"force":true}`), 200)
	refused := mustDo(t, approver.req("PUT", wsPath+"/pipeline", `{"stages":{"review":{"members":[{"runner":"glm"}]}}}`), 409)
	if !strings.Contains(refused.body, "glm") || !strings.Contains(refused.body, "policy") {
		t.Fatalf("the refusal names the runner: %s", refused.body)
	}
	mustDo(t, approver.req("PUT", wsPath+"/pipeline", `{"stages":{"review":{"members":[{"runner":"claude-opus"}]}}}`), 200)

	// Everything as one document, and back with nothing to do.
	mustDo(t, ops.req("GET", "/api/config/export", ""), 403, "an export lists every workspace")
	exported := root.req("GET", "/api/config/export", "")
	mustDo(t, exported, 200)
	for _, want := range []string{`"glm"`, `"fortix"`, ws, `"npm run ci"`, `"claude-opus"`} {
		if !strings.Contains(exported.body, want) {
			t.Fatalf("the export lacks %s: %s", want, exported.body)
		}
	}
	plan := mustJSON(t, root.req("POST", "/api/config/apply", `{"plan":true,"file_hash":"h1","commit":"c1","document":`+exported.body+`}`), 200).(map[string]any)
	if changes, _ := plan["changes"].([]any); len(changes) != 0 || plan["applied"] != false {
		t.Fatalf("export then apply changes nothing: %v", plan)
	}
	mustDo(t, approver.req("POST", "/api/config/apply", `{"plan":true,"document":{}}`), 403, "an apply needs the admin permission")
	// A misspelt section must not read as one left out (which clears it).
	mustDo(t, root.req("POST", "/api/config/apply", `{"plan":true,"document":{"workspaces":{"`+ws+`":{"confg":{"checks":["x"]}}}}}`), 400, "a misspelt section")
	// The group's admin sees only what it administers as unlisted.
	scopedPlan := mustJSON(t, ops.req("POST", "/api/config/apply", `{"plan":true,"document":{}}`), 200).(map[string]any)
	if unlisted, _ := scopedPlan["unlisted"].([]any); len(unlisted) != 1 || unlisted[0] != "workspace:"+ws {
		t.Fatalf("a scoped admin's plan names only its own workspace: %v", scopedPlan["unlisted"])
	}
	// A change through the file: recorded with where the file came from.
	changed := strings.Replace(exported.body, `"npm run ci"`, `"npm run verify"`, 1)
	applied := mustJSON(t, root.req("POST", "/api/config/apply", `{"file_hash":"h2","commit":"c2","document":`+changed+`}`), 200).(map[string]any)
	if changes, _ := applied["changes"].([]any); len(changes) != 1 || applied["applied"] != true ||
		changes[0].(map[string]any)["kind"] != "config" || changes[0].(map[string]any)["revision"] == "" {
		t.Fatalf("the apply reports the revision it created: %v", applied)
	}

	// The trail: who changed the config, how, and the commands in full.
	var notes, vias string
	if err := sqlDB.QueryRow(`SELECT group_concat(action || ' ' || actor || ' ' || COALESCE(via, ''), '; '), group_concat(COALESCE(notes, ''), ' | ')
		FROM (SELECT * FROM audit_log WHERE entity_type = 'workspace_config' AND entity_id = ? ORDER BY id)`, "workspace:"+ws).Scan(&vias, &notes); err != nil {
		t.Fatal(err)
	}
	if vias != "config.update key ops api; config.update key ops api; config.update root apply" {
		t.Fatalf("audit: %s", vias)
	}
	if !strings.Contains(notes, `checks = ["npm test"]`) || !strings.Contains(notes, `checks = ["npm run verify"]`) {
		t.Fatalf("command fields are recorded with their values: %s", notes)
	}
	var applies int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'config.apply' AND entity_id = 'h2' AND notes LIKE '%commit c2%'`).Scan(&applies); err != nil || applies != 1 {
		t.Fatalf("the apply is recorded with the file's hash and commit: %d %v", applies, err)
	}
	var runRev string
	if err := sqlDB.QueryRow(`SELECT config_rev FROM runs WHERE id = ?`, run.ID).Scan(&runRev); err != nil || runRev != again.ConfigRev {
		t.Fatalf("the run records the revision of its last claim: %q %v", runRev, err)
	}

	// Clearing it hands the workspace back to the workers' files.
	mustDo(t, approver.req("DELETE", wsPath+"/config", ""), 403)
	mustDo(t, ops.req("DELETE", wsPath+"/config", ""), 200)
	if eff, err := wc.EffectiveConfig(ctx, ws); err != nil || eff.Managed {
		t.Fatalf("cleared: %+v %v", eff, err)
	}
	if at, err := wc.ConfigAt(ctx, ws, first); err != nil || (*at.Config.Checks)[0] != "npm test" {
		t.Fatalf("revisions stay readable after the config is cleared: %+v %v", at, err)
	}
}
