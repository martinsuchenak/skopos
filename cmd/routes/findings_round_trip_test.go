package routes

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/martinsuchenak/skopos/internal/runs"
)

// Review findings as records, over REST, across three rounds:
//   - a review's findings get ids, in the record and on the run alike; its
//     blocking finding is open, its nit noted;
//   - a second opinion's finding is noted until the approver sends it back
//     with the retry (approver-only; a wrong id stops the retry);
//   - the implementer is given what to fix and answers each by id;
//   - the next review is given the same findings with the implementer's
//     word, and what it finds still open counts against it;
//   - the approver waives what is left, and no later run is given it.
func TestIntegrationFindingsRoundTrip(t *testing.T) {
	const ws = "github.com/globex/shop"
	_, baseURL := workerStackWith(t, func(r *runs.Service) {
		r.SetPrefs(map[string]*runs.WorkspacePrefs{ws: {Stages: map[string]string{"review": "claude"}}})
	})
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}
	claimAs := func(id, runner string) map[string]any {
		t.Helper()
		return claim(t, worker, `{"worker_id":"`+id+`","runners":["`+runner+`"],"workspaces":["`+ws+`"]}`)
	}
	report := func(run map[string]any, workerID, body string) {
		t.Helper()
		id := run["id"].(string)
		mustDo(t, worker.reqH("POST", "/api/runs/"+id+"/report", `{"worker_id":"`+workerID+`",`+body+`}`, leases[id]), 200, "report "+run["stage"].(string))
	}
	// read is what a run's prompt gets: the ids in each list.
	read := func(run map[string]any, workerID string) map[string][]map[string]any {
		t.Helper()
		id := run["id"].(string)
		resp := mustDo(t, worker.reqH("POST", "/api/runs/"+id+"/findings/read", `{"worker_id":"`+workerID+`"}`, leases[id]), 200, "read findings")
		var out map[string][]map[string]any
		if err := json.Unmarshal([]byte(resp.body), &out); err != nil {
			t.Fatalf("findings/read: %v: %s", err, resp.body)
		}
		return out
	}
	texts := func(es []map[string]any) string {
		var out []string
		for _, e := range es {
			out = append(out, e["text"].(string))
		}
		return strings.Join(out, " | ")
	}

	// Plan, approve, implement (head aaa111).
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"rate limit","author_agent_id":"alex"}`), 201).(map[string]any)
	itemID := item["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)
	run := claimAs("w1", "claude")
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"p","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	revID := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"d2ce73d"}`), 201).(map[string]any)["id"].(string)
	report(run, "w1", `"status":"succeeded","summary":"plan ready","branch":"agent/rl"`)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+revID+`"}`), 200)
	impl := claimAs("w1", "claude")
	if got := read(impl, "w1"); len(got["fix"]) != 0 {
		t.Fatalf("nothing to fix before any review: %v", got)
	}
	report(impl, "w1", `"status":"succeeded","summary":"done","head_sha":"aaa111","branch":"agent/rl"`)

	// Round one: the review blocks with one blocking finding and a nit.
	rev1 := claimAs("w1", "claude")
	if rev1["stage"] != "review" {
		t.Fatalf("expected the review run, got %v", rev1["stage"])
	}
	// The read is the run holder's: another lease gets nothing.
	mustDo(t, worker.reqH("POST", "/api/runs/"+rev1["id"].(string)+"/findings/read", `{"worker_id":"w1"}`, "not-the-lease"), 409, "read with another lease")
	report(rev1, "w1", `"status":"failed","reason":"review failed: per process","summary":"per process","head_sha":"aaa111",
		"findings":[{"id":"worker-made-this-up","severity":"blocking","text":"counts per process","file":"limit.go:41"},{"severity":"nit","text":"rename lim"}]`)
	status := func() string {
		return mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)["status"].(string)
	}
	if status() != "blocked" {
		t.Fatalf("a failed review blocks: %s", status())
	}
	record := func(c workerClient) map[string]map[string]any {
		t.Helper()
		out := mustJSON(t, c.req("GET", "/api/inbox/"+itemID+"/findings", ""), 200).(map[string]any)
		by := map[string]map[string]any{}
		for _, f := range out["findings"].([]any) {
			m := f.(map[string]any)
			by[m["text"].(string)] = m
		}
		return by
	}
	// The record says which action takes findings to send back now: retry,
	// for an approver, since the item's plan is approved; nothing for a key
	// that may not send them.
	sendBack := func(c workerClient) any {
		t.Helper()
		return mustJSON(t, c.req("GET", "/api/inbox/"+itemID+"/findings", ""), 200).(map[string]any)["send_back"]
	}
	if sendBack(approver) != "retry" || sendBack(plain) != "" {
		t.Fatalf("send_back while blocked: approver %v, plain key %v", sendBack(approver), sendBack(plain))
	}
	rec := record(plain) // any key with the item in scope may read it
	block, nit := rec["counts per process"], rec["rename lim"]
	if block["state"] != "open" || block["tracked"] != true || nit["state"] != "noted" || block["head_sha"] != "aaa111" || block["file"] != "limit.go:41" {
		t.Fatalf("round one's record: %v / %v", block, nit)
	}
	blockID := block["id"].(string)
	if blockID == "worker-made-this-up" || len(blockID) != 36 {
		t.Fatalf("the server names findings: %q", blockID)
	}
	// The run's own copy carries the same id.
	gotRun := mustJSON(t, root.req("GET", "/api/runs/"+rev1["id"].(string), ""), 200).(map[string]any)
	if fs := gotRun["findings"].([]any); fs[0].(map[string]any)["id"] != blockID {
		t.Fatalf("the run's findings carry the record's ids: %v", fs)
	}

	// A second opinion's finding is advisory: noted, and not the
	// implementer's to fix until the approver sends it back.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"codex"}`), 202)
	so := claimAs("w-codex", "codex")
	report(so, "w-codex", `"status":"succeeded","verdict":"fail","summary":"a race","head_sha":"aaa111","findings":[{"severity":"blocking","text":"race in the counter"}]`)
	race := record(root)["race in the counter"]
	if race["state"] != "noted" || race["tracked"] != false || race["stage"] != "second_opinion" {
		t.Fatalf("a second opinion's finding is noted: %v", race)
	}
	raceID := race["id"].(string)

	// Retry, sending the second opinion's finding back. Approver-only, and
	// an id the item does not have stops it before anything is queued.
	mustDo(t, plain.req("POST", "/api/inbox/"+itemID+"/retry", `{"answer":"fix both","findings":["`+raceID+`"]}`), 403, "plain key")
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/retry", `{"answer":"fix both","findings":["no-such-finding"]}`), 400, "unknown finding")
	if status() != "blocked" {
		t.Fatalf("a refused retry leaves the item where it was: %s", status())
	}
	mustDo(t, worker.req("POST", "/api/runs/claim", `{"worker_id":"w1","runners":["claude"],"workspaces":["`+ws+`"]}`), 204, "nothing was queued by the refused retry")
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/retry", `{"answer":"fix both","findings":["`+raceID+`"]}`), 200)
	content := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)["content"].(string)
	if !strings.Contains(content, "Findings to fix:") || !strings.Contains(content, "race in the counter") {
		t.Fatalf("the item says what was sent back:\n%s", content)
	}
	if race := record(root)["race in the counter"]; race["state"] != "open" || race["sent_back"] != true {
		t.Fatalf("sent back: %v", race)
	}

	// Round two: the implementer is given both, and answers each.
	impl2 := claimAs("w1", "claude")
	if impl2["stage"] != "implement" {
		t.Fatalf("expected the implement run, got %v", impl2["stage"])
	}
	fix := read(impl2, "w1")["fix"]
	if texts(fix) != "counts per process | race in the counter" {
		t.Fatalf("to fix: %s", texts(fix))
	}
	report(impl2, "w1", `"status":"succeeded","summary":"moved the counter","head_sha":"bbb222","branch":"agent/rl",
		"finding_answers":[{"id":"`+blockID+`","status":"fixed","note":"the counter is in the database"},{"id":"`+raceID+`","status":"not_fixed","note":"needs a schema change"},{"id":"nope","status":"fixed"}]`)
	rec = record(root)
	if rec["counts per process"]["state"] != "fixed" || rec["race in the counter"]["state"] != "open" {
		t.Fatalf("after the implementer: %v / %v", rec["counts per process"]["state"], rec["race in the counter"]["state"])
	}

	// The review is given both with the implementer's word. It confirms
	// one and finds the other still open: that fails it without a new
	// finding of its own.
	rev2 := claimAs("w1", "claude")
	check := read(rev2, "w1")["check"]
	if texts(check) != "counts per process | race in the counter" || check[0]["claim"] != "fixed" || check[1]["claim"] != "not_fixed" || check[1]["claim_note"] != "needs a schema change" {
		t.Fatalf("to check: %v", check)
	}
	report(rev2, "w1", `"status":"failed","reason":"review failed: the race is still there","summary":"the race is still there","head_sha":"bbb222",
		"finding_answers":[{"id":"`+blockID+`","status":"addressed","note":"shared now"},{"id":"`+raceID+`","status":"still_open","note":"two statements, no transaction"}]`)
	gotRun = mustJSON(t, root.req("GET", "/api/runs/"+rev2["id"].(string), ""), 200).(map[string]any)
	if gotRun["still_open"] != float64(1) {
		t.Fatalf("the run records what it found still open: %v", gotRun["still_open"])
	}
	rec = record(root)
	if rec["counts per process"]["state"] != "addressed" || rec["race in the counter"]["state"] != "still_open" {
		t.Fatalf("after the second review: %v / %v", rec["counts per process"]["state"], rec["race in the counter"]["state"])
	}
	st := mustJSON(t, approver.req("GET", "/api/inbox/"+itemID+"/state", ""), 200).(map[string]any)
	last := st["reviews"].([]any)[0].(map[string]any)
	if last["passed"] != false || last["blocking"] != float64(1) || last["still_open"] != float64(1) || last["nits"] != float64(0) {
		t.Fatalf("the item state counts a still-open finding as blocking: %v", last)
	}
	// The state has the findings in short: counts by state, and the ones
	// something is still owed on.
	fv := st["findings"].(map[string]any)
	owed := fv["open"].([]any)
	if counts := fv["counts"].(map[string]any); counts["addressed"] != float64(1) || counts["still_open"] != float64(1) || counts["noted"] != float64(1) || len(owed) != 1 {
		t.Fatalf("findings in the item state: %v", fv)
	}
	if f := owed[0].(map[string]any); f["id"] != raceID || f["state"] != "still_open" || f["by"] != "second opinion · codex" {
		t.Fatalf("the finding still owed: %v", f)
	}

	// The approver accepts it as it is. Approver-only; recorded with who.
	mustDo(t, plain.req("POST", "/api/inbox/"+itemID+"/findings/"+raceID+"/waive", `{"note":"accepted for now"}`), 403, "plain key")
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/findings/no-such-finding/waive", `{}`), 404, "unknown finding")
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/findings/"+raceID+"/waive", `{}`), 400, "a blocking finding without a reason")
	waived := mustJSON(t, approver.req("POST", "/api/inbox/"+itemID+"/findings/"+raceID+"/waive", `{"note":"accepted for now"}`), 200).(map[string]any)
	ups := waived["updates"].([]any)
	if up := ups[len(ups)-1].(map[string]any); waived["state"] != "waived" || up["kind"] != "waived" || !strings.Contains(up["actor"].(string), "alex") || up["note"] != "accepted for now" {
		t.Fatalf("waived by the approver: %v", waived)
	}

	// Round three: nothing is left to fix, and the review is told what the
	// approver accepted instead of being asked to check it.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/retry", `{"answer":"go on"}`), 200)
	impl3 := claimAs("w1", "claude")
	if got := read(impl3, "w1"); len(got["fix"]) != 0 || texts(got["waived"]) != "race in the counter" {
		t.Fatalf("round three, to fix: %v", got)
	}
	report(impl3, "w1", `"status":"succeeded","summary":"nothing to do","head_sha":"ccc333","branch":"agent/rl"`)
	rev3 := claimAs("w1", "claude")
	got := read(rev3, "w1")
	if len(got["check"]) != 0 || texts(got["waived"]) != "race in the counter" || got["waived"][0]["waive_note"] != "accepted for now" {
		t.Fatalf("round three, to check: %v", got)
	}
	report(rev3, "w1", `"status":"succeeded","summary":"fine","head_sha":"ccc333"`)
	if status() != "in_review" {
		t.Fatalf("a passing review hands the item to the approver: %s", status())
	}

	if sendBack(approver) != "request-changes" {
		t.Fatalf("send_back at sign-off: %v", sendBack(approver))
	}
	// At the review gate, request changes can send a finding back too: the
	// nit from round one, which nobody followed until now.
	nitID := record(root)["rename lim"]["id"].(string)
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/request-changes", `{"findings":["`+nitID+`"]}`), 200)
	if nit := record(root)["rename lim"]; nit["state"] != "open" || nit["tracked"] != true {
		t.Fatalf("a nit sent back is followed: %v", nit)
	}
	impl4 := claimAs("w1", "claude")
	if got := read(impl4, "w1"); texts(got["fix"]) != "rename lim" {
		t.Fatalf("round four, to fix: %v", got)
	}
	if sendBack(approver) != "" {
		t.Fatalf("send_back while implementing: %v", sendBack(approver))
	}

	// Sign-off: the audit entry says what the approver was given with the
	// commit — the findings' state, and by id the ones no review settled
	// and the ones they accepted.
	report(impl4, "w1", `"status":"succeeded","summary":"renamed","head_sha":"ddd444","branch":"agent/rl","finding_answers":[{"id":"`+nitID+`","status":"fixed"}]`)
	rev4 := claimAs("w1", "claude")
	// The review fails without answering for the nit: it stays claimed.
	report(rev4, "w1", `"status":"succeeded","summary":"fine","head_sha":"ddd444","finding_answers":[{"id":"`+nitID+`","status":"still_open","note":"half renamed"}]`)
	if status() != "in_review" {
		t.Fatalf("a still-open nit does not block: %s", status())
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/mark-done", `{"head_sha":"ddd444"}`), 200)
	signedOff := ""
	resp := mustDo(t, root.req("GET", "/api/inbox/"+itemID+"/timeline", ""), 200)
	var entries []map[string]any
	if err := json.Unmarshal([]byte(resp.body), &entries); err != nil {
		t.Fatalf("timeline: %v: %s", err, resp.body)
	}
	for _, e := range entries {
		if e["action"] == "inbox.mark_done" {
			signedOff, _ = e["notes"].(string)
		}
	}
	for _, want := range []string{"findings at sign-off: ", "1 still open (" + nitID + ")", "1 waived (" + raceID + ")", "addressed"} {
		if !strings.Contains(signedOff, want) {
			t.Fatalf("the sign-off's audit entry lacks %q: %q", want, signedOff)
		}
	}
}

// Findings with a review panel, and who may see them:
//   - what a sibling found in the same round is not shown to the other
//     member, so each verdict is its own;
//   - in the next round each finding is checked by one member: its own
//     reviewer, and the first member for a second opinion's finding the
//     approver sent back; the other member sees them as someone else's and
//     its answer for them is not taken;
//   - the panel's summary says what was found still open;
//   - a key without the workspace gets 404; findings are sent back only
//     from the review gate.
func TestIntegrationFindingsPanelAndScope(t *testing.T) {
	const ws = "github.com/globex/shop"
	_, baseURL := workerStackWith(t, func(r *runs.Service) {
		r.SetPrefs(map[string]*runs.WorkspacePrefs{ws: {
			Stages: map[string]string{},
			Groups: map[string][]runs.Member{"review": {{Runner: "claude", Focus: "security"}, {Runner: "glm", Focus: "tests"}}},
		}})
	})
	root := workerClient{base: baseURL, key: "root-secret"}
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"github.com/globex/other"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	outsider := workerClient{base: baseURL, key: mint(t, root, `{"name":"elsewhere","workspaces":["github.com/globex/other"],"approver":true}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}
	claimAs := func(id, runner string) map[string]any {
		t.Helper()
		return claim(t, worker, `{"worker_id":"`+id+`","runners":["`+runner+`"],"workspaces":["`+ws+`"]}`)
	}
	report := func(run map[string]any, workerID, body string) {
		t.Helper()
		id := run["id"].(string)
		mustDo(t, worker.reqH("POST", "/api/runs/"+id+"/report", `{"worker_id":"`+workerID+`",`+body+`}`, leases[id]), 200, "report")
	}
	read := func(run map[string]any, workerID string) map[string]string {
		t.Helper()
		id := run["id"].(string)
		resp := mustDo(t, worker.reqH("POST", "/api/runs/"+id+"/findings/read", `{"worker_id":"`+workerID+`"}`, leases[id]), 200, "read findings")
		var raw map[string][]map[string]any
		if err := json.Unmarshal([]byte(resp.body), &raw); err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for list, es := range raw {
			var texts []string
			for _, e := range es {
				texts = append(texts, e["text"].(string))
			}
			out[list] = strings.Join(texts, " | ")
		}
		return out
	}
	// planned takes a new item to awaiting_approval and returns its ids.
	planned := func(title string) (itemID, revID string) {
		t.Helper()
		item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"`+title+`","author_agent_id":"alex"}`), 201).(map[string]any)
		itemID = item["id"].(string)
		mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/queue", ""), 200)
		run := claimAs("w1", "claude")
		plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"p","author_agent_id":"worker-1"}`), 201).(map[string]any)
		planID := plan["id"].(string)
		mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step"}`), 201)
		mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
		revID = mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"d2ce73d"}`), 201).(map[string]any)["id"].(string)
		report(run, "w1", `"status":"succeeded","summary":"plan ready","branch":"agent/p"`)
		return itemID, revID
	}

	itemID, revID := planned("panel item")
	// Findings go back from the review gate, not the plan gate.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/request-changes", `{"notes":"replan","findings":["anything"]}`), 400, "findings at the plan gate")
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/approve", `{"revision_id":"`+revID+`"}`), 200)
	report(claimAs("w1", "claude"), "w1", `"status":"succeeded","summary":"done","head_sha":"aaa111","branch":"agent/p"`)

	// Round one. Security reports first; the tests member, still running,
	// is shown nothing of it.
	sec := claimAs("w-claude", "claude")
	tst := claimAs("w-glm", "glm")
	report(sec, "w-claude", `"status":"failed","reason":"review failed: injection","summary":"injection","head_sha":"aaa111","findings":[{"severity":"blocking","text":"sql injection","file":"q.go:7"}]`)
	if got := read(tst, "w-glm"); got["check"] != "" || got["others"] != "" {
		t.Fatalf("a sibling's finding of the same round is not shown: %v", got)
	}
	report(tst, "w-glm", `"status":"succeeded","summary":"tests ok","head_sha":"aaa111"`)

	// A second opinion adds a finding; the approver sends it back.
	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/second-opinion", `{"runner":"codex"}`), 202)
	report(claimAs("w-codex", "codex"), "w-codex", `"status":"succeeded","verdict":"fail","summary":"a race","head_sha":"aaa111","findings":[{"severity":"blocking","text":"race in the counter"}]`)
	rec := mustJSON(t, approver.req("GET", "/api/inbox/"+itemID+"/findings", ""), 200).(map[string]any)
	ids := map[string]string{}
	for _, f := range rec["findings"].([]any) {
		m := f.(map[string]any)
		ids[m["text"].(string)] = m["id"].(string)
	}
	if counts := rec["counts"].(map[string]any); counts["open"] != float64(1) || counts["noted"] != float64(1) {
		t.Fatalf("counts by state: %v", counts)
	}

	// A key without the workspace sees neither the record nor a finding,
	// approver or not.
	mustDo(t, outsider.req("GET", "/api/inbox/"+itemID+"/findings", ""), 404, "record, out of scope")
	mustDo(t, outsider.req("POST", "/api/inbox/"+itemID+"/findings/"+ids["race in the counter"]+"/waive", `{}`), 404, "waive, out of scope")
	mustDo(t, outsider.req("POST", "/api/inbox/"+itemID+"/retry", `{"answer":"x","findings":["`+ids["race in the counter"]+`"]}`), 404, "send back, out of scope")

	mustDo(t, approver.req("POST", "/api/inbox/"+itemID+"/retry", `{"answer":"fix both","findings":["`+ids["race in the counter"]+`"]}`), 200)
	impl := claimAs("w1", "claude")
	if got := read(impl, "w1"); got["fix"] != "sql injection | race in the counter" {
		t.Fatalf("to fix: %v", got)
	}
	report(impl, "w1", `"status":"succeeded","summary":"fixed","head_sha":"bbb222","branch":"agent/p",
		"finding_answers":[{"id":"`+ids["sql injection"]+`","status":"fixed"},{"id":"`+ids["race in the counter"]+`","status":"fixed"}]`)

	// Round two: security (the first member) checks its own finding and the
	// one sent back; tests checks neither.
	sec2 := claimAs("w-claude", "claude")
	tst2 := claimAs("w-glm", "glm")
	if got := read(sec2, "w-claude"); got["check"] != "sql injection | race in the counter" || got["others"] != "" {
		t.Fatalf("the first member: %v", got)
	}
	if got := read(tst2, "w-glm"); got["check"] != "" || got["others"] != "sql injection | race in the counter" {
		t.Fatalf("the second member: %v", got)
	}
	// The tests member passes and answers for findings that are not its:
	// neither its word nor its passing settles them.
	report(tst2, "w-glm", `"status":"succeeded","summary":"tests ok","head_sha":"bbb222","finding_answers":[{"id":"`+ids["race in the counter"]+`","status":"addressed"}]`)
	state := func(text string) string {
		t.Helper()
		for _, f := range mustJSON(t, root.req("GET", "/api/inbox/"+itemID+"/findings", ""), 200).(map[string]any)["findings"].([]any) {
			if m := f.(map[string]any); m["text"] == text {
				return m["state"].(string)
			}
		}
		return ""
	}
	if state("race in the counter") != "fixed" || state("sql injection") != "fixed" {
		t.Fatalf("not the second member's to settle: %s / %s", state("race in the counter"), state("sql injection"))
	}
	report(sec2, "w-claude", `"status":"failed","reason":"review failed: the race is still there","summary":"the race is still there","head_sha":"bbb222",
		"finding_answers":[{"id":"`+ids["sql injection"]+`","status":"addressed","note":"parameterised"},{"id":"`+ids["race in the counter"]+`","status":"still_open","note":"no transaction"}]`)
	if state("sql injection") != "addressed" || state("race in the counter") != "still_open" {
		t.Fatalf("after round two: %s / %s", state("sql injection"), state("race in the counter"))
	}
	got := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	if got["status"] != "blocked" {
		t.Fatalf("a member that found an earlier finding still open blocks the panel: %v", got["status"])
	}
	content := got["content"].(string)
	for _, want := range []string{"1 earlier still open", "**Earlier findings still open:**", "(blocking, raised by second opinion · codex) race in the counter — no transaction", "**Earlier findings addressed:**", "sql injection — parameterised"} {
		if !strings.Contains(content, want) {
			t.Fatalf("the panel's summary lacks %q:\n%s", want, content)
		}
	}
}
