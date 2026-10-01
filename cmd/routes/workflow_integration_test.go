package routes

import (
	"bytes"
	"context"
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
	"github.com/martinsuchenak/skopos/internal/events"
	"github.com/martinsuchenak/skopos/internal/inbox"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/rest"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/martinsuchenak/skopos/internal/workspaces"
	logslog "github.com/paularlott/logger/slog"
	_ "modernc.org/sqlite"
)

// workflowIntegrationSetup mirrors serve.go's 1a wiring: the inbox workflow
// with its audit sink, approvals, and plan revisions, plus the groups scope
// notifier and the workspace-registration trigger.
func workflowIntegrationSetup(t *testing.T) *httptest.Server {
	t.Helper()
	log := logslog.New(logslog.Config{Level: "error", Writer: io.Discard})
	sqlDB, err := db.Connect(log, filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rest.SetLogger(log)

	hub := events.NewHub()
	authn := auth.NewAuthenticator("root-secret", apikeys.NewStorage(sqlDB))

	auditService := audit.NewService(audit.NewStorage(sqlDB))
	approvalsService := approvals.NewService(approvals.NewStorage(sqlDB))
	plansService := plans.NewService(plans.NewStorage(sqlDB))
	inboxService := inbox.NewService(inbox.NewStorage(sqlDB))
	inboxService.SetAuditRecorder(auditService)
	inboxService.SetApprovalLog(approvalsService)
	inboxService.SetPlanRevisions(plansService)

	apiKeysService := apikeys.NewService(apikeys.NewStorage(sqlDB))
	apiKeysHandler := apikeys.NewHandler(apiKeysService, workspaces.NewService(workspaces.NewStorage(sqlDB)))
	apiKeysService.SetRevocationNotifier(hub.DropKey)
	apiKeysService.SetScopeNotifier(func(keyIDs []string) {
		for _, id := range keyIDs {
			hub.DropKey(id)
		}
	})

	workspacesService := workspaces.NewService(workspaces.NewStorage(sqlDB))
	workspacesService.SetCreateNotifier(func(wsID string) {
		apiKeysService.OnWorkspaceRegistered(context.Background(), wsID)
	})

	st := status.NewHandler(status.NewService(status.NewStorage(sqlDB)), authn)
	bb := blackboard.NewHandler(blackboard.NewService(blackboard.NewStorage(sqlDB)), authn)
	pl := plans.NewHandler(plansService, authn)
	ib := inbox.NewHandler(inboxService, authn)
	ws := workspaces.NewHandler(workspacesService, authn)

	webMux := http.NewServeMux()
	apiMux := http.NewServeMux()
	RegisterRoutes(webMux, apiMux, st, bb, pl, ib, ws, nil, apiKeysHandler)
	root := http.NewServeMux()
	root.Handle("/api/", authn.Middleware(apiMux))
	root.Handle("/", webMux)
	root.Handle("GET /api/events/stream", authn.Middleware(events.StreamHandler(hub)))
	ts := httptest.NewServer(root)
	t.Cleanup(ts.Close)
	return ts
}

// The 1a round trip over REST with real Bearer keys: root mints an approver
// key and a worker key, the worker drives the machine phases, the approver
// works the gates, and the plan revision locks on approve.
func TestIntegrationWorkflowRoundTrip(t *testing.T) {
	ts := workflowIntegrationSetup(t)
	root := bearerClient{ts, "root-secret"}

	// Registry + group + keys.
	mustStatus(t, root.do("POST", "/api/workspaces", `{"id":"github.com/fortix/freedom3","name":"freedom3"}`), 201, "register workspace")
	mustStatus(t, root.do("POST", "/api/groups", `{"name":"work","patterns":["github.com/fortix/*"]}`), 201, "create group")

	approverSecret := mintKey(t, root, `{"name":"relay","workspaces":[],"groups":["work"],"approver":true}`)
	workerSecret := mintKey(t, root, `{"name":"worker","workspaces":[],"groups":["work"]}`)
	approver := bearerClient{ts, approverSecret}
	worker := bearerClient{ts, workerSecret}

	// Capture the item (root), then the workflow.
	itemID := createItem(t, root)

	// The worker (plain scoped key) cannot queue: approver gate end to end.
	if w := worker.do("POST", "/api/inbox/"+itemID+"/queue", ""); w.StatusCode != 403 {
		t.Fatalf("worker queue must be 403, got %d %s", w.StatusCode, w.Body.String())
	}
	mustStatus(t, approver.do("POST", "/api/inbox/"+itemID+"/queue", ""), 200, "queue")

	// Planner: worker creates the plan, links it, snapshots revision 1.
	planBody := `{"workspace_id":"github.com/fortix/freedom3","name":"ship it","author_agent_id":"planner"}`
	planResp := mustStatus(t, worker.do("POST", "/api/plans", planBody), 201, "create plan")
	var plan struct {
		ID string `json:"id"`
	}
	json.Unmarshal(planResp.Body.Bytes(), &plan)
	mustStatus(t, worker.do("POST", "/api/plans/"+plan.ID+"/items", `{"title":"step 1"}`), 201, "add step")
	mustStatus(t, worker.do("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+plan.ID+`"}`), 200, "link plan")
	revResp := mustStatus(t, worker.do("POST", "/api/plans/"+plan.ID+"/revisions", `{"base_sha":"d2ce73dfdd1"}`), 201, "snapshot revision")
	var rev struct {
		ID string `json:"id"`
	}
	json.Unmarshal(revResp.Body.Bytes(), &rev)

	mustStatus(t, worker.do("POST", "/api/inbox/"+itemID+"/transition", `{"to":"planning","via":"worker"}`), 200, "planning")
	mustStatus(t, worker.do("POST", "/api/inbox/"+itemID+"/transition", `{"to":"awaiting_approval","via":"worker"}`), 200, "awaiting approval")

	// Approve with overrides: revision locks, approval records.
	mustStatus(t, approver.do("POST", "/api/inbox/"+itemID+"/approve", `{"notes":"wr=108900"}`), 200, "approve")

	// The plan is locked content now: the worker cannot add steps.
	if w := worker.do("POST", "/api/plans/"+plan.ID+"/items", `{"title":"sneak"}`); w.StatusCode != 409 || !strings.Contains(w.Body.String(), "locked") {
		t.Fatalf("add to locked plan must be 409 locked, got %d %s", w.StatusCode, w.Body.String())
	}

	mustStatus(t, worker.do("POST", "/api/inbox/"+itemID+"/transition", `{"to":"implementing","via":"worker"}`), 200, "implementing")
	mustStatus(t, worker.do("POST", "/api/inbox/"+itemID+"/transition", `{"to":"in_review","via":"worker"}`), 200, "in review")
	mustStatus(t, approver.do("POST", "/api/inbox/"+itemID+"/mark-done", `{"head_sha":"abc123"}`), 200, "mark done")

	// Timeline and approvals tell the whole story.
	tl := mustStatus(t, approver.do("GET", "/api/inbox/"+itemID+"/timeline", ""), 200, "timeline")
	if !strings.Contains(tl.Body.String(), "inbox.queue") || !strings.Contains(tl.Body.String(), "inbox.approve") {
		t.Fatalf("timeline incomplete: %s", tl.Body.String())
	}
	ap := mustStatus(t, root.do("GET", "/api/inbox/"+itemID+"/approvals", ""), 200, "approvals")
	body := ap.Body.String()
	if !strings.Contains(body, `"gate":"plan"`) || !strings.Contains(body, rev.ID+"+d2ce73dfdd1") {
		t.Fatalf("plan approval missing or wrong subject: %s", body)
	}
	if !strings.Contains(body, `"gate":"review"`) || !strings.Contains(body, "abc123") {
		t.Fatalf("review approval missing or wrong subject: %s", body)
	}
}

func createItem(t *testing.T, c bearerClient) string {
	t.Helper()
	resp := mustStatus(t, c.do("POST", "/api/inbox", `{"workspace_id":"github.com/fortix/freedom3","title":"pipeline item","author_agent_id":"martin"}`), 201, "create item")
	var item struct {
		ID string `json:"id"`
	}
	json.Unmarshal(resp.Body.Bytes(), &item)
	return item.ID
}

func mintKey(t *testing.T, root bearerClient, body string) string {
	t.Helper()
	resp := mustStatus(t, root.do("POST", "/api/keys", body), 201, "mint key")
	var created struct {
		Secret string `json:"key_secret"`
	}
	json.Unmarshal(resp.Body.Bytes(), &created)
	return created.Secret
}

type bearerClient struct {
	ts     *httptest.Server
	secret string
}

func (c bearerClient) do(method, path, body string) *httpResponse {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, c.ts.URL+path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &httpResponse{StatusCode: 0, Body: bytes.NewBufferString(err.Error())}
	}
	defer resp.Body.Close()
	buf := &bytes.Buffer{}
	io.Copy(buf, resp.Body)
	return &httpResponse{StatusCode: resp.StatusCode, Body: buf}
}

type httpResponse struct {
	StatusCode int
	Body       *bytes.Buffer
}

func (r *httpResponse) String() string { return r.Body.String() }

func mustStatus(t *testing.T, r *httpResponse, want int, what string) *httpResponse {
	t.Helper()
	if r.StatusCode != want {
		t.Fatalf("%s: expected %d, got %d: %s", what, want, r.StatusCode, r.Body.String())
	}
	return r
}

// Migration dry-run over REST on a seeded trial-tagged item set.
func TestIntegrationMigrationDryRun(t *testing.T) {
	ts := workflowIntegrationSetup(t)
	root := bearerClient{ts, "root-secret"}
	mustStatus(t, root.do("POST", "/api/workspaces", `{"id":"ws-a","name":"a"}`), 201, "workspace")

	ready := createItem(t, root)
	_ = ready
	// Tag it like the trial would.
	mustStatus(t, root.do("PATCH", "/api/inbox/"+ready, `{"tags":["ready"]}`), 204, "tag ready")

	dry := mustStatus(t, root.do("POST", "/api/inbox/migrate-workflow", `{"dry_run":true}`), 200, "dry run")
	if !strings.Contains(dry.Body.String(), `"ready"`) {
		t.Fatalf("dry run must report the ready item: %s", dry.Body.String())
	}
	// Nothing written.
	got := mustStatus(t, root.do("GET", "/api/inbox/"+ready, ""), 200, "get item")
	if !strings.Contains(got.Body.String(), `"status":"open"`) {
		t.Fatalf("dry run must not write: %s", got.Body.String())
	}
}
