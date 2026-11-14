package routes

import (
	"fmt"
	"testing"
)

// Routing by label over HTTP: an approver labels a worker (the worker's own
// key cannot), an item names the labels it needs, and its run is handed
// only to the labelled worker; the item state and the run carry the needs.
func TestIntegrationWorkerLabelsRouteAnItem(t *testing.T) {
	sqlDB, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws = "github.com/globex/shop"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`"],"approver":true}`)}
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: map[string]string{}}
	poll := func(id string) string {
		return `{"worker_id":"` + id + `","runners":["claude"],"workspaces":["` + ws + `"]}`
	}

	// A worker is labelled once it has polled.
	mustDo(t, approver.req("PUT", "/api/workers/w-stock/labels", `{"labels":["stock"]}`), 404)
	mustDo(t, worker.req("POST", "/api/runs/claim", poll("w-plain")), 204)
	mustDo(t, worker.req("POST", "/api/runs/claim", poll("w-stock")), 204)
	mustDo(t, plain.req("PUT", "/api/workers/w-stock/labels", `{"labels":["stock"]}`), 403)
	mustDo(t, worker.req("PUT", "/api/workers/w-stock/labels", `{"labels":["stock"]}`), 403)
	mustDo(t, approver.req("PUT", "/api/workers/w-stock/labels", `{"labels":["has space"]}`), 400)
	mustDo(t, approver.req("PUT", "/api/workers/w-stock/labels", `{"label":["stock"]}`), 400)
	set := mustJSON(t, approver.req("PUT", "/api/workers/w-stock/labels", `{"labels":["Stock","plant-2","stock"]}`), 200).(map[string]any)
	if got := fmt.Sprint(set["labels"]); got != "[plant-2 stock]" {
		t.Fatalf("labels: %v", set)
	}
	for _, w := range workerList(t, plain) {
		w := w.(map[string]any)
		want := "<nil>"
		if w["id"] == "w-stock" {
			want = "[plant-2 stock]"
		}
		if got := fmt.Sprint(w["labels"]); got != want {
			t.Fatalf("%v listed with labels %s, want %s", w["id"], got, want)
		}
	}

	// An item that needs a label: queued, its plan run waits for the
	// labelled worker.
	mustDo(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"x","author_agent_id":"alex","needs":["no good"]}`), 400)
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"stock count","author_agent_id":"alex","needs":["stock"]}`), 201).(map[string]any)
	id := item["id"].(string)
	if got := fmt.Sprint(item["needs"]); got != "[stock]" {
		t.Fatalf("created: %v", item)
	}
	mustDo(t, approver.req("POST", "/api/inbox/"+id+"/queue", ""), 200)
	state := mustJSON(t, approver.req("GET", "/api/inbox/"+id+"/state", ""), 200).(map[string]any)
	if got := fmt.Sprint(state["item"].(map[string]any)["needs"]); got != "[stock]" {
		t.Fatalf("item state: %v", state["item"])
	}
	mustDo(t, worker.req("POST", "/api/runs/claim", poll("w-plain")), 204)

	// In the workflow the needs are the approver's to change.
	mustDo(t, plain.req("PATCH", "/api/inbox/"+id, `{"needs":[]}`), 403)
	mustDo(t, approver.req("PATCH", "/api/inbox/"+id, `{"needs":["stock","plant-2"]}`), 204)

	run := claim(t, worker, poll("w-stock"))
	if run["stage"] != "plan" || run["item_id"] != id || fmt.Sprint(run["needs"]) != "[plant-2 stock]" {
		t.Fatalf("the labelled worker takes it: %v", run)
	}

	var audited string
	if err := sqlDB.QueryRow(`SELECT group_concat(action || ' ' || actor || ' ' || COALESCE(notes, ''), '; ') FROM (SELECT * FROM audit_log WHERE action = 'worker.labels' ORDER BY id)`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != "worker.labels key alex plant-2, stock" {
		t.Fatalf("audit: %s", audited)
	}
	var edits int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'inbox.edit' AND entity_id = ? AND notes LIKE '%needs%'`, id).Scan(&edits); err != nil || edits != 1 {
		t.Fatalf("the needs change is audited once: %d %v", edits, err)
	}
}
