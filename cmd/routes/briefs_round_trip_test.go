package routes

import (
	"strings"
	"testing"
)

// Briefs held on the server, end to end: an approver saves one for a
// workspace, a plain key cannot; a run whose member has that focus gets it
// with the claim, from the workspace before its group; a worker whose
// release cannot take briefs gets none of the workspace's runs; removing
// the brief leaves the run to the repository's file; every change is a
// revision and an audit entry.
func TestIntegrationPipelineBriefs(t *testing.T) {
	sqlDB, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws = "github.com/globex/api"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}
	briefs := "/api/workspaces/" + escapeWS(ws) + "/pipeline/briefs"

	// Who may save one, and what is refused.
	mustDo(t, plain.req("PUT", briefs+"/e2e", `{"body":"Test on the local system."}`), 403)
	mustDo(t, approver.req("PUT", briefs+"/E2E", `{"body":"x"}`), 400)
	mustDo(t, approver.req("PUT", briefs+"/e2e", `{"body":"  "}`), 400)
	mustDo(t, approver.req("PUT", briefs+"/e2e", `{"body":"`+strings.Repeat("a", 17<<10)+`"}`), 400)
	mustDo(t, approver.req("PUT", briefs+"/e2e", `{"text":"a misspelt field"}`), 400)
	first := mustJSON(t, approver.req("PUT", briefs+"/e2e", `{"body":"Test on the local system."}`), 200).(map[string]any)
	if first["name"] != "e2e" || first["rev"] == "" || first["body"] != nil {
		t.Fatalf("saved brief: %v", first)
	}
	// The same text again is not a new revision.
	if again := mustJSON(t, approver.req("PUT", briefs+"/e2e", `{"body":"Test on the local system.\n"}`), 200).(map[string]any); again["rev"] != first["rev"] {
		t.Fatalf("an unchanged save must not add a revision: %v vs %v", again["rev"], first["rev"])
	}
	// Any key with the workspace in scope reads them.
	list := mustJSON(t, plain.req("GET", briefs, ""), 200).(map[string]any)["briefs"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["source"] != "workspace" || list[0].(map[string]any)["body"] != nil {
		t.Fatalf("list: %v", list)
	}
	if got := mustJSON(t, plain.req("GET", briefs+"/e2e", ""), 200).(map[string]any); got["body"] != "Test on the local system." {
		t.Fatalf("read: %v", got)
	}

	// The plan stage's member has that focus: its run gets the brief with
	// the claim — but only a worker that says it takes briefs gets the run.
	mustDo(t, approver.req("PUT", "/api/workspaces/"+escapeWS(ws)+"/pipeline", `{"stages":{"plan":{"members":[{"runner":"claude","focus":"e2e"}]}}}`), 200)
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"briefed","author_agent_id":"alex"}`), 201).(map[string]any)
	mustDo(t, approver.req("POST", "/api/inbox/"+item["id"].(string)+"/queue", ""), 200)
	mustDo(t, worker.req("POST", "/api/runs/claim", `{"worker_id":"w-old","runners":["claude"],"workspaces":["`+ws+`"]}`), 204)
	for _, w := range workerList(t, root) {
		m := w.(map[string]any)
		if m["id"] == "w-old" {
			if un, _ := m["unhealthy"].(map[string]any); !strings.Contains(un[ws].(string), "cannot take the workspace's briefs") {
				t.Fatalf("a worker that cannot take briefs is marked for the workspace: %v", m)
			}
		}
	}
	run := claim(t, worker, `{"worker_id":"w1","runners":["claude"],"workspaces":["`+ws+`"],"features":["run_brief"]}`)
	brief, _ := run["brief"].(map[string]any)
	if run["focus"] != "e2e" || brief["name"] != "e2e" || brief["body"] != "Test on the local system." || brief["source"] != "workspace" || brief["rev"] != first["rev"] {
		t.Fatalf("the claim carries the workspace's brief: %v", run)
	}
	// Never stored with the run.
	if got := mustJSON(t, root.req("GET", "/api/runs/"+run["id"].(string), ""), 200).(map[string]any); got["brief"] != nil {
		t.Fatalf("the brief is in the claim answer only: %v", got)
	}

	// A group's brief reaches the workspace when it holds none of its own.
	mustDo(t, root.req("POST", "/api/groups", `{"name":"globex","patterns":["github.com/globex/*"]}`), 201)
	mustDo(t, approver.req("PUT", "/api/groups/globex/pipeline/briefs/security", `{"body":"Check every query."}`), 403)
	mustDo(t, root.req("PUT", "/api/groups/globex/pipeline/briefs/security", `{"body":"Check every query."}`), 200)
	mustDo(t, root.req("PUT", "/api/groups/globex/pipeline/briefs/e2e", `{"body":"The group's way of testing."}`), 200)
	by := map[string]map[string]any{}
	for _, b := range mustJSON(t, approver.req("GET", briefs, ""), 200).(map[string]any)["briefs"].([]any) {
		by[b.(map[string]any)["name"].(string)] = b.(map[string]any)
	}
	if by["security"]["source"] != "group:globex" || by["e2e"]["source"] != "workspace" {
		t.Fatalf("a workspace's own brief wins over its group's: %v", by)
	}
	// Removed from the workspace: the group's is in force, with a history
	// of what the workspace held.
	mustDo(t, plain.req("DELETE", briefs+"/e2e", ""), 403)
	mustDo(t, approver.req("DELETE", briefs+"/e2e", ""), 204)
	if got := mustJSON(t, approver.req("GET", briefs+"/e2e", ""), 200).(map[string]any); got["body"] != "The group's way of testing." || got["source"] != "group:globex" || got["history"] != nil {
		t.Fatalf("after the removal the group's applies: %v", got)
	}
	mustDo(t, approver.req("DELETE", briefs+"/e2e", ""), 404)
	mustDo(t, root.req("DELETE", "/api/groups/globex/pipeline/briefs/e2e", ""), 204)
	mustDo(t, approver.req("GET", briefs+"/e2e", ""), 404)
	// Set again: the history keeps every revision, newest first.
	mustDo(t, approver.req("PUT", briefs+"/e2e", `{"body":"Second version."}`), 200)
	hist := mustJSON(t, approver.req("GET", briefs+"/e2e", ""), 200).(map[string]any)["history"].([]any)
	if len(hist) != 3 || hist[0].(map[string]any)["deleted"] == true || hist[1].(map[string]any)["deleted"] != true || hist[2].(map[string]any)["rev"] != first["rev"] {
		t.Fatalf("history: %v", hist)
	}
	// Every change is in the audit log, without the text.
	var audit string
	if err := sqlDB.QueryRow(`SELECT group_concat(action || ' ' || COALESCE(notes, ''), '; ') FROM (SELECT * FROM audit_log WHERE entity_type = 'workspace_pipeline' ORDER BY id)`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pipeline.brief_set", "pipeline.brief_delete", "brief e2e set: 25 bytes", "brief e2e removed"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit log misses %q", want)
		}
	}
	if strings.Contains(audit, "Test on the local system") {
		t.Error("the audit log must not hold a brief's text")
	}
}
