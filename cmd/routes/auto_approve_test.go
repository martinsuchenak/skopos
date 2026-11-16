package routes

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/runs"
)

// A plan approved automatically: an item with auto_approve_plan whose
// planner asks no question goes straight on to its implementation, the
// approval recorded as the server's in the name of who switched it on. A
// question from the planner, or a re-plan the approver asked for, holds the
// plan at the gate with the reason on record. Only an approver switches it.
func TestIntegrationAutoApprovePlan(t *testing.T) {
	const ws = "github.com/globex/shop"
	_, baseURL := workerStackWith(t, func(*runs.Service) {})
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}
	claimNext := func() map[string]any {
		t.Helper()
		return claim(t, worker, `{"worker_id":"w1","runners":["claude"],"workspaces":["`+ws+`"]}`)
	}
	report := func(run map[string]any, body string) {
		t.Helper()
		id := run["id"].(string)
		mustDo(t, worker.reqH("POST", "/api/runs/"+id+"/report", `{"worker_id":"w1",`+body+`}`, leases[id]), 200, "report "+run["stage"].(string))
	}
	item := func(id string) map[string]any {
		t.Helper()
		return mustJSON(t, approver.req("GET", "/api/inbox/"+id, ""), 200).(map[string]any)
	}
	timeline := func(id string) []map[string]any {
		t.Helper()
		var out []map[string]any
		if err := json.Unmarshal([]byte(mustDo(t, approver.req("GET", "/api/inbox/"+id+"/timeline", ""), 200).body), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	entry := func(id, action string) map[string]any {
		t.Helper()
		for _, e := range timeline(id) { // newest first
			if e["action"] == action {
				return e
			}
		}
		return nil
	}
	// plan writes the plan a planner's run would (plan, step, revision) and
	// reports the run.
	plan := func(itemID string, run map[string]any) {
		t.Helper()
		planID, _ := item(itemID)["plan_id"].(string)
		if planID == "" {
			p := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"p","author_agent_id":"worker-1"}`), 201).(map[string]any)
			planID = p["id"].(string)
			mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"bump the dependencies"}`), 201)
			mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
		}
		mustDo(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"0a1b2c3"}`), 201)
		report(run, `"status":"succeeded","summary":"plan ready","branch":"agent/deps"`)
	}

	// Only an approver switches it on, at creation as later.
	resp := plain.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"x","author_agent_id":"a","auto_approve_plan":true}`)
	if resp.code != 403 {
		t.Fatalf("a key without the approver permission creates no item that approves itself: %d %s", resp.code, resp.body)
	}

	// No question: approved at once, the implementation queued.
	auto := mustJSON(t, approver.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"Update the dependencies","author_agent_id":"alex","auto_approve_plan":true,"start":"queue"}`), 201).(map[string]any)
	autoID := auto["id"].(string)
	if auto["auto_approve_plan"] != true || auto["auto_approve_by"] != "key alex" {
		t.Fatalf("the item says who switched it on: %v %v", auto["auto_approve_plan"], auto["auto_approve_by"])
	}
	plan(autoID, claimNext())
	if got := item(autoID)["status"]; got != "approved" {
		t.Fatalf("a plan without questions is approved automatically: %v", got)
	}
	e := entry(autoID, "inbox.approve")
	if e == nil || e["via"] != "auto" || e["actor"] != "auto (set by key alex)" {
		t.Fatalf("the approval is the server's, in alex's name: %v", e)
	}
	if entry(autoID, "inbox.auto_approve_held") != nil {
		t.Fatal("nothing held it")
	}
	// Another worker takes it: w1's next poll would give back a run it
	// still holds.
	if impl := claim(t, worker, `{"worker_id":"w2","runners":["claude"],"workspaces":["`+ws+`"]}`); impl["stage"] != "implement" || impl["item_id"] != autoID {
		t.Fatalf("the implementation is queued: %v", impl)
	}

	// The planner asks: held at the gate, with the reason on record.
	asked := mustJSON(t, approver.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"Update the dependencies","author_agent_id":"alex","start":"queue"}`), 201).(map[string]any)
	askedID := asked["id"].(string)
	// Switched on later, on an item already in the workflow: the plain key
	// may not, the approver may, and it is audited.
	if r := plain.req("PATCH", "/api/inbox/"+askedID, `{"auto_approve_plan":true}`); r.code != 403 {
		t.Fatalf("a plain key switching it on: %d %s", r.code, r.body)
	}
	mustDo(t, approver.req("PATCH", "/api/inbox/"+askedID, `{"auto_approve_plan":true}`), 204)
	if e := entry(askedID, "inbox.edit"); e == nil || !strings.Contains(e["notes"].(string), "auto_approve_plan (on)") {
		t.Fatalf("switching it on is audited: %v", e)
	}
	run := claimNext()
	runID := run["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/questions",
		`{"worker_id":"w1","asker":"planner","blocking":false,"subject_kind":"base","subject":"0a1b2c3",
		  "questions":[{"question":"Take the new major version of the router?","options":["yes","no"]}]}`, leases[runID]), 201, "the planner asks")
	plan(askedID, run)
	if got := item(askedID)["status"]; got != "awaiting_approval" {
		t.Fatalf("a plan with a question waits for the approver: %v", got)
	}
	if e := entry(askedID, "inbox.auto_approve_held"); e == nil || e["notes"] != "the planner asked 1 question" || e["actor"] != "system" {
		t.Fatalf("the hold is on record with its reason: %v", e)
	}

	// Sent back: the plan that answers the change request is held too.
	mustDo(t, approver.req("POST", "/api/inbox/"+askedID+"/request-changes", `{"notes":"keep the router on 4"}`), 200)
	plan(askedID, claimNext())
	if got := item(askedID)["status"]; got != "awaiting_approval" {
		t.Fatalf("a re-plan the approver asked for waits for them: %v", got)
	}
	if e := entry(askedID, "inbox.auto_approve_held"); e == nil || e["notes"] != "you asked for this plan again" {
		t.Fatalf("the re-plan's hold: %v", e)
	}

	// Approved by hand with a reason: kept with the approval and in the
	// report, never in what the implementer reads.
	rev := mustJSON(t, approver.req("GET", "/api/inbox/"+askedID+"/state", ""), 200).(map[string]any)["plan"].(map[string]any)["revisions"].([]any)[0].(map[string]any)
	mustDo(t, approver.req("POST", "/api/inbox/"+askedID+"/approve", `{"revision_id":"`+rev["revision_id"].(string)+`","notes":"pin the router","reason":"the smaller of the two changes"}`), 200)
	if impl := claim(t, worker, `{"worker_id":"w3","runners":["claude"],"workspaces":["`+ws+`"]}`); impl["item_id"] != askedID || impl["stage"] != "implement" {
		t.Fatalf("approved by hand, implemented: %v", impl)
	}
	if c := item(askedID)["content"].(string); strings.Contains(c, "smaller of the two") || !strings.Contains(c, "pin the router") {
		t.Fatalf("the reason is not appended for the implementer:\n%s", c)
	}
	var appr []map[string]any
	if err := json.Unmarshal([]byte(mustDo(t, approver.req("GET", "/api/inbox/"+askedID+"/approvals", ""), 200).body), &appr); err != nil || len(appr) == 0 || appr[0]["reason"] != "the smaller of the two changes" {
		t.Fatalf("the approval keeps the reason: %v %v", appr, err)
	}
	if rep := mustDo(t, approver.req("GET", "/api/inbox/"+askedID+"/report", ""), 200).body; !strings.Contains(rep, "**Why this plan:** the smaller of the two changes") || strings.Contains(rep, "> Why this plan") {
		t.Fatalf("the report shows the reason once:\n%s", rep)
	}

	// The item report names both.
	if rep := mustDo(t, approver.req("GET", "/api/inbox/"+autoID+"/report?level=summary", ""), 200).body; !strings.Contains(rep, "· Plan approved automatically, switched on by key alex") {
		t.Fatalf("the report names the automatic approval:\n%s", rep)
	}
	if rep := mustDo(t, approver.req("GET", "/api/inbox/"+askedID+"/report?level=summary", ""), 200).body; !strings.Contains(rep, "· Not approved automatically: the planner asked 1 question") {
		t.Fatalf("the report names the hold:\n%s", rep)
	}
	if st := mustJSON(t, approver.req("GET", "/api/inbox/"+askedID+"/state", ""), 200).(map[string]any); st["item"].(map[string]any)["auto_approve_plan"] != true {
		t.Fatalf("the item state carries the setting: %v", st["item"])
	}

	// The approval itself fails (here: the planner linked no plan): the item
	// waits at the gate after all, with the reason on record.
	broken := mustJSON(t, approver.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"Broken","author_agent_id":"alex","auto_approve_plan":true,"start":"queue"}`), 201).(map[string]any)
	brokenID := broken["id"].(string)
	report(claimNext(), `"status":"succeeded","summary":"plan ready","branch":"agent/x"`)
	if got := item(brokenID)["status"]; got != "awaiting_approval" {
		t.Fatalf("a failed automatic approval leaves the gate open: %v", got)
	}
	if e := entry(brokenID, "inbox.auto_approve_held"); e == nil || !strings.HasPrefix(e["notes"].(string), "the approval failed: ") {
		t.Fatalf("the failed approval's hold: %v", e)
	}

	// Switched off: the gate is the person's again, and nothing is held.
	off := mustJSON(t, approver.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"Tidy","author_agent_id":"alex","auto_approve_plan":true}`), 201).(map[string]any)
	offID := off["id"].(string)
	mustDo(t, approver.req("PATCH", "/api/inbox/"+offID, `{"auto_approve_plan":false}`), 204)
	if got := item(offID); got["auto_approve_plan"] != nil || got["auto_approve_by"] != nil {
		t.Fatalf("switched off: %v %v", got["auto_approve_plan"], got["auto_approve_by"])
	}
	if e := entry(offID, "inbox.edit"); e == nil || !strings.Contains(e["notes"].(string), "auto_approve_plan (off)") {
		t.Fatalf("switching it off on an open item is audited too: %v", e)
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+offID+"/queue", ""), 200)
	plan(offID, claimNext())
	if got := item(offID)["status"]; got != "awaiting_approval" || entry(offID, "inbox.auto_approve_held") != nil {
		t.Fatalf("an item without it waits at the gate as always: %v", got)
	}
}
