package routes

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	wk "github.com/martinsuchenak/skopos/internal/worker"
)

// holdsText reports the tables in which some column of some row holds
// text: every table, every column, text and blobs alike.
func holdsText(t *testing.T, sqlDB *sql.DB, text string) []string {
	t.Helper()
	rows, err := sqlDB.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()
	var found []string
	for _, table := range tables {
		data, err := sqlDB.Query(fmt.Sprintf(`SELECT * FROM %q`, table))
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		cols, _ := data.Columns()
		for data.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := data.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				var s string
				switch x := v.(type) {
				case string:
					s = x
				case []byte:
					s = string(x)
				}
				if strings.Contains(s, text) {
					found = append(found, table+"."+cols[i])
				}
			}
		}
		_ = data.Close()
	}
	return found
}

// MCP servers on the server, end to end through the real routes and the
// worker's own client: a fleet admin defines a server and sets its secret;
// a workspace's admin gives the workspace's runs that server; the claim of
// a run in that workspace carries it with the secret filled in, and the
// claim of a run elsewhere does not; a worker that cannot take servers
// from a claim gets none of the workspace's runs. The secret's value is in
// no table, no audit note, no export and no later read of the run.
func TestIntegrationMCPServersForRuns(t *testing.T) {
	sqlDB, baseURL := workerStack(t)
	root := workerClient{base: baseURL, key: "root-secret"}
	const ws, other = "github.com/globex/shop", "github.com/alex/personal"
	wsPath := "/api/workspaces/" + url.PathEscape(ws)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+ws+`"}`), 201)
	mustDo(t, root.req("POST", "/api/workspaces", `{"id":"`+other+`"}`), 201)
	mustDo(t, root.req("POST", "/api/groups", `{"name":"globex","members":["`+ws+`"]}`), 201)
	approver := workerClient{base: baseURL, key: mint(t, root, `{"name":"alex","workspaces":["`+ws+`","`+other+`"],"approver":true}`)}
	ops := workerClient{base: baseURL, key: mint(t, root, `{"name":"ops","groups":["globex"],"admin":true}`)}
	workerKey := mint(t, root, `{"name":"worker-1","workspaces":["`+ws+`","`+other+`"],"worker":true}`)
	// A worker key is a worker's and nothing more.
	mustDo(t, root.req("POST", "/api/keys", `{"name":"both","workspaces":["`+ws+`"],"worker":true,"admin":true}`), 400, "a worker key with the admin permission")
	mustDo(t, root.req("POST", "/api/keys", `{"name":"both","workspaces":["`+ws+`"],"worker":true,"approver":true}`), 400, "a worker key with the approver permission")
	worker := workerClient{base: baseURL, key: workerKey}
	wc := wk.NewClient(baseURL, workerKey)
	wc.SetServerCommands(true)
	ctx := t.Context()
	const secret = "fabrikam-router-token-9f3a7c21"

	// Definitions: the fleet's.
	def := `{"type":"http","url":"https://router.example.com/mcp","headers":{"Authorization":"Bearer ${secret:token}"},"workspaces":["github.com/globex/*"]}`
	mustDo(t, ops.req("PUT", "/api/fleet/mcp-servers/router", def), 403, "a group admin does not define MCP servers")
	mustDo(t, root.req("PUT", "/api/fleet/mcp-servers/router", `{"type":"http","url":"https://x/mcp","headers":{"Authorization":"Bearer `+secret+`"}}`), 400, "a credential written into the definition")
	mustDo(t, root.req("PUT", "/api/fleet/mcp-servers/router", `{"type":"http","urls":"https://x/mcp"}`), 400, "a misspelt field")
	mustDo(t, root.req("PUT", "/api/fleet/mcp-servers/skopos", `{"type":"http","url":"https://x/mcp"}`), 400, "the worker's own name")
	mustDo(t, root.req("PUT", "/api/fleet/mcp-servers/router", `{"type":"http","url":"https://x/mcp?api_key=`+secret+`"}`), 400, "a credential in the URL")
	mustDo(t, root.req("PUT", "/api/fleet/mcp-servers/router", `{"type":"http","url":"https://x/mcp","headers":{"Authorization":"Bearer ${secret:token}"}}`), 400, "secrets without the workspaces they are for")
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"mcp":["router"],"mcp_servers":["mcp__router"]}}`), 400, "a server that is not defined")
	mustDo(t, root.req("PUT", "/api/fleet/mcp-servers/router", def), 200)
	mustDo(t, root.req("PUT", "/api/fleet/mcp-servers/scripts", `{"type":"command","command":["${TOOLS_DIR}/scripts","mcp"]}`), 200)

	// The secret: set by a fleet admin, never read back by anyone.
	secretPath := "/api/fleet/mcp-servers/router/secrets/token"
	mustDo(t, ops.req("PUT", secretPath, `{"value":"`+secret+`"}`), 403, "a group admin does not set secrets")
	mustDo(t, worker.req("PUT", secretPath, `{"value":"`+secret+`"}`), 403, "the worker's key")
	mustDo(t, root.req("PUT", "/api/fleet/mcp-servers/router/secrets/nope", `{"value":"`+secret+`"}`), 400, "a secret the definition does not name")
	mustDo(t, root.req("PUT", secretPath, `{"valu":"`+secret+`"}`), 400, "a misspelt body")
	mustDo(t, root.req("PUT", secretPath, `{"value":"`+secret+`"}`), 204)
	mustDo(t, worker.req("GET", "/api/fleet/mcp-servers", ""), 403, "a worker's key does not list MCP servers")
	listed := ops.req("GET", "/api/fleet/mcp-servers", "")
	mustDo(t, listed, 200)
	if !strings.Contains(listed.body, `"name":"token","set":true`) || strings.Contains(listed.body, secret) {
		t.Fatalf("the list says the secret is set and never shows it: %s", listed.body)
	}

	// The workspace's runs get the servers; its tools have to be allowed.
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"mcp":["router","scripts"],"mcp_servers":["mcp__scripts"]}}`), 400, "a server whose tools are not allowed")
	mustDo(t, ops.req("PUT", wsPath+"/config", `{"config":{"mcp":["router","scripts"],"mcp_servers":["mcp__router","mcp__scripts"]}}`), 200)
	// The other workspace is managed too, without any MCP setting. Its own
	// admin cannot pull the Globex server — and its credentials — into it:
	// the server's definition says which workspaces it is for.
	mustDo(t, root.req("PUT", "/api/workspaces/"+url.PathEscape(other)+"/config", `{"config":{"checks":["true"]}}`), 200)
	personal := workerClient{base: baseURL, key: mint(t, root, `{"name":"personal-admin","workspaces":["`+other+`"],"admin":true}`)}
	refused := personal.req("PUT", "/api/workspaces/"+url.PathEscape(other)+"/config", `{"config":{"mcp":["router"],"mcp_servers":["mcp__router"]}}`)
	if mustDo(t, refused, 400, "another workspace's admin attaching the server"); !strings.Contains(refused.body, "may not be given to the runs of") {
		t.Fatalf("the refusal: %s", refused.body)
	}

	queue := func(workspace, title string) {
		t.Helper()
		item := mustJSON(t, root.req("POST", "/api/inbox", `{"workspace_id":"`+workspace+`","title":"`+title+`","author_agent_id":"alex"}`), 201).(map[string]any)
		mustDo(t, approver.req("POST", "/api/inbox/"+item["id"].(string)+"/queue", ""), 200)
	}
	queue(ws, "needs the globex tools")

	// A worker from before this feature: the raw claim, without it.
	old := worker.req("POST", "/api/runs/claim", `{"worker_id":"w-old","runners":["claude"],"workspaces":["`+ws+`"],"server_commands":true}`)
	mustDo(t, old, http.StatusNoContent, "a worker that cannot take MCP servers from a claim")
	var reason string
	if err := sqlDB.QueryRow(`SELECT json_extract(unhealthy, '$."` + ws + `"') FROM workers WHERE id = 'w-old'`).Scan(&reason); err != nil || !strings.Contains(reason, "MCP servers") {
		t.Fatalf("it shows as unhealthy for the workspace, with why: %q %v", reason, err)
	}

	// Any key with the workspace in scope can call the claim endpoint. One
	// that is not a worker's gets no run of a workspace whose MCP servers
	// the server holds: the credentials go to worker keys only.
	plain := approver.req("POST", "/api/runs/claim", `{"worker_id":"w-plain","runners":["claude"],"workspaces":["`+ws+`"],"server_commands":true,"features":["run_mcp","run_brief"]}`)
	if mustDo(t, plain, http.StatusNoContent, "a key that is not a worker key"); strings.Contains(plain.body, secret) {
		t.Fatal("the secret left the server for a key that is not a worker's")
	}
	if err := sqlDB.QueryRow(`SELECT json_extract(unhealthy, '$."` + ws + `"') FROM workers WHERE id = 'w-plain'`).Scan(&reason); err != nil || !strings.Contains(reason, "not a worker key") {
		t.Fatalf("it shows as unhealthy for the workspace, with the way to fix it: %q %v", reason, err)
	}

	// The claim: the servers for this run's workspace, the secret filled in.
	run, err := wc.Claim(ctx, "w1", []string{"claude"}, []string{ws, other}, nil, nil, nil)
	if err != nil || run == nil || run.WorkspaceID != ws || run.MCP == nil {
		t.Fatalf("claim: %+v %v", run, err)
	}
	if len(run.MCP.Servers) != 2 || len(run.MCP.Secrets) != 1 || run.MCP.Secrets[0] != secret || len(run.MCP.Notes) != 0 {
		t.Fatalf("the run's servers: %+v", run.MCP)
	}
	var entry struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(run.MCP.Servers["router"], &entry); err != nil || entry.Headers["Authorization"] != "Bearer "+secret {
		t.Fatalf("the delivered server: %s %v", run.MCP.Servers["router"], err)
	}
	// Reading the run afterwards — by anyone — shows none of it.
	for _, c := range []workerClient{root, worker, approver} {
		got := c.req("GET", "/api/runs/"+run.ID, "")
		if mustDo(t, got, 200); strings.Contains(got.body, secret) || strings.Contains(got.body, `"mcp"`) {
			t.Fatalf("a read of the run carries its MCP servers: %s", got.body)
		}
	}

	// A run in the other workspace carries nothing: its worker's own
	// mcp_config applies there.
	queue(other, "a personal repo")
	second, err := wc.Claim(ctx, "w2", []string{"claude"}, []string{other}, nil, nil, nil)
	if err != nil || second == nil || second.WorkspaceID != other || second.MCP != nil {
		t.Fatalf("a run of a workspace without MCP servers on the server: %+v %v", second, err)
	}

	// A worker that does not take commands gets the http server only.
	queue(ws, "another globex item")
	noCommands := wk.NewClient(baseURL, workerKey)
	third, err := noCommands.Claim(ctx, "w3", []string{"claude"}, []string{ws}, nil, nil, nil)
	if err != nil || third == nil || third.MCP == nil || len(third.MCP.Servers) != 1 || third.MCP.Servers["router"] == nil ||
		len(third.MCP.Notes) != 1 || !strings.Contains(third.MCP.Notes[0], "server_commands") {
		t.Fatalf("without server_commands: %+v %v", third, err)
	}

	// The value is nowhere: no table (it is sealed in `secrets`), no audit
	// note, no export.
	if found := holdsText(t, sqlDB, secret); len(found) != 0 {
		t.Fatalf("the secret's value is stored in plain text in: %v", found)
	}
	exported := root.req("GET", "/api/config/export", "")
	if mustDo(t, exported, 200); strings.Contains(exported.body, secret) || !strings.Contains(exported.body, "${secret:token}") {
		t.Fatalf("the export: %s", exported.body)
	}
	plan := mustJSON(t, root.req("POST", "/api/config/apply", `{"plan":true,"document":`+exported.body+`}`), 200).(map[string]any)
	if changes, _ := plan["changes"].([]any); len(changes) != 0 {
		t.Fatalf("export then apply changes nothing: %v", plan)
	}
	var actions string
	if err := sqlDB.QueryRow(`SELECT group_concat(action, ' ') FROM (SELECT action FROM audit_log WHERE entity_type = 'mcp_server' AND entity_id = 'router' ORDER BY id)`).Scan(&actions); err != nil || actions != "mcp_server.create mcp_server.secret_set" {
		t.Fatalf("audit: %q %v", actions, err)
	}

	// The secret key: its status and a rotation are the root key's.
	mustDo(t, ops.req("GET", "/api/fleet/secret-key", ""), 403)
	mustDo(t, ops.req("POST", "/api/fleet/secret-key/rotate", ""), 403)
	status := mustJSON(t, root.req("GET", "/api/fleet/secret-key", ""), 200).(map[string]any)
	if status["current"] == "" || status["pending"] != float64(0) || status["unreadable"] != float64(0) {
		t.Fatalf("status: %v", status)
	}
	rotated := mustJSON(t, root.req("POST", "/api/fleet/secret-key/rotate", ""), 200).(map[string]any)
	if rotated["resealed"] != float64(0) {
		t.Fatalf("nothing to rotate: %v", rotated)
	}

	// Taking the secret away: the next run goes without that server, and
	// is told which.
	mustDo(t, root.req("DELETE", secretPath, ""), 204)
	mustDo(t, root.req("DELETE", secretPath, ""), 404)
	queue(ws, "after the secret was unset")
	fourth, err := wc.Claim(ctx, "w4", []string{"claude"}, []string{ws}, nil, nil, nil)
	if err != nil || fourth == nil || fourth.MCP == nil || fourth.MCP.Servers["router"] != nil || !strings.Contains(strings.Join(fourth.MCP.Notes, " "), "secret token is not set") {
		t.Fatalf("after unsetting the secret: %+v %v", fourth, err)
	}
	mustDo(t, root.req("DELETE", "/api/fleet/mcp-servers/router", ""), 409, "a server a config still names")
	mustDo(t, root.req("DELETE", "/api/fleet/mcp-servers/router?force=1", ""), 204)
}
