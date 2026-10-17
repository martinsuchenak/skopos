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
	defer resp.Body.Close()
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
	log := logslog.New(logslog.Config{Level: "error", Writer: io.Discard})
	sqlDB, err := db.Connect(log, filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
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
	inboxService.SetRunQueuer(runsService)

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
