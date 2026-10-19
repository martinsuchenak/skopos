package routes

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/apikeys"
	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/pipeline"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/runs"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	logslog "github.com/paularlott/logger/slog"
	_ "modernc.org/sqlite"
)

// The stub-worker round trip: a fake worker drives the whole loop over REST
// with real keys — queue -> plan run -> gate -> approve -> implement run ->
// in_review -> mark done — including the per-workspace stage preference and
// a review stage chained by the queue.
func TestIntegrationStubWorkerRoundTrip(t *testing.T) {
	_, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}

	// Workspace + approver + worker keys.
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"github.com/fortix/freedom3","name":"freedom3"}`), 201)
	approverKey := mint(t, root, `{"name":"relay","workspaces":["github.com/fortix/freedom3"],"approver":true}`)
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["github.com/fortix/freedom3"]}`)
	approver := workerClient{base: baseURL, key: approverKey}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: workerKey, lease: leases}

	// The item enters the workflow.
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"github.com/fortix/freedom3","title":"stub item","author_agent_id":"martin"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)

	// The plan run was queued by the hook.
	run := claim(t, worker, `{"worker_id":"w1","workspaces":["github.com/fortix/freedom3"]}`)
	if run["stage"] != "plan" {
		t.Fatalf("first claim should be plan, got %v", run["stage"])
	}
	runID := run["id"].(string)

	// The worker plans: create the plan + revision + link (the real worker
	// does this from the agent's output).
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"github.com/fortix/freedom3","name":"stub plan","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"do the thing"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	snap := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"d2ce73d"}`), 201).(map[string]any)
	revID := snap["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/heartbeat", `{"worker_id":"w1"}`, leases[runID]), 204)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"succeeded","summary":"plan ready","branch":"agent/stub-item"}`, leases[runID]), 200)

	// Chaining moved the item to the gate.
	got := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	if got["status"] != "awaiting_approval" {
		t.Fatalf("after plan: %v", got["status"])
	}

	// Approve (approver key) queues the implement run. An approval must
	// name the revision reviewed (M2): a bare approve is refused.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{}`), 400)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+revID+`"}`), 200)
	run = claim(t, worker, `{"worker_id":"w1","workspaces":["github.com/fortix/freedom3"]}`)
	if run["stage"] != "implement" {
		t.Fatalf("second claim should be implement, got %v", run["stage"])
	}
	runID = run["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"succeeded","summary":"done","head_sha":"abc123","branch":"agent/stub-item"}`, leases[runID]), 200)

	// No review preference configured: straight to in_review.
	got = mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	if got["status"] != "in_review" {
		t.Fatalf("after implement: %v", got["status"])
	}

	// Mark done needs the head sha; cleanup cancels leftover runs.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/mark-done", `{"head_sha":"abc123"}`), 200)
	listedRaw := root.req("GET", "/api/runs?item_id="+itemID, "")
	mustDo(t, listedRaw, 200, "list runs")
	var listed []map[string]any
	if err := json.Unmarshal([]byte(listedRaw.body), &listed); err != nil {
		t.Fatalf("runs list json: %v", err)
	}
	for _, m := range listed {
		if m["status"] == "queued" || m["status"] == "claimed" {
			t.Fatalf("active run left after done: %v", m)
		}
	}
}

func claim(t *testing.T, c workerClient, body string) map[string]any {
	t.Helper()
	resp := c.req("POST", "/api/runs/claim", body)
	if resp.code == 204 {
		t.Fatalf("claim returned 204 (nothing queued)")
	}
	if resp.code != 200 {
		t.Fatalf("claim: %d %s", resp.code, resp.body)
	}
	m := jsonMap(t, resp.body)
	if c.lease != nil {
		if id, ok := m["id"].(string); ok {
			if tok, ok := m["lease_token"].(string); ok {
				c.lease[id] = tok
			}
		}
	}
	return m
}

func jsonMapOrNil(s string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func mint(t *testing.T, root workerClient, body string) string {
	t.Helper()
	resp := mustJSON(t, root.req("POST", "/api/keys", body), 201).(map[string]any)
	return resp["key_secret"].(string)
}

func jsonMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad json: %v: %s", err, s)
	}
	return m
}

type workerClient struct {
	base, key string
	lease     map[string]string // run id -> fencing token
}

func (c workerClient) req(method, path, body string) *stubResp {
	return c.reqH(method, path, body, "")
}

func (c workerClient) reqH(method, path, body, leaseToken string) *stubResp {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if leaseToken != "" {
		req.Header.Set("X-Skopos-Lease", leaseToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &stubResp{code: 0, body: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if r := jsonMapOrNil(string(b)); r != nil {
		if id, ok := r["id"].(string); ok {
			if tok, ok := r["lease_token"].(string); ok && tok != "" && c.lease != nil {
				c.lease[id] = tok
			}
		}
	}
	return &stubResp{code: resp.StatusCode, body: string(b)}
}

type stubResp struct {
	code int
	body string
}

func mustDo(t *testing.T, r *stubResp, want int, what ...string) *stubResp {
	t.Helper()
	if r.code != want {
		t.Fatalf("%s: expected %d got %d: %s", strings.Join(what, " "), want, r.code, r.body)
	}
	return r
}

func mustJSON(t *testing.T, r *stubResp, want int) any {
	t.Helper()
	mustDo(t, r, want)
	return jsonMap(t, r.body)
}

// workerStack builds a serve-shaped stack with the runs queue wired to the
// inbox hooks (queue->plan run, approve->implement run) — the same wiring
// serve.go installs.
func workerStack(t *testing.T) (*sql.DB, string) {
	t.Helper()
	return workerStackWith(t, nil)
}

// workerStackWith lets a test configure the run queue (routing) before the
// server starts.
func workerStackWith(t *testing.T, configure func(*runs.Service)) (*sql.DB, string) {
	t.Helper()
	log := logslog.New(logslog.Config{Level: "error", Writer: io.Discard})
	sqlDB, err := db.Connect(log, filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatal(err)
	}
	rest.SetLogger(log)

	authn := auth.NewAuthenticator("root-secret", apikeys.NewStorage(sqlDB))
	auditService := audit.NewService(audit.NewStorage(sqlDB))
	approvalsService := approvals.NewService(approvals.NewStorage(sqlDB))
	plansService := plans.NewService(plans.NewStorage(sqlDB))
	inboxService := inbox.NewService(inbox.NewStorage(sqlDB))
	inboxService.SetAuditRecorder(auditService)
	inboxService.SetApprovalLog(approvalsService)
	inboxService.SetPlanRevisions(plansService)
	runsService := runs.NewService(runs.NewStorage(sqlDB))
	runsService.SetItemTransition(func(ctx context.Context, itemID, to, reason, agentID string) error {
		_, err := inboxService.SystemTransition(ctx, itemID, inbox.Status(to), reason, agentID, audit.ViaWorker)
		return err
	})
	runsService.SetItemAppend(func(ctx context.Context, itemID, heading, body string) error {
		return inboxService.AppendSection(ctx, itemID, heading, body, audit.ViaWorker)
	})
	inboxService.SetRunQueuer(runsService)
	// Plan-group hooks, as serve.go wires them.
	runsService.SetPlanHooks(func(ctx context.Context, itemID string) error {
		item, err := inboxService.GetItem(ctx, itemID)
		if err != nil || item.PlanID == "" {
			return nil
		}
		return plansService.SupersedeOpenCandidates(ctx, item.PlanID)
	}, func(ctx context.Context, itemID string) error {
		item, err := inboxService.GetItem(ctx, itemID)
		if err != nil || item.PlanID != "" {
			return err
		}
		plan, err := plansService.CreatePlan(ctx, plans.CreatePlanInput{WorkspaceID: item.WorkspaceID, Name: item.Title, AuthorAgentID: "skopos"})
		if err != nil {
			return err
		}
		_, err = inboxService.LinkPlan(ctx, itemID, plan.ID)
		return err
	})
	// Pipeline settings, as serve.go wires them (a test's configure may
	// still pin routing with SetPrefs, which replaces this source).
	pipelineService := pipeline.NewService(pipeline.NewStorage(sqlDB))
	pipelineService.SetAuditSink(auditService)
	runsService.SetPrefsSource(func(ctx context.Context, ws string) *runs.WorkspacePrefs {
		return pipelineService.Prefs(ctx, ws)
	})
	if configure != nil {
		configure(runsService)
	}

	st := status.NewHandler(status.NewService(status.NewStorage(sqlDB)), authn)
	bb := blackboard.NewHandler(blackboard.NewService(blackboard.NewStorage(sqlDB)), authn)
	pl := plans.NewHandler(plansService, authn)
	ib := inbox.NewHandler(inboxService, authn)
	ws := workspaces.NewHandler(workspaces.NewService(workspaces.NewStorage(sqlDB)), authn)
	ak := apikeys.NewHandler(apikeys.NewService(apikeys.NewStorage(sqlDB)), workspaces.NewService(workspaces.NewStorage(sqlDB)))
	rh := runs.NewHandler(runsService)

	webMux := http.NewServeMux()
	apiMux := http.NewServeMux()
	RegisterRoutes(webMux, apiMux, st, bb, pl, ib, ws, nil, ak, rh)
	RegisterPipelineRoutes(apiMux, pipeline.NewHandler(pipelineService, authn))
	root := http.NewServeMux()
	root.Handle("/api/", authn.Middleware(apiMux))
	root.Handle("/", webMux)
	ts := httptest.NewServer(root)
	t.Cleanup(ts.Close)
	return sqlDB, ts.URL
}

// Capability profiles over REST: the worker's replan carries the selected
// skills, the revision exposes them (and the approval still verifies), a
// malformed skill name is refused, and the run report records its executor.
func TestIntegrationReplanSkillsAndExecutor(t *testing.T) {
	_, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"github.com/fortix/freedom3","name":"freedom3"}`), 201)
	approverKey := mint(t, root, `{"name":"relay","workspaces":["github.com/fortix/freedom3"],"approver":true}`)
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["github.com/fortix/freedom3"]}`)
	approver := workerClient{base: baseURL, key: approverKey}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: workerKey, lease: leases}

	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"github.com/fortix/freedom3","title":"skills item","author_agent_id":"martin"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)
	run := claim(t, worker, `{"worker_id":"w1","workspaces":["github.com/fortix/freedom3"]}`)
	runID := run["id"].(string)

	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"github.com/fortix/freedom3","name":"p","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/replan", `{"base_sha":"d2ce73d","steps":[{"title":"one"}],"skills":["../etc"]}`), 400)
	rev := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/replan", `{"base_sha":"d2ce73d","steps":[{"title":"one"}],"skills":["php-dto","e2e"]}`), 200).(map[string]any)
	if sk, _ := rev["skills"].([]any); len(sk) != 2 || sk[0] != "e2e" || sk[1] != "php-dto" {
		t.Fatalf("replan must bind the skills into the revision: %v", rev["skills"])
	}
	var revs []map[string]any
	if err := json.Unmarshal([]byte(mustDo(t, worker.req("GET", "/api/plans/"+planID+"/revisions", ""), 200).body), &revs); err != nil || len(revs) == 0 {
		t.Fatalf("revisions listing: %v", err)
	}
	if sk, _ := revs[0]["skills"].([]any); len(sk) != 2 {
		t.Fatalf("revisions listing must expose the skills: %v", revs[0])
	}
	report := mustJSON(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"succeeded","summary":"plan ready","executor":"sandbox:0123456789ab"}`, leases[runID]), 200).(map[string]any)
	if report["executor"] != "sandbox:0123456789ab" {
		t.Fatalf("the report must record the executor: %v", report["executor"])
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+rev["id"].(string)+`"}`), 200)
}

// Multi-agent review over REST: two reviewers (each pinned to its runner)
// report structured findings and cost; the group decides once, blocks the
// item with one merged summary, and the groups endpoint shows it all.
func TestIntegrationReviewGroupRoundTrip(t *testing.T) {
	const ws = "github.com/fortix/freedom3"
	_, baseURL := workerStackWith(t, func(r *runs.Service) {
		r.SetPrefs(map[string]*runs.WorkspacePrefs{ws: {
			Stages: map[string]string{},
			Groups: map[string][]runs.Member{"review": {{Runner: "claude", Focus: "security"}, {Runner: "glm", Focus: "tests"}}},
		}})
	})
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"freedom3"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"relay","workspaces":["`+ws+`"],"approver":true}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}

	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"group item","author_agent_id":"martin"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)
	run := claim(t, worker, `{"worker_id":"w1","runners":["claude"],"workspaces":["`+ws+`"]}`)
	runID := run["id"].(string)
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"p","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	revID := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"d2ce73d"}`), 201).(map[string]any)["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"succeeded","summary":"plan ready","branch":"agent/g","cost_usd":0.12}`, leases[runID]), 200)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+revID+`"}`), 200)
	run = claim(t, worker, `{"worker_id":"w1","runners":["claude"],"workspaces":["`+ws+`"]}`)
	runID = run["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"succeeded","summary":"done","head_sha":"abc123","branch":"agent/g"}`, leases[runID]), 200)

	// The review group: each worker gets exactly its own member.
	sec := claim(t, worker, `{"worker_id":"w-claude","runners":["claude"],"workspaces":["`+ws+`"]}`)
	tst := claim(t, worker, `{"worker_id":"w-glm","runners":["glm"],"workspaces":["`+ws+`"]}`)
	if sec["focus"] != "security" || tst["focus"] != "tests" || sec["group_id"] == nil || sec["group_id"] != tst["group_id"] {
		t.Fatalf("members: %v / %v", sec, tst)
	}
	mustDo(t, worker.req("POST", "/api/runs/claim", `{"worker_id":"w-x","runners":["claude","glm"],"workspaces":["`+ws+`"]}`), 204, "no third member")
	secID, tstID := sec["id"].(string), tst["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+secID+"/report", `{"worker_id":"w-claude","status":"failed","reason":"review failed: injection","summary":"injection","cost_usd":0.31,"findings":[{"severity":"blocking","text":"Query built by concatenation","file":"db.go:12"}]}`, leases[secID]), 200)
	got := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	if got["status"] != "implementing" {
		t.Fatalf("one member must not decide the group: %v", got["status"])
	}
	mustDo(t, worker.reqH("POST", "/api/runs/"+tstID+"/report", `{"worker_id":"w-glm","status":"succeeded","summary":"tests ok","findings":[{"severity":"nit","text":"table-driven please"}]}`, leases[tstID]), 200)

	got = mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	content, _ := got["content"].(string)
	if got["status"] != "blocked" || !strings.Contains(content, "## Review summary — round 1") || !strings.Contains(content, "`db.go:12` Query built by concatenation — _security · claude_") || !strings.Contains(content, "### Nits (not blocking)") {
		t.Fatalf("after the group: status %v\n%s", got["status"], content)
	}
	groupsResp := mustDo(t, root.req("GET", "/api/runs/groups?item_id="+itemID, ""), 200)
	var groups []map[string]any
	if err := json.Unmarshal([]byte(groupsResp.body), &groups); err != nil || len(groups) != 1 {
		t.Fatalf("groups: %v %s", err, groupsResp.body)
	}
	g := groups[0]
	members := g["members"].([]any)
	if g["outcome"] != "fail" || g["round"].(float64) != 1 || len(members) != 2 {
		t.Fatalf("group view: %v", g)
	}
	m0 := members[0].(map[string]any)
	if m0["cost_usd"].(float64) != 0.31 || len(m0["findings"].([]any)) != 1 {
		t.Fatalf("member carries cost and findings: %v", m0)
	}
}

// Multi-agent planning over REST: two planners write candidates at the
// group's pinned base without touching the live plan; the gate opens with
// both; approving candidate 2 makes it the plan (same step ids, so the
// approval binds the candidate itself), supersedes candidate 1, and the
// implement run is bound to candidate 2 and its base.
func TestIntegrationPlanCandidatesRoundTrip(t *testing.T) {
	const ws = "github.com/fortix/freedom3"
	_, baseURL := workerStackWith(t, func(r *runs.Service) {
		r.SetPrefs(map[string]*runs.WorkspacePrefs{ws: {
			Stages: map[string]string{"implement": "claude"},
			Groups: map[string][]runs.Member{"plan": {{Runner: "claude"}, {Runner: "glm"}}},
		}})
	})
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"freedom3"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"relay","workspaces":["`+ws+`"],"approver":true}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}

	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"three ways","author_agent_id":"martin"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)

	a := claim(t, worker, `{"worker_id":"w-claude","runners":["claude"],"workspaces":["`+ws+`"]}`)
	b := claim(t, worker, `{"worker_id":"w-glm","runners":["glm"],"workspaces":["`+ws+`"]}`)
	if a["stage"] != "plan" || b["stage"] != "plan" || a["group_id"] == nil || a["group_id"] != b["group_id"] {
		t.Fatalf("plan members: %v / %v", a, b)
	}
	got := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	planID, _ := got["plan_id"].(string)
	if planID == "" || got["status"] != "planning" {
		t.Fatalf("claiming a member must link the plan: %v", got)
	}
	aID, bID := a["id"].(string), b["id"].(string)
	// The first member pins the base; the second gets it back.
	pin := mustJSON(t, worker.reqH("POST", "/api/runs/"+aID+"/pin-base", `{"base_sha":"aaaa111"}`, leases[aID]), 200).(map[string]any)
	pin2 := mustJSON(t, worker.reqH("POST", "/api/runs/"+bID+"/pin-base", `{"base_sha":"bbbb222"}`, leases[bID]), 200).(map[string]any)
	if pin["base_sha"] != "aaaa111" || pin2["base_sha"] != "aaaa111" {
		t.Fatalf("pinned base: %v / %v", pin, pin2)
	}
	c1 := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/candidates", `{"base_sha":"aaaa111","group_id":"`+a["group_id"].(string)+`","label":"claude","steps":[{"title":"small fix"}]}`), 201).(map[string]any)
	c2 := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/candidates", `{"base_sha":"aaaa111","group_id":"`+a["group_id"].(string)+`","label":"glm","steps":[{"title":"extract module","description":"move x"},{"title":"add tests"}],"skills":["go-tests"]}`), 201).(map[string]any)
	planNow := mustJSON(t, root.req("GET", "/api/plans/"+planID, ""), 200).(map[string]any)
	if items, _ := planNow["items"].([]any); len(items) != 0 {
		t.Fatalf("candidates must not touch the live plan: %v", items)
	}
	mustDo(t, worker.reqH("POST", "/api/runs/"+aID+"/report", `{"worker_id":"w-claude","status":"succeeded","summary":"candidate ready"}`, leases[aID]), 200)
	if got = mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any); got["status"] != "planning" {
		t.Fatalf("one planner must not open the gate: %v", got["status"])
	}
	mustDo(t, worker.reqH("POST", "/api/runs/"+bID+"/report", `{"worker_id":"w-glm","status":"succeeded","summary":"candidate ready"}`, leases[bID]), 200)
	got = mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	if got["status"] != "awaiting_approval" || !strings.Contains(got["content"].(string), "## Plan candidates — round 1") {
		t.Fatalf("gate after both planners: %v\n%v", got["status"], got["content"])
	}
	// A bare (latest-style) approve can't pick a candidate.
	if r := approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"nope"}`); r.code != 400 || !strings.Contains(r.body, "several candidates") {
		t.Fatalf("approve without a candidate: %d %s", r.code, r.body)
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+c2["id"].(string)+`"}`), 200)

	revsResp := mustDo(t, root.req("GET", "/api/plans/"+planID+"/revisions", ""), 200)
	var revs []map[string]any
	if err := json.Unmarshal([]byte(revsResp.body), &revs); err != nil {
		t.Fatal(err)
	}
	states := map[string]any{}
	for _, r := range revs {
		states[r["label"].(string)] = r["candidate_state"]
	}
	if states["glm"] != "approved" || states["claude"] != "superseded" {
		t.Fatalf("candidate states: %v", states)
	}
	planNow = mustJSON(t, root.req("GET", "/api/plans/"+planID, ""), 200).(map[string]any)
	items := planNow["items"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["title"] != "extract module" {
		t.Fatalf("approved candidate must become the plan (2 steps + review): %v", items)
	}
	impl := claim(t, worker, `{"worker_id":"w-claude","runners":["claude"],"workspaces":["`+ws+`"]}`)
	if impl["stage"] != "implement" || impl["plan_rev_id"] != c2["id"] || impl["base_sha"] != "aaaa111" {
		t.Fatalf("implement must bind candidate 2 and its base: %v", impl)
	}
	_ = c1
}

// At a candidates gate, request-changes either revises ONE candidate (a
// single plan run pinned to that candidate's runner, seeded with it) or,
// without a candidate, re-runs every planner as round 2. Either way the
// round's open candidates are superseded, so an old candidate can't be
// approved later.
func TestIntegrationPlanCandidatesRequestChanges(t *testing.T) {
	const ws = "github.com/fortix/freedom3"
	_, baseURL := workerStackWith(t, func(r *runs.Service) {
		r.SetPrefs(map[string]*runs.WorkspacePrefs{ws: {
			Stages: map[string]string{},
			Groups: map[string][]runs.Member{"plan": {{Runner: "claude"}, {Runner: "glm"}}},
		}})
	})
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"freedom3"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"relay","workspaces":["`+ws+`"],"approver":true}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}

	// Round 1 with two candidates, gate open.
	round := func(itemID string) (planID string, cands []map[string]any) {
		a := claim(t, worker, `{"worker_id":"w-claude","runners":["claude"],"workspaces":["`+ws+`"]}`)
		b := claim(t, worker, `{"worker_id":"w-glm","runners":["glm"],"workspaces":["`+ws+`"]}`)
		got := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
		planID = got["plan_id"].(string)
		for _, m := range []map[string]any{a, b} {
			id := m["id"].(string)
			c := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/candidates", `{"base_sha":"abc","group_id":"`+m["group_id"].(string)+`","label":"`+m["runner"].(string)+`","steps":[{"title":"s"}]}`), 201).(map[string]any)
			cands = append(cands, c)
			mustDo(t, worker.reqH("POST", "/api/runs/"+id+"/report", `{"worker_id":"`+m["worker_id"].(string)+`","status":"succeeded","summary":"ok"}`, leases[id]), 200)
		}
		return planID, cands
	}
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"pick one","author_agent_id":"martin"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)
	_, cands := round(itemID)

	// Revise candidate 2 (glm): one run, pinned to glm, seeded with it.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/request-changes", `{"notes":"split step 1","revision_id":"`+cands[1]["id"].(string)+`"}`), 200)
	mustDo(t, worker.req("POST", "/api/runs/claim", `{"worker_id":"w-claude","runners":["claude"],"workspaces":["`+ws+`"]}`), 204, "a revise is pinned to the candidate's runner")
	rev := claim(t, worker, `{"worker_id":"w-glm","runners":["glm"],"workspaces":["`+ws+`"]}`)
	if rev["stage"] != "plan" || rev["group_id"] != nil || rev["plan_rev_id"] != cands[1]["id"] || rev["runner"] != "glm" {
		t.Fatalf("seeded revise run: %v", rev)
	}
	// The revise writes an ordinary plan; at the reopened gate, round 1's
	// candidates are closed (stale) and the revised plan is approvable.
	planID := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)["plan_id"].(string)
	revised := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/replan", `{"base_sha":"abc","steps":[{"title":"s1a"},{"title":"s1b"}]}`), 200).(map[string]any)
	revID := rev["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+revID+"/report", `{"worker_id":"w-glm","status":"succeeded","summary":"revised"}`, leases[revID]), 200)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+cands[0]["id"].(string)+`"}`), 409)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+revised["id"].(string)+`"}`), 200)
	if impl := claim(t, worker, `{"worker_id":"w-drain","runners":["claude","glm"],"workspaces":["`+ws+`"]}`); impl["stage"] != "implement" || impl["plan_rev_id"] != revised["id"] {
		t.Fatalf("the revised plan's implement run: %v", impl)
	}

	// Re-plan everything on a second item: round 2 opens with both members.
	item2 := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"again","author_agent_id":"martin"}`), 201).(map[string]any)
	item2ID := item2["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+item2ID+"/queue", ""), 200)
	_, c2 := round(item2ID)
	mustDo(t, approver.req("POST", "/api/inbox/"+item2ID+"/request-changes", `{"notes":"none of these"}`), 200)
	groupsResp := mustDo(t, root.req("GET", "/api/runs/groups?item_id="+item2ID, ""), 200)
	var groups []map[string]any
	if err := json.Unmarshal([]byte(groupsResp.body), &groups); err != nil || len(groups) != 2 || groups[0]["round"].(float64) != 2 {
		t.Fatalf("re-plan must open round 2: %v %s", err, groupsResp.body)
	}
	_, r2 := round(item2ID)
	mustDo(t, approver.req("POST", "/api/inbox/"+item2ID+"/approve", `{"revision_id":"`+c2[1]["id"].(string)+`"}`), 409)
	mustDo(t, approver.req("POST", "/api/inbox/"+item2ID+"/approve", `{"revision_id":"`+r2[0]["id"].(string)+`"}`), 200)
}
