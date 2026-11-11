package routes

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// mcpCall posts one tools/call to the stack's MCP endpoint with a bearer
// credential and returns the HTTP status and the raw answer.
func mcpCall(t *testing.T, base, bearer, tool string, args map[string]any) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Run tokens end to end, through the real routes and the MCP endpoint: the
// claim hands the worker a token for that run; with it an agent works in
// the run's workspace over MCP as that run, cannot read another workspace
// the worker's key reaches, cannot use REST or tools outside its stage, and
// loses the token the moment the run is reported.
func TestIntegrationRunToken(t *testing.T) {
	_, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const wsA, wsB = "github.com/acme/app", "github.com/fortix/freedom3"
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+wsA+`","name":"app"}`), 201)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+wsB+`","name":"freedom3"}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"relay","workspaces":["`+wsA+`"],"approver":true}`)}
	// The worker serves both repositories: its key reaches both.
	worker := workerClient{base: baseURL, key: mint(t, root, `{"name":"worker-1","workspaces":["`+wsA+`","`+wsB+`"]}`), lease: map[string]string{}}

	// Something recorded about the other repository.
	mustDo(t, root.req("POST", "/api/blackboard/entries", `{"workspace_id":"`+wsB+`","scope":"project","entry_type":"finding","title":"ff3 secret design","content":"only for ff3","author_agent_id":"martin"}`), 201)

	item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+wsA+`","title":"an item","author_agent_id":"martin"}`), 201).(map[string]any)
	mustDo(t, approver.req("POST", "/api/inbox/"+item["id"].(string)+"/queue", ""), 200)
	run := claim(t, worker, `{"worker_id":"w1","workspaces":["`+wsA+`","`+wsB+`"]}`)
	runID := run["id"].(string)
	token, _ := run["agent_token"].(string)
	if !strings.HasPrefix(token, "skr_") {
		t.Fatalf("the claim must carry the run's agent token: %v", run)
	}

	// The run's own workspace: reads and a recorded finding work.
	if code, body := mcpCall(t, baseURL, token, "skopos_context", map[string]any{"workspace_id": wsA}); code != 200 || strings.Contains(body, `"isError":true`) {
		t.Fatalf("skopos_context in the run's workspace: %d %s", code, body)
	}
	if code, body := mcpCall(t, baseURL, token, "blackboard_write", map[string]any{
		"workspace_id": wsA, "scope": "project", "entry_type": "finding", "title": "found by the run", "content": "c", "author_agent_id": "claude@" + runID,
	}); code != 200 || strings.Contains(body, `"isError":true`) {
		t.Fatalf("blackboard_write in the run's workspace: %d %s", code, body)
	}
	if list := root.req("GET", "/api/blackboard/entries?workspace_id="+wsA, ""); list.code != 200 || !strings.Contains(list.body, "found by the run") {
		t.Fatalf("the run's finding must be stored: %d %s", list.code, list.body)
	}

	// The other workspace the worker's key reaches is closed to the run.
	for tool, args := range map[string]map[string]any{
		"blackboard_read": {"workspace_id": wsB},
		"skopos_context":  {"workspace_id": wsB},
		"code_search":     {"workspace_id": wsB, "q": "secret"},
	} {
		code, body := mcpCall(t, baseURL, token, tool, args)
		if code != 200 || !strings.Contains(body, "out of scope") || strings.Contains(body, "ff3 secret design") {
			t.Fatalf("%s on the other workspace must be out of scope: %d %s", tool, code, body)
		}
	}
	// Tools outside the stage, and plans it does not own.
	if _, body := mcpCall(t, baseURL, token, "plan_create", map[string]any{"workspace_id": wsA, "name": "x", "author_agent_id": "a"}); !strings.Contains(body, "do not allow plan_create") {
		t.Fatalf("plan_create must be refused: %s", body)
	}
	if _, body := mcpCall(t, baseURL, token, "inbox_list", map[string]any{"workspace_id": wsA}); !strings.Contains(body, "do not allow inbox_list") {
		t.Fatalf("inbox_list must be refused: %s", body)
	}

	// REST is closed, except whoami, which names the run.
	agent := workerClient{base: baseURL, key: token}
	mustDo(t, agent.req("GET", "/api/blackboard/entries?workspace_id="+wsA, ""), 403, "REST with a run token")
	mustDo(t, agent.req("POST", "/api/runs/claim", `{"worker_id":"w9","workspaces":["`+wsA+`"]}`), 403, "claim with a run token")
	mustDo(t, agent.req("GET", "/api/keys", ""), 403, "keys with a run token")
	who := mustJSON(t, agent.req("GET", "/api/whoami", ""), 200).(map[string]any)
	if r, _ := who["run"].(map[string]any); who["root"] != false || r == nil || r["id"] != runID || r["workspace_id"] != wsA || r["worker_id"] != "w1" {
		t.Fatalf("whoami must name the run: %v", who)
	}
	if list, _ := who["workspaces"].([]any); len(list) != 1 {
		t.Fatalf("a run reaches one workspace: %v", who["workspaces"])
	}

	// The worker's own key is no substitute on the MCP side of the same
	// question: it still reaches both (that is what the token narrows).
	if _, body := mcpCall(t, baseURL, worker.key, "blackboard_read", map[string]any{"workspace_id": wsB}); !strings.Contains(body, "ff3 secret design") {
		t.Fatalf("the worker key reaches the other workspace (the control for this test): %s", body)
	}

	// Reported: the token is dead at once.
	mustDo(t, worker.reqH("POST", "/api/runs/"+runID+"/report", `{"worker_id":"w1","status":"failed","reason":"test over"}`, worker.lease[runID]), 200, "report")
	if code, body := mcpCall(t, baseURL, token, "skopos_context", map[string]any{"workspace_id": wsA}); code != http.StatusUnauthorized {
		t.Fatalf("a reported run's token must be refused: %d %s", code, body)
	}
	mustDo(t, agent.req("GET", "/api/whoami", ""), 401, "whoami after the report")
}
