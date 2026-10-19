package routes

import (
	"encoding/json"
	"strings"
	"testing"
)

// Pipeline settings over HTTP, end to end: an approver sets a workspace's
// routing, a plain key cannot, the next queued stage follows it; a group
// default reaches the workspace by pattern; worker presence shows a worker
// from its first (idle) poll and its goodbye.
func TestIntegrationPipelineSettingsAndPresence(t *testing.T) {
	_, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws = "github.com/fortix/api"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"martin","workspaces":["`+ws+`"],"approver":true}`)}
	plain := workerClient{base: baseURL, key: mint(t, root, `{"name":"agent","workspaces":["`+ws+`"]}`)}
	leases := map[string]string{}
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`"]}`), lease: leases}

	// Presence: an idle poll registers the worker before it has any run.
	mustDo(t, worker.req("POST", "/api/runs/claim", `{"worker_id":"w1","runners":["claude","opencode-glm"],"workspaces":["`+ws+`"],"version":"v0.17.0","poll_ms":30000}`), 204)
	workers := workerList(t, root)
	if len(workers) != 1 || workers[0].(map[string]any)["state"] != "idle" || workers[0].(map[string]any)["version"] != "v0.17.0" {
		t.Fatalf("idle worker visible from its first poll: %v", workers)
	}

	// Who may set a workspace pipeline.
	body := `{"stages":{"plan":{"members":[{"runner":"opencode-glm"}]},"review":{"members":[{"runner":"claude","focus":"security"},{"runner":"opencode-glm","focus":"tests"}],"policy":"majority"}},"via":"dashboard"}`
	mustDo(t, plain.req("PUT", "/api/workspaces/"+escapeWS(ws)+"/pipeline", body), 403)
	view := mustJSON(t, approver.req("PUT", "/api/workspaces/"+escapeWS(ws)+"/pipeline", body), 200).(map[string]any)
	if eff := view["effective"].(map[string]any); eff["review"].(map[string]any)["policy"] != "majority" {
		t.Fatalf("saved view: %v", view)
	}
	if got := mustJSON(t, plain.req("GET", "/api/workspaces/"+escapeWS(ws)+"/pipeline", ""), 200).(map[string]any); got["owner_id"] != ws {
		t.Fatalf("any key in scope may read: %v", got)
	}

	// The next queued stage follows the stored routing: plan pinned to
	// opencode-glm, so a worker offering only claude gets nothing.
	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+ws+`","title":"routed item","author_agent_id":"martin"}`), 201).(map[string]any)
	mustDo(t, approver.req("POST", "/api/inbox/"+item["id"].(string)+"/queue", ""), 200)
	mustDo(t, worker.req("POST", "/api/runs/claim", `{"worker_id":"w1","runners":["claude"],"workspaces":["`+ws+`"]}`), 204)
	run := claim(t, worker, `{"worker_id":"w1","runners":["opencode-glm"],"workspaces":["`+ws+`"]}`)
	if run["stage"] != "plan" || run["runner"] != "opencode-glm" {
		t.Fatalf("plan follows the pipeline settings: %v", run)
	}
	if w := workerList(t, root)[0].(map[string]any); w["state"] != "busy" {
		t.Fatalf("holding a run is busy: %v", w)
	}

	// A group default reaches the workspace by pattern (root only).
	mustDo(t, root.req("POST", "/api/groups", `{"name":"fortix","patterns":["github.com/fortix/*"]}`), 201)
	mustDo(t, approver.req("PUT", "/api/groups/fortix/pipeline", `{"stages":{"qa":{"members":[{"runner":"claude"}]}}}`), 403)
	mustDo(t, root.req("PUT", "/api/groups/fortix/pipeline", `{"stages":{"qa":{"members":[{"runner":"claude"}]}}}`), 200)
	eff := mustJSON(t, approver.req("GET", "/api/workspaces/"+escapeWS(ws)+"/pipeline", ""), 200).(map[string]any)["effective"].(map[string]any)
	if eff["qa"].(map[string]any)["source"] != "group:fortix" || eff["plan"].(map[string]any)["source"] != "workspace" {
		t.Fatalf("group default + workspace override: %v", eff)
	}

	// Goodbye: only the key that owns the worker row may say it.
	mustDo(t, approver.req("POST", "/api/workers/w1/offline", ""), 404)
	mustDo(t, worker.req("POST", "/api/workers/w1/offline", ""), 204)
	if w := workerList(t, root)[0].(map[string]any); w["state"] != "offline" {
		t.Fatalf("goodbye shows offline: %v", w)
	}
}

func escapeWS(ws string) string { return strings.ReplaceAll(ws, "/", "%2F") }

func workerList(t *testing.T, c workerClient) []any {
	t.Helper()
	r := mustDo(t, c.req("GET", "/api/workers", ""), 200)
	var out []any
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("workers: %v: %s", err, r.body)
	}
	return out
}
