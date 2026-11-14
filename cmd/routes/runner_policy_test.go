package routes

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	wk "github.com/martinsuchenak/skopos/internal/worker"
)

const (
	factsClaude = `{"name":"claude","kind":"claude","model":"claude-opus","provider":"anthropic","endpoint":"api.anthropic.com"}`
	factsGLM    = `{"name":"opencode-glm","kind":"opencode","model":"zai-coding-plan/glm-5.3","provider":"zai","endpoint":"opencode:zai-coding-plan"}`
	factsCodex  = `{"name":"codex","kind":"codex","model":"gpt-5.2","provider":"openai","endpoint":"api.openai.com","stages":["plan","review","second_opinion"]}`
)

// The runner policy end to end, through the real routes: an admin of a
// group sets its policy (an approver may not; the group admin may not
// touch the fleet); a worker whose runner the policy refuses gets nothing,
// one without runner facts gets nothing, and one that passes gets the run
// with its runner, the policy revision and the facts recorded; a second
// opinion from a refused runner is turned down with reason "policy"; and
// the changes are in the audit trail.
func TestIntegrationRunnerPolicy(t *testing.T) {
	sqlDB, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws = "github.com/globex/shop"
	wsPath := "/api/workspaces/" + url.PathEscape(ws)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"shop"}`), 201)
	mustDo(t, root.req("POST", "/api/groups", `{"name":"globex","members":["`+ws+`"]}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	ops := workerClient{base: baseURL, key: mint(t, root, `{"name":"ops","groups":["globex"],"admin":true}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}

	// Who may set it.
	policyBody := `{"allow":[{"provider":"anthropic"},{"provider":"openai","stages":["second_opinion"]}],"via":"cli"}`
	mustDo(t, approver.req("PUT", "/api/groups/globex/policy", policyBody), 403, "an approver is not an admin")
	mustDo(t, worker.req("PUT", wsPath+"/policy", policyBody), 403, "a plain key")
	mustDo(t, ops.req("PUT", "/api/policy/fleet/ceiling", policyBody), 403, "a group admin does not reach the fleet")
	mustDo(t, ops.req("PUT", "/api/groups/globex/policy", `{"allow":[{"stages":["deploy"]}]}`), 400, "an unknown stage")
	// A misspelt field is refused: read as left out, the rule would allow
	// every executor (or every stage, or every provider).
	mustDo(t, ops.req("PUT", "/api/groups/globex/policy", `{"allow":[{"provider":"anthropic","executer":"isolated"}]}`), 400, "a misspelt rule field")
	mustDo(t, ops.req("PUT", "/api/groups/globex/policy", `{"allow":[{"provider":"anthropic","stage":["review"]}]}`), 400, "stage for stages")
	mustDo(t, ops.req("PUT", "/api/groups/globex/policy", `{"alow":[{"provider":"anthropic"}]}`), 400, "a misspelt allow")
	set := mustJSON(t, ops.req("PUT", "/api/groups/globex/policy", policyBody), 200).(map[string]any)
	rev, _ := set["rev"].(string)
	if rev == "" || set["updated_by"] != "key ops" {
		t.Fatalf("the saved policy names its revision and who set it: %v", set)
	}
	// Everyone in scope can see what binds the workspace.
	eff := mustJSON(t, worker.req("GET", wsPath+"/policy/effective", ""), 200).(map[string]any)
	layers, _ := eff["layers"].([]any)
	if eff["allow_all"] != false || len(layers) != 1 || layers[0].(map[string]any)["source"] != "group globex" || layers[0].(map[string]any)["rev"] != rev {
		t.Fatalf("effective policy: %v", eff)
	}
	// The pipeline view shows it too.
	view := mustJSON(t, approver.req("GET", wsPath+"/pipeline", ""), 200).(map[string]any)
	if p, _ := view["policy"].(map[string]any); p == nil || p["allow_all"] != false {
		t.Fatalf("the pipeline view carries the policy: %v", view["policy"])
	}

	// An item's plan run: nothing pins it.
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"policy item","author_agent_id":"alex"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)
	claimBody := func(workerID string, runners string, facts ...string) string {
		b := `{"worker_id":"` + workerID + `","runners":[` + runners + `],"workspaces":["` + ws + `"]`
		if len(facts) > 0 {
			b += `,"runner_facts":[` + strings.Join(facts, ",") + `]`
		}
		return b + `}`
	}
	mustDo(t, worker.req("POST", "/api/runs/claim", claimBody("w-glm", `"opencode-glm"`, factsGLM)), 204, "GLM is not allowed on this workspace")
	mustDo(t, worker.req("POST", "/api/runs/claim", claimBody("w-old", `"claude"`)), 204, "a worker without runner facts gets nothing under a policy")

	// The server says it reads runner facts.
	req, _ := http.NewRequest("POST", baseURL+"/api/runs/claim", strings.NewReader(claimBody("w-codex", `"codex"`, factsCodex)))
	req.Header.Set("Authorization", "Bearer "+worker.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Skopos-Runner-Facts") == "" {
		t.Fatalf("codex is allowed for second opinions only, and the header must be set: %d %q", resp.StatusCode, resp.Header.Get("Skopos-Runner-Facts"))
	}

	run := claim(t, worker, claimBody("w-claude", `"opencode-glm","claude"`, factsGLM, factsClaude))
	runID := run["id"].(string)
	if run["runner"] != "claude" || run["runner_picked"] != true || run["policy_rev"] == "" || run["policy_rev"] == nil {
		t.Fatalf("the server picks the allowed runner and records the policy: %v", run)
	}
	facts, _ := run["facts"].(map[string]any)
	if facts == nil || facts["provider"] != "anthropic" || facts["endpoint"] != "api.anthropic.com" || facts["picked"] != true {
		t.Fatalf("the run records the facts it was decided with: %v", run["facts"])
	}
	if revs, _ := facts["policy"].([]any); len(revs) != 1 || revs[0] != rev {
		t.Fatalf("the run names the policy revision that applied: %v", facts["policy"])
	}

	// Through plan, approval and implementation to the sign-off.
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"p","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	revID := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"0a1b2c3"}`), 201).(map[string]any)["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w-claude","status":"succeeded","summary":"plan ready","branch":"agent/p"}`, leases[runID]), 200)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+revID+`"}`), 200)
	impl := claim(t, worker, claimBody("w-claude", `"opencode-glm","claude"`, factsGLM, factsClaude))
	if impl["stage"] != "implement" || impl["runner"] != "claude" {
		t.Fatalf("implement on the allowed runner: %v", impl)
	}
	mustDo(t, worker.reqH("POST", "/api/runs/"+impl["id"].(string)+"/report", `{"worker_id":"w-claude","status":"succeeded","summary":"done","head_sha":"abc1234def","branch":"agent/p"}`, leases[impl["id"].(string)]), 200)

	// A second opinion from GLM is refused by the policy; from codex it is
	// queued (the group allows OpenAI for second opinions).
	refused := approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"opencode-glm"}`)
	if refused.code != 403 || !strings.Contains(refused.body, `"reason":"policy"`) || !strings.Contains(refused.body, "group globex policy does not allow runner opencode-glm") {
		t.Fatalf("a second opinion from a refused runner: %d %s", refused.code, refused.body)
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"codex"}`), 202, "codex second opinion")
	so := claim(t, worker, claimBody("w-codex", `"codex"`, factsCodex))
	if so["stage"] != "second_opinion" || so["runner"] != "codex" {
		t.Fatalf("the codex worker takes the second opinion: %v", so)
	}

	// The audit trail has the policy change and what each worker reported.
	count := func(action string) int {
		var n int
		if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("policy.set") != 1 {
		t.Fatalf("one policy change, one event: %d", count("policy.set"))
	}
	if count("worker.facts") < 3 {
		t.Fatalf("each worker's first report of its runner facts is recorded: %d", count("worker.facts"))
	}
	var notes, actor string
	if err := sqlDB.QueryRow(`SELECT notes, actor FROM audit_log WHERE action = 'policy.set'`).Scan(&notes, &actor); err != nil {
		t.Fatal(err)
	}
	if actor != "key ops" || !strings.Contains(notes, "revision "+rev) || !strings.Contains(notes, "provider=anthropic") {
		t.Fatalf("the policy event names who and what: %q %q", actor, notes)
	}

	// Clearing the policy opens the workspace again, GLM included.
	mustDo(t, ops.req("DELETE", "/api/groups/globex/policy", ""), 200)
	if again := mustJSON(t, worker.req("GET", wsPath+"/policy/effective", ""), 200).(map[string]any); again["allow_all"] != true {
		t.Fatalf("no policy after the clear: %v", again)
	}
}

// The real worker client against the real routes: it sends the facts its
// config derives, learns that the server reads them, and gets a run that
// names the runner the server picked — or nothing when the policy refuses
// its only runner. The worker list then shows what it reported.
func TestIntegrationWorkerSendsRunnerFacts(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"} {
		t.Setenv(k, "")
	}
	_, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws = "github.com/acme/app"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"app"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`)
	mustDo(t, root.req("PUT", "/api/workspaces/"+url.PathEscape(ws)+"/policy", `{"allow":[{"provider":"anthropic"}]}`), 200)
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"facts item","author_agent_id":"alex"}`), 201).(map[string]any)
	mustDo(t, approver.req("POST", "/api/inbox/"+item["id"].(string)+"/queue", ""), 200)

	glm := &wk.Config{Runners: []string{"opencode-glm"}, RunnerSpecs: map[string]*wk.RunnerSpec{"opencode-glm": {Kind: "opencode"}},
		Accounts: []*wk.Account{{Name: "glm", Runner: "opencode-glm", Model: "zai-coding-plan/glm-5.3"}}}
	wcGLM := wk.NewClient(baseURL, workerKey)
	if run, err := wcGLM.Claim(t.Context(), "worker-glm", glm.Runners, []string{ws}, nil, nil, glm.RunnerFacts()); err != nil || run != nil {
		t.Fatalf("the policy keeps the workspace to Anthropic; a GLM worker gets nothing: %+v %v", run, err)
	}
	if !wcGLM.FactsUsed() {
		t.Fatal("the worker must learn that the server reads its runner facts")
	}

	claude := &wk.Config{Runners: []string{"claude"}, Accounts: []*wk.Account{{Name: "c1", Runner: "claude", Model: "opus"}}}
	wcClaude := wk.NewClient(baseURL, workerKey)
	run, err := wcClaude.Claim(t.Context(), "worker-claude", claude.Runners, []string{ws}, nil, nil, claude.RunnerFacts())
	if err != nil || run == nil || run.Runner != "claude" || run.AgentToken == "" {
		t.Fatalf("the Claude worker gets the run with its runner named: %+v %v", run, err)
	}

	// A worker that sends no facts does not claim that the server used them.
	wcOld := wk.NewClient(baseURL, workerKey)
	if _, err := wcOld.Claim(t.Context(), "worker-old", []string{"claude"}, []string{ws}, nil, nil, nil); err != nil || wcOld.FactsUsed() {
		t.Fatalf("no facts sent: %v %v", err, wcOld.FactsUsed())
	}

	seen := map[string]string{}
	for _, w := range workerList(t, root) {
		row := w.(map[string]any)
		facts, _ := row["runner_facts"].([]any)
		if len(facts) == 1 {
			f := facts[0].(map[string]any)
			seen[row["id"].(string)] = f["name"].(string) + "/" + f["provider"].(string) + "/" + f["endpoint"].(string)
		}
	}
	if seen["worker-glm"] != "opencode-glm/zai/opencode:zai-coding-plan" || seen["worker-claude"] != "claude/anthropic/api.anthropic.com" || seen["worker-old"] != "" {
		t.Fatalf("the worker list shows what each worker reported: %v", seen)
	}
}

// Containment as a policy rule, end to end: a worker reports what contains
// its runs from its own config, the claim matches a rule's `executor` on
// it, and a report whose executor the policy does not allow fails the run
// and is recorded. Another key sees only its own workspaces' executors.
func TestIntegrationExecutorRule(t *testing.T) {
	sqlDB, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws, other = "github.com/acme/app", "github.com/acme/other"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"app"}`), 201)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+other+`","name":"other"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`","`+other+`"]}`)
	wsPath := "/api/workspaces/" + url.PathEscape(ws)
	mustDo(t, root.req("PUT", wsPath+"/policy", `{"allow":[{"provider":"anthropic"},{"provider":"zai","executor":"sometimes"}]}`), 400)
	view := mustJSON(t, root.req("PUT", wsPath+"/policy", `{"allow":[{"provider":"anthropic"},{"provider":"zai","executor":"isolated"}]}`), 200).(map[string]any)
	if rules := view["policy"].(map[string]any)["allow"].([]any); rules[1].(map[string]any)["executor"] != "isolated" {
		t.Fatalf("the rule keeps its executor: %v", view)
	}
	mustDo(t, approver.req("PUT", wsPath+"/pipeline", `{"stages":{"plan":{"members":[{"runner":"opencode-glm"}]}}}`), 200)
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"contained item","author_agent_id":"alex"}`), 201).(map[string]any)
	mustDo(t, approver.req("POST", "/api/inbox/"+item["id"].(string)+"/queue", ""), 200)

	config := func(wrapper []string) *wk.Config {
		c := &wk.Config{Runners: []string{"opencode-glm"}, Workspaces: []string{ws, other},
			RunnerSpecs: map[string]*wk.RunnerSpec{"opencode-glm": {Kind: "opencode"}},
			Accounts:    []*wk.Account{{Name: "glm", Runner: "opencode-glm", Model: "zai-coding-plan/glm-5.3"}},
			Repos:       map[string]*wk.Repo{ws: {Path: "/clones/app"}, other: {Path: "/clones/other"}}}
		if wrapper != nil {
			c.Profiles = map[string]map[string]*wk.StageProfile{"bwrap": {"plan": {Wrapper: wrapper}}}
			c.Repos[ws].Profile = "bwrap"
		}
		if err := c.Default(t.TempDir()); err != nil {
			t.Fatal(err)
		}
		return c
	}
	claimAs := func(id string, c *wk.Config) (*wk.Client, *wk.Run) {
		t.Helper()
		wc := wk.NewClient(baseURL, workerKey)
		wc.SetExecutors(c.ExecutorFacts())
		run, err := wc.Claim(t.Context(), id, c.Runners, c.Workspaces, nil, nil, c.RunnerFacts())
		if err != nil {
			t.Fatalf("claim by %s: %v", id, err)
		}
		return wc, run
	}
	if _, run := claimAs("worker-bare", config(nil)); run != nil {
		t.Fatalf("GLM with nothing around it must not get the run: %+v", run)
	}
	if _, run := claimAs("worker-env", config([]string{"env", "SKOPOS_CONTAINED_BY=knot"})); run != nil {
		t.Fatalf("a host container is not isolation: %+v", run)
	}
	wrapped := config([]string{"bwrap", "--bind", "{dir}", "{dir}"})
	wc, run := claimAs("worker-bwrap", wrapped)
	if run == nil || run.Runner != "opencode-glm" {
		t.Fatalf("the wrapped worker takes it: %+v", run)
	}
	want := wrapped.ExecutorFor(ws, "plan", "opencode-glm")
	stored := mustJSON(t, root.req("GET", "/api/runs/"+run.ID, ""), 200).(map[string]any)
	if got := stored["facts"].(map[string]any)["executor"]; got != want || !strings.HasPrefix(want, "wrapper:bwrap:") {
		t.Fatalf("the run records the executor the claim matched on: %v, want %s", got, want)
	}

	// The workers list: what each reported; a key sees its workspaces only.
	execOf := func(c workerClient, id string) map[string]any {
		for _, w := range workerList(t, c) {
			if row := w.(map[string]any); row["id"] == id {
				m, _ := row["executors"].(map[string]any)
				return m
			}
		}
		return nil
	}
	if got := execOf(root, "worker-bwrap"); len(got) != 2 || len(got[ws].([]any)) != 2 || len(got[other].([]any)) != 1 {
		t.Fatalf("root sees every workspace's executors: %v", got)
	}
	if got := execOf(approver, "worker-bwrap"); len(got) != 1 || got[ws] == nil {
		t.Fatalf("a scoped key sees its own workspace's executors only: %v", got)
	}

	// The worker says it ran local after all: the policy does not allow
	// GLM there, so the run is failed and the difference recorded.
	if err := wc.ReportResult(t.Context(), run.ID, "worker-bwrap", true, map[string]any{"summary": "planned", "executor": "local"}); err != nil {
		t.Fatal(err)
	}
	after := mustJSON(t, root.req("GET", "/api/runs/"+run.ID, ""), 200).(map[string]any)
	if after["status"] != "failed" || !strings.Contains(after["fail_reason"].(string), "ran local, the claim matched "+want) || after["executor"] != "local" {
		t.Fatalf("a run that ran outside the rule is failed: %v", after)
	}
	var n int
	var notes string
	if err := sqlDB.QueryRow(`SELECT COUNT(*), COALESCE(MAX(notes), '') FROM audit_log WHERE action = 'run.executor_mismatch' AND entity_id = ?`, run.ID).Scan(&n, &notes); err != nil || n != 1 ||
		!strings.Contains(notes, "the workspace policy does not allow local there") {
		t.Fatalf("mismatch audit: %d %q %v", n, notes, err)
	}
	if got := mustJSON(t, root.req("GET", "/api/inbox/"+item["id"].(string), ""), 200).(map[string]any); got["status"] != "blocked" {
		t.Fatalf("the item is blocked, not moved on: %v", got["status"])
	}
}
