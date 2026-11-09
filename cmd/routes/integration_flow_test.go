package routes

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// hReq is a request with extra headers; it returns the code, body and
// response headers.
func hReq(t *testing.T, base, key, method, path, body string, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// Another system's flow over REST (tmp/integrations — design.md): an
// integration key creates and starts an item once (a retry with the same
// Idempotency-Key gets the same item), finds it by its job's reference,
// follows it through the state endpoint, and approves the plan on behalf
// of a named person.
func TestIntegrationExternalSystemFlow(t *testing.T) {
	sqlDB, base := workerStack(t)
	root := workerClient{base: base, key: "root-secret"}
	const ws = "github.com/fortix/freedom3"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"freedom3"}`), 201)
	fortix := mint(t, root, `{"name":"fortix","workspaces":["`+ws+`"],"preset":"integration","approver":true,"delegate":true}`)
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`)
	leases := map[string]string{}
	worker := workerClient{base: base, key: workerKey, lease: leases}

	create := `{"workspace_id":"` + ws + `","title":"WR110375 AI configurations","content":"the job","author_agent_id":"fortix",` +
		`"ref":"WR110375","external_url":"https://fortix.example/wr/110375","start":"queue"}`
	idem := map[string]string{"Idempotency-Key": "wr-110375-create"}
	code, body, h := hReq(t, base, fortix, "POST", "/api/inbox", create, idem)
	if code != 201 || h.Get("Idempotent-Replayed") != "" {
		t.Fatalf("create: %d %s", code, body)
	}
	item := jsonMap(t, body)
	itemID := item["id"].(string)
	if item["status"] != "queued" || item["external_url"] != "https://fortix.example/wr/110375" {
		t.Fatalf("created: %v", item)
	}
	code, body, h = hReq(t, base, fortix, "POST", "/api/inbox", create, idem)
	if code != 201 || h.Get("Idempotent-Replayed") != "true" || jsonMap(t, body)["id"] != itemID {
		t.Fatalf("retry: %d %v %s", code, h, body)
	}
	code, _, _ = hReq(t, base, fortix, "POST", "/api/inbox", strings.Replace(create, "the job", "another job", 1), idem)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("reused key, other body: %d", code)
	}

	// Found by ref: exactly the one item.
	code, body, _ = hReq(t, base, fortix, "GET", "/api/inbox?ref=WR110375", "", nil)
	if code != 200 || strings.Count(body, `"id":`) != 1 || !strings.Contains(body, itemID) {
		t.Fatalf("by ref: %d %s", code, body)
	}

	// The agent's turn: only reject is open to a person.
	st := mustJSON(t, workerClient{base: base, key: fortix}.req("GET", "/api/inbox/"+itemID+"/state", ""), 200).(map[string]any)
	if st["turn"] != "agent" || st["brief"] != "the job" || strings.Join(toStrings(st["actions"]), ",") != "reject" {
		t.Fatalf("queued state: %v", st)
	}

	// The worker plans.
	run := claim(t, worker, `{"worker_id":"w1","workspaces":["`+ws+`"]}`)
	runID := run["id"].(string)
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"plan","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step one"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"d2ce73d"}`), 201)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"succeeded","summary":"plan ready","branch":"agent/x"}`, leases[runID]), 200)

	// At the gate: the plan to approve, with its hash, and the actions.
	st = mustJSON(t, workerClient{base: base, key: fortix}.req("GET", "/api/inbox/"+itemID+"/state", ""), 200).(map[string]any)
	if st["turn"] != "person" || st["gate"] != "plan_approval" {
		t.Fatalf("gate state: %v", st)
	}
	if got := strings.Join(toStrings(st["actions"]), ","); got != "approve,request-changes,answers,reject" {
		t.Fatalf("gate actions: %s", got)
	}
	revs := st["plan"].(map[string]any)["revisions"].([]any)
	rev := revs[0].(map[string]any)
	steps := rev["steps"].([]any)
	if len(revs) != 1 || len(steps) != 1 || steps[0].(map[string]any)["title"] != "step one" {
		t.Fatalf("plan: %v", st["plan"])
	}

	// Approved on behalf of a person: the audit and the approval name them.
	person := map[string]string{"X-Skopos-On-Behalf-Of": "martin@fortix.example", "Idempotency-Key": "wr-110375-approve-1"}
	approve := `{"revision_id":"` + rev["revision_id"].(string) + `","content_hash":"` + rev["content_hash"].(string) + `"}`
	if code, body, _ := hReq(t, base, fortix, "POST", "/api/inbox/"+itemID+"/approve", approve, person); code != 200 {
		t.Fatalf("approve: %d %s", code, body)
	}
	var actor, onBehalf, via string
	if err := sqlDB.QueryRow(`SELECT actor, on_behalf_of, via FROM audit_log WHERE entity_id = ? AND action = 'inbox.approve'`, itemID).Scan(&actor, &onBehalf, &via); err != nil {
		t.Fatal(err)
	}
	if actor != "martin@fortix.example (key fortix)" || onBehalf != "martin@fortix.example" || via != "api" {
		t.Fatalf("audit: %q %q %q", actor, onBehalf, via)
	}
	if got := approvalActor(t, sqlDB, itemID); got != "martin@fortix.example (key fortix)" {
		t.Fatalf("approval actor: %q", got)
	}

	// The preset keeps the key to the inbox, plans and runs reads.
	if code, _, _ := hReq(t, base, fortix, "GET", "/api/runs?item_id="+itemID, "", nil); code != 200 {
		t.Fatalf("runs read: %d", code)
	}
	if code, _, _ := hReq(t, base, fortix, "POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"x","author_agent_id":"fortix"}`, nil); code != http.StatusForbidden {
		t.Fatalf("plans write: %d", code)
	}
	// A key without the delegate permission can't name a person.
	plain := mint(t, root, `{"name":"plain","workspaces":["`+ws+`"],"approver":true}`)
	if code, _, _ := hReq(t, base, plain, "GET", "/api/inbox/"+itemID+"/state", "", map[string]string{"X-Skopos-On-Behalf-Of": "martin"}); code != http.StatusBadRequest {
		t.Fatalf("non-delegate on-behalf-of: %d", code)
	}
	// Granting the permission by a key edit (with a scope change, in one
	// update) lets it.
	var plainID string
	for _, k := range mustJSONList(t, root.req("GET", "/api/keys", "")) {
		if k.(map[string]any)["name"] == "plain" {
			plainID = k.(map[string]any)["id"].(string)
		}
	}
	mustDo(t, root.req("PATCH", "/api/keys/"+plainID, `{"delegate":true,"workspaces":["`+ws+`"]}`), 200)
	if code, _, _ := hReq(t, base, plain, "GET", "/api/inbox/"+itemID+"/state", "", map[string]string{"X-Skopos-On-Behalf-Of": "martin"}); code != http.StatusOK {
		t.Fatalf("after granting delegate: %d", code)
	}
	// A non-approver key sees the state but no actions.
	viewer := mint(t, root, `{"name":"viewer","workspaces":["`+ws+`"]}`)
	st = mustJSON(t, workerClient{base: base, key: viewer}.req("GET", "/api/inbox/"+itemID+"/state", ""), 200).(map[string]any)
	if len(toStrings(st["actions"])) != 0 {
		t.Fatalf("viewer actions: %v", st["actions"])
	}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"github.com/acme/other","name":"other"}`), 201)
	other := mint(t, root, `{"name":"other","workspaces":["github.com/acme/other"]}`)
	mustDo(t, workerClient{base: base, key: other}.req("GET", "/api/inbox/"+itemID+"/state", ""), 404)
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func approvalActor(t *testing.T, sqlDB *sql.DB, itemID string) string {
	t.Helper()
	var actor string
	if err := sqlDB.QueryRow(`SELECT actor FROM approvals WHERE item_id = ? ORDER BY created_at DESC LIMIT 1`, itemID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	return actor
}

func mustJSONList(t *testing.T, r *stubResp) []any {
	t.Helper()
	mustDo(t, r, 200)
	var out []any
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("bad json: %v: %s", err, r.body)
	}
	return out
}
