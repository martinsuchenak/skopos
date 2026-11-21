package routes

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Another system shows an item as a card (internal/cards): found by its
// reference, the card follows the item from the agents' work to the plan
// gate; an unknown reference is a "none" card; a key that may only read
// the inbox renders cards without inputs; a key that may act gets the open
// questions as inputs and answers them through the card's button, as the
// person it names; an action on a card that has changed is refused.
func TestIntegrationCards(t *testing.T) {
	_, base := workerStack(t)
	root := workerClient{base: base, key: "root-secret"}
	const ws = "github.com/globex/shop"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`","name":"shop"}`), 201)
	globex := mint(t, root, `{"name":"globex","workspaces":["`+ws+`"],"preset":"integration","approver":true,"delegate":true}`)
	reader := mint(t, root, `{"name":"reader","workspaces":["`+ws+`"],"capabilities":["rest","inbox:read"]}`)
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`)
	leases := map[string]string{}
	worker := workerClient{base: base, key: workerKey, lease: leases}

	card := func(key, body string) (int, map[string]any) {
		t.Helper()
		code, out, _ := hReq(t, base, key, "POST", "/api/inbox/card", body, nil)
		if code != http.StatusOK {
			return code, map[string]any{"error": out}
		}
		return code, jsonMap(t, out)
	}
	subject := `{"subject":{"ref":"JOB100235","title":"AI configurations","url":"https://jobs.example.com/job/100235"}}`

	if code, env := card(globex, subject); code != 200 || env["status"] != "none" || env["key"] != "none" {
		t.Fatalf("before the item: %d %v", code, env)
	}

	create := `{"workspace_id":"` + ws + `","title":"JOB100235 AI configurations","content":"the job","author_agent_id":"globex",` +
		`"ref":"JOB100235","external_url":"https://jobs.example.com/job/100235","start":"queue"}`
	code, body, _ := hReq(t, base, globex, "POST", "/api/inbox", create, nil)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	itemID := jsonMap(t, body)["id"].(string)

	_, env := card(globex, subject)
	if env["status"] != "agent" || env["attention"] != false || !strings.HasPrefix(env["key"].(string), itemID+":agent") {
		t.Fatalf("queued: %v", env)
	}
	if !strings.Contains(cardText(env), "Queued: planning, waiting for a worker.") {
		t.Fatalf("queued text: %s", cardText(env))
	}
	agentKey := env["key"]

	run := claim(t, worker, `{"worker_id":"w1","workspaces":["`+ws+`"]}`)
	runID := run["id"].(string)
	if _, env = card(globex, subject); env["key"] != agentKey || !strings.Contains(cardText(env), "### Skopos is working on it: planning") {
		t.Fatalf("claimed: the same key, the stage named: %v", env)
	}
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"plan","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step one","description":"do the first thing"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	revID := mustJSON(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"0a1b2c3"}`), 201).(map[string]any)["id"].(string)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/questions", `{"worker_id":"w1","asker":"planner","blocking":false,"subject_kind":"plan_revision","subject":"`+revID+`",
		"questions":[{"question":"Which configurations?","options":["all of them","only the new ones"]},{"question":"Anything to keep?","options":[]}]}`, leases[runID]), 201)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"succeeded","summary":"plan ready","branch":"agent/x"}`, leases[runID]), 200)

	_, env = card(globex, subject)
	if env["status"] != "plan_approval" || env["attention"] != true || env["summary"] != "Plan ready for approval — 2 questions" || env["key"] == agentKey {
		t.Fatalf("at the gate: %v", env)
	}
	if !strings.Contains(cardText(env), "1. **step one**\\\n   do the first thing") {
		t.Fatalf("the plan's steps: %s", cardText(env))
	}
	if url, _ := env["url"].(string); url != "" && !strings.HasSuffix(url, "/?item="+itemID) {
		t.Fatalf("the link: %v", url)
	}

	// Read-only: the card renders, with the questions as text and no
	// inputs; another POST to the inbox does not.
	if code, env := card(reader, subject); code != 200 || env["status"] != "plan_approval" || len(cardKinds(env)["TextField"]) != 0 ||
		!strings.Contains(cardText(env), "(a) all of them") {
		t.Fatalf("a read-only key: %d %v", code, env)
	}
	if code, _, _ := hReq(t, base, reader, "POST", "/api/inbox/"+itemID+"/answers", `{"answers":"x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("a read-only key may not write: %d", code)
	}
	// The reference in the address, as a host fills it into the endpoint it
	// is set up with: with no body, or a body that only describes the record.
	for _, body := range []string{"", `{"subject":{"title":"AI configurations","url":"https://jobs.example.com/job/100235"}}`} {
		code, out, _ := hReq(t, base, globex, "POST", "/api/inbox/card?ref=JOB100235", body, nil)
		if code != 200 || jsonMap(t, out)["status"] != "plan_approval" {
			t.Fatalf("?ref= with body %q: %d %s", body, code, out)
		}
	}
	if code, out, _ := hReq(t, base, globex, "POST", "/api/inbox/card?ref=JOB100235", `{"subject":{"ref":"JOB999"}}`, nil); code != 400 || !strings.Contains(out, "differ") {
		t.Fatalf("two references that differ: %d %s", code, out)
	}
	// Workspace narrowing, and what is refused.
	if _, env := card(globex, `{"subject":{"ref":"JOB100235"}}`); env["status"] != "plan_approval" {
		t.Fatalf("by ref alone: %v", env)
	}
	if code, out, _ := hReq(t, base, globex, "POST", "/api/inbox/card?workspace=github.com/other/repo", subject, nil); code != http.StatusForbidden {
		t.Fatalf("a workspace outside the key: %d %s", code, out)
	}
	// A key that may act: the gate's buttons, each question an input bound
	// under its id, a notes field, and the buttons' context naming the item
	// and the card's key.
	kinds := cardKinds(env)
	if len(kinds["ChoicePicker"]) != 1 || len(kinds["TextField"]) != 3 || len(kinds["Button"]) != 4 {
		t.Fatalf("the plan gate's form: %v", kinds)
	}
	var names []string
	for _, b := range kinds["Button"] {
		names = append(names, b["action"].(map[string]any)["event"].(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "approve,request-changes,answers,reject" {
		t.Fatalf("the buttons: %v", names)
	}
	ctxAns := kinds["Button"][0]["action"].(map[string]any)["event"].(map[string]any)["context"].(map[string]any)
	if ctxAns["item"] != itemID || ctxAns["key"] != env["key"] {
		t.Fatalf("the button's context: %v", ctxAns)
	}
	model := cardDataModel(env)["answers"].(map[string]any)
	var q1, q2 string
	for id := range model {
		if strings.HasPrefix(kinds["ChoicePicker"][0]["value"].(map[string]any)["@path"].(string), "/answers/"+id+"/") {
			q1 = id
		} else {
			q2 = id
		}
	}
	if q1 == "" || q2 == "" {
		t.Fatalf("the data model names both questions: %v", model)
	}
	// since: when the item entered its status, as the card's buttons carry it.
	since := func(card map[string]any) string {
		t.Helper()
		b := cardKinds(card)["Button"][0]["action"].(map[string]any)["event"].(map[string]any)["context"].(map[string]any)
		return b["since"].(string)
	}
	cardSince := since(env)
	if cardSince == "" {
		t.Fatal("the buttons carry when the item entered its status")
	}
	act := func(key, name, item string, cardKey any, rest string) (int, string) {
		t.Helper()
		body := `{"subject":{"ref":"JOB100235"},"action":{"name":"` + name + `","surfaceId":"skopos-` + itemID + `","sourceComponentId":"c1","timestamp":"2026-11-20T10:00:00Z",` +
			`"context":{"item":"` + item + `","key":"` + cardKey.(string) + `","since":"` + cardSince + `"` + rest + `}}}`
		var person map[string]string // only a delegate key may name a person
		if key == globex {
			person = map[string]string{"X-Skopos-On-Behalf-Of": "alex@globex.example"}
		}
		code, out, _ := hReq(t, base, key, "POST", "/api/inbox/card", body, person)
		return code, out
	}
	gateKey := env["key"]
	pickA := `,"answers":{"` + q1 + `":{"option":["a"],"text":""},"` + q2 + `":{"option":[],"text":"keep the old report"}}`
	if code, _ := act(reader, "answers", itemID, gateKey, pickA); code != http.StatusForbidden {
		t.Fatalf("a read-only key may not act: %d", code)
	}
	if code, _ := act(globex, "answers", itemID, "another-key", pickA); code != http.StatusConflict {
		t.Fatalf("a card drawn before something happened: %d", code)
	}
	if code, _ := act(globex, "answers", "another-item", gateKey, pickA); code != http.StatusConflict {
		t.Fatalf("another item than the card's: %d", code)
	}
	if code, out := act(globex, "answers", itemID, gateKey, `,"answers":{"`+q1+`":{"option":[],"text":"  "}}`); code != http.StatusBadRequest {
		t.Fatalf("nothing answered: %d %s", code, out)
	}
	if code, out := act(globex, "reject", itemID, gateKey, `,"notes":""`); code != http.StatusBadRequest {
		t.Fatalf("reject without a reason: %d %s", code, out)
	}
	code, out := act(globex, "answers", itemID, gateKey, pickA)
	if code != http.StatusOK {
		t.Fatalf("answering: %d %s", code, out)
	}
	after := jsonMap(t, out)
	if after["status"] != "plan_approval" || strings.Contains(cardText(after), "Which configurations?") || after["key"] == gateKey {
		t.Fatalf("the card after the answers: no question open, a new key: %v", after)
	}
	record := mustJSON(t, root.req("GET", "/api/inbox/"+itemID+"/questions", ""), 200).(map[string]any)
	if s := fmtJSON(record); !strings.Contains(s, `"option":"a"`) || !strings.Contains(s, "keep the old report") || !strings.Contains(s, "alex@globex.example") {
		t.Fatalf("the answers are on record, as the person: %s", s)
	}
	if code, _ := act(globex, "answers", itemID, gateKey, pickA); code != http.StatusConflict {
		t.Fatalf("the same answers again (a double click): %d", code)
	}

	// Approve on the card: the plan at the gate, the note with it, as the
	// person; the card follows the item to the agents.
	code, out = act(globex, "approve", itemID, after["key"], `,"notes":"keep it small"`)
	if code != http.StatusOK {
		t.Fatalf("approving: %d %s", code, out)
	}
	if approved := jsonMap(t, out); approved["status"] != "agent" || len(cardKinds(approved)["Button"]) != 1 ||
		!strings.Contains(cardText(approved), "**Implement**") || !strings.Contains(cardText(approved), "Reject needs a note.") {
		t.Fatalf("after the approval: %v", approved)
	}
	item := mustJSON(t, root.req("GET", "/api/inbox/"+itemID, ""), 200).(map[string]any)
	if item["status"] != "approved" && item["status"] != "implementing" {
		t.Fatalf("the item is approved: %v", item["status"])
	}
	_, report, _ := hReq(t, base, "root-secret", "GET", "/api/inbox/"+itemID+"/report?format=md", "", nil)
	if !strings.Contains(report, "alex@globex.example") || !strings.Contains(report, "keep it small") || !strings.Contains(report, "(card)") {
		t.Fatalf("the approval and its note are the person's: %s", report)
	}
	if code, _ := act(globex, "approve", itemID, after["key"], ""); code != http.StatusConflict {
		t.Fatalf("approving twice: %d", code)
	}

	for _, bad := range []string{
		`{"subject":{"ref":""}}`,
		`{"subject":{"ref":"JOB100235"},"action":{"name":"delete"}}`,
		`{"subject":{"ref":"JOB100235"},"action":"answer"}`,
		`{"subject":{"ref":"JOB100235"},"extra":1}`,
	} {
		if code, _ := card(globex, bad); code != http.StatusBadRequest {
			t.Errorf("refused %s: %d", bad, code)
		}
	}
}

// cardKinds is the card's components by kind.
func cardKinds(env map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, m := range env["a2ui"].([]any) {
		create, ok := m.(map[string]any)["createSurface"].(map[string]any)
		if !ok {
			continue
		}
		for _, c := range create["components"].([]any) {
			cm := c.(map[string]any)
			kind := cm["component"].(string)
			out[kind] = append(out[kind], cm)
		}
	}
	return out
}

// cardDataModel is the surface's starting data model.
func cardDataModel(env map[string]any) map[string]any {
	for _, m := range env["a2ui"].([]any) {
		if create, ok := m.(map[string]any)["createSurface"].(map[string]any); ok {
			dm, _ := create["dataModel"].(map[string]any)
			return dm
		}
	}
	return nil
}

func fmtJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// cardText is every Text of the card's surface, joined.
func cardText(env map[string]any) string {
	var out []string
	for _, m := range env["a2ui"].([]any) {
		create, ok := m.(map[string]any)["createSurface"].(map[string]any)
		if !ok {
			continue // the deleteSurface before it
		}
		cs := create["components"].([]any)
		for _, c := range cs {
			if cm := c.(map[string]any); cm["component"] == "Text" {
				out = append(out, cm["text"].(string))
			}
		}
	}
	return strings.Join(out, "\n")
}
