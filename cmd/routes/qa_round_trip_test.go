package routes

import (
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/runs"
)

// QA is a stage of its own in the queue. Set without a review, it follows
// the implementation. A QA that fails with findings sends the item back to
// the implementer, as a review does; a QA that could not test asks, and the
// approver's answer starts QA again on the same branch — nothing was wrong
// with the implementation, so it is not run again.
func TestIntegrationQAAsksAndRetests(t *testing.T) {
	const ws = "github.com/globex/shop"
	_, baseURL := workerStackWith(t, func(r *runs.Service) {
		r.SetPrefs(map[string]*runs.WorkspacePrefs{ws: {Stages: map[string]string{"qa": "claude"}}})
	})
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
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

	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"date of birth is mandatory","author_agent_id":"alex"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	get := func() map[string]any {
		t.Helper()
		return mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)
	run := claimNext()
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"p","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	revID := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"0a1b2c3"}`), 201).(map[string]any)["id"].(string)
	report(run, `"status":"succeeded","summary":"plan ready","branch":"agent/dob"`)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+revID+`"}`), 200)
	report(claimNext(), `"status":"succeeded","summary":"done","head_sha":"aaa111","branch":"agent/dob"`)

	// No review is set: QA follows the implementation. It finds a bug.
	qa1 := claimNext()
	if qa1["stage"] != "qa" || qa1["branch"] != "agent/dob" {
		t.Fatalf("QA follows the implementation when no review is set: %v on %v", qa1["stage"], qa1["branch"])
	}
	if st := get()["status"]; st != "implementing" {
		t.Fatalf("the item waits for QA before sign-off: %v", st)
	}
	report(qa1, `"status":"failed","reason":"qa failed: saves without a date","summary":"saves without a date","head_sha":"aaa111",
		"findings":[{"severity":"blocking","text":"the form saves with an empty date of birth"}]`)
	if st := get()["status"]; st != "blocked" {
		t.Fatalf("a failed QA blocks: %v", st)
	}
	// Findings: the retry starts an implementation, as after a review.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/retry", `{"answer":"fix it"}`), 200)
	impl2 := claimNext()
	if impl2["stage"] != "implement" {
		t.Fatalf("a QA that found a bug sends the item back to the implementer: %v", impl2["stage"])
	}
	report(impl2, `"status":"succeeded","summary":"fixed","head_sha":"bbb222","branch":"agent/dob"`)

	// This time QA cannot test and asks.
	qa2 := claimNext()
	if qa2["stage"] != "qa" {
		t.Fatalf("QA runs on the new head: %v", qa2["stage"])
	}
	qa2ID := qa2["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+qa2ID+"/questions",
		`{"worker_id":"w1","asker":"worker","blocking":true,"subject_kind":"head","subject":"bbb222",
		  "questions":[{"question":"The local system does not answer. Which system should QA test on?","options":["start the local system","a new test space"]}]}`,
		leases[qa2ID]), 201, "QA asks")
	report(qa2, `"status":"failed","reason":"qa needs an answer: The local system does not answer. Which system should QA test on?","head_sha":"bbb222"`)
	blocked := get()
	if blocked["status"] != "blocked" {
		t.Fatalf("a QA that asks waits for the approver: %v", blocked["status"])
	}
	content := blocked["content"].(string)
	if !strings.Contains(content, "## Questions — QA") || !strings.Contains(content, "Which system should QA test on?") || !strings.Contains(content, "(a) start the local system") {
		t.Fatalf("QA's question is in the item under its own heading:\n%s", content)
	}
	// Nothing to send back to an implementer: this retry starts QA.
	if sb := mustJSON(t, approver.req("GET", "/api/inbox/"+itemID+"/findings", ""), 200).(map[string]any)["send_back"]; sb != "" {
		t.Fatalf("a retry that starts QA takes no findings to send back: %v", sb)
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/retry", `{"answer":"start the local system; it is up now"}`), 200)
	qa3 := claimNext()
	if qa3["stage"] != "qa" || qa3["branch"] != "agent/dob" {
		t.Fatalf("the answer starts QA again, not a new implementation: %v on %v", qa3["stage"], qa3["branch"])
	}
	if !strings.Contains(get()["content"].(string), "it is up now") {
		t.Fatal("the answer is in the item for the QA run that follows")
	}
	report(qa3, `"status":"succeeded","summary":"3 scenarios passed on the local system","head_sha":"bbb222"`)
	if st := get()["status"]; st != "in_review" {
		t.Fatalf("a passing QA hands the item to the approver: %v", st)
	}
}
