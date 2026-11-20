package routes

import (
	"net/http"
	"strings"
	"testing"
)

// Another system shows an item as a card (internal/cards): found by its
// reference, the card follows the item from the agents' work to the plan
// gate; an unknown reference is a "none" card; a key that may only read
// the inbox renders cards; an action is refused (cards carry no buttons
// yet).
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
	if !strings.Contains(cardText(env), "**Queued:** planning") {
		t.Fatalf("queued text: %s", cardText(env))
	}
	agentKey := env["key"]

	run := claim(t, worker, `{"worker_id":"w1","workspaces":["`+ws+`"]}`)
	runID := run["id"].(string)
	if _, env = card(globex, subject); env["key"] != agentKey || !strings.Contains(cardText(env), "Skopos is working on it:** planning") {
		t.Fatalf("claimed: the same key, the stage named: %v", env)
	}
	plan := mustJSON(t, worker.req("POST", "/api/plans", `{"workspace_id":"`+ws+`","name":"plan","author_agent_id":"worker-1"}`), 201).(map[string]any)
	planID := plan["id"].(string)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/items", `{"title":"step one","description":"do the first thing"}`), 201)
	mustDo(t, worker.req("POST", "/api/inbox/"+itemID+"/link-plan", `{"plan_id":"`+planID+`"}`), 200)
	mustDo(t, worker.req("POST", "/api/plans/"+planID+"/revisions", `{"base_sha":"0a1b2c3"}`), 201)
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"succeeded","summary":"plan ready","branch":"agent/x"}`, leases[runID]), 200)

	_, env = card(globex, subject)
	if env["status"] != "plan_approval" || env["attention"] != true || env["summary"] != "Plan ready for approval" || env["key"] == agentKey {
		t.Fatalf("at the gate: %v", env)
	}
	if !strings.Contains(cardText(env), "1. **step one**\\\n   do the first thing") {
		t.Fatalf("the plan's steps: %s", cardText(env))
	}
	if url, _ := env["url"].(string); url != "" && !strings.HasSuffix(url, "/?item="+itemID) {
		t.Fatalf("the link: %v", url)
	}

	// Read-only: the card renders; another POST to the inbox does not.
	if code, env := card(reader, subject); code != 200 || env["status"] != "plan_approval" {
		t.Fatalf("a read-only key: %d %v", code, env)
	}
	if code, _, _ := hReq(t, base, reader, "POST", "/api/inbox/"+itemID+"/answers", `{"answers":"x"}`, nil); code != http.StatusForbidden {
		t.Fatalf("a read-only key may not write: %d", code)
	}
	// Workspace narrowing, and what is refused.
	if _, env := card(globex, `{"subject":{"ref":"JOB100235"}}`); env["status"] != "plan_approval" {
		t.Fatalf("by ref alone: %v", env)
	}
	if code, out, _ := hReq(t, base, globex, "POST", "/api/inbox/card?workspace=github.com/other/repo", subject, nil); code != http.StatusForbidden {
		t.Fatalf("a workspace outside the key: %d %s", code, out)
	}
	for _, bad := range []string{
		`{"subject":{"ref":""}}`,
		`{"subject":{"ref":"JOB100235"},"action":{"name":"approve"}}`,
		`{"subject":{"ref":"JOB100235"},"extra":1}`,
	} {
		if code, _ := card(globex, bad); code != http.StatusBadRequest {
			t.Errorf("refused %s: %d", bad, code)
		}
	}
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
